package runner

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// pickLocation selects a location: the requested one, else local, else first OK.
func (r *Runner) pickLocation(run *store.Run, locationID int64) (*store.RunLocation, storage.Backend, error) {
	var cands []store.RunLocation
	for _, l := range run.Locations {
		if l.Status != store.LocOK || l.StorageID == nil {
			continue
		}
		if locationID > 0 && l.ID != locationID {
			continue
		}
		cands = append(cands, l)
	}
	if len(cands) == 0 {
		return nil, nil, errors.New("no available storage location for this backup")
	}
	// prefer local
	var lastErr error
	for pass := 0; pass < 2; pass++ {
		for i := range cands {
			l := cands[i]
			st, err := r.Store.GetStorage(*l.StorageID)
			if err != nil {
				lastErr = err
				continue
			}
			isLocal := st.Type == "local"
			if (pass == 0) != isLocal {
				continue
			}
			b, err := r.Storage.Get(st)
			if err != nil {
				lastErr = err
				continue
			}
			return &l, b, nil
		}
	}
	return nil, nil, lastErr
}

// StartVerify launches an integrity check of a backup.
func (r *Runner) StartVerify(runID, locationID int64) error {
	if err := r.beginStart(); err != nil {
		return err
	}
	defer r.endStart()
	run, err := r.Store.GetRun(runID)
	if err != nil {
		return err
	}
	if run.Status != store.StatusSuccess && run.Status != store.StatusPartial {
		return errors.New("only successful backups can be verified")
	}
	key := fmt.Sprintf("verify:%d", runID)
	if _, ok := r.Task(key); ok {
		return errors.New("this backup is already being verified")
	}
	task, ctx := r.addTask("verify", runID, 0, "Verify "+run.JobName+" "+run.DirName)
	r.Store.SetRunVerify(runID, "running", "")
	release := r.useRuns(runID)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer release()
		defer r.removeTask(task)
		lg, err := tasklog.Open(r.RunLogPath(runID))
		if err != nil {
			lg = tasklog.Discard()
		}
		defer lg.Close()
		r.verifyRun(ctx, task, runID, locationID, lg)
	}()
	return nil
}

// cheapestCopy picks the copy of a backup that costs least to read back: a
// local directory first, else the first storage of the job (0: none).
func (r *Runner) cheapestCopy(runID int64) int64 {
	run, err := r.Store.GetRun(runID)
	if err != nil {
		return 0
	}
	var first int64
	for _, l := range run.Locations {
		if l.Status != store.LocOK || l.StorageID == nil {
			continue
		}
		if first == 0 {
			first = l.ID
		}
		if st, err := r.Store.GetStorage(*l.StorageID); err == nil && st.Type == "local" {
			return l.ID
		}
	}
	return first
}

func (r *Runner) verifyRun(ctx context.Context, task *Task, runID, locationID int64, lg *tasklog.Logger) bool {
	run, err := r.Store.GetRun(runID)
	if err != nil {
		return false
	}
	lg.Printf("=== INTEGRITY CHECK (verify) ===")
	task.SetPhase("verify")
	var total int64
	for _, f := range run.Files {
		total += f.Size
	}
	// every copy is checked (a badge "verified" must hold for all of them)
	var targets []store.RunLocation
	for _, l := range run.Locations {
		if l.Status == store.LocOK && l.StorageID != nil && (locationID == 0 || l.ID == locationID) {
			targets = append(targets, l)
		}
	}
	if len(targets) == 0 {
		lg.Printf("Verify error: no available storage location")
		r.Store.SetRunVerify(runID, "failed", "no available storage location")
		return false
	}
	task.SetEstimate(total * int64(len(targets)))
	start := time.Now()
	var problems, checked []string
	for i := range targets {
		loc := &targets[i]
		st, err := r.Store.GetStorage(*loc.StorageID)
		var backend storage.Backend
		if err == nil {
			backend, err = r.Storage.Get(st)
		}
		if err != nil {
			problems = append(problems, loc.StorageName+": "+err.Error())
			continue
		}
		lg.Printf("Source: %s (%s)", loc.StorageName, loc.Path)
		for _, f := range run.Files {
			if ctx.Err() != nil {
				problems = append(problems, "canceled")
				break
			}
			if err := r.verifyFile(ctx, task, run, loc, backend, f); err != nil {
				lg.Printf("  ✗ %s: %v", f.Name, err)
				problems = append(problems, loc.StorageName+"/"+f.Name+": "+err.Error())
			} else {
				lg.Printf("  ✓ %s (%s) sha256 OK", f.Name, humanBytes(f.Size))
			}
		}
		checked = append(checked, loc.StorageName)
	}
	if len(problems) > 0 {
		msg := strings.Join(problems, "; ")
		r.Store.SetRunVerify(runID, "failed", msg)
		lg.Printf("Verify FAILED after %s", humanDuration(time.Since(start)))
		if r.Notifier != nil {
			ev := notifyEvent("verify", "failed", "Backup verification FAILED: "+run.JobName, msg)
			ev.Add("Backup", run.DirName)
			r.Notifier.Send(ev)
		}
		return false
	}
	where := fmt.Sprintf("%d storages", len(checked))
	if len(checked) == 1 {
		where = checked[0]
	}
	if n := len(run.Locations); len(checked) < n && locationID != 0 {
		where += fmt.Sprintf(" (1 of %d copies)", n)
	}
	r.Store.SetRunVerify(runID, "ok", fmt.Sprintf("%d files OK on %s, %s", len(run.Files), where, humanDuration(time.Since(start))))
	lg.Printf("Verify OK in %s", humanDuration(time.Since(start)))
	return true
}

func (r *Runner) verifyFile(ctx context.Context, task *Task, run *store.Run, loc *store.RunLocation, b storage.Backend, f store.BackupFile) error {
	rc, err := b.Open(ctx, loc.Path+"/"+f.Name)
	if err != nil {
		return err
	}
	defer rc.Close()
	hr := pipeline.NewHashingReader(rc, &task.Raw)
	comp, enc := pipeline.DetectFromName(f.Name)
	dec, cleanup, err := pipeline.NewReader(hr, comp, enc, run.Passphrase)
	if err != nil {
		return err
	}
	defer cleanup()
	switch f.Kind {
	case "tar":
		tr := tar.NewReader(dec)
		n, hasMeta := 0, false
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("corrupt tar: %w", err)
			}
			if h.Name == "metadata" {
				hasMeta = true
			}
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return err
			}
			n++
		}
		if !hasMeta {
			return errors.New("mydumper metadata file is missing")
		}
	case "sql":
		t := &tailWriter{max: 512}
		if _, err := io.Copy(t, dec); err != nil {
			return fmt.Errorf("decompression error: %w", err)
		}
		if !strings.Contains(string(t.b), "Dump completed") {
			return errors.New("SQL file has no 'Dump completed' line (truncated dump?)")
		}
	default:
		if _, err := io.Copy(io.Discard, dec); err != nil {
			return fmt.Errorf("decompression error: %w", err)
		}
	}
	// drain anything left (e.g. tar padding) so the hash covers the whole file
	if _, err := io.Copy(io.Discard, hr); err != nil {
		return err
	}
	if f.SHA256 != "" && hr.SHA256() != f.SHA256 {
		return fmt.Errorf("checksum mismatch (sha256 %.12s != %.12s)", hr.SHA256(), f.SHA256)
	}
	if f.Size > 0 && hr.N != f.Size {
		return fmt.Errorf("size mismatch %d != %d", hr.N, f.Size)
	}
	return nil
}

type tailWriter struct {
	b   []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}
