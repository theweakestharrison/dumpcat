package mssql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// Log archiving for SQL Server is BACKUP LOG on a schedule: every N minutes
// (the job's interval) each database of the job in the FULL or BULK_LOGGED
// recovery model gets a log backup, written into the backup folder, moved
// into DumpCat's staging folder and archived like the other engines' log
// files. "Close now" takes one at once. The log chain is the LSNs of the
// headers: each log backup starts where the previous one ended.

// DefaultLogMinutes is the interval of a job that sets none.
const DefaultLogMinutes = 15

func (Driver) LogInfo() engine.LogInfo {
	return engine.LogInfo{Name: "transaction log", File: "log backup", Files: "log backups", Prefix: "trn",
		Targets: []string{"time", "mark", "lsn", "latest"}, Close: "BACKUP LOG", Recovery: true}
}

func (Driver) NewStream(h *engine.LogHost) engine.LogStream { return &logStream{h: h} }

// logStream takes the log backups of one job.
type logStream struct {
	h    *engine.LogHost
	mu   sync.Mutex
	last time.Time // the last round of log backups: a new session keeps the schedule
}

// staged names: <seq>-<database>.trn, with the header in <name>.json
var reStaged = regexp.MustCompile(`^(\d{8,})-.+\.trn$`)

func (s *logStream) Session(ctx context.Context, job *store.Job, server *store.Server, rotate <-chan struct{}) error {
	h := s.h
	db, err := Open(ctx, server, "")
	if err != nil {
		return err
	}
	defer db.Close()
	in, err := inspect(ctx, db, server, false)
	if err != nil {
		return err
	}
	if in.folderErr != nil {
		return in.folderErr
	}
	every := time.Duration(job.Options.LogRotateMinutes) * time.Minute
	if every <= 0 {
		every = DefaultLogMinutes * time.Minute
	}
	now := time.Now()
	h.Update(func(st *engine.LogStatus) {
		st.State, st.Since, st.Error, st.ErrorAt, st.NextTry = "streaming", &now, "", nil, nil
	})
	h.Log.Printf("Log backups every %s into %s", every, in.folder.describe("reads"))
	if s.last.IsZero() {
		if f := h.Store.LastLogFile(job.UUID); f != nil {
			s.last = f.CreatedAt
		}
	}
	warned := map[string]bool{}
	for {
		wait := time.Until(s.last.Add(every))
		if wait > 0 {
			next := time.Now().Add(wait)
			h.Update(func(st *engine.LogStatus) { st.NextTry = &next })
			select {
			case <-ctx.Done():
				return nil
			case <-rotate:
			case <-time.After(wait):
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		s.last = time.Now()
		if err := s.backupAll(ctx, db, in, job, warned); err != nil {
			return err
		}
	}
}

// backupAll takes one log backup of every database of the job that has a log chain.
func (s *logStream) backupAll(ctx context.Context, db *sql.DB, in *instance, job *store.Job, warned map[string]bool) error {
	h := s.h
	dbs, err := listDatabases(ctx, db, false)
	if err != nil {
		return err
	}
	var failed []string
	took := 0
	for _, d := range jobDatabases(job, dbs) {
		if d.RecoveryModel == "SIMPLE" {
			if !warned[d.Name] {
				h.Log.Printf("WARNING: %s uses the SIMPLE recovery model: no log backups (ALTER DATABASE %s SET RECOVERY FULL, then a full backup)", d.Name, QuoteName(d.Name))
				warned[d.Name] = true
			}
			continue
		}
		delete(warned, d.Name)
		if err := s.backupLog(ctx, db, in, d.Name); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			h.Log.Printf("ERROR: %s: %v", d.Name, err)
			failed = append(failed, d.Name+": "+err.Error())
			continue
		}
		took++
	}
	now := time.Now()
	h.Update(func(st *engine.LogStatus) {
		st.CheckedAt, st.LagBytes, st.LagSeconds = &now, 0, 0
		if len(failed) == 0 {
			st.CaughtUpAt, st.RotatedAt = &now, &now
		}
		st.ServerPosition = fmt.Sprintf("%d database(s)", took)
	})
	if took > 0 {
		h.Kick()
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// jobDatabases are the databases of the job that log backups cover.
func jobDatabases(job *store.Job, all []dbconn.DBInfo) []dbconn.DBInfo {
	byName := map[string]dbconn.DBInfo{}
	for _, d := range all {
		byName[d.Name] = d
	}
	var out []dbconn.DBInfo
	switch job.DBMode {
	case "include":
		for _, n := range job.Databases {
			if d, ok := byName[n]; ok && d.State == "ONLINE" && !strings.EqualFold(n, "tempdb") {
				out = append(out, d)
			}
		}
	default:
		ex := map[string]bool{}
		if job.DBMode == "exclude" {
			for _, n := range job.Databases {
				ex[n] = true
			}
		}
		for _, d := range all {
			if !d.System && !ex[d.Name] {
				out = append(out, d)
			}
		}
	}
	return out
}

// backupLog takes one log backup and stages it.
func (s *logStream) backupLog(ctx context.Context, db *sql.DB, in *instance, database string) error {
	h := s.h
	name := fileName(database, ".trn")
	sp := in.folder.server(name)
	defer in.folder.remove(name)
	with := []string{"CHECKSUM", "FORMAT", "INIT", "SKIP"}
	if in.Compression {
		with = append(with, "COMPRESSION")
	}
	q := "BACKUP LOG " + QuoteName(database) + " TO DISK = " + QuoteString(sp) + " WITH " + strings.Join(with, ", ")
	if err := exec(ctx, db, q, nil); err != nil {
		switch {
		case hasError(err, 4214):
			return errors.New("SQL Server has no full backup of this database since its log chain began: run a full backup first")
		case hasError(err, 4208):
			return errors.New("the database uses the SIMPLE recovery model: no log backups")
		case hasError(err, 3201, 3013) && osError(err) != 0:
			return deviceError(err, in.folder, sp, in.ServiceAccount)
		}
		return errors.New(errText(err))
	}
	hdr, err := header(ctx, db, sp, in.offset)
	if err != nil {
		return fmt.Errorf("reading the log backup's header: %s", errText(err))
	}
	if ms, err := marks(ctx, db, database, hdr.FirstLSN, hdr.LastLSN, in.offset); err == nil {
		hdr.Marks = ms
	} else {
		hdr.Marks = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.nextSeq()
	staged := fmt.Sprintf("%08d-%s.trn", seq, engine.SafeFileName(database))
	// the header first: a staged file without it would never be uploaded
	b, _ := json.Marshal(hdr)
	if err := os.WriteFile(filepath.Join(h.Dir, staged+".json"), b, 0o600); err != nil {
		return err
	}
	if err := moveIn(ctx, in.folder, name, filepath.Join(h.Dir, staged)); err != nil {
		os.Remove(filepath.Join(h.Dir, staged+".json"))
		return fmt.Errorf("moving the log backup into the staging folder: %w", err)
	}
	h.Log.Printf("BACKUP LOG %s: LSN %s → %s (%s)", database, hdr.FirstLSN, hdr.LastLSN, staged)
	at := hdr.Start
	h.Update(func(st *engine.LogStatus) {
		st.Current, st.ReceivedAt = staged, &at
	})
	return nil
}

// nextSeq numbers log backups after the newest archived or staged one.
func (s *logStream) nextSeq() int64 {
	var n int64
	if f := s.h.Store.LastLogFile(s.h.JobUUID); f != nil {
		n = f.Seq
	}
	for _, name := range s.staged() {
		if m := reStaged.FindStringSubmatch(name); m != nil {
			if v, _ := strconv.ParseInt(m[1], 10, 64); v > n {
				n = v
			}
		}
	}
	return n + 1
}

func (s *logStream) staged() []string {
	entries, err := os.ReadDir(s.h.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && reStaged.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out) // the sequence number leads
	return out
}

// moveIn moves a log backup from the backup folder into staging (a copy
// when they are on different file systems, or the agent reads it).
func moveIn(ctx context.Context, f folder, name, dst string) error {
	if d, ok := f.Files.(localFiles); ok {
		if err := os.Rename(d.path(name), dst); err == nil {
			return nil
		}
	}
	in, size, err := f.Files.Open(ctx, name)
	if err != nil {
		return err
	}
	defer in.Close()
	part := dst + ".part" // never listed: a crash cannot leave half a log backup to upload
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, in)
	if err == nil && n != size {
		err = fmt.Errorf("read %d of %d bytes", n, size)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, dst)
	}
	if err != nil {
		os.Remove(part)
	}
	return err
}

func (s *logStream) Closed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, n := range s.staged() {
		if _, err := os.Stat(filepath.Join(s.h.Dir, n+".json")); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *logStream) Seal(ctx context.Context, job *store.Job, name string) (*engine.LogFileInfo, error) {
	b, err := os.ReadFile(filepath.Join(s.h.Dir, name+".json"))
	if err != nil {
		return nil, err
	}
	var hdr Header
	if err := json.Unmarshal(b, &hdr); err != nil {
		return nil, err
	}
	fi, err := os.Stat(filepath.Join(s.h.Dir, name))
	if err != nil {
		return nil, err
	}
	m := reStaged.FindStringSubmatch(name)
	seq, _ := strconv.ParseInt(m[1], 10, 64)
	// the log backup holds what was logged since the previous one up to its start
	first := hdr.Start
	if prev := s.previous(job.UUID, hdr.Database, seq); prev != nil && !prev.LastTS.IsZero() {
		first = prev.LastTS
	}
	return &engine.LogFileInfo{Seq: seq, First: first, Last: hdr.Start, Size: fi.Size(), Meta: headerMeta(&hdr)}, nil
}

// previous is the newest archived log backup of database before seq.
func (s *logStream) previous(jobUUID, database string, seq int64) *store.LogFile {
	files, err := s.h.Store.ListLogFiles(jobUUID)
	if err != nil {
		return nil
	}
	var best *store.LogFile
	for i := range files {
		f := &files[i]
		if f.Seq < seq && f.Meta["database"] == database && (best == nil || f.Seq > best.Seq) {
			best = f
		}
	}
	return best
}

func (s *logStream) Archived(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Remove(filepath.Join(s.h.Dir, name))
	os.Remove(filepath.Join(s.h.Dir, name+".json"))
}

// Current: a log backup is complete when it is staged, nothing is received.
func (s *logStream) Current() engine.LogCurrent { return engine.LogCurrent{Status: s.h.Status()} }

func (s *logStream) Snapshot(dst string) (*engine.LogSnapshot, error) {
	return nil, errors.New("SQL Server has no log file being received: close the log now to back it up")
}

// Release: nothing is kept on the server, but the log of a database in the
// FULL recovery model now grows until something backs it up again.
func (s *logStream) Release(ctx context.Context, server *store.Server, lg *tasklog.Logger) {
	lg.Printf("Log backups stopped: the transaction log of databases in the FULL recovery model keeps growing until another log backup runs (or the database switches to SIMPLE)")
}
