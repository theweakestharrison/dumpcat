package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	loc := time.FixedZone("ICT", 7*3600)
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, loc) }
	cases := []struct {
		name       string
		now        time.Time
		from, to   int
		start, end time.Time
		in         bool
	}{
		{"before", at(23, 1, 30), 2, 5, at(23, 2, 0), at(23, 5, 0), false},
		{"inside", at(23, 3, 10), 2, 5, at(23, 2, 0), at(23, 5, 0), true},
		{"at the start", at(23, 2, 0), 2, 5, at(23, 2, 0), at(23, 5, 0), true},
		{"at the end", at(23, 5, 0), 2, 5, at(24, 2, 0), at(24, 5, 0), false},
		{"after", at(23, 18, 0), 2, 5, at(24, 2, 0), at(24, 5, 0), false},
		{"wraps, evening", at(23, 23, 0), 22, 4, at(23, 22, 0), at(24, 4, 0), true},
		{"wraps, after midnight", at(23, 1, 0), 22, 4, at(22, 22, 0), at(23, 4, 0), true},
		{"wraps, daytime", at(23, 9, 0), 22, 4, at(23, 22, 0), at(24, 4, 0), false},
	}
	for _, c := range cases {
		s, e := Window(c.now, c.from, c.to, loc)
		if !s.Equal(c.start) || !e.Equal(c.end) || InWindow(c.now, c.from, c.to, loc) != c.in {
			t.Errorf("%s: window %s – %s, in %v; want %s – %s, %v", c.name, s, e, InWindow(c.now, c.from, c.to, loc), c.start, c.end, c.in)
		}
	}
	if !InWindow(at(23, 13, 37), 3, 3, loc) {
		t.Error("from == to is not any hour")
	}
}

// autoEnv is an env whose release server offers a signed 1.1.0 and whose policy
// installs automatically in the window from..to (hours of the local time zone).
func autoEnv(t *testing.T, from, to int) (*env, *[]string) {
	t.Helper()
	pub, priv := newKey(t)
	bin := []byte("new binary")
	m := manifestFor("1.1.0", bin)
	srv, _ := signedServer(t, "1.1.0", bin, m, Sign(priv, m))
	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	e.m.PublicKeys = []ed25519.PublicKey{pub}
	e.m.Policy = func() AutoPolicy {
		return AutoPolicy{Check: true, Install: true, FromHour: from, ToHour: to, Loc: time.Local}
	}
	audit := &[]string{}
	e.m.Audit = func(action, details string) { *audit = append(*audit, action+" "+details) }
	return e, audit
}

func TestAutoInstallOnlyInsideTheWindow(t *testing.T) {
	h := time.Now().Hour()
	e, audit := autoEnv(t, (h+2)%24, (h+4)%24)
	e.m.autoTick(context.Background(), e.m.Policy(), time.Now())
	if st := e.m.Status(); !st.Available || st.Auto.Next == nil || !st.Auto.Next.After(time.Now()) || st.Auto.Blocked != "" {
		t.Fatalf("outside the window: status %+v", st)
	}
	noRestart(t, e)

	e.m.Policy = func() AutoPolicy {
		return AutoPolicy{Check: true, Install: true, FromHour: h, ToHour: (h + 1) % 24, Loc: time.Local}
	}
	e.m.autoTick(context.Background(), e.m.Policy(), time.Now())
	p := e.waitRestart(t)
	if p.ToVersion != "1.1.0" || p.By != AutoBy {
		t.Fatalf("pending %+v", p)
	}
	if len(*audit) != 1 || !strings.Contains((*audit)[0], "1.0.0 → 1.1.0") {
		t.Fatalf("audit %v", *audit)
	}
}

func TestAutoInstallOncePerWindow(t *testing.T) {
	h := time.Now().Hour()
	e, audit := autoEnv(t, h, (h+1)%24)
	probe = func(ctx context.Context, exe, v string) error { return errors.New("does not run here") }
	var mu sync.Mutex
	var notified []Result
	e.m.Notify = func(r Result) { mu.Lock(); notified = append(notified, r); mu.Unlock() }
	e.m.autoTick(context.Background(), e.m.Policy(), time.Now())
	deadline := time.Now().Add(10 * time.Second)
	for e.m.Status().State.Phase != "failed" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if len(notified) != 1 || notified[0].By != AutoBy || notified[0].ToVersion != "1.1.0" || !strings.Contains(notified[0].Message, "does not run here") {
		t.Fatalf("notified %+v", notified)
	}
	mu.Unlock()
	// the same version is not tried again in this window
	e.m.autoTick(context.Background(), e.m.Policy(), time.Now())
	if len(*audit) != 1 {
		t.Fatalf("tried again in the same window: %v", *audit)
	}
	noRestart(t, e)
}

func TestAutoInstallNeedsASigningKey(t *testing.T) {
	h := time.Now().Hour()
	srv := releaseServer(t, "1.1.0", []byte("new binary"), "")
	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	e.m.Policy = func() AutoPolicy {
		return AutoPolicy{Check: true, Install: true, FromHour: h, ToHour: (h + 1) % 24, Loc: time.Local}
	}
	e.m.autoTick(context.Background(), e.m.Policy(), time.Now())
	st := e.m.Status()
	if !st.Available || !strings.Contains(st.Auto.Blocked, "signing key") || st.Auto.Next != nil {
		t.Fatalf("status %+v", st)
	}
	noRestart(t, e)
}

func TestAutoCheckOnlyInstallsNothing(t *testing.T) {
	h := time.Now().Hour()
	e, _ := autoEnv(t, h, (h+1)%24)
	e.m.Policy = func() AutoPolicy { return AutoPolicy{Check: true, FromHour: h, ToHour: (h + 1) % 24, Loc: time.Local} }
	e.m.autoTick(context.Background(), e.m.Policy(), time.Now())
	if st := e.m.Status(); !st.Available || st.Auto.Enabled {
		t.Fatalf("status %+v", st)
	}
	noRestart(t, e)
}

func TestTakeResultToAnnounce(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	SaveResult(dir, &Result{Kind: "update", Status: "success", FromVersion: "1.0.0", ToVersion: "1.1.0", FinishedAt: now})
	if r := TakeResultToAnnounce(dir, now); r == nil || r.ToVersion != "1.1.0" {
		t.Fatalf("first take: %+v", r)
	}
	if r := TakeResultToAnnounce(dir, now); r != nil {
		t.Fatalf("announced twice: %+v", r)
	}
	SaveResult(dir, &Result{Kind: "update", Status: "success", FinishedAt: now.Add(-48 * time.Hour)})
	if r := TakeResultToAnnounce(dir, now); r != nil {
		t.Fatalf("an old result was announced: %+v", r)
	}
}
