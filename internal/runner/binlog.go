package runner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/binlog"
	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/notify"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// Continuous binlog archiving: one supervised `mysqlbinlog --raw
// --stop-never` per job copies the server's binary logs into a private
// staging folder. Every file the server has closed is compressed/encrypted
// like a backup file, uploaded to the job's storages and recorded in the
// catalog; the file still being written stays local until it rotates.

const (
	archiveTick = 5 * time.Second
	// replication server id of the archiver connection: must differ from
	// every real server/replica id of the source, and per job
	binlogServerIDBase = 3_900_000_000
	stallTimeout       = 3 * time.Minute
)

var (
	errRestart = errors.New("restart requested")
	errStalled = errors.New("no new data although the server kept writing — reconnecting")
)

// ArchiverStatus is the live state of a job's archiver (shown in the UI).
type ArchiverStatus struct {
	JobID         int64      `json:"job_id"`
	JobName       string     `json:"job_name"`
	ServerName    string     `json:"server_name"`
	State         string     `json:"state"` // starting, streaming, retrying, stopped
	Error         string     `json:"error"`
	ErrorAt       *time.Time `json:"error_at"`
	NextTry       *time.Time `json:"next_try"`
	Since         *time.Time `json:"since"`
	Restarts      int        `json:"restarts"`
	Current       string     `json:"current_file"`
	Received      int64      `json:"received_bytes"`
	ReceivedAt    *time.Time `json:"received_at"` // time of the last event received
	ServerFile    string     `json:"server_file"`
	ServerPos     int64      `json:"server_pos"`
	CheckedAt     *time.Time `json:"checked_at"`
	LagBytes      int64      `json:"lag_bytes"`
	LagSeconds    int64      `json:"lag_seconds"`
	Pending       int        `json:"pending_files"`
	PendingBytes  int64      `json:"pending_bytes"`
	UploadError   string     `json:"upload_error"`
	LastArchived  string     `json:"last_archived"`
	ArchivedAt    *time.Time `json:"archived_at"`
	ArchivedUntil *time.Time `json:"archived_until"` // last event of the newest archived file
	RotatedAt     *time.Time `json:"rotated_at"`
	StartedAt     time.Time  `json:"started_at"`
	CaughtUpAt    *time.Time `json:"caught_up_at"`        // the stream was last level with the server
	PendingSince  *time.Time `json:"pending_since"`       // the oldest closed binlog waiting for upload
	ArchiveLag    int64      `json:"archive_lag_seconds"` // how long the archive has been behind the server
}

// archiveLag is how long the archive has been behind the server: since the
// stream was last level with it (or started), or since the oldest binlog
// waiting for upload was closed, whichever is longer.
func (st *ArchiverStatus) archiveLag(now time.Time) time.Duration {
	var lag time.Duration
	level := st.State == "streaming" && st.LagBytes == 0 && st.CheckedAt != nil && now.Sub(*st.CheckedAt) < 2*time.Minute
	if !level {
		ref := st.StartedAt
		if st.CaughtUpAt != nil && st.CaughtUpAt.After(ref) {
			ref = *st.CaughtUpAt
		}
		lag = now.Sub(ref)
	}
	if st.Pending > 0 && st.PendingSince != nil {
		lag = max(lag, now.Sub(*st.PendingSince))
	}
	return max(lag, 0)
}

type archiver struct {
	r        *Runner
	jobID    int64
	serverID int64
	dir      string
	cancel   context.CancelFunc
	done     chan struct{}
	kick     chan struct{}
	rotate   chan struct{}
	restart  chan struct{}
	lg       *tasklog.Logger

	mu       sync.Mutex
	st       ArchiverStatus
	current  string // file being written, or the one to resume from
	orphans  map[string]bool
	tail     *binlog.Tail // owned by the session goroutine
	curInfo  binlog.Info  // snapshot of tail.Info for other goroutines
	srvSizes map[string]int64
	lastGrow time.Time
	lastSize int64

	fileMu    sync.Mutex // guards deletion of staged files (vs. PITR snapshots)
	nextTry   map[string]time.Time
	attempts  map[string]int
	refetched map[string]bool
	fails     int
	notified  bool
}

func (r *Runner) BinlogLogPath(jobID int64) string {
	return filepath.Join(r.DataDir, "logs", fmt.Sprintf("binlog-%d.log", jobID))
}

func (r *Runner) binlogStageDir(jobID int64) string {
	return filepath.Join(r.stagingRoot(), fmt.Sprintf("binlog-%d", jobID))
}

// SyncArchivers starts/stops archivers to match the job configuration.
func (r *Runner) SyncArchivers() {
	jobs, err := r.Store.ListJobs()
	if err != nil {
		return
	}
	exists := map[int64]bool{}
	want := map[int64]*store.Job{}
	for i := range jobs {
		j := &jobs[i]
		exists[j.ID] = true
		if j.Enabled && j.Options.BinlogArchive {
			want[j.ID] = j
		}
	}
	r.archMu.Lock()
	defer r.archMu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	for id, a := range r.archivers {
		j, ok := want[id]
		if ok && j.ServerID == a.serverID {
			continue
		}
		a.stop()
		delete(r.archivers, id)
	}
	// staged binlogs of deleted jobs are never uploaded any more
	if entries, err := os.ReadDir(r.stagingRoot()); err == nil {
		for _, e := range entries {
			var id int64
			if _, err := fmt.Sscanf(e.Name(), "binlog-%d", &id); err == nil && e.IsDir() && !exists[id] {
				os.RemoveAll(filepath.Join(r.stagingRoot(), e.Name()))
			}
		}
	}
	for id, j := range want {
		if _, ok := r.archivers[id]; !ok {
			r.archivers[id] = r.startArchiver(j)
		}
	}
}

// StopArchiver stops a job's archiver until the next SyncArchivers.
func (r *Runner) StopArchiver(jobID int64) {
	r.archMu.Lock()
	defer r.archMu.Unlock()
	if a, ok := r.archivers[jobID]; ok {
		a.stop()
		delete(r.archivers, jobID)
	}
}

func (r *Runner) startArchiver(j *store.Job) *archiver {
	ctx, cancel := context.WithCancel(r.ctx)
	a := &archiver{r: r, jobID: j.ID, serverID: j.ServerID, dir: r.binlogStageDir(j.ID), cancel: cancel, done: make(chan struct{}),
		kick: make(chan struct{}, 1), rotate: make(chan struct{}, 1), restart: make(chan struct{}, 1),
		orphans: map[string]bool{}, srvSizes: map[string]int64{}, nextTry: map[string]time.Time{}, attempts: map[string]int{}, refetched: map[string]bool{}}
	a.st = ArchiverStatus{JobID: j.ID, JobName: j.Name, ServerName: j.ServerName, State: "starting", StartedAt: time.Now()}
	if last := r.Store.LastBinlogFile(j.UUID); last != nil {
		a.st.LastArchived = last.FileName
		ca, lt := last.CreatedAt, last.LastTS
		a.st.ArchivedAt, a.st.ArchivedUntil = &ca, &lt
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(a.done)
		a.run(ctx)
	}()
	return a
}

func (a *archiver) stop() {
	a.cancel()
	select {
	case <-a.done:
	case <-time.After(30 * time.Second):
	}
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (r *Runner) archiver(jobID int64) *archiver {
	r.archMu.Lock()
	defer r.archMu.Unlock()
	return r.archivers[jobID]
}

// ArchiverStatuses returns the state of every running archiver.
func (r *Runner) ArchiverStatuses() []ArchiverStatus {
	r.archMu.Lock()
	list := make([]*archiver, 0, len(r.archivers))
	for _, a := range r.archivers {
		list = append(list, a)
	}
	r.archMu.Unlock()
	out := make([]ArchiverStatus, 0, len(list))
	for _, a := range list {
		out = append(out, a.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobName < out[j].JobName })
	return out
}

func (r *Runner) ArchiverStatus(jobID int64) (ArchiverStatus, bool) {
	a := r.archiver(jobID)
	if a == nil {
		return ArchiverStatus{}, false
	}
	return a.status(), true
}

// RotateBinlog asks the server to close the current binlog so that it is archived now.
func (r *Runner) RotateBinlog(jobID int64) error {
	a := r.archiver(jobID)
	if a == nil {
		return errors.New("binlog archiving is not enabled for this job")
	}
	poke(a.rotate)
	return nil
}

func (r *Runner) RestartArchiver(jobID int64) error {
	a := r.archiver(jobID)
	if a == nil {
		return errors.New("binlog archiving is not enabled for this job")
	}
	poke(a.restart)
	return nil
}

func (a *archiver) status() ArchiverStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.st
	st.ArchiveLag = int64(st.archiveLag(time.Now()).Seconds())
	return st
}

func (a *archiver) set(fn func(st *ArchiverStatus)) {
	a.mu.Lock()
	fn(&a.st)
	a.mu.Unlock()
}

func openArchiveLog(p string) *tasklog.Logger {
	// long running: keep one previous generation instead of growing forever
	if st, err := os.Stat(p); err == nil && st.Size() > 8<<20 {
		_ = os.Rename(p, p+".1")
	}
	lg, err := tasklog.Open(p)
	if err != nil {
		return tasklog.Discard()
	}
	return lg
}

func (a *archiver) run(ctx context.Context) {
	a.lg = openArchiveLog(a.r.BinlogLogPath(a.jobID))
	defer a.lg.Close()
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		a.lg.Printf("ERROR: cannot create staging folder %s: %v", a.dir, err)
	}
	a.lg.Printf("DumpCat %s — binlog archiving started (staging folder %s)", a.r.AppVersion, a.dir)
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		a.uploader(ctx)
	}()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(began) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		if errors.Is(err, errRestart) {
			a.lg.Printf("Restarting the binlog stream")
			continue
		}
		a.failed(err)
		next := time.Now().Add(backoff)
		a.set(func(st *ArchiverStatus) { st.State, st.NextTry = "retrying", &next; st.Restarts++ })
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		case <-a.restart:
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
	<-upDone
	a.set(func(st *ArchiverStatus) { st.State = "stopped" })
	a.lg.Printf("Binlog archiving stopped")
}

func (a *archiver) failed(err error) {
	if err == nil {
		err = errors.New("process exited")
	}
	now := time.Now()
	a.set(func(st *ArchiverStatus) { st.Error, st.ErrorAt = err.Error(), &now })
	a.lg.Printf("ERROR: %v", err)
	a.fails++
	if a.fails >= 3 && !a.notified {
		a.notified = true
		job, jerr := a.r.Store.GetJob(a.jobID)
		if jerr == nil && job.NotifyFailure && a.r.Notifier != nil {
			ev := notify.Event{Kind: "binlog", Status: store.StatusFailed, Title: "Binlog archiving FAILED: " + job.Name, Error: err.Error(), URL: a.r.link("/#/binlogs")}
			ev.Add("Server", job.ServerName)
			a.r.Notifier.Send(ev)
		}
	}
}

// ---- server queries ----

type serverLog struct {
	Name string
	Size int64
}

func scanStrings(rows *sql.Rows) ([]sql.NullString, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	return vals, rows.Scan(ptrs...)
}

func binaryLogs(ctx context.Context, db *sql.DB) ([]serverLog, error) {
	rows, err := db.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []serverLog
	for rows.Next() {
		v, err := scanStrings(rows)
		if err != nil {
			return nil, err
		}
		if len(v) < 2 {
			continue
		}
		var sz int64
		fmt.Sscan(v[1].String, &sz)
		out = append(out, serverLog{Name: v[0].String, Size: sz})
	}
	return out, rows.Err()
}

func binlogStatus(ctx context.Context, db *sql.DB) (string, int64, error) {
	var lastErr error
	// MySQL >= 8.2 renamed SHOW MASTER STATUS
	for _, q := range []string{"SHOW BINARY LOG STATUS", "SHOW MASTER STATUS"} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			lastErr = err
			continue
		}
		defer rows.Close()
		if !rows.Next() {
			return "", 0, errors.New("binary logging is disabled on the server (log_bin=OFF)")
		}
		v, err := scanStrings(rows)
		if err != nil || len(v) < 2 {
			return "", 0, err
		}
		var pos int64
		fmt.Sscan(v[1].String, &pos)
		return v[0].String, pos, nil
	}
	return "", 0, lastErr
}

// ---- local staging ----

func binlogLess(a, b string) bool {
	ba, sa, _ := binlog.SplitName(a)
	bb, sb, _ := binlog.SplitName(b)
	if ba != bb {
		return ba < bb
	}
	return sa < sb
}

func (a *archiver) localFiles() []string {
	entries, _ := os.ReadDir(a.dir)
	var out []string
	for _, e := range entries {
		if !e.IsDir() && binlog.IsName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return binlogLess(out[i], out[j]) })
	return out
}

// closedFiles lists staged files the server has finished with.
func (a *archiver) closedFiles() []string {
	a.mu.Lock()
	cur := a.current
	orphans := make(map[string]bool, len(a.orphans))
	for k := range a.orphans {
		orphans[k] = true
	}
	a.mu.Unlock()
	if cur == "" {
		return nil
	}
	cb, cs, _ := binlog.SplitName(cur)
	var out []string
	for _, f := range a.localFiles() {
		if f == cur {
			continue
		}
		b, s, _ := binlog.SplitName(f)
		if orphans[f] || b != cb || s < cs {
			out = append(out, f)
		}
	}
	return out
}

// resumePoint picks the binlog to start streaming from.
func (a *archiver) resumePoint(job *store.Job, logs []serverLog) string {
	onServer := map[string]bool{}
	for _, l := range logs {
		onServer[l.Name] = true
	}
	// 1. the newest staged file still on the server (re-streamed from its start)
	resume := ""
	for _, f := range a.localFiles() {
		if !onServer[f] {
			a.mu.Lock()
			if !a.orphans[f] {
				a.lg.Printf("WARNING: %s is no longer on the server — archiving the part received", f)
			}
			a.orphans[f] = true
			a.mu.Unlock()
			continue
		}
		if resume == "" || binlogLess(resume, f) {
			resume = f
		}
	}
	if resume != "" {
		return resume
	}
	newest := logs[len(logs)-1]
	nb, ns, _ := binlog.SplitName(newest.Name)
	// 2. the file after the last archived one
	if last := a.r.Store.LastBinlogFile(job.UUID); last != nil {
		lb, ls, _ := binlog.SplitName(last.FileName)
		for _, l := range logs {
			b, s, _ := binlog.SplitName(l.Name)
			if b == lb && s > ls {
				if s != ls+1 {
					a.lg.Printf("WARNING: binlogs %s.%06d → %s.%06d are missing (purged on the server before they were archived) — PITR cannot cross this gap", lb, ls+1, lb, s-1)
				}
				return l.Name
			}
		}
		if nb != lb || ns < ls {
			a.lg.Printf("The server restarted its binlog numbering (RESET MASTER / physical restore) — starting a new sequence at %s", logs[0].Name)
			return logs[0].Name
		}
		return newest.Name
	}
	// 3. first start: the oldest binlog a retained backup of this job needs
	runs, _ := a.r.Store.RetentionCandidates(job.ID)
	best, older := "", false
	for i := range runs {
		for _, p := range runStarts(&runs[i]) {
			b, s, ok := binlog.SplitName(p.File)
			if !ok || b != nb {
				continue
			}
			if onServer[p.File] {
				if best == "" || binlogLess(p.File, best) {
					best = p.File
				}
			} else if _, s0, _ := binlog.SplitName(logs[0].Name); s < s0 {
				older = true
			}
		}
	}
	switch {
	case older:
		a.lg.Printf("Some backups need binlogs already purged on the server — starting at the oldest one left, %s", logs[0].Name)
		return logs[0].Name
	case best != "":
		return best
	}
	return newest.Name
}

type lastBytes struct {
	mu sync.Mutex
	b  []byte
}

func (l *lastBytes) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.b = append(l.b, p...)
	if len(l.b) > 1024 {
		l.b = l.b[len(l.b)-1024:]
	}
	l.mu.Unlock()
	return len(p), nil
}

func (l *lastBytes) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(strings.Fields(string(l.b)), " ")
}

// session runs one mysqlbinlog process until it exits.
func (a *archiver) session(ctx context.Context) error {
	r := a.r
	job, err := r.Store.GetJob(a.jobID)
	if err != nil {
		return err
	}
	server, err := r.Store.GetServer(job.ServerID)
	if err != nil {
		return fmt.Errorf("server not found: %w", err)
	}
	a.lg.Redact(server.Password)
	db, err := dbconn.Open(ctx, server, "")
	if err != nil {
		return fmt.Errorf("cannot connect to %s: %w", server.Name, err)
	}
	defer db.Close()
	var version, comment, logBin sql.NullString
	_ = db.QueryRowContext(ctx, "SELECT VERSION(), @@version_comment, @@log_bin").Scan(&version, &comment, &logBin)
	if logBin.String != "1" {
		return errors.New("binary logging is disabled on the server (log_bin=OFF) — enable log_bin and server_id to archive binlogs")
	}
	flavor := server.Flavor
	if flavor == "" || flavor == "auto" {
		flavor = dbconn.DetectFlavor(version.String, comment.String)
	}
	logs, err := binaryLogs(ctx, db)
	if err != nil {
		return fmt.Errorf("SHOW BINARY LOGS: %w (the user needs REPLICATION CLIENT / BINLOG MONITOR)", err)
	}
	if len(logs) == 0 {
		return errors.New("the server has no binary logs")
	}
	start := a.resumePoint(job, logs)
	if err := engine.CheckBinlogName(start); err != nil {
		return err
	}
	toolName := engine.BinlogToolName(flavor)
	tool := r.Tools.Get(toolName)
	if !tool.Found {
		return fmt.Errorf("%s is missing on the DumpCat host (install: %s)", toolName, tool.Hint)
	}
	cnfDir := filepath.Join(a.dir, ".conn")
	defer os.RemoveAll(cnfDir)
	cnf, extraEnv, err := engine.WriteCnf(cnfDir, server, engine.CnfOptions{Groups: []string{"client"}, TLS: true, MariaDBClient: tool.MariaDB})
	if err != nil {
		return err
	}
	args := engine.BinlogStreamArgs(r.Tools, toolName, cnf, a.dir, start, uint32(binlogServerIDBase+a.jobID%100_000_000), true)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	cmd := execx.Command(sctx, tool.Path, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	lw := a.lg.Writer("[" + toolName + "] ")
	defer lw.Close()
	errTail := &lastBytes{}
	cmd.Stdout = lw
	cmd.Stderr = io.MultiWriter(lw, errTail)
	a.lg.Printf("$ %s", execx.Quote(tool.Path, args))
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	now := time.Now()
	a.mu.Lock()
	a.current = start
	a.tail = &binlog.Tail{Path: filepath.Join(a.dir, start)}
	a.curInfo = binlog.Info{}
	a.lastGrow, a.lastSize = now, -1
	a.st.State, a.st.Since, a.st.NextTry, a.st.Current = "streaming", &now, nil, start
	a.mu.Unlock()
	a.lg.Printf("Streaming binlogs from %s (%s) starting at %s", server.Name, version.String, start)
	poke(a.kick)

	t := time.NewTicker(archiveTick)
	defer t.Stop()
	for {
		select {
		case err := <-exited:
			if msg := errTail.String(); msg != "" {
				return fmt.Errorf("%s stopped (%v): %s", toolName, err, msg)
			}
			return fmt.Errorf("%s stopped: %v", toolName, err)
		case <-ctx.Done():
			scancel()
			<-exited
			return ctx.Err()
		case <-a.restart:
			scancel()
			<-exited
			return errRestart
		case <-a.rotate:
			a.flush(ctx, db, "on request")
		case <-t.C:
			if err := a.monitor(ctx, db); err != nil {
				scancel()
				<-exited
				return err
			}
		}
	}
}

func (a *archiver) flush(ctx context.Context, db *sql.DB, why string) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(qctx, "FLUSH BINARY LOGS"); err != nil {
		a.lg.Printf("Cannot close the current binlog (FLUSH BINARY LOGS needs RELOAD): %v", err)
		return
	}
	now := time.Now()
	a.set(func(st *ArchiverStatus) { st.RotatedAt = &now })
	a.lg.Printf("Asked the server to close the current binlog (%s)", why)
}

// monitor follows the stream, computes lag, rotates and detects stalls.
func (a *archiver) monitor(ctx context.Context, db *sql.DB) error {
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// server position first: everything up to it is then known to be received
	// when the local copy is at least as far
	sfile, spos, serr := binlogStatus(qctx, db)
	logs, _ := binaryLogs(qctx, db)

	a.mu.Lock()
	cur := a.current
	for _, f := range a.localFiles() {
		cb, cs, _ := binlog.SplitName(cur)
		b, s, _ := binlog.SplitName(f)
		if b == cb && s > cs {
			cur = f
		}
	}
	changed := cur != a.current
	if changed {
		a.current = cur
		a.tail = &binlog.Tail{Path: filepath.Join(a.dir, cur)}
		a.curInfo = binlog.Info{}
	}
	tl := a.tail
	if len(logs) > 0 {
		a.srvSizes = map[string]int64{}
		for _, l := range logs {
			a.srvSizes[l.Name] = l.Size
		}
	}
	a.mu.Unlock()
	if changed {
		poke(a.kick)
	}
	if err := tl.Update(); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.lg.Printf("Cannot read %s: %v", cur, err)
	}
	inf := tl.Info
	now := time.Now()

	var lag int64
	if serr == nil {
		if sfile == cur {
			lag = max(spos-inf.Size, 0)
		} else {
			cb, cs, _ := binlog.SplitName(cur)
			for _, l := range logs {
				b, s, _ := binlog.SplitName(l.Name)
				if b == cb && s >= cs {
					lag += l.Size
				}
			}
			lag = max(lag-inf.Size, 0)
		}
	}
	a.mu.Lock()
	a.curInfo = inf // the tail itself is only touched by this goroutine
	if inf.Size != a.lastSize {
		a.lastSize, a.lastGrow = inf.Size, now
	}
	stalled := serr == nil && lag > 0 && now.Sub(a.lastGrow) > stallTimeout
	st := &a.st
	st.Current, st.Received = cur, inf.Size
	if inf.Events > 0 {
		t := inf.LastTS
		st.ReceivedAt = &t
	}
	if serr == nil {
		st.ServerFile, st.ServerPos, st.CheckedAt, st.LagBytes = sfile, spos, &now, lag
		st.LagSeconds = 0
		if lag == 0 {
			st.CaughtUpAt = &now
		}
		if lag > 0 && st.ReceivedAt != nil {
			st.LagSeconds = int64(now.Sub(*st.ReceivedAt).Seconds())
		}
		if st.State == "streaming" && st.Error != "" && st.Since != nil && now.Sub(*st.Since) > time.Minute {
			st.Error, st.ErrorAt = "", nil
			a.fails, a.notified = 0, false
		}
	}
	a.mu.Unlock()
	if stalled {
		return errStalled
	}

	// bound the data loss window: close the file once it holds transactions
	// for longer than the configured interval
	if job, err := a.r.Store.GetJob(a.jobID); err == nil && job.Options.BinlogRotateMinutes > 0 && serr == nil && sfile == cur && inf.HasTrx {
		every := time.Duration(job.Options.BinlogRotateMinutes) * time.Minute
		a.mu.Lock()
		recent := a.st.RotatedAt != nil && now.Sub(*a.st.RotatedAt) < time.Minute
		a.mu.Unlock()
		if !recent && now.Sub(inf.FirstTS) >= every {
			a.flush(ctx, db, fmt.Sprintf("every %d minutes", job.Options.BinlogRotateMinutes))
		}
	}
	return nil
}

// ---- upload ----

func (a *archiver) uploader(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		a.uploadPending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
		}
	}
}

func (a *archiver) uploadPending(ctx context.Context) {
	closed := a.closedFiles()
	pendingBytes, since := a.pendingInfo(closed)
	a.set(func(st *ArchiverStatus) {
		st.Pending, st.PendingBytes, st.PendingSince = len(closed), pendingBytes, since
	})
	if len(closed) == 0 {
		return
	}
	job, err := a.r.Store.GetJob(a.jobID)
	if err != nil {
		return
	}
	for _, name := range closed {
		if ctx.Err() != nil {
			return
		}
		if t, ok := a.nextTry[name]; ok && time.Now().Before(t) {
			continue
		}
		if err := a.archiveFile(ctx, job, name); err != nil {
			if ctx.Err() != nil {
				return
			}
			a.attempts[name]++
			wait := min(time.Duration(a.attempts[name])*time.Minute, 30*time.Minute)
			a.nextTry[name] = time.Now().Add(wait)
			msg := fmt.Sprintf("%s: %v", name, err)
			a.set(func(st *ArchiverStatus) { st.UploadError = msg })
			a.lg.Printf("Archiving %s FAILED (retrying in %s): %v", name, humanDuration(wait), err)
			continue
		}
		delete(a.nextTry, name)
		delete(a.attempts, name)
		delete(a.refetched, name)
	}
	left := a.closedFiles()
	leftBytes, since := a.pendingInfo(left)
	a.set(func(st *ArchiverStatus) {
		st.Pending, st.PendingBytes, st.PendingSince = len(left), leftBytes, since
		if len(left) == 0 {
			st.UploadError = ""
		}
	})
}

// pendingInfo returns the size of the staged closed binlogs and when the
// oldest of them was closed (its last write).
func (a *archiver) pendingInfo(files []string) (int64, *time.Time) {
	var n int64
	var oldest *time.Time
	for _, f := range files {
		st, err := os.Stat(filepath.Join(a.dir, f))
		if err != nil {
			continue
		}
		n += st.Size()
		if t := st.ModTime(); oldest == nil || t.Before(*oldest) {
			oldest = &t
		}
	}
	return n, oldest
}

type binlogTarget struct {
	st      *store.Storage
	backend storage.Backend
}

func (a *archiver) targets(job *store.Job) ([]binlogTarget, error) {
	var out []binlogTarget
	for _, t := range job.Targets {
		st, err := a.r.Store.GetStorage(t.StorageID)
		if err != nil {
			continue
		}
		b, err := a.r.Storage.Get(st)
		if err != nil {
			return nil, fmt.Errorf("storage %s: %w", st.Name, err)
		}
		out = append(out, binlogTarget{st: st, backend: b})
	}
	if len(out) == 0 {
		return nil, errors.New("the job has no storage targets")
	}
	return out, nil
}

// archiveFile uploads one closed binlog to every storage of the job and
// removes the staged copy once all of them hold it.
func (a *archiver) archiveFile(ctx context.Context, job *store.Job, name string) error {
	raw := filepath.Join(a.dir, name)
	a.mu.Lock()
	orphan := a.orphans[name]
	srvSize, onServer := a.srvSizes[name]
	a.mu.Unlock()
	inf, err := binlog.ScanFile(raw)
	if err != nil {
		return err
	}
	if inf.Events == 0 {
		if orphan {
			a.removeStaged(name)
			return nil
		}
		return errors.New("empty file")
	}
	if onServer && !orphan && inf.Size != srvSize {
		if a.refetched[name] {
			// e.g. binlog encryption at rest: the server file differs from the stream
			a.lg.Printf("WARNING: %s still differs from the server size after a re-download (%d / %d bytes) — archiving the copy received", name, inf.Size, srvSize)
		} else {
			a.lg.Printf("%s: the copy received (%d bytes) differs from the server (%d bytes) — downloading it again", name, inf.Size, srvSize)
			a.refetched[name] = true
			if err := a.refetch(ctx, job, name); err != nil {
				return fmt.Errorf("re-downloading %s: %w", name, err)
			}
			if inf, err = binlog.ScanFile(raw); err != nil {
				return err
			}
		}
	}
	if inf.Truncated {
		a.lg.Printf("WARNING: %s ends in the middle of an event — archiving only the first %d bytes (up to the last complete event)", name, inf.Size)
		if err := os.Truncate(raw, inf.Size); err != nil {
			return err
		}
	}
	targets, err := a.targets(job)
	if err != nil {
		return err
	}
	_, seq, _ := binlog.SplitName(name)
	stored := filepath.Join(a.dir, ".enc", name+".stored")
	row, err := a.r.Store.FindBinlogFile(job.UUID, name, inf.FirstTS)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	op := &storage.Op{Log: a.lg, BwLimit: job.Options.BwLimit}
	if row == nil {
		comp := pipeline.NormalizeCompression(job.Compression)
		pass := ""
		if job.Encrypt {
			if job.Passphrase == "" {
				return errNoPassphrase // never archive in plaintext what should be encrypted
			}
			pass = job.Passphrase
		}
		size, sum, rawSum, err := encodeBinlog(raw, stored, pipeline.ChainOptions{Compression: comp, Level: job.CompressionLevel, Threads: 2, Passphrase: pass})
		if err != nil {
			os.Remove(stored)
			return err
		}
		rel := a.storagePath(job, name, inf.FirstTS, comp, pass != "")
		jid, sid := job.ID, job.ServerID
		row = &store.BinlogFile{JobID: &jid, JobUUID: job.UUID, JobName: job.Name, ServerID: &sid, ServerName: job.ServerName,
			FileName: name, Seq: seq, FirstTS: inf.FirstTS, LastTS: inf.LastTS, SizeBytes: inf.Size, StoredBytes: size,
			SHA256: sum, RawSHA256: rawSum, Compression: comp, Encrypted: pass != "", Passphrase: pass, Path: rel}
		var errs []string
		for _, t := range targets {
			sid := t.st.ID
			loc := store.BinlogLocation{StorageID: &sid, StorageName: t.st.Name, StorageType: t.st.Type, Status: store.LocOK}
			if err := putFile(ctx, t.backend, stored, rel, size, op); err != nil {
				loc.Status, loc.Error = store.LocFailed, err.Error()
				errs = append(errs, t.st.Name+": "+err.Error())
			}
			row.Locations = append(row.Locations, loc)
		}
		if len(errs) == len(targets) {
			os.Remove(stored) // nothing refers to these bytes yet: re-encode next time
			return errors.New(strings.Join(errs, "; "))
		}
		if err := a.r.Store.AddBinlogFile(row); err != nil {
			return err
		}
		if len(errs) > 0 {
			return errors.New(strings.Join(errs, "; ")) // retried later from the stored copy
		}
	}
	// storages still missing the file (earlier failures, targets added later)
	var errs []string
	for _, t := range targets {
		if row.OKOn(t.st.ID) != nil {
			continue
		}
		if err := a.ensureStored(ctx, row, stored, targets); err != nil {
			return err
		}
		var loc *store.BinlogLocation
		for i := range row.Locations {
			if l := &row.Locations[i]; l.StorageID != nil && *l.StorageID == t.st.ID {
				loc = l
			}
		}
		if loc == nil {
			sid := t.st.ID
			row.Locations = append(row.Locations, store.BinlogLocation{BinlogID: row.ID, StorageID: &sid, StorageName: t.st.Name, StorageType: t.st.Type})
			loc = &row.Locations[len(row.Locations)-1]
		}
		loc.Status, loc.Error = store.LocOK, ""
		if err := putFile(ctx, t.backend, stored, row.Path, row.StoredBytes, op); err != nil {
			loc.Status, loc.Error = store.LocFailed, err.Error()
			errs = append(errs, t.st.Name+": "+err.Error())
		}
		_ = a.r.Store.SaveBinlogLocation(loc)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	a.removeStaged(name)
	now := time.Now()
	lt := inf.LastTS
	a.set(func(st *ArchiverStatus) {
		st.LastArchived, st.ArchivedAt = name, &now
		if st.ArchivedUntil == nil || lt.After(*st.ArchivedUntil) {
			st.ArchivedUntil = &lt
		}
	})
	loc := a.r.location()
	a.lg.Printf("Archived %s (%s → %s, %s → %s) to %d storage(s)", name, inf.FirstTS.In(loc).Format("15:04:05"), inf.LastTS.In(loc).Format("2006-01-02 15:04:05"),
		humanBytes(inf.Size), humanBytes(row.StoredBytes), len(targets))
	return nil
}

func (a *archiver) removeStaged(name string) {
	a.fileMu.Lock()
	os.Remove(filepath.Join(a.dir, name))
	os.Remove(filepath.Join(a.dir, ".enc", name+".stored"))
	a.fileMu.Unlock()
	a.mu.Lock()
	delete(a.orphans, name)
	a.mu.Unlock()
}

// storagePath is <job>/binlog/<day>/<file><ext>; a file of an older binlog
// numbering with the same name gets its creation time appended.
func (a *archiver) storagePath(job *store.Job, name string, first time.Time, comp string, enc bool) string {
	dir := JobDir(job.Name, job.ID) + "/binlog/" + first.In(a.r.location()).Format("20060102")
	ext := pipeline.Ext(comp, enc)
	rel := dir + "/" + name + ext
	if a.r.Store.BinlogPathUsed(rel) {
		rel = fmt.Sprintf("%s/%s-%d%s", dir, name, first.Unix(), ext)
	}
	return rel
}

// ensureStored makes sure the encoded copy recorded in row exists locally,
// fetching it back from a storage that has it when needed.
func (a *archiver) ensureStored(ctx context.Context, row *store.BinlogFile, stored string, targets []binlogTarget) error {
	if sum, err := fileSHA256(stored); err == nil && sum == row.SHA256 {
		return nil
	}
	for _, t := range targets {
		if row.OKOn(t.st.ID) == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(stored), 0o700); err != nil {
			return err
		}
		if err := t.backend.Download(ctx, row.Path, stored, nil); err != nil {
			continue
		}
		if sum, err := fileSHA256(stored); err == nil && sum == row.SHA256 {
			return nil
		}
	}
	return errors.New("the local encoded copy is gone and could not be fetched from another storage")
}

// refetch downloads one file again from the server (single file, no follow).
func (a *archiver) refetch(ctx context.Context, job *store.Job, name string) error {
	server, err := a.r.Store.GetServer(job.ServerID)
	if err != nil {
		return err
	}
	info, err := dbconn.Inspect(ctx, server, false)
	if err != nil {
		return err
	}
	flavor := server.Flavor
	if flavor == "" || flavor == "auto" {
		flavor = info.Flavor
	}
	toolName := engine.BinlogToolName(flavor)
	tool := a.r.Tools.Get(toolName)
	if !tool.Found {
		return fmt.Errorf("%s is missing", toolName)
	}
	tmp := filepath.Join(a.dir, ".refetch")
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	cnf, extraEnv, err := engine.WriteCnf(filepath.Join(tmp, ".conn"), server, engine.CnfOptions{Groups: []string{"client"}, TLS: true, MariaDBClient: tool.MariaDB})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := execx.Command(cctx, tool.Path, engine.BinlogStreamArgs(a.r.Tools, toolName, cnf, tmp, name, 0, false)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	w := a.lg.Writer("[" + toolName + "] ")
	defer w.Close()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return err
	}
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	return os.Rename(filepath.Join(tmp, name), filepath.Join(a.dir, name))
}

// encodeBinlog compresses/encrypts src into dst like a backup file.
func encodeBinlog(src, dst string, o pipeline.ChainOptions) (size int64, sum, rawSum string, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, "", "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, "", "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", "", err
	}
	defer out.Close()
	ch, err := pipeline.NewWriteChain(out, o)
	if err != nil {
		return 0, "", "", err
	}
	rh := sha256.New()
	if _, err := io.Copy(ch, io.TeeReader(in, rh)); err != nil {
		ch.Close()
		return 0, "", "", err
	}
	if err := ch.Close(); err != nil {
		return 0, "", "", err
	}
	if err := out.Sync(); err != nil {
		return 0, "", "", err
	}
	return ch.OutBytes(), ch.SHA256(), hex.EncodeToString(rh.Sum(nil)), nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// putFile uploads a local file and checks its size on the storage.
func putFile(ctx context.Context, b storage.Backend, local, rel string, size int64, op *storage.Op) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := b.PutStream(ctx, rel, f, op); err != nil {
		return err
	}
	entries, err := b.List(ctx, path.Dir(rel))
	if err != nil {
		return fmt.Errorf("post-upload check: %w", err)
	}
	for _, e := range entries {
		if e.Name == path.Base(rel) {
			if e.Size >= 0 && e.Size != size {
				return fmt.Errorf("wrong size after upload (%d != %d)", e.Size, size)
			}
			return nil
		}
	}
	return errors.New("file not found on the storage after upload")
}

// snapshotCurrent copies the file still being streamed (up to its last
// complete event) for a restore. caughtUp tells whether the copy held every
// event the server had written at checkedAt.
func (a *archiver) snapshotCurrent(dst string) (name string, inf *binlog.Info, caughtUp bool, checkedAt time.Time, err error) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	st := a.status()
	a.mu.Lock()
	name = a.current
	a.mu.Unlock()
	if name == "" {
		return "", nil, false, time.Time{}, errors.New("no binlog is being received yet")
	}
	in, err := os.Open(filepath.Join(a.dir, name))
	if err != nil {
		return "", nil, false, time.Time{}, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", nil, false, time.Time{}, err
	}
	out, err := os.Create(dst)
	if err != nil {
		return "", nil, false, time.Time{}, err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", nil, false, time.Time{}, err
	}
	if inf, err = binlog.ScanFile(dst); err != nil {
		return "", nil, false, time.Time{}, err
	}
	if inf.Truncated {
		if err := os.Truncate(dst, inf.Size); err != nil {
			return "", nil, false, time.Time{}, err
		}
	}
	if st.CheckedAt != nil && st.State == "streaming" && st.LagBytes == 0 && st.Current == name && time.Since(*st.CheckedAt) < time.Minute {
		caughtUp, checkedAt = true, *st.CheckedAt
	}
	return name, inf, caughtUp, checkedAt, nil
}

// currentInfo describes the file being streamed (for planning).
func (a *archiver) currentInfo() (string, binlog.Info, ArchiverStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current, a.curInfo, a.st
}

// ---- deletion ----

// DeleteJobBinlogs removes every archived binlog of a job from all storages.
func (r *Runner) DeleteJobBinlogs(ctx context.Context, jobUUID string) (int, error) {
	files, err := r.Store.ListBinlogFiles(jobUUID)
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, f := range files {
		for _, l := range f.Locations {
			if l.StorageID != nil && l.Status == store.LocOK {
				st, err := r.Store.GetStorage(*l.StorageID)
				if err == nil {
					b, err := r.Storage.Get(st)
					if err == nil {
						err = b.DeleteFile(ctx, f.Path)
					}
					if err != nil {
						errs = append(errs, fmt.Errorf("%s %s: %w", l.StorageName, f.FileName, err))
						continue
					}
				}
			}
			_ = r.Store.DeleteBinlogLocation(l.ID)
		}
		r.Store.DeleteBinlogIfOrphan(f.ID)
		n++
	}
	return n, errors.Join(errs...)
}
