package runner

import (
	"errors"
	"fmt"
)

// ErrUpdating is returned while an update is being installed.
var ErrUpdating = errors.New("DumpCat is installing an update — try again in a minute")

// Deferred is a scheduled backup that fired while an update was being installed;
// the new version starts it right after it comes up.
type Deferred struct {
	JobID int64
	Type  string
}

// SetDraining stops (or resumes) accepting new backups, restores, verifications and copies.
func (r *Runner) SetDraining(on bool) {
	r.mu.Lock()
	r.draining = on
	r.mu.Unlock()
}

// TakeDeferred returns and clears the scheduled backups rejected while draining.
func (r *Runner) TakeDeferred() []Deferred {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deferred
	r.deferred = nil
	return d
}

// ActiveTaskTitles lists what is running or about to start (binlog archivers excluded:
// they resume after a restart).
func (r *Runner) ActiveTaskTitles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	backups := 0
	for _, t := range r.tasks {
		title := t.Title
		switch t.Kind {
		case "backup":
			backups++
			title = "Backup " + t.Title
		case "restore":
			title = "Restore " + t.Title
		}
		out = append(out, title)
	}
	if n := len(r.jobRunning) - backups + r.starting; n > 0 {
		out = append(out, fmt.Sprintf("%d task(s) starting", n))
	}
	return out
}

// beginStart guards the entry of a task that is not a backup.
func (r *Runner) beginStart() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return ErrUpdating
	}
	r.starting++
	return nil
}

func (r *Runner) endStart() {
	r.mu.Lock()
	r.starting--
	r.mu.Unlock()
}
