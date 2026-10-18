package update

import (
	"fmt"
	"os"
	"time"
)

// maxAttempts is how many times a new version may fail to start (crash, service
// restarts) before the previous version is put back.
const maxAttempts = 3

// Boot tracks a version change across the restart that applies it.
type Boot struct {
	dataDir string
	version string
	exe     string
	p       *Pending
	done    bool
}

// StartBoot is called first thing in "serve", before the database is opened.
func StartBoot(dataDir, version, exe string) *Boot {
	Cleanup(exe)
	b := &Boot{dataDir: dataDir, version: version, exe: exe}
	p, err := LoadPending(dataDir)
	if err != nil || p == nil {
		return b
	}
	if p.ToVersion != version {
		// we are not the version the change was going to: the swap did not happen
		if p.Kind == "update" && p.FromVersion == version {
			SaveResult(dataDir, &Result{Kind: p.Kind, Status: "failed", FromVersion: p.FromVersion, ToVersion: p.ToVersion,
				Message: "the new binary was not installed; still running " + version, By: p.By})
		}
		ClearPending(dataDir)
		return b
	}
	p.Attempts++
	SavePending(dataDir, p)
	b.p = p
	return b
}

// Active reports whether this start completes an update or rollback.
func (b *Boot) Active() bool { return b.p != nil && !b.done }

// GaveUp reports whether the new version already failed to start too often.
func (b *Boot) GaveUp() bool { return b.Active() && b.p.Attempts > maxAttempts }

func (b *Boot) Attempts() int {
	if b.p == nil {
		return 0
	}
	return b.p.Attempts
}

// Healthy records a successful start and returns the scheduled backups to run now.
func (b *Boot) Healthy() []Deferred {
	if !b.Active() {
		return nil
	}
	b.done = true
	r := &Result{Kind: b.p.Kind, Status: "success", FromVersion: b.p.FromVersion, ToVersion: b.p.ToVersion,
		Snapshot: b.p.Snapshot, By: b.p.By}
	if b.p.Kind == "rollback" {
		r.Status = "rolled_back"
		r.Message = "rolled back by " + b.p.By
		// there is nothing to roll back to any more
		os.RemoveAll(PreviousExe(b.dataDir))
		os.Remove(PreviousExe(b.dataDir) + ".tmp")
	}
	SaveResult(b.dataDir, r)
	ClearPending(b.dataDir)
	return b.p.Deferred
}

// Fail puts the previous version and its database back after the new version
// could not start. It returns true when a relaunch (of the previous version) is needed.
func (b *Boot) Fail(reason string) (relaunch bool, err error) {
	if !b.Active() {
		return false, nil
	}
	b.done = true
	p := b.p
	ClearPending(b.dataDir)
	if p.Kind != "update" {
		// the version we rolled back to does not start either: nothing older to go to
		SaveResult(b.dataDir, &Result{Kind: p.Kind, Status: "failed", FromVersion: p.FromVersion, ToVersion: p.ToVersion,
			Message: "the previous version did not start: " + reason, By: p.By})
		return false, nil
	}
	err = RestoreDB(p.DBPath, p.Snapshot)
	if err == nil {
		err = copyFile(p.Previous, NewPath(b.exe), 0o755)
	}
	if err == nil {
		err = ReplaceBinary(b.exe, NewPath(b.exe))
	}
	if err != nil {
		SaveResult(b.dataDir, &Result{Kind: p.Kind, Status: "failed", FromVersion: p.FromVersion, ToVersion: p.ToVersion,
			Message: fmt.Sprintf("%s did not start (%s) and the automatic rollback failed: %v", p.ToVersion, reason, err), By: p.By})
		return false, err
	}
	SaveResult(b.dataDir, &Result{Kind: p.Kind, Status: "rolled_back", FromVersion: p.FromVersion, ToVersion: p.ToVersion,
		Message: fmt.Sprintf("DumpCat %s did not start (%s) — %s and its database were restored automatically", p.ToVersion, reason, p.FromVersion),
		By:      p.By, FinishedAt: time.Now()})
	os.Remove(PreviousExe(b.dataDir))
	return true, nil
}
