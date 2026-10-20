package api

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"dumpcat/internal/drivers"
	"dumpcat/internal/monitor"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/runner"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

func (s *Server) runsList(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f := store.RunFilter{
		JobID:  int64(queryInt(r, "job_id", 0)),
		Status: q.Get("status"),
		Method: q.Get("method"),
		Search: strings.TrimSpace(q.Get("q")),
		Limit:  queryInt(r, "limit", 50),
		Offset: queryInt(r, "offset", 0),
	}
	if q.Get("live") == "1" {
		f.OnlyLive = true
	}
	runs, total, err := s.Store.ListRuns(f)
	if err != nil {
		return err
	}
	if runs == nil {
		runs = []store.Run{}
	}
	return ok(w, map[string]any{"items": runs, "total": total})
}

func (s *Server) runGet(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	run, err := s.Store.GetRun(id)
	if err != nil {
		return err
	}
	out := map[string]any{"run": run}
	if t, ok := s.Runner.Task(fmt.Sprintf("backup:%d", id)); ok {
		out["task"] = t
	} else if t, ok := s.Runner.Task(fmt.Sprintf("verify:%d", id)); ok {
		out["task"] = t
	} else if t, ok := s.Runner.Task(fmt.Sprintf("copy:%d", id)); ok {
		out["task"] = t
	}
	if drills, _, err := s.Store.ListDrillRuns(store.DrillRunFilter{RunID: id, Limit: 20}); err == nil {
		out["drill_runs"] = drills
	}
	if run.JobID != nil {
		if list, err := s.Store.ListDrills(*run.JobID); err == nil {
			defs := []map[string]any{}
			for _, d := range list {
				defs = append(defs, map[string]any{"id": d.ID, "name": d.Name, "target_server_name": d.TargetServerName, "running": s.Runner.DrillRunning(d.ID)})
			}
			out["drills"] = defs
		}
	}
	if deps, err := s.Store.Dependents(id); err == nil {
		ids := []int64{}
		for _, d := range deps {
			ids = append(ids, d.ID)
		}
		out["dependents"] = ids
	}
	if run.Type == store.TypeIncremental {
		chain := []map[string]any{}
		cur := run
		for i := 0; i < 500 && cur != nil; i++ {
			chain = append([]map[string]any{{"id": cur.ID, "type": cur.Type, "dir_name": cur.DirName, "status": cur.Status}}, chain...)
			if cur.ParentID == nil {
				break
			}
			p, err := s.Store.GetRun(*cur.ParentID)
			if err != nil {
				break
			}
			cur = p
		}
		out["chain"] = chain
	}
	return ok(w, out)
}

func (s *Server) sendLog(w http.ResponseWriter, r *http.Request, path string) error {
	offset := int64(queryInt(r, "offset", 0))
	if r.URL.Query().Get("tail") == "1" {
		offset = -200 * 1024
	}
	data, next, err := tasklog.ReadChunk(path, offset, 512*1024)
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"data": data, "next": next})
}

func (s *Server) runLog(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	return s.sendLog(w, r, s.Runner.RunLogPath(id))
}

func (s *Server) runCancel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	okb := s.Runner.Cancel(fmt.Sprintf("backup:%d", id))
	if !okb {
		okb = s.Runner.Cancel(fmt.Sprintf("verify:%d", id))
	}
	if !okb {
		return badRequest("the task is no longer running")
	}
	s.audit(r, "run_cancel", fmt.Sprintf("run:%d", id), "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) runVerify(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		LocationID int64 `json:"location_id"`
	}
	_ = readJSON(r, &in)
	if err := s.Runner.StartVerify(id, in.LocationID); err != nil {
		return badRequest("%v", err)
	}
	s.audit(r, "run_verify", fmt.Sprintf("run:%d", id), "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) runCopy(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		StorageID int64 `json:"storage_id"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := s.Runner.StartCopy(id, in.StorageID); err != nil {
		return badRequest("%v", err)
	}
	s.audit(r, "run_copy", fmt.Sprintf("run:%d", id), fmt.Sprintf("storage:%d", in.StorageID))
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) runDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		LocationID int64 `json:"location_id"`
		Cascade    bool  `json:"cascade"`
	}
	_ = readJSON(r, &in)
	run, err := s.Store.GetRun(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	if err := s.Runner.DeleteRun(ctx, id, in.LocationID, in.Cascade); err != nil {
		return badRequest("%v", err)
	}
	s.audit(r, "run_delete", fmt.Sprintf("run:%d", id), fmt.Sprintf("%s %s location=%d", run.JobName, run.DirName, in.LocationID))
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) runRestore(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		runner.RestoreRequest
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	run, err := s.Store.GetRun(id)
	if err != nil {
		return err
	}
	u := userOf(r)
	if m, _, ok := drivers.Method(run.Method); ok && m.Physical && in.Apply && u.Role != store.RoleAdmin {
		return errf(403, "only admins can replace the datadir")
	}
	// the service runs as root: arbitrary extraction paths are admin-only
	if strings.TrimSpace(in.TargetDir) != "" && u.Role != store.RoleAdmin {
		return errf(403, "only admins can choose a custom extraction directory")
	}
	if in.DropExisting || in.Apply {
		if strings.TrimSpace(in.Confirm) != "RESTORE" {
			return badRequest("type RESTORE to confirm the overwrite")
		}
	}
	rs, err := s.Runner.StartRestore(id, in.RestoreRequest, u.ID, u.Username)
	if err != nil {
		return badRequest("%v", err)
	}
	s.audit(r, "restore_start", fmt.Sprintf("run:%d", id), fmt.Sprintf("%s %s → %s", run.JobName, run.DirName, rs.TargetServerName))
	return ok(w, rs)
}

// runRestoreCheck compares a backup with the server it would be restored to
// (version, flavor, databases already there) and suggests portability options.
func (s *Server) runRestoreCheck(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		TargetServerID int64 `json:"target_server_id"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tc, err := s.Runner.CheckRestoreTarget(ctx, id, in.TargetServerID)
	if err != nil {
		return badRequest("%v", err)
	}
	return ok(w, tc)
}

// runDownload streams a stored backup file (as stored, or decoded with ?decode=1).
func (s *Server) runDownload(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	run, err := s.Store.GetRun(id)
	if err != nil {
		return err
	}
	name := r.URL.Query().Get("file")
	var file *store.BackupFile
	if name == "manifest.json" {
		file = &store.BackupFile{Name: name}
	}
	for i := range run.Files {
		if run.Files[i].Name == name {
			file = &run.Files[i]
		}
	}
	if file == nil {
		return errf(404, "file is not part of this backup")
	}
	locID := int64(queryInt(r, "location_id", 0))
	var loc *store.RunLocation
	for i := range run.Locations {
		l := &run.Locations[i]
		if l.Status == store.LocOK && l.StorageID != nil && (locID == 0 || l.ID == locID) {
			loc = l
			break
		}
	}
	if loc == nil {
		return errf(404, "no available storage location")
	}
	st, err := s.Store.GetStorage(*loc.StorageID)
	if err != nil {
		return err
	}
	b, err := s.Storage.Get(st)
	if err != nil {
		return err
	}
	rc, err := b.Open(r.Context(), loc.Path+"/"+file.Name)
	if err != nil {
		return err
	}
	defer rc.Close()
	var body io.Reader = rc
	outName := file.Name
	decode := r.URL.Query().Get("decode") == "1" && file.Name != "manifest.json"
	if decode {
		comp, enc := pipeline.DetectFromName(file.Name)
		dec, cleanup, err := pipeline.NewReader(rc, comp, enc, run.Passphrase)
		if err != nil {
			return badRequest("%v", err)
		}
		defer cleanup()
		body = dec
		outName = pipeline.StripExt(file.Name)
	} else if file.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(file.Size))
	}
	dl := fmt.Sprintf("%s_%s_%s", run.JobSlug, run.DirName, path.Base(outName))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": dl}))
	s.audit(r, "run_download", fmt.Sprintf("run:%d", id), outName)
	_, err = io.Copy(w, body)
	if err != nil {
		return nil // client aborted; headers already sent
	}
	return nil
}

// ---- restores ----

func (s *Server) restoresList(w http.ResponseWriter, r *http.Request) error {
	list, total, err := s.Store.ListRestores(queryInt(r, "limit", 50), queryInt(r, "offset", 0))
	if err != nil {
		return err
	}
	if list == nil {
		list = []store.Restore{}
	}
	return ok(w, map[string]any{"items": list, "total": total})
}

func (s *Server) restoreGet(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	rs, err := s.Store.GetRestore(id)
	if err != nil {
		return err
	}
	out := map[string]any{"restore": rs}
	if t, ok := s.Runner.Task(fmt.Sprintf("restore:%d", id)); ok {
		out["task"] = t
	}
	return ok(w, out)
}

func (s *Server) restoreLog(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	return s.sendLog(w, r, s.Runner.RestoreLogPath(id))
}

func (s *Server) restoreCancel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if !s.Runner.Cancel(fmt.Sprintf("restore:%d", id)) {
		return badRequest("the task is no longer running")
	}
	s.audit(r, "restore_cancel", fmt.Sprintf("restore:%d", id), "")
	return ok(w, map[string]bool{"ok": true})
}

// ---- activity / dashboard ----

func (s *Server) activity(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.Runner.Tasks())
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) error {
	stats := s.Store.Stats()
	recent, _, _ := s.Store.ListRuns(store.RunFilter{Limit: 12})
	if recent == nil {
		recent = []store.Run{}
	}
	jobs, _ := s.Store.ListJobs()
	var health []map[string]any
	var warnings []string
	now, loc, set := time.Now(), s.Runner.Location(), s.Store.GetSettings()
	grace := time.Duration(set.StaleGraceMinutes) * time.Minute
	for i := range jobs {
		j := &jobs[i]
		h := map[string]any{"id": j.ID, "name": j.Name, "method": j.Method, "enabled": j.Enabled, "server_name": j.ServerName, "running": s.Runner.IsJobRunning(j.ID)}
		if lr := s.Store.LastRun(j.ID); lr != nil {
			h["last_status"] = lr.Status
			h["last_at"] = lr.StartedAt
			h["last_size"] = lr.SizeBytes
			if lr.Status == store.StatusFailed && j.Enabled {
				warnings = append(warnings, fmt.Sprintf("Job \"%s\": last run FAILED — %s", j.Name, lr.Error))
			}
		} else if j.Enabled {
			h["last_status"] = ""
		}
		var last *time.Time
		if ls := s.Store.LastSuccessfulRun(j.ID, j.Method); ls != nil {
			h["last_success_at"] = ls.StartedAt
			last = &ls.StartedAt
		}
		// the same rule as the freshness alerts
		if f := monitor.Check(j, last, now, loc, grace); f.Monitored {
			h["due_at"] = f.Due
			h["stale"] = f.Stale
			if f.Stale {
				since := "yet"
				if last != nil {
					since = "since " + last.In(loc).Format("2 Jan 15:04")
				}
				warnings = append(warnings, fmt.Sprintf("Job \"%s\": no successful backup %s — one was due by %s", j.Name, since, f.Due.In(loc).Format("2 Jan 15:04")))
			}
		}
		if t, typ := s.Sched.NextRun(j.ID); !t.IsZero() {
			h["next_at"] = t
			h["next_type"] = typ
		}
		health = append(health, h)
	}
	if health == nil {
		health = []map[string]any{}
	}
	if !s.Tools.Get("rclone").Found {
		sts, _ := s.Store.ListStorages()
		for _, st := range sts {
			if st.Type != "local" {
				warnings = append(warnings, "rclone is not installed but a remote storage is configured ("+st.Name+")")
				break
			}
		}
	}
	for _, a := range s.Runner.ArchiverStatuses() {
		log := a.Log // binlog, WAL
		switch {
		case a.State != "streaming" && a.Error != "":
			warnings = append(warnings, fmt.Sprintf("Job \"%s\": %s archiving is failing — %s", a.JobName, log, a.Error))
		case a.UploadError != "":
			warnings = append(warnings, fmt.Sprintf("Job \"%s\": %s upload is failing — %s", a.JobName, log, a.UploadError))
		case a.Pending > 5:
			warnings = append(warnings, fmt.Sprintf("Job \"%s\": %d %s files are waiting for upload", a.JobName, a.Pending, log))
		case set.BinlogLagMinutes > 0 && a.ArchiveLag > int64(set.BinlogLagMinutes)*60:
			warnings = append(warnings, fmt.Sprintf("Job \"%s\": the %s archive is %s behind the server", a.JobName, log, monitor.Duration(time.Duration(a.ArchiveLag)*time.Second)))
		}
	}
	if drills, err := s.Store.ListDrills(0); err == nil {
		for _, d := range drills {
			x := s.Store.LastDrillRun(d.ID)
			switch {
			case x == nil:
			case x.Status == store.DrillFailed:
				msg, _, _ := strings.Cut(x.Error, "\n")
				warnings = append(warnings, fmt.Sprintf("Restore drill \"%s\": the last run FAILED — %s", d.Name, msg))
			case len(x.Leftover) > 0 && x.Status != store.StatusRunning:
				warnings = append(warnings, fmt.Sprintf("Restore drill \"%s\": temporary databases left on %s: %s", d.Name, x.TargetServerName, strings.Join(x.Leftover, ", ")))
			}
		}
	}
	if warnings == nil {
		warnings = []string{}
	}
	return ok(w, map[string]any{
		"stats": stats, "recent": recent, "jobs": health, "tasks": s.Runner.Tasks(), "warnings": warnings,
		"daily": s.Store.DailyStats(14, s.Runner.Location()),
	})
}
