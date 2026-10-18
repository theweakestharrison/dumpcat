package api

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"

	"dumpcat/internal/auth"
	"dumpcat/internal/store"
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.@\-]{3,64}$`)

// setupMu makes "no user yet -> create the first admin" atomic, so two
// concurrent first visitors cannot both become admin. It also guards the
// setup token (setup.go).
var setupMu sync.Mutex

func (s *Server) userView(u *store.User) map[string]any {
	return map[string]any{
		"id": u.ID, "username": u.Username, "email": u.Email, "role": u.Role, "totp_enabled": u.TOTPEnabled,
		"disabled": u.Disabled, "created_at": u.CreatedAt, "last_login_at": u.LastLoginAt,
	}
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) error {
	n, _ := s.Store.CountUsers()
	out := map[string]any{"setup_required": n == 0, "authenticated": false, "version": s.Version, "platform": runtime.GOOS}
	if n > 0 {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			if se, err := s.Store.GetSession(auth.SHA256Hex(c.Value)); err == nil && time.Now().Before(se.ExpiresAt) {
				if u, err := s.Store.GetUser(se.UserID); err == nil && !u.Disabled {
					out["authenticated"] = true
					out["user"] = s.userView(u)
					out["csrf"] = se.CSRF
				}
			}
		}
	}
	return ok(w, out)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User, remember bool) (*store.Session, error) {
	tok := auth.RandomToken(32)
	now := time.Now()
	dur := time.Duration(s.Store.GetSettings().SessionHours) * time.Hour
	if remember {
		dur = 30 * 24 * time.Hour
	}
	ua := r.UserAgent()
	if len(ua) > 250 {
		ua = ua[:250]
	}
	se := &store.Session{ID: auth.SHA256Hex(tok), UserID: u.ID, CSRF: auth.RandomToken(24), IP: s.clientIP(r), UserAgent: ua,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(dur)}
	if err := s.Store.CreateSession(se); err != nil {
		return nil, err
	}
	c := &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r)}
	if remember {
		c.Expires = se.ExpiresAt
	}
	http.SetCookie(w, c)
	s.Store.TouchLogin(u.ID)
	return se, nil
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		Email      string `json:"email"`
		SetupToken string `json:"setup_token"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	in.Username = strings.TrimSpace(in.Username)
	setupMu.Lock()
	defer setupMu.Unlock()
	n, err := s.Store.CountUsers()
	if err != nil {
		return err
	}
	if n > 0 {
		return errf(409, "setup has already been completed")
	}
	if err := s.checkSetupToken(in.SetupToken, s.clientIP(r)); err != nil {
		return err
	}
	if !usernameRe.MatchString(in.Username) {
		return badRequest("username must be 3-64 characters: letters, digits, _ . @ -")
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return badRequest("%v", err)
	}
	u := &store.User{Username: in.Username, Email: in.Email, PasswordHash: hash, Role: store.RoleAdmin}
	if err := s.Store.CreateUser(u); err != nil {
		return err
	}
	s.endSetup()
	se, err := s.startSession(w, r, u, false)
	if err != nil {
		return err
	}
	s.Store.Audit(&u.ID, u.Username, "setup", "user:"+u.Username, "created the first admin account", s.clientIP(r))
	return ok(w, map[string]any{"user": s.userView(u), "csrf": se.CSRF})
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTP     string `json:"totp"`
		Remember bool   `json:"remember"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ip := s.clientIP(r)
	userKey := "u:" + strings.ToLower(strings.TrimSpace(in.Username))
	ipKey := "ip:" + ip
	if d := max(s.Limiter.Check(userKey), s.IPLimit.Check(ipKey)); d > 0 {
		return errf(429, "too many failed sign-in attempts, try again in %d minutes", int(d.Minutes())+1)
	}
	fail := func(msg string) error {
		s.Limiter.Fail(userKey)
		s.IPLimit.Fail(ipKey)
		s.Store.Audit(nil, in.Username, "login_failed", "", msg, ip)
		return errf(401, "%s", msg)
	}
	u, err := s.Store.GetUserByName(strings.TrimSpace(in.Username))
	if err != nil {
		auth.BurnTime(in.Password)
		return fail("incorrect username or password")
	}
	if !auth.CheckPassword(u.PasswordHash, in.Password) {
		return fail("incorrect username or password")
	}
	if u.Disabled {
		return errf(403, "account is locked")
	}
	if u.TOTPEnabled {
		if strings.TrimSpace(in.TOTP) == "" {
			return ok(w, map[string]any{"totp_required": true})
		}
		if !auth.VerifyTOTP(u.TOTPSecret, in.TOTP, time.Now()) {
			return fail("incorrect two-factor code")
		}
	}
	s.Limiter.Reset(userKey)
	se, err := s.startSession(w, r, u, in.Remember)
	if err != nil {
		return err
	}
	s.Store.Audit(&u.ID, u.Username, "login", "", "", ip)
	resp := map[string]any{"user": s.userView(u), "csrf": se.CSRF}
	if u.Role == store.RoleAdmin && !u.TOTPEnabled && s.Store.GetSettings().Require2FAAdmins {
		resp["require_2fa_setup"] = true
	}
	return ok(w, resp)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if se, err := s.Store.GetSession(auth.SHA256Hex(c.Value)); err == nil {
			if !auth.ConstantTimeEqual(r.Header.Get("X-CSRF-Token"), se.CSRF) {
				return errf(403, "invalid CSRF token")
			}
			s.Store.DeleteSession(se.ID)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	a := authFrom(r)
	out := map[string]any{"user": s.userView(a.User)}
	if a.Session != nil {
		out["csrf"] = a.Session.CSRF
	}
	return ok(w, out)
}

func (s *Server) mePassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	a := authFrom(r)
	u, err := s.Store.GetUser(a.User.ID)
	if err != nil {
		return err
	}
	if !auth.CheckPassword(u.PasswordHash, in.Current) {
		return badRequest("current password is incorrect")
	}
	hash, err := auth.HashPassword(in.New)
	if err != nil {
		return badRequest("%v", err)
	}
	u.PasswordHash = hash
	if err := s.Store.UpdateUser(u); err != nil {
		return err
	}
	keep := ""
	if a.Session != nil {
		keep = a.Session.ID
	}
	s.Store.DeleteUserSessions(u.ID, keep)
	s.audit(r, "password_change", "user:"+u.Username, "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) meTOTPSetup(w http.ResponseWriter, r *http.Request) error {
	u, err := s.Store.GetUser(userOf(r).ID)
	if err != nil {
		return err
	}
	if u.TOTPEnabled {
		return badRequest("2FA is already enabled — disable it before setting it up again")
	}
	secret := auth.NewTOTPSecret()
	u.TOTPSecret = secret
	if err := s.Store.UpdateUser(u); err != nil {
		return err
	}
	url := auth.TOTPURL("DumpCat", u.Username, secret)
	out := map[string]any{"secret": secret, "url": url}
	if code, err := qr.Encode(url, qr.M); err == nil {
		code.Scale = 6
		out["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
	}
	return ok(w, out)
}

func (s *Server) meTOTPEnable(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	u, err := s.Store.GetUser(userOf(r).ID)
	if err != nil {
		return err
	}
	if u.TOTPSecret == "" {
		return badRequest("2FA setup has not been started")
	}
	if !auth.VerifyTOTP(u.TOTPSecret, in.Code, time.Now()) {
		return badRequest("incorrect code — check that your phone's clock is correct")
	}
	u.TOTPEnabled = true
	if err := s.Store.UpdateUser(u); err != nil {
		return err
	}
	s.audit(r, "2fa_enable", "user:"+u.Username, "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) meTOTPDisable(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	u, err := s.Store.GetUser(userOf(r).ID)
	if err != nil {
		return err
	}
	if !auth.CheckPassword(u.PasswordHash, in.Password) {
		return badRequest("incorrect password")
	}
	u.TOTPEnabled = false
	u.TOTPSecret = ""
	if err := s.Store.UpdateUser(u); err != nil {
		return err
	}
	s.audit(r, "2fa_disable", "user:"+u.Username, "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) meTokens(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListTokens(userOf(r).ID)
	if err != nil {
		return err
	}
	if list == nil {
		list = []store.APIToken{}
	}
	return ok(w, list)
}

func (s *Server) meTokenCreate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name        string `json:"name"`
		ExpiresDays int    `json:"expires_days"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Name) == "" {
		return badRequest("token name is required")
	}
	raw := "mv_" + auth.RandomToken(30)
	t := &store.APIToken{UserID: userOf(r).ID, Name: strings.TrimSpace(in.Name), Prefix: raw[:10], Hash: auth.SHA256Hex(raw)}
	if in.ExpiresDays > 0 {
		e := time.Now().AddDate(0, 0, in.ExpiresDays)
		t.ExpiresAt = &e
	}
	if err := s.Store.CreateToken(t); err != nil {
		return err
	}
	s.audit(r, "token_create", "token:"+t.Name, "")
	return ok(w, map[string]any{"token": raw, "item": t})
}

func (s *Server) meTokenDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.Store.DeleteToken(userOf(r).ID, id); err != nil {
		return err
	}
	s.audit(r, "token_delete", "token:"+r.PathValue("id"), "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) meLogoutOthers(w http.ResponseWriter, r *http.Request) error {
	a := authFrom(r)
	keep := ""
	if a.Session != nil {
		keep = a.Session.ID
	}
	s.Store.DeleteUserSessions(a.User.ID, keep)
	return ok(w, map[string]bool{"ok": true})
}

// ---- user management (admin) ----

func (s *Server) usersList(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListUsers()
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, s.userView(&list[i]))
	}
	return ok(w, out)
}

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if !usernameRe.MatchString(in.Username) {
		return badRequest("username must be 3-64 characters: letters, digits, _ . @ -")
	}
	if !store.ValidRole(in.Role) {
		return badRequest("invalid role")
	}
	if _, err := s.Store.GetUserByName(in.Username); err == nil {
		return badRequest("username already exists")
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return badRequest("%v", err)
	}
	u := &store.User{Username: in.Username, Email: in.Email, PasswordHash: hash, Role: in.Role}
	if err := s.Store.CreateUser(u); err != nil {
		return err
	}
	s.audit(r, "user_create", "user:"+u.Username, "role="+u.Role)
	return ok(w, s.userView(u))
}

func (s *Server) userUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Email    *string `json:"email"`
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
		Password string  `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	u, err := s.Store.GetUser(id)
	if err != nil {
		return err
	}
	me := userOf(r)
	wasAdmin := u.Role == store.RoleAdmin && !u.Disabled
	if in.Email != nil {
		u.Email = *in.Email
	}
	if in.Role != nil {
		if !store.ValidRole(*in.Role) {
			return badRequest("invalid role")
		}
		u.Role = *in.Role
	}
	if in.Disabled != nil {
		if *in.Disabled && u.ID == me.ID {
			return badRequest("you cannot lock your own account")
		}
		u.Disabled = *in.Disabled
	}
	if wasAdmin && (u.Role != store.RoleAdmin || u.Disabled) {
		if n, _ := s.Store.CountAdmins(); n <= 1 {
			return badRequest("there must be at least one active admin")
		}
	}
	if in.Password != "" {
		hash, err := auth.HashPassword(in.Password)
		if err != nil {
			return badRequest("%v", err)
		}
		u.PasswordHash = hash
	}
	if err := s.Store.UpdateUser(u); err != nil {
		return err
	}
	if u.Disabled || in.Password != "" {
		s.Store.DeleteUserSessions(u.ID, "")
	}
	s.audit(r, "user_update", "user:"+u.Username, "role="+u.Role)
	return ok(w, s.userView(u))
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if id == userOf(r).ID {
		return badRequest("you cannot delete your own account")
	}
	u, err := s.Store.GetUser(id)
	if err != nil {
		return err
	}
	if u.Role == store.RoleAdmin {
		if n, _ := s.Store.CountAdmins(); n <= 1 {
			return badRequest("there must be at least one admin")
		}
	}
	if err := s.Store.DeleteUser(id); err != nil {
		return err
	}
	s.audit(r, "user_delete", "user:"+u.Username, "")
	return ok(w, map[string]bool{"ok": true})
}

func (s *Server) userReset2FA(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	u, err := s.Store.GetUser(id)
	if err != nil {
		return err
	}
	u.TOTPEnabled, u.TOTPSecret = false, ""
	if err := s.Store.UpdateUser(u); err != nil {
		return err
	}
	s.audit(r, "user_reset_2fa", "user:"+u.Username, "")
	return ok(w, map[string]bool{"ok": true})
}
