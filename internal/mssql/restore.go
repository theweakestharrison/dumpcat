package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/store"
	"dumpcat/internal/sysinfo"
)

// Restore restores native backups into spec.Target: per database the full
// backup, the differential of the run when it is one, and the log backups
// of a point-in-time recovery. Every file is first copied into the target's
// backup folder and checked there (RESTORE VERIFYONLY, the header must be
// the backup set the catalog recorded) before the database is touched.
func (Driver) Restore(env *engine.Env, src engine.RestoreSource, spec *engine.RestoreSpec) error {
	if spec.Run.Method != store.MethodMSSQL {
		return fmt.Errorf("this backup (%s) cannot be restored by the SQL Server driver", spec.Run.Method)
	}
	if len(spec.Databases) == 0 {
		return errors.New("no databases selected")
	}
	ctx := env.Ctx
	db, err := Open(ctx, spec.Target, "")
	if err != nil {
		return err
	}
	defer db.Close()
	in, err := inspect(ctx, db, spec.Target, false)
	if err != nil {
		return err
	}
	if in.folderErr != nil {
		return in.folderErr
	}
	chain := append(append([]engine.ChainRun{}, spec.Chain...), engine.ChainRun{Run: spec.Run, Source: src})
	if !store.Chained(spec.Run.Type) {
		chain = chain[len(chain)-1:]
	} else if len(chain) < 2 || chain[0].Run.Type != store.TypeFull {
		return errors.New("the full backup of this differential is missing")
	}
	var rs *replayState
	if spec.Replay != nil {
		var ok bool
		if rs, ok = spec.Replay.State.(*replayState); !ok {
			return errors.New("the point-in-time recovery was not prepared by the SQL Server driver")
		}
	}
	env.Log.Printf("Source: %s → server %s (%s %s)", src.Describe(), spec.Target.Name, versionName(in.Major), in.Version)
	env.Log.Printf("Backup folder of the target: %s (DumpCat writes it as %s)", in.folder.Server, in.folder.Local)
	var plans []*dbRestore
	targets := map[string]bool{}
	for _, e := range spec.Databases {
		p, err := planDatabase(ctx, db, in, chain, rs, e, spec)
		if err != nil {
			return err
		}
		if targets[strings.ToLower(p.target)] {
			return fmt.Errorf("two databases would be restored as %s", p.target)
		}
		targets[strings.ToLower(p.target)] = true
		plans = append(plans, p)
	}
	// nothing is changed before every file proved readable with this passphrase
	src.SetPhase("prepare")
	for _, part := range chain {
		var files []store.BackupFile
		for _, p := range plans {
			for _, f := range p.files {
				if f.part == part.Run {
					files = append(files, f.file)
				}
			}
		}
		if err := part.Source.Preflight(ctx, files); err != nil {
			return err
		}
	}
	var total int64
	for _, p := range plans {
		total += p.bytes()
	}
	// every database's files are checked before the first one is replaced,
	// unless the folder cannot hold them all at once
	together := true
	if _, free, err := sysinfo.DiskUsage(in.folder.Local); err == nil && free < total+total/10 {
		together = false
		env.Log.Printf("The backup folder has %s free for %s of backup files: each database is copied, checked and restored in turn", engine.HumanBytes(free), engine.HumanBytes(total))
	}
	defer func() {
		for _, p := range plans {
			p.cleanup()
		}
	}()
	src.SetPhase("restore")
	src.SetEstimate(total)
	if together {
		for _, p := range plans {
			if err := p.stage(ctx, env, db, in); err != nil {
				return err
			}
		}
	}
	for _, p := range plans {
		if !together {
			if err := p.stage(ctx, env, db, in); err != nil {
				return err
			}
		}
		if err := p.restore(ctx, env, db, in, spec); err != nil {
			return fmt.Errorf("%s: %w", p.target, err)
		}
		p.cleanup()
	}
	return nil
}

// stagedFile is one backup file of a database restore.
type stagedFile struct {
	part   *store.Run // the backup it belongs to
	src    engine.RestoreSource
	file   store.BackupFile // its file in that backup (zero for a log backup)
	log    *engine.LocalLog // a fetched log backup
	hdr    *Header          // what the catalog recorded
	name   string           // its name in the backup folder ("" until copied)
	kind   string           // full, differential, log
	folder folder
}

func (f *stagedFile) label() string {
	if f.log != nil {
		return "log backup " + f.log.Name
	}
	return fmt.Sprintf("%s backup #%d (%s)", f.kind, f.part.ID, f.file.Name)
}

// dbRestore is the restore of one database.
type dbRestore struct {
	source   store.DBEntry
	target   string
	exists   bool
	files    []*stagedFile // full, differential, then log backups
	stop     *engine.PITRTarget
	existing map[string]string // logical name → physical file of the database being replaced
}

func (p *dbRestore) bytes() int64 {
	var n int64
	for _, f := range p.files {
		if f.log != nil && f.log.File != nil {
			n += f.log.File.SizeBytes
		} else {
			n += max(f.file.RawSize, f.file.Size)
		}
	}
	return n
}

func (p *dbRestore) cleanup() {
	for _, f := range p.files {
		if f.name != "" {
			removeLocal(f.folder.local(f.name))
			f.name = ""
		}
	}
}

// planDatabase finds the files of one database in the chain and checks
// the target name before anything is copied.
func planDatabase(ctx context.Context, db *sql.DB, in *instance, chain []engine.ChainRun, rs *replayState, e store.DBEntry, spec *engine.RestoreSpec) (*dbRestore, error) {
	p := &dbRestore{source: e, target: spec.TargetName(e.Name)}
	if err := (Driver{}).CheckName(p.target); err != nil {
		return nil, err
	}
	for _, part := range chain {
		var entry *store.DBEntry
		for i := range part.Run.Databases {
			if part.Run.Databases[i].Name == e.Name {
				entry = &part.Run.Databases[i]
			}
		}
		if entry == nil {
			return nil, fmt.Errorf("database %s is not in backup #%d", e.Name, part.Run.ID)
		}
		f, ok := part.Source.Files()[pipeline.StripExt(entry.File)]
		if !ok {
			return nil, fmt.Errorf("the file of database %s is missing from backup #%d", e.Name, part.Run.ID)
		}
		kind := "full"
		if part.Run.Type == store.TypeDifferential {
			kind = "differential"
		}
		p.files = append(p.files, &stagedFile{part: part.Run, src: part.Source, file: f, hdr: headersOf(part.Run.Meta)[e.Name], kind: kind, folder: in.folder})
	}
	if rs != nil {
		if why := rs.problems[e.Name]; why != "" {
			return nil, fmt.Errorf("database %s cannot be recovered to %s: %s", e.Name, rs.target.Describe(time.Local), why)
		}
		if _, ok := rs.ids[e.Name]; !ok {
			return nil, fmt.Errorf("database %s: backup #%d records no LSNs for it, so no log backups can follow it", e.Name, spec.Run.ID)
		}
		for i := range rs.files[e.Name] {
			l := rs.files[e.Name][i]
			p.files = append(p.files, &stagedFile{log: &l, hdr: logHeader(l.File.Meta), kind: "log", folder: in.folder})
		}
		t := rs.target
		p.stop = &t
	}
	var state sql.NullString
	err := db.QueryRowContext(ctx, "SELECT state_desc FROM sys.databases WHERE name = @p1", p.target).Scan(&state)
	switch {
	case err == nil:
		p.exists = true
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if p.exists && !spec.DropExisting {
		return nil, fmt.Errorf("database %s exists on %s: tick \"Replace existing databases\" to overwrite it, or restore under another name", p.target, spec.Target.Name)
	}
	if p.exists {
		p.existing = map[string]string{}
		rows, err := db.QueryContext(ctx, "SELECT name, physical_name FROM sys.master_files WHERE database_id = DB_ID(@p1)", p.target)
		if err == nil {
			for rows.Next() {
				var n, ph string
				if rows.Scan(&n, &ph) == nil {
					p.existing[strings.ToLower(n)] = ph
				}
			}
			rows.Close()
		}
	}
	return p, nil
}

// stage copies the database's files into the backup folder and checks each
// one there.
func (p *dbRestore) stage(ctx context.Context, env *engine.Env, db *sql.DB, in *instance) error {
	lg := env.Log
	for _, f := range p.files {
		name := fileName(p.source.Name, map[bool]string{true: ".trn", false: FileSuffix}[f.log != nil])
		lp := f.folder.local(name)
		if f.log != nil {
			r, err := os.Open(f.log.Path)
			if err != nil {
				return err
			}
			_, err = writeLocal(lp, r)
			r.Close()
			if err != nil {
				return fmt.Errorf("copying %s into the backup folder: %w", f.label(), err)
			}
		} else {
			lg.Printf("Downloading %s for %s (%s)", f.label(), p.source.Name, engine.HumanBytes(f.file.Size))
			rd, done, err := f.src.Open(ctx, f.file)
			if err != nil {
				return err
			}
			_, err = writeLocal(lp, rd)
			if derr := done(err == nil); err == nil && derr != nil {
				removeLocal(lp)
				return fmt.Errorf("%s: %w", f.label(), derr) // not the file that was backed up
			}
			if err != nil {
				return fmt.Errorf("copying %s into the backup folder: %w", f.label(), err)
			}
		}
		f.name = name
		sp := f.folder.server(name)
		h, err := readHeader(ctx, db, sp, in.offset)
		if err != nil {
			if hasError(err, 3201) {
				return deviceError(err, f.folder, sp, in.ServiceAccount)
			}
			if !hasError(err, 262, 229, 297, 300, 15247) {
				return fmt.Errorf("%s: %s", f.label(), errText(err))
			}
			lg.Printf("NOTE: the restore account may not read backup headers: %s is checked by RESTORE VERIFYONLY only", f.label())
		} else if f.hdr != nil && f.hdr.BackupSetGUID != "" && !strings.EqualFold(h.BackupSetGUID, f.hdr.BackupSetGUID) {
			return fmt.Errorf("%s holds backup set %s, the catalog recorded %s", f.label(), h.BackupSetGUID, f.hdr.BackupSetGUID)
		} else if f.hdr == nil {
			f.hdr = h
		}
		vq := "RESTORE VERIFYONLY FROM DISK = " + QuoteString(sp)
		if f.hdr != nil && f.hdr.Checksums {
			vq += " WITH CHECKSUM"
		}
		if err := exec(ctx, db, vq, nil); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s is not restorable (RESTORE VERIFYONLY): %s", f.label(), errText(err))
		}
	}
	lg.Printf("Checked %d file(s) of %s on the target (RESTORE VERIFYONLY)", len(p.files), p.source.Name)
	return nil
}

// restore runs the RESTORE statements of one database.
func (p *dbRestore) restore(ctx context.Context, env *engine.Env, db *sql.DB, in *instance, spec *engine.RestoreSpec) error {
	lg := env.Log
	t0 := time.Now()
	full := p.files[0]
	list, err := fileList(ctx, db, full.folder.server(full.name))
	if err != nil {
		return fmt.Errorf("reading the file list of %s: %s", full.label(), errText(err))
	}
	moves := placeFiles(list, p.target, in.dataDir, in.logDir, p.existing, takenFiles(ctx, db, p.target))
	if p.exists {
		q := "ALTER DATABASE " + QuoteName(p.target) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE"
		lg.Printf("%s", q)
		if err := exec(ctx, db, q, nil); err != nil {
			lg.Printf("NOTE: %s (the database is not online: restoring over it)", errText(err))
		}
	}
	notice := func(m string) { lg.Printf("[%s] %s", p.target, m) }
	last := len(p.files) - 1
	recovered := false
	for i, f := range p.files {
		sp := f.folder.server(f.name)
		var with []string
		switch {
		case i == 0:
			for _, e := range list {
				with = append(with, "MOVE "+QuoteString(e.Logical)+" TO "+QuoteString(moves[e.Logical]))
			}
			if p.exists {
				with = append(with, "REPLACE")
			}
		}
		if f.hdr != nil && f.hdr.Checksums {
			with = append(with, "CHECKSUM")
		}
		stopHere := f.log != nil && p.stop != nil && p.stop.Type != "latest"
		switch {
		case stopHere:
			with = append(with, stopClause(*p.stop, in.offset), "RECOVERY")
		case i == last && p.stop == nil:
			with = append(with, "RECOVERY")
		default:
			with = append(with, "NORECOVERY")
		}
		with = append(with, "STATS = 10")
		verb := "DATABASE"
		if f.log != nil {
			verb = "LOG"
		}
		q := "RESTORE " + verb + " " + QuoteName(p.target) + " FROM DISK = " + QuoteString(sp) + " WITH " + strings.Join(with, ", ")
		lg.Printf("%s", q)
		if err := exec(ctx, db, q, notice); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if p.exists && i == 0 {
				_ = exec(context.Background(), db, "ALTER DATABASE "+QuoteName(p.target)+" SET MULTI_USER", nil)
			}
			return fmt.Errorf("%s: %s", f.label(), errText(err))
		}
		removeLocal(f.folder.local(f.name))
		f.name = ""
		if stopHere {
			if st := queryString(ctx, db, "SELECT state_desc FROM sys.databases WHERE name = @p1", p.target); st == "ONLINE" {
				lg.Printf("%s reached %s in %s and is online", p.target, p.stop.Describe(env.Location()), f.label())
				recovered = true
				break
			}
		}
		if i == last && p.stop == nil {
			recovered = true
		}
	}
	if !recovered {
		if p.stop != nil && p.stop.Type == "mark" {
			return fmt.Errorf("the archived log has no marked transaction %q: %s is left restoring (RESTORE DATABASE %s WITH RECOVERY brings it online at the end of the log)",
				p.stop.Name, p.target, QuoteName(p.target))
		}
		if p.stop != nil && p.stop.Type != "latest" {
			lg.Printf("The archived log ends before %s: %s is recovered at its end", p.stop.Describe(env.Location()), p.target)
		}
		q := "RESTORE DATABASE " + QuoteName(p.target) + " WITH RECOVERY"
		lg.Printf("%s", q)
		if err := exec(ctx, db, q, notice); err != nil {
			return errors.New(errText(err))
		}
	}
	lg.Printf("Done %s → %s in %s", p.source.Name, p.target, engine.HumanDuration(time.Since(t0)))
	reportOrphans(ctx, db, p.target, lg)
	return nil
}

// stopClause is the STOPAT / STOPATMARK option of a recovery target: times
// in the server's local time.
func stopClause(t engine.PITRTarget, offset time.Duration) string {
	switch t.Type {
	case "time":
		return "STOPAT = " + QuoteString(t.Time.UTC().Add(offset).Format("2006-01-02T15:04:05.000"))
	case "mark":
		return "STOPATMARK = " + QuoteString(t.Name)
	case "lsn":
		return "STOPATMARK = " + QuoteString("lsn:"+t.LSN)
	}
	return ""
}

// placeFiles chooses where the restored database's files go: the files of
// the database it replaces, else the instance's default data and log
// folders, named after the database.
func placeFiles(list []FileListEntry, target, dataDir, logDir string, existing map[string]string, taken map[string]bool) map[string]string {
	out := map[string]string{}
	base := engine.SafeFileName(target)
	logs := 0
	for _, f := range list {
		if f.Type == "L" {
			logs++
		}
	}
	firstLog := true
	for _, f := range list {
		if p, ok := existing[strings.ToLower(f.Logical)]; ok {
			out[f.Logical] = p
			continue
		}
		dir, name := dataDir, base
		switch {
		case f.Type == "L":
			dir = logDir
			if firstLog && logs >= 1 {
				name += "_log.ldf"
			} else {
				name += "_" + engine.SafeFileName(f.Logical) + ".ldf"
			}
			firstLog = false
		case f.Type == "D" && f.FileID == 1:
			name += ".mdf"
		case f.Type == "D":
			name += "_" + engine.SafeFileName(f.Logical) + ".ndf"
		default: // FILESTREAM, memory-optimized and full-text containers are folders
			name += "_" + engine.SafeFileName(f.Logical)
		}
		p := joinServer(dir, name)
		for k := 2; taken[strings.ToLower(p)]; k++ {
			stem, ext := name, ""
			if i := strings.LastIndex(name, "."); i > 0 {
				stem, ext = name[:i], name[i:]
			}
			p = joinServer(dir, fmt.Sprintf("%s_%d%s", stem, k, ext))
		}
		taken[strings.ToLower(p)] = true
		out[f.Logical] = p
	}
	return out
}

// takenFiles lists the files of the other databases of the server.
func takenFiles(ctx context.Context, db *sql.DB, except string) map[string]bool {
	out := map[string]bool{}
	rows, err := db.QueryContext(ctx, "SELECT physical_name FROM sys.master_files WHERE database_id <> ISNULL(DB_ID(@p1), 0)", except)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out[strings.ToLower(p)] = true
		}
	}
	return out
}

// reportOrphans lists the users of a restored database whose login does not
// exist on this server.
func reportOrphans(ctx context.Context, db *sql.DB, database string, lg interface{ Printf(string, ...any) }) {
	q := "SELECT dp.name FROM " + QuoteName(database) + ".sys.database_principals dp LEFT JOIN sys.server_principals sp ON sp.sid = dp.sid " +
		"WHERE dp.type IN ('S', 'U', 'G') AND dp.principal_id > 4 AND dp.sid IS NOT NULL AND DATALENGTH(dp.sid) > 0 AND sp.sid IS NULL " +
		"AND dp.authentication_type IN (1, 3)"
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		lg.Printf("NOTE: users of %s without a login on this server (orphaned): %s — ALTER USER [name] WITH LOGIN = [login] connects one to a login", database, listNames(names))
	}
	if queryString(ctx, db, "SELECT CASE WHEN SUSER_SNAME(owner_sid) IS NULL THEN '1' END FROM sys.databases WHERE name = @p1", database) == "1" {
		lg.Printf("NOTE: the owner of %s is not a login of this server: ALTER AUTHORIZATION ON DATABASE::%s TO [login] gives it one", database, QuoteName(database))
	}
}
