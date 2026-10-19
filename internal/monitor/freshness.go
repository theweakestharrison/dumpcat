// Package monitor watches backup freshness and binlog archiving, sends the
// alerts and the weekly digest, and exposes Prometheus metrics.
package monitor

import (
	"fmt"
	"strings"
	"time"

	"dumpcat/internal/scheduler"
	"dumpcat/internal/store"
)

// Freshness tells whether a job has a recent enough successful backup.
type Freshness struct {
	Monitored   bool          // enabled, with a schedule or a max age
	LastSuccess *time.Time    // start of the newest successful backup
	Due         time.Time     // a newer successful backup is expected by then
	Interval    time.Duration // between two scheduled runs (0 with a max age)
	Window      time.Duration // the age that is tolerated
	MaxAge      bool          // the window is the job's max_age_hours
	Stale       bool
}

// Check evaluates the freshness of a job whose newest successful backup
// started at last (nil: none yet, the job's creation counts).
//
// Without a max age on the job, the next successful backup is due half an
// interval plus the grace period after the first run scheduled after the last
// one: 1.5 × the interval between two runs plus the grace for a regular
// schedule ("0 2 * * *": 36h + grace), and correct across gaps such as
// weekends ("0 2 * * 1-5": Monday's run is due by Monday 14:00 + grace). With
// several schedules (full and incremental) the earliest due time counts.
func Check(j *store.Job, last *time.Time, now time.Time, loc *time.Location, grace time.Duration) Freshness {
	f := Freshness{LastSuccess: last}
	if !j.Enabled {
		return f
	}
	if loc == nil {
		loc = time.Local
	}
	ref := j.CreatedAt
	if last != nil {
		ref = *last
	}
	if j.Options.MaxAgeHours > 0 {
		f.Monitored, f.MaxAge = true, true
		f.Window = time.Duration(j.Options.MaxAgeHours) * time.Hour
		f.Due = ref.Add(f.Window)
		f.Stale = now.After(f.Due)
		return f
	}
	for _, expr := range Schedules(j) {
		s := scheduler.Parse(expr)
		if s == nil {
			continue
		}
		// a minute later: a run that started a moment before its slot is that run
		n1 := s.Next(ref.In(loc).Add(time.Minute))
		if n1.IsZero() {
			continue
		}
		n2 := s.Next(n1)
		if n2.IsZero() {
			continue
		}
		iv := n2.Sub(n1)
		if due := n1.Add(iv/2 + grace); f.Due.IsZero() || due.Before(f.Due) {
			f.Due, f.Interval = due, iv
		}
	}
	if f.Due.IsZero() {
		return f
	}
	f.Monitored = true
	f.Window = f.Interval*3/2 + grace
	f.Stale = now.After(f.Due)
	return f
}

// Schedules returns the cron expressions that run a job.
func Schedules(j *store.Job) []string {
	var out []string
	if strings.TrimSpace(j.Schedule) != "" {
		out = append(out, j.Schedule)
	}
	if j.Method == store.MethodPhysical && strings.TrimSpace(j.IncrSchedule) != "" {
		out = append(out, j.IncrSchedule)
	}
	return out
}

// AutoWindow is the age tolerated for a schedule without a max age: 1.5 ×
// the interval between its next two runs plus the grace period (0 when expr
// schedules nothing).
func AutoWindow(expr string, now time.Time, loc *time.Location, grace time.Duration) time.Duration {
	s := scheduler.Parse(expr)
	if s == nil {
		return 0
	}
	if loc == nil {
		loc = time.Local
	}
	n1 := s.Next(now.In(loc))
	if n1.IsZero() {
		return 0
	}
	n2 := s.Next(n1)
	if n2.IsZero() {
		return 0
	}
	return n2.Sub(n1)*3/2 + grace
}

// Expected describes the window, e.g. "every 24h by the schedule, so within 37h".
func (f Freshness) Expected() string {
	if f.MaxAge {
		return "within " + Duration(f.Window) + " (max age set on the job)"
	}
	return fmt.Sprintf("every %s by the schedule, so within %s (1.5 × the interval + grace)", Duration(f.Interval), Duration(f.Window))
}

// Duration formats a duration for people: 45s, 12m, 3h20m, 2d4h.
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh%02dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	days, h := int(d.Hours())/24, int(d.Hours())%24
	if h != 0 {
		return fmt.Sprintf("%dd%dh", days, h)
	}
	return fmt.Sprintf("%dd", days)
}
