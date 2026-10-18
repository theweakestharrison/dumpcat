package api

import (
	"errors"
	"net/http"

	"dumpcat/internal/update"
)

func (s *Server) updater() (*update.Manager, error) {
	if s.Updater == nil {
		return nil, errf(503, "updates are not available")
	}
	return s.Updater, nil
}

// optional JSON body: {"url": "...", "force": true}
type updateReq struct {
	URL   string `json:"url"`
	Force bool   `json:"force"`
}

func readUpdateReq(r *http.Request) (updateReq, error) {
	var in updateReq
	if r.ContentLength != 0 {
		if err := readJSON(r, &in); err != nil {
			return in, err
		}
	}
	if in.URL != "" {
		if _, err := update.ManifestURL(in.URL); err != nil {
			return in, badRequest("%v", err)
		}
	}
	return in, nil
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) error {
	m, err := s.updater()
	if err != nil {
		return err
	}
	return ok(w, m.Status())
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) error {
	m, err := s.updater()
	if err != nil {
		return err
	}
	in, err := readUpdateReq(r)
	if err != nil {
		return err
	}
	// the result (or the error) is part of the status
	_, _ = m.Check(r.Context(), in.URL)
	return ok(w, m.Status())
}

func (s *Server) updateInstall(w http.ResponseWriter, r *http.Request) error {
	m, err := s.updater()
	if err != nil {
		return err
	}
	in, err := readUpdateReq(r)
	if err != nil {
		return err
	}
	if err := m.Install(in.URL, in.Force, userOf(r).Username); err != nil {
		if errors.Is(err, update.ErrBusy) {
			return errf(409, "%v", err)
		}
		return badRequest("%v", err)
	}
	s.audit(r, "update_install", "system", in.URL)
	return ok(w, m.Status())
}

func (s *Server) updateCancel(w http.ResponseWriter, r *http.Request) error {
	m, err := s.updater()
	if err != nil {
		return err
	}
	if !m.Cancel() {
		return errf(409, "the update can no longer be cancelled")
	}
	s.audit(r, "update_cancel", "system", "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) error {
	m, err := s.updater()
	if err != nil {
		return err
	}
	if err := m.Rollback(userOf(r).Username); err != nil {
		if errors.Is(err, update.ErrBusy) {
			return errf(409, "%v", err)
		}
		return badRequest("%v", err)
	}
	s.audit(r, "update_rollback", "system", "")
	return ok(w, m.Status())
}
