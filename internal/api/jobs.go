package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dumpcat/internal/pipeline"
	"dumpcat/internal/runner"
	"dumpcat/internal/scheduler"
	"dumpcat/internal/store"
)

type jobInput struct {
	Name             string            `json:"name"`
	ServerID         int64             `json:"server_id"`
	Method           string            `json:"method"`
	DBMode           string            `json:"db_mode"`
	Databases        []string          `json:"databases"`
	ExcludeTables    []string          `json:"exclude_tables"`
	Schedule         string            `json:"schedule"`
	IncrSchedule     string            `json:"incr_schedule"`
	Compression      string            `json:"compression"`
	CompressionLevel int               `json:"compression_level"`
	Encrypt          bool              `json:"encrypt"`
	Passphrase       string            `json:"passphrase"`
	Threads          int               `json:"threads"`
	Options          store.JobOptions  `json:"options"`
	Retention        store.Retention   `json:"retention"`
	Targets          []store.JobTarget `json:"targets"`
	Enabled          bool              `json:"enabled"`
	NotifySuccess    bool              `json:"notify_success"`
	NotifyFailure    bool              `json:"notify_failure"`
	TimeoutMinutes   int               `json:"timeout_minutes"`
}

func cleanList(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) applyJob(in *jobInput, j *store.Job, isAdmin bool) error {
	j.Name = strings.TrimSpace(in.Name)
	if j.Name == "" {
		return badRequest("job name is required")
	}
	sv, err := s.Store.GetServer(in.ServerID)
	if err != nil {
		return badRequest("server does not exist")
	}
	j.ServerID = in.ServerID
	switch in.Method {
	case store.MethodMysqldump, store.MethodMydumper, store.MethodPhysical:
	default:
		return badRequest("invalid backup method")
	}
	if in.Method == store.MethodPhysical && sv.WGPeerID != nil && *sv.WGPeerID > 0 {
		return badRequest("server %s is reached through WireGuard: physical backups need the backup tool on the database host — connect through an agent started with --allow-physical, or use mysqldump or mydumper", sv.Name)
	}
	j.Method = in.Method
	j.DBMode = in.DBMode
	switch j.DBMode {
	case "all", "include", "exclude":
	default:
		j.DBMode = "all"
	}
	j.Databases = cleanList(in.Databases)
	j.ExcludeTables = cleanList(in.ExcludeTables)
	if j.Method == store.MethodPhysical {
		j.DBMode = "all"
		j.Databases = []string{}
		j.ExcludeTables = []string{}
	}
	if j.DBMode == "include" && len(j.Databases) == 0 {
		return badRequest("no databases selected")
	}
	for _, e := range j.ExcludeTables {
		if !strings.Contains(e, ".") {
			return badRequest("excluded tables must be in database.table form: %s", e)
		}
	}
	if err := scheduler.Validate(in.Schedule); err != nil {
		return badRequest("full schedule: %v", err)
	}
	if err := scheduler.Validate(in.IncrSchedule); err != nil {
		return badRequest("incremental schedule: %v", err)
	}
	j.Schedule = strings.TrimSpace(in.Schedule)
	j.IncrSchedule = strings.TrimSpace(in.IncrSchedule)
	if j.Method != store.MethodPhysical {
		j.IncrSchedule = ""
	}
	j.Compression = pipeline.NormalizeCompression(in.Compression)
	j.CompressionLevel = in.CompressionLevel
	j.Encrypt = in.Encrypt
	if in.Passphrase != "" {
		if len(in.Passphrase) < 8 {
			return badRequest("the encryption passphrase must be at least 8 characters")
		}
		j.Passphrase = in.Passphrase
	}
	if j.Encrypt && j.Passphrase == "" {
		return badRequest("a passphrase is required when encryption is enabled")
	}
	j.Threads = in.Threads
	if j.Threads <= 0 {
		j.Threads = s.Store.GetSettings().DefaultThreads
	}
	if j.Threads > 64 {
		j.Threads = 64
	}
	// hooks and raw tool arguments execute as the service user (root)
	if !isAdmin && (in.Options.PreCommand != j.Options.PreCommand || in.Options.PostCommand != j.Options.PostCommand ||
		strings.TrimSpace(in.Options.ExtraArgs) != strings.TrimSpace(j.Options.ExtraArgs)) {
		return errf(403, "only admins can set pre/post commands and extra tool arguments")
	}
	j.Options = in.Options
	switch j.Options.VerifyMode {
	case "", store.VerifyOff, store.VerifyOne, store.VerifyAll:
	default:
		return badRequest("invalid verify mode (off, one or all)")
	}
	if j.ID == 0 && j.Options.VerifyMode == "" && !j.Options.VerifyAfter {
		j.Options.VerifyMode = store.VerifyOne // new jobs check one copy after every backup
	}
	j.Options.VerifyMode = j.Options.Verify()
	j.Options.VerifyAfter = j.Options.VerifyMode != store.VerifyOff
	if j.Options.BinlogArchive && j.Method == store.MethodMysqldump {
		j.Options.RecordBinlog = true // PITR replays from the position of every dump
	}
	j.Options.BinlogRotateMinutes = max(0, min(j.Options.BinlogRotateMinutes, 1440))
	j.Retention = in.Retention
	if len(in.Targets) == 0 {
		return badRequest("select at least one target storage")
	}
	seen := map[int64]bool{}
	j.Targets = nil
	for _, t := range in.Targets {
		if seen[t.StorageID] {
			continue
		}
		if _, err := s.Store.GetStorage(t.StorageID); err != nil {
			return badRequest("storage #%d does not exist", t.StorageID)
		}
		seen[t.StorageID] = true
		if t.Retention != nil && t.Retention.IsZero() {
			t.Retention = nil
		}
		j.Targets = append(j.Targets, store.JobTarget{StorageID: t.StorageID, Retention: t.Retention})
	}
	j.Enabled = in.Enabled
	j.NotifySuccess = in.NotifySuccess
	j.NotifyFailure = in.NotifyFailure
	j.TimeoutMinutes = in.TimeoutMinutes
	return nil
}

func (s *Server) jobView(j *store.Job) map[string]any {
	v := map[string]any{"job": j, "running": s.Runner.IsJobRunning(j.ID)}
	if lr := s.Store.LastRun(j.ID); lr != nil {
		v["last_run"] = map[string]any{"id": lr.ID, "status": lr.Status, "type": lr.Type, "started_at": lr.StartedAt,
			"finished_at": lr.FinishedAt, "size_bytes": lr.SizeBytes, "error": lr.Error}
	}
	if t, typ := s.Sched.NextRun(j.ID); !t.IsZero() {
		v["next_run"] = map[string]any{"at": t, "type": typ}
	}
	if list, n, err := s.Store.ListDrillRuns(store.DrillRunFilter{JobID: j.ID, Limit: 1}); err == nil && n > 0 {
		x := list[0]
		v["last_drill"] = map[string]any{"id": x.ID, "status": x.Status, "started_at": x.StartedAt, "summary": x.Summary, "error": x.Error}
	}
	return v
}

func (s *Server) jobsList(w http.ResponseWriter, r *http.Request) error {
	jobs, err := s.Store.ListJobs()
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(jobs))
	for i := range jobs {
		out = append(out, s.jobView(&jobs[i]))
	}
	return ok(w, out)
}

func (s *Server) jobGet(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	j, err := s.Store.GetJob(id)
	if err != nil {
		return err
	}
	return ok(w, s.jobView(j))
}

func (s *Server) jobCreate(w http.ResponseWriter, r *http.Request) error {
	var in jobInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	j := &store.Job{UUID: runner.NewUUID()}
	if err := s.applyJob(&in, j, userOf(r).Role == store.RoleAdmin); err != nil {
		return err
	}
	if err := s.Store.CreateJob(j); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("a job with this name already exists")
		}
		return err
	}
	s.Sched.Reload()
	s.Runner.SyncArchivers()
	s.audit(r, "job_create", "job:"+j.Name, j.Method)
	j, _ = s.Store.GetJob(j.ID)
	return ok(w, s.jobView(j))
}

func (s *Server) jobUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in jobInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	j, err := s.Store.GetJob(id)
	if err != nil {
		return err
	}
	if err := s.applyJob(&in, j, userOf(r).Role == store.RoleAdmin); err != nil {
		return err
	}
	if err := s.Store.UpdateJob(j); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("a job with this name already exists")
		}
		return err
	}
	s.Sched.Reload()
	s.Runner.SyncArchivers()
	s.audit(r, "job_update", "job:"+j.Name, "")
	j, _ = s.Store.GetJob(j.ID)
	return ok(w, s.jobView(j))
}

func (s *Server) jobDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	j, err := s.Store.GetJob(id)
	if err != nil {
		return err
	}
	if s.Runner.IsJobRunning(id) {
		return badRequest("the job is running — cancel it first")
	}
	deleted, deletedBinlogs := 0, 0
	var errs []string
	if r.URL.Query().Get("delete_backups") == "1" {
		s.Runner.StopArchiver(id)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		var runs []store.Run
		for off := 0; ; off += 1000 {
			page, _, err := s.Store.ListRuns(store.RunFilter{JobID: id, Limit: 1000, Offset: off})
			if err != nil || len(page) == 0 {
				break
			}
			runs = append(runs, page...)
		}
		for i := len(runs) - 1; i >= 0; i-- {
			if runs[i].Status == store.StatusDeleted {
				continue
			}
			if err := s.Runner.DeleteRun(ctx, runs[i].ID, 0, true); err != nil {
				errs = append(errs, err.Error())
			} else {
				deleted++
			}
		}
		n, err := s.Runner.DeleteJobBinlogs(ctx, j.UUID)
		if err != nil {
			errs = append(errs, err.Error())
		}
		deletedBinlogs = n
	}
	if err := s.Store.DeleteJob(id); err != nil {
		return err
	}
	s.Sched.Reload()
	s.Runner.SyncArchivers()
	s.audit(r, "job_delete", "job:"+j.Name, fmt.Sprintf("deleted_backups=%d deleted_binlogs=%d", deleted, deletedBinlogs))
	return ok(w, map[string]any{"ok": true, "deleted_backups": deleted, "deleted_binlogs": deletedBinlogs, "errors": errs})
}

func (s *Server) jobRun(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Type string `json:"type"`
	}
	_ = readJSON(r, &in)
	run, err := s.Runner.StartBackup(id, in.Type, "manual", userOf(r).Username)
	if err != nil {
		if errors.Is(err, runner.ErrJobRunning) {
			return errf(409, "the job is already running")
		}
		return err
	}
	s.audit(r, "job_run", "job:"+run.JobName, run.Type)
	return ok(w, run)
}

func (s *Server) jobToggle(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	j, err := s.Store.GetJob(id)
	if err != nil {
		return err
	}
	if err := s.Store.SetJobEnabled(id, !j.Enabled); err != nil {
		return err
	}
	s.Sched.Reload()
	s.Runner.SyncArchivers()
	s.audit(r, "job_toggle", "job:"+j.Name, fmt.Sprintf("enabled=%v", !j.Enabled))
	return ok(w, map[string]bool{"enabled": !j.Enabled})
}

func (s *Server) cronPreview(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Expr string `json:"expr"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Expr) == "" {
		return ok(w, map[string]any{"valid": true, "next": []time.Time{}})
	}
	if err := scheduler.Validate(in.Expr); err != nil {
		return ok(w, map[string]any{"valid": false, "error": err.Error()})
	}
	return ok(w, map[string]any{"valid": true, "next": scheduler.Next(in.Expr, 5, s.Runner.Location())})
}
