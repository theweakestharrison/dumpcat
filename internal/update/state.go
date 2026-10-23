package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Files kept in <data>/update:
//
//	pending.json   written right before restarting into a new version (or a rollback)
//	last.json      result of the last update or rollback
//	previous/      the binary that ran before the last update, and its VERSION
//	dumpcat-<version>-<time>.db  database snapshots taken before each change

// Dir is the update working directory inside the data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, "update") }

// Deferred is a scheduled backup postponed by an update.
type Deferred struct {
	JobID int64  `json:"job_id"`
	Type  string `json:"type"`
}

// Pending describes a version change that is about to happen (or just happened).
type Pending struct {
	Kind        string     `json:"kind"` // update | rollback
	FromVersion string     `json:"from_version"`
	ToVersion   string     `json:"to_version"`
	Exe         string     `json:"exe"`      // binary being replaced
	NewExe      string     `json:"new_exe"`  // replacement, next to Exe
	Previous    string     `json:"previous"` // copy of the old binary (update)
	Snapshot    string     `json:"snapshot"` // database before the change (update) / to restore (rollback)
	DBPath      string     `json:"db_path"`
	By          string     `json:"by,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	Attempts    int        `json:"attempts"`
	Deferred    []Deferred `json:"deferred,omitempty"`
}

// Result is the outcome of the last update or rollback.
type Result struct {
	Kind        string    `json:"kind"`
	Status      string    `json:"status"` // success | rolled_back | failed
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	Message     string    `json:"message,omitempty"`
	Snapshot    string    `json:"snapshot,omitempty"`
	By          string    `json:"by,omitempty"`
	FinishedAt  time.Time `json:"finished_at"`
	Notified    bool      `json:"notified,omitempty"` // the notification about it was sent
}

func exeName() string {
	if runtime.GOOS == "windows" {
		return "dumpcat.exe"
	}
	return "dumpcat"
}

// PreviousExe is where the binary of the version before the last update is kept.
func PreviousExe(dataDir string) string { return filepath.Join(Dir(dataDir), "previous", exeName()) }

// PreviousVersion returns the version of PreviousExe, "" when there is none.
func PreviousVersion(dataDir string) string {
	if _, err := os.Stat(PreviousExe(dataDir)); err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(Dir(dataDir), "previous", "VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeJSON replaces path atomically.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadPending(dataDir string) (*Pending, error) {
	var p Pending
	if err := readJSON(filepath.Join(Dir(dataDir), "pending.json"), &p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func SavePending(dataDir string, p *Pending) error {
	return writeJSON(filepath.Join(Dir(dataDir), "pending.json"), p)
}

func ClearPending(dataDir string) { os.Remove(filepath.Join(Dir(dataDir), "pending.json")) }

func LoadResult(dataDir string) *Result {
	var r Result
	if err := readJSON(filepath.Join(Dir(dataDir), "last.json"), &r); err != nil {
		return nil
	}
	return &r
}

// TakeResultToAnnounce returns the result of the last update or rollback when no
// notification was sent about it yet and it is recent, and marks it as announced.
func TakeResultToAnnounce(dataDir string, now time.Time) *Result {
	r := LoadResult(dataDir)
	if r == nil || r.Notified {
		return nil
	}
	r.Notified = true
	if SaveResult(dataDir, r) != nil || now.Sub(r.FinishedAt) > 24*time.Hour {
		return nil
	}
	return r
}

func SaveResult(dataDir string, r *Result) error {
	if r.FinishedAt.IsZero() {
		r.FinishedAt = time.Now()
	}
	return writeJSON(filepath.Join(Dir(dataDir), "last.json"), r)
}

// copyFile copies src to dst through a temporary file (dst is replaced atomically).
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// savePrevious keeps a copy of the running binary so the update can be rolled back.
func savePrevious(dataDir, exe, version string) error {
	if err := copyFile(exe, PreviousExe(dataDir), 0o755); err != nil {
		return fmt.Errorf("could not keep a copy of the current binary: %w", err)
	}
	return os.WriteFile(filepath.Join(Dir(dataDir), "previous", "VERSION"), []byte(version+"\n"), 0o640)
}

// ReplaceBinary moves newFile over exe. On Windows the running binary cannot be
// overwritten but it can be renamed, so it is moved aside to <exe>.old first.
func ReplaceBinary(exe, newFile string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(newFile, exe)
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(newFile, exe); err != nil {
		os.Rename(old, exe)
		return err
	}
	return nil
}

// RestoreDB puts a snapshot back in place of the database (which must be closed).
func RestoreDB(dbPath, snapshot string) error {
	if _, err := os.Stat(snapshot); err != nil {
		return fmt.Errorf("database snapshot not found: %w", err)
	}
	if err := copyFile(snapshot, dbPath+".restore", 0o600); err != nil {
		return err
	}
	// the WAL belongs to the newer database
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	return os.Rename(dbPath+".restore", dbPath)
}

// Apply performs a pending change once the server has shut down.
func Apply(p *Pending) error {
	if p.Kind == "rollback" {
		if err := RestoreDB(p.DBPath, p.Snapshot); err != nil {
			return fmt.Errorf("restoring the database: %w", err)
		}
	}
	if err := ReplaceBinary(p.Exe, p.NewExe); err != nil {
		return fmt.Errorf("replacing %s: %w", p.Exe, err)
	}
	return nil
}

// NewPath is where a replacement binary is staged, next to exe so that the
// final rename stays on one filesystem ("dumpcat.new", "dumpcat.new.exe").
func NewPath(exe string) string {
	ext := filepath.Ext(exe)
	return strings.TrimSuffix(exe, ext) + ".new" + ext
}

// writable checks that a file can be created at path.
func writable(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(path)
}

// Cleanup removes leftovers of earlier swaps (Windows keeps the replaced binary as .old).
func Cleanup(exe string) {
	os.Remove(exe + ".old")
	os.Remove(NewPath(exe))
}

// pruneSnapshots keeps the newest n database snapshots.
func pruneSnapshots(dataDir string, keep int) {
	m, _ := filepath.Glob(filepath.Join(Dir(dataDir), "dumpcat-*.db"))
	if len(m) <= keep {
		return
	}
	sort.Slice(m, func(i, j int) bool {
		a, _ := os.Stat(m[i])
		b, _ := os.Stat(m[j])
		if a == nil || b == nil {
			return m[i] < m[j]
		}
		return a.ModTime().After(b.ModTime())
	})
	for _, p := range m[keep:] {
		os.Remove(p)
	}
}
