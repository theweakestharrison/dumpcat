package runner

import (
	"testing"
	"time"

	"dumpcat/internal/engine"
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
		b, err := selectPITRBase(engine.MySQL{}, runs, tg, nil)
		if err != nil {
			return 0
		}
		return b.Run.ID
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
	b, err := selectPITRBase(engine.MySQL{}, runs, PITRTarget{Type: "latest"}, func(r *store.Run) bool { return r.ID != 3 })
	if err != nil || b.Run.ID != 2 {
		t.Fatalf("usable filter: %v %v", b, err)
	}
}

func ids(fs []store.LogFile) []int64 {
	out := []int64{}
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func TestExpiredLogFiles(t *testing.T) {
	files := []store.LogFile{
		bf(1, "mysql-bin.000001", 10),
		bf(2, "mysql-bin.000002", 20),
		bf(3, "mysql-bin.000003", 30), // open when the backup at 22 ran
		bf(4, "mysql-bin.000004", 40),
	}
	kept := []store.Run{logicalRun(10, 35, 36, "mysql-bin.000004", 90), logicalRun(9, 22, 23, "mysql-bin.000003", 500)}
	if got := ids(expiredLogFiles(files, kept)); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("expired: %v", got)
	}
	if got := expiredLogFiles(files, nil); len(got) != 0 {
		t.Fatal("no kept backups: keep everything")
	}
	// an older kept backup (even without a binlog position) keeps more
	nopos := logicalRun(11, 15, 16, "", 0)
	nopos.Meta = map[string]any{}
	if got := ids(expiredLogFiles(files, append(kept, nopos))); len(got) != 1 || got[0] != 1 {
		t.Fatalf("backup at 15: %v", got)
	}
	late := logicalRun(12, 200, 201, "mysql-bin.000099", 4)
	if got := ids(expiredLogFiles(files, []store.Run{late})); len(got) != 4 {
		t.Fatalf("everything archived before the only backup expires: %v", got)
	}
	// timeline history files stay
	files[0].Meta = map[string]any{"pinned": true}
	if got := ids(expiredLogFiles(files, []store.Run{late})); len(got) != 3 || got[0] != 2 {
		t.Fatalf("pinned: %v", got)
	}
}
