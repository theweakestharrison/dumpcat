package api

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

func (s *Server) baseURL() string {
	if b := strings.TrimRight(s.Store.GetSettings().BaseURL, "/"); b != "" {
		return b
	}
	return s.Cfg.BaseURL
}

func (s *Server) storageTypes(w http.ResponseWriter, r *http.Request) error {
	return ok(w, map[string]any{"types": storage.Types, "oauth_callback": s.oauthCallbackURL()})
}

func (s *Server) oauthCallbackURL() string {
	if b := s.baseURL(); b != "" {
		return b + "/api/oauth/callback"
	}
	return ""
}

func typeLabel(t string) string {
	if td, ok := storage.GetType(t); ok {
		return td.Label
	}
	return t
}

func (s *Server) storageView(st *store.Storage, withConfig bool) map[string]any {
	v := map[string]any{
		"id": st.ID, "name": st.Name, "type": st.Type, "type_label": typeLabel(st.Type),
		"created_at": st.CreatedAt, "updated_at": st.UpdatedAt,
		"last_test_at": st.LastTestAt, "last_test_ok": st.LastTestOK, "last_test_msg": st.LastTestMsg,
	}
	if b, err := s.Storage.Get(st); err == nil {
		v["describe"] = b.Describe()
	} else {
		v["describe"] = ""
		v["config_error"] = err.Error()
	}
	if withConfig {
		v["config"] = storage.Masked(st.Type, st.Config)
	}
	inUse, _ := s.Store.StorageInUse(st.ID)
	if inUse == nil {
		inUse = []string{}
	}
	v["in_use"] = inUse
	return v
}

func (s *Server) storagesList(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListStorages()
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, s.storageView(&list[i], false))
	}
	return ok(w, out)
}

func (s *Server) storageGet(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	st, err := s.Store.GetStorage(id)
	if err != nil {
		return err
	}
	return ok(w, s.storageView(st, true))
}

type storageInput struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
}

// privilegedType reports storage types that write to arbitrary paths of the
// (root) host filesystem; only admins may configure them.
func privilegedType(t string) bool { return t == "local" || t == "rclone" }

// buildStorage merges secrets with an existing storage and validates.
func (s *Server) buildStorage(ctx context.Context, in storageInput, existing *store.Storage) (*store.Storage, error) {
	if _, ok := storage.GetType(in.Type); !ok {
		return nil, badRequest("invalid storage type")
	}
	if u := userFromCtx(ctx); u != nil && u.Role != store.RoleAdmin && (privilegedType(in.Type) || (existing != nil && privilegedType(existing.Type))) {
		return nil, errf(403, "only admins can configure local-directory or custom rclone storage")
	}
	st := &store.Storage{Name: strings.TrimSpace(in.Name), Type: in.Type}
	if existing != nil {
		*st = *existing
		st.Name = strings.TrimSpace(in.Name)
		if st.Type != in.Type {
			existing = nil
			st.Type = in.Type
		}
	}
	old := map[string]string{}
	if existing != nil {
		old = existing.Config
	}
	if existing != nil {
		if err := storage.RequireSecretsOnMove(in.Type, in.Config, old); err != nil {
			return nil, badRequest("%v", err)
		}
	}
	cfg := storage.MergeSecrets(in.Type, in.Config, old)
	for k, v := range cfg {
		if k == "token" {
			cfg[k] = storage.NormalizeToken(v)
		} else if k != "config" && k != "key_pem" && k != "service_account_credentials" {
			cfg[k] = strings.TrimSpace(v)
		}
	}
	// S3 without keys signs in with the host's own cloud identity (instance role, environment,
	// ~/.aws): that reaches every bucket the host may use, so only admins may choose it
	if in.Type == "s3" && cfg["access_key_id"] == "" {
		if u := userFromCtx(ctx); u != nil && u.Role != store.RoleAdmin {
			return nil, errf(403, "only admins can configure S3 storage without an access key (it uses the host's own cloud credentials)")
		}
	}
	if st.Name == "" {
		return nil, badRequest("storage name is required")
	}
	if err := storage.Validate(in.Type, cfg); err != nil {
		return nil, badRequest("%v", err)
	}
	if in.Type == "onedrive" && cfg["drive_id"] == "" {
		if at := storage.AccessTokenFrom(cfg["token"]); at != "" {
			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if id, dt, _, err := storage.DiscoverOneDrive(dctx, at); err == nil {
				cfg["drive_id"], cfg["drive_type"] = id, dt
			}
			cancel()
		}
		if cfg["drive_id"] == "" {
			return nil, badRequest("could not determine the OneDrive Drive ID — authorize again or enter the Drive ID")
		}
	}
	st.Config = cfg
	return st, nil
}

func (s *Server) storageCreate(w http.ResponseWriter, r *http.Request) error {
	var in storageInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	st, err := s.buildStorage(r.Context(), in, nil)
	if err != nil {
		return err
	}
	if err := s.Store.CreateStorage(st); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("a storage with this name already exists")
		}
		return err
	}
	s.audit(r, "storage_create", "storage:"+st.Name, st.Type)
	return ok(w, s.storageView(st, true))
}

func (s *Server) storageUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in storageInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	existing, err := s.Store.GetStorage(id)
	if err != nil {
		return err
	}
	st, err := s.buildStorage(r.Context(), in, existing)
	if err != nil {
		return err
	}
	st.ID = id
	if err := s.Store.UpdateStorage(st); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("a storage with this name already exists")
		}
		return err
	}
	s.audit(r, "storage_update", "storage:"+st.Name, st.Type)
	return ok(w, s.storageView(st, true))
}

func (s *Server) storageDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	st, err := s.Store.GetStorage(id)
	if err != nil {
		return err
	}
	if jobs, _ := s.Store.StorageInUse(id); len(jobs) > 0 {
		return badRequest("storage is in use by job(s): %s", strings.Join(jobs, ", "))
	}
	if err := s.Store.DeleteStorage(id); err != nil {
		return err
	}
	s.audit(r, "storage_delete", "storage:"+st.Name, "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) testStorage(ctx context.Context, st *store.Storage) (string, error) {
	b, err := s.Storage.Get(st)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return b.Test(ctx)
}

func (s *Server) storageTest(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	st, err := s.Store.GetStorage(id)
	if err != nil {
		return err
	}
	msg, err := s.testStorage(r.Context(), st)
	if err != nil {
		s.Store.UpdateStorageTest(id, false, err.Error())
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	s.Store.UpdateStorageTest(id, true, msg)
	return ok(w, map[string]any{"ok": true, "message": msg})
}

func (s *Server) storageTestUnsaved(w http.ResponseWriter, r *http.Request) error {
	var in storageInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	var existing *store.Storage
	if in.ID > 0 {
		existing, _ = s.Store.GetStorage(in.ID)
	}
	if strings.TrimSpace(in.Name) == "" {
		in.Name = "test"
	}
	st, err := s.buildStorage(r.Context(), in, existing)
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	msg, err := s.testStorage(r.Context(), st)
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	return ok(w, map[string]any{"ok": true, "message": msg})
}

func (s *Server) storageBrowse(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	st, err := s.Store.GetStorage(id)
	if err != nil {
		return err
	}
	b, err := s.Storage.Get(st)
	if err != nil {
		return badRequest("%v", err)
	}
	rel, err := storage.CleanRel(r.URL.Query().Get("path"))
	if err != nil {
		return badRequest("%v", err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	entries, err := b.List(ctx, rel)
	if err != nil {
		return badRequest("%v", err)
	}
	if entries == nil {
		entries = []storage.Entry{}
	}
	return ok(w, map[string]any{"path": rel, "entries": entries})
}

func (s *Server) storageUsage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	st, err := s.Store.GetStorage(id)
	if err != nil {
		return err
	}
	b, err := s.Storage.Get(st)
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	u, err := b.About(ctx)
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": "this storage does not report usage"})
	}
	return ok(w, map[string]any{"ok": true, "usage": u})
}

func (s *Server) storageImport(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.Runner.ImportFromStorage(ctx, id)
	if err != nil {
		return badRequest("%v", err)
	}
	s.audit(r, "storage_import", fmt.Sprintf("storage:%d", id), fmt.Sprintf("imported=%d linked=%d", res.Imported, res.Linked))
	return ok(w, res)
}

// ---- OAuth ----

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Type      string            `json:"type"`
		Config    map[string]string `json:"config"`
		StorageID int64             `json:"storage_id"`
		Mode      string            `json:"mode"` // callback | manual
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	cfg := in.Config
	if in.StorageID > 0 {
		if ex, err := s.Store.GetStorage(in.StorageID); err == nil && ex.Type == in.Type {
			cfg = storage.MergeSecrets(in.Type, in.Config, ex.Config)
		}
	}
	redirect := ""
	if in.Mode == "callback" {
		redirect = s.oauthCallbackURL()
		if redirect == "" {
			return badRequest("no Base URL configured (Settings), so the callback can't be used — use the paste-URL mode instead")
		}
	}
	u, state, used, err := s.OAuth.Start(in.Type, cfg, redirect, userOf(r).ID)
	if err != nil {
		return badRequest("%v", err)
	}
	return ok(w, map[string]any{"auth_url": u, "state": state, "redirect_uri": used})
}

func (s *Server) oauthExchange(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		State string `json:"state"`
		Input string `json:"input"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	code, state, err := storage.ParseCallback(in.Input)
	if err != nil {
		return badRequest("%v", err)
	}
	if state != "" && state != in.State {
		return badRequest("state mismatch — paste the URL from this authorization attempt")
	}
	res, err := s.OAuth.Exchange(r.Context(), in.State, code, userOf(r).ID)
	if err != nil {
		return badRequest("%v", err)
	}
	return ok(w, res)
}

func (s *Server) oauthResult(w http.ResponseWriter, r *http.Request) error {
	res, msg, done := s.OAuth.Result(r.URL.Query().Get("state"), userOf(r).ID)
	return ok(w, map[string]any{"done": done, "result": res, "error": msg})
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	msg := "Authorization succeeded. You can close this tab and return to DumpCat."
	if e := q.Get("error"); e != "" {
		msg = "Authorization failed: " + e + " " + q.Get("error_description")
	} else if _, err := s.OAuth.Exchange(r.Context(), q.Get("state"), q.Get("code"), userOf(r).ID); err != nil {
		msg = "Authorization failed: " + err.Error()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>DumpCat OAuth</title><link rel="stylesheet" href="/assets/app.css"></head><body class="oauth-done"><div class="card"><h2>DumpCat</h2><p>%s</p></div></body></html>`, html.EscapeString(msg))
	return nil
}
