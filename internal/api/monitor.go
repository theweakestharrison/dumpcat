package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"dumpcat/internal/auth"
	"dumpcat/internal/monitor"
	"dumpcat/internal/notify"
	"dumpcat/internal/store"
)

// the bearer token of GET /metrics (hashed like API tokens)
const (
	metricsTokenHashKey   = "metrics_token_hash"
	metricsTokenPrefixKey = "metrics_token_prefix"
)

// ---- Prometheus metrics ----

// fromLocalhost: the request comes from this machine and not through a
// reverse proxy (which would make every visitor look local).
func fromLocalhost(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) metricsEndpoint(w http.ResponseWriter, r *http.Request) {
	plain := func(code int, msg string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		w.Write([]byte(msg + "\n"))
	}
	switch s.Store.GetSettings().MetricsAccess {
	case store.MetricsToken:
		hash, _ := s.Store.GetKV(metricsTokenHashKey)
		h := r.Header.Get("Authorization")
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if hash == "" || !strings.HasPrefix(h, "Bearer ") || !auth.ConstantTimeEqual(auth.SHA256Hex(tok), hash) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="DumpCat metrics"`)
			plain(http.StatusUnauthorized, "a valid bearer token is required (Settings → Monitoring)")
			return
		}
	case store.MetricsLocalhost:
		if !fromLocalhost(r) {
			plain(http.StatusForbidden, "metrics are only served to this machine (Settings → Monitoring)")
			return
		}
	default:
		plain(http.StatusNotFound, "metrics are turned off (Settings → Monitoring)")
		return
	}
	if s.Monitor == nil {
		plain(http.StatusServiceUnavailable, "monitoring is not running")
		return
	}
	src := monitor.MetricSources{Version: s.Version, Started: s.Started, DataDir: s.Cfg.DataDir}
	if s.Agents != nil {
		src.AgentOnline = func(id int64) bool { return s.Agents.Status(id).Online }
	}
	if s.WG != nil {
		src.WireGuard = func() (bool, map[int64]monitor.PeerState) {
			peers := map[int64]monitor.PeerState{}
			for id, st := range s.WG.Status() {
				peers[id] = monitor.PeerState{Online: st.Online, LastHandshake: st.LastHandshake}
			}
			return s.WG.State().Running, peers
		}
	}
	m := s.Monitor.Metrics(src)
	w.Header().Set("Content-Type", monitor.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	_ = m.Write(w)
}

// metricsTokenCreate replaces the bearer token of /metrics; it is shown once.
func (s *Server) metricsTokenCreate(w http.ResponseWriter, r *http.Request) error {
	raw := "dcm_" + auth.RandomToken(30)
	if err := s.Store.SetKV(metricsTokenHashKey, auth.SHA256Hex(raw)); err != nil {
		return err
	}
	if err := s.Store.SetKV(metricsTokenPrefixKey, raw[:10]); err != nil {
		return err
	}
	s.audit(r, "metrics_token", "settings", "")
	return ok(w, map[string]string{"token": raw, "prefix": raw[:10]})
}

// ---- weekly digest ----

func (s *Server) lastWeekDigest() (*notify.Digest, error) {
	if s.Monitor == nil {
		return nil, errors.New("monitoring is not running")
	}
	now := time.Now()
	return s.Monitor.BuildDigest(now.AddDate(0, 0, -7), now), nil
}

// channelDigest sends the digest of the last 7 days through a (maybe unsaved) email channel.
func (s *Server) channelDigest(w http.ResponseWriter, r *http.Request) error {
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
	d, err := s.lastWeekDigest()
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if err := s.Notifier.SendDigest(ctx, c, d); err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	s.audit(r, "digest_send", "channel:"+c.Name, "")
	return ok(w, map[string]any{"ok": true})
}

// digestPreview returns the email page of the digest of the last 7 days.
func (s *Server) digestPreview(w http.ResponseWriter, r *http.Request) error {
	d, err := s.lastWeekDigest()
	if err != nil {
		return badRequest("%v", err)
	}
	page, err := d.HTML(s.Notifier.Host)
	if err != nil {
		return err
	}
	page = strings.ReplaceAll(page, "cid:logo@dumpcat", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(notify.Logo()))
	return ok(w, map[string]string{"html": page})
}
