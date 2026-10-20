package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"dumpcat/internal/drivers"
	"dumpcat/internal/runner"
	"dumpcat/internal/scheduler"
	"dumpcat/internal/store"
)

// ---- restore drills ----

type drillInput struct {
	Name           string                 `json:"name"`
	JobID          int64                  `json:"job_id"`
	TargetServerID int64                  `json:"target_server_id"`
	Schedule       string                 `json:"schedule"`
	Enabled        bool                   `json:"enabled"`
	Databases      []string               `json:"databases"`
	Assertions     []store.DrillAssertion `json:"assertions"`
	Options        store.DrillOptions     `json:"options"`
	NotifySuccess  bool                   `json:"notify_success"`
	NotifyFailure  bool                   `json:"notify_failure"`
}

func (s *Server) applyDrill(in *drillInput, d *store.Drill) error {
	d.Name = strings.TrimSpace(in.Name)
	if d.Name == "" {
		return badRequest("drill name is required")
	}
	if len([]rune(d.Name)) > 100 {
		return badRequest("the drill name is too long (max 100 characters)")
	}
	job, err := s.Store.GetJob(in.JobID)
	if err != nil {
		return badRequest("job does not exist")
	}
	if p := runner.CheckDrillJob(job); p != "" {
		return badRequest("%s", p)
	}
	if _, err := s.Store.GetServer(in.TargetServerID); err != nil {
		return badRequest("sandbox server does not exist")
	}
	if err := scheduler.Validate(in.Schedule); err != nil {
		return badRequest("schedule: %v", err)
	}
	d.JobID, d.TargetServerID = in.JobID, in.TargetServerID
	d.Schedule = strings.TrimSpace(in.Schedule)
	d.Enabled = in.Enabled
	d.Databases = cleanList(in.Databases)
	if len(in.Assertions) > 50 {
		return badRequest("too many assertions (max 50)")
	}
	d.Assertions = []store.DrillAssertion{}
	for i, a := range in.Assertions {
		a.Name = strings.TrimSpace(a.Name)
		a.Database = strings.TrimSpace(a.Database)
		a.SQL = strings.TrimSpace(a.SQL)
		if a.SQL == "" && a.Name == "" {
			continue
		}
		if a.Name == "" {
			a.Name = fmt.Sprintf("Assertion %d", i+1)
		}
		if err := runner.ValidateAssertion(a.SQL); err != nil {
			return badRequest("assertion %q: %v", a.Name, err)
		}
		if len(a.SQL) > 64*1024 {
			return badRequest("assertion %q is too long", a.Name)
		}
		d.Assertions = append(d.Assertions, a)
	}
	d.Options = in.Options
	switch d.Options.RowCounts {
	case "", store.DrillRowsEstimate:
	default:
		return badRequest("invalid row count mode (exact or estimate)")
	}
	if m, _, ok := drivers.Method(job.Method); ok && !m.Estimates {
		d.Options.RowCounts = "" // only engines that keep row estimates
	}
	if sv, err := s.Store.GetServer(in.TargetServerID); err == nil {
		if js, err := s.Store.GetServer(job.ServerID); err == nil && js.EngineName() != sv.EngineName() {
			return badRequest("the sandbox %s is not a %s server like the job's", sv.Name, drivers.ForServer(js).Product())
		}
	}
	d.Options.Threads = max(0, min(d.Options.Threads, 32))
	d.Options.MaxRTOMinutes = max(0, d.Options.MaxRTOMinutes)
	d.NotifySuccess, d.NotifyFailure = in.NotifySuccess, in.NotifyFailure
	return nil
}

func (s *Server) drillView(d *store.Drill) map[string]any {
	v := map[string]any{"drill": d, "running": s.Runner.DrillRunning(d.ID)}
	if x := s.Store.LastDrillRun(d.ID); x != nil {
		v["last_run"] = map[string]any{"id": x.ID, "status": x.Status, "run_id": x.RunID, "run_dir": x.RunDir, "started_at": x.StartedAt,
			"finished_at": x.FinishedAt, "restore_ms": x.RestoreMs, "summary": x.Summary, "error": x.Error}
	}
	if t := s.Sched.NextDrill(d.ID); !t.IsZero() && d.Enabled {
		v["next_at"] = t
	}
	if job, err := s.Store.GetJob(d.JobID); err == nil {
		if sv, err := s.Store.GetServer(d.TargetServerID); err == nil {
			v["same_server"] = s.Runner.DrillOnSource(job, sv)
		}
		if p := runner.CheckDrillJob(job); p != "" {
			v["problem"] = p
		}
	}
	return v
}

func (s *Server) drillsList(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListDrills(int64(queryInt(r, "job_id", 0)))
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, s.drillView(&list[i]))
	}
	return ok(w, out)
}

func (s *Server) drillGet(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	d, err := s.Store.GetDrill(id)
	if err != nil {
		return err
	}
	return ok(w, s.drillView(d))
}

func (s *Server) drillCreate(w http.ResponseWriter, r *http.Request) error {
	var in drillInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	d := &store.Drill{}
	if err := s.applyDrill(&in, d); err != nil {
		return err
	}
	if err := s.Store.CreateDrill(d); err != nil {
		return err
	}
	s.Sched.Reload()
	s.audit(r, "drill_create", "drill:"+d.Name, fmt.Sprintf("job:%d sandbox:%d", d.JobID, d.TargetServerID))
	d, _ = s.Store.GetDrill(d.ID)
	return ok(w, s.drillView(d))
}

func (s *Server) drillUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	d, err := s.Store.GetDrill(id)
	if err != nil {
		return err
	}
	var in drillInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := s.applyDrill(&in, d); err != nil {
		return err
	}
	if err := s.Store.UpdateDrill(d); err != nil {
		return err
	}
	s.Sched.Reload()
	s.audit(r, "drill_update", "drill:"+d.Name, "")
	d, _ = s.Store.GetDrill(d.ID)
	return ok(w, s.drillView(d))
}

func (s *Server) drillDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	d, err := s.Store.GetDrill(id)
	if err != nil {
		return err
	}
	if s.Runner.DrillRunning(id) {
		return badRequest("the drill is running — cancel it first")
	}
	if err := s.Store.DeleteDrill(id); err != nil {
		return err
	}
	s.Sched.Reload()
	s.audit(r, "drill_delete", "drill:"+d.Name, "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) drillRun(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		RunID int64 `json:"run_id"`
	}
	_ = readJSON(r, &in)
	x, err := s.Runner.StartDrill(id, in.RunID, "manual", userOf(r).Username)
	if err != nil {
		if errors.Is(err, runner.ErrDrillRunning) {
			return errf(409, "%v", err)
		}
		if errors.Is(err, runner.ErrUpdating) || errors.Is(err, store.ErrNotFound) {
			return err
		}
		return badRequest("%v", err)
	}
	s.audit(r, "drill_run", "drill:"+x.DrillName, fmt.Sprintf("run:%d", *x.RunID))
	return ok(w, x)
}

func (s *Server) drillRunsList(w http.ResponseWriter, r *http.Request) error {
	list, total, err := s.Store.ListDrillRuns(store.DrillRunFilter{
		DrillID: int64(queryInt(r, "drill_id", 0)), JobID: int64(queryInt(r, "job_id", 0)), RunID: int64(queryInt(r, "run_id", 0)),
		Limit: queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	})
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"items": list, "total": total})
}

func (s *Server) drillRunGet(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	x, err := s.Store.GetDrillRun(id)
	if err != nil {
		return err
	}
	out := map[string]any{"drill_run": x}
	if t, ok := s.Runner.Task(fmt.Sprintf("drill:%d", id)); ok {
		out["task"] = t
	}
	return ok(w, out)
}

func (s *Server) drillRunLog(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	return s.sendLog(w, r, s.Runner.DrillLogPath(id))
}

func (s *Server) drillRunCancel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if !s.Runner.Cancel(fmt.Sprintf("drill:%d", id)) {
		return badRequest("the drill is no longer running")
	}
	s.audit(r, "drill_cancel", fmt.Sprintf("drill_run:%d", id), "")
	return ok(w, map[string]bool{"ok": true})
}
