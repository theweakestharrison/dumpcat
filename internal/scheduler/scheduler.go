// Package scheduler triggers job backups from cron expressions.
package scheduler

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"dumpcat/internal/runner"
	"dumpcat/internal/store"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Validate checks a 5-field cron expression or descriptor (@daily, @every 6h...).
func Validate(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return nil
	}
	_, err := parser.Parse(expr)
	if err != nil {
		return errors.New("invalid cron expression: " + err.Error())
	}
	return nil
}

// Next returns the next n activation times of expr.
func Next(expr string, n int, loc *time.Location) []time.Time {
	s, err := parser.Parse(expr)
	if err != nil {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}
	t := time.Now().In(loc)
	out := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

type Scheduler struct {
	mu      sync.Mutex
	store   *store.Store
	runner  *runner.Runner
	cron    *cron.Cron
	loc     *time.Location
	entries map[int64][]entryRef
}

type entryRef struct {
	id  cron.EntryID
	typ string
}

func New(st *store.Store, r *runner.Runner) *Scheduler {
	return &Scheduler{store: st, runner: r, entries: map[int64][]entryRef{}}
}

func (s *Scheduler) location() *time.Location {
	if tz := s.store.GetSettings().Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// Reload rebuilds all cron entries from the database.
func (s *Scheduler) Reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		s.cron.Stop()
	}
	s.loc = s.location()
	s.cron = cron.New(cron.WithParser(parser), cron.WithLocation(s.loc))
	s.entries = map[int64][]entryRef{}
	jobs, err := s.store.ListJobs()
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		s.add(j.ID, j.Schedule, store.TypeFull, "")
		if j.Method == store.MethodPhysical && strings.TrimSpace(j.IncrSchedule) != "" {
			s.add(j.ID, j.IncrSchedule, store.TypeIncremental, j.Schedule)
		}
	}
	// housekeeping every hour
	s.cron.AddFunc("17 * * * *", func() { s.runner.Housekeeping() })
	s.cron.Start()
}

func (s *Scheduler) add(jobID int64, expr, typ, fullExpr string) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return
	}
	var full cron.Schedule
	if strings.TrimSpace(fullExpr) != "" {
		full, _ = parser.Parse(fullExpr)
	}
	loc := s.loc
	id, err := s.cron.AddFunc(expr, func() {
		// When the full and the incremental schedule fire in the same minute,
		// only the full runs (the two cron entries race otherwise).
		if full != nil {
			m := time.Now().In(loc).Truncate(time.Minute)
			if full.Next(m.Add(-time.Second)).Equal(m) {
				return
			}
		}
		if _, err := s.runner.StartBackup(jobID, typ, "schedule", "scheduler"); err != nil {
			if errors.Is(err, runner.ErrJobRunning) {
				log.Printf("scheduler: job %d is already running, skipping this %s run", jobID, typ)
				return
			}
			log.Printf("scheduler: job %d: %v", jobID, err)
		}
	})
	if err != nil {
		log.Printf("scheduler: job %d cron %q: %v", jobID, expr, err)
		return
	}
	s.entries[jobID] = append(s.entries[jobID], entryRef{id, typ})
}

// NextRun returns the next scheduled time of a job (zero if none).
func (s *Scheduler) NextRun(jobID int64) (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return time.Time{}, ""
	}
	var best time.Time
	typ := ""
	for _, ref := range s.entries[jobID] {
		e := s.cron.Entry(ref.id)
		if e.Next.IsZero() {
			continue
		}
		if best.IsZero() || e.Next.Before(best) {
			best, typ = e.Next, ref.typ
		}
	}
	return best, typ
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		<-s.cron.Stop().Done()
	}
}
