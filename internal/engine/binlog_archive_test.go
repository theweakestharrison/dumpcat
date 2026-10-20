package engine

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"dumpcat/internal/store"
)

var t0 = time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func tp(t time.Time) *time.Time { return &t }

// bf is an archived file uploaded at minute archived. Its event times are
// deliberately bogus: replayed statements keep old timestamps, so ordering
// must never depend on them.
func bf(id int64, name string, archived int) store.LogFile {
	sid := int64(1)
	return store.LogFile{ID: id, FileName: name, FirstTS: at(-1000), LastTS: at(-999), CreatedAt: at(archived), SizeBytes: 1000,
		Locations: []store.LogLocation{{StorageID: &sid, Status: store.LocOK}}}
}

func logicalRun(id int64, started, consistent int, file string, pos int64) store.Run {
	fin := at(consistent + 1)
	return store.Run{ID: id, Method: store.MethodMysqldump, Type: store.TypeFull, Status: store.StatusSuccess, StartedAt: at(started), FinishedAt: &fin,
		Meta: map[string]any{"binlog_file": file, "binlog_pos": float64(pos), "consistent_at": float64(at(consistent).Unix())}}
}

func TestRunStartsAndConsistentAt(t *testing.T) {
	r := logicalRun(1, 10, 11, "mysql-bin.000003", 400)
	r.Databases = []store.DBEntry{{Name: "a", BinlogFile: "mysql-bin.000003", BinlogPos: 400}, {Name: "b", BinlogFile: "mysql-bin.000004", BinlogPos: 120}, {Name: "c"}}
	st := runStarts(&r)
	if st["a"].Pos != 400 || st["b"].File != "mysql-bin.000004" || st[""].Pos != 400 {
		t.Fatalf("starts: %+v", st)
	}
	if p, _ := startFor(st, "c"); p.Pos != 400 {
		t.Fatal("database without own position uses the run position")
	}
	lo, hi, ok := startRange(st)
	if !ok || lo.String() != "mysql-bin.000003:400" || hi.String() != "mysql-bin.000004:120" {
		t.Fatalf("range %v %v", lo, hi)
	}
	if !ConsistentAt(&r).Equal(at(11)) {
		t.Fatal("consistent_at from meta")
	}
	delete(r.Meta, "consistent_at")
	if !ConsistentAt(&r).Equal(at(12)) {
		t.Fatal("falls back to the end of the run")
	}
	// single-file dumps share one snapshot: per-database entries are ignored
	r.Meta["single_file"] = true
	if len(runStarts(&r)) != 1 {
		t.Fatal("single file")
	}
	none := store.Run{Meta: map[string]any{}}
	if _, _, ok := startRange(runStarts(&none)); ok {
		t.Fatal("no position")
	}
}

func TestBinlogChain(t *testing.T) {
	files := []store.LogFile{
		bf(1, "mysql-bin.000001", 10),
		bf(3, "mysql-bin.000003", 31), // uploaded before 000002 (retry)
		bf(2, "mysql-bin.000002", 32),
		// numbering restarted (RESET MASTER / physical restore): same names
		bf(4, "mysql-bin.000001", 110),
		bf(5, "mysql-bin.000002", 120),
	}
	// backup started at minute 15 while 000002 was open
	chain, err := binlogChain(files, binlogPos{"mysql-bin.000002", 50}, at(15))
	if err != nil || len(ids(chain)) != 2 || chain[0].ID != 2 || chain[1].ID != 3 {
		t.Fatalf("old sequence: %v %v", ids(chain), err)
	}
	// backup at minute 5 starts in the first 000001 and follows its sequence
	chain, err = binlogChain(files, binlogPos{"mysql-bin.000001", 4}, at(5))
	if err != nil || len(chain) != 3 || chain[0].ID != 1 || chain[1].ID != 2 || chain[2].ID != 3 {
		t.Fatalf("old sequence from 1: %v %v", ids(chain), err)
	}
	// backup after the reset: the earlier 000001 was archived before it began
	chain, err = binlogChain(files, binlogPos{"mysql-bin.000001", 4}, at(100))
	if err != nil || len(chain) != 2 || chain[0].ID != 4 || chain[1].ID != 5 {
		t.Fatalf("new sequence: %v %v", ids(chain), err)
	}
	// a gap ends the chain
	gap := []store.LogFile{files[0], files[1]}
	chain, _ = binlogChain(gap, binlogPos{"mysql-bin.000001", 4}, at(5))
	if len(chain) != 1 {
		t.Fatalf("gap: %v", ids(chain))
	}
	if _, err := binlogChain(files, binlogPos{"mysql-bin.000007", 4}, at(5)); err == nil {
		t.Fatal("missing start file must fail")
	}
	if _, err := binlogChain(files, binlogPos{"mysql-bin.000003", 4}, at(200)); err == nil {
		t.Fatal("a file archived before the backup cannot hold its position")
	}
}

func ids(fs []store.LogFile) []int64 {
	out := []int64{}
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func TestBinlogSteps(t *testing.T) {
	run := logicalRun(1, 0, 1, "mysql-bin.000001", 500)
	run.Databases = []store.DBEntry{
		{Name: "shop", BinlogFile: "mysql-bin.000001", BinlogPos: 500},
		{Name: "crm", BinlogFile: "mysql-bin.000002", BinlogPos: 120},
		{Name: "other", BinlogFile: "mysql-bin.000001", BinlogPos: 600},
	}
	plan := &LogPlan{Base: &LogBase{Run: &run, State: runStarts(&run)}, State: binlogPos{"mysql-bin.000001", 500}}
	rp := &Replay{Plan: plan, State: &binlogPos{"mysql-bin.000002", 900}, Files: []LocalLog{
		{Name: "mysql-bin.000001", Path: "w/1"}, {Name: "mysql-bin.000002", Path: "w/2"}, {Name: "mysql-bin.000003", Path: "w/3"}}}
	spec := &ReplaySpec{Run: &run, Databases: run.Databases[:2], Rename: map[string]string{"crm": "crm_restore"}}
	var got []string
	for _, s := range binlogSteps(rp, spec) {
		got = append(got, strings.Join([]string{s.File, s.Database, s.TargetDB, itoa(s.StartPos), itoa(s.StopPos)}, ","))
	}
	want := []string{
		"w/1,shop,,500,0",
		"w/2,shop,,0,900",
		"w/2,crm,crm_restore,120,900",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("logical steps:\n%v\n%v", got, want)
	}
	// stop before a database's start: nothing for it in that file
	rp.State = &binlogPos{"mysql-bin.000002", 100}
	for _, s := range binlogSteps(rp, spec) {
		if s.Database == "crm" {
			t.Fatalf("crm must be skipped: %+v", s)
		}
	}
	// physical: everything, unfiltered, to the end
	phys := run
	phys.Method = store.MethodPhysical
	rp.State = (*binlogPos)(nil)
	steps := binlogSteps(rp, &ReplaySpec{Run: &phys})
	if len(steps) != 3 || steps[0].StartPos != 500 || steps[0].Database != "" || steps[2].StopPos != 0 {
		t.Fatalf("physical: %+v", steps)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestPITRTargetNormalize(t *testing.T) {
	ok := []PITRTarget{
		{Type: "time", Time: tp(time.Now().Add(-time.Hour))},
		{Type: "position", File: "mysql-bin.000002", Pos: 4},
		{Type: "gtid", GTID: "0-1-5"},
		{Type: "latest"},
	}
	for _, x := range ok {
		if err := NormalizeTarget(MySQL{}, &x); err != nil {
			t.Errorf("%+v: %v", x, err)
		}
	}
	bad := []PITRTarget{
		{Type: "time"},
		{Type: "time", Time: tp(time.Now().Add(time.Hour))},
		{Type: "position", File: "-x.000001", Pos: 10},
		{Type: "position", File: "mysql-bin.000001", Pos: 1},
		{Type: "gtid", GTID: "nope"},
		{Type: "lsn", LSN: "0/3000000"}, // PostgreSQL's
		{Type: "xx"},
	}
	for _, x := range bad {
		if err := NormalizeTarget(MySQL{}, &x); err == nil {
			t.Errorf("%+v accepted", x)
		}
	}
}
