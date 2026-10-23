package update

import (
	"context"
	"fmt"
	"time"
)

// AutoBy is who an update installed by DumpCat itself is "started by".
const AutoBy = "automatic update"

// AutoPolicy is how new versions are looked for and installed without anyone clicking.
type AutoPolicy struct {
	Check    bool // look for a new version once a day
	Install  bool // and install it (implies Check), inside the window
	FromHour int  // the window starts at this hour,
	ToHour   int  // and ends at this one: ToHour < FromHour wraps past midnight, FromHour == ToHour is any hour
	Loc      *time.Location
}

// AutoStatus tells the UI what automatic installs will do.
type AutoStatus struct {
	Enabled  bool       `json:"enabled"`
	FromHour int        `json:"from_hour"`
	ToHour   int        `json:"to_hour"`
	Timezone string     `json:"timezone,omitempty"`
	Next     *time.Time `json:"next,omitempty"`    // start of the next window (now when inside it)
	Blocked  string     `json:"blocked,omitempty"` // why nothing will be installed
}

// Window returns the install window that contains now, or the next one: its start and end.
func Window(now time.Time, from, to int, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	if from == to { // any hour: a day-long window that always contains now
		return now, now.Add(24 * time.Hour)
	}
	length := time.Duration((to-from+24)%24) * time.Hour
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	// the window may have started yesterday (22:00–04:00 at 01:00)
	for _, d := range []int{-1, 0, 1} {
		s := time.Date(day.Year(), day.Month(), day.Day()+d, from, 0, 0, 0, loc)
		e := s.Add(length)
		if now.Before(e) {
			return s, e
		}
	}
	s := time.Date(day.Year(), day.Month(), day.Day()+2, from, 0, 0, 0, loc)
	return s, s.Add(length)
}

// InWindow reports whether now is inside the install window.
func InWindow(now time.Time, from, to int, loc *time.Location) bool {
	s, _ := Window(now, from, to, loc)
	return !now.Before(s)
}

// autoBlocked says why automatic installs cannot happen, "" when they can.
func (m *Manager) autoBlocked() string {
	switch u, _ := m.Server(); {
	case m.Restart == nil:
		return "updates are not available in this mode"
	case u == "":
		return "no release server is configured"
	case len(m.PublicKeys) == 0:
		return "this build trusts no release signing key, and only signed releases are installed automatically (set DUMPCAT_UPDATE_PUBLIC_KEY)"
	}
	return ""
}

func (m *Manager) autoStatus(now time.Time) AutoStatus {
	if m.Policy == nil {
		return AutoStatus{}
	}
	p := m.Policy()
	st := AutoStatus{Enabled: p.Install, FromHour: p.FromHour, ToHour: p.ToHour}
	if p.Loc != nil {
		st.Timezone = p.Loc.String()
	}
	if !p.Install {
		return st
	}
	if st.Blocked = m.autoBlocked(); st.Blocked == "" {
		s, _ := Window(now, p.FromHour, p.ToHour, p.Loc)
		if now.After(s) {
			s = now
		}
		st.Next = &s
	}
	return st
}

// Auto looks for a new version shortly after start and then once a day and, when
// the policy says so, installs a newer signed release inside the install window:
// one attempt per version and window, waiting for running tasks until the window ends.
func (m *Manager) Auto(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 5 * time.Minute
		if m.Policy != nil {
			m.autoTick(ctx, m.Policy(), time.Now())
		}
	}
}

func (m *Manager) autoTick(ctx context.Context, p AutoPolicy, now time.Time) {
	if u, _ := m.Server(); u == "" || !(p.Check || p.Install) {
		return
	}
	m.mu.Lock()
	busy, last := m.busy(), m.checkedAt
	m.mu.Unlock()
	if busy {
		return
	}
	start, end := Window(now, p.FromHour, p.ToHour, p.Loc)
	inWindow := p.Install && !now.Before(start)
	// once a day, and in the install window at least once an hour
	if last.IsZero() || now.Sub(last) >= 24*time.Hour || (inWindow && now.Sub(last) >= time.Hour) {
		if _, err := m.Check(ctx, ""); err != nil {
			m.logf("update check failed: %v", err)
			return
		}
	}
	if !inWindow || m.autoBlocked() != "" {
		return
	}
	m.mu.Lock()
	rel := m.latest
	attempt := ""
	if rel != nil {
		attempt = rel.Version + "@" + start.Format(time.RFC3339)
	}
	due := rel != nil && rel.SignedBy != "" && Compare(rel.Version, m.Version) > 0 && m.autoTried != attempt
	if due {
		m.autoTried = attempt
	}
	m.mu.Unlock()
	if !due {
		return
	}
	left := end.Sub(now)
	if left < 10*time.Minute {
		left = 10 * time.Minute
	}
	m.logf("automatic update to %s (install window %02d:00–%02d:00)", rel.Version, p.FromHour, p.ToHour)
	if m.Audit != nil {
		m.Audit("update_install", fmt.Sprintf("%s → %s", m.Version, rel.Version))
	}
	if err := m.install("", false, AutoBy, left); err != nil {
		m.logf("automatic update: %v", err)
	}
}
