package pgsql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// PostgreSQL's log archiver: pg_receivewal streams the WAL of the server
// into the staging folder over a replication connection, through a
// physical replication slot of the job, so the server keeps every segment
// until DumpCat has it. Complete segments (and timeline history files) are
// archived by the runner; the segment being received stays local as
// <segment>.partial. A recovery is PostgreSQL's own: the physical restore
// runs it with restore_command reading the fetched segments.

const (
	walTick      = 5 * time.Second
	walStall     = 3 * time.Minute
	holdDir      = "hold" // staged files pg_receivewal must not resume from (after a gap)
	partialExt   = ".partial"
	pageHeaderSz = longPageHeader
)

var walLogInfo = engine.LogInfo{Name: "WAL", File: "WAL segment", Files: "WAL segments", Prefix: "wal", Close: "pg_switch_wal()",
	Targets: []string{"time", "name", "lsn", "latest"}, Recovery: true}

func (Driver) LogInfo() engine.LogInfo { return walLogInfo }

func (Driver) NewStream(h *engine.LogHost) engine.LogStream {
	return &walStream{h: h, slot: SlotName(h.JobID, h.JobUUID)}
}

// SlotName is the replication slot of a job's WAL archiving.
func SlotName(jobID int64, uuid string) string {
	u := strings.ToLower(strings.ReplaceAll(uuid, "-", ""))
	var b strings.Builder
	for _, r := range u {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	u = b.String()
	if len(u) > 12 {
		u = u[:12]
	}
	return fmt.Sprintf("dumpcat_job%d_%s", jobID, u)
}

func (Driver) CheckTarget(t *engine.PITRTarget) error {
	switch t.Type {
	case "lsn":
		l, err := ParseLSN(t.LSN)
		if err != nil {
			return err
		}
		*t = engine.PITRTarget{Type: t.Type, LSN: l.String()}
	case "name":
		n := t.Name
		if strings.TrimSpace(n) == "" {
			return errors.New("no restore point name given")
		}
		if len(n) > 63 || !utf8.ValidString(n) || strings.ContainsFunc(n, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("invalid restore point name %q", n)
		}
		*t = engine.PITRTarget{Type: t.Type, Name: n}
	default:
		return fmt.Errorf("invalid recovery target type: %q", t.Type)
	}
	return nil
}

// ---- receiving ----

type walStream struct {
	h    *engine.LogHost
	slot string

	mu       sync.Mutex
	segSize  int64
	current  string // segment being received (name without .partial)
	tail     *walTail
	cur      SegInfo // snapshot of tail.Info for other goroutines
	kept     string  // newest archived segment, kept for pg_receivewal to resume after
	lastEnd  LSN
	lastGrow time.Time
	noSwitch bool   // pg_switch_wal() was refused: not asked again
	lastErr  string // the last error of the monitor's query (logged once)

	fileMu sync.Mutex // guards deletion of staged files (vs. snapshots)
}

// stagedSeg is a WAL file of the staging folder.
type stagedSeg struct {
	name    string // relative to the staging folder
	tli     uint32
	segno   uint64
	partial bool
	history bool
}

func (a *walStream) segSizeOr() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.segSize > 0 {
		return a.segSize
	}
	return DefaultWALSegSize
}

// staged lists the WAL files of dir (and of its hold folder), in WAL order:
// history files first, then segments by number and timeline.
func (a *walStream) staged() []stagedSeg {
	seg := uint64(a.segSizeOr())
	var out []stagedSeg
	for _, sub := range []string{holdDir, ""} {
		entries, _ := os.ReadDir(filepath.Join(a.h.Dir, sub))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			rel := n
			if sub != "" {
				rel = sub + "/" + n
			}
			if t, ok := HistoryTLI(n); ok {
				out = append(out, stagedSeg{name: rel, tli: t, history: true})
				continue
			}
			base, partial := strings.CutSuffix(n, partialExt)
			if t, no, ok := ParseSegName(base, seg); ok {
				out = append(out, stagedSeg{name: rel, tli: t, segno: no, partial: partial})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.history != y.history {
			return x.history
		}
		if x.segno != y.segno {
			return x.segno < y.segno
		}
		return x.tli < y.tli
	})
	return out
}

// live is the .partial pg_receivewal writes: the newest one at the top level.
func liveOf(files []stagedSeg) *stagedSeg {
	var live *stagedSeg
	for i := range files {
		f := &files[i]
		if f.partial && !strings.Contains(f.name, "/") && (live == nil || f.segno > live.segno || f.segno == live.segno && f.tli > live.tli) {
			live = f
		}
	}
	return live
}

// Closed lists the staged files ready for upload: complete segments (but
// the one kept for resuming), history files, and partial segments that
// will never complete (an older timeline, or WAL before a gap).
func (a *walStream) Closed() []string {
	files := a.staged()
	live := liveOf(files)
	a.mu.Lock()
	kept := a.kept
	a.mu.Unlock()
	var out []string
	for _, f := range files {
		switch {
		case f.name == kept:
		case live != nil && f.name == live.name:
		default:
			out = append(out, f.name)
		}
	}
	return out
}

// resumeLSN is where pg_receivewal will start from the staging folder (0:
// nothing staged, it starts at the slot).
func (a *walStream) resumeLSN() LSN {
	seg := uint64(a.segSizeOr())
	var best *stagedSeg
	for _, f := range a.staged() {
		if f.history || strings.Contains(f.name, "/") {
			continue
		}
		f := f
		if best == nil || f.segno > best.segno || f.segno == best.segno && f.partial && !best.partial {
			best = &f
		}
	}
	if best == nil {
		return 0
	}
	if best.partial {
		return LSN(best.segno * seg)
	}
	return LSN((best.segno + 1) * seg)
}

// hold moves every staged WAL file out of pg_receivewal's sight: they are
// still archived, but streaming starts again from the slot.
func (a *walStream) hold() error {
	if err := os.MkdirAll(filepath.Join(a.h.Dir, holdDir), 0o700); err != nil {
		return err
	}
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	for _, f := range a.staged() {
		if strings.Contains(f.name, "/") {
			continue
		}
		if f.name == a.kept {
			os.Remove(filepath.Join(a.h.Dir, f.name)) // archived already
			continue
		}
		if err := os.Rename(filepath.Join(a.h.Dir, f.name), filepath.Join(a.h.Dir, holdDir, f.name)); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.kept = ""
	a.mu.Unlock()
	return nil
}

// identifySystem runs IDENTIFY_SYSTEM on a replication connection: it also
// proves the account may replicate and pg_hba.conf lets it.
func identifySystem(ctx context.Context, s *store.Server) (sysID string, tli int, pos LSN, err error) {
	c, err := replicationConn(ctx, s)
	if err != nil {
		return "", 0, 0, fmt.Errorf("replication connection: %w (the account needs REPLICATION and a 'replication' line in pg_hba.conf)", err)
	}
	defer c.Close(context.Background())
	res, err := c.Exec(ctx, "IDENTIFY_SYSTEM").ReadAll()
	if err != nil {
		return "", 0, 0, err
	}
	if len(res) == 0 || len(res[0].Rows) == 0 || len(res[0].Rows[0]) < 3 {
		return "", 0, 0, errors.New("IDENTIFY_SYSTEM returned nothing")
	}
	row := res[0].Rows[0]
	tli, _ = strconv.Atoi(string(row[1]))
	pos, err = ParseLSN(string(row[2]))
	return string(row[0]), tli, pos, err
}

type slotState struct {
	exists, active bool
	pid            int64
	restart        LSN
	status         string // wal_status (13+): reserved, extended, unreserved, lost
}

func (a *walStream) slotState(ctx context.Context, c *pgx.Conn, major int) (slotState, error) {
	var st slotState
	q := `SELECT active, coalesce(active_pid, 0), coalesce(restart_lsn::text, ''), ''::text FROM pg_replication_slots WHERE slot_name = $1`
	if major >= 13 {
		q = `SELECT active, coalesce(active_pid, 0), coalesce(restart_lsn::text, ''), coalesce(wal_status, '') FROM pg_replication_slots WHERE slot_name = $1`
	}
	var restart string
	err := c.QueryRow(ctx, q, a.slot).Scan(&st.active, &st.pid, &restart, &st.status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return st, nil
		}
		return st, err
	}
	st.exists = true
	if restart != "" {
		st.restart, _ = ParseLSN(restart)
	}
	return st, nil
}

// Session runs one pg_receivewal until it exits.
func (a *walStream) Session(ctx context.Context, job *store.Job, server *store.Server, rotate <-chan struct{}) error {
	h := a.h
	c, err := Connect(ctx, server, "")
	if err != nil {
		return fmt.Errorf("cannot connect to %s: %w", server.Name, err)
	}
	defer c.Close(context.Background())
	var version, walLevel string
	var num int
	var segSize int64
	var standby, repl bool
	if err := c.QueryRow(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::int, current_setting('wal_level'),
		pg_size_bytes(current_setting('wal_segment_size')), pg_is_in_recovery(),
		coalesce((SELECT rolreplication OR rolsuper FROM pg_roles WHERE rolname = current_user), false)`).
		Scan(&version, &num, &walLevel, &segSize, &standby, &repl); err != nil {
		return err
	}
	major := num / 10000
	switch {
	case major < 10:
		return fmt.Errorf("PostgreSQL %s is too old for WAL archiving (10 or newer)", version)
	case walLevel == "minimal":
		return errors.New("wal_level is minimal on the server: set it to replica (restart needed) to archive WAL")
	case !repl:
		return fmt.Errorf("the account %s needs the REPLICATION attribute (ALTER ROLE %s REPLICATION) to archive WAL", server.Username, QuoteIdent(server.Username))
	}
	a.mu.Lock()
	a.segSize = segSize
	a.mu.Unlock()
	sysID, tli, pos, err := identifySystem(ctx, server)
	if err != nil {
		return err
	}
	tool, err := h.Tools.PGFor("pg_receivewal", major)
	if err != nil {
		return err
	}

	// the slot keeps the WAL on the server until it is received
	st, err := a.slotState(ctx, c, major)
	if err != nil {
		return fmt.Errorf("reading replication slot %s: %w", a.slot, err)
	}
	if st.exists && st.status == "lost" {
		h.Log.Printf("WARNING: the server removed WAL that replication slot %s still needed (max_slot_wal_keep_size) — the archive has a gap; PITR cannot cross it, the next base backup starts a new chain", a.slot)
		if _, err := c.Exec(ctx, "SELECT pg_drop_replication_slot($1)", a.slot); err != nil {
			return fmt.Errorf("dropping the lost slot %s: %w", a.slot, err)
		}
		st = slotState{}
	}
	if st.exists && st.active && st.pid > 0 {
		h.Log.Printf("Slot %s is still in use by an earlier stream of this job (process %d) — ending it", a.slot, st.pid)
		_, _ = c.Exec(ctx, "SELECT pg_terminate_backend($1)", st.pid)
		time.Sleep(time.Second)
	}
	if !st.exists {
		if _, err := c.Exec(ctx, "SELECT pg_create_physical_replication_slot($1, true)", a.slot); err != nil {
			return fmt.Errorf("creating replication slot %s: %w", a.slot, err)
		}
		if st, err = a.slotState(ctx, c, major); err != nil {
			return err
		}
		h.Log.Printf("Created replication slot %s at %s: the server keeps its WAL until DumpCat has received it (drop the slot by hand if DumpCat is removed)", a.slot, st.restart)
	}
	resume := a.resumeLSN()
	switch {
	case resume > 0 && st.restart > resume:
		h.Log.Printf("WARNING: the server no longer has the WAL from %s (the slot starts at %s) — the archive has a gap; PITR cannot cross it, the next base backup starts a new chain", resume, st.restart)
		if err := a.hold(); err != nil {
			return err
		}
	case resume == 0 && major < 15 && pos > st.restart+LSN(segSize):
		h.Log.Printf("WARNING: nothing staged and PostgreSQL %d cannot resume from the slot: streaming starts at the server's position %s, WAL since %s is not archived", major, pos, st.restart)
	}

	pwEnv, err := WritePassFile(filepath.Join(h.Dir, ".conn"), server)
	if err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Join(h.Dir, ".conn"))
	conn, err := ConnInfo(server, "postgres", filepath.Join(h.Dir, ".conn"))
	if err != nil {
		return err
	}
	args := []string{"--directory=" + h.Dir, "--slot=" + a.slot, "--no-loop", "--no-password", "--verbose", "--status-interval=10", "--dbname=" + conn}
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	cmd := execx.Command(sctx, tool.Path, args...)
	cmd.Env = append(append(os.Environ(), pwEnv...), cEnv...)
	lw := h.Log.Writer("[pg_receivewal] ")
	defer lw.Close()
	errTail := &tailLines{}
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
	a.lastGrow, a.lastEnd = now, 0
	a.mu.Unlock()
	h.Update(func(s *engine.LogStatus) {
		s.State, s.Since, s.NextTry, s.Slot = "streaming", &now, nil, a.slot
	})
	from := "the slot"
	if r := a.resumeLSN(); r > 0 {
		from = r.String()
	}
	role := "primary"
	if standby {
		role = "standby"
	}
	h.Log.Printf("Streaming WAL from %s (PostgreSQL %s, %s, system %s, timeline %d) through slot %s, from %s", server.Name, version, role, sysID, tli, a.slot, from)

	t := time.NewTicker(walTick)
	defer t.Stop()
	for {
		select {
		case err := <-exited:
			if msg := errTail.String(); msg != "" {
				return fmt.Errorf("pg_receivewal stopped (%v): %s", err, msg)
			}
			return fmt.Errorf("pg_receivewal stopped: %v", err)
		case <-ctx.Done():
			scancel()
			<-exited
			return ctx.Err()
		case <-rotate:
			a.switchWAL(ctx, c, standby, "on request")
		case <-t.C:
			if err := a.monitor(ctx, c, job.ID, standby); err != nil {
				scancel()
				<-exited
				return err
			}
		}
	}
}

func (a *walStream) switchWAL(ctx context.Context, c *pgx.Conn, standby bool, why string) {
	if standby {
		a.h.Log.Printf("Cannot close the current WAL segment on a standby (%s): it is closed when the primary switches", why)
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.Exec(qctx, "SELECT pg_switch_wal()"); err != nil {
		a.mu.Lock()
		a.noSwitch = true
		a.mu.Unlock()
		a.h.Log.Printf("Cannot close the current WAL segment (pg_switch_wal() needs a superuser or GRANT EXECUTE ON FUNCTION pg_switch_wal() TO the account): %v", err)
		return
	}
	now := time.Now()
	a.h.Update(func(st *engine.LogStatus) { st.RotatedAt = &now })
	a.h.Log.Printf("Asked the server to close the current WAL segment (%s)", why)
}

// monitor follows the segment being received, computes the lag, closes
// the segment when asked to, and detects stalls.
func (a *walStream) monitor(ctx context.Context, c *pgx.Conn, jobID int64, standby bool) error {
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var srv, restart string
	var retained int64
	q := `SELECT (CASE WHEN pg_is_in_recovery() THEN pg_last_wal_receive_lsn() ELSE pg_current_wal_flush_lsn() END)::text,
		coalesce((SELECT restart_lsn::text FROM pg_replication_slots WHERE slot_name = $1), ''),
		coalesce((SELECT pg_wal_lsn_diff(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_receive_lsn() ELSE pg_current_wal_flush_lsn() END, restart_lsn)::bigint
			FROM pg_replication_slots WHERE slot_name = $1), 0)`
	serr := c.QueryRow(qctx, q, a.slot).Scan(&srv, &restart, &retained)
	srvLSN, _ := ParseLSN(srv)

	files := a.staged()
	live := liveOf(files)
	a.mu.Lock()
	changed := false
	if live != nil {
		name := strings.TrimSuffix(live.name, partialExt)
		if name != a.current || a.tail == nil {
			a.current, changed = name, true
			a.tail = &walTail{Path: filepath.Join(a.h.Dir, live.name)}
		}
	}
	tl := a.tail
	a.mu.Unlock()
	if changed {
		a.h.Kick()
	}
	var inf SegInfo
	if tl != nil {
		if err := tl.Update(); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.h.Log.Printf("Cannot read %s: %v", filepath.Base(tl.Path), err)
		}
		inf = tl.Info
	}
	end := inf.End
	now := time.Now()
	var lag int64
	if serr == nil && end > 0 && srvLSN > end {
		lag = int64(srvLSN - end)
		if lag <= pageHeaderSz {
			lag = 0 // a page header the next record starts after
		}
	}
	a.mu.Lock()
	a.cur = inf
	if end != a.lastEnd {
		a.lastEnd, a.lastGrow = end, now
	}
	stalled := serr == nil && lag > 0 && now.Sub(a.lastGrow) > walStall
	noSwitch := a.noSwitch
	cur := a.current
	a.mu.Unlock()
	recent := false
	a.h.Update(func(st *engine.LogStatus) {
		st.Current = cur
		if end > 0 {
			st.Received = int64(end - inf.Start)
		}
		if !inf.Last.IsZero() {
			t := inf.Last
			st.ReceivedAt = &t
		}
		if serr == nil {
			st.ServerPosition, st.CheckedAt, st.LagBytes, st.SlotRetained = srvLSN.String(), &now, lag, retained
			st.LagSeconds = 0
			if lag == 0 && end > 0 {
				st.CaughtUpAt = &now
			}
			if lag > 0 && st.ReceivedAt != nil {
				st.LagSeconds = int64(now.Sub(*st.ReceivedAt).Seconds())
			}
		}
		recent = st.RotatedAt != nil && now.Sub(*st.RotatedAt) < time.Minute
	})
	a.mu.Lock()
	report := serr != nil && serr.Error() != a.lastErr
	a.lastErr = ""
	if serr != nil {
		a.lastErr = serr.Error()
	}
	a.mu.Unlock()
	if report {
		a.h.Log.Printf("Cannot read the server's WAL position: %v", serr)
	}
	if stalled {
		return engine.ErrStalled
	}
	// bound the data loss window: close the segment once it holds commits
	// for longer than the configured interval
	if job, err := a.h.Store.GetJob(jobID); err == nil && job.Options.LogRotateMinutes > 0 && !noSwitch && !standby && serr == nil && inf.Timed > 0 && !recent {
		every := time.Duration(job.Options.LogRotateMinutes) * time.Minute
		if now.Sub(inf.First) >= every {
			a.switchWAL(ctx, c, standby, fmt.Sprintf("every %d minutes", job.Options.LogRotateMinutes))
		}
	}
	return nil
}

// tailLines keeps the end of a process's error output.
type tailLines struct {
	mu sync.Mutex
	b  []byte
}

func (l *tailLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.b = append(l.b, p...)
	if len(l.b) > 1024 {
		l.b = l.b[len(l.b)-1024:]
	}
	l.mu.Unlock()
	return len(p), nil
}

func (l *tailLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(strings.Fields(string(l.b)), " ")
}

// ---- archiving ----

// Seal reads a closed WAL file before its upload.
func (a *walStream) Seal(ctx context.Context, job *store.Job, name string) (*engine.LogFileInfo, error) {
	p := filepath.Join(a.h.Dir, filepath.FromSlash(name))
	base := path.Base(name)
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if t, ok := HistoryTLI(base); ok {
		return &engine.LogFileInfo{Name: base, Size: st.Size(), Meta: map[string]any{"history": true, "tli": t, "pinned": true}}, nil
	}
	seg := base
	partial := false
	if s, ok := strings.CutSuffix(base, partialExt); ok {
		seg, partial = s, true
	}
	inf, err := ScanSegment(p)
	if err != nil {
		return nil, err
	}
	if inf.SysID == 0 || inf.Records == 0 {
		if partial {
			return &engine.LogFileInfo{Skip: true}, nil // nothing was received in it
		}
		return nil, errors.New("not a WAL segment, or no valid record in it")
	}
	tli, segno, ok := ParseSegName(seg, inf.SegSize)
	if !ok || LSN(segno*inf.SegSize) != inf.Start {
		return nil, fmt.Errorf("the file does not hold segment %s (it starts at %s)", seg, inf.Start)
	}
	if !partial && st.Size() != int64(inf.SegSize) {
		return nil, fmt.Errorf("incomplete segment (%d of %d bytes)", st.Size(), inf.SegSize)
	}
	meta := map[string]any{"tli": tli, "segno": segno, "system_id": strconv.FormatUint(inf.SysID, 10), "seg_size": inf.SegSize,
		"start": inf.Start.String(), "end": inf.End.String()}
	if partial {
		meta["partial"] = true
	}
	if inf.Switch {
		meta["switch"] = true
	}
	if len(inf.Points) > 0 {
		meta["points"] = inf.Points
	}
	return &engine.LogFileInfo{Name: base, Seq: int64(segno), First: inf.First, Last: inf.Last, Size: st.Size(), Meta: meta}, nil
}

// Archived drops the staged copy, but keeps the newest complete segment:
// pg_receivewal resumes after the newest segment it finds.
func (a *walStream) Archived(name string) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	if strings.Contains(name, "/") || !IsSegName(name) {
		os.Remove(filepath.Join(a.h.Dir, filepath.FromSlash(name)))
		return
	}
	a.mu.Lock()
	old := a.kept
	seg := uint64(a.segSize)
	if seg == 0 {
		seg = DefaultWALSegSize
	}
	keep := true
	if old != "" {
		_, on, _ := ParseSegName(old, seg)
		_, nn, _ := ParseSegName(name, seg)
		keep = nn >= on
	}
	if keep {
		a.kept = name
	}
	a.mu.Unlock()
	switch {
	case keep && old != "" && old != name:
		os.Remove(filepath.Join(a.h.Dir, old))
	case !keep:
		os.Remove(filepath.Join(a.h.Dir, name))
	}
}

// Current describes the segment being received.
func (a *walStream) Current() engine.LogCurrent {
	st := a.h.Status()
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := engine.LogCurrent{Name: a.current, Status: st}
	if a.current == "" || a.cur.Records == 0 {
		return cur
	}
	cur.Size, cur.Events, cur.First, cur.Last = int64(a.cur.End-a.cur.Start), true, a.cur.First, a.cur.Last
	cur.Meta = segMeta(a.current, &a.cur)
	return cur
}

func segMeta(name string, inf *SegInfo) map[string]any {
	tli, segno, _ := ParseSegName(name, inf.SegSize)
	m := map[string]any{"tli": tli, "segno": segno, "system_id": strconv.FormatUint(inf.SysID, 10), "seg_size": inf.SegSize,
		"start": inf.Start.String(), "end": inf.End.String(), "partial": true}
	if len(inf.Points) > 0 {
		m["points"] = inf.Points
	}
	return m
}

// Snapshot copies the segment being received for a restore, under its
// segment name: recovery reads up to its last valid record.
func (a *walStream) Snapshot(dst string) (*engine.LogSnapshot, error) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	st := a.h.Status()
	a.mu.Lock()
	name := a.current
	a.mu.Unlock()
	if name == "" {
		return nil, errors.New("no WAL segment is being received yet")
	}
	if err := copyFile(filepath.Join(a.h.Dir, name+partialExt), dst); err != nil {
		return nil, err
	}
	inf, err := ScanSegment(dst)
	if err != nil {
		return nil, err
	}
	snap := &engine.LogSnapshot{Name: name, Size: int64(inf.End - inf.Start), Last: inf.Last, Meta: segMeta(name, inf)}
	if st.Level(name, time.Minute) {
		snap.CaughtUp, snap.CheckedAt = true, *st.CheckedAt
	}
	return snap, nil
}

// Release drops the job's replication slot: without it the server would
// keep its WAL for ever.
func (a *walStream) Release(ctx context.Context, server *store.Server, lg *tasklog.Logger) {
	c, err := Connect(ctx, server, "")
	if err != nil {
		lg.Printf("WARNING: could not connect to drop replication slot %s (%v) — drop it on %s: SELECT pg_drop_replication_slot('%s');", a.slot, err, server.Name, a.slot)
		return
	}
	defer c.Close(context.Background())
	for i := 0; ; i++ {
		var active bool
		var pid int64
		err := c.QueryRow(ctx, "SELECT active, coalesce(active_pid, 0) FROM pg_replication_slots WHERE slot_name = $1", a.slot).Scan(&active, &pid)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err == nil && active && pid > 0 && i < 10 {
			if i == 3 {
				_, _ = c.Exec(ctx, "SELECT pg_terminate_backend($1)", pid)
			}
			time.Sleep(time.Second)
			continue
		}
		if _, err = c.Exec(ctx, "SELECT pg_drop_replication_slot($1)", a.slot); err != nil {
			lg.Printf("WARNING: could not drop replication slot %s (%v) — drop it on %s: SELECT pg_drop_replication_slot('%s');", a.slot, err, server.Name, a.slot)
			return
		}
		lg.Printf("WAL archiving is off: dropped replication slot %s, the server no longer keeps WAL for DumpCat", a.slot)
		return
	}
}

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
