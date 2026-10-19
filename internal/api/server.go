// Package api exposes the JSON REST API and serves the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/agent"
	"dumpcat/internal/auth"
	"dumpcat/internal/config"
	"dumpcat/internal/engine"
	"dumpcat/internal/notify"
	"dumpcat/internal/runner"
	"dumpcat/internal/scheduler"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/update"
	"dumpcat/internal/wg"
)

type Server struct {
	Cfg      *config.Config
	Store    *store.Store
	Runner   *runner.Runner
	Sched    *scheduler.Scheduler
	Storage  *storage.Manager
	OAuth    *storage.OAuthManager
	Tools    *engine.Tools
	Notifier *notify.Notifier
	Limiter  *auth.Limiter // per-username login failures
	IPLimit  *auth.Limiter // per-IP login failures (higher threshold, shared NAT)
	Version  string
	WebFS    fs.FS
	Updater  *update.Manager
	Agents   *agent.Manager // database hosts that connect out (nil = disabled)
	WG       *wg.Manager    // built-in WireGuard (nil = not available)
	wgMu     sync.Mutex     // serializes WireGuard configuration changes
	// ControlToken authenticates "dumpcat update" on the same machine (read from DATA_DIR/control.token).
	ControlToken string

	// one-time token required to create the first admin (set by PrepareSetup, guarded by setupMu)
	setupToken     string // normalized; "" = no setup possible
	setupTokenPath string
	setupLimit     *auth.Limiter // per-IP wrong setup tokens
}

const sessionCookie = "dumpcat_session"

type ctxKey int

const authKey ctxKey = 1

type authInfo struct {
	User    *store.User
	Session *store.Session
	Token   *store.APIToken
}

func authFrom(r *http.Request) *authInfo {
	a, _ := r.Context().Value(authKey).(*authInfo)
	return a
}

func userFromCtx(ctx context.Context) *store.User {
	if a, _ := ctx.Value(authKey).(*authInfo); a != nil {
		return a.User
	}
	return nil
}

func userOf(r *http.Request) *store.User {
	if a := authFrom(r); a != nil {
		return a.User
	}
	return nil
}

type httpError struct {
	Code int
	Msg  string
}

func (e *httpError) Error() string { return e.Msg }

func errf(code int, format string, args ...any) error {
	return &httpError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error { return errf(400, format, args...) }

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func ok(w http.ResponseWriter, v any) error {
	writeJSON(w, 200, v)
	return nil
}

func readJSON(r *http.Request, v any) error {
	// a cross-site form cannot send application/json without a CORS preflight
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return errf(415, "Content-Type must be application/json")
	}
	body := http.MaxBytesReader(nil, r.Body, 4<<20)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return badRequest("missing JSON body")
		}
		return badRequest("invalid JSON: %v", err)
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || v <= 0 {
		return 0, badRequest("invalid id")
	}
	return v, nil
}

func queryInt(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return def
}

func (s *Server) clientIP(r *http.Request) string {
	if s.Cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if s.Cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
}

func (s *Server) audit(r *http.Request, action, target string, details string) {
	u := userOf(r)
	var uid *int64
	name := ""
	if u != nil {
		name = u.Username
		if u.ID > 0 {
			id := u.ID
			uid = &id
		}
	}
	s.Store.Audit(uid, name, action, target, details, s.clientIP(r))
}

// route registers a handler requiring minRole ("" = public).
func (s *Server) route(mux *http.ServeMux, pattern, minRole string, h handlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic %s %s: %v", r.Method, r.URL.Path, rec)
				writeJSON(w, 500, map[string]string{"error": "internal error"})
			}
		}()
		if minRole != "" {
			a, err := s.authenticate(w, r)
			if err != nil {
				s.writeErr(w, err)
				return
			}
			if store.RoleLevel(a.User.Role) < store.RoleLevel(minRole) {
				s.writeErr(w, errf(403, "you don't have permission to do this (requires role %s)", minRole))
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), authKey, a))
		}
		if err := h(w, r); err != nil {
			s.writeErr(w, err)
		}
	})
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeJSON(w, he.Code, map[string]string{"error": he.Msg})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	if errors.Is(err, runner.ErrUpdating) {
		writeJSON(w, 503, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 500, map[string]string{"error": err.Error()})
}

func isMutating(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// authenticate resolves the session cookie or bearer API token.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*authInfo, error) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if s.ControlToken != "" && strings.HasPrefix(r.URL.Path, "/api/update") && auth.ConstantTimeEqual(tok, s.ControlToken) {
			return &authInfo{User: &store.User{Username: "dumpcat update (local)", Role: store.RoleAdmin}}, nil
		}
		t, err := s.Store.GetTokenByHash(auth.SHA256Hex(tok))
		if err != nil {
			return nil, errf(401, "invalid API token")
		}
		if t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) {
			return nil, errf(401, "API token has expired")
		}
		u, err := s.Store.GetUser(t.UserID)
		if err != nil || u.Disabled {
			return nil, errf(401, "account is locked")
		}
		if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
			s.Store.TouchToken(t.ID)
		}
		return &authInfo{User: u, Token: t}, nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, errf(401, "not signed in")
	}
	se, err := s.Store.GetSession(auth.SHA256Hex(c.Value))
	if err != nil {
		return nil, errf(401, "invalid session")
	}
	now := time.Now()
	if now.After(se.ExpiresAt) {
		s.Store.DeleteSession(se.ID)
		return nil, errf(401, "session has expired")
	}
	u, err := s.Store.GetUser(se.UserID)
	if err != nil || u.Disabled {
		s.Store.DeleteSession(se.ID)
		return nil, errf(401, "account is locked")
	}
	if isMutating(r.Method) {
		if !auth.ConstantTimeEqual(r.Header.Get("X-CSRF-Token"), se.CSRF) {
			return nil, errf(403, "invalid CSRF token — please reload the page")
		}
	}
	if now.Sub(se.LastSeenAt) > time.Minute {
		// sliding expiration (idle timeout); remember-me sessions keep their longer window
		idle := time.Duration(s.Store.GetSettings().SessionHours) * time.Hour
		exp := se.ExpiresAt
		if now.Add(idle).After(exp) {
			exp = now.Add(idle)
		}
		if exp.Sub(se.CreatedAt) > 30*24*time.Hour {
			exp = se.CreatedAt.Add(30 * 24 * time.Hour)
		}
		s.Store.TouchSession(se.ID, now, exp)
	}
	return &authInfo{User: u, Session: se}, nil
}

// securityHeaders wraps all responses.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func accessLog(next http.Handler) http.Handler {
	quiet := os.Getenv("DUMPCAT_ACCESS_LOG") == ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quiet {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	// static UI: content-hash ETags + no-cache so upgrades never serve stale JS
	fileServer := http.FileServer(http.FS(s.WebFS))
	etags := map[string]string{}
	_ = fs.WalkDir(s.WebFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := fs.ReadFile(s.WebFS, p); err == nil {
				etags[p] = `"` + auth.SHA256Hex(string(b))[:20] + `"`
			}
		}
		return nil
	})
	dev := os.Getenv("DUMPCAT_WEB_DIR") != ""
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(s.WebFS, p); err != nil {
			p = "index.html"
			r.URL.Path = "/"
		}
		w.Header().Set("Cache-Control", "no-cache")
		if et, ok := etags[p]; ok && !dev {
			w.Header().Set("ETag", et)
			if r.Header.Get("If-None-Match") == et {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
	return accessLog(securityHeaders(mux))
}

func (s *Server) routes(mux *http.ServeMux) {
	const (
		pub = ""
		vw  = store.RoleViewer
		op  = store.RoleOperator
		adm = store.RoleAdmin
	)
	// auth
	s.route(mux, "GET /api/auth/status", pub, s.authStatus)
	s.route(mux, "POST /api/auth/setup", pub, s.authSetup)
	s.route(mux, "POST /api/auth/login", pub, s.authLogin)
	s.route(mux, "POST /api/auth/logout", pub, s.authLogout)
	s.route(mux, "GET /api/me", vw, s.me)
	s.route(mux, "POST /api/me/password", vw, s.mePassword)
	s.route(mux, "POST /api/me/totp/setup", vw, s.meTOTPSetup)
	s.route(mux, "POST /api/me/totp/enable", vw, s.meTOTPEnable)
	s.route(mux, "POST /api/me/totp/disable", vw, s.meTOTPDisable)
	s.route(mux, "GET /api/me/tokens", vw, s.meTokens)
	s.route(mux, "POST /api/me/tokens", vw, s.meTokenCreate)
	s.route(mux, "DELETE /api/me/tokens/{id}", vw, s.meTokenDelete)
	s.route(mux, "POST /api/me/logout-others", vw, s.meLogoutOthers)
	// users
	s.route(mux, "GET /api/users", adm, s.usersList)
	s.route(mux, "POST /api/users", adm, s.userCreate)
	s.route(mux, "PUT /api/users/{id}", adm, s.userUpdate)
	s.route(mux, "DELETE /api/users/{id}", adm, s.userDelete)
	s.route(mux, "POST /api/users/{id}/reset-2fa", adm, s.userReset2FA)
	// servers
	s.route(mux, "GET /api/servers", vw, s.serversList)
	s.route(mux, "POST /api/servers", op, s.serverCreate)
	s.route(mux, "POST /api/servers/test", op, s.serverTestUnsaved)
	s.route(mux, "GET /api/servers/{id}", vw, s.serverGet)
	s.route(mux, "PUT /api/servers/{id}", op, s.serverUpdate)
	s.route(mux, "DELETE /api/servers/{id}", op, s.serverDelete)
	s.route(mux, "GET /api/servers/{id}/info", vw, s.serverInfo)
	// agents: database hosts that connect out to DumpCat
	s.route(mux, "GET /api/agents", vw, s.agentsList)
	s.route(mux, "POST /api/agents", adm, s.agentCreate)
	s.route(mux, "PUT /api/agents/{id}", adm, s.agentUpdate)
	s.route(mux, "POST /api/agents/{id}/rotate", adm, s.agentRotate)
	s.route(mux, "DELETE /api/agents/{id}", adm, s.agentDelete)
	mux.HandleFunc("GET "+agent.PathConnect, s.agentEndpoint(false))
	// built-in WireGuard: database hosts join DumpCat's VPN (off by default)
	s.route(mux, "GET /api/wireguard", vw, s.wgGet)
	s.route(mux, "PUT /api/wireguard", adm, s.wgUpdate)
	s.route(mux, "POST /api/wireguard/peers", adm, s.wgPeerCreate)
	s.route(mux, "PUT /api/wireguard/peers/{id}", adm, s.wgPeerUpdate)
	s.route(mux, "POST /api/wireguard/peers/{id}/rotate", adm, s.wgPeerRotate)
	s.route(mux, "DELETE /api/wireguard/peers/{id}", adm, s.wgPeerDelete)
	mux.HandleFunc("GET "+agent.PathStream, s.agentEndpoint(true))
	// storages
	s.route(mux, "GET /api/storage-types", vw, s.storageTypes)
	s.route(mux, "GET /api/storages", vw, s.storagesList)
	s.route(mux, "POST /api/storages", op, s.storageCreate)
	s.route(mux, "POST /api/storages/test", op, s.storageTestUnsaved)
	s.route(mux, "GET /api/storages/{id}", vw, s.storageGet)
	s.route(mux, "PUT /api/storages/{id}", op, s.storageUpdate)
	s.route(mux, "DELETE /api/storages/{id}", op, s.storageDelete)
	s.route(mux, "POST /api/storages/{id}/test", op, s.storageTest)
	s.route(mux, "GET /api/storages/{id}/browse", op, s.storageBrowse)
	s.route(mux, "GET /api/storages/{id}/usage", vw, s.storageUsage)
	s.route(mux, "POST /api/storages/{id}/import", op, s.storageImport)
	s.route(mux, "POST /api/oauth/start", op, s.oauthStart)
	s.route(mux, "POST /api/oauth/exchange", op, s.oauthExchange)
	s.route(mux, "GET /api/oauth/result", op, s.oauthResult)
	s.route(mux, "GET /api/oauth/callback", vw, s.oauthCallback)
	// jobs
	s.route(mux, "GET /api/jobs", vw, s.jobsList)
	s.route(mux, "POST /api/jobs", op, s.jobCreate)
	s.route(mux, "GET /api/jobs/{id}", vw, s.jobGet)
	s.route(mux, "PUT /api/jobs/{id}", op, s.jobUpdate)
	s.route(mux, "DELETE /api/jobs/{id}", op, s.jobDelete)
	s.route(mux, "POST /api/jobs/{id}/run", op, s.jobRun)
	s.route(mux, "POST /api/jobs/{id}/toggle", op, s.jobToggle)
	s.route(mux, "POST /api/cron/preview", vw, s.cronPreview)
	// runs
	s.route(mux, "GET /api/runs", vw, s.runsList)
	s.route(mux, "GET /api/runs/{id}", vw, s.runGet)
	s.route(mux, "GET /api/runs/{id}/log", vw, s.runLog)
	s.route(mux, "POST /api/runs/{id}/cancel", op, s.runCancel)
	s.route(mux, "POST /api/runs/{id}/verify", op, s.runVerify)
	s.route(mux, "POST /api/runs/{id}/delete", op, s.runDelete)
	s.route(mux, "POST /api/runs/{id}/restore", op, s.runRestore)
	s.route(mux, "POST /api/runs/{id}/restore-check", op, s.runRestoreCheck)
	s.route(mux, "POST /api/runs/{id}/copy", op, s.runCopy)
	s.route(mux, "GET /api/runs/{id}/download", op, s.runDownload)
	// restores
	s.route(mux, "GET /api/restores", vw, s.restoresList)
	s.route(mux, "GET /api/restores/{id}", vw, s.restoreGet)
	s.route(mux, "GET /api/restores/{id}/log", vw, s.restoreLog)
	s.route(mux, "POST /api/restores/{id}/cancel", op, s.restoreCancel)
	// binlog archiving & point-in-time recovery
	s.route(mux, "GET /api/binlog", vw, s.binlogOverview)
	s.route(mux, "GET /api/jobs/{id}/binlogs", vw, s.jobBinlogs)
	s.route(mux, "GET /api/jobs/{id}/binlog/log", vw, s.jobBinlogLog)
	s.route(mux, "POST /api/jobs/{id}/binlog/rotate", op, s.jobBinlogRotate)
	s.route(mux, "POST /api/jobs/{id}/binlog/restart", op, s.jobBinlogRestart)
	s.route(mux, "POST /api/pitr/plan", op, s.pitrPlan)
	// activity & dashboard
	s.route(mux, "GET /api/activity", vw, s.activity)
	s.route(mux, "GET /api/dashboard", vw, s.dashboard)
	// notifications
	s.route(mux, "GET /api/notify-types", vw, s.notifyTypes)
	s.route(mux, "GET /api/notifications", adm, s.channelsList)
	s.route(mux, "POST /api/notifications", adm, s.channelSave)
	s.route(mux, "PUT /api/notifications/{id}", adm, s.channelSave)
	s.route(mux, "DELETE /api/notifications/{id}", adm, s.channelDelete)
	s.route(mux, "POST /api/notifications/{id}/test", adm, s.channelTest)
	s.route(mux, "POST /api/notifications/test", adm, s.channelTestUnsaved)
	// settings & system
	s.route(mux, "GET /api/settings", adm, s.settingsGet)
	s.route(mux, "PUT /api/settings", adm, s.settingsPut)
	s.route(mux, "GET /api/system", vw, s.system)
	s.route(mux, "GET /api/update", adm, s.updateStatus)
	s.route(mux, "POST /api/update/check", adm, s.updateCheck)
	s.route(mux, "POST /api/update/install", adm, s.updateInstall)
	s.route(mux, "POST /api/update/cancel", adm, s.updateCancel)
	s.route(mux, "POST /api/update/rollback", adm, s.updateRollback)
	s.route(mux, "POST /api/system/tools/refresh", adm, s.toolsRefresh)
	s.route(mux, "GET /api/audit", adm, s.auditList)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"error": "API endpoint not found"})
	})
}
