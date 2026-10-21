package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

// FileSuffix ends the name of every database's file in a backup.
const FileSuffix = ".bak"

// Backup runs BACKUP DATABASE for each database (full or differential) into
// the server's backup folder, checks the file, reads its header and copies it
// into the sink, then deletes it from the folder.
func (Driver) Backup(env *engine.Env, spec *engine.BackupSpec, sink engine.Sink) (*engine.Result, error) {
	if spec.Job.Method != store.MethodMSSQL {
		return nil, fmt.Errorf("unsupported method: %s", spec.Job.Method)
	}
	if len(spec.Databases) == 0 {
		return nil, errors.New("no databases to back up")
	}
	for _, d := range spec.Databases {
		if strings.EqualFold(d.Name, "tempdb") {
			return nil, errors.New("tempdb cannot be backed up: remove it from the job")
		}
	}
	ctx := env.Ctx
	db, err := Open(ctx, spec.Server, "")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	in, err := inspect(ctx, db, spec.Server, false)
	if err != nil {
		return nil, err
	}
	if in.folderErr != nil {
		return nil, in.folderErr
	}
	o := spec.Job.Options
	if err := CheckWithOptions(o.ExtraArgs); err != nil {
		return nil, err
	}
	diff := spec.Type == store.TypeDifferential
	if diff && o.CopyOnly {
		return nil, errors.New("a copy-only job takes full backups only")
	}
	compress := false
	switch o.NativeCompression {
	case "on":
		compress = true
	case "off":
	default:
		compress = in.Compression
	}
	if compress && !in.Compression {
		env.Log.Printf("NOTE: %s may not compress backups: DumpCat asks for it anyway (the job says so)", nonEmpty(in.Edition, "this edition"))
	}
	kind := "full"
	if diff {
		kind = "differential"
	}
	env.Log.Printf("Backup folder: %s", in.folder.describe("reads"))
	env.Log.Printf("BACKUP DATABASE: %s%s, checksums, %s", kind, map[bool]string{true: ", copy-only", false: ""}[o.CopyOnly],
		map[bool]string{true: "compressed by SQL Server", false: "not compressed by SQL Server"}[compress])
	var parents map[string]*Header
	if diff {
		if spec.Parent == nil {
			return nil, errors.New("a differential backup needs its full backup")
		}
		parents = headersOf(spec.Parent.Meta)
	}
	res := &engine.Result{Meta: map[string]any{"copy_only": o.CopyOnly, "native_compression": compress,
		"edition": in.Edition, "platform": in.Platform}, Databases: engine.ToEntries(spec.Databases)}
	used := map[string]bool{}
	names := make([]string, len(spec.Databases))
	for i, d := range spec.Databases {
		fn := engine.SafeFileName(d.Name)
		for k := 2; used[strings.ToLower(fn)]; k++ {
			fn = engine.SafeFileName(d.Name) + "_" + strconv.Itoa(k)
		}
		used[strings.ToLower(fn)] = true
		names[i] = fn + FileSuffix
		res.Databases[i].File = names[i]
	}
	var mu sync.Mutex
	headers := map[string]*Header{}
	counted := store.TableStats{}
	threads := max(spec.Job.Threads, 1)
	idx := make([]int, len(spec.Databases))
	for i := range idx {
		idx[i] = i
	}
	err = engine.Parallel(ctx, threads, idx, func(ctx context.Context, i int) error {
		d := spec.Databases[i]
		b := &dbBackup{env: env, db: db, in: in, name: d.Name, file: names[i], diff: diff, copyOnly: o.CopyOnly, compress: compress,
			verify: o.VerifyOnly, extra: strings.TrimSpace(o.ExtraArgs), parent: parents[d.Name], label: spec.Job.Name}
		h, rows, err := b.run(ctx, sink)
		if err != nil {
			return fmt.Errorf("database %s: %w", d.Name, err)
		}
		mu.Lock()
		headers[d.Name] = h
		if rows != nil {
			counted[d.Name] = rows
			res.Databases[i].Tables = len(rows)
		}
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	var latest time.Time
	for _, h := range headers {
		if h.Finish.After(latest) {
			latest = h.Finish
		}
	}
	if !latest.IsZero() {
		res.Meta["consistent_at"] = latest.Unix()
	}
	res.Meta["mssql"] = headers
	res.SetRows(counted)
	return res, nil
}

// reWith matches BACKUP … WITH options: NAME, or NAME = number, word or 'text'.
var reWith = regexp.MustCompile(`(?i)^\s*[a-z_]+(\s*=\s*(\d+|n?'[^']*'|[a-z_]+))?(\s*,\s*[a-z_]+(\s*=\s*(\d+|n?'[^']*'|[a-z_]+))?)*\s*$`)

// CheckWithOptions validates a job's extra BACKUP options ("BUFFERCOUNT = 64,
// MAXTRANSFERSIZE = 4194304"): options only, and none that changes what the
// backup is or leaves the database restoring.
func CheckWithOptions(extra string) error {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return nil
	}
	if !reWith.MatchString(extra) {
		return fmt.Errorf("extra options must be BACKUP WITH options, e.g. BUFFERCOUNT = 64, MAXTRANSFERSIZE = 4194304: %q", extra)
	}
	for _, part := range strings.Split(extra, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch strings.ToUpper(strings.TrimSpace(name)) {
		case "NORECOVERY", "STANDBY", "NO_TRUNCATE", "COPY_ONLY", "DIFFERENTIAL", "COMPRESSION", "NO_COMPRESSION", "CHECKSUM", "NO_CHECKSUM",
			"FORMAT", "NOFORMAT", "INIT", "NOINIT", "SKIP", "NOSKIP", "NAME", "STATS", "ENCRYPTION", "MEDIANAME", "MEDIAPASSWORD", "PASSWORD":
			return fmt.Errorf("the extra option %s is set by DumpCat or not allowed", strings.TrimSpace(name))
		}
	}
	return nil
}

// dbBackup is the backup of one database.
type dbBackup struct {
	env      *engine.Env
	db       *sql.DB
	in       *instance
	name     string // the database
	file     string // its file in the backup
	diff     bool
	copyOnly bool
	compress bool
	verify   bool
	extra    string  // more WITH options from the job
	parent   *Header // the full backup a differential follows
	label    string
}

func (b *dbBackup) statement(path string) string {
	with := []string{"CHECKSUM", "FORMAT", "INIT", "SKIP", "STATS = 10"}
	if b.diff {
		with = append([]string{"DIFFERENTIAL"}, with...)
	}
	if b.copyOnly {
		with = append(with, "COPY_ONLY")
	}
	if b.compress {
		with = append(with, "COMPRESSION")
	} else {
		with = append(with, "NO_COMPRESSION")
	}
	desc := "DumpCat " + b.label
	if len(desc) > 128 {
		desc = desc[:128]
	}
	with = append(with, "NAME = "+QuoteString(desc))
	if b.extra != "" {
		with = append(with, b.extra)
	}
	return "BACKUP DATABASE " + QuoteName(b.name) + " TO DISK = " + QuoteString(path) + " WITH " + strings.Join(with, ", ")
}

func (b *dbBackup) run(ctx context.Context, sink engine.Sink) (*Header, map[string]int64, error) {
	lg := b.env.Log
	f := b.in.folder
	xname := fileName(b.name, FileSuffix)
	spath := f.server(xname)
	defer f.remove(xname)
	q := b.statement(spath)
	lg.Printf("%s", q)
	t0 := time.Now()
	err := exec(ctx, b.db, q, func(m string) { lg.Printf("[%s] %s", b.name, m) })
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if hasError(err, 3201, 3013) && osError(err) != 0 {
			return nil, nil, deviceError(err, f, spath, b.in.ServiceAccount)
		}
		if b.diff && hasError(err, 3035) {
			return nil, nil, fmt.Errorf("SQL Server has no full backup to base a differential on (%s): run a full backup", errText(err))
		}
		return nil, nil, errors.New(errText(err))
	}
	h, err := header(ctx, b.db, spath, b.in.offset)
	if err != nil {
		lg.Printf("WARNING: %s: could not read the backup's header (%s): this backup cannot be the base of differential or log backups", b.name, errText(err))
		h = &Header{Database: b.name, Type: map[bool]string{true: "differential", false: "full"}[b.diff], Finish: time.Now()}
	} else if b.parent != nil && h.DiffBaseGUID != "" && b.parent.BackupSetGUID != "" && !strings.EqualFold(h.DiffBaseGUID, b.parent.BackupSetGUID) {
		// a full backup taken since the check (another tool): this differential follows that one
		return nil, nil, fmt.Errorf("the differential follows a full backup that is not DumpCat's (backup set %s): another tool took a full backup meanwhile — run a full backup", h.DiffBaseGUID)
	}
	if h.Damaged {
		return nil, nil, errors.New("SQL Server marks the backup as damaged")
	}
	if b.verify {
		vq := "RESTORE VERIFYONLY FROM DISK = " + QuoteString(spath) + " WITH CHECKSUM"
		lg.Printf("%s", vq)
		if err := exec(ctx, b.db, vq, nil); err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, fmt.Errorf("RESTORE VERIFYONLY failed: %s", errText(err))
		}
	}
	rows := rowCounts(ctx, b.db, b.name)
	in, size, err := f.Files.Open(ctx, xname)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if !f.same() {
			return nil, nil, fmt.Errorf("SQL Server wrote %s, but DumpCat cannot read it from %s: are both paths the same folder? (%v)", spath, f.where(), err)
		}
		return nil, nil, fmt.Errorf("DumpCat cannot read the backup SQL Server wrote from %s: %v", f.where(), err)
	}
	defer in.Close()
	w, err := sink.Create(b.file, engine.FileOpts{Kind: "bak", Database: b.name, Compress: !b.compress})
	if err != nil {
		return nil, nil, err
	}
	n, err := io.Copy(w, in)
	if err == nil && n != size {
		err = fmt.Errorf("read %d of %d bytes", n, size)
	}
	if err := engine.Finish(w, err); err != nil {
		return nil, nil, err
	}
	lg.Printf("Finished %s: %s %s backup (LSN %s → %s) in %s", b.name, engine.HumanBytes(size), h.Type, nonEmpty(h.FirstLSN, "?"), nonEmpty(h.LastLSN, "?"),
		engine.HumanDuration(time.Since(t0)))
	return h, rows, nil
}

// rowCounts reads the rows of every user table from the server's own
// counts (sys.partitions), right after the backup: restore drills compare
// them with the restored copy (exactly when nothing wrote meanwhile).
func rowCounts(ctx context.Context, db *sql.DB, database string) map[string]int64 {
	q := "SELECT s.name, t.name, SUM(p.rows) FROM " + QuoteName(database) + ".sys.tables t JOIN " + QuoteName(database) + ".sys.schemas s ON s.schema_id = t.schema_id JOIN " +
		QuoteName(database) + ".sys.partitions p ON p.object_id = t.object_id AND p.index_id IN (0, 1) WHERE t.is_ms_shipped = 0 GROUP BY s.name, t.name"
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var s, t string
		var n int64
		if rows.Scan(&s, &t, &n) != nil {
			return nil
		}
		out[s+"."+t] = n
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// DifferentialBase tells why base cannot be the full backup of a
// differential of dbs: a database it does not hold, or whose base SQL
// Server has moved on since (a full backup taken by another tool).
func (Driver) DifferentialBase(ctx context.Context, s *store.Server, base *store.Run, dbs []dbconn.DBInfo) error {
	if base.Meta["copy_only"] == true {
		return fmt.Errorf("backup #%d is copy-only: it cannot be the base of a differential", base.ID)
	}
	hdrs := headersOf(base.Meta)
	if hdrs == nil {
		return fmt.Errorf("backup #%d records no backup headers", base.ID)
	}
	db, err := Open(ctx, s, "")
	if err != nil {
		return err
	}
	defer db.Close()
	for _, d := range dbs {
		h := hdrs[d.Name]
		if h == nil || h.BackupSetGUID == "" {
			return fmt.Errorf("database %s is not in backup #%d", d.Name, base.ID)
		}
		cur := queryString(ctx, db, "SELECT CAST(differential_base_guid AS nvarchar(64)) FROM "+QuoteName(d.Name)+".sys.database_files WHERE file_id = 1")
		if cur == "" {
			continue // not visible to this account: BACKUP's own header is checked afterwards
		}
		if !strings.EqualFold(cur, h.BackupSetGUID) {
			return fmt.Errorf("the differential base of database %s is no longer backup #%d: a full backup was taken outside DumpCat since (a maintenance plan, another tool) — give that one COPY_ONLY", d.Name, base.ID)
		}
	}
	return nil
}
