package runner

import (
	"testing"
	"time"

	"dumpcat/internal/store"
)

func mkRuns(now time.Time, spec []struct {
	id, base int64
	ago      time.Duration
}) []store.Run {
	var out []store.Run
	for _, s := range spec {
		r := store.Run{ID: s.id, Type: store.TypeFull, StartedAt: now.Add(-s.ago)}
		if s.base != 0 {
			b := s.base
			r.Type, r.BaseID = store.TypeIncremental, &b
		}
		out = append(out, r)
	}
	return out
}

func TestRetentionChains(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	runs := mkRuns(now, []struct {
		id, base int64
		ago      time.Duration
	}{
		{1, 0, 10 * day}, {2, 1, 9 * day}, {3, 1, 8 * day},
		{4, 0, 3 * day}, {5, 4, 2 * day},
		{6, 0, 1 * day},
	})
	chains := buildChains(runs)
	if len(chains) != 3 || chains[0].key != 6 || chains[2].key != 1 || len(chains[2].members) != 3 {
		t.Fatalf("chains: %+v", chains)
	}
	keep := keepSet(chains, store.Retention{KeepLast: 2}, now, time.UTC)
	if !keep[6] || !keep[4] || keep[1] {
		t.Fatalf("keep last: %v", keep)
	}
	keep = keepSet(chains, store.Retention{KeepDays: 5}, now, time.UTC)
	if !keep[6] || !keep[4] || keep[1] {
		t.Fatalf("keep days: %v", keep)
	}
	// chain 1's newest member is 8 days old -> kept with 9 days
	keep = keepSet(chains, store.Retention{KeepDays: 9}, now, time.UTC)
	if !keep[1] {
		t.Fatalf("keep days uses newest member: %v", keep)
	}
	keep = keepSet(chains, store.Retention{}, now, time.UTC)
	if len(keep) != 3 {
		t.Fatal("zero policy keeps all")
	}
	keep = keepSet(chains, store.Retention{KeepWeekly: 1}, now, time.UTC)
	if !keep[6] || keep[1] {
		t.Fatalf("weekly: %v", keep)
	}
}

func TestOrphanIncrementals(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	// full #1 is NOT on this storage, only its incrementals (2,3); full #4 is.
	runs := mkRuns(now, []struct {
		id, base int64
		ago      time.Duration
	}{{2, 1, 2 * day}, {3, 1, 1 * day}, {4, 0, 5 * day}})
	chains := buildChains(runs)
	var complete []*chain
	for _, c := range chains {
		if c.full != nil {
			complete = append(complete, c)
		}
	}
	if len(complete) != 1 || complete[0].key != 4 {
		t.Fatalf("complete chains: %+v", complete)
	}
	keep := keepSet(complete, store.Retention{KeepLast: 1}, now, time.UTC)
	if !keep[4] {
		t.Fatal("the only restorable chain must be kept")
	}
}

func TestJobDir(t *testing.T) {
	if JobDir("Đơn hàng", 3) == JobDir("don hang", 4) {
		t.Fatal("job dirs must differ per job")
	}
}

func TestSlugify(t *testing.T) {
	if s := Slugify("Sao lưu Đơn hàng #1", 3); s != "sao-luu-don-hang-1" {
		t.Fatal(s)
	}
	if s := Slugify("###", 7); s != "job-7" {
		t.Fatal(s)
	}
}

// SQL Server differentials belong to the chain of their full backup: it is
// kept (and deleted) with them.
func TestRetentionDifferentials(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	runs := mkRuns(now, []struct {
		id, base int64
		ago      time.Duration
	}{
		{1, 0, 7 * day}, {2, 1, 6 * day}, {3, 1, 5 * day},
		{4, 0, 2 * day}, {5, 4, 1 * day},
	})
	for i := range runs {
		if runs[i].BaseID != nil {
			runs[i].Type = store.TypeDifferential
		}
	}
	chains := buildChains(runs)
	if len(chains) != 2 || chains[0].key != 4 || len(chains[0].members) != 2 || len(chains[1].members) != 3 {
		t.Fatalf("chains: %+v", chains)
	}
	keep := keepSet(chains, store.Retention{KeepLast: 1}, now, time.UTC)
	if !keep[4] || keep[1] {
		t.Fatalf("keep last 1: %v", keep)
	}
}
