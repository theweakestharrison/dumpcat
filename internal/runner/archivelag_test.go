package runner

import (
	"testing"
	"time"
)

func TestArchiveLag(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	for name, c := range map[string]struct {
		st   ArchiverStatus
		want time.Duration
	}{
		"level with the server":   {ArchiverStatus{State: "streaming", StartedAt: now.Add(-time.Hour), CheckedAt: ago(10 * time.Second), CaughtUpAt: ago(10 * time.Second)}, 0},
		"behind while streaming":  {ArchiverStatus{State: "streaming", StartedAt: now.Add(-time.Hour), LagBytes: 4096, CheckedAt: ago(5 * time.Second), CaughtUpAt: ago(7 * time.Minute)}, 7 * time.Minute},
		"stream down":             {ArchiverStatus{State: "retrying", StartedAt: now.Add(-time.Hour), CaughtUpAt: ago(20 * time.Minute)}, 20 * time.Minute},
		"never connected":         {ArchiverStatus{State: "retrying", StartedAt: now.Add(-45 * time.Minute)}, 45 * time.Minute},
		"no check for a while":    {ArchiverStatus{State: "streaming", StartedAt: now.Add(-time.Hour), CheckedAt: ago(5 * time.Minute), CaughtUpAt: ago(5 * time.Minute)}, 5 * time.Minute},
		"upload waiting":          {ArchiverStatus{State: "streaming", StartedAt: now.Add(-time.Hour), CheckedAt: ago(time.Second), Pending: 2, PendingSince: ago(33 * time.Minute)}, 33 * time.Minute},
		"caught up after a start": {ArchiverStatus{State: "starting", StartedAt: now.Add(-time.Minute), CaughtUpAt: ago(2 * time.Hour)}, time.Minute},
	} {
		if got := c.st.ArchiveLagAt(now); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
