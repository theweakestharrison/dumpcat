package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/notify"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

type RestoreRequest struct {
	LocationID     int64             `json:"location_id"`
	TargetServerID int64             `json:"target_server_id"`
	Databases      []string          `json:"databases"`
	Rename         map[string]string `json:"rename"`
	DropExisting   bool              `json:"drop_existing"`
	NoBinlog       bool              `json:"no_binlog"`
	FastMode       bool              `json:"fast_mode"`
	RestoreUsers   bool              `json:"restore_users"`
	Threads        int               `json:"threads"`
	Passphrase     string            `json:"passphrase,omitempty"`
	// physical
	TargetDir   string `json:"target_dir"`
	Apply       bool   `json:"apply"`
	ServiceName string `json:"service_name"`
	Datadir     string `json:"datadir"`
	MoveBack    bool   `json:"move_back"`
	UseMemory   string `json:"use_memory"`
	OSUser      string `json:"os_user"`
	// point-in-time recovery: replay archived binlogs after the restore
	PITR *PITRTarget `json:"pitr,omitempty"`
}

func notifyEvent(kind, status, title, errMsg string) notify.Event {
	return notify.Event{Kind: kind, Status: status, Title: title, Error: errMsg}
}

// StartRestore validates and launches a restore task.
func (r *Runner) StartRestore(runID int64, req RestoreRequest, userID int64, username string) (*store.Restore, error) {
	if err := r.beginStart(); err != nil {
		return nil, err
	}
	defer r.endStart()
	run, err := r.Store.GetRun(runID)
	if err != nil {
		return nil, err
	}
	if run.Status != store.StatusSuccess && run.Status != store.StatusPartial {
		return nil, errors.New("only successful backups can be restored")
	}
	if run.Encrypted && run.Passphrase == "" && req.Passphrase == "" {
		return nil, errors.New("the backup is encrypted: a passphrase is required")
	}
	var target *store.Server
	if req.TargetServerID > 0 {
		target, err = r.Store.GetServer(req.TargetServerID)
		if err != nil {
			return nil, fmt.Errorf("target server: %w", err)
		}
		target = target.ForRestore()
	} else if run.Method != store.MethodPhysical {
		return nil, errors.New("no target server selected")
	}
	kind := "logical"
	if run.Method == store.MethodPhysical {
		kind = "physical"
		if req.Apply && runtime.GOOS != "linux" {
			return nil, errors.New("automatic datadir replacement is only supported on Linux")
		}
	}
	if run.Method != store.MethodPhysical && run.Meta["single_file"] == true {
		if len(req.Databases) > 0 && len(req.Databases) != len(run.Databases) {
			return nil, errors.New("a single-file backup can only be restored in full")
		}
		for k, v := range req.Rename {
			if v != "" && v != k {
				return nil, errors.New("a single-file backup does not support renaming databases")
			}
		}
	}
	for _, dst := range req.Rename {
		dst = strings.TrimSpace(dst)
		if dst == "" {
			continue
		}
		// a leading '-' would be parsed as a client option
		if strings.HasPrefix(dst, "-") || len(dst) > 64 || strings.ContainsRune(dst, 0) {
			return nil, fmt.Errorf("invalid target database name: %q", dst)
		}
	}
	var plan *pitrPlan
	if req.PITR != nil {
		if err := req.PITR.Normalize(); err != nil {
			return nil, err
		}
		if run.Method == store.MethodPhysical && (!req.Apply || target == nil) {
			return nil, errors.New("PITR from a physical backup requires 'Apply to MySQL now' and a target server, so binlogs can be replayed once it is running")
		}
		if run.Type == store.TypeIncremental && !r.chainIntact(run) {
			return nil, errors.New("the incremental chain of this backup is no longer complete")
		}
		plan, err = r.planPITR(run, *req.PITR)
		if err != nil {
			return nil, fmt.Errorf("PITR: %w", err)
		}
	}
	opts := req
	opts.Passphrase = ""
	optsJSON, _ := json.Marshal(opts)
	rs := &store.Restore{RunUUID: run.UUID, JobName: run.JobName, Kind: kind, Options: optsJSON, Status: store.StatusRunning, Username: username}
	rid := run.ID
	rs.RunID = &rid
	if target != nil {
		tid := target.ID
		rs.TargetServerID, rs.TargetServerName = &tid, target.Name
	}
	if userID > 0 {
		uid := userID
		rs.UserID = &uid
	}
	if err := r.Store.CreateRestore(rs); err != nil {
		return nil, err
	}
	ids := []int64{run.ID}
	if chain, err := r.backupChain(run); err == nil {
		ids = ids[:0]
		for _, c := range chain {
			ids = append(ids, c.ID)
		}
	}
	release := r.useRuns(ids...)
	releaseBinlogs := func() {}
	title := "Restore " + run.JobName + " " + run.DirName
	if plan != nil {
		releaseBinlogs = r.useBinlogs(plan.fileIDs()...)
		title += " + PITR"
	}
	task, ctx := r.addTask("restore", rs.ID, 0, title)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer release()
		defer releaseBinlogs()
		defer r.removeTask(task)
		r.execRestore(ctx, task, rs, run, target, req, plan)
	}()
	return rs, nil
}

func (r *Runner) execRestore(ctx context.Context, task *Task, rs *store.Restore, run *store.Run, target *store.Server, req RestoreRequest, plan *pitrPlan) {
	lg, err := tasklog.Open(r.RestoreLogPath(rs.ID))
	if err != nil {
		lg = tasklog.Discard()
	}
	defer lg.Close()
	pass := run.Passphrase
	if pass == "" {
		pass = req.Passphrase
	}
	lg.Redact(pass)
	if target != nil {
		lg.Redact(target.Password)
	}
	start := time.Now()
	lg.Printf("DumpCat %s — RESTORE backup #%d \"%s\" %s (%s/%s)", r.AppVersion, run.ID, run.JobName, run.DirName, run.Method, run.Type)
	workDir := filepath.Join(r.stagingRoot(), fmt.Sprintf("work-restore-%d", rs.ID))
	defer os.RemoveAll(workDir)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		r.finishRestore(rs, run, lg, start, err)
		return
	}
	env := &engine.Env{Ctx: ctx, Log: lg, Tools: r.Tools, TmpDir: workDir}
	// PITR: every binlog is fetched and verified, and the stop point
	// resolved, before the base restore touches the target
	var pst *pitrState
	if plan != nil {
		pst, err = r.pitrPrepare(ctx, task, lg, plan, workDir, pass)
		if errors.Is(err, errReplan) {
			lg.Printf("The binlog being written was just closed — planning PITR again")
			if plan, err = r.planPITR(run, plan.Target); err == nil {
				pst, err = r.pitrPrepare(ctx, task, lg, plan, workDir, pass)
			}
		}
		if err != nil {
			r.finishRestore(rs, run, lg, start, fmt.Errorf("PITR: %w", err))
			return
		}
	}
	switch run.Method {
	case store.MethodPhysical:
		err = r.restorePhysical(ctx, task, env, run, target, req, pass)
	case store.MethodMydumper:
		err = r.restoreMydumper(ctx, task, env, run, target, req, pass, workDir)
	default:
		err = r.restoreMysqldump(ctx, task, env, run, target, req, pass)
	}
	if err == nil && pst != nil {
		err = r.pitrApply(ctx, task, env, pst, run, target, req)
	}
	r.finishRestore(rs, run, lg, start, err)
}

func (r *Runner) finishRestore(rs *store.Restore, run *store.Run, lg *tasklog.Logger, start time.Time, err error) {
	status, msg := store.StatusSuccess, ""
	if err != nil {
		status, msg = store.StatusFailed, err.Error()
		if errors.Is(err, context.Canceled) {
			status, msg = store.StatusCanceled, "Canceled"
		}
		lg.Printf("RESTORE FAILED: %s", msg)
	} else {
		lg.Printf("RESTORE SUCCEEDED in %s", humanDuration(time.Since(start)))
	}
	r.Store.FinishRestore(rs.ID, status, msg)
	if r.Notifier != nil && status != store.StatusCanceled {
		title := "Restore succeeded: " + run.JobName
		if status != store.StatusSuccess {
			title = "Restore FAILED: " + run.JobName
		}
		ev := notifyEvent("restore", status, title, msg)
		ev.Add("Backup", run.DirName)
		ev.Add("Target server", rs.TargetServerName)
		ev.Add("User", rs.Username)
		ev.Add("Duration", humanDuration(time.Since(start)))
		ev.URL = r.link(fmt.Sprintf("/#/restores/%d", rs.ID))
		r.Notifier.Send(ev)
	}
}

// openBackupFile opens a stored file and returns a decoded reader. The
// returned done(ok) closes it; with ok=true it first drains the rest of the
// stream to complete the checksum (skipped after a failure).
func (r *Runner) openBackupFile(ctx context.Context, task *Task, b storage.Backend, loc *store.RunLocation, f store.BackupFile, pass string) (io.Reader, func(bool) error, error) {
	rc, err := b.Open(ctx, loc.Path+"/"+f.Name)
	if err != nil {
		return nil, nil, err
	}
	hr := pipeline.NewHashingReader(rc, &task.Raw)
	comp, enc := pipeline.DetectFromName(f.Name)
	dec, cleanup, err := pipeline.NewReader(hr, comp, enc, pass)
	if err != nil {
		rc.Close()
		return nil, nil, err
	}
	done := func(ok bool) error {
		if !ok {
			cleanup()
			rc.Close()
			return nil
		}
		_, derr := io.Copy(io.Discard, hr)
		cleanup()
		rc.Close()
		if derr != nil {
			return derr
		}
		if f.SHA256 != "" && hr.SHA256() != f.SHA256 {
			return fmt.Errorf("WARNING: checksum mismatch for %s", f.Name)
		}
		return nil
	}
	return dec, done, nil
}

func (r *Runner) targetFlavor(ctx context.Context, s *store.Server) (string, error) {
	info, err := dbconn.Inspect(ctx, s, false)
	if err != nil {
		return "", fmt.Errorf("could not connect to the target server: %w", err)
	}
	if s.Flavor != "" && s.Flavor != "auto" {
		return s.Flavor, nil
	}
	return info.Flavor, nil
}

// ---- fast restore mode (global settings, reference counted per server) ----

type fastOrig struct {
	Flush string `json:"flush"`
	Sync  string `json:"sync"`
	count int
}

func fastKey(serverID int64) string { return fmt.Sprintf("fastmode:%d", serverID) }

// enableFastMode relaxes durability on the target while restoring. Original
// values are persisted so they are put back even after a crash/restart.
// The returned func must be called when the restore ends.
func (r *Runner) enableFastMode(ctx context.Context, s *store.Server, lg *tasklog.Logger) func() {
	r.fastMu.Lock()
	defer r.fastMu.Unlock()
	if st, ok := r.fast[s.ID]; ok {
		st.count++
		lg.Printf("Fast mode: already enabled by another restore on this server")
		return func() { r.disableFastMode(s, lg) }
	}
	db, err := dbconn.Open(ctx, s, "")
	if err != nil {
		lg.Printf("Fast mode: could not connect (%v) — skipping", err)
		return func() {}
	}
	defer db.Close()
	st := &fastOrig{count: 1}
	st.Flush, _ = dbconn.GetGlobal(ctx, db, "innodb_flush_log_at_trx_commit")
	st.Sync, _ = dbconn.GetGlobal(ctx, db, "sync_binlog")
	b, _ := json.Marshal(st)
	_ = r.Store.SetKV(fastKey(s.ID), string(b)) // persist before changing anything
	if err := dbconn.SetGlobal(ctx, db, "innodb_flush_log_at_trx_commit", "2"); err != nil {
		_ = r.Store.SetKV(fastKey(s.ID), "")
		lg.Printf("Fast mode: could not set innodb_flush_log_at_trx_commit (%v) — skipping", err)
		return func() {}
	}
	_ = dbconn.SetGlobal(ctx, db, "sync_binlog", "0")
	r.fast[s.ID] = st
	lg.Printf("Fast mode: temporarily set innodb_flush_log_at_trx_commit=2, sync_binlog=0 (will restore %s/%s)", st.Flush, st.Sync)
	return func() { r.disableFastMode(s, lg) }
}

func (r *Runner) disableFastMode(s *store.Server, lg *tasklog.Logger) {
	r.fastMu.Lock()
	defer r.fastMu.Unlock()
	st, ok := r.fast[s.ID]
	if !ok {
		return
	}
	st.count--
	if st.count > 0 {
		return
	}
	delete(r.fast, s.ID)
	if err := applyGlobals(s, st.Flush, st.Sync); err != nil {
		lg.Printf("WARNING: could not restore the fast mode settings (%v) — will retry when DumpCat starts", err)
		return
	}
	_ = r.Store.SetKV(fastKey(s.ID), "")
	lg.Printf("Fast mode: restored innodb_flush_log_at_trx_commit=%s, sync_binlog=%s", st.Flush, st.Sync)
}

func applyGlobals(s *store.Server, flush, sync string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := dbconn.Open(ctx, s, "")
	if err != nil {
		return err
	}
	defer db.Close()
	if flush != "" {
		if err := dbconn.SetGlobal(ctx, db, "innodb_flush_log_at_trx_commit", flush); err != nil {
			return err
		}
	}
	if sync != "" {
		if err := dbconn.SetGlobal(ctx, db, "sync_binlog", sync); err != nil {
			return err
		}
	}
	return nil
}

// RecoverFastMode puts back settings left relaxed by an interrupted restore.
func (r *Runner) RecoverFastMode() {
	servers, err := r.Store.ListServers()
	if err != nil {
		return
	}
	for i := range servers {
		v, ok := r.Store.GetKV(fastKey(servers[i].ID))
		if !ok || v == "" {
			continue
		}
		var st fastOrig
		if json.Unmarshal([]byte(v), &st) != nil {
			continue
		}
		if err := applyGlobals(servers[i].ForRestore(), st.Flush, st.Sync); err == nil {
			_ = r.Store.SetKV(fastKey(servers[i].ID), "")
			log.Printf("fast mode: restored the original settings on server %s", servers[i].Name)
		} else {
			log.Printf("fast mode: could not restore the settings on server %s yet: %v", servers[i].Name, err)
		}
	}
}

// checkNoBinlog fails early when the restore user may not disable binary
// logging, so a "no binlog" restore never silently replicates.
func checkNoBinlog(ctx context.Context, s *store.Server) error {
	db, err := dbconn.Open(ctx, s, "")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "SET SESSION sql_log_bin=0"); err != nil {
		return fmt.Errorf("the restore account is not allowed to disable binlog (requires SUPER / SYSTEM_VARIABLES_ADMIN / BINLOG ADMIN): %w", err)
	}
	return nil
}

// preflight opens every file that will be restored and decodes its first
// bytes: a wrong passphrase, missing file or unreachable storage is detected
// before anything destructive (DROP DATABASE) happens.
func (r *Runner) preflight(ctx context.Context, b storage.Backend, loc *store.RunLocation, files []store.BackupFile, pass string, lg *tasklog.Logger) error {
	for _, f := range files {
		rc, err := b.Open(ctx, loc.Path+"/"+f.Name)
		if err != nil {
			return fmt.Errorf("could not open %s: %w", f.Name, err)
		}
		comp, enc := pipeline.DetectFromName(f.Name)
		dec, cleanup, err := pipeline.NewReader(rc, comp, enc, pass)
		if err == nil {
			var one [1]byte
			if _, rerr := io.ReadFull(dec, one[:]); rerr != nil {
				err = fmt.Errorf("could not read data: %w", rerr)
			}
			cleanup()
		}
		rc.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	lg.Printf("Pre-restore check: %d file(s) opened & decoded", len(files))
	return nil
}

func selectDatabases(run *store.Run, req RestoreRequest) []store.DBEntry {
	if len(req.Databases) == 0 {
		return run.Databases
	}
	want := map[string]bool{}
	for _, d := range req.Databases {
		want[d] = true
	}
	var out []store.DBEntry
	for _, d := range run.Databases {
		if want[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

func targetName(req RestoreRequest, src string) string {
	if v := strings.TrimSpace(req.Rename[src]); v != "" {
		return v
	}
	return src
}

func prepareDatabase(ctx context.Context, s *store.Server, d store.DBEntry, name string, drop, noBinlog bool, lg *tasklog.Logger) error {
	db, err := dbconn.Open(ctx, s, "")
	if err != nil {
		return err
	}
	defer db.Close()
	// a single session so that sql_log_bin=0 also covers DROP/CREATE DATABASE
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if noBinlog {
		if _, err := conn.ExecContext(ctx, "SET SESSION sql_log_bin=0"); err != nil {
			return err
		}
	}
	if drop {
		lg.Printf("DROP DATABASE IF EXISTS %s", dbconn.QuoteIdent(name))
		if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbconn.QuoteIdent(name)); err != nil {
			return err
		}
	}
	q := "CREATE DATABASE IF NOT EXISTS " + dbconn.QuoteIdent(name)
	if d.Charset != "" {
		q += " CHARACTER SET " + d.Charset
	}
	if d.Collation != "" {
		q += " COLLATE " + d.Collation
	}
	_, err = conn.ExecContext(ctx, q)
	return err
}

func (r *Runner) restoreMysqldump(ctx context.Context, task *Task, env *engine.Env, run *store.Run, target *store.Server, req RestoreRequest, pass string) error {
	flavor, err := r.targetFlavor(ctx, target)
	if err != nil {
		return err
	}
	loc, backend, err := r.pickLocation(run, req.LocationID)
	if err != nil {
		return err
	}
	env.Log.Printf("Source: %s (%s) → server %s (%s)", loc.StorageName, loc.Path, target.Name, flavor)
	files := map[string]store.BackupFile{}
	for _, f := range run.Files {
		files[pipeline.StripExt(f.Name)] = f
	}
	var total int64
	if req.NoBinlog {
		if err := checkNoBinlog(ctx, target); err != nil {
			return err
		}
	}
	single := run.Meta["single_file"] == true
	dbs := selectDatabases(run, req)
	var check []store.BackupFile
	if single {
		f, ok := files["all-databases.sql"]
		if !ok {
			return errors.New("all-databases.sql not found in the backup")
		}
		check = append(check, f)
	} else {
		if len(dbs) == 0 {
			return errors.New("no databases selected")
		}
		for _, d := range dbs {
			f, ok := files[pipeline.StripExt(d.File)]
			if !ok {
				return fmt.Errorf("file for database %s not found", d.Name)
			}
			check = append(check, f)
		}
	}
	if f, ok := files["_users.sql"]; ok && req.RestoreUsers {
		check = append(check, f)
	}
	// nothing is dropped before every file proved readable with this passphrase
	task.SetPhase("prepare")
	if err := r.preflight(ctx, backend, loc, check, pass, env.Log); err != nil {
		return err
	}
	if req.FastMode {
		defer r.enableFastMode(ctx, target, env.Log)()
	}
	task.SetPhase("restore")
	if single {
		f := files["all-databases.sql"]
		task.SetEstimate(f.Size)
		if req.DropExisting {
			for _, d := range run.Databases {
				if err := prepareDatabase(ctx, target, d, d.Name, true, req.NoBinlog, env.Log); err != nil {
					return err
				}
			}
		}
		env.Log.Printf("Restore file %s (%s)", f.Name, humanBytes(f.Size))
		rd, done, err := r.openBackupFile(ctx, task, backend, loc, f, pass)
		if err != nil {
			return err
		}
		err = engine.RestoreSQL(env, target, flavor, rd, engine.SQLRestoreOpts{NoBinlog: req.NoBinlog})
		if derr := done(err == nil); err == nil && derr != nil {
			env.Log.Printf("%v", derr)
		}
		if err != nil {
			return err
		}
	} else {
		for _, d := range dbs {
			if f, ok := files[pipeline.StripExt(d.File)]; ok {
				total += f.Size
			}
		}
		task.SetEstimate(total)
		threads := req.Threads
		if threads <= 0 {
			threads = 1
		}
		env.Log.Printf("Restoring %d database(s) (%s compressed), %d in parallel", len(dbs), humanBytes(total), threads)
		err = engine.Parallel(ctx, threads, dbs, func(ctx context.Context, d store.DBEntry) error {
			f, ok := files[pipeline.StripExt(d.File)]
			if !ok {
				return fmt.Errorf("file for database %s not found", d.Name)
			}
			name := targetName(req, d.Name)
			if err := prepareDatabase(ctx, target, d, name, req.DropExisting, req.NoBinlog, env.Log); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			env.Log.Printf("Restore %s → %s (%s)", d.Name, name, humanBytes(f.Size))
			t0 := time.Now()
			rd, done, err := r.openBackupFile(ctx, task, backend, loc, f, pass)
			if err != nil {
				return err
			}
			e2 := *env
			e2.Ctx = ctx
			err = engine.RestoreSQL(&e2, target, flavor, rd, engine.SQLRestoreOpts{Database: name, NoBinlog: req.NoBinlog})
			if derr := done(err == nil); err == nil && derr != nil {
				env.Log.Printf("%v", derr)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			env.Log.Printf("Done %s in %s", name, humanDuration(time.Since(t0)))
			return nil
		})
		if err != nil {
			return err
		}
	}
	return r.restoreUsers(ctx, task, env, run, target, flavor, backend, loc, req, pass, files)
}

func (r *Runner) restoreUsers(ctx context.Context, task *Task, env *engine.Env, run *store.Run, target *store.Server, flavor string,
	backend storage.Backend, loc *store.RunLocation, req RestoreRequest, pass string, files map[string]store.BackupFile) error {
	if !req.RestoreUsers {
		return nil
	}
	f, ok := files["_users.sql"]
	if !ok {
		env.Log.Printf("The backup contains no users/grants — skipping")
		return nil
	}
	env.Log.Printf("Restoring users & grants (--force, errors in individual statements are ignored)")
	rd, done, err := r.openBackupFile(ctx, task, backend, loc, f, pass)
	if err != nil {
		return err
	}
	err = engine.RestoreSQL(env, target, flavor, rd, engine.SQLRestoreOpts{NoBinlog: req.NoBinlog, Force: true})
	done(err == nil)
	if err != nil {
		env.Log.Printf("Some users/grants statements failed: %v", err)
	}
	return nil
}

func (r *Runner) restoreMydumper(ctx context.Context, task *Task, env *engine.Env, run *store.Run, target *store.Server, req RestoreRequest, pass, workDir string) error {
	flavor, err := r.targetFlavor(ctx, target)
	if err != nil {
		return err
	}
	loc, backend, err := r.pickLocation(run, req.LocationID)
	if err != nil {
		return err
	}
	files := map[string]store.BackupFile{}
	for _, f := range run.Files {
		files[pipeline.StripExt(f.Name)] = f
	}
	f, ok := files["mydumper.tar"]
	if !ok {
		return errors.New("mydumper.tar not found in the backup")
	}
	if req.NoBinlog {
		if err := checkNoBinlog(ctx, target); err != nil {
			return err
		}
	}
	dir := filepath.Join(workDir, "dump")
	if total, free, err := diskUsage(workDir); err == nil {
		env.Log.Printf("Staging directory %s: %s free / %s, needs ~%s", workDir, humanBytes(free), humanBytes(total), humanBytes(f.Size))
	}
	task.SetPhase("download")
	task.SetEstimate(f.Size)
	env.Log.Printf("Downloading and extracting %s (%s) from %s", f.Name, humanBytes(f.Size), loc.StorageName)
	rd, done, err := r.openBackupFile(ctx, task, backend, loc, f, pass)
	if err != nil {
		return err
	}
	n, err := engine.UntarTo(rd, dir)
	if derr := done(err == nil); err == nil && derr != nil {
		return derr
	}
	if err != nil {
		return err
	}
	env.Log.Printf("Extracted %d files", n)
	if req.FastMode {
		defer r.enableFastMode(ctx, target, env.Log)()
	}
	task.SetPhase("restore")
	threads := req.Threads
	if threads <= 0 {
		threads = 4
	}
	dbs := selectDatabases(run, req)
	renamed := false
	for _, d := range dbs {
		if targetName(req, d.Name) != d.Name {
			renamed = true
		}
	}
	all := len(dbs) == len(run.Databases) && !renamed
	for _, d := range dbs {
		name := targetName(req, d.Name)
		if req.DropExisting {
			if err := prepareDatabase(ctx, target, d, name, true, req.NoBinlog, env.Log); err != nil {
				return err
			}
		}
	}
	base := engine.MyloaderOpts{Dir: dir, Threads: threads, Overwrite: req.DropExisting, EnableBinlog: !req.NoBinlog, OptimizeKeys: true}
	if all {
		env.Log.Printf("myloader: all %d database(s), %d threads", len(dbs), threads)
		if err := engine.Myloader(env, target, base); err != nil {
			return err
		}
	} else {
		for _, d := range dbs {
			o := base
			o.SourceDB = d.Name
			o.TargetDB = targetName(req, d.Name)
			if !req.DropExisting {
				if err := prepareDatabase(ctx, target, d, o.TargetDB, false, req.NoBinlog, env.Log); err != nil {
					return err
				}
			}
			env.Log.Printf("myloader %s → %s, %d threads", d.Name, o.TargetDB, threads)
			if err := engine.Myloader(env, target, o); err != nil {
				return err
			}
		}
	}
	os.RemoveAll(dir)
	return r.restoreUsers(ctx, task, env, run, target, flavor, backend, loc, req, pass, files)
}

// backupChain returns full → ... → run following parent links.
func (r *Runner) backupChain(run *store.Run) ([]*store.Run, error) {
	chain := []*store.Run{run}
	cur := run
	for cur.Type == store.TypeIncremental {
		if cur.ParentID == nil {
			return nil, fmt.Errorf("backup #%d has no parent", cur.ID)
		}
		p, err := r.Store.GetRun(*cur.ParentID)
		if err != nil {
			return nil, fmt.Errorf("parent backup #%d not found", *cur.ParentID)
		}
		if p.Status != store.StatusSuccess && p.Status != store.StatusPartial {
			return nil, fmt.Errorf("parent backup #%d is no longer available (%s)", p.ID, p.Status)
		}
		chain = append([]*store.Run{p}, chain...)
		cur = p
		if len(chain) > 1000 {
			return nil, errors.New("incremental chain is too long")
		}
	}
	return chain, nil
}

func (r *Runner) restorePhysical(ctx context.Context, task *Task, env *engine.Env, run *store.Run, target *store.Server, req RestoreRequest, pass string) error {
	flavor, _ := run.Meta["flavor"].(string)
	if flavor == "" {
		flavor = "mysql"
	}
	chain, err := r.backupChain(run)
	if err != nil {
		return err
	}
	dir := strings.TrimSpace(req.TargetDir)
	if dir == "" {
		dir = filepath.Join(r.DataDir, "restore", fmt.Sprintf("%s-%s", run.JobSlug, run.DirName))
	}
	dir, _ = filepath.Abs(dir)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("target directory %s is not empty", dir)
	}
	env.Log.Printf("Restore chain: %d backup(s) (1 full + %d incremental), directory: %s", len(chain), len(chain)-1, dir)
	var total int64
	for _, c := range chain {
		total += c.SizeBytes
	}
	if t, free, err := diskUsage(filepath.Dir(dir)); err == nil {
		env.Log.Printf("Free space: %s / %s (compressed data %s; more is needed after extraction)", humanBytes(free), humanBytes(t), humanBytes(total))
	}
	task.SetPhase("download")
	task.SetEstimate(total)
	threads := req.Threads
	if threads <= 0 {
		threads = 4
	}
	fullDir := filepath.Join(dir, "full")
	var incDirs []string
	for i, c := range chain {
		cpass := c.Passphrase
		if cpass == "" {
			cpass = pass
		}
		locID := int64(0)
		if c.ID == run.ID {
			locID = req.LocationID
		}
		loc, backend, err := r.pickLocation(c, locID)
		if err != nil {
			return fmt.Errorf("backup #%d: %w", c.ID, err)
		}
		var f *store.BackupFile
		for j := range c.Files {
			if c.Files[j].Kind == "xbstream" {
				f = &c.Files[j]
			}
		}
		if f == nil {
			return fmt.Errorf("backup #%d has no xbstream file", c.ID)
		}
		d := fullDir
		if i > 0 {
			d = filepath.Join(dir, fmt.Sprintf("inc%02d", i))
			incDirs = append(incDirs, d)
		}
		env.Log.Printf("[%d/%d] Downloading & extracting %s %s (%s) from %s", i+1, len(chain), c.Type, c.DirName, humanBytes(f.Size), loc.StorageName)
		rd, done, err := r.openBackupFile(ctx, task, backend, loc, *f, cpass)
		if err != nil {
			return err
		}
		err = engine.ExtractXbstream(env, flavor, rd, d, threads)
		if derr := done(err == nil); err == nil && derr != nil {
			return derr
		}
		if err != nil {
			return err
		}
	}
	task.SetPhase("prepare")
	useMem := req.UseMemory
	if useMem == "" {
		useMem = "1G"
	}
	if len(incDirs) == 0 {
		env.Log.Printf("Preparing the full backup...")
		if err := engine.Prepare(env, flavor, fullDir, "", false, useMem); err != nil {
			return err
		}
	} else {
		env.Log.Printf("Preparing the full backup (apply-log-only)...")
		if err := engine.Prepare(env, flavor, fullDir, "", true, useMem); err != nil {
			return err
		}
		for i, inc := range incDirs {
			last := i == len(incDirs)-1
			env.Log.Printf("Applying incremental %d/%d...", i+1, len(incDirs))
			if err := engine.Prepare(env, flavor, fullDir, inc, !last, useMem); err != nil {
				return err
			}
			os.RemoveAll(inc)
		}
	}
	env.Log.Printf("The backup has been prepared at: %s", fullDir)
	if !req.Apply {
		env.Log.Printf("Not applied to MySQL. To use it manually:")
		env.Log.Printf("  systemctl stop <mysql|mariadb>; mv <datadir> <datadir>.old; mkdir <datadir>")
		env.Log.Printf("  %s --copy-back --target-dir=%s --datadir=<datadir>", map[bool]string{true: "mariabackup", false: "xtrabackup"}[flavor == "mariadb"], fullDir)
		env.Log.Printf("  chown -R mysql:mysql <datadir>; systemctl start <mysql|mariadb>")
		return nil
	}
	return r.applyPhysical(ctx, task, env, flavor, fullDir, target, req, threads)
}

func runSimple(ctx context.Context, lg *tasklog.Logger, name string, args ...string) error {
	lg.Printf("$ %s", execx.Quote(name, args))
	cmd := execx.Command(ctx, name, args...)
	w := lg.Writer("[" + filepath.Base(name) + "] ")
	defer w.Close()
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

func (r *Runner) applyPhysical(ctx context.Context, task *Task, env *engine.Env, flavor, fullDir string, target *store.Server, req RestoreRequest, threads int) error {
	lg := env.Log
	task.SetPhase("apply")
	datadir := strings.TrimSpace(req.Datadir)
	service := strings.TrimSpace(req.ServiceName)
	osUser := strings.TrimSpace(req.OSUser)
	if target != nil {
		if datadir == "" {
			datadir = target.Datadir
		}
		if service == "" {
			service = target.ServiceName
		}
		if osUser == "" {
			osUser = target.OSUser
		}
	}
	if osUser == "" {
		osUser = "mysql"
	}
	datadir = filepath.Clean(datadir)
	if datadir == "" || datadir == "." || datadir == "/" || !filepath.IsAbs(datadir) || len(datadir) < 5 {
		return fmt.Errorf("invalid datadir: %q", datadir)
	}
	if service == "" {
		for _, s := range []string{"mariadb", "mysql", "mysqld"} {
			if exec.Command("systemctl", "cat", s+".service").Run() == nil {
				service = s
				break
			}
		}
	}
	if service == "" {
		return errors.New("could not determine the MySQL/MariaDB service name (enter it in the form)")
	}
	stamp := time.Now().Format("20060102-150405")
	oldDir := datadir + ".before-restore-" + stamp
	lg.Printf("=== APPLYING TO DATADIR %s (service %s) ===", datadir, service)
	if err := runSimple(ctx, lg, "systemctl", "stop", service); err != nil {
		return fmt.Errorf("could not stop the service: %w", err)
	}
	if _, err := os.Stat(datadir); err == nil {
		lg.Printf("Renaming the old datadir → %s", oldDir)
		if err := os.Rename(datadir, oldDir); err != nil {
			_ = runSimple(context.Background(), lg, "systemctl", "start", service)
			return fmt.Errorf("could not rename the old datadir: %w", err)
		}
	}
	rollback := func(cause error) error {
		lg.Printf("ERROR: %v — rolling back to the old datadir", cause)
		os.RemoveAll(datadir)
		if _, err := os.Stat(oldDir); err == nil {
			if err := os.Rename(oldDir, datadir); err != nil {
				lg.Printf("Rollback failed: %v. Run it yourself: mv %s %s", err, oldDir, datadir)
			}
		}
		_ = runSimple(context.Background(), lg, "systemctl", "start", service)
		return cause
	}
	if err := os.MkdirAll(datadir, 0o750); err != nil {
		return rollback(err)
	}
	if err := engine.CopyBack(env, flavor, fullDir, datadir, req.MoveBack, threads); err != nil {
		return rollback(err)
	}
	if err := runSimple(ctx, lg, "chown", "-R", osUser+":"+osUser, datadir); err != nil {
		return rollback(err)
	}
	if p, err := exec.LookPath("restorecon"); err == nil {
		_ = runSimple(ctx, lg, p, "-R", datadir)
	}
	if err := runSimple(ctx, lg, "systemctl", "start", service); err != nil {
		lg.Printf("The service failed to start with the new data.")
		return rollback(fmt.Errorf("could not start the service: %w", err))
	}
	lg.Printf("Service %s is running with the restored data.", service)
	lg.Printf("The old datadir is kept at %s — delete it manually once you have verified the restore.", oldDir)
	return nil
}
