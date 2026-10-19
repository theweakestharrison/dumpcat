package scheduler

import (
	"errors"
	"log"
	"strings"
	"time"

	"dumpcat/internal/runner"
)

// addDrills schedules the enabled restore drills (called by Reload, locked).
func (s *Scheduler) addDrills() {
	drills, err := s.store.ListDrills(0)
	if err != nil {
		log.Printf("scheduler: drills: %v", err)
		return
	}
	for _, d := range drills {
		expr := strings.TrimSpace(d.Schedule)
		if !d.Enabled || expr == "" {
			continue
		}
		id := d.ID
		eid, err := s.cron.AddFunc(expr, func() {
			if _, err := s.runner.StartDrill(id, 0, "schedule", "scheduler"); err != nil {
				if errors.Is(err, runner.ErrDrillRunning) {
					log.Printf("scheduler: drill %d is still running, skipping this run", id)
					return
				}
				log.Printf("scheduler: drill %d: %v", id, err)
			}
		})
		if err != nil {
			log.Printf("scheduler: drill %d cron %q: %v", id, expr, err)
			continue
		}
		s.drills[id] = eid
	}
}

// NextDrill returns the next scheduled time of a drill (zero if none).
func (s *Scheduler) NextDrill(drillID int64) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return time.Time{}
	}
	id, ok := s.drills[drillID]
	if !ok {
		return time.Time{}
	}
	return s.cron.Entry(id).Next
}
