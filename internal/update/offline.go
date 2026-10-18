package update

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Offline performs updates and rollbacks while the server is not running
// ("dumpcat update" with the service stopped).
type Offline struct {
	Version  string
	DataDir  string
	DBPath   string
	Exe      string
	Snapshot func(path string) error

	// PublicKeys are the trusted release signing keys (see Manager.PublicKeys).
	PublicKeys []ed25519.PublicKey
}

// Check fetches latest.json from server and verifies its signature when keys are configured.
func (o *Offline) Check(ctx context.Context, server string) (*Release, error) {
	return Fetch(ctx, server, o.PublicKeys)
}

// Install downloads rel, verifies it, backs up the database and swaps the binary.
// The next start of the service completes the update (and rolls back if it fails).
func (o *Offline) Install(ctx context.Context, rel *Release, by string, progress func(done, total int64)) error {
	if len(o.PublicKeys) > 0 && rel.SignedBy == "" {
		return fmt.Errorf("release %s refused: its signature was not verified (use Offline.Check)", rel.Version)
	}
	f, err := rel.For(Platform())
	if err != nil {
		return err
	}
	newExe := NewPath(o.Exe)
	if err := Download(ctx, f, newExe, progress); err != nil {
		return err
	}
	if err := probe(ctx, newExe, rel.Version); err != nil {
		os.Remove(newExe)
		return err
	}
	if err := os.MkdirAll(Dir(o.DataDir), 0o750); err != nil {
		return err
	}
	snap := filepath.Join(Dir(o.DataDir), fmt.Sprintf("dumpcat-%s-%s.db", o.Version, time.Now().Format("20060102-150405")))
	if err := o.Snapshot(snap); err != nil {
		os.Remove(newExe)
		return fmt.Errorf("backing up the database: %w", err)
	}
	if err := savePrevious(o.DataDir, o.Exe, o.Version); err != nil {
		os.Remove(newExe)
		return err
	}
	pruneSnapshots(o.DataDir, 3)
	p := &Pending{Kind: "update", FromVersion: o.Version, ToVersion: rel.Version, Exe: o.Exe, NewExe: newExe,
		Previous: PreviousExe(o.DataDir), Snapshot: snap, DBPath: o.DBPath, By: by, StartedAt: time.Now()}
	if err := SavePending(o.DataDir, p); err != nil {
		os.Remove(newExe)
		return err
	}
	if err := ReplaceBinary(o.Exe, newExe); err != nil {
		ClearPending(o.DataDir)
		os.Remove(newExe)
		return fmt.Errorf("replacing %s: %w", o.Exe, err)
	}
	return nil
}

// Rollback restores the previous binary and the database from before the last update.
// The database must not be open.
func (o *Offline) Rollback(by string) (string, error) {
	m := &Manager{Version: o.Version, DataDir: o.DataDir}
	info := m.rollbackInfo(LoadResult(o.DataDir))
	if !info.Available {
		return "", fmt.Errorf("rollback is not possible: %s", info.Reason)
	}
	newExe := NewPath(o.Exe)
	if err := copyFile(PreviousExe(o.DataDir), newExe, 0o755); err != nil {
		return "", err
	}
	if err := RestoreDB(o.DBPath, info.Snapshot); err != nil {
		os.Remove(newExe)
		return "", err
	}
	if err := ReplaceBinary(o.Exe, newExe); err != nil {
		os.Remove(newExe)
		return "", err
	}
	SaveResult(o.DataDir, &Result{Kind: "rollback", Status: "rolled_back", FromVersion: o.Version, ToVersion: info.Version,
		Message: "rolled back by " + by, By: by})
	os.Remove(PreviousExe(o.DataDir))
	return info.Version, nil
}
