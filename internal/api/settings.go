package api

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"dumpcat/internal/notify"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/sysinfo"
	"dumpcat/internal/update"
)

// ---- notification channels ----

func (s *Server) notifyTypes(w http.ResponseWriter, r *http.Request) error {
	return ok(w, notify.Types)
}

func channelView(c *store.NotifyChannel) map[string]any {
	return map[string]any{
		"id": c.ID, "name": c.Name, "type": c.Type, "enabled": c.Enabled, "on_success": c.OnSuccess, "on_failure": c.OnFailure,
		"created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "config": maskWith(notify.SecretKeys(c.Type), c.Config),
	}
}

func maskWith(sec map[string]bool, cfg map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range cfg {
		if sec[k] && v != "" {
			out[k] = storage.SecretMask
		} else {
			out[k] = v
		}
	}
	return out
}

func (s *Server) channelsList(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListChannels()
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, channelView(&list[i]))
	}
	return ok(w, out)
}

type channelInput struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Config    map[string]string `json:"config"`
	Enabled   bool              `json:"enabled"`
	OnSuccess bool              `json:"on_success"`
	OnFailure bool              `json:"on_failure"`
}

func (s *Server) buildChannel(in channelInput, existing *store.NotifyChannel) (*store.NotifyChannel, error) {
	td, ok := notify.GetType(in.Type)
	if !ok {
		return nil, badRequest("invalid channel type")
	}
	c := &store.NotifyChannel{}
	old := map[string]string{}
	if existing != nil {
		*c = *existing
		if existing.Type == in.Type {
			old = existing.Config
		}
	}
	sec := notify.SecretKeys(in.Type)
	if existing != nil && existing.Type == in.Type {
		if err := storage.RequireSecretsOnMove(in.Type, in.Config, old); err != nil {
			return nil, badRequest("%v", err)
		}
	}
	cfg := map[string]string{}
	for k, v := range in.Config {
		if sec[k] && v == storage.SecretMask {
			cfg[k] = old[k]
		} else {
			cfg[k] = strings.TrimSpace(v)
		}
	}
	for _, f := range td.Fields {
		if f.Required && cfg[f.Key] == "" {
			return nil, badRequest("missing field: %s", f.Label)
		}
	}
	c.Name = strings.TrimSpace(in.Name)
	if c.Name == "" {
		c.Name = td.Label
	}
	c.Type, c.Config, c.Enabled, c.OnSuccess, c.OnFailure = in.Type, cfg, in.Enabled, in.OnSuccess, in.OnFailure
	return c, nil
}

func (s *Server) channelSave(w http.ResponseWriter, r *http.Request) error {
	var in channelInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	var existing *store.NotifyChannel
	if r.PathValue("id") != "" {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		existing, err = s.Store.GetChannel(id)
		if err != nil {
			return err
		}
	}
	c, err := s.buildChannel(in, existing)
	if err != nil {
		return err
	}
	if err := s.Store.SaveChannel(c); err != nil {
		return err
	}
	s.audit(r, "notify_save", "channel:"+c.Name, c.Type)
	return ok(w, channelView(c))
}

func (s *Server) channelDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.Store.DeleteChannel(id); err != nil {
		return err
	}
	s.audit(r, "notify_delete", "channel:"+r.PathValue("id"), "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) channelTest(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	c, err := s.Store.GetChannel(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if err := s.Notifier.Test(ctx, c); err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	return ok(w, map[string]any{"ok": true})
}

func (s *Server) channelTestUnsaved(w http.ResponseWriter, r *http.Request) error {
	var in channelInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	var existing *store.NotifyChannel
	if in.ID > 0 {
		existing, _ = s.Store.GetChannel(in.ID)
	}
	c, err := s.buildChannel(in, existing)
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if err := s.Notifier.Test(ctx, c); err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	return ok(w, map[string]any{"ok": true})
}

// ---- settings ----

// settingsView adds what is not stored in the settings themselves.
type settingsView struct {
	store.Settings
	MetricsTokenPrefix string `json:"metrics_token_prefix"` // "" = no token yet
}

func (s *Server) settingsView(st store.Settings) settingsView {
	prefix, _ := s.Store.GetKV(metricsTokenPrefixKey)
	return settingsView{Settings: st, MetricsTokenPrefix: prefix}
}

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.settingsView(s.Store.GetSettings()))
}

func (s *Server) settingsPut(w http.ResponseWriter, r *http.Request) error {
	// fields a client does not send keep their value
	in := s.Store.GetSettings()
	in.ToolPaths = nil
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Timezone != "" {
		if _, err := time.LoadLocation(in.Timezone); err != nil {
			return badRequest("invalid timezone: %s", in.Timezone)
		}
	}
	if in.MaxConcurrent <= 0 || in.MaxConcurrent > 32 {
		return badRequest("max concurrent jobs must be between 1 and 32")
	}
	if in.SessionHours <= 0 || in.SessionHours > 720 {
		return badRequest("session timeout must be 1-720 hours")
	}
	switch in.MetricsAccess {
	case store.MetricsOff, store.MetricsToken, store.MetricsLocalhost:
	default:
		return badRequest("metrics access must be off, token or localhost")
	}
	if in.StaleGraceMinutes < 0 || in.StaleGraceMinutes > 10080 {
		return badRequest("the grace period must be 0-10080 minutes")
	}
	if in.BinlogLagMinutes < 0 || in.BinlogLagMinutes > 10080 {
		return badRequest("the binlog lag threshold must be 0-10080 minutes")
	}
	if in.DigestWeekday < 0 || in.DigestWeekday > 6 || in.DigestHour < 0 || in.DigestHour > 23 {
		return badRequest("invalid day or hour for the weekly digest")
	}
	in.InstanceName = strings.TrimSpace(in.InstanceName)
	if err := checkInstanceName(in.InstanceName); err != nil {
		return err
	}
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	in.UpdateURL = strings.TrimSpace(in.UpdateURL)
	if in.UpdateURL != "" {
		if _, err := update.ManifestURL(in.UpdateURL); err != nil {
			return badRequest("release server: %v", err)
		}
	}
	in.StagingDir = strings.TrimSpace(in.StagingDir)
	if in.StagingDir != "" {
		if err := os.MkdirAll(in.StagingDir, 0o750); err != nil {
			return badRequest("could not create the staging directory: %v", err)
		}
	}
	if in.ToolPaths == nil {
		in.ToolPaths = map[string]string{}
	}
	for k, v := range in.ToolPaths {
		v = strings.TrimSpace(v)
		if v == "" {
			delete(in.ToolPaths, k)
			continue
		}
		if st, err := os.Stat(v); err != nil || st.IsDir() {
			return badRequest("tool path for %s does not exist: %s", k, v)
		}
		in.ToolPaths[k] = v
	}
	old := s.Store.GetSettings()
	if err := s.Store.SaveSettings(in); err != nil {
		return err
	}
	s.allowIndexing.Store(in.AllowIndexing)
	s.Tools.Refresh()
	if old.Timezone != in.Timezone {
		s.Sched.Reload()
	}
	s.audit(r, "settings_update", "settings", "")
	return ok(w, s.settingsView(in))
}

// ---- system ----

func (s *Server) system(w http.ResponseWriter, r *http.Request) error {
	tools := s.Tools.All()
	host, _ := os.Hostname()
	out := map[string]any{
		"version": s.Version, "go": runtime.Version(), "os": runtime.GOOS + "/" + runtime.GOARCH, "hostname": host,
		"data_dir": s.Cfg.DataDir, "tools": tools, "listen": s.Cfg.Listen, "base_url": s.baseURL(),
		"tls": s.Cfg.TLSEnabled(), "cpus": runtime.NumCPU(), "uid": os.Getuid(),
	}
	if t, f, err := sysinfo.DiskUsage(s.Cfg.DataDir); err == nil {
		out["data_disk"] = map[string]int64{"total": t, "free": f}
	}
	staging := s.Store.GetSettings().StagingDir
	if staging == "" {
		staging = s.Cfg.Path("staging")
	}
	out["staging_dir"] = staging
	_ = os.MkdirAll(staging, 0o750)
	if t, f, err := sysinfo.DiskUsage(staging); err == nil {
		out["staging_disk"] = map[string]int64{"total": t, "free": f}
	}
	return ok(w, out)
}

func (s *Server) toolsRefresh(w http.ResponseWriter, r *http.Request) error {
	s.Tools.Refresh()
	return ok(w, s.Tools.All())
}

func (s *Server) auditList(w http.ResponseWriter, r *http.Request) error {
	list, total, err := s.Store.ListAudit(queryInt(r, "limit", 100), queryInt(r, "offset", 0), strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		return err
	}
	if list == nil {
		list = []store.AuditEntry{}
	}
	return ok(w, map[string]any{"items": list, "total": total})
}
