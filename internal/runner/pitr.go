package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"dumpcat/internal/binlog"
	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// Point-in-time recovery: restore a base backup, then replay the archived
// binary logs from the position recorded with that backup up to the target.

// PITRTarget is where a point-in-time restore stops.
type PITRTarget struct {
	Type string     `json:"type"` // time, position, gtid, latest
	Time *time.Time `json:"time,omitempty"`
	File string     `json:"file,omitempty"`
	Pos  int64      `json:"pos,omitempty"`
	GTID string     `json:"gtid,omitempty"`
}

func (t *PITRTarget) Normalize() error {
	switch t.Type {
	case "time":
		if t.Time == nil || t.Time.IsZero() {
			return errors.New("no recovery time selected")
		}
		if t.Time.After(time.Now().Add(time.Minute)) {
			return errors.New("the recovery time is in the future")
		}
		t.File, t.Pos, t.GTID = "", 0, ""
	case "position":
		if err := engine.CheckBinlogName(t.File); err != nil || !binlog.IsName(t.File) {
			return fmt.Errorf("invalid binlog file name: %q", t.File)
		}
		if t.Pos < 4 {
			return errors.New("the binlog position must be ≥ 4")
		}
		t.Time, t.GTID = nil, ""
	case "gtid":
		g, err := binlog.NormalizeGTID(t.GTID)
		if err != nil {
			return err
		}
		t.GTID, t.Time, t.File, t.Pos = g, nil, "", 0
	case "latest":
		t.Time, t.File, t.Pos, t.GTID = nil, "", 0, ""
	default:
		return fmt.Errorf("invalid recovery target type: %q", t.Type)
	}
	return nil
}

func (t PITRTarget) Describe(loc *time.Location) string {
	switch t.Type {
	case "time":
		return "time " + t.Time.In(loc).Format("2006-01-02 15:04:05")
	case "position":
		return fmt.Sprintf("position %s:%d", t.File, t.Pos)
	case "gtid":
		return "just before GTID " + t.GTID
	}
	return "the latest archived binlog"
}

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

func metaInt(m map[string]any, k string) (int64, bool) {
	switch v := m[k].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// runStarts returns where binlog replay starts for each database of a run;
// key "" holds the run-wide position (single snapshot dumps, physical).
func runStarts(run *store.Run) map[string]binlogPos {
	out := map[string]binlogPos{}
	if f, _ := run.Meta["binlog_file"].(string); f != "" {
		if p, ok := metaInt(run.Meta, "binlog_pos"); ok && p > 0 {
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

// consistentAt is an upper bound of the moment the backup's data reflects:
// recorded precisely by newer backups, otherwise the end of the run.
func consistentAt(run *store.Run) time.Time {
	if v, ok := metaInt(run.Meta, "consistent_at"); ok && v > 0 {
		return time.Unix(v, 0)
	}
	if run.FinishedAt != nil {
		return *run.FinishedAt
	}
	return run.StartedAt
}

// baseErr explains why run cannot be the starting point for target.
func baseErr(run *store.Run, t PITRTarget) error {
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
		if at := consistentAt(run); at.After(*t.Time) {
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

// selectPITRBase picks the newest usable backup whose data precedes the target.
func selectPITRBase(runs []store.Run, t PITRTarget, usable func(*store.Run) bool) (*store.Run, error) {
	var best *store.Run
	var bestAt time.Time
	var lastErr error
	for i := range runs {
		r := &runs[i]
		if r.Status != store.StatusSuccess && r.Status != store.StatusPartial {
			continue
		}
		if usable != nil && !usable(r) {
			continue
		}
		if err := baseErr(r, t); err != nil {
			lastErr = err
			continue
		}
		if at := consistentAt(r); best == nil || at.After(bestAt) {
			best, bestAt = r, at
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

var errNotArchived = errors.New("has not been archived")

// binlogChain follows the archived files from the one holding start through
// consecutive numbers. A RESET MASTER (or a physical restore) restarts the
// numbering, so one name can exist several times: event times cannot tell
// them apart (replayed statements keep their original timestamps), the
// archive order can. The file holding a backup's position was still open
// when the backup started, so it was archived after startedAt.
func binlogChain(files []store.BinlogFile, start binlogPos, startedAt time.Time) ([]store.BinlogFile, error) {
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
		return nil, fmt.Errorf("binlog %s (where the backup starts) %w", start.File, errNotArchived)
	}
	chain := []store.BinlogFile{files[idx]}
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

func binlogStored(f *store.BinlogFile) bool {
	for _, l := range f.Locations {
		if l.Status == store.LocOK && l.StorageID != nil {
			return true
		}
	}
	return false
}

type pitrPlan struct {
	Target   PITRTarget
	Base     *store.Run
	Starts   map[string]binlogPos
	From     binlogPos          // lowest start position
	Files    []store.BinlogFile // archived files to fetch, in replay order
	Partial  string             // file still being streamed, appended after Files
	Coverage time.Time          // binlog events are complete up to here
	Bytes    int64
	Warnings []string
}

func (p *pitrPlan) fileIDs() []int64 {
	ids := make([]int64, len(p.Files))
	for i, f := range p.Files {
		ids[i] = f.ID
	}
	return ids
}

// planPITR checks that base plus archived binlogs reach the target.
func (r *Runner) planPITR(base *store.Run, t PITRTarget) (*pitrPlan, error) {
	if err := baseErr(base, t); err != nil {
		return nil, err
	}
	starts := runStarts(base)
	lo, _, _ := startRange(starts)
	all, err := r.Store.ListBinlogFiles(base.JobUUID)
	if err != nil {
		return nil, err
	}
	var files []store.BinlogFile
	for i := range all {
		if binlogStored(&all[i]) {
			files = append(files, all[i])
		}
	}
	p := &pitrPlan{Target: t, Base: base, Starts: starts, From: lo}
	chain, cerr := binlogChain(files, lo, base.StartedAt)

	// the file being streamed right now may continue (or start) the chain
	var cur string
	var curInf binlog.Info
	var ast ArchiverStatus
	if base.JobID != nil {
		if a := r.archiver(*base.JobID); a != nil {
			cur, curInf, ast = a.currentInfo()
		}
	}
	partial := false
	if cur != "" && curInf.Events > 0 {
		if len(chain) == 0 {
			partial = cur == lo.File
		} else {
			last := chain[len(chain)-1]
			lb, ls, _ := binlog.SplitName(last.FileName)
			cb, cs, _ := binlog.SplitName(cur)
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
		cov := curInf.LastTS
		if ast.State == "streaming" && ast.LagBytes == 0 && ast.Current == cur && ast.CheckedAt != nil && ast.CheckedAt.After(cov) {
			cov = *ast.CheckedAt
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
		have[cur] = true
	}
	for db, s := range starts {
		if !have[s.File] {
			if db == "" {
				return nil, fmt.Errorf("binlog %s %w", s.File, errNotArchived)
			}
			return nil, fmt.Errorf("binlog %s (where database %s starts) %w", s.File, db, errNotArchived)
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
			return nil, fmt.Errorf("binlogs are only archived up to %s — pick an earlier time, click 'Close binlog now' and wait for the upload, or choose 'Latest'",
				p.Coverage.In(r.location()).Format("2006-01-02 15:04:05"))
		}
		// commit times only lag file creation by a moment: files created
		// well after the target cannot hold anything older than it
		limit := t.Time.Add(5 * time.Minute)
		for _, f := range chain {
			if f.FirstTS.After(limit) {
				break
			}
			p.Files = append(p.Files, f)
		}
		if partial && len(p.Files) == len(chain) && !curInf.FirstTS.After(limit) {
			p.Partial = cur
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
		if !found && partial && cur == t.File {
			if t.Pos > curInf.Size {
				return nil, fmt.Errorf("position %d has not been received yet (%s has %d bytes)", t.Pos, cur, curInf.Size)
			}
			p.Partial, found = cur, true
		}
		if !found {
			return nil, fmt.Errorf("binlog %s is not in the archived sequence after the backup", t.File)
		}
	default: // gtid, latest
		p.Files = chain
		if partial {
			p.Partial = cur
		}
	}
	for _, f := range p.Files {
		p.Bytes += f.SizeBytes
	}
	if p.Partial != "" {
		p.Bytes += curInf.Size
	}
	return p, nil
}

// ---- API view ----

type PlanRun struct {
	ID           int64     `json:"id"`
	DirName      string    `json:"dir_name"`
	Method       string    `json:"method"`
	Type         string    `json:"type"`
	StartedAt    time.Time `json:"started_at"`
	ConsistentAt time.Time `json:"consistent_at"`
	BinlogFile   string    `json:"binlog_file"`
	BinlogPos    int64     `json:"binlog_pos"`
}

type PITRPlanView struct {
	OK        bool       `json:"ok"`
	Error     string     `json:"error,omitempty"`
	Target    string     `json:"target"`
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
// archived binlogs reach it. runID (optional) is checked as a base too.
func (r *Runner) PlanPITR(jobID, runID int64, t PITRTarget) (*PITRPlanView, error) {
	if err := t.Normalize(); err != nil {
		return nil, err
	}
	loc := r.location()
	v := &PITRPlanView{Target: t.Describe(loc), Warnings: []string{}}
	var jobUUID string
	var asked *store.Run
	if runID > 0 {
		run, err := r.Store.GetRun(runID)
		if err != nil {
			return nil, err
		}
		jobUUID, asked = run.JobUUID, run
	} else {
		job, err := r.Store.GetJob(jobID)
		if err != nil {
			return nil, err
		}
		jobUUID = job.UUID
	}
	runs, err := r.Store.RunsByJobUUID(jobUUID)
	if err != nil {
		return nil, err
	}
	base, err := selectPITRBase(runs, t, r.pitrUsable)
	if err != nil {
		v.Error = err.Error()
		if asked != nil {
			v.RunError = err.Error()
		}
		return v, nil
	}
	if asked != nil {
		if asked.ID == base.ID {
			v.RunOK = true
		} else if err := baseErr(asked, t); err != nil {
			v.RunError = err.Error()
		} else if _, err := r.planPITR(asked, t); err != nil {
			v.RunError = err.Error()
		} else {
			v.RunOK = true
		}
	}
	lo, _, _ := startRange(runStarts(base))
	v.Base = &PlanRun{ID: base.ID, DirName: base.DirName, Method: base.Method, Type: base.Type, StartedAt: base.StartedAt,
		ConsistentAt: consistentAt(base), BinlogFile: lo.File, BinlogPos: lo.Pos}
	p, err := r.planPITR(base, t)
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
		v.Warnings = append(v.Warnings, fmt.Sprintf("Also uses the binlog still being written, %s (not uploaded yet), from the DumpCat staging folder", p.Partial))
	}
	return v, nil
}

// ---- restore: download, verify, resolve the stop point ----

type pitrState struct {
	plan  *pitrPlan
	Files []string // local raw files in replay order
	Names []string
	Stop  *binlogPos // nil = replay everything
}

var errReplan = errors.New("the binlog was closed while preparing")

// pitrPrepare fetches every binlog needed into the work dir and resolves the
// stop position — before anything destructive happens to the target.
func (r *Runner) pitrPrepare(ctx context.Context, task *Task, lg *tasklog.Logger, plan *pitrPlan, workDir, fallbackPass string) (*pitrState, error) {
	t := plan.Target
	loc := r.location()
	dir := filepath.Join(workDir, "binlog")
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lg.Printf("PITR: recover to %s — base backup #%d %s, replaying binlogs from %s (%d archived file(s)%s, %s)", t.Describe(loc), plan.Base.ID, plan.Base.DirName,
		plan.From, len(plan.Files), map[bool]string{true: " + file being written " + plan.Partial, false: ""}[plan.Partial != ""], humanBytes(plan.Bytes))
	if total, free, err := diskUsage(dir); err == nil {
		lg.Printf("Binlog work folder %s: %s free of %s", dir, humanBytes(free), humanBytes(total))
		if free < plan.Bytes {
			return nil, fmt.Errorf("not enough free space for the binlogs (%s needed)", humanBytes(plan.Bytes))
		}
	}
	task.SetPhase("binlog_download")
	task.SetEstimate(plan.Bytes)
	stopper := &binlog.Stopper{GTID: t.GTID}
	if t.Type == "time" {
		stopper.Time = *t.Time
	}
	st := &pitrState{plan: plan}
	resolve := t.Type == "time" || t.Type == "gtid"
	check := func(path, name string, from int64) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		// every replay start in this file must be an event boundary: if not,
		// this file is not the one the backup saw (e.g. numbering restarted)
		starts := map[int64]bool{}
		for _, s := range plan.Starts {
			if s.File == name {
				starts[s.Pos] = false
			}
		}
		stopper.StartFile(from)
		end, err := binlog.Scan(f, 0, func(e *binlog.Event) error {
			if _, ok := starts[e.Offset]; ok {
				starts[e.Offset] = true
			}
			if !resolve {
				return nil
			}
			if off, ok := stopper.Feed(e); ok {
				st.Stop = &binlogPos{name, off}
				return io.EOF
			}
			return nil
		})
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("binlog %s is truncated", name)
		}
		if err != nil {
			return fmt.Errorf("binlog %s: %w", name, err)
		}
		for pos, seen := range starts {
			if !seen && pos != end && (st.Stop == nil || st.Stop.File != name || pos < st.Stop.Pos) {
				return fmt.Errorf("binlog %s does not match the backup: position %d is not an event boundary (was the binlog numbering reset?)", name, pos)
			}
		}
		if resolve && st.Stop == nil {
			if off, ok := stopper.EndFile(); ok {
				st.Stop = &binlogPos{name, off}
			}
		}
		return nil
	}
	for i := range plan.Files {
		f := &plan.Files[i]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		local := filepath.Join(dir, f.FileName)
		lg.Printf("[%d/%d] Fetching %s (%s, %s → %s)", i+1, len(plan.Files), f.FileName, humanBytes(f.SizeBytes),
			f.FirstTS.In(loc).Format("01-02 15:04:05"), f.LastTS.In(loc).Format("01-02 15:04:05"))
		if err := r.fetchBinlog(ctx, task, f, local, fallbackPass); err != nil {
			return nil, fmt.Errorf("%s: %w", f.FileName, err)
		}
		st.Files, st.Names = append(st.Files, local), append(st.Names, f.FileName)
		from := int64(0)
		if i == 0 {
			from = plan.From.Pos
		}
		if err := check(local, f.FileName, from); err != nil {
			return nil, err
		}
		if st.Stop != nil {
			break
		}
	}
	if plan.Partial != "" && st.Stop == nil {
		a := r.archiver(*plan.Base.JobID)
		if a == nil {
			return nil, errors.New("binlog archiving has stopped — the binlog being written is not available")
		}
		local := filepath.Join(dir, plan.Partial)
		name, inf, caught, checked, err := a.snapshotCurrent(local)
		if err != nil {
			return nil, err
		}
		if name != plan.Partial {
			return nil, errReplan
		}
		cov := inf.LastTS
		if caught && checked.After(cov) {
			cov = checked
		}
		if t.Type == "time" && t.Time.After(cov) {
			return nil, fmt.Errorf("binlogs only reach %s", cov.In(loc).Format("2006-01-02 15:04:05"))
		}
		lg.Printf("Including the binlog being written, %s (%s, up to %s)", name, humanBytes(inf.Size), inf.LastTS.In(loc).Format("2006-01-02 15:04:05"))
		st.Files, st.Names = append(st.Files, local), append(st.Names, name)
		from := int64(0)
		if len(plan.Files) == 0 {
			from = plan.From.Pos
		}
		if err := check(local, name, from); err != nil {
			return nil, err
		}
	}
	switch t.Type {
	case "position":
		st.Stop = &binlogPos{t.File, t.Pos}
	case "gtid":
		if st.Stop == nil {
			return nil, fmt.Errorf("GTID %s not found in the binlogs after the base backup", t.GTID)
		}
	}
	if len(st.Files) == 0 {
		return nil, errors.New("no binlog to replay")
	}
	if st.Stop != nil {
		for db, s := range plan.Starts {
			if c, _ := posCompare(*st.Stop, s); c < 0 {
				what := "the backup"
				if db != "" {
					what = "database " + db
				}
				return nil, fmt.Errorf("the stop point %s is before the position of %s (%s) — choose a later target", st.Stop, what, s)
			}
		}
		lg.Printf("PITR: stop point %s (events from this position on are not replayed)", st.Stop)
	} else {
		lg.Printf("PITR: replaying all %d binlog file(s)", len(st.Files))
	}
	return st, nil
}

// fetchBinlog downloads, decrypts and decompresses one archived file and
// checks both checksums.
func (r *Runner) fetchBinlog(ctx context.Context, task *Task, f *store.BinlogFile, local, fallbackPass string) error {
	var lastErr error = errors.New("no storage holds this file")
	// local storages first
	locs := append([]store.BinlogLocation{}, f.Locations...)
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
		if err := r.fetchBinlogFrom(ctx, task, b, f, local, fallbackPass); err != nil {
			lastErr = fmt.Errorf("%s: %w", st.Name, err)
			continue
		}
		return nil
	}
	return lastErr
}

func (r *Runner) fetchBinlogFrom(ctx context.Context, task *Task, b storage.Backend, f *store.BinlogFile, local, fallbackPass string) error {
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
		return errors.New("checksum mismatch of the decoded binlog")
	}
	return nil
}

// ---- restore: replay ----

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

// pitrSteps turns the prepared files into replay steps. Logical restores
// replay each restored database from its own snapshot position (filtered and
// renamed like the restore); physical restores replay everything.
func pitrSteps(st *pitrState, run *store.Run, req RestoreRequest) []engine.BinlogStep {
	idx := map[string]int{}
	for i, n := range st.Names {
		idx[n] = i
	}
	last := len(st.Files) - 1
	if st.Stop != nil {
		if i, ok := idx[st.Stop.File]; ok {
			last = i
		}
	}
	type stream struct {
		db, to string
		from   binlogPos
	}
	var streams []stream
	if run.Method == store.MethodPhysical {
		streams = append(streams, stream{from: st.plan.From})
	} else {
		for _, d := range selectDatabases(run, req) {
			if p, ok := startFor(st.plan.Starts, d.Name); ok {
				streams = append(streams, stream{db: d.Name, to: targetName(req, d.Name), from: p})
			}
		}
	}
	var steps []engine.BinlogStep
	for i := 0; i <= last; i++ {
		for _, s := range streams {
			first, ok := idx[s.from.File]
			if !ok || i < first {
				continue
			}
			step := engine.BinlogStep{File: st.Files[i], Database: s.db}
			if s.to != s.db {
				step.TargetDB = s.to
			}
			if i == first {
				step.StartPos = s.from.Pos
			}
			if st.Stop != nil && i == last {
				step.StopPos = st.Stop.Pos
				if step.StartPos >= step.StopPos {
					continue
				}
			}
			steps = append(steps, step)
		}
	}
	return steps
}

func (r *Runner) pitrApply(ctx context.Context, task *Task, env *engine.Env, st *pitrState, run *store.Run, target *store.Server, req RestoreRequest) error {
	lg := env.Log
	if run.Method == store.MethodPhysical {
		if err := waitForServer(ctx, target, lg); err != nil {
			return err
		}
	}
	task.SetPhase("binlog_apply")
	srcFlavor, _ := run.Meta["flavor"].(string)
	if srcFlavor == "" {
		srcFlavor = "mysql"
	}
	clientFlavor, err := r.targetFlavor(ctx, target)
	if err != nil {
		return err
	}
	steps := pitrSteps(st, run, req)
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
	task.SetEstimate(task.Raw.N() + total)
	if req.FastMode {
		defer r.enableFastMode(ctx, target, lg)()
	}
	if run.Method == store.MethodPhysical {
		lg.Printf("PITR: replaying %d binlog file(s) into %s (whole instance)", len(steps), target.Name)
	} else {
		lg.Printf("PITR: replaying binlogs for %d database(s) (%d steps) into %s — each database from its own snapshot position", len(selectDatabases(run, req)), len(steps), target.Name)
	}
	t0 := time.Now()
	err = engine.ApplyBinlogs(env, target, steps, engine.BinlogApplyOpts{
		BinlogTool:   engine.BinlogToolName(srcFlavor),
		ClientFlavor: clientFlavor,
		NoBinlog:     req.NoBinlog,
		// a logical restore onto a server that already executed the original
		// transactions (usually the source itself) needs new GTIDs
		SkipGTIDs: run.Method != store.MethodPhysical,
		OnStep:    func(i int) { task.Raw.Add(sizes[steps[i].File]) },
	})
	if err != nil {
		return fmt.Errorf("replaying binlogs: %w", err)
	}
	stop := "end of the binlogs"
	if st.Stop != nil {
		stop = st.Stop.String()
	}
	lg.Printf("PITR: binlogs replayed up to %s (%s) in %s", st.plan.Target.Describe(r.location()), stop, humanDuration(time.Since(t0)))
	return nil
}
