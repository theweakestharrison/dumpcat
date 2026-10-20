package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/binlog"
	"dumpcat/internal/dbconn"
	"dumpcat/internal/execx"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// MySQL's log archiver: one supervised `mysqlbinlog --raw --stop-never` per
// job copies the server's binary logs into the staging folder; every file
// the server has closed is archived by the runner, the file still being
// written stays local until it rotates. A recovery replays the archived
// files with `mysqlbinlog | mysql` after the base restore.

const (
	archiveTick = 5 * time.Second
	// replication server id of the archiver connection: must differ from
	// every real server/replica id of the source, and per job
	binlogServerIDBase = 3_900_000_000
	stallTimeout       = 3 * time.Minute
)

// ErrStalled ends a session whose stream stopped growing although the
// server kept writing.
var ErrStalled = errors.New("no new data although the server kept writing — reconnecting")

var mysqlLogInfo = LogInfo{Name: "binlog", Files: "binlogs", Prefix: "binlog", Close: "FLUSH BINARY LOGS",
	Targets: []string{"time", "gtid", "position", "latest"}}

func (MySQL) LogInfo() LogInfo { return mysqlLogInfo }

func (MySQL) NewStream(h *LogHost) LogStream {
	return &binlogStream{h: h, orphans: map[string]bool{}, srvSizes: map[string]int64{}, refetched: map[string]bool{}}
}

func (MySQL) CheckTarget(t *PITRTarget) error {
	switch t.Type {
	case "position":
		if err := CheckBinlogName(t.File); err != nil || !binlog.IsName(t.File) {
			return fmt.Errorf("invalid binlog file name: %q", t.File)
		}
		if t.Pos < 4 {
			return errors.New("the binlog position must be ≥ 4")
		}
		*t = PITRTarget{Type: t.Type, File: t.File, Pos: t.Pos}
	case "gtid":
		g, err := binlog.NormalizeGTID(t.GTID)
		if err != nil {
			return err
		}
		*t = PITRTarget{Type: t.Type, GTID: g}
	default:
		return fmt.Errorf("invalid recovery target type: %q", t.Type)
	}
	return nil
}

type binlogStream struct {
	h *LogHost

	mu       sync.Mutex
	current  string // file being written, or the one to resume from
	orphans  map[string]bool
	tail     *binlog.Tail // owned by the session goroutine
	curInfo  binlog.Info  // snapshot of tail.Info for other goroutines
	srvSizes map[string]int64
	lastGrow time.Time
	lastSize int64

	fileMu    sync.Mutex // guards deletion of staged files (vs. PITR snapshots)
	refetched map[string]bool
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

func (a *binlogStream) localFiles() []string {
	entries, _ := os.ReadDir(a.h.Dir)
	var out []string
	for _, e := range entries {
		if !e.IsDir() && binlog.IsName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return binlogLess(out[i], out[j]) })
	return out
}

// Closed lists staged files the server has finished with.
func (a *binlogStream) Closed() []string {
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
func (a *binlogStream) resumePoint(job *store.Job, logs []serverLog) string {
	lg := a.h.Log
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
				lg.Printf("WARNING: %s is no longer on the server — archiving the part received", f)
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
	if last := a.h.Store.LastLogFile(job.UUID); last != nil {
		lb, ls, _ := binlog.SplitName(last.FileName)
		for _, l := range logs {
			b, s, _ := binlog.SplitName(l.Name)
			if b == lb && s > ls {
				if s != ls+1 {
					lg.Printf("WARNING: binlogs %s.%06d → %s.%06d are missing (purged on the server before they were archived) — PITR cannot cross this gap", lb, ls+1, lb, s-1)
				}
				return l.Name
			}
		}
		if nb != lb || ns < ls {
			lg.Printf("The server restarted its binlog numbering (RESET MASTER / physical restore) — starting a new sequence at %s", logs[0].Name)
			return logs[0].Name
		}
		return newest.Name
	}
	// 3. first start: the oldest binlog a retained backup of this job needs
	runs, _ := a.h.Store.RetentionCandidates(job.ID)
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
		lg.Printf("Some backups need binlogs already purged on the server — starting at the oldest one left, %s", logs[0].Name)
		return logs[0].Name
	case best != "":
		return best
	}
	return newest.Name
}

// lastBytes keeps the end of a process's error output.
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

// Session runs one mysqlbinlog process until it exits.
func (a *binlogStream) Session(ctx context.Context, job *store.Job, server *store.Server, rotate <-chan struct{}) error {
	h := a.h
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
	if err := CheckBinlogName(start); err != nil {
		return err
	}
	toolName := BinlogToolName(flavor)
	tool := h.Tools.Get(toolName)
	if !tool.Found {
		return fmt.Errorf("%s is missing on the DumpCat host (install: %s)", toolName, tool.Hint)
	}
	cnfDir := filepath.Join(h.Dir, ".conn")
	defer os.RemoveAll(cnfDir)
	cnf, extraEnv, err := WriteCnf(cnfDir, server, CnfOptions{Groups: []string{"client"}, TLS: true, MariaDBClient: tool.MariaDB})
	if err != nil {
		return err
	}
	args := BinlogStreamArgs(h.Tools, toolName, cnf, h.Dir, start, uint32(binlogServerIDBase+h.JobID%100_000_000), true)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	cmd := execx.Command(sctx, tool.Path, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	lw := h.Log.Writer("[" + toolName + "] ")
	defer lw.Close()
	errTail := &lastBytes{}
	cmd.Stdout = lw
	cmd.Stderr = io.MultiWriter(lw, errTail)
	h.Log.Printf("$ %s", execx.Quote(tool.Path, args))
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	now := time.Now()
	a.mu.Lock()
	a.current = start
	a.tail = &binlog.Tail{Path: filepath.Join(h.Dir, start)}
	a.curInfo = binlog.Info{}
	a.lastGrow, a.lastSize = now, -1
	a.mu.Unlock()
	h.Update(func(st *LogStatus) { st.State, st.Since, st.NextTry, st.Current = "streaming", &now, nil, start })
	h.Log.Printf("Streaming binlogs from %s (%s) starting at %s", server.Name, version.String, start)
	h.Kick()

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
		case <-rotate:
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

func (a *binlogStream) flush(ctx context.Context, db *sql.DB, why string) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(qctx, "FLUSH BINARY LOGS"); err != nil {
		a.h.Log.Printf("Cannot close the current binlog (FLUSH BINARY LOGS needs RELOAD): %v", err)
		return
	}
	now := time.Now()
	a.h.Update(func(st *LogStatus) { st.RotatedAt = &now })
	a.h.Log.Printf("Asked the server to close the current binlog (%s)", why)
}

// monitor follows the stream, computes lag, rotates and detects stalls.
func (a *binlogStream) monitor(ctx context.Context, db *sql.DB) error {
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
		a.tail = &binlog.Tail{Path: filepath.Join(a.h.Dir, cur)}
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
		a.h.Kick()
	}
	if err := tl.Update(); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.h.Log.Printf("Cannot read %s: %v", cur, err)
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
	a.mu.Unlock()
	recent := false
	a.h.Update(func(st *LogStatus) {
		st.Current, st.Received = cur, inf.Size
		if inf.Events > 0 {
			t := inf.LastTS
			st.ReceivedAt = &t
		}
		if serr == nil {
			st.ServerPosition, st.CheckedAt, st.LagBytes = fmt.Sprintf("%s:%d", sfile, spos), &now, lag
			st.LagSeconds = 0
			if lag == 0 {
				st.CaughtUpAt = &now
			}
			if lag > 0 && st.ReceivedAt != nil {
				st.LagSeconds = int64(now.Sub(*st.ReceivedAt).Seconds())
			}
		}
		recent = st.RotatedAt != nil && now.Sub(*st.RotatedAt) < time.Minute
	})
	if stalled {
		return ErrStalled
	}

	// bound the data loss window: close the file once it holds transactions
	// for longer than the configured interval
	if job, err := a.h.Store.GetJob(a.h.JobID); err == nil && job.Options.LogRotateMinutes > 0 && serr == nil && sfile == cur && inf.HasTrx {
		every := time.Duration(job.Options.LogRotateMinutes) * time.Minute
		if !recent && now.Sub(inf.FirstTS) >= every {
			a.flush(ctx, db, fmt.Sprintf("every %d minutes", job.Options.LogRotateMinutes))
		}
	}
	return nil
}

// ---- archiving ----

// Seal checks a closed binlog before its upload: empty files of a stream
// that ended are dropped, a copy whose size differs from the server is
// downloaded again, a truncated tail is cut at the last complete event.
func (a *binlogStream) Seal(ctx context.Context, job *store.Job, name string) (*LogFileInfo, error) {
	raw := filepath.Join(a.h.Dir, name)
	a.mu.Lock()
	orphan := a.orphans[name]
	srvSize, onServer := a.srvSizes[name]
	refetched := a.refetched[name]
	a.mu.Unlock()
	inf, err := binlog.ScanFile(raw)
	if err != nil {
		return nil, err
	}
	if inf.Events == 0 {
		if orphan {
			return &LogFileInfo{Skip: true}, nil
		}
		return nil, errors.New("empty file")
	}
	if onServer && !orphan && inf.Size != srvSize {
		if refetched {
			// e.g. binlog encryption at rest: the server file differs from the stream
			a.h.Log.Printf("WARNING: %s still differs from the server size after a re-download (%d / %d bytes) — archiving the copy received", name, inf.Size, srvSize)
		} else {
			a.h.Log.Printf("%s: the copy received (%d bytes) differs from the server (%d bytes) — downloading it again", name, inf.Size, srvSize)
			a.mu.Lock()
			a.refetched[name] = true
			a.mu.Unlock()
			if err := a.refetch(ctx, job, name); err != nil {
				return nil, fmt.Errorf("re-downloading %s: %w", name, err)
			}
			if inf, err = binlog.ScanFile(raw); err != nil {
				return nil, err
			}
		}
	}
	if inf.Truncated {
		a.h.Log.Printf("WARNING: %s ends in the middle of an event — archiving only the first %d bytes (up to the last complete event)", name, inf.Size)
		if err := os.Truncate(raw, inf.Size); err != nil {
			return nil, err
		}
	}
	_, seq, _ := binlog.SplitName(name)
	return &LogFileInfo{Seq: seq, First: inf.FirstTS, Last: inf.LastTS, Size: inf.Size}, nil
}

// Archived removes the staged copy.
func (a *binlogStream) Archived(name string) {
	a.fileMu.Lock()
	os.Remove(filepath.Join(a.h.Dir, name))
	a.fileMu.Unlock()
	a.mu.Lock()
	delete(a.orphans, name)
	delete(a.refetched, name)
	a.mu.Unlock()
}

// refetch downloads one file again from the server (single file, no follow).
func (a *binlogStream) refetch(ctx context.Context, job *store.Job, name string) error {
	server, err := a.h.Store.GetServer(job.ServerID)
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
	toolName := BinlogToolName(flavor)
	tool := a.h.Tools.Get(toolName)
	if !tool.Found {
		return fmt.Errorf("%s is missing", toolName)
	}
	tmp := filepath.Join(a.h.Dir, ".refetch")
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	cnf, extraEnv, err := WriteCnf(filepath.Join(tmp, ".conn"), server, CnfOptions{Groups: []string{"client"}, TLS: true, MariaDBClient: tool.MariaDB})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := execx.Command(cctx, tool.Path, BinlogStreamArgs(a.h.Tools, toolName, cnf, tmp, name, 0, false)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	w := a.h.Log.Writer("[" + toolName + "] ")
	defer w.Close()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return err
	}
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	return os.Rename(filepath.Join(tmp, name), filepath.Join(a.h.Dir, name))
}

// Current describes the file being streamed (for planning).
func (a *binlogStream) Current() LogCurrent {
	st := a.h.Status()
	a.mu.Lock()
	defer a.mu.Unlock()
	return LogCurrent{Name: a.current, Size: a.curInfo.Size, Events: a.curInfo.Events > 0, First: a.curInfo.FirstTS, Last: a.curInfo.LastTS, Status: st}
}

// Snapshot copies the file still being streamed (up to its last complete
// event) for a restore.
func (a *binlogStream) Snapshot(dst string) (*LogSnapshot, error) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	st := a.h.Status()
	a.mu.Lock()
	name := a.current
	a.mu.Unlock()
	if name == "" {
		return nil, errors.New("no binlog is being received yet")
	}
	if err := copyFile(filepath.Join(a.h.Dir, name), dst); err != nil {
		return nil, err
	}
	inf, err := binlog.ScanFile(dst)
	if err != nil {
		return nil, err
	}
	if inf.Truncated {
		if err := os.Truncate(dst, inf.Size); err != nil {
			return nil, err
		}
	}
	snap := &LogSnapshot{Name: name, Size: inf.Size, Last: inf.LastTS}
	if st.Level(name, time.Minute) {
		snap.CaughtUp, snap.CheckedAt = true, *st.CheckedAt
	}
	return snap, nil
}

// Release: binlog archiving keeps nothing on the server.
func (a *binlogStream) Release(context.Context, *store.Server, *tasklog.Logger) {}

// copyFile copies src to dst (0600).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---- point-in-time recovery ----

type binlogPos struct {
	File string `json:"file"`
	Pos  int64  `json:"pos"`
}

func (p binlogPos) String() string { return fmt.Sprintf("%s:%d", p.File, p.Pos) }

// posCompare orders positions of the same binlog numbering (ok=false when
// the base names differ).
func posCompare(a, b binlogPos) (int, bool) {
	ab, as, ok1 := binlog.SplitName(a.File)
	bb, bs, ok2 := binlog.SplitName(b.File)
	if !ok1 || !ok2 || ab != bb {
		return 0, false
	}
	switch {
	case as != bs:
		if as < bs {
			return -1, true
		}
		return 1, true
	case a.Pos < b.Pos:
		return -1, true
	case a.Pos > b.Pos:
		return 1, true
	}
	return 0, true
}

// runStarts returns where binlog replay starts for each database of a run;
// key "" holds the run-wide position (single snapshot dumps, physical).
func runStarts(run *store.Run) map[string]binlogPos {
	out := map[string]binlogPos{}
	if f, _ := run.Meta["binlog_file"].(string); f != "" {
		if p, ok := MetaInt(run.Meta, "binlog_pos"); ok && p > 0 {
			out[""] = binlogPos{f, p}
		}
	}
	if run.Method == store.MethodMysqldump && run.Meta["single_file"] != true {
		for _, d := range run.Databases {
			if d.BinlogFile != "" && d.BinlogPos > 0 {
				out[d.Name] = binlogPos{d.BinlogFile, d.BinlogPos}
			}
		}
	}
	return out
}

func startFor(starts map[string]binlogPos, db string) (binlogPos, bool) {
	if p, ok := starts[db]; ok {
		return p, true
	}
	p, ok := starts[""]
	return p, ok
}

// startRange returns the lowest and highest start position.
func startRange(starts map[string]binlogPos) (lo, hi binlogPos, ok bool) {
	for _, p := range starts {
		if !ok {
			lo, hi, ok = p, p, true
			continue
		}
		if c, _ := posCompare(p, lo); c < 0 {
			lo = p
		}
		if c, _ := posCompare(p, hi); c > 0 {
			hi = p
		}
	}
	return
}

// binlogBaseErr explains why run cannot be the starting point for t.
func binlogBaseErr(run *store.Run, t PITRTarget) error {
	starts := runStarts(run)
	_, hi, ok := startRange(starts)
	if !ok {
		return errors.New("the backup has no binlog position (mysqldump: enable 'Record binlog position')")
	}
	for _, p := range starts {
		if _, ok := posCompare(p, hi); !ok {
			return errors.New("invalid binlog position in the backup")
		}
	}
	switch t.Type {
	case "time":
		if at := ConsistentAt(run); at.After(*t.Time) {
			return fmt.Errorf("the backup already contains data up to %s — after the recovery target", at.Format(time.RFC3339))
		}
	case "position":
		c, ok := posCompare(binlogPos{t.File, t.Pos}, hi)
		if !ok {
			return fmt.Errorf("binlog %s is not in the same sequence as the backup (%s)", t.File, hi.File)
		}
		if c < 0 {
			return fmt.Errorf("the target position is before the backup position (%s)", hi)
		}
	case "gtid":
		set, _ := run.Meta["gtid"].(string)
		if inc, known := binlog.GTIDIncluded(set, t.GTID); known && inc {
			return fmt.Errorf("GTID %s is already contained in the backup", t.GTID)
		}
	}
	return nil
}

func (MySQL) Base(run *store.Run, t *PITRTarget) (*LogBase, error) {
	if err := binlogBaseErr(run, *t); err != nil {
		return nil, err
	}
	starts := runStarts(run)
	lo, _, _ := startRange(starts)
	return &LogBase{Run: run, From: lo.String(), ConsistentAt: ConsistentAt(run), State: starts}, nil
}

// ErrNotArchived: a file a recovery needs is not in the archive.
var ErrNotArchived = errors.New("has not been archived")

// binlogChain follows the archived files from the one holding start through
// consecutive numbers. A RESET MASTER (or a physical restore) restarts the
// numbering, so one name can exist several times: event times cannot tell
// them apart (replayed statements keep their original timestamps), the
// archive order can. The file holding a backup's position was still open
// when the backup started, so it was archived after startedAt.
func binlogChain(files []store.LogFile, start binlogPos, startedAt time.Time) ([]store.LogFile, error) {
	byName := map[string][]int{}
	for i, f := range files {
		byName[f.FileName] = append(byName[f.FileName], i)
	}
	// every file the backup needs was still open (or not written yet) when it
	// started, so it was archived afterwards
	after := func(i int) bool { return !files[i].CreatedAt.Before(startedAt.Truncate(time.Second)) }
	idx := -1
	for _, i := range byName[start.File] {
		if after(i) && (idx < 0 || files[i].ID < files[idx].ID) {
			idx = i
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("binlog %s (where the backup starts) %w", start.File, ErrNotArchived)
	}
	chain := []store.LogFile{files[idx]}
	cur := files[idx]
	for len(chain) <= len(files) {
		b, s, _ := binlog.SplitName(cur.FileName)
		next := -1
		dist := func(i int) int64 { return max(files[i].ID-cur.ID, cur.ID-files[i].ID) }
		for _, i := range byName[fmt.Sprintf("%s.%06d", b, s+1)] {
			if after(i) && (next < 0 || dist(i) < dist(next)) {
				next = i
			}
		}
		if next < 0 {
			break
		}
		cur = files[next]
		chain = append(chain, cur)
	}
	return chain, nil
}

// Plan checks that the base plus archived binlogs reach the target.
func (MySQL) Plan(in *PlanInput) (*LogPlan, error) {
	base, t, cur := in.Base, in.Target, in.Current
	starts := base.State.(map[string]binlogPos)
	lo, _, _ := startRange(starts)
	files := in.Files
	p := &LogPlan{Target: t, Base: base, State: lo}
	chain, cerr := binlogChain(files, lo, base.Run.StartedAt)

	// the file being streamed right now may continue (or start) the chain
	partial := false
	if cur.Name != "" && cur.Events {
		if len(chain) == 0 {
			partial = cur.Name == lo.File
		} else {
			last := chain[len(chain)-1]
			lb, ls, _ := binlog.SplitName(last.FileName)
			cb, cs, _ := binlog.SplitName(cur.Name)
			partial = cb == lb && cs == ls+1
		}
	}
	if cerr != nil && !partial {
		return nil, cerr
	}
	if len(chain) > 0 {
		p.Coverage = chain[len(chain)-1].LastTS
	}
	if partial {
		cov := cur.Last
		if cur.Status.Level(cur.Name, 0) && cur.Status.CheckedAt.After(cov) {
			cov = *cur.Status.CheckedAt
		}
		if cov.After(p.Coverage) {
			p.Coverage = cov
		}
	}
	have := map[string]bool{}
	for _, f := range chain {
		have[f.FileName] = true
	}
	if partial {
		have[cur.Name] = true
	}
	for db, s := range starts {
		if !have[s.File] {
			if db == "" {
				return nil, fmt.Errorf("binlog %s %w", s.File, ErrNotArchived)
			}
			return nil, fmt.Errorf("binlog %s (where database %s starts) %w", s.File, db, ErrNotArchived)
		}
	}
	if len(chain) > 0 && !partial {
		last := chain[len(chain)-1]
		in := map[int64]bool{}
		for _, f := range chain {
			in[f.ID] = true
		}
		for _, f := range files {
			if f.ID > last.ID && !in[f.ID] {
				p.Warnings = append(p.Warnings, fmt.Sprintf("The binlog sequence is broken after %s — later archived files cannot be used with this backup", last.FileName))
				break
			}
		}
	}

	switch t.Type {
	case "time":
		if t.Time.After(p.Coverage) {
			return nil, CoverageError(mysqlLogInfo, p.Coverage, in.Loc)
		}
		limit := t.Time.Add(TimeLimit)
		for _, f := range chain {
			if f.FirstTS.After(limit) {
				break
			}
			p.Files = append(p.Files, f)
		}
		if partial && len(p.Files) == len(chain) && !cur.First.After(limit) {
			p.Partial = cur.Name
		}
	case "position":
		found := false
		for _, f := range chain {
			p.Files = append(p.Files, f)
			if f.FileName == t.File {
				if t.Pos > f.SizeBytes {
					return nil, fmt.Errorf("position %d is beyond the size of %s (%d)", t.Pos, f.FileName, f.SizeBytes)
				}
				found = true
				break
			}
		}
		if !found && partial && cur.Name == t.File {
			if t.Pos > cur.Size {
				return nil, fmt.Errorf("position %d has not been received yet (%s has %d bytes)", t.Pos, cur.Name, cur.Size)
			}
			p.Partial, found = cur.Name, true
		}
		if !found {
			return nil, fmt.Errorf("binlog %s is not in the archived sequence after the backup", t.File)
		}
	default: // gtid, latest
		p.Files = chain
		if partial {
			p.Partial = cur.Name
		}
	}
	for _, f := range p.Files {
		p.Bytes += f.SizeBytes
	}
	if p.Partial != "" {
		p.Bytes += cur.Size
	}
	return p, nil
}

// binlogCheck verifies the fetched binlogs and resolves the stop position.
type binlogCheck struct {
	env     *Env
	plan    *LogPlan
	starts  map[string]binlogPos
	from    binlogPos
	stopper *binlog.Stopper
	resolve bool
	stop    *binlogPos
	files   []LocalLog
}

func (MySQL) NewCheck(env *Env, plan *LogPlan) LogCheck {
	t := plan.Target
	c := &binlogCheck{env: env, plan: plan, starts: plan.Base.State.(map[string]binlogPos), from: plan.State.(binlogPos),
		stopper: &binlog.Stopper{GTID: t.GTID}, resolve: t.Type == "time" || t.Type == "gtid"}
	if t.Type == "time" {
		c.stopper.Time = *t.Time
	}
	return c
}

func (c *binlogCheck) Add(lf LocalLog) (bool, error) {
	from := int64(0)
	if len(c.files) == 0 {
		from = c.from.Pos
	}
	c.files = append(c.files, lf)
	f, err := os.Open(lf.Path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	name := lf.Name
	// every replay start in this file must be an event boundary: if not,
	// this file is not the one the backup saw (e.g. numbering restarted)
	starts := map[int64]bool{}
	for _, s := range c.starts {
		if s.File == name {
			starts[s.Pos] = false
		}
	}
	c.stopper.StartFile(from)
	end, err := binlog.Scan(f, 0, func(e *binlog.Event) error {
		if _, ok := starts[e.Offset]; ok {
			starts[e.Offset] = true
		}
		if !c.resolve {
			return nil
		}
		if off, ok := c.stopper.Feed(e); ok {
			c.stop = &binlogPos{name, off}
			return io.EOF
		}
		return nil
	})
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return false, fmt.Errorf("binlog %s is truncated", name)
	}
	if err != nil {
		return false, fmt.Errorf("binlog %s: %w", name, err)
	}
	for pos, seen := range starts {
		if !seen && pos != end && (c.stop == nil || c.stop.File != name || pos < c.stop.Pos) {
			return false, fmt.Errorf("binlog %s does not match the backup: position %d is not an event boundary (was the binlog numbering reset?)", name, pos)
		}
	}
	if c.resolve && c.stop == nil {
		if off, ok := c.stopper.EndFile(); ok {
			c.stop = &binlogPos{name, off}
		}
	}
	return c.stop != nil, nil
}

func (c *binlogCheck) Finish() (*Replay, error) {
	t := c.plan.Target
	switch t.Type {
	case "position":
		c.stop = &binlogPos{t.File, t.Pos}
	case "gtid":
		if c.stop == nil {
			return nil, fmt.Errorf("GTID %s not found in the binlogs after the base backup", t.GTID)
		}
	}
	if len(c.files) == 0 {
		return nil, errors.New("no binlog to replay")
	}
	rp := &Replay{Plan: c.plan, Files: c.files, State: c.stop}
	if c.stop != nil {
		for db, s := range c.starts {
			if cmp, _ := posCompare(*c.stop, s); cmp < 0 {
				what := "the backup"
				if db != "" {
					what = "database " + db
				}
				return nil, fmt.Errorf("the stop point %s is before the position of %s (%s) — choose a later target", c.stop, what, s)
			}
		}
		rp.Stop = c.stop.String()
		c.env.Log.Printf("PITR: stop point %s (events from this position on are not replayed)", c.stop)
	} else {
		rp.Stop = "end of the binlogs"
		c.env.Log.Printf("PITR: replaying all %d binlog file(s)", len(c.files))
	}
	return rp, nil
}

func waitForServer(ctx context.Context, s *store.Server, lg *tasklog.Logger) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		db, err := dbconn.Open(ctx, s, "")
		if err == nil {
			db.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server %s is not ready: %w", s.Name, err)
		}
		lg.Printf("Waiting for server %s to accept connections...", s.Name)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// binlogSteps turns the checked files into replay steps. Logical restores
// replay each restored database from its own snapshot position (filtered
// and renamed like the restore); physical restores replay everything.
func binlogSteps(rp *Replay, spec *ReplaySpec) []BinlogStep {
	idx := map[string]int{}
	for i, f := range rp.Files {
		idx[f.Name] = i
	}
	stop, _ := rp.State.(*binlogPos)
	last := len(rp.Files) - 1
	if stop != nil {
		if i, ok := idx[stop.File]; ok {
			last = i
		}
	}
	type stream struct {
		db, to string
		from   binlogPos
	}
	var streams []stream
	starts := rp.Plan.Base.State.(map[string]binlogPos)
	if spec.Run.Method == store.MethodPhysical {
		streams = append(streams, stream{from: rp.Plan.State.(binlogPos)})
	} else {
		for _, d := range spec.Databases {
			if p, ok := startFor(starts, d.Name); ok {
				to := d.Name
				if v := strings.TrimSpace(spec.Rename[d.Name]); v != "" {
					to = v
				}
				streams = append(streams, stream{db: d.Name, to: to, from: p})
			}
		}
	}
	var steps []BinlogStep
	for i := 0; i <= last; i++ {
		for _, s := range streams {
			first, ok := idx[s.from.File]
			if !ok || i < first {
				continue
			}
			step := BinlogStep{File: rp.Files[i].Path, Database: s.db}
			if s.to != s.db {
				step.TargetDB = s.to
			}
			if i == first {
				step.StartPos = s.from.Pos
			}
			if stop != nil && i == last {
				step.StopPos = stop.Pos
				if step.StartPos >= step.StopPos {
					continue
				}
			}
			steps = append(steps, step)
		}
	}
	return steps
}

// Replay applies the checked binlogs to the restored server.
func (MySQL) Replay(env *Env, rp *Replay, spec *ReplaySpec) error {
	lg := env.Log
	ctx := env.Ctx
	run, target := spec.Run, spec.Target
	physical := run.Method == store.MethodPhysical
	if physical {
		if err := waitForServer(ctx, target, lg); err != nil {
			return err
		}
	}
	srcFlavor, _ := run.Meta["flavor"].(string)
	if srcFlavor == "" {
		srcFlavor = "mysql"
	}
	info, err := dbconn.Inspect(ctx, target, false)
	if err != nil {
		return fmt.Errorf("could not connect to the target server: %w", err)
	}
	clientFlavor := Flavor(target, info)
	steps := binlogSteps(rp, spec)
	if len(steps) == 0 {
		lg.Printf("PITR: no changes after the backup to replay")
		return nil
	}
	sizes := map[string]int64{}
	var total int64
	for _, s := range steps {
		if _, ok := sizes[s.File]; !ok {
			if fi, err := os.Stat(s.File); err == nil {
				sizes[s.File] = fi.Size()
			}
		}
		total += sizes[s.File]
	}
	if spec.Estimate != nil {
		spec.Estimate(total)
	}
	if spec.FastMode != nil {
		defer spec.FastMode()()
	}
	if physical {
		lg.Printf("PITR: replaying %d binlog file(s) into %s (whole instance)", len(steps), target.Name)
	} else {
		lg.Printf("PITR: replaying binlogs for %d database(s) (%d steps) into %s — each database from its own snapshot position", len(spec.Databases), len(steps), target.Name)
	}
	t0 := time.Now()
	err = ApplyBinlogs(env, target, steps, BinlogApplyOpts{
		BinlogTool:   BinlogToolName(srcFlavor),
		ClientFlavor: clientFlavor,
		NoBinlog:     spec.NoBinlog,
		// a logical restore onto a server that already executed the original
		// transactions (usually the source itself) needs new GTIDs
		SkipGTIDs: !physical,
		OnStep: func(i int) {
			if spec.Progress != nil {
				spec.Progress(sizes[steps[i].File])
			}
		},
	})
	if err != nil {
		return fmt.Errorf("replaying binlogs: %w", err)
	}
	lg.Printf("PITR: binlogs replayed up to %s (%s) in %s", rp.Plan.Target.Describe(env.location()), rp.Stop, HumanDuration(time.Since(t0)))
	return nil
}
