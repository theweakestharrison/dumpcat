package api

import (
	"fmt"
	"net/http"

	"dumpcat/internal/runner"
	"dumpcat/internal/store"
)

// binlogOverview lists jobs that archive binlogs (or still have archived files).
func (s *Server) binlogOverview(w http.ResponseWriter, r *http.Request) error {
	jobs, err := s.Store.ListJobs()
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for i := range jobs {
		j := &jobs[i]
		stats := s.Store.BinlogStats(j.UUID)
		if !j.Options.BinlogArchive && stats.Files == 0 {
			continue
		}
		item := map[string]any{
			"job_id": j.ID, "job_name": j.Name, "server_name": j.ServerName, "method": j.Method, "enabled": j.Enabled,
			"archive": j.Options.BinlogArchive, "rotate_minutes": j.Options.BinlogRotateMinutes, "stats": stats,
		}
		if st, ok := s.Runner.ArchiverStatus(j.ID); ok {
			item["status"] = st
		}
		out = append(out, item)
	}
	return ok(w, out)
}

func (s *Server) jobBinlogs(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	j, err := s.Store.GetJob(id)
	if err != nil {
		return err
	}
	items, total, err := s.Store.ListBinlogPage(j.UUID, queryInt(r, "limit", 100), queryInt(r, "offset", 0))
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.BinlogFile{}
	}
	return ok(w, map[string]any{"items": items, "total": total})
}

func (s *Server) jobBinlogLog(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	return s.sendLog(w, r, s.Runner.BinlogLogPath(id))
}

func (s *Server) jobBinlogRotate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.Runner.RotateBinlog(id); err != nil {
		return badRequest("%v", err)
	}
	s.audit(r, "binlog_rotate", fmt.Sprintf("job:%d", id), "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) jobBinlogRestart(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.Runner.RestartArchiver(id); err != nil {
		return badRequest("%v", err)
	}
	s.audit(r, "binlog_restart", fmt.Sprintf("job:%d", id), "")
	return ok(w, map[string]bool{"ok": true})
}

// pitrPlan picks the base backup for a recovery target and checks that the
// archived binlogs reach it (nothing is changed).
func (s *Server) pitrPlan(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		JobID  int64             `json:"job_id"`
		RunID  int64             `json:"run_id"`
		Target runner.PITRTarget `json:"target"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.JobID <= 0 && in.RunID <= 0 {
		return badRequest("job_id or run_id is required")
	}
	plan, err := s.Runner.PlanPITR(in.JobID, in.RunID, in.Target)
	if err != nil {
		return badRequest("%v", err)
	}
	return ok(w, plan)
}
