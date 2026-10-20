package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// chain = one full backup and the incrementals based on it.
type chain struct {
	full    *store.Run // may be nil for orphan incrementals
	members []*store.Run
	key     int64
	start   time.Time // time of the full (or first member)
	newest  time.Time
}

func buildChains(runs []store.Run) []*chain {
	byKey := map[int64]*chain{}
	var order []*chain
	for i := range runs {
		r := &runs[i]
		key := r.ID
		if r.Type == store.TypeIncremental && r.BaseID != nil {
			key = *r.BaseID
		}
		c, ok := byKey[key]
		if !ok {
			c = &chain{key: key}
			byKey[key] = c
			order = append(order, c)
		}
		c.members = append(c.members, r)
		if r.ID == key {
			c.full = r
		}
	}
	for _, c := range order {
		sort.Slice(c.members, func(i, j int) bool { return c.members[i].StartedAt.Before(c.members[j].StartedAt) })
		c.start = c.members[0].StartedAt
		if c.full != nil {
			c.start = c.full.StartedAt
		}
		c.newest = c.members[len(c.members)-1].StartedAt
	}
	sort.Slice(order, func(i, j int) bool { return order[i].start.After(order[j].start) })
	return order
}

// keepSet decides which chains (sorted newest first) are retained.
func keepSet(chains []*chain, p store.Retention, now time.Time, loc *time.Location) map[int64]bool {
	keep := map[int64]bool{}
	if len(chains) == 0 {
		return keep
	}
	if p.IsZero() {
		for _, c := range chains {
			keep[c.key] = true
		}
		return keep
	}
	keep[chains[0].key] = true // never delete the newest backup
	for i, c := range chains {
		if p.KeepLast > 0 && i < p.KeepLast {
			keep[c.key] = true
		}
		if p.KeepDays > 0 && c.newest.After(now.AddDate(0, 0, -p.KeepDays)) {
			keep[c.key] = true
		}
	}
	bucket := func(n int, f func(t time.Time) string) {
		if n <= 0 {
			return
		}
		seen := map[string]bool{}
		for _, c := range chains {
			k := f(c.start.In(loc))
			if seen[k] {
				continue
			}
			if len(seen) >= n {
				break
			}
			seen[k] = true
			keep[c.key] = true
		}
	}
	bucket(p.KeepDaily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(p.KeepWeekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) })
	bucket(p.KeepMonthly, func(t time.Time) string { return t.Format("2006-01") })
	return keep
}

func hasOKLocation(r *store.Run, storageID int64) *store.RunLocation {
	for i := range r.Locations {
		l := &r.Locations[i]
		if l.StorageID != nil && *l.StorageID == storageID && l.Status == store.LocOK {
			return l
		}
	}
	return nil
}

// applyRetention deletes expired backups per storage target.
func (r *Runner) applyRetention(ctx context.Context, job *store.Job, lg *tasklog.Logger) {
	runs, err := r.Store.RetentionCandidates(job.ID)
	if err != nil {
		lg.Printf("retention: %v", err)
		return
	}
	loc := r.location()
	now := time.Now()
	for _, t := range job.Targets {
		policy := job.Retention
		if t.Retention != nil {
			policy = *t.Retention
		}
		if policy.IsZero() {
			continue
		}
		var onStorage []store.Run
		for _, run := range runs {
			if hasOKLocation(&run, t.StorageID) != nil {
				onStorage = append(onStorage, run)
			}
		}
		chains := buildChains(onStorage)
		// Only chains whose full backup is on this storage are restorable from
		// it; they alone count for keep rules (and "never delete the newest").
		var complete, orphans []*chain
		for _, c := range chains {
			if c.full != nil {
				complete = append(complete, c)
			} else {
				orphans = append(orphans, c)
			}
		}
		keep := keepSet(complete, policy, now, loc)
		var oldestKept time.Time
		for _, c := range complete {
			if keep[c.key] && (oldestKept.IsZero() || c.start.Before(oldestKept)) {
				oldestKept = c.start
			}
		}
		for _, c := range orphans {
			// incrementals without their full here: keep while within the
			// retained window, drop once older than every kept full
			if oldestKept.IsZero() || !c.start.Before(oldestKept) {
				keep[c.key] = true
			}
		}
		var expired []*chain
		for _, c := range chains {
			if keep[c.key] {
				continue
			}
			busy := false
			for _, m := range c.members {
				if r.runInUse(m.ID) {
					busy = true
				}
			}
			if busy {
				lg.Printf("Retention: skipping chain #%d because it is being restored/verified", c.key)
				continue
			}
			expired = append(expired, c)
		}
		if len(expired) == 0 {
			continue
		}
		st, err := r.Store.GetStorage(t.StorageID)
		if err != nil {
			continue
		}
		backend, err := r.Storage.Get(st)
		if err != nil {
			lg.Printf("retention %s: %v", st.Name, err)
			continue
		}
		lg.Printf("Retention on %s: keeping %d, deleting %d (chains)", st.Name, len(chains)-len(expired), len(expired))
		for _, c := range expired {
			// delete incrementals first, newest first
			for i := len(c.members) - 1; i >= 0; i-- {
				m := c.members[i]
				l := hasOKLocation(m, t.StorageID)
				if l == nil {
					continue
				}
				if err := backend.DeleteDir(ctx, l.Path); err != nil {
					lg.Printf("  could not delete %s: %v", l.Path, err)
					continue
				}
				_ = r.Store.DeleteLocation(l.ID)
				lg.Printf("  deleted %s (backup #%d)", l.Path, m.ID)
				r.markDeletedIfEmpty(m.ID)
			}
		}
	}
}

func (r *Runner) markDeletedIfEmpty(runID int64) {
	run, err := r.Store.GetRun(runID)
	if err != nil {
		return
	}
	for _, l := range run.Locations {
		if l.Status == store.LocOK {
			return
		}
	}
	run.Status = store.StatusDeleted
	_ = r.Store.UpdateRun(run)
}

// DeleteRun removes a backup from one location (locationID>0) or all
// locations. Dependent incrementals block deletion unless cascade is set.
func (r *Runner) DeleteRun(ctx context.Context, runID, locationID int64, cascade bool) error {
	run, err := r.Store.GetRun(runID)
	if err != nil {
		return err
	}
	if run.Status == store.StatusRunning || run.Status == store.StatusQueued {
		return errors.New("the backup is running — cancel it first")
	}
	if r.runInUse(runID) {
		return errors.New("the backup is being restored/verified/copied — try again later")
	}
	deps, _ := r.Store.Dependents(runID)
	if len(deps) > 0 && locationID == 0 {
		if !cascade {
			return fmt.Errorf("%d incremental backup(s) depend on this one — choose to delete the whole chain", len(deps))
		}
		for i := len(deps) - 1; i >= 0; i-- {
			if err := r.DeleteRun(ctx, deps[i].ID, 0, true); err != nil {
				return err
			}
		}
	}
	var errs []error
	for _, l := range run.Locations {
		if locationID > 0 && l.ID != locationID {
			continue
		}
		if l.Status == store.LocOK && l.StorageID != nil {
			st, err := r.Store.GetStorage(*l.StorageID)
			if err == nil {
				b, err := r.Storage.Get(st)
				if err == nil {
					err = b.DeleteDir(ctx, l.Path)
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", l.StorageName, err))
					continue
				}
			}
		}
		_ = r.Store.DeleteLocation(l.ID)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	fresh, err := r.Store.GetRun(runID)
	if err != nil {
		return nil
	}
	if len(fresh.Locations) == 0 {
		if fresh.Status == store.StatusFailed || fresh.Status == store.StatusCanceled {
			_ = r.Store.DeleteRunRow(runID)
			return nil
		}
		fresh.Status = store.StatusDeleted
		_ = r.Store.UpdateRun(fresh)
	} else {
		r.markDeletedIfEmpty(runID)
	}
	return nil
}

// expiredLogFiles returns the archived files no kept backup can replay: a
// file archived before a backup started was closed before it, so it only
// holds events the backup already contains. Archive times are used because
// event times are not monotonic. Without kept backups nothing expires;
// pinned files (PostgreSQL timeline history) never do.
func expiredLogFiles(files []store.LogFile, kept []store.Run) []store.LogFile {
	if len(kept) == 0 || len(files) == 0 {
		return nil
	}
	oldest := kept[0].StartedAt
	for _, k := range kept[1:] {
		if k.StartedAt.Before(oldest) {
			oldest = k.StartedAt
		}
	}
	oldest = oldest.Truncate(time.Second)
	var out []store.LogFile
	for _, f := range files {
		if f.CreatedAt.Before(oldest) && f.Meta["pinned"] != true {
			out = append(out, f)
		}
	}
	return out
}

// applyLogRetention deletes archived log files that are older than the
// oldest backup kept on each storage (they can no longer be replayed).
func (r *Runner) applyLogRetention(ctx context.Context, job *store.Job, lg *tasklog.Logger) {
	files, err := r.Store.ListLogFiles(job.UUID)
	if err != nil || len(files) == 0 {
		return
	}
	runs, err := r.Store.RetentionCandidates(job.ID)
	if err != nil {
		return
	}
	what := "log file"
	if la := logArchiverOf(job.Method); la != nil {
		what = la.LogInfo().Name
	}
	for _, t := range job.Targets {
		var kept []store.Run
		for _, run := range runs {
			if hasOKLocation(&run, t.StorageID) != nil {
				kept = append(kept, run)
			}
		}
		var here []store.LogFile
		for _, f := range files {
			if f.OKOn(t.StorageID) != nil {
				here = append(here, f)
			}
		}
		var expired []store.LogFile
		for _, f := range expiredLogFiles(here, kept) {
			if !r.logFileInUse(f.ID) {
				expired = append(expired, f)
			}
		}
		if len(expired) == 0 {
			continue
		}
		st, err := r.Store.GetStorage(t.StorageID)
		if err != nil {
			continue
		}
		backend, err := r.Storage.Get(st)
		if err != nil {
			lg.Printf("%s retention %s: %v", what, st.Name, err)
			continue
		}
		deleted := 0
		for _, f := range expired {
			if err := backend.DeleteFile(ctx, f.Path); err != nil {
				lg.Printf("  could not delete %s %s: %v", what, f.Path, err)
				continue
			}
			_ = r.Store.DeleteLogLocation(f.OKOn(t.StorageID).ID)
			r.Store.DeleteLogIfOrphan(f.ID)
			deleted++
		}
		lg.Printf("Log retention on %s: deleted %d archived %s file(s) older than the oldest kept backup", st.Name, deleted, what)
	}
}
