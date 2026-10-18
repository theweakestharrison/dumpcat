package runner

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
func bf(id int64, name string, archived int) store.BinlogFile {
	sid := int64(1)
	return store.BinlogFile{ID: id, FileName: name, FirstTS: at(-1000), LastTS: at(-999), CreatedAt: at(archived), SizeBytes: 1000,
		Locations: []store.BinlogLocation{{StorageID: &sid, Status: store.LocOK}}}
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
	if !consistentAt(&r).Equal(at(11)) {
		t.Fatal("consistent_at from meta")
	}
	delete(r.Meta, "consistent_at")
	if !consistentAt(&r).Equal(at(12)) {
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

func TestSelectPITRBase(t *testing.T) {
	runs := []store.Run{
		logicalRun(3, 120, 121, "mysql-bin.000009", 4000), // newest
		logicalRun(2, 60, 62, "mysql-bin.000005", 200),
		logicalRun(1, 0, 1, "mysql-bin.000001", 300),
	}
	noPos := logicalRun(4, 90, 91, "", 0)
	noPos.Meta = map[string]any{}
	runs = append(runs, noPos)
	failed := logicalRun(5, 100, 101, "mysql-bin.000008", 5)
	failed.Status = store.StatusFailed
	runs = append(runs, failed)

	pick := func(tg PITRTarget) int64 {
		b, err := selectPITRBase(runs, tg, nil)
		if err != nil {
			return 0
		}
		return b.ID
	}
	if id := pick(PITRTarget{Type: "time", Time: tp(at(100))}); id != 2 {
		t.Fatalf("time 100 -> %d", id)
	}
	if id := pick(PITRTarget{Type: "time", Time: tp(at(61))}); id != 1 {
		t.Fatalf("inside the dump of #2 -> must use #1, got %d", id)
	}
	if id := pick(PITRTarget{Type: "time", Time: tp(at(500))}); id != 3 {
		t.Fatalf("latest time -> %d", id)
	}
	if id := pick(PITRTarget{Type: "time", Time: tp(at(0))}); id != 0 {
		t.Fatalf("before every backup -> %d", id)
	}
	if id := pick(PITRTarget{Type: "position", File: "mysql-bin.000005", Pos: 199}); id != 1 {
		t.Fatalf("position before #2 -> %d", id)
	}
	if id := pick(PITRTarget{Type: "position", File: "mysql-bin.000009", Pos: 4000}); id != 3 {
		t.Fatalf("position at #3 -> %d", id)
	}
	if id := pick(PITRTarget{Type: "latest"}); id != 3 {
		t.Fatalf("latest -> %d", id)
	}
	runs[0].Meta["gtid"] = "0-1-500"
	runs[1].Meta["gtid"] = "0-1-200"
	if id := pick(PITRTarget{Type: "gtid", GTID: "0-1-300"}); id != 2 {
		t.Fatalf("gtid 300 -> %d", id)
	}
	// usable() filters out backups that cannot be read
	b, err := selectPITRBase(runs, PITRTarget{Type: "latest"}, func(r *store.Run) bool { return r.ID != 3 })
	if err != nil || b.ID != 2 {
		t.Fatalf("usable filter: %v %v", b, err)
	}
}

func TestBinlogChain(t *testing.T) {
	files := []store.BinlogFile{
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
	gap := []store.BinlogFile{files[0], files[1]}
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

func ids(fs []store.BinlogFile) []int64 {
	out := []int64{}
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func TestExpiredBinlogs(t *testing.T) {
	files := []store.BinlogFile{
		bf(1, "mysql-bin.000001", 10),
		bf(2, "mysql-bin.000002", 20),
		bf(3, "mysql-bin.000003", 30), // open when the backup at 22 ran
		bf(4, "mysql-bin.000004", 40),
	}
	kept := []store.Run{logicalRun(10, 35, 36, "mysql-bin.000004", 90), logicalRun(9, 22, 23, "mysql-bin.000003", 500)}
	if got := ids(expiredBinlogs(files, kept)); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("expired: %v", got)
	}
	if got := expiredBinlogs(files, nil); len(got) != 0 {
		t.Fatal("no kept backups: keep everything")
	}
	// an older kept backup (even without a binlog position) keeps more
	nopos := logicalRun(11, 15, 16, "", 0)
	nopos.Meta = map[string]any{}
	if got := ids(expiredBinlogs(files, append(kept, nopos))); len(got) != 1 || got[0] != 1 {
		t.Fatalf("backup at 15: %v", got)
	}
	late := logicalRun(12, 200, 201, "mysql-bin.000099", 4)
	if got := ids(expiredBinlogs(files, []store.Run{late})); len(got) != 4 {
		t.Fatalf("everything archived before the only backup expires: %v", got)
	}
}

func TestPITRSteps(t *testing.T) {
	run := logicalRun(1, 0, 1, "mysql-bin.000001", 500)
	run.Databases = []store.DBEntry{
		{Name: "shop", BinlogFile: "mysql-bin.000001", BinlogPos: 500},
		{Name: "crm", BinlogFile: "mysql-bin.000002", BinlogPos: 120},
		{Name: "other", BinlogFile: "mysql-bin.000001", BinlogPos: 600},
	}
	plan := &pitrPlan{Starts: runStarts(&run), From: binlogPos{"mysql-bin.000001", 500}}
	st := &pitrState{plan: plan, Files: []string{"w/1", "w/2", "w/3"}, Names: []string{"mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003"},
		Stop: &binlogPos{"mysql-bin.000002", 900}}
	req := RestoreRequest{Databases: []string{"shop", "crm"}, Rename: map[string]string{"crm": "crm_restore"}}
	var got []string
	for _, s := range pitrSteps(st, &run, req) {
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
	st.Stop = &binlogPos{"mysql-bin.000002", 100}
	for _, s := range pitrSteps(st, &run, req) {
		if s.Database == "crm" {
			t.Fatalf("crm must be skipped: %+v", s)
		}
	}
	// physical: everything, unfiltered, to the end
	phys := run
	phys.Method = store.MethodPhysical
	st.Stop = nil
	steps := pitrSteps(st, &phys, RestoreRequest{})
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
		if err := x.Normalize(); err != nil {
			t.Errorf("%+v: %v", x, err)
		}
	}
	bad := []PITRTarget{
		{Type: "time"},
		{Type: "time", Time: tp(time.Now().Add(time.Hour))},
		{Type: "position", File: "-x.000001", Pos: 10},
		{Type: "position", File: "mysql-bin.000001", Pos: 1},
		{Type: "gtid", GTID: "nope"},
		{Type: "xx"},
	}
	for _, x := range bad {
		if err := x.Normalize(); err == nil {
			t.Errorf("%+v accepted", x)
		}
	}
}
