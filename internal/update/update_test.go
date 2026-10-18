package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0}, {"1.0.1", "1.0.0", 1}, {"1.2.0", "1.10.0", -1}, {"v2.0", "1.9.9", 1},
		{"1.1.0-rc.1", "1.1.0", -1}, {"1.1.0", "1.1.0-rc.1", 1}, {"dev", "0.0.1", -1}, {"1.0.0", "dev", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestManifestURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://dl.example.com/dumpcat":           "https://dl.example.com/dumpcat/latest.json",
		"https://dl.example.com/dumpcat/":          "https://dl.example.com/dumpcat/latest.json",
		"https://dl.example.com/beta/channel.json": "https://dl.example.com/beta/channel.json",
		"http://127.0.0.1:9000/rel":                "http://127.0.0.1:9000/rel/latest.json",
		"http://localhost/rel":                     "http://localhost/rel/latest.json",
	} {
		u, err := ManifestURL(in)
		if err != nil || u.String() != want {
			t.Errorf("ManifestURL(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x/y", "http://dl.example.com/dumpcat", "not a url"} {
		if _, err := ManifestURL(bad); err == nil {
			t.Errorf("ManifestURL(%q) accepted", bad)
		}
	}
}

// releaseServer serves latest.json and one binary per platform.
func releaseServer(t *testing.T, version string, binary []byte, sum string) *httptest.Server {
	t.Helper()
	if sum == "" {
		h := sha256.Sum256(binary)
		sum = hex.EncodeToString(h[:])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/rel/latest.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"version": version, "notes": "Fixes", "files": map[string]any{
			Platform(): map[string]any{"url": version + "/dumpcat-bin", "sha256": sum, "size": len(binary)}}})
	})
	mux.HandleFunc("/rel/"+version+"/dumpcat-bin", func(w http.ResponseWriter, r *http.Request) { w.Write(binary) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchAndDownload(t *testing.T) {
	bin := []byte("new binary")
	srv := releaseServer(t, "1.2.0", bin, "")
	rel, err := Fetch(context.Background(), srv.URL+"/rel", nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := rel.For(Platform())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.URL, srv.URL+"/rel/1.2.0/") {
		t.Fatalf("relative URL resolved to %s", f.URL)
	}
	dest := filepath.Join(t.TempDir(), "dl")
	if err := Download(context.Background(), f, dest, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != string(bin) {
		t.Fatalf("downloaded %q", b)
	}
	if _, err := rel.For("plan9/mips"); err == nil {
		t.Fatal("missing platform accepted")
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	srv := releaseServer(t, "1.2.0", []byte("tampered"), strings.Repeat("ab", 32))
	rel, err := Fetch(context.Background(), srv.URL+"/rel", nil)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := rel.For(Platform())
	dest := filepath.Join(t.TempDir(), "dl")
	err = Download(context.Background(), f, dest, nil)
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a file with a wrong checksum was left behind")
	}
}

type fakeDrain struct {
	mu       sync.Mutex
	busyFor  int // number of polls that still report a running task
	draining bool
}

func (d *fakeDrain) ActiveTaskTitles() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busyFor > 0 {
		d.busyFor--
		return []string{"Backup shop"}
	}
	return nil
}

func (d *fakeDrain) SetDraining(on bool) {
	d.mu.Lock()
	d.draining = on
	d.mu.Unlock()
}

type env struct {
	data, exe, db string
	m             *Manager
	restarts      chan *Pending
	drain         *fakeDrain
}

func newEnv(t *testing.T, version, server string) *env {
	t.Helper()
	probe = func(ctx context.Context, exe, v string) error { return nil }
	t.Cleanup(func() { probe = Probe })
	dir := t.TempDir()
	e := &env{data: filepath.Join(dir, "data"), exe: filepath.Join(dir, "bin", exeName()), restarts: make(chan *Pending, 1), drain: &fakeDrain{}}
	e.db = filepath.Join(e.data, "dumpcat.db")
	os.MkdirAll(filepath.Dir(e.exe), 0o755)
	os.MkdirAll(e.data, 0o755)
	os.WriteFile(e.exe, []byte("old binary"), 0o755)
	os.WriteFile(e.db, []byte("db v1"), 0o600)
	e.m = &Manager{Version: version, DataDir: e.data, DBPath: e.db, Exe: e.exe, Drain: e.drain,
		Server:   func() (string, string) { return server, "settings" },
		Snapshot: func(p string) error { b, _ := os.ReadFile(e.db); return os.WriteFile(p, b, 0o600) },
		Restart:  func(p *Pending) { e.restarts <- p }}
	return e
}

func (e *env) waitRestart(t *testing.T) *Pending {
	t.Helper()
	select {
	case p := <-e.restarts:
		return p
	case <-time.After(20 * time.Second):
		t.Fatalf("no restart; state %+v", e.m.Status().State)
	}
	return nil
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallWaitsForTasksThenSwaps(t *testing.T) {
	srv := releaseServer(t, "1.1.0", []byte("new binary"), "")
	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	e.drain.busyFor = 2 // a backup is running for two polls
	if err := e.m.Install("", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Install("", false, "admin"); err != ErrBusy {
		t.Fatalf("second install: %v", err)
	}
	p := e.waitRestart(t)
	if !e.drain.draining {
		t.Fatal("new tasks are not blocked while restarting")
	}
	if p.FromVersion != "1.0.0" || p.ToVersion != "1.1.0" || p.Kind != "update" {
		t.Fatalf("pending %+v", p)
	}
	if read(t, p.Snapshot) != "db v1" || read(t, PreviousExe(e.data)) != "old binary" || PreviousVersion(e.data) != "1.0.0" {
		t.Fatal("database snapshot or previous binary missing")
	}
	if err := Apply(p); err != nil {
		t.Fatal(err)
	}
	if read(t, e.exe) != "new binary" {
		t.Fatal("binary not replaced")
	}
	// the new version starts
	b := StartBoot(e.data, "1.1.0", e.exe)
	if !b.Active() || b.Attempts() != 1 {
		t.Fatalf("boot active=%v attempts=%d", b.Active(), b.Attempts())
	}
	b.Healthy()
	r := LoadResult(e.data)
	if r == nil || r.Status != "success" || r.ToVersion != "1.1.0" {
		t.Fatalf("result %+v", r)
	}
	if p, _ := LoadPending(e.data); p != nil {
		t.Fatal("pending left behind")
	}
	m2 := &Manager{Version: "1.1.0", DataDir: e.data}
	if info := m2.rollbackInfo(r); !info.Available || info.Version != "1.0.0" {
		t.Fatalf("rollback info %+v", info)
	}
}

func TestUpToDateAndChecksumFailureDoNotRestart(t *testing.T) {
	srv := releaseServer(t, "1.0.0", []byte("same"), "")
	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	e.m.Install("", false, "admin")
	waitPhase(t, e.m, "failed")
	if !strings.Contains(e.m.Status().State.Error, "already up to date") {
		t.Fatalf("state %+v", e.m.Status().State)
	}
	bad := releaseServer(t, "2.0.0", []byte("evil"), strings.Repeat("cd", 32))
	e.m.Server = func() (string, string) { return bad.URL + "/rel", "settings" }
	if err := e.m.Install("", false, "admin"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e.m, "failed")
	if !strings.Contains(e.m.Status().State.Error, "SHA-256") || read(t, e.exe) != "old binary" {
		t.Fatalf("state %+v", e.m.Status().State)
	}
	select {
	case <-e.restarts:
		t.Fatal("restarted after a failed check")
	default:
	}
}

func waitPhase(t *testing.T, m *Manager, phase string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if m.Status().State.Phase == phase {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("phase %s never reached: %+v", phase, m.Status().State)
}

// installed applies an update to 1.1.0 and returns the env.
func installed(t *testing.T) *env {
	srv := releaseServer(t, "1.1.0", []byte("new binary"), "")
	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	if err := e.m.Install("", false, "admin"); err != nil {
		t.Fatal(err)
	}
	p := e.waitRestart(t)
	if err := Apply(p); err != nil {
		t.Fatal(err)
	}
	// the new version changes the database before failing
	os.WriteFile(e.db, []byte("db v2 (half migrated)"), 0o600)
	return e
}

func TestFailedStartRollsBack(t *testing.T) {
	e := installed(t)
	b := StartBoot(e.data, "1.1.0", e.exe)
	again, err := b.Fail("migration 4 failed")
	if err != nil || !again {
		t.Fatalf("Fail = %v, %v", again, err)
	}
	if read(t, e.exe) != "old binary" || read(t, e.db) != "db v1" {
		t.Fatal("previous binary or database not restored")
	}
	r := LoadResult(e.data)
	if r.Status != "rolled_back" || !strings.Contains(r.Message, "migration 4 failed") {
		t.Fatalf("result %+v", r)
	}
	// the old version starts normally afterwards
	if b := StartBoot(e.data, "1.0.0", e.exe); b.Active() {
		t.Fatal("old version thinks an update is pending")
	}
}

func TestCrashLoopGivesUp(t *testing.T) {
	e := installed(t)
	for i := 1; i <= maxAttempts; i++ {
		if b := StartBoot(e.data, "1.1.0", e.exe); b.GaveUp() {
			t.Fatalf("gave up after %d attempts", i)
		}
	}
	b := StartBoot(e.data, "1.1.0", e.exe)
	if !b.GaveUp() {
		t.Fatal("did not give up")
	}
	if again, err := b.Fail("crash loop"); err != nil || !again {
		t.Fatal(again, err)
	}
	if read(t, e.exe) != "old binary" {
		t.Fatal("not rolled back")
	}
}

func TestManualRollback(t *testing.T) {
	e := installed(t)
	StartBoot(e.data, "1.1.0", e.exe).Healthy()
	m := &Manager{Version: "1.1.0", DataDir: e.data, DBPath: e.db, Exe: e.exe, Drain: &fakeDrain{},
		Server:   func() (string, string) { return "", "" },
		Snapshot: func(p string) error { return os.WriteFile(p, []byte("snap"), 0o600) },
		Restart:  func(p *Pending) { e.restarts <- p }}
	if err := m.Rollback("admin"); err != nil {
		t.Fatal(err)
	}
	p := e.waitRestart(t)
	if p.Kind != "rollback" || p.ToVersion != "1.0.0" {
		t.Fatalf("pending %+v", p)
	}
	if err := Apply(p); err != nil {
		t.Fatal(err)
	}
	if read(t, e.exe) != "old binary" || read(t, e.db) != "db v1" {
		t.Fatal("rollback did not restore binary and database")
	}
	b := StartBoot(e.data, "1.0.0", e.exe)
	if !b.Active() {
		t.Fatal("rollback boot not tracked")
	}
	b.Healthy()
	if r := LoadResult(e.data); r.Kind != "rollback" || r.Status != "rolled_back" {
		t.Fatalf("result %+v", r)
	}
	if v := PreviousVersion(e.data); v != "" {
		t.Fatalf("previous version still offered: %s", v)
	}
}

func TestSwapNotAppliedIsReported(t *testing.T) {
	e := installed(t)
	// pretend the old binary came back up (e.g. the swap was undone by hand)
	b := StartBoot(e.data, "1.0.0", e.exe)
	if b.Active() {
		t.Fatal("unexpected active boot")
	}
	if r := LoadResult(e.data); r == nil || r.Status != "failed" {
		t.Fatalf("result %+v", r)
	}
}
