package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// StartCopy replicates an existing backup to another storage.
func (r *Runner) StartCopy(runID, storageID int64) error {
	if err := r.beginStart(); err != nil {
		return err
	}
	defer r.endStart()
	run, err := r.Store.GetRun(runID)
	if err != nil {
		return err
	}
	if run.Status != store.StatusSuccess && run.Status != store.StatusPartial {
		return errors.New("only successful backups can be copied")
	}
	if r.Store.LocationExists(runID, storageID) {
		return errors.New("the backup is already on this storage")
	}
	dst, err := r.Store.GetStorage(storageID)
	if err != nil {
		return fmt.Errorf("target storage: %w", err)
	}
	if _, ok := r.Task(fmt.Sprintf("copy:%d", runID)); ok {
		return errors.New("this backup is already being copied")
	}
	task, ctx := r.addTask("copy", runID, 0, "Copy "+run.JobName+" "+run.DirName+" → "+dst.Name)
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
		if err := r.copyRun(ctx, task, run, dst, lg); err != nil {
			lg.Printf("Copy to %s FAILED: %v", dst.Name, err)
		}
	}()
	return nil
}

func (r *Runner) copyRun(ctx context.Context, task *Task, run *store.Run, dst *store.Storage, lg *tasklog.Logger) error {
	lg.Printf("=== COPY TO %s ===", dst.Name)
	srcLoc, srcB, err := r.pickLocation(run, 0)
	if err != nil {
		return err
	}
	dstB, err := r.Storage.Get(dst)
	if err != nil {
		return err
	}
	rel := run.RelDir()
	for _, l := range run.Locations { // drop stale failed attempts on this storage
		if l.StorageID != nil && *l.StorageID == dst.ID && l.Status != store.LocOK {
			_ = r.Store.DeleteLocation(l.ID)
		}
	}
	sid := dst.ID
	loc := &store.RunLocation{RunID: run.ID, StorageID: &sid, StorageName: dst.Name, StorageType: dst.Type, Path: rel, Status: store.LocUploading}
	if err := r.Store.AddLocation(loc); err != nil {
		return err
	}
	fail := func(err error) error {
		loc.Status, loc.Error = store.LocFailed, err.Error()
		_ = r.Store.UpdateLocation(loc)
		dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		_ = dstB.DeleteDir(dctx, rel)
		cancel()
		return err
	}
	var total int64
	for _, f := range run.Files {
		total += f.Size
	}
	task.SetEstimate(total)
	start := time.Now()
	// a local source can be uploaded directly; otherwise stage a local copy
	srcDir := ""
	if srcB.IsLocal() {
		srcDir = srcB.LocalPath(srcLoc.Path)
	} else {
		task.SetPhase("download")
		srcDir = filepath.Join(r.stagingRoot(), fmt.Sprintf("work-copy-%d", run.ID))
		defer os.RemoveAll(srcDir)
		if err := os.MkdirAll(srcDir, 0o750); err != nil {
			return fail(err)
		}
		names := append([]string{"manifest.json"}, fileNames(run.Files)...)
		for _, n := range names {
			lg.Printf("Downloading %s from %s", n, srcLoc.StorageName)
			if err := srcB.Download(ctx, srcLoc.Path+"/"+n, filepath.Join(srcDir, n), &storage.Op{Log: lg}); err != nil {
				return fail(fmt.Errorf("download %s: %w", n, err))
			}
		}
	}
	task.SetPhase("upload")
	if dstB.IsLocal() {
		if err := os.MkdirAll(dstB.LocalPath(rel), 0o750); err != nil {
			return fail(err)
		}
	}
	if err := dstB.UploadDir(ctx, srcDir, rel, &storage.Op{Log: lg, OnProgress: task.SetUploadPct}); err != nil {
		return fail(err)
	}
	if err := verifyRemote(ctx, dstB, rel, run.Files); err != nil {
		return fail(err)
	}
	loc.Status, loc.SizeBytes, loc.Error = store.LocOK, run.SizeBytes, ""
	_ = r.Store.UpdateLocation(loc)
	if run.Status == store.StatusPartial {
		if fresh, err := r.Store.GetRun(run.ID); err == nil {
			allOK := true
			for _, l := range fresh.Locations {
				if l.Status != store.LocOK {
					allOK = false
				}
			}
			if allOK {
				fresh.Status, fresh.Error = store.StatusSuccess, ""
				_ = r.Store.UpdateRun(fresh)
			}
		}
	}
	lg.Printf("Copy to %s OK in %s", dst.Name, humanDuration(time.Since(start)))
	return nil
}

func fileNames(files []store.BackupFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Name
	}
	return out
}
