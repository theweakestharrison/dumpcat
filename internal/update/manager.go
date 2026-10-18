package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Drainer is the part of the task runner the updater needs.
type Drainer interface {
	ActiveTaskTitles() []string
	SetDraining(on bool)
}

// State is the progress of an update or rollback in this process.
type State struct {
	Phase     string    `json:"phase"` // idle checking downloading verifying waiting backing_up restarting failed
	Kind      string    `json:"kind,omitempty"`
	Target    string    `json:"target,omitempty"`
	Message   string    `json:"message,omitempty"`
	Done      int64     `json:"done,omitempty"`
	Total     int64     `json:"total,omitempty"`
	Waiting   []string  `json:"waiting,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

// Manager checks for, installs and rolls back DumpCat versions.
type Manager struct {
	Version  string
	DataDir  string
	DBPath   string
	Exe      string
	Server   func() (url, source string) // release server and where it is configured
	Drain    Drainer
	Snapshot func(path string) error // consistent copy of the open database
	Restart  func(p *Pending)        // shut the server down, apply p and relaunch
	Logf     func(format string, args ...any)

	// PublicKeys are the trusted release signing keys; when there is at least
	// one, only releases signed by one of them are installed.
	PublicKeys []ed25519.PublicKey

	// MaxWait bounds how long an update waits for running tasks.
	MaxWait time.Duration

	mu        sync.Mutex
	state     State
	latest    *Release
	checkedAt time.Time
	checkErr  string
	cancel    context.CancelFunc
	announced string
}

var ErrBusy = errors.New("an update is already in progress")

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Manager) set(s State) {
	m.mu.Lock()
	if s.StartedAt.IsZero() {
		s.StartedAt = m.state.StartedAt
	}
	m.state = s
	m.mu.Unlock()
}

func (m *Manager) busy() bool {
	switch m.state.Phase {
	case "", "idle", "failed":
		return false
	}
	return true
}

// Check fetches latest.json from server (or the configured release server).
func (m *Manager) Check(ctx context.Context, server string) (*Release, error) {
	if server == "" {
		server, _ = m.Server()
	}
	rel, err := Fetch(ctx, server, m.PublicKeys)
	m.mu.Lock()
	m.checkedAt = time.Now()
	announce := false
	if err != nil {
		m.checkErr = err.Error()
	} else {
		m.checkErr = ""
		m.latest = rel
		if Compare(rel.Version, m.Version) > 0 && m.announced != rel.Version {
			m.announced, announce = rel.Version, true
		}
	}
	m.mu.Unlock()
	if announce {
		m.logf("DumpCat %s is available (running %s)", rel.Version, m.Version)
	}
	return rel, err
}

// AutoCheck looks for a new version shortly after start and then once a day.
func (m *Manager) AutoCheck(ctx context.Context, enabled func() bool) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 24 * time.Hour
		if u, _ := m.Server(); u == "" || !enabled() {
			continue
		}
		m.mu.Lock()
		busy := m.busy()
		m.mu.Unlock()
		if !busy {
			if _, err := m.Check(ctx, ""); err != nil {
				m.logf("update check failed: %v", err)
			}
		}
	}
}

// Rollback information for the UI.
type RollbackInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Snapshot  string `json:"snapshot,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Status is everything the UI shows about updates.
type Status struct {
	Current    string       `json:"current"`
	Platform   string       `json:"platform"`
	Server     string       `json:"server"`
	Source     string       `json:"server_source"`
	Latest     *Release     `json:"latest"`
	Available  bool         `json:"available"`
	CheckedAt  *time.Time   `json:"checked_at"`
	CheckError string       `json:"check_error,omitempty"`
	State      State        `json:"state"`
	Last       *Result      `json:"last"`
	Rollback   RollbackInfo `json:"rollback"`
	Signing    SigningInfo  `json:"signing"`
}

func (m *Manager) Status() Status {
	u, src := m.Server()
	m.mu.Lock()
	st := Status{Current: m.Version, Platform: Platform(), Server: u, Source: src, Latest: m.latest,
		CheckError: m.checkErr, State: m.state, Signing: Signing(m.PublicKeys)}
	if !m.checkedAt.IsZero() {
		t := m.checkedAt
		st.CheckedAt = &t
	}
	m.mu.Unlock()
	if st.State.Phase == "" {
		st.State.Phase = "idle"
	}
	st.Available = st.Latest != nil && Compare(st.Latest.Version, m.Version) > 0
	st.Last = LoadResult(m.DataDir)
	st.Rollback = m.rollbackInfo(st.Last)
	return st
}

func (m *Manager) rollbackInfo(last *Result) RollbackInfo {
	prev := PreviousVersion(m.DataDir)
	switch {
	case prev == "":
		return RollbackInfo{Reason: "no previous version is kept"}
	case last == nil || last.Kind != "update" || last.Status != "success" || last.ToVersion != m.Version:
		return RollbackInfo{Version: prev, Reason: "only the last installed update can be rolled back"}
	}
	if _, err := os.Stat(last.Snapshot); err != nil {
		return RollbackInfo{Version: prev, Reason: "the database snapshot from before the update is missing"}
	}
	return RollbackInfo{Available: true, Version: prev, Snapshot: last.Snapshot}
}

// Cancel stops an update that has not started replacing anything yet.
func (m *Manager) Cancel() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.state.Phase {
	case "checking", "downloading", "verifying", "waiting":
		if m.cancel != nil {
			m.cancel()
		}
		return true
	}
	return false
}

func (m *Manager) begin(kind string) (context.Context, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy() {
		return nil, ErrBusy
	}
	if m.Restart == nil {
		return nil, errors.New("updates are not available in this mode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.state = State{Phase: "checking", Kind: kind, StartedAt: time.Now()}
	return ctx, nil
}

func (m *Manager) fail(kind, target string, err error) {
	if errors.Is(err, context.Canceled) {
		err = errors.New("cancelled")
	}
	m.logf("%s failed: %v", kind, err)
	m.set(State{Phase: "failed", Kind: kind, Target: target, Error: err.Error()})
}

// Install downloads, verifies and installs the latest version in the background.
// force reinstalls even when the release is not newer.
func (m *Manager) Install(server string, force bool, by string) error {
	if server == "" {
		server, _ = m.Server()
	}
	if _, err := ManifestURL(server); err != nil {
		return err
	}
	ctx, err := m.begin("update")
	if err != nil {
		return err
	}
	go m.runInstall(ctx, server, force, by)
	return nil
}

func (m *Manager) runInstall(ctx context.Context, server string, force bool, by string) {
	const kind = "update"
	rel, err := m.Check(ctx, server)
	if err != nil {
		m.fail(kind, "", err)
		return
	}
	target := rel.Version
	if !force && Compare(target, m.Version) <= 0 {
		m.fail(kind, target, fmt.Errorf("already up to date (running %s, latest %s)", m.Version, target))
		return
	}
	f, err := rel.For(Platform())
	if err != nil {
		m.fail(kind, target, err)
		return
	}
	m.logf("update to %s started by %s", target, by)
	newExe := NewPath(m.Exe)
	if err := writable(newExe); err != nil {
		m.fail(kind, target, fmt.Errorf("DumpCat cannot replace its own binary (%v) — give it write access to %s", err, filepath.Dir(m.Exe)))
		return
	}
	m.set(State{Phase: "downloading", Kind: kind, Target: target, Total: f.Size})
	err = Download(ctx, f, newExe, func(done, total int64) {
		m.mu.Lock()
		m.state.Done, m.state.Total = done, total
		m.mu.Unlock()
	})
	if err != nil {
		m.fail(kind, target, err)
		return
	}
	verified := "SHA-256 OK"
	if rel.SignedBy != "" {
		verified = "Signature and SHA-256 OK"
	}
	m.set(State{Phase: "verifying", Kind: kind, Target: target, Message: verified + ", test-running the new binary"})
	if err := probe(ctx, newExe, target); err != nil {
		os.Remove(newExe)
		m.fail(kind, target, err)
		return
	}
	if err := m.waitIdle(ctx, kind, target); err != nil {
		os.Remove(newExe)
		m.fail(kind, target, err)
		return
	}
	// from here on the update can no longer be cancelled
	m.set(State{Phase: "backing_up", Kind: kind, Target: target, Message: "Backing up the DumpCat database"})
	snap := filepath.Join(Dir(m.DataDir), fmt.Sprintf("dumpcat-%s-%s.db", m.Version, time.Now().Format("20060102-150405")))
	if err := os.MkdirAll(Dir(m.DataDir), 0o750); err == nil {
		err = m.Snapshot(snap)
	}
	if err == nil {
		err = savePrevious(m.DataDir, m.Exe, m.Version)
	}
	if err != nil {
		m.Drain.SetDraining(false)
		os.Remove(newExe)
		m.fail(kind, target, fmt.Errorf("backing up before the update: %w", err))
		return
	}
	pruneSnapshots(m.DataDir, 3)
	p := &Pending{Kind: kind, FromVersion: m.Version, ToVersion: target, Exe: m.Exe, NewExe: newExe,
		Previous: PreviousExe(m.DataDir), Snapshot: snap, DBPath: m.DBPath, By: by, StartedAt: time.Now()}
	if err := SavePending(m.DataDir, p); err != nil {
		m.Drain.SetDraining(false)
		os.Remove(newExe)
		m.fail(kind, target, err)
		return
	}
	m.set(State{Phase: "restarting", Kind: kind, Target: target, Message: "Restarting into DumpCat " + target})
	m.logf("installing DumpCat %s and restarting", target)
	m.Restart(p)
}

// Rollback goes back to the version that ran before the last update and its database.
func (m *Manager) Rollback(by string) error {
	info := m.rollbackInfo(LoadResult(m.DataDir))
	if !info.Available {
		return errors.New("rollback is not possible: " + info.Reason)
	}
	ctx, err := m.begin("rollback")
	if err != nil {
		return err
	}
	go m.runRollback(ctx, info, by)
	return nil
}

func (m *Manager) runRollback(ctx context.Context, info RollbackInfo, by string) {
	const kind = "rollback"
	target := info.Version
	m.logf("rollback to %s started by %s", target, by)
	if err := m.waitIdle(ctx, kind, target); err != nil {
		m.fail(kind, target, err)
		return
	}
	m.set(State{Phase: "backing_up", Kind: kind, Target: target, Message: "Backing up the current database"})
	newExe := NewPath(m.Exe)
	err := os.MkdirAll(Dir(m.DataDir), 0o750)
	if err == nil {
		// keep the current state too, in case the rollback was a mistake
		err = m.Snapshot(filepath.Join(Dir(m.DataDir), fmt.Sprintf("dumpcat-%s-%s-before-rollback.db", m.Version, time.Now().Format("20060102-150405"))))
	}
	if err == nil {
		err = copyFile(PreviousExe(m.DataDir), newExe, 0o755)
	}
	if err != nil {
		m.Drain.SetDraining(false)
		m.fail(kind, target, err)
		return
	}
	p := &Pending{Kind: kind, FromVersion: m.Version, ToVersion: target, Exe: m.Exe, NewExe: newExe,
		Snapshot: info.Snapshot, DBPath: m.DBPath, By: by, StartedAt: time.Now()}
	if err := SavePending(m.DataDir, p); err != nil {
		m.Drain.SetDraining(false)
		os.Remove(newExe)
		m.fail(kind, target, err)
		return
	}
	m.set(State{Phase: "restarting", Kind: kind, Target: target, Message: "Restarting into DumpCat " + target})
	m.Restart(p)
}

// waitIdle waits until no backup/restore/verify/copy runs, then stops new ones.
func (m *Manager) waitIdle(ctx context.Context, kind, target string) error {
	max := m.MaxWait
	if max <= 0 {
		max = 12 * time.Hour
	}
	deadline := time.Now().Add(max)
	for {
		if titles := m.Drain.ActiveTaskTitles(); len(titles) == 0 {
			m.Drain.SetDraining(true)
			if len(m.Drain.ActiveTaskTitles()) == 0 {
				return nil
			}
			m.Drain.SetDraining(false) // something started at the same moment
		} else {
			m.set(State{Phase: "waiting", Kind: kind, Target: target, Waiting: titles,
				Message: fmt.Sprintf("Waiting for %d running task(s) to finish", len(titles))})
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tasks were still running after %s", max)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// probe is Probe; tests replace it.
var probe = Probe

// Probe runs "<exe> version" and checks that it reports the expected version.
func Probe(ctx context.Context, exe, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run on this machine: %v %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), version) {
		return fmt.Errorf("the downloaded binary reports %q, expected version %s", strings.TrimSpace(string(out)), version)
	}
	return nil
}
