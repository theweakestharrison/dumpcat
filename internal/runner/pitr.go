package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

// Point-in-time recovery: restore a base backup, then replay the archived
// log (MySQL binlogs, PostgreSQL WAL) from where that backup ends up to the
// target. The base selection, the plan, the downloads and their checks are
// shared; the driver's LogArchiver knows positions, chains and replay.

// PITRTarget is where a point-in-time restore stops.
type PITRTarget = engine.PITRTarget

// selectPITRBase picks the newest usable backup whose data precedes the target.
func selectPITRBase(la engine.LogArchiver, runs []store.Run, t engine.PITRTarget, usable func(*store.Run) bool) (*engine.LogBase, error) {
	var best *engine.LogBase
	var lastErr error
	for i := range runs {
		r := &runs[i]
		if r.Status != store.StatusSuccess && r.Status != store.StatusPartial {
			continue
		}
		if usable != nil && !usable(r) {
			continue
		}
		b, err := la.Base(r, &t)
		if err != nil {
			lastErr = err
			continue
		}
		if best == nil || b.ConsistentAt.After(best.ConsistentAt) {
			best = b
		}
	}
	if best == nil {
		if lastErr != nil {
			return nil, fmt.Errorf("no backup can be used as a base (%v)", lastErr)
		}
		return nil, errors.New("no backup can be used as a base")
	}
	return best, nil
}

// planPITR checks that the base plus archived log files reach the target.
func (r *Runner) planPITR(la engine.LogArchiver, base *engine.LogBase, t engine.PITRTarget) (*engine.LogPlan, error) {
	all, err := r.Store.ListLogFiles(base.Run.JobUUID)
	if err != nil {
		return nil, err
	}
	var files []store.LogFile
	for i := range all {
		if engine.LogStored(&all[i]) {
			files = append(files, all[i])
		}
	}
	in := &engine.PlanInput{Base: base, Files: files, Target: t, Loc: r.location()}
	// the file being received right now may continue (or start) the chain
	if base.Run.JobID != nil {
		if a := r.archiver(*base.Run.JobID); a != nil && a.la == la {
			in.Current = a.stream.Current()
		}
	}
	return la.Plan(in)
}

// pitrBase returns the log archiver of a backup's method and its base for
// t (normalized).
func pitrBase(run *store.Run, t *engine.PITRTarget) (engine.LogArchiver, *engine.LogBase, error) {
	la := logArchiverOf(run.Method)
	if la == nil {
		return nil, nil, fmt.Errorf("%s backups cannot be the base of a point-in-time recovery", run.Method)
	}
	if err := engine.NormalizeTarget(la, t); err != nil {
		return nil, nil, err
	}
	b, err := la.Base(run, t)
	return la, b, err
}

// ---- API view ----

type PlanRun struct {
	ID           int64     `json:"id"`
	DirName      string    `json:"dir_name"`
	Method       string    `json:"method"`
	Type         string    `json:"type"`
	StartedAt    time.Time `json:"started_at"`
	ConsistentAt time.Time `json:"consistent_at"`
	Start        string    `json:"start"` // where replay starts: mysql-bin.000012:154, 0/3000028
}

type PITRPlanView struct {
	OK        bool       `json:"ok"`
	Error     string     `json:"error,omitempty"`
	Target    string     `json:"target"`
	Log       string     `json:"log"`        // binlog, WAL
	FilesName string     `json:"files_name"` // binlogs, WAL segments
	Base      *PlanRun   `json:"base,omitempty"`
	RunOK     bool       `json:"run_ok"`              // the requested run can be the base
	RunError  string     `json:"run_error,omitempty"` // why it cannot
	Files     int        `json:"files"`
	Bytes     int64      `json:"bytes"`
	FirstFile string     `json:"first_file,omitempty"`
	LastFile  string     `json:"last_file,omitempty"`
	Partial   string     `json:"partial,omitempty"`
	Coverage  *time.Time `json:"coverage,omitempty"`
	Warnings  []string   `json:"warnings"`
}

func (r *Runner) pitrUsable(run *store.Run) bool {
	if run.Type == store.TypeIncremental {
		return r.chainIntact(run)
	}
	for _, l := range run.Locations {
		if l.Status == store.LocOK {
			return true
		}
	}
	return false
}

// PlanPITR selects the base backup of a job for target and checks that the
// archived log reaches it. runID (optional) is checked as a base too.
func (r *Runner) PlanPITR(jobID, runID int64, t PITRTarget) (*PITRPlanView, error) {
	var method, jobUUID string
	var asked *store.Run
	if runID > 0 {
		run, err := r.Store.GetRun(runID)
		if err != nil {
			return nil, err
		}
		method, jobUUID, asked = run.Method, run.JobUUID, run
	} else {
		job, err := r.Store.GetJob(jobID)
		if err != nil {
			return nil, err
		}
		method, jobUUID = job.Method, job.UUID
	}
	la := logArchiverOf(method)
	if la == nil {
		return nil, fmt.Errorf("%s backups cannot be the base of a point-in-time recovery", method)
	}
	if err := engine.NormalizeTarget(la, &t); err != nil {
		return nil, err
	}
	info := la.LogInfo()
	v := &PITRPlanView{Target: t.Describe(r.location()), Log: info.Name, FilesName: info.Files, Warnings: []string{}}
	runs, err := r.Store.RunsByJobUUID(jobUUID)
	if err != nil {
		return nil, err
	}
	base, err := selectPITRBase(la, runs, t, r.pitrUsable)
	if err != nil {
		v.Error = err.Error()
		if asked != nil {
			v.RunError = err.Error()
		}
		return v, nil
	}
	if asked != nil {
		if asked.ID == base.Run.ID {
			v.RunOK = true
		} else if ab, err := la.Base(asked, &t); err != nil {
			v.RunError = err.Error()
		} else if _, err := r.planPITR(la, ab, t); err != nil {
			v.RunError = err.Error()
		} else {
			v.RunOK = true
		}
	}
	b := base.Run
	v.Base = &PlanRun{ID: b.ID, DirName: b.DirName, Method: b.Method, Type: b.Type, StartedAt: b.StartedAt, ConsistentAt: base.ConsistentAt, Start: base.From}
	p, err := r.planPITR(la, base, t)
	if err != nil {
		v.Error = err.Error()
		return v, nil
	}
	v.OK = true
	v.Files, v.Bytes, v.Partial, v.Warnings = len(p.Files), p.Bytes, p.Partial, append(v.Warnings, p.Warnings...)
	if len(p.Files) > 0 {
		v.FirstFile, v.LastFile = p.Files[0].FileName, p.Files[len(p.Files)-1].FileName
	}
	if !p.Coverage.IsZero() {
		c := p.Coverage
		v.Coverage = &c
	}
	if p.Partial != "" {
		v.Warnings = append(v.Warnings, fmt.Sprintf("Also uses the %s still being received, %s (not uploaded yet), from the DumpCat staging folder", info.File, p.Partial))
	}
	return v, nil
}

// ---- restore: download, verify, resolve the stop point ----

var errReplan = errors.New("the file being received was just closed")

// pitrPrepare fetches every file needed into the work dir and lets the
// driver check them and resolve the stop — before anything destructive
// happens to the target.
func (r *Runner) pitrPrepare(ctx context.Context, task *Task, env *engine.Env, la engine.LogArchiver, plan *engine.LogPlan, workDir, fallbackPass string) (*engine.Replay, error) {
	lg := env.Log
	info := la.LogInfo()
	t := plan.Target
	loc := r.location()
	dir := filepath.Join(workDir, info.Prefix)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	partial := ""
	if plan.Partial != "" {
		partial = " + file being received " + plan.Partial
	}
	lg.Printf("PITR: recover to %s — base backup #%d %s, replaying %s from %s (%d archived file(s)%s, %s)", t.Describe(loc), plan.Base.Run.ID, plan.Base.Run.DirName,
		info.Files, plan.Base.From, len(plan.Files), partial, humanBytes(plan.Bytes))
	if total, free, err := diskUsage(dir); err == nil {
		lg.Printf("PITR work folder %s: %s free of %s", dir, humanBytes(free), humanBytes(total))
		if free < plan.Bytes {
			return nil, fmt.Errorf("not enough free space for the %s (%s needed)", info.Files, humanBytes(plan.Bytes))
		}
	}
	task.SetPhase("binlog_download")
	task.SetEstimate(plan.Bytes)
	chk := la.NewCheck(env, plan)
	done := false
	for i := range plan.Files {
		f := &plan.Files[i]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		local := filepath.Join(dir, f.FileName)
		span := ""
		if !f.FirstTS.IsZero() {
			span = ", " + f.FirstTS.In(loc).Format("01-02 15:04:05") + " → " + f.LastTS.In(loc).Format("01-02 15:04:05")
		}
		lg.Printf("[%d/%d] Fetching %s (%s%s)", i+1, len(plan.Files), f.FileName, humanBytes(f.SizeBytes), span)
		if err := r.fetchLogFile(ctx, task, f, local, fallbackPass); err != nil {
			return nil, fmt.Errorf("%s: %w", f.FileName, err)
		}
		var err error
		if done, err = chk.Add(engine.LocalLog{Name: f.FileName, Path: local, File: f}); err != nil {
			return nil, err
		}
		if done {
			break
		}
	}
	if plan.Partial != "" && !done {
		a := r.archiver(*plan.Base.Run.JobID)
		if a == nil {
			return nil, fmt.Errorf("%s archiving has stopped — the %s being received is not available", info.Name, info.File)
		}
		tmp := filepath.Join(dir, ".partial", plan.Partial)
		snap, err := a.stream.Snapshot(tmp)
		if err != nil {
			return nil, err
		}
		if snap.Name != plan.Partial {
			return nil, errReplan
		}
		cov := snap.Last
		if snap.CaughtUp && snap.CheckedAt.After(cov) {
			cov = snap.CheckedAt
		}
		if t.Type == "time" && t.Time.After(cov) {
			return nil, fmt.Errorf("%s only reach %s", info.Files, cov.In(loc).Format("2006-01-02 15:04:05"))
		}
		local := filepath.Join(dir, snap.Name)
		if err := os.Rename(tmp, local); err != nil {
			return nil, err
		}
		upTo := ""
		if !snap.Last.IsZero() {
			upTo = ", up to " + snap.Last.In(loc).Format("2006-01-02 15:04:05")
		}
		lg.Printf("Including the %s being received, %s (%s%s)", info.File, snap.Name, humanBytes(snap.Size), upTo)
		if _, err := chk.Add(engine.LocalLog{Name: snap.Name, Path: local}); err != nil {
			return nil, err
		}
	}
	return chk.Finish()
}

// fetchLogFile downloads, decrypts and decompresses one archived file and
// checks both checksums.
func (r *Runner) fetchLogFile(ctx context.Context, task *Task, f *store.LogFile, local, fallbackPass string) error {
	var lastErr error = errors.New("no storage holds this file")
	// local storages first
	locs := append([]store.LogLocation{}, f.Locations...)
	sort.SliceStable(locs, func(i, j int) bool { return locs[i].StorageType == "local" && locs[j].StorageType != "local" })
	for _, l := range locs {
		if l.Status != store.LocOK || l.StorageID == nil {
			continue
		}
		st, err := r.Store.GetStorage(*l.StorageID)
		if err != nil {
			lastErr = err
			continue
		}
		b, err := r.Storage.Get(st)
		if err != nil {
			lastErr = err
			continue
		}
		if err := r.fetchLogFileFrom(ctx, task, b, f, local, fallbackPass); err != nil {
			lastErr = fmt.Errorf("%s: %w", st.Name, err)
			continue
		}
		return nil
	}
	return lastErr
}

func (r *Runner) fetchLogFileFrom(ctx context.Context, task *Task, b storage.Backend, f *store.LogFile, local, fallbackPass string) error {
	pass := f.Passphrase
	if pass == "" {
		pass = fallbackPass
	}
	rc, err := b.Open(ctx, f.Path)
	if err != nil {
		return err
	}
	defer rc.Close()
	hr := pipeline.NewHashingReader(rc, &task.Raw)
	dec, cleanup, err := pipeline.NewReader(hr, f.Compression, f.Encrypted, pass)
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := os.OpenFile(local, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	rh := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, rh), dec)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, hr); err != nil {
		return err
	}
	if f.SHA256 != "" && hr.SHA256() != f.SHA256 {
		return errors.New("checksum mismatch of the stored file")
	}
	if f.RawSHA256 != "" && hex.EncodeToString(rh.Sum(nil)) != f.RawSHA256 {
		return errors.New("checksum mismatch of the decoded file")
	}
	return nil
}

// pitrApply replays the checked files after the base restore (not for
// drivers whose physical restore runs the recovery itself).
func (r *Runner) pitrApply(ctx context.Context, task *Task, env *engine.Env, la engine.LogArchiver, rp *engine.Replay, run *store.Run, target *store.Server, req RestoreRequest) error {
	task.SetPhase("binlog_apply")
	spec := &engine.ReplaySpec{Run: run, Target: target, Databases: selectDatabases(run, req), Rename: req.Rename, NoBinlog: req.NoBinlog,
		Progress: func(n int64) { task.Raw.Add(n) },
		Estimate: func(n int64) { task.SetEstimate(task.Raw.N() + n) }}
	if req.FastMode {
		spec.FastMode = func() func() { return r.enableFastMode(ctx, target, env.Log) }
	}
	return la.Replay(env, rp, spec)
}
