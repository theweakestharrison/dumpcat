package pgsql

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

// Point-in-time recovery of PostgreSQL: a pg_basebackup backup is the base,
// the archived WAL from the segment where it ends is replayed by the server
// itself (see recovery.go).

// walBase is where a recovery starts: the WAL range of the base backup.
type walBase struct {
	sysID      string
	tli        uint32
	start, end LSN
	segSize    uint64
}

func (b *walBase) segno(l LSN) uint64 { return uint64(l) / b.segSize }

func baseOf(run *store.Run) (*walBase, error) {
	if run.Method != store.MethodPgBase {
		return nil, errors.New("WAL can only be replayed on a physical backup (pg_basebackup)")
	}
	sys, _ := run.Meta["system_id"].(string)
	s, _ := run.Meta["start_lsn"].(string)
	e, _ := run.Meta["end_lsn"].(string)
	start, err1 := ParseLSN(s)
	end, err2 := ParseLSN(e)
	tli, _ := engine.MetaInt(run.Meta, "timeline")
	seg, _ := engine.MetaInt(run.Meta, "wal_segment_size")
	if sys == "" || err1 != nil || err2 != nil || tli <= 0 || seg <= 0 {
		return nil, errors.New("the backup does not record its WAL position")
	}
	return &walBase{sysID: sys, tli: uint32(tli), start: start, end: end, segSize: uint64(seg)}, nil
}

func (Driver) Base(run *store.Run, t *engine.PITRTarget) (*engine.LogBase, error) {
	b, err := baseOf(run)
	if err != nil {
		return nil, err
	}
	at := engine.ConsistentAt(run)
	switch t.Type {
	case "time":
		if at.After(*t.Time) {
			return nil, fmt.Errorf("the backup is only consistent at %s — after the recovery target", at.Format(time.RFC3339))
		}
	case "lsn":
		if l, _ := ParseLSN(t.LSN); l < b.end {
			return nil, fmt.Errorf("LSN %s is before the end of the backup (%s)", t.LSN, b.end)
		}
	}
	return &engine.LogBase{Run: run, From: fmt.Sprintf("%s (timeline %d)", b.end, b.tli), ConsistentAt: at, State: b}, nil
}

// walFile is an archived WAL file as the plan sees it.
type walFile struct {
	f       *store.LogFile
	tli     uint32
	segno   uint64
	partial bool
	history bool
	end     LSN
	points  []RestorePoint
}

func walFileOf(f *store.LogFile, b *walBase) (walFile, bool) {
	m := f.Meta
	if m["history"] == true {
		t, _ := engine.MetaInt(m, "tli")
		return walFile{f: f, tli: uint32(t), history: true}, uint32(t) > b.tli
	}
	if sys, _ := m["system_id"].(string); sys != b.sysID {
		return walFile{}, false
	}
	t, ok1 := engine.MetaInt(m, "tli")
	no, ok2 := engine.MetaInt(m, "segno")
	e, _ := m["end"].(string)
	end, err := ParseLSN(e)
	if !ok1 || !ok2 || err != nil || uint32(t) < b.tli {
		return walFile{}, false
	}
	w := walFile{f: f, tli: uint32(t), segno: uint64(no), partial: m["partial"] == true, end: end}
	w.points = metaPoints(m["points"])
	return w, true
}

// metaPoints reads the restore points of a file's meta (JSON or Go values).
func metaPoints(v any) []RestorePoint {
	switch p := v.(type) {
	case []RestorePoint:
		return p
	case []any:
		var out []RestorePoint
		for _, x := range p {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			rp := RestorePoint{}
			rp.Name, _ = m["name"].(string)
			rp.LSN, _ = m["lsn"].(string)
			if s, ok := m["time"].(string); ok {
				rp.Time, _ = time.Parse(time.RFC3339Nano, s)
			}
			out = append(out, rp)
		}
		return out
	}
	return nil
}

// walPlan is the PostgreSQL part of a plan.
type walPlan struct {
	base *walBase
}

// Plan follows the archived segments from the one holding the end of the
// base backup: per segment number every timeline at or after the base's
// (recovery takes the latest), the segment being received after them.
func (Driver) Plan(in *engine.PlanInput) (*engine.LogPlan, error) {
	b := in.Base.State.(*walBase)
	t, cur := in.Target, in.Current
	p := &engine.LogPlan{Target: t, Base: in.Base, State: &walPlan{base: b}}
	bySeg := map[uint64][]walFile{}
	var history []walFile
	var maxSeg uint64
	for i := range in.Files {
		w, ok := walFileOf(&in.Files[i], b)
		switch {
		case !ok:
		case w.history:
			history = append(history, w)
		case w.segno >= b.segno(b.end):
			bySeg[w.segno] = append(bySeg[w.segno], w)
			maxSeg = max(maxSeg, w.segno)
		}
	}
	first := b.segno(b.end)
	var chain [][]walFile // per segment number
	next := first
	for {
		ws := bySeg[next]
		if len(ws) == 0 {
			break
		}
		sort.Slice(ws, func(i, j int) bool { return ws[i].tli < ws[j].tli })
		chain = append(chain, ws)
		top := ws[len(ws)-1]
		if top.partial {
			break // nothing follows a segment that never completed
		}
		next++
	}
	// the segment being received may continue (or start) the chain
	partial := false
	if cur.Name != "" && cur.Events {
		tli, no, ok := ParseSegName(cur.Name, b.segSize)
		sys, _ := cur.Meta["system_id"].(string)
		partial = ok && no == next && tli >= b.tli && sys == b.sysID
	}
	if len(chain) == 0 && !partial {
		return nil, fmt.Errorf("WAL segment %s (where the backup ends) %w", SegName(b.tli, first, b.segSize), engine.ErrNotArchived)
	}
	if !partial && next <= maxSeg && len(chain) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("The WAL sequence is broken after %s — later archived segments cannot be used with this backup", chain[len(chain)-1][0].f.FileName))
	}
	// coverage: events are complete up to the last timed one, at least the
	// moment the backup became consistent
	p.Coverage = in.Base.ConsistentAt
	endLSN := b.end
	for _, ws := range chain {
		for _, w := range ws {
			if w.f.LastTS.After(p.Coverage) {
				p.Coverage = w.f.LastTS
			}
			endLSN = max(endLSN, w.end)
		}
	}
	if partial {
		cov := cur.Last
		if cur.Status.Level(cur.Name, 0) && cur.Status.CheckedAt.After(cov) {
			cov = *cur.Status.CheckedAt
		}
		if cov.After(p.Coverage) {
			p.Coverage = cov
		}
		if e, err := ParseLSN(fmt.Sprint(cur.Meta["end"])); err == nil {
			endLSN = max(endLSN, e)
		}
	}
	// how many segment numbers to fetch (cut), and whether the partial is needed
	cut := len(chain)
	usePartial := partial
	switch t.Type {
	case "time":
		if t.Time.After(p.Coverage) {
			return nil, engine.CoverageError(walLogInfo, p.Coverage, in.Loc)
		}
		limit := t.Time.Add(engine.TimeLimit)
		for i, ws := range chain {
			if f := ws[len(ws)-1].f.FirstTS; !f.IsZero() && f.After(limit) {
				// one more: a commit can start in the segment before and end in this one
				cut, usePartial = i+1, false
				break
			}
		}
	case "lsn":
		l, _ := ParseLSN(t.LSN)
		if l > endLSN {
			return nil, fmt.Errorf("LSN %s has not been archived yet (the WAL reaches %s)", t.LSN, endLSN)
		}
		if no := uint64(l) / b.segSize; no+1 < first+uint64(len(chain)) {
			cut, usePartial = int(no-first)+2, false
		}
	case "name":
		found := false
		for i, ws := range chain {
			for _, w := range ws {
				for _, rp := range w.points {
					if l, err := ParseLSN(rp.LSN); err == nil && rp.Name == t.Name && l >= b.end {
						found = true
					}
				}
			}
			if found {
				cut, usePartial = min(i+2, len(chain)), partial && i+2 > len(chain)
				break
			}
		}
		if !found && partial {
			for _, rp := range metaPoints(cur.Meta["points"]) {
				if l, err := ParseLSN(rp.LSN); err == nil && rp.Name == t.Name && l >= b.end {
					found = true
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("restore point %q is not in the WAL archived after the backup", t.Name)
		}
	}
	for _, h := range history {
		p.Files = append(p.Files, *h.f)
	}
	for _, ws := range chain[:cut] {
		for _, w := range ws {
			p.Files = append(p.Files, *w.f)
		}
	}
	if usePartial {
		p.Partial = cur.Name
	}
	for _, f := range p.Files {
		p.Bytes += f.SizeBytes
	}
	if p.Partial != "" {
		p.Bytes += int64(b.segSize)
	}
	return p, nil
}

// ---- checks before anything is changed ----

// WALReplay is what the physical restore needs to recover: the fetched
// files and the recovery settings.
type WALReplay struct {
	Files    []engine.LocalLog // segments (every timeline) and history files
	Settings [][2]string       // recovery_target_* (none: replay everything)
	SysID    string
}

type walCheck struct {
	env   *engine.Env
	plan  *engine.LogPlan
	files []engine.LocalLog
}

func (Driver) NewCheck(env *engine.Env, plan *engine.LogPlan) engine.LogCheck {
	return &walCheck{env: env, plan: plan}
}

func (c *walCheck) Add(f engine.LocalLog) (bool, error) {
	c.files = append(c.files, f)
	return false, nil
}

// Finish reads the fetched WAL in a row (the latest timeline of every
// segment), checks it belongs to the base's cluster and continues it, and
// resolves the recovery target.
func (c *walCheck) Finish() (*engine.Replay, error) {
	b := c.plan.State.(*walPlan).base
	t := c.plan.Target
	lg := c.env.Log
	type seg struct {
		path  string
		tli   uint32
		segno uint64
	}
	main := map[uint64]seg{}
	for _, f := range c.files {
		name := f.Name
		if _, ok := HistoryTLI(name); ok {
			continue
		}
		if s, ok := cutPartial(name); ok {
			name = s
		}
		tli, no, ok := ParseSegName(name, b.segSize)
		if !ok {
			return nil, fmt.Errorf("unexpected file %s among the fetched WAL", f.Name)
		}
		if m, ok := main[no]; !ok || tli > m.tli {
			main[no] = seg{f.Path, tli, no}
		}
	}
	nos := make([]uint64, 0, len(main))
	for no := range main {
		nos = append(nos, no)
	}
	sort.Slice(nos, func(i, j int) bool { return nos[i] < nos[j] })
	if len(nos) == 0 {
		return nil, errors.New("no WAL segment to replay")
	}
	if nos[0] != b.segno(b.end) {
		return nil, fmt.Errorf("the fetched WAL starts at segment %d, the backup ends in segment %d", nos[0], b.segno(b.end))
	}
	var paths []string
	for i, no := range nos {
		if i > 0 && no != nos[i-1]+1 {
			return nil, fmt.Errorf("WAL segment %d is missing", nos[i-1]+1)
		}
		paths = append(paths, main[no].path)
	}
	var after, timed int // timed records after the target / after the base
	found := false
	target := LSN(0)
	if t.Type == "lsn" {
		target, _ = ParseLSN(t.LSN)
	}
	info, err := scanSegments(paths, int64(b.segSize), func(r *walRecord) error {
		if r.LSN < b.end {
			return nil
		}
		if !r.Time.IsZero() {
			timed++
			if t.Type == "time" && r.Time.After(*t.Time) {
				after++
			}
		}
		if t.Type == "name" && r.Point == t.Name {
			found = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if strconv.FormatUint(info.SysID, 10) != b.sysID {
		return nil, fmt.Errorf("the archived WAL belongs to another cluster (system %d, the backup's is %s)", info.SysID, b.sysID)
	}
	if info.End < b.end {
		return nil, fmt.Errorf("the archived WAL ends at %s, before the end of the backup (%s)", info.End, b.end)
	}
	rp := &WALReplay{Files: c.files, SysID: b.sysID}
	stop := fmt.Sprintf("the end of the archived WAL (%s)", info.End)
	switch t.Type {
	case "time":
		if after > 0 {
			ts := t.Time.UTC().Format("2006-01-02 15:04:05.000000+00")
			rp.Settings = append(rp.Settings, [2]string{"recovery_target_time", ts})
			stop = "recovery_target_time = " + ts
		} else {
			lg.Printf("PITR: no transaction committed between the last one archived and %s — replaying all fetched WAL", t.Time.In(c.env.Location()).Format("2006-01-02 15:04:05"))
		}
	case "lsn":
		if target < info.End {
			rp.Settings = append(rp.Settings, [2]string{"recovery_target_lsn", target.String()})
			stop = "recovery_target_lsn = " + target.String()
		} else {
			lg.Printf("PITR: LSN %s is the end of the archived WAL — replaying all of it", target)
		}
	case "name":
		if !found {
			return nil, fmt.Errorf("restore point %q is not in the fetched WAL", t.Name)
		}
		rp.Settings = append(rp.Settings, [2]string{"recovery_target_name", t.Name})
		stop = fmt.Sprintf("recovery_target_name = %q", t.Name)
	}
	lg.Printf("PITR: %d WAL segment(s) checked, %s → %s, %d timed record(s) after the backup; recovery stops at %s", len(paths), b.end, info.End, timed, stop)
	return &engine.Replay{Plan: c.plan, Files: c.files, Stop: stop, State: rp}, nil
}

func cutPartial(name string) (string, bool) {
	if len(name) > len(partialExt) && name[len(name)-len(partialExt):] == partialExt {
		return name[:len(name)-len(partialExt)], true
	}
	return name, false
}

// Replay: PostgreSQL replays WAL during the physical restore's recovery.
func (Driver) Replay(*engine.Env, *engine.Replay, *engine.ReplaySpec) error {
	return errors.New("PostgreSQL replays WAL during the physical restore")
}

// placeWAL moves the fetched WAL into dir, under the names recovery asks for
// (a partial segment is used under its segment name).
func placeWAL(rp *WALReplay, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range rp.Files {
		name, _ := cutPartial(f.Name)
		if err := os.Rename(f.Path, filepath.Join(dir, name)); err != nil {
			if err := copyFile(f.Path, filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}
