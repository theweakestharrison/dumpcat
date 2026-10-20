package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"dumpcat/internal/auth"
	"dumpcat/internal/oidc/oidctest"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

type ssoEnv struct {
	t   *testing.T
	s   *Server
	h   http.Handler
	idp *oidctest.Issuer
}

// newSSOEnv is a DumpCat with SSO on against a fake provider (client "dumpcat").
func newSSOEnv(t *testing.T, mutate func(*store.SSOConfig)) *ssoEnv {
	t.Helper()
	s, h, _ := newSetupServer(t)
	s.Limiter, s.IPLimit = auth.NewLimiter(), auth.NewLimiter()
	idp := oidctest.New("dumpcat", "s3cret")
	t.Cleanup(idp.Close)
	c := store.SSOConfig{Enabled: true, Issuer: idp.URL, ClientID: "dumpcat", ClientSecret: "s3cret", DefaultRole: store.RoleViewer}
	if mutate != nil {
		mutate(&c)
	}
	if err := s.Store.SaveSSO(c); err != nil {
		t.Fatal(err)
	}
	return &ssoEnv{t: t, s: s, h: h, idp: idp}
}

func (e *ssoEnv) user(name, email, role string, mutate func(*store.User)) *store.User {
	e.t.Helper()
	u := &store.User{Username: name, Email: email, Role: role, PasswordHash: "x"}
	if mutate != nil {
		mutate(u)
	}
	if err := e.s.Store.CreateUser(u); err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *ssoEnv) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "198.51.100.7:5000"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func cookieOf(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 && c.Value != "" {
			return c
		}
	}
	return nil
}

// start begins a sign-in and returns the provider URL and the browser's state cookie.
func (e *ssoEnv) start(path string) (string, *http.Cookie) {
	e.t.Helper()
	rec := e.get(path)
	c := cookieOf(rec, ssoStateCookie)
	if rec.Code != http.StatusFound || c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		e.t.Fatalf("start: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	return rec.Header().Get("Location"), c
}

func (e *ssoEnv) callback(code, state string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return e.get("/api/auth/sso/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), cookies...)
}

// signIn runs a whole sign-in: DumpCat → the provider (with these ID token claims and
// userinfo) → back to DumpCat.
func (e *ssoEnv) signIn(claims, userinfo map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	loc, c := e.start("/api/auth/sso/start")
	code, state, err := e.idp.Authorize(loc, claims, userinfo)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.callback(code, state, c)
}

func (e *ssoEnv) lastAudit() store.AuditEntry {
	list, _, _ := e.s.Store.ListAudit(1, 0, "")
	if len(list) == 0 {
		return store.AuditEntry{}
	}
	return list[0]
}

func (e *ssoEnv) signedIn(rec *httptest.ResponseRecorder) string {
	e.t.Helper()
	sc := cookieOf(rec, sessionCookie)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" || sc == nil {
		e.t.Fatalf("not signed in: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	me := e.get("/api/me", sc)
	var out struct {
		User struct{ Username, Role string } `json:"user"`
	}
	_ = json.Unmarshal(me.Body.Bytes(), &out)
	return out.User.Username + "/" + out.User.Role
}

func TestSSOSignIn(t *testing.T) {
	e := newSSOEnv(t, nil)
	alice := e.user("alice", "Alice@Example.com", store.RoleOperator, nil)

	// the authorization request: code flow, PKCE S256, state, nonce, the callback URL
	loc, _ := e.start("/api/auth/sso/start?remember=1")
	q, _ := url.Parse(loc)
	if !strings.HasPrefix(loc, e.idp.URL+"/authorize?") || q.Query().Get("redirect_uri") != "http://example.com"+ssoCallbackPath ||
		q.Query().Get("scope") != "openid email profile" {
		t.Fatalf("authorization URL %s", loc)
	}

	// found by email (case does not matter), then linked to the provider account
	rec := e.signIn(nil, nil)
	if who := e.signedIn(rec); who != "alice/operator" {
		t.Fatalf("signed in as %s", who)
	}
	if a := e.lastAudit(); a.Action != "sso_login" || a.Username != "alice" || !strings.Contains(a.Details, e.idp.URL) {
		t.Fatalf("audit %+v", a)
	}
	u, _ := e.s.Store.GetUser(alice.ID)
	if u.SSOSubject != e.idp.URL+" user-1" || u.LastLoginAt == nil {
		t.Fatalf("not linked: %q", u.SSOSubject)
	}

	// later sign-ins find the linked account even when the email changed at the provider
	if who := e.signedIn(e.signIn(map[string]any{"email": "alice.renamed@example.com"}, nil)); who != "alice/operator" {
		t.Fatalf("second sign-in as %s", who)
	}
	if e.idp.Exchanges != 2 {
		t.Fatalf("%d code exchanges", e.idp.Exchanges)
	}

	// a state works once: replaying the callback fails
	loc, c := e.start("/api/auth/sso/start")
	code, state, _ := e.idp.Authorize(loc, nil, nil)
	e.signedIn(e.callback(code, state, c))
	if rec := e.callback(code, state, c); rec.Code != http.StatusForbidden || cookieOf(rec, sessionCookie) != nil {
		t.Fatalf("replay: %d", rec.Code)
	}

	// the sign-in page shows the button while SSO is on
	var st struct {
		SSO struct {
			Enabled bool
			Label   string
		} `json:"sso"`
	}
	_ = json.Unmarshal(e.get("/api/auth/status").Body.Bytes(), &st)
	if !st.SSO.Enabled || st.SSO.Label != "Sign in with SSO" {
		t.Fatalf("status: %+v", st)
	}
	cfg := e.s.Store.GetSSO()
	cfg.Enabled = false
	_ = e.s.Store.SaveSSO(cfg)
	if rec := e.get("/api/auth/sso/start"); rec.Code != http.StatusNotFound {
		t.Fatalf("start while off: %d", rec.Code)
	}
	if strings.Contains(e.get("/api/auth/status").Body.String(), `"sso"`) {
		t.Fatal("the button shows while SSO is off")
	}
}

func TestSSORefused(t *testing.T) {
	type tc struct {
		cfg    func(*store.SSOConfig)
		user   func(*store.User)
		claims map[string]any
		info   map[string]any
		run    func(e *ssoEnv) *httptest.ResponseRecorder // instead of a normal sign-in
		want   string
	}
	cases := map[string]tc{
		"state of another sign-in": {run: func(e *ssoEnv) *httptest.ResponseRecorder {
			loc, c := e.start("/api/auth/sso/start")
			code, _, _ := e.idp.Authorize(loc, nil, nil)
			return e.callback(code, "forged-state", c)
		}, want: "another browser"},
		"no state cookie (another browser)": {run: func(e *ssoEnv) *httptest.ResponseRecorder {
			loc, _ := e.start("/api/auth/sso/start")
			code, state, _ := e.idp.Authorize(loc, nil, nil)
			return e.callback(code, state)
		}, want: "another browser"},
		"code of another sign-in (PKCE)": {run: func(e *ssoEnv) *httptest.ResponseRecorder {
			locA, _ := e.start("/api/auth/sso/start")
			codeA, _, _ := e.idp.Authorize(locA, nil, nil)
			locB, cB := e.start("/api/auth/sso/start")
			_, stateB, _ := e.idp.Authorize(locB, nil, nil)
			return e.callback(codeA, stateB, cB)
		}, want: "PKCE"},
		"provider error": {run: func(e *ssoEnv) *httptest.ResponseRecorder {
			loc, c := e.start("/api/auth/sso/start")
			q, _ := url.Parse(loc)
			return e.get("/api/auth/sso/callback?error=access_denied&error_description=User+cancelled&state="+q.Query().Get("state"), c)
		}, want: "access_denied User cancelled"},
		"other nonce":          {claims: map[string]any{"nonce": "from-another-sign-in"}, want: "nonce"},
		"other audience":       {claims: map[string]any{"aud": "another-client"}, want: "meant for"},
		"expired token":        {claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix(), "iat": time.Now().Add(-2 * time.Hour).Unix()}, want: "expired"},
		"other issuer":         {claims: map[string]any{"iss": "https://evil.example"}, want: "issued by"},
		"email not verified":   {claims: map[string]any{"email_verified": false}, want: "not verified"},
		"no email":             {claims: map[string]any{"email": nil}, want: "no email address"},
		"domain not allowed":   {cfg: func(c *store.SSOConfig) { c.AllowedDomains = "corp.example, @ops.corp.example" }, want: "limited to corp.example, ops.corp.example"},
		"unknown user":         {claims: map[string]any{"email": "mallory@example.com"}, want: "no account with the email address mallory@example.com"},
		"locked account":       {user: func(u *store.User) { u.Disabled = true }, want: "is locked"},
		"linked to another":    {user: func(u *store.User) { u.SSOSubject = "https://other-idp user-9" }, want: "linked to another"},
		"in no allowed group":  {cfg: func(c *store.SSOConfig) { c.RoleClaim, c.AdminValues = "groups", "dc-admins" }, info: map[string]any{"groups": []string{"staff"}}, want: "none of the groups"},
		"group claim missing":  {cfg: func(c *store.SSOConfig) { c.RoleClaim, c.ViewerValues = "groups", "staff" }, want: "none of the groups"},
		"auto-create, bad dom": {cfg: func(c *store.SSOConfig) { c.AutoCreate, c.AllowedDomains = true, "corp.example" }, claims: map[string]any{"email": "eve@gmail.com"}, want: "limited to"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newSSOEnv(t, c.cfg)
			e.user("alice", "alice@example.com", store.RoleOperator, c.user)
			var rec *httptest.ResponseRecorder
			if c.run != nil {
				rec = c.run(e)
			} else {
				rec = e.signIn(c.claims, c.info)
			}
			body := rec.Body.String()
			if rec.Code != http.StatusForbidden || cookieOf(rec, sessionCookie) != nil || !strings.Contains(body, "Sign-in failed") ||
				!strings.Contains(body, html2(c.want)) {
				t.Fatalf("%d %s", rec.Code, body)
			}
			if a := e.lastAudit(); a.Action != "sso_login_failed" || !strings.Contains(a.Details, c.want) {
				t.Fatalf("audit %+v", a)
			}
			if n, _ := e.s.Store.CountUsers(); n != 1 {
				t.Fatalf("%d users", n)
			}
		})
	}
}

// html2 is how a message shows in a page.
func html2(s string) string { return strings.NewReplacer(`"`, "&#34;", "'", "&#39;").Replace(s) }

func TestSSOAutoCreateAndRoles(t *testing.T) {
	e := newSSOEnv(t, func(c *store.SSOConfig) { c.AutoCreate, c.AllowedDomains = true, "example.com" })
	// a new user: the provider's username, the default role, no password
	if who := e.signedIn(e.signIn(map[string]any{"sub": "bob-1", "email": "bob@example.com", "preferred_username": "bob"}, nil)); who != "bob/viewer" {
		t.Fatalf("created %s", who)
	}
	bob, err := e.s.Store.GetUserByName("bob")
	if err != nil || bob.PasswordHash != "" || bob.Email != "bob@example.com" || auth.CheckPassword(bob.PasswordHash, "") {
		t.Fatalf("bob: %+v %v", bob, err)
	}
	// a taken username gets a suffix
	if who := e.signedIn(e.signIn(map[string]any{"sub": "bob-2", "email": "bob2@example.com", "preferred_username": "bob"}, nil)); who != "bob-2/viewer" {
		t.Fatalf("second bob: %s", who)
	}

	// role mapping from a group claim the provider only sends from userinfo; the highest role wins
	cfg := e.s.Store.GetSSO()
	cfg.RoleClaim, cfg.AdminValues, cfg.OperatorValues = "groups", "dc-admins", "dc-ops, backup-team"
	_ = e.s.Store.SaveSSO(cfg)
	carol := map[string]any{"sub": "carol-1", "email": "carol@example.com", "preferred_username": "carol"}
	if who := e.signedIn(e.signIn(carol, map[string]any{"groups": []string{"staff", "Backup-Team"}})); who != "carol/operator" {
		t.Fatalf("carol: %s", who)
	}
	if who := e.signedIn(e.signIn(carol, map[string]any{"groups": []string{"dc-admins", "dc-ops"}})); who != "carol/admin" {
		t.Fatalf("carol promoted: %s", who)
	}
	if a := e.lastAudit(); a.Action != "sso_login" {
		t.Fatalf("audit %+v", a)
	}
	// Keycloak's realm roles: a nested claim (another admin exists, so carol may be demoted)
	root := e.user("root", "root@example.com", store.RoleAdmin, nil)
	cfg.RoleClaim = "realm_access.roles"
	_ = e.s.Store.SaveSSO(cfg)
	if who := e.signedIn(e.signIn(carol, map[string]any{"realm_access": map[string]any{"roles": []string{"dc-ops"}}})); who != "carol/operator" {
		t.Fatalf("carol by realm role: %s", who)
	}
	// the last active admin keeps the role whatever the groups say
	cfg.RoleClaim, cfg.ViewerValues = "groups", "staff"
	_ = e.s.Store.SaveSSO(cfg)
	dave := map[string]any{"sub": "dave-1", "email": "dave@example.com", "preferred_username": "dave"}
	e.signedIn(e.signIn(dave, map[string]any{"groups": []string{"dc-admins"}}))
	root.Disabled = true
	_ = e.s.Store.UpdateUser(root)
	if who := e.signedIn(e.signIn(dave, map[string]any{"groups": []string{"staff"}})); who != "dave/admin" {
		t.Fatalf("the last admin was demoted: %s", who)
	}
}

func TestSSOWithTwoFactor(t *testing.T) {
	e := newSSOEnv(t, nil)
	secret := auth.NewTOTPSecret()
	e.user("alice", "alice@example.com", store.RoleAdmin, func(u *store.User) { u.TOTPEnabled, u.TOTPSecret = true, secret })

	// SSO alone does not sign in an account with 2FA: the sign-in page asks for the code
	rec := e.signIn(nil, nil)
	pend := cookieOf(rec, ssoTOTPCookie)
	if rec.Code != http.StatusFound || cookieOf(rec, sessionCookie) != nil || pend == nil || pend.Path != "/api/auth" {
		t.Fatalf("2FA step: %d %v", rec.Code, rec.Result().Cookies())
	}
	if !strings.Contains(e.get("/api/auth/status", pend).Body.String(), `"sso_totp":true`) {
		t.Fatal("status does not ask for the code")
	}
	post := func(code string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/sso/totp", strings.NewReader(`{"totp":"`+code+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.7:5000"
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	now, _ := auth.TOTPCode(secret, time.Now())
	if rec := post(now); rec.Code != 401 {
		t.Fatalf("without the SSO step: %d", rec.Code)
	}
	if rec := post("000000", pend); rec.Code != 401 || cookieOf(rec, sessionCookie) != nil {
		t.Fatalf("wrong code: %d", rec.Code)
	}
	rec = post(now, pend)
	if rec.Code != 200 || cookieOf(rec, sessionCookie) == nil {
		t.Fatalf("right code: %d %s", rec.Code, rec.Body.String())
	}
	if a := e.lastAudit(); a.Action != "sso_login" || !strings.Contains(a.Details, "two-factor") {
		t.Fatalf("audit %+v", a)
	}
	if rec := post(now, pend); rec.Code != 401 {
		t.Fatalf("the step was used twice: %d", rec.Code)
	}
}

func TestSSOPasswordFallback(t *testing.T) {
	e := newSSOEnv(t, func(c *store.SSOConfig) { c.PasswordAdminsOnly = true })
	hash, _ := auth.HashPassword("correct horse")
	e.user("root", "root@example.com", store.RoleAdmin, func(u *store.User) { u.PasswordHash = hash })
	e.user("ops", "ops@example.com", store.RoleOperator, func(u *store.User) { u.PasswordHash = hash })
	login := func(name string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"`+name+`","password":"correct horse"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := login("root"); c != 200 {
		t.Fatalf("admin fallback: %d", c)
	}
	if c := login("ops"); c != 403 {
		t.Fatalf("operator with a password: %d", c)
	}
	cfg := e.s.Store.GetSSO()
	cfg.Enabled = false
	_ = e.s.Store.SaveSSO(cfg)
	if c := login("ops"); c != 200 {
		t.Fatalf("SSO off: %d", c)
	}
}

func TestSSOSettingsAndTest(t *testing.T) {
	e := newSSOEnv(t, func(c *store.SSOConfig) { *c = store.SSOConfig{} })
	admin := tokenFor(t, e.s, "root", store.RoleAdmin)
	viewer := tokenFor(t, e.s, "watcher", store.RoleViewer)
	e.user("alice", "alice@example.com", store.RoleOperator, nil)

	if rec := send(e.h, "GET", "/api/settings/sso", viewer, nil); rec.Code != 403 {
		t.Fatalf("viewer: %d", rec.Code)
	}
	for body, want := range map[string]string{
		`{"enabled":true,"issuer":"http://idp.example.com","client_id":"x"}`: "https",
		`{"enabled":true,"issuer":"","client_id":"x"}`:                       "issuer URL and the client ID",
		`{"enabled":true,"issuer":"http://127.0.0.1:1","client_id":"x"}`:     "cannot be used",
		`{"allowed_domains":"exa mple.com, a_b.com"}`:                        "invalid email domain",
		`{"role_claim":"groups; drop"}`:                                      "invalid claim",
		`{"default_role":"root"}`:                                            "default role",
	} {
		var in map[string]any
		_ = json.Unmarshal([]byte(body), &in)
		if rec := send(e.h, "PUT", "/api/settings/sso", admin, in); rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}

	in := map[string]any{"enabled": true, "issuer": e.idp.URL + "/", "client_id": "dumpcat", "client_secret": "s3cret", "scopes": "email profile groups",
		"allowed_domains": "@Example.com", "role_claim": "", "button_label": "Sign in with Example ID"}
	rec := send(e.h, "PUT", "/api/settings/sso", admin, in)
	var view struct {
		ClientSecret   string `json:"client_secret"`
		Scopes         string `json:"scopes"`
		AllowedDomains string `json:"allowed_domains"`
		RedirectURL    string `json:"redirect_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if rec.Code != 200 || view.ClientSecret != storage.SecretMask || view.Scopes != "openid email profile groups" || view.AllowedDomains != "example.com" ||
		view.RedirectURL != "http://example.com"+ssoCallbackPath {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	// the secret is encrypted at rest and never sent back; the mask keeps it
	raw, _ := e.s.Store.GetKV("sso")
	if strings.Contains(raw, "s3cret") || !strings.Contains(raw, `"client_secret":"enc:`) {
		t.Fatalf("stored: %s", raw)
	}
	if body := send(e.h, "GET", "/api/settings/sso", admin, nil).Body.String(); strings.Contains(body, "s3cret") || !strings.Contains(body, storage.SecretMask) {
		t.Fatalf("get: %s", body)
	}
	in["client_secret"] = storage.SecretMask
	in["button_label"] = "Example ID"
	if rec := send(e.h, "PUT", "/api/settings/sso", admin, in); rec.Code != 200 || e.s.Store.GetSSO().ClientSecret != "s3cret" {
		t.Fatalf("mask: %d %q", rec.Code, e.s.Store.GetSSO().ClientSecret)
	}
	if !strings.Contains(e.get("/api/auth/status").Body.String(), `"label":"Example ID"`) {
		t.Fatal("button label")
	}

	// Test SSO with unsaved settings (auto-create on): shows what would happen, signs nobody in
	test := map[string]any{"issuer": e.idp.URL, "client_id": "dumpcat", "client_secret": storage.SecretMask, "auto_create": true, "default_role": "operator"}
	rec = send(e.h, "POST", "/api/settings/sso/test", admin, test)
	var tr struct{ URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &tr)
	sc := cookieOf(rec, ssoStateCookie)
	if rec.Code != 200 || tr.URL == "" || sc == nil {
		t.Fatalf("test start: %d %s", rec.Code, rec.Body.String())
	}
	code, state, err := e.idp.Authorize(tr.URL, map[string]any{"email": "newbie@example.com", "preferred_username": "newbie"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec = e.callback(code, state, sc)
	page := rec.Body.String()
	if rec.Code != 200 || cookieOf(rec, sessionCookie) != nil || !strings.Contains(page, "SSO test passed") ||
		!strings.Contains(page, "creates the account newbie with the role operator") || !strings.Contains(page, "preferred_username") {
		t.Fatalf("test page: %d %s", rec.Code, page)
	}
	if _, err := e.s.Store.GetUserByName("newbie"); err == nil {
		t.Fatal("the test created an account")
	}
	if a := e.lastAudit(); a.Action != "sso_test" || a.Username != "root" {
		t.Fatalf("audit %+v", a)
	}
	// a failing test says why
	rec = send(e.h, "POST", "/api/settings/sso/test", admin, map[string]any{"client_id": "wrong-client"})
	_ = json.Unmarshal(rec.Body.Bytes(), &tr)
	code, state, _ = e.idp.Authorize(strings.Replace(tr.URL, "client_id=wrong-client", "client_id=dumpcat", 1), nil, nil)
	if page := e.callback(code, state, cookieOf(rec, ssoStateCookie)).Body.String(); !strings.Contains(page, "SSO test failed") || !strings.Contains(page, "invalid_client") {
		t.Fatalf("failed test page: %s", page)
	}
	if u, _ := e.s.Store.GetUserByName("alice"); u.SSOSubject != "" {
		t.Fatal("a test linked an account")
	}
}

func TestUsersWithoutPassword(t *testing.T) {
	e := newSSOEnv(t, nil)
	admin := tokenFor(t, e.s, "root", store.RoleAdmin)
	if rec := send(e.h, "POST", "/api/users", admin, map[string]string{"username": "sso-only", "role": "viewer"}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "email") {
		t.Fatalf("no email: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(e.h, "POST", "/api/users", admin, map[string]string{"username": "sso-only", "email": "alice@example.com", "role": "viewer"}); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"has_password":false`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if who := e.signedIn(e.signIn(nil, nil)); who != "sso-only/viewer" {
		t.Fatalf("signed in as %s", who)
	}
	u, _ := e.s.Store.GetUserByName("sso-only")
	if rec := send(e.h, "PUT", "/api/users/"+itoa(u.ID), admin, map[string]any{"unlink_sso": true}); rec.Code != 200 || strings.Contains(rec.Body.String(), `"sso":true`) {
		t.Fatalf("unlink: %d %s", rec.Code, rec.Body.String())
	}
	if u, _ := e.s.Store.GetUserByName("sso-only"); u.SSOSubject != "" {
		t.Fatal("still linked")
	}
	// with SSO off, a password is required again
	cfg := e.s.Store.GetSSO()
	cfg.Enabled = false
	_ = e.s.Store.SaveSSO(cfg)
	if rec := send(e.h, "POST", "/api/users", admin, map[string]string{"username": "nopass", "email": "n@example.com", "role": "viewer"}); rec.Code != 400 {
		t.Fatalf("SSO off, no password: %d", rec.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
