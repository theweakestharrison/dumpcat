package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dumpcat/internal/agent"
	"dumpcat/internal/auth"
	"dumpcat/internal/dbconn"
	"dumpcat/internal/drivers"
	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/notify"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/sysinfo"
	"dumpcat/internal/tasklog"
)

func newUUID() string {
	b := auth.RandomBytes(16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func NewUUID() string { return newUUID() }

// StartBackup queues a backup of a job. typ is "full", "incremental" or "".
func (r *Runner) StartBackup(jobID int64, typ, trigger, user string) (*store.Run, error) {
	job, err := r.Store.GetJob(jobID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.draining {
		if trigger == "schedule" {
			r.deferred = append(r.deferred, Deferred{JobID: jobID, Type: typ})
		}
		r.mu.Unlock()
		return nil, ErrUpdating
	}
	if r.jobRunning[jobID] {
		r.mu.Unlock()
		return nil, ErrJobRunning
	}
	r.jobRunning[jobID] = true
	r.mu.Unlock()

	if typ != store.TypeIncremental || job.Method != store.MethodPhysical {
		typ = store.TypeFull
	}
	jid := job.ID
	sid := job.ServerID
	run := &store.Run{
		UUID: newUUID(), JobID: &jid, JobName: job.Name, JobUUID: job.UUID, JobSlug: JobDir(job.Name, job.ID),
		ServerID: &sid, ServerName: job.ServerName, Method: job.Method, Type: typ, Status: store.StatusQueued,
		Trigger: trigger, TriggeredBy: user, StartedAt: time.Now(), Compression: pipeline.NormalizeCompression(job.Compression),
		Encrypted: job.Encrypt && job.Passphrase != "", Meta: map[string]any{},
	}
	if run.Encrypted {
		run.Passphrase = job.Passphrase
	}
	if err := r.Store.CreateRun(run); err != nil {
		r.mu.Lock()
		delete(r.jobRunning, jobID)
		r.mu.Unlock()
		return nil, err
	}
	task, ctx := r.addTask("backup", run.ID, job.ID, job.Name)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.removeTask(task)
		r.execBackup(ctx, task, run, job)
	}()
	return run, nil
}

type targetCtx struct {
	st      *store.Storage
	backend storage.Backend
	loc     *store.RunLocation
	err     error
}

func (r *Runner) execBackup(ctx context.Context, task *Task, run *store.Run, job *store.Job) {
	lg, err := tasklog.Open(r.RunLogPath(run.ID))
	if err != nil {
		lg = tasklog.Discard()
	}
	defer lg.Close()
	start := time.Now()
	fail := func(err error) {
		msg := err.Error()
		status := store.StatusFailed
		if errors.Is(err, context.Canceled) {
			status = store.StatusCanceled
			msg = "Canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			msg = "Time limit exceeded (timeout)"
		}
		lg.Printf("FAILED: %s", msg)
		now := time.Now()
		run.Status, run.Error, run.FinishedAt = status, msg, &now
		_ = r.Store.UpdateRun(run)
		r.Store.FailUploadingLocations(run.ID, "backup failed")
		if status == store.StatusFailed && job.NotifyFailure {
			r.notifyRun(run, job, now.Sub(start))
		}
	}

	lg.Printf("DumpCat %s — backup job \"%s\" (%s, %s)", r.AppVersion, job.Name, job.Method, run.Type)
	if job.Encrypt && job.Passphrase == "" {
		// never fall back to plaintext: an empty passphrase means it could not be decrypted
		fail(errNoPassphrase)
		return
	}
	lg.Printf("Waiting for an execution slot...")
	if err := r.acquire(ctx); err != nil {
		fail(err)
		return
	}
	defer r.release()
	// The timeout bounds connecting + dumping (the snapshot window), never the
	// upload of an already complete backup. Cancel (ctx) still covers both.
	dumpCtx := ctx
	if job.TimeoutMinutes > 0 {
		var cancel context.CancelFunc
		dumpCtx, cancel = context.WithTimeout(ctx, time.Duration(job.TimeoutMinutes)*time.Minute)
		defer cancel()
	}
	start = time.Now()
	run.Status = store.StatusRunning
	run.StartedAt = start
	_ = r.Store.UpdateRun(run)
	task.SetPhase("prepare")

	server, err := r.Store.GetServer(job.ServerID)
	if err != nil {
		fail(fmt.Errorf("server not found: %w", err))
		return
	}
	lg.Redact(server.Password, job.Passphrase)
	drv := drivers.ForServer(server)
	if drv == nil {
		fail(fmt.Errorf("server %s: unknown database engine %q", server.Name, server.Engine))
		return
	}
	if _, ok := engine.FindMethod(drv, job.Method); !ok {
		fail(fmt.Errorf("the %s method does not back up %s servers: edit the job and pick another method", job.Method, drv.Product()))
		return
	}
	if job.Method == store.MethodPhysical && server.WGPeerID != nil && *server.WGPeerID > 0 {
		// xtrabackup/mariabackup copy the datadir of the machine they run on
		fail(errors.New("physical backups need the backup tool on the database host: WireGuard only forwards the MySQL connection (use an agent started with --allow-physical, DumpCat installed on the database host, or mysqldump/mydumper)"))
		return
	}

	if job.Options.PreCommand != "" {
		lg.Printf("Running pre-command")
		if err := r.hook(dumpCtx, lg, job.Options.PreCommand, run, ""); err != nil {
			fail(fmt.Errorf("pre-command failed: %w", err))
			return
		}
	}

	lg.Printf("Connecting to %s (%s:%d)...", server.Name, server.Host, server.Port)
	info, err := drv.Inspect(dumpCtx, server, true)
	if err != nil {
		fail(fmt.Errorf("could not connect to %s: %w", drv.Product(), err))
		return
	}
	flavor := engine.Flavor(server, info)
	r.Store.UpdateServerDetected(server.ID, info.Version, info.Datadir)
	if server.Datadir == "" {
		server.Datadir = info.Datadir
	}
	lg.Printf("Server: %s (%s), datadir=%s, binlog=%v, replica=%v", info.Version, flavor, info.Datadir, info.LogBin, info.IsReplica)
	for _, w := range info.Warnings {
		lg.Printf("Warning: %s", w)
	}

	dbs, missing := resolveDatabases(job, info.Databases)
	for _, m := range missing {
		lg.Printf("Warning: database %s does not exist on the server, skipping", m)
	}
	if job.Method != store.MethodPhysical && len(dbs) == 0 {
		fail(errors.New("no databases match the job selection"))
		return
	}
	var estimate int64
	for _, d := range dbs {
		estimate += d.SizeBytes
	}
	if job.Method == store.MethodPhysical {
		estimate = info.TotalBytes
	}
	task.SetEstimate(estimate)
	names := make([]string, len(dbs))
	for i, d := range dbs {
		names[i] = d.Name
	}
	if job.Method != store.MethodPhysical {
		lg.Printf("Databases (%d, ~%s): %s", len(dbs), humanBytes(estimate), strings.Join(names, ", "))
	} else {
		lg.Printf("Physical backup of the whole instance (~%s of data)", humanBytes(estimate))
	}

	// incremental base
	spec := &engine.BackupSpec{Server: server, Flavor: flavor, ServerVersion: info.Version, Info: info, Job: job, Databases: dbs}
	if job.Method == store.MethodPhysical && server.AgentID != nil && *server.AgentID > 0 {
		if spec.Remote, err = r.agentPhysical(server, flavor, lg); err != nil {
			fail(err)
			return
		}
	}
	if run.Type == store.TypeIncremental {
		parent := r.Store.LastSuccessfulRun(job.ID, store.MethodPhysical)
		lsn := ""
		if parent != nil {
			lsn, _ = parent.Meta["to_lsn"].(string)
		}
		sameServer := parent != nil && parent.ServerID != nil && *parent.ServerID == job.ServerID
		if sameServer {
			if h, _ := parent.Meta["server_host"].(string); h != "" && info.Hostname != "" && h != info.Hostname {
				sameServer = false
			}
		}
		if parent != nil && !sameServer {
			lg.Printf("The previous backup (#%d) belongs to a different server — it cannot be an incremental base", parent.ID)
		}
		if parent == nil || !sameServer || lsn == "" || !r.chainIntact(parent) {
			lg.Printf("No valid full backup to use as a base — switching to a FULL backup")
			run.Type = store.TypeFull
		} else {
			spec.IncrementalLSN = lsn
			pid := parent.ID
			run.ParentID, run.ParentUUID = &pid, parent.UUID
			if parent.BaseID != nil {
				run.BaseID, run.BaseUUID = parent.BaseID, parent.BaseUUID
			} else {
				run.BaseID, run.BaseUUID = &pid, parent.UUID
			}
			lg.Printf("Incremental based on backup #%d (%s)", parent.ID, parent.DirName)
		}
	}
	short := "full"
	if run.Type == store.TypeIncremental {
		short = "incr"
	}
	run.DirName = start.In(r.location()).Format("20060102-150405") + "-" + short
	run.Databases = make([]store.DBEntry, 0)
	_ = r.Store.UpdateRun(run)
	rel := run.RelDir()

	// targets
	var targets []*targetCtx
	var primaryLocal *targetCtx
	for _, t := range job.Targets {
		st, err := r.Store.GetStorage(t.StorageID)
		if err != nil {
			lg.Printf("Warning: storage #%d does not exist", t.StorageID)
			continue
		}
		tc := &targetCtx{st: st}
		tc.backend, tc.err = r.Storage.Get(st)
		sid := st.ID
		tc.loc = &store.RunLocation{RunID: run.ID, StorageID: &sid, StorageName: st.Name, StorageType: st.Type, Path: rel, Status: store.LocUploading}
		_ = r.Store.AddLocation(tc.loc)
		if tc.err != nil {
			lg.Printf("Storage %s configuration error: %v", st.Name, tc.err)
			tc.loc.Status, tc.loc.Error = store.LocFailed, tc.err.Error()
			_ = r.Store.UpdateLocation(tc.loc)
		} else if tc.backend.IsLocal() && primaryLocal == nil {
			primaryLocal = tc
		}
		targets = append(targets, tc)
	}
	if len(targets) == 0 {
		fail(errors.New("the job has no target storage"))
		return
	}

	chainOpts := pipeline.ChainOptions{Compression: run.Compression, Level: job.CompressionLevel, Threads: job.Threads,
		RawProgress: &task.Raw, OutProgress: &task.Out}
	if run.Encrypted {
		chainOpts.Passphrase = run.Passphrase
	}
	workDir := filepath.Join(r.stagingRoot(), fmt.Sprintf("work-backup-%d", run.ID))
	defer os.RemoveAll(workDir)
	env := &engine.Env{Ctx: dumpCtx, Log: lg, Tools: r.Tools, TmpDir: workDir, LowPriority: job.Options.LowPriority}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		fail(err)
		return
	}

	var primaryDir string // local dir holding the finished backup (nil in stream mode)
	var stagingUsed bool
	var sink engine.Sink
	var files func() []store.BackupFile
	var stream *streamSink
	streamMode := primaryLocal == nil && job.Options.StreamUpload
	switch {
	case primaryLocal != nil:
		primaryDir = primaryLocal.backend.LocalPath(rel)
		lg.Printf("Writing directly to local storage %s: %s", primaryLocal.st.Name, primaryDir)
	case streamMode:
		lg.Printf("Stream mode: uploading directly to storage, no temporary disk")
		stream = &streamSink{sinkBase: sinkBase{chain: chainOpts, encrypted: run.Encrypted}, ctx: dumpCtx, rel: rel,
			op: &storage.Op{Log: lg, BwLimit: job.Options.BwLimit}}
		for _, t := range targets {
			if t.err == nil {
				stream.targets = append(stream.targets, &streamTarget{loc: t.loc, backend: t.backend})
			}
		}
		sink, files = stream, stream.Files
	default:
		primaryDir = filepath.Join(r.stagingRoot(), fmt.Sprintf("stage-backup-%d", run.ID), filepath.FromSlash(rel))
		stagingUsed = true
		lg.Printf("Writing to staging directory %s, then uploading", primaryDir)
	}
	if primaryDir != "" {
		if total, free, err := diskUsage(filepath.Dir(primaryDir)); err == nil {
			lg.Printf("Free space at the write target: %s / %s", humanBytes(free), humanBytes(total))
			if estimate > 0 && free < estimate/3 {
				lg.Printf("WARNING: free space may not be enough (estimated data %s)", humanBytes(estimate))
			}
		}
		ds, err := newDirSink(primaryDir, chainOpts)
		if err != nil {
			fail(err)
			return
		}
		sink, files = ds, ds.Files
	}

	cleanupPartial := func() {
		if primaryDir != "" {
			os.RemoveAll(primaryDir)
		}
		if stagingUsed {
			os.RemoveAll(filepath.Join(r.stagingRoot(), fmt.Sprintf("stage-backup-%d", run.ID)))
		}
		for _, t := range targets {
			if t.loc.Status == store.LocUploading || t.loc.Status == store.LocOK {
				if stream != nil && t.backend != nil && !t.backend.IsLocal() {
					dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					_ = t.backend.DeleteDir(dctx, rel)
					cancel()
				}
				t.loc.Status, t.loc.Error = store.LocFailed, "backup failed"
				_ = r.Store.UpdateLocation(t.loc)
			}
		}
	}

	task.SetPhase("dump")
	lg.Printf("Starting %s...", job.Method)
	res, err := drv.Backup(env, spec, sink)
	if err != nil {
		cleanupPartial()
		fail(err)
		return
	}
	dumpDur := time.Since(start)
	run.Files = files()
	run.Databases = res.Databases
	for k, v := range res.Meta {
		run.Meta[k] = v
	}
	run.Meta["server_version"] = info.Version
	run.Meta["server_host"] = info.Hostname
	run.Meta["flavor"] = flavor
	if _, ok := run.Meta["consistent_at"]; !ok {
		// the dump has ended: its data cannot be newer than now (PITR base selection)
		run.Meta["consistent_at"] = time.Now().Unix()
	}
	run.RawBytes, run.SizeBytes = 0, 0
	for _, f := range run.Files {
		run.RawBytes += f.RawSize
		run.SizeBytes += f.Size
	}
	if run.RawBytes == 0 {
		run.RawBytes = task.Raw.N()
	}
	lg.Printf("Dump finished in %s: data %s → stored %s (%d files)", humanDuration(dumpDur), humanBytes(run.RawBytes), humanBytes(run.SizeBytes), len(run.Files))
	if res.Tables != nil {
		var tables int
		var rows int64
		for _, ts := range res.Tables {
			for _, n := range ts {
				tables++
				rows += n
			}
		}
		lg.Printf("Counted %d rows in %d tables (checked by restore drills)", rows, tables)
		_ = r.Store.SetRunTables(run.ID, res.Tables)
	}

	// manifest
	fin := time.Now()
	man := r.manifest(run, server, flavor, fin)
	man.Engine = drv.Engine()
	man.Tables = res.Tables
	manData, _ := json.MarshalIndent(man, "", "  ")
	if primaryDir != "" {
		if err := os.WriteFile(filepath.Join(primaryDir, "manifest.json"), manData, 0o640); err != nil {
			cleanupPartial()
			fail(fmt.Errorf("write manifest: %w", err))
			return
		}
	}

	// local primary location is complete
	if primaryLocal != nil {
		primaryLocal.loc.Status, primaryLocal.loc.SizeBytes = store.LocOK, run.SizeBytes
		_ = r.Store.UpdateLocation(primaryLocal.loc)
	}

	task.SetPhase("upload")
	if stream != nil {
		for _, st := range stream.targets {
			loc := st.loc
			if st.failed != nil {
				loc.Status, loc.Error = store.LocFailed, st.failed.Error()
				lg.Printf("Storage %s: FAILED %v", loc.StorageName, st.failed)
			} else if err := st.backend.PutStream(ctx, rel+"/manifest.json", bytes.NewReader(manData), nil); err != nil {
				loc.Status, loc.Error = store.LocFailed, "manifest: "+err.Error()
			} else if err := verifyRemote(ctx, st.backend, rel, run.Files); err != nil {
				loc.Status, loc.Error = store.LocFailed, err.Error()
				lg.Printf("Storage %s: post-upload check failed: %v", loc.StorageName, err)
			} else {
				loc.Status, loc.SizeBytes, loc.Error = store.LocOK, run.SizeBytes, ""
				lg.Printf("Storage %s: OK", loc.StorageName)
			}
			_ = r.Store.UpdateLocation(loc)
		}
	} else {
		for _, t := range targets {
			if t.err != nil || t == primaryLocal {
				continue
			}
			if ctx.Err() != nil {
				t.loc.Status, t.loc.Error = store.LocFailed, "canceled before upload"
				_ = r.Store.UpdateLocation(t.loc)
				continue
			}
			lg.Printf("Uploading to %s (%s)...", t.st.Name, t.backend.Describe())
			upStart := time.Now()
			op := &storage.Op{Log: lg, BwLimit: job.Options.BwLimit, OnProgress: task.SetUploadPct}
			err := t.backend.UploadDir(ctx, primaryDir, rel, op)
			if err == nil {
				err = verifyRemote(ctx, t.backend, rel, run.Files)
			}
			if err != nil {
				if ctx.Err() != nil {
					err = errors.New("canceled during upload")
				}
				t.loc.Status, t.loc.Error = store.LocFailed, err.Error()
				lg.Printf("Upload to %s FAILED: %v", t.st.Name, err)
				// remove the partial copy so it can never be mistaken for a backup
				dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				_ = t.backend.DeleteDir(dctx, rel)
				cancel()
			} else {
				t.loc.Status, t.loc.SizeBytes, t.loc.Error = store.LocOK, run.SizeBytes, ""
				d := time.Since(upStart)
				lg.Printf("Upload to %s OK in %s (%s/s)", t.st.Name, humanDuration(d), humanBytes(int64(float64(run.SizeBytes)/max(d.Seconds(), 1))))
			}
			_ = r.Store.UpdateLocation(t.loc)
		}
	}
	if stagingUsed {
		os.RemoveAll(filepath.Join(r.stagingRoot(), fmt.Sprintf("stage-backup-%d", run.ID)))
	}

	okCount, failCount := 0, 0
	for _, t := range targets {
		if t.loc.Status == store.LocOK {
			okCount++
		} else {
			failCount++
		}
	}
	now := time.Now()
	run.FinishedAt = &now
	canceled := ctx.Err() != nil
	switch {
	case okCount == 0 && canceled:
		run.Status = store.StatusCanceled
		run.Error = "Canceled"
	case okCount == 0:
		run.Status = store.StatusFailed
		run.Error = "the backup could not be stored on any storage"
	case canceled:
		run.Status = store.StatusPartial
		run.Error = fmt.Sprintf("canceled during upload — stored on only %d/%d storages", okCount, len(targets))
	case failCount > 0:
		run.Status = store.StatusPartial
		run.Error = fmt.Sprintf("%d/%d storages failed", failCount, len(targets))
	default:
		run.Status = store.StatusSuccess
		run.Error = ""
	}
	_ = r.Store.UpdateRun(run)
	lg.Printf("RESULT: %s — total time %s", strings.ToUpper(run.Status), humanDuration(now.Sub(start)))

	if job.Options.PostCommand != "" {
		lg.Printf("Running post-command")
		if err := r.hook(context.Background(), lg, job.Options.PostCommand, run, run.Status); err != nil {
			lg.Printf("post-command failed: %v", err)
		}
	}
	if run.Status != store.StatusFailed && run.Status != store.StatusCanceled && !canceled {
		task.SetPhase("retention")
		r.applyRetention(ctx, job, lg)
		r.applyLogRetention(ctx, job, lg)
		switch job.Options.Verify() {
		case store.VerifyAll:
			task.SetPhase("verify")
			r.verifyRun(ctx, task, run.ID, 0, lg)
		case store.VerifyOne:
			task.SetPhase("verify")
			r.verifyRun(ctx, task, run.ID, r.cheapestCopy(run.ID), lg)
		}
	}
	if (run.Status == store.StatusSuccess && job.NotifySuccess) || (run.Status != store.StatusSuccess && run.Status != store.StatusCanceled && job.NotifyFailure) {
		r.notifyRun(run, job, now.Sub(start))
	}
}

// agentPhysical runs a physical backup of a server reached through an agent
// on the database host: the agent starts the tool there and streams the
// xbstream back, into the same pipeline as a local backup.
func (r *Runner) agentPhysical(server *store.Server, flavor string, lg *tasklog.Logger) (engine.RemotePhysical, error) {
	id := *server.AgentID
	name := fmt.Sprintf("#%d", id)
	if a, err := r.Store.GetAgent(id); err == nil {
		name = a.Name
	}
	if r.Agents == nil {
		return nil, errors.New("agents are not available")
	}
	st := r.Agents.Status(id)
	tool, _ := engine.PhysicalToolNames(flavor)
	if p := st.PhysicalProblem(tool); p != "" {
		return nil, fmt.Errorf("agent %s: %s", name, p)
	}
	lg.Printf("The backup runs on the database host %s through agent %s (%s, as %s)", st.Hostname, name, st.Caps.Physical.Tool(tool).Version, st.Caps.Physical.User)
	network, addr := server.AgentTarget()
	return func(ctx context.Context, o engine.PhysicalOptions, stdout, stderr io.Writer) (map[string]string, string, error) {
		req := &agent.PhysicalRequest{PhysicalOptions: o, Net: network, Addr: addr, User: server.Username, Password: server.Password}
		res, err := r.Agents.Physical(ctx, id, req, stdout, stderr)
		if err != nil {
			return nil, "", fmt.Errorf("agent %s: %w", name, err)
		}
		if res.Error != "" {
			return res.Files, res.Tool, errors.New(res.Error)
		}
		return res.Files, res.Tool, nil
	}, nil
}

func diskUsage(p string) (int64, int64, error) {
	_ = os.MkdirAll(p, 0o750)
	return sysinfo.DiskUsage(p)
}

func (r *Runner) hook(ctx context.Context, lg *tasklog.Logger, script string, run *store.Run, status string) error {
	hctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := execx.Shell(hctx, script)
	cmd.Env = append(os.Environ(),
		"DUMPCAT_RUN_ID="+fmt.Sprint(run.ID), "DUMPCAT_JOB="+run.JobName, "DUMPCAT_STATUS="+status,
		"DUMPCAT_METHOD="+run.Method, "DUMPCAT_TYPE="+run.Type, "DUMPCAT_DIR="+run.RelDir())
	w := lg.Writer("[hook] ")
	defer w.Close()
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// chainIntact checks that every backup of the chain up to run is available.
func (r *Runner) chainIntact(run *store.Run) bool {
	cur := run
	for i := 0; i < 1000; i++ {
		ok := false
		for _, l := range cur.Locations {
			if l.Status == store.LocOK {
				ok = true
			}
		}
		if !ok || (cur.Status != store.StatusSuccess && cur.Status != store.StatusPartial) {
			return false
		}
		if cur.Type == store.TypeFull {
			return true
		}
		if cur.ParentID == nil {
			return false
		}
		p, err := r.Store.GetRun(*cur.ParentID)
		if err != nil {
			return false
		}
		cur = p
	}
	return false
}

// verifyRemote checks that all files exist remotely with the expected size.
func verifyRemote(ctx context.Context, b storage.Backend, rel string, files []store.BackupFile) error {
	entries, err := b.List(ctx, rel)
	if err != nil {
		return err
	}
	sizes := map[string]int64{}
	for _, e := range entries {
		sizes[e.Name] = e.Size
	}
	for _, f := range files {
		sz, ok := sizes[f.Name]
		if !ok {
			return fmt.Errorf("file %s is missing on the storage", f.Name)
		}
		if sz >= 0 && sz != f.Size {
			return fmt.Errorf("file %s has the wrong size (%d != %d)", f.Name, sz, f.Size)
		}
	}
	return nil
}

func resolveDatabases(job *store.Job, all []dbconn.DBInfo) ([]dbconn.DBInfo, []string) {
	byName := map[string]dbconn.DBInfo{}
	for _, d := range all {
		byName[d.Name] = d
	}
	var out []dbconn.DBInfo
	var missing []string
	switch job.DBMode {
	case "include":
		for _, n := range job.Databases {
			if d, ok := byName[n]; ok {
				out = append(out, d)
			} else {
				missing = append(missing, n)
			}
		}
	case "exclude":
		ex := map[string]bool{}
		for _, n := range job.Databases {
			ex[n] = true
		}
		for _, d := range all {
			if !d.System && !ex[d.Name] {
				out = append(out, d)
			}
		}
	default:
		for _, d := range all {
			if !d.System {
				out = append(out, d)
			}
		}
	}
	return out, missing
}

func (r *Runner) manifest(run *store.Run, server *store.Server, flavor string, fin time.Time) store.Manifest {
	enc := ""
	if run.Encrypted {
		enc = pipeline.EncAge
	}
	return store.Manifest{
		Format: store.ManifestFormat, AppVersion: r.AppVersion, UUID: run.UUID, JobUUID: run.JobUUID, JobName: run.JobName,
		JobSlug: run.JobSlug, DirName: run.DirName, Method: run.Method, Type: run.Type, ParentUUID: run.ParentUUID, BaseUUID: run.BaseUUID,
		StartedAt: run.StartedAt, FinishedAt: fin,
		Server:      store.ManifestServer{Name: server.Name, Host: server.Host, Port: server.Port, Version: fmt.Sprint(run.Meta["server_version"]), Flavor: flavor},
		Compression: run.Compression, Encryption: enc, Databases: run.Databases, Files: run.Files,
		RawBytes: run.RawBytes, SizeBytes: run.SizeBytes, Meta: run.Meta,
	}
}

func (r *Runner) notifyRun(run *store.Run, job *store.Job, dur time.Duration) {
	if r.Notifier == nil {
		return
	}
	st := run.Status
	title := "Backup succeeded: " + run.JobName
	switch st {
	case store.StatusPartial:
		title = "Backup partially completed: " + run.JobName
	case store.StatusFailed:
		title = "Backup FAILED: " + run.JobName
	}
	ev := notify.Event{Kind: "backup", Status: st, Title: title, Error: run.Error, URL: r.link(fmt.Sprintf("/#/runs/%d", run.ID))}
	ev.Add("Server", run.ServerName)
	ev.Add("Method", run.Method+" / "+run.Type)
	ev.Add("Duration", humanDuration(dur))
	if run.SizeBytes > 0 {
		ev.Add("Size", humanBytes(run.SizeBytes))
	}
	fresh, err := r.Store.GetRun(run.ID) // retention and verification ran since
	if err != nil {
		fresh = run
	}
	var locs []string
	for _, l := range fresh.Locations {
		locs = append(locs, l.StorageName+"="+l.Status)
	}
	if len(locs) > 0 {
		ev.Add("Storage", strings.Join(locs, ", "))
	}
	switch fresh.VerifyStatus {
	case "ok":
		ev.Add("Integrity", "Verified: "+fresh.VerifyMsg)
	case "failed":
		ev.Add("Integrity", "Verification FAILED: "+fresh.VerifyMsg)
	}
	r.Notifier.Send(ev)
}

// errNoPassphrase: the job encrypts its backups but its passphrase reads as empty, which happens
// when the master key does not match the database (master.key lost or replaced).
var errNoPassphrase = errors.New("this job encrypts its backups but its passphrase cannot be read (was DATA_DIR/master.key replaced or lost?): nothing is stored unencrypted; restore the original master.key, or enter the passphrase again in the job")
