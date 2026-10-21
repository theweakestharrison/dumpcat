package mssql

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

// Point-in-time recovery: the full backup (and the differential) of each
// database is restored WITH NORECOVERY, then its log backups in LSN order,
// the one holding the target WITH STOPAT / STOPATMARK and RECOVERY. The
// driver's Restore does all of it (LogInfo().Recovery).

// CheckTarget validates a marked transaction name or an LSN.
func (Driver) CheckTarget(t *engine.PITRTarget) error {
	switch t.Type {
	case "mark":
		name := strings.TrimSpace(t.Name)
		if name == "" || len([]rune(name)) > 128 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return errors.New("enter the name of the marked transaction (BEGIN TRANSACTION name WITH MARK)")
		}
		*t = engine.PITRTarget{Type: t.Type, Name: name}
		return nil
	case "lsn":
		lsn, err := ParseLSN(t.LSN)
		if err != nil {
			return err
		}
		*t = engine.PITRTarget{Type: t.Type, LSN: lsn}
		return nil
	}
	return fmt.Errorf("invalid recovery target type: %q", t.Type)
}

// Base: a full or differential native backup whose databases recorded
// their LSNs, at least one of them in the FULL or BULK_LOGGED model.
func (Driver) Base(run *store.Run, t *engine.PITRTarget) (*engine.LogBase, error) {
	if run.Method != store.MethodMSSQL {
		return nil, fmt.Errorf("%s backups are not SQL Server backups", run.Method)
	}
	hdrs := headersOf(run.Meta)
	if len(hdrs) == 0 {
		return nil, fmt.Errorf("backup #%d records no LSNs", run.ID)
	}
	var withLog, simple []string
	var latest time.Time
	for name, h := range hdrs {
		if h.LastLSN == "" {
			continue
		}
		if !h.PITR() {
			simple = append(simple, name)
			continue
		}
		withLog = append(withLog, name)
		if h.Finish.After(latest) {
			latest = h.Finish
		}
	}
	if len(withLog) == 0 {
		if len(simple) > 0 {
			return nil, fmt.Errorf("the databases of backup #%d use the SIMPLE recovery model: no log backups follow it", run.ID)
		}
		return nil, fmt.Errorf("backup #%d records no LSNs", run.ID)
	}
	if latest.IsZero() {
		latest = engine.ConsistentAt(run)
	}
	if t != nil && t.Type == "time" && t.Time != nil && t.Time.Before(latest) {
		return nil, fmt.Errorf("backup #%d ended after the recovery time", run.ID)
	}
	sort.Strings(withLog)
	from := fmt.Sprintf("%d database(s)", len(withLog))
	if len(withLog) == 1 {
		from = withLog[0] + " at LSN " + hdrs[withLog[0]].LastLSN
	}
	return &engine.LogBase{Run: run, From: from, ConsistentAt: latest, State: hdrs}, nil
}

// replayState is the plan (and, once checked, the fetched files) per database.
type replayState struct {
	target   engine.PITRTarget
	ids      map[string][]int64           // database → log backups to apply (catalog ids)
	files    map[string][]engine.LocalLog // database → fetched log backups, in order
	problems map[string]string            // database → why it cannot reach the target
}

// chainOf returns the log backups of database that follow base, in order,
// and the first gap of the chain ("" when there is none).
func chainOf(base *Header, files []store.LogFile) (chain []store.LogFile, gap string) {
	var mine []store.LogFile
	for _, f := range files {
		if h := logHeader(f.Meta); h != nil && h.Database == base.Database && CompareLSN(h.LastLSN, base.LastLSN) > 0 {
			mine = append(mine, f)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool {
		return CompareLSN(logHeader(mine[i].Meta).FirstLSN, logHeader(mine[j].Meta).FirstLSN) < 0
	})
	at := base.LastLSN
	for _, f := range mine {
		h := logHeader(f.Meta)
		if len(chain) == 0 {
			if CompareLSN(h.FirstLSN, at) > 0 {
				return nil, fmt.Sprintf("the first log backup after the backup, %s, starts at LSN %s, after the backup's end (LSN %s): a log backup taken outside DumpCat is missing", f.FileName, h.FirstLSN, at)
			}
		} else if CompareLSN(h.FirstLSN, at) != 0 {
			if CompareLSN(h.LastLSN, at) <= 0 {
				continue // a copy of one already in the chain
			}
			return chain, fmt.Sprintf("the log chain breaks after %s (LSN %s): %s starts at LSN %s — a log backup taken outside DumpCat is missing, or the database was restored", chain[len(chain)-1].FileName, at, f.FileName, h.FirstLSN)
		}
		chain = append(chain, f)
		at = h.LastLSN
	}
	return chain, ""
}

// reaches cuts a chain at the log backup that holds the target; ok tells
// whether one does.
func reaches(chain []store.LogFile, t engine.PITRTarget) (cut []store.LogFile, ok bool) {
	for i, f := range chain {
		h := logHeader(f.Meta)
		switch t.Type {
		case "time":
			if !f.LastTS.Before(*t.Time) {
				return chain[:i+1], true
			}
		case "lsn":
			if CompareLSN(h.LastLSN, t.LSN) >= 0 && CompareLSN(h.FirstLSN, t.LSN) <= 0 {
				return chain[:i+1], true
			}
		case "mark":
			for _, m := range h.Marks {
				if m.Name == t.Name {
					return chain[:i+1], true
				}
			}
		}
	}
	return chain, t.Type == "latest"
}

func (Driver) Plan(in *engine.PlanInput) (*engine.LogPlan, error) {
	hdrs, _ := in.Base.State.(map[string]*Header)
	if hdrs == nil {
		return nil, errors.New("the base backup records no LSNs")
	}
	plan := &engine.LogPlan{Target: in.Target, Base: in.Base}
	st := &replayState{target: in.Target, ids: map[string][]int64{}, problems: map[string]string{}}
	names := make([]string, 0, len(hdrs))
	for n := range hdrs {
		names = append(names, n)
	}
	sort.Strings(names)
	var coverage time.Time
	marksKnown := false
	for _, f := range in.Files {
		if h := logHeader(f.Meta); h != nil && h.Marks != nil {
			marksKnown = true
		}
	}
	ok := 0
	var firstProblem string
	for _, name := range names {
		h := hdrs[name]
		if !h.PITR() || h.LastLSN == "" {
			st.problems[name] = "it used the SIMPLE recovery model when the backup was made (no log backups)"
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: no point-in-time recovery (SIMPLE recovery model) — deselect it, or restore the backup itself", name))
			continue
		}
		h.Database = name
		chain, gap := chainOf(h, in.Files)
		cut, reached := reaches(chain, in.Target)
		var why string
		switch {
		case reached:
		case gap != "":
			why = gap
		case in.Target.Type == "time":
			last := in.Base.ConsistentAt
			if len(chain) > 0 {
				last = chain[len(chain)-1].LastTS
			}
			why = engine.CoverageError(Driver{}.LogInfo(), last, in.Loc).Error()
		case in.Target.Type == "mark" && !marksKnown:
			// msdb's mark history was not readable: replay until SQL Server finds the mark
			cut, reached = chain, true
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: the archived log backups record no marks (msdb.dbo.logmarkhistory was not readable): every log backup is applied until SQL Server finds %q", name, in.Target.Name))
		case in.Target.Type == "mark":
			why = fmt.Sprintf("no archived log backup holds the marked transaction %q", in.Target.Name)
		case CompareLSN(in.Target.LSN, h.LastLSN) <= 0:
			why = fmt.Sprintf("LSN %s is before the end of the backup (LSN %s): choose an older backup, or another point", in.Target.LSN, h.LastLSN)
		default:
			why = fmt.Sprintf("the archived log backups end before LSN %s (an LSN names a point in the log of one database)", in.Target.LSN)
		}
		if !reached {
			st.problems[name] = why
			if firstProblem == "" {
				firstProblem = name + ": " + why
			}
			plan.Warnings = append(plan.Warnings, name+": "+why)
			continue
		}
		ok++
		if gap != "" && in.Target.Type == "latest" {
			plan.Warnings = append(plan.Warnings, name+": recovered up to the break of its log chain — "+gap)
		}
		st.ids[name] = []int64{}
		for _, f := range cut {
			plan.Files = append(plan.Files, f)
			plan.Bytes += f.SizeBytes
			st.ids[name] = append(st.ids[name], f.ID)
		}
		end := in.Base.ConsistentAt
		if len(cut) > 0 {
			end = cut[len(cut)-1].LastTS
		}
		if coverage.IsZero() || end.Before(coverage) {
			coverage = end
		}
	}
	if ok == 0 {
		if firstProblem == "" {
			firstProblem = "no database of the backup can be recovered to this point"
		}
		return nil, errors.New(firstProblem)
	}
	sort.SliceStable(plan.Files, func(i, j int) bool { return plan.Files[i].Seq < plan.Files[j].Seq })
	plan.Coverage = coverage
	plan.State = st
	return plan, nil
}

// logCheck collects the fetched log backups per database.
type logCheck struct {
	plan  *engine.LogPlan
	st    *replayState
	files map[string][]engine.LocalLog
}

func (Driver) NewCheck(env *engine.Env, plan *engine.LogPlan) engine.LogCheck {
	st, _ := plan.State.(*replayState)
	return &logCheck{plan: plan, st: st, files: map[string][]engine.LocalLog{}}
}

func (c *logCheck) Add(f engine.LocalLog) (bool, error) {
	if f.File == nil {
		return false, errors.New("SQL Server has no log file being received")
	}
	h := logHeader(f.File.Meta)
	if h == nil {
		return false, fmt.Errorf("%s records no LSNs", f.Name)
	}
	prev := c.files[h.Database]
	if n := len(prev); n > 0 {
		if p := logHeader(prev[n-1].File.Meta); CompareLSN(p.LastLSN, h.FirstLSN) != 0 {
			return false, fmt.Errorf("%s does not continue %s (LSN %s, then %s)", f.Name, prev[n-1].Name, p.LastLSN, h.FirstLSN)
		}
	}
	c.files[h.Database] = append(prev, f)
	return false, nil
}

func (c *logCheck) Finish() (*engine.Replay, error) {
	if c.st == nil {
		return nil, errors.New("the plan was not made by the SQL Server driver")
	}
	st := &replayState{target: c.st.target, ids: c.st.ids, files: map[string][]engine.LocalLog{}, problems: c.st.problems}
	var all []engine.LocalLog
	for db, ids := range c.st.ids {
		got := c.files[db]
		if len(got) != len(ids) {
			return nil, fmt.Errorf("%s: %d of %d log backups were fetched", db, len(got), len(ids))
		}
		st.files[db] = got
		all = append(all, got...)
	}
	return &engine.Replay{Plan: c.plan, Files: all, Stop: c.st.target.Describe(time.Local), State: st}, nil
}

// Replay is not used: the driver's Restore applies the log backups.
func (Driver) Replay(env *engine.Env, rp *engine.Replay, spec *engine.ReplaySpec) error {
	return errors.New("SQL Server log backups are applied by the restore itself")
}
