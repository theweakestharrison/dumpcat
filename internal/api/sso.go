package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/oauth2"

	"dumpcat/internal/auth"
	"dumpcat/internal/oidc"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

// Single sign-on with OpenID Connect (Settings → Authentication): the authorization code flow
// with PKCE, a state bound to the browser by a cookie and a nonce, and an ID token checked against
// the provider's keys (internal/oidc). A user is found by the provider account they signed in
// with before, else by their email address (verified at the provider), and is linked to that
// account from then on; unknown users may be created with a default role. Password sign-in stays
// as the fallback. A DumpCat account with two-factor authentication still asks for its code after
// SSO, and a locked account cannot sign in either way.

const (
	ssoStateCookie  = "dumpcat_sso"
	ssoTOTPCookie   = "dumpcat_sso_2fa"
	ssoFlowTTL      = 10 * time.Minute
	ssoTOTPTTL      = 5 * time.Minute
	ssoCallbackPath = "/api/auth/sso/callback"
	ssoDefaultScope = "openid email profile"
	ssoMaxFlows     = 1000
)

// ssoFlow is a sign-in in progress, from the redirect to the provider until it comes back.
type ssoFlow struct {
	nonce, verifier, redirect string
	cfg                       store.SSOConfig // a test runs with the settings of the form, saved or not
	provider                  *oidc.Provider
	remember                  bool
	test                      bool // Settings → Test SSO: show what happens, sign nobody in
	tester                    string
	expires                   time.Time
}

// ssoPending is an SSO sign-in that waits for the account's two-factor code.
type ssoPending struct {
	userID   int64
	remember bool
	issuer   string
	expires  time.Time
}

type ssoProviderEntry struct {
	p  *oidc.Provider
	at time.Time
}

// ssoState keeps sign-ins in progress in memory: a restart only cancels them.
type ssoState struct {
	mu        sync.Mutex
	flows     map[string]*ssoFlow    // by state
	pending   map[string]*ssoPending // by the hash of the 2FA cookie
	providers map[string]ssoProviderEntry
	client    *http.Client // nil: a 15 s timeout (tests use their own)
}

func (st *ssoState) httpClient() *http.Client {
	if st.client != nil {
		return st.client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// ssoProvider returns the issuer's discovery, kept for an hour (fresh: read again).
func (s *Server) ssoProvider(ctx context.Context, issuer string, fresh bool) (*oidc.Provider, error) {
	st := &s.sso
	st.mu.Lock()
	e, ok := st.providers[issuer]
	st.mu.Unlock()
	if ok && !fresh && time.Since(e.at) < time.Hour {
		return e.p, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p, err := oidc.Discover(ctx, st.httpClient(), issuer)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	if st.providers == nil {
		st.providers = map[string]ssoProviderEntry{}
	}
	st.providers[issuer] = ssoProviderEntry{p: p, at: time.Now()}
	st.mu.Unlock()
	return p, nil
}

// ssoRedirectURL is the callback to register at the provider: under the Base URL when it is set.
func (s *Server) ssoRedirectURL(r *http.Request) string {
	if b := s.baseURL(); b != "" {
		return b + ssoCallbackPath
	}
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + ssoCallbackPath
}

func ssoScopes(c store.SSOConfig) []string {
	sc := strings.Fields(c.Scopes)
	if len(sc) == 0 {
		sc = strings.Fields(ssoDefaultScope)
	}
	return sc
}

func ssoOAuth(c store.SSOConfig, p *oidc.Provider, redirect string) *oauth2.Config {
	return &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, RedirectURL: redirect, Scopes: ssoScopes(c),
		Endpoint: oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL}}
}

// ssoBegin starts a sign-in: it remembers the flow, binds it to this browser and returns the
// provider's URL to go to.
func (s *Server) ssoBegin(w http.ResponseWriter, r *http.Request, f *ssoFlow) string {
	state := auth.RandomToken(24)
	f.nonce, f.verifier, f.redirect = auth.RandomToken(24), oauth2.GenerateVerifier(), s.ssoRedirectURL(r)
	f.expires = time.Now().Add(ssoFlowTTL)
	st := &s.sso
	st.mu.Lock()
	if st.flows == nil {
		st.flows = map[string]*ssoFlow{}
	}
	now := time.Now()
	for k, v := range st.flows {
		if now.After(v.expires) || len(st.flows) >= ssoMaxFlows {
			delete(st.flows, k)
		}
	}
	st.flows[state] = f
	st.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: ssoStateCookie, Value: state, Path: "/api/auth/sso", MaxAge: int(ssoFlowTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r)})
	return ssoOAuth(f.cfg, f.provider, f.redirect).AuthCodeURL(state, oauth2.S256ChallengeOption(f.verifier),
		oauth2.SetAuthURLParam("nonce", f.nonce))
}

// ssoTake ends a flow: the state must be the one this browser's cookie holds, and works once.
func (s *Server) ssoTake(r *http.Request) *ssoFlow {
	state := r.URL.Query().Get("state")
	c, err := r.Cookie(ssoStateCookie)
	if state == "" || err != nil || !auth.ConstantTimeEqual(c.Value, state) {
		return nil
	}
	st := &s.sso
	st.mu.Lock()
	f := st.flows[state]
	delete(st.flows, state)
	st.mu.Unlock()
	if f == nil || time.Now().After(f.expires) {
		return nil
	}
	return f
}

// ---- sign-in ----

func (s *Server) ssoStart(w http.ResponseWriter, r *http.Request) error {
	cfg := s.Store.GetSSO()
	if !cfg.Enabled {
		return s.ssoPage(w, http.StatusNotFound, "Single sign-on is off", "<p>Sign in with your username and password.</p>", "/", "Back to sign in")
	}
	p, err := s.ssoProvider(r.Context(), cfg.Issuer, false)
	if err != nil {
		return s.ssoPage(w, http.StatusBadGateway, "The identity provider cannot be reached", "<p>"+html.EscapeString(err.Error())+"</p>", "/", "Back to sign in")
	}
	u := s.ssoBegin(w, r, &ssoFlow{cfg: cfg, provider: p, remember: r.URL.Query().Get("remember") == "1"})
	http.Redirect(w, r, u, http.StatusFound)
	return nil
}

// ssoIdentity is who the provider says signed in.
type ssoIdentity struct {
	Issuer, Subject, Email, Name, Username string
	EmailVerified                          *bool // nil: the provider does not say
	Claims                                 oidc.Claims
}

func ssoEmailClaim(c store.SSOConfig) string {
	if c.EmailClaim != "" {
		return c.EmailClaim
	}
	return "email"
}

// ssoIdentify exchanges the code (with the PKCE verifier) and checks the ID token.
func (s *Server) ssoIdentify(ctx context.Context, f *ssoFlow, code string) (*ssoIdentity, error) {
	if code == "" {
		return nil, errors.New("the identity provider sent no authorization code")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.sso.httpClient())
	tok, err := ssoOAuth(f.cfg, f.provider, f.redirect).Exchange(ctx, code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode != "" {
			return nil, fmt.Errorf("the provider refused the code: %s %s", re.ErrorCode, re.ErrorDescription)
		}
		return nil, fmt.Errorf("the code exchange failed: %v", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("the identity provider sent no ID token (the scopes must include openid)")
	}
	claims, err := f.provider.Verify(ctx, raw, f.cfg.ClientID, f.nonce, time.Now())
	if err != nil {
		return nil, err
	}
	// userinfo fills in what the ID token leaves out (the email or groups at some providers)
	if info, err := f.provider.Userinfo(ctx, tok.AccessToken); err == nil && info.String("sub") == claims.String("sub") {
		for k, v := range info {
			if _, ok := claims[k]; !ok {
				claims[k] = v
			}
		}
	}
	ec := ssoEmailClaim(f.cfg)
	id := &ssoIdentity{Issuer: claims.String("iss"), Subject: claims.String("sub"), Name: claims.String("name"),
		Username: claims.String("preferred_username"), Email: strings.ToLower(strings.TrimSpace(claims.String(ec))), Claims: claims}
	if v, ok := claims.Bool("email_verified"); ok && ec == "email" {
		id.EmailVerified = &v
	}
	return id, nil
}

func ssoList(s string) []string {
	var out []string
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' }) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func ssoDomains(s string) []string {
	var out []string
	for _, d := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) }) {
		if d = strings.TrimPrefix(d, "@"); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func ssoDomainAllowed(list, email string) bool {
	ds := ssoDomains(list)
	if len(ds) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	for _, d := range ds {
		if email[at+1:] == d {
			return true
		}
	}
	return false
}

// ssoRole maps the values of the role claim to a role: the highest one that matches.
func ssoRole(c store.SSOConfig, got []string) string {
	has := func(list string) bool {
		for _, want := range ssoList(list) {
			for _, g := range got {
				if strings.EqualFold(strings.TrimSpace(g), want) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has(c.AdminValues):
		return store.RoleAdmin
	case has(c.OperatorValues):
		return store.RoleOperator
	case has(c.ViewerValues):
		return store.RoleViewer
	}
	return ""
}

// ssoUsername picks a free username for a new account: the provider's username, else the email.
func (s *Server) ssoUsername(id *ssoIdentity) string {
	base := ""
	for _, c := range []string{id.Username, id.Email} {
		c = strings.Map(func(r rune) rune {
			if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.@-", r)) {
				return r
			}
			return '-'
		}, strings.TrimSpace(c))
		if len(c) > 60 {
			c = c[:60]
		}
		if usernameRe.MatchString(c) {
			base = c
			break
		}
	}
	if base == "" {
		base = "sso-user"
	}
	name := base
	for i := 2; i < 1000; i++ {
		if _, err := s.Store.GetUserByName(name); errors.Is(err, store.ErrNotFound) {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return base + "-" + strings.ToLower(auth.RandomToken(4))
}

// ssoUser finds (or creates) the account of a provider identity and applies the role mapping. A
// dry run (Test SSO) changes nothing and says what a sign-in would do.
func (s *Server) ssoUser(c store.SSOConfig, id *ssoIdentity, dry bool, ip string) (*store.User, string, error) {
	if id.Email == "" {
		return nil, "", fmt.Errorf("the identity provider sent no email address (claim %q): add the email scope, or pick the claim that holds it", ssoEmailClaim(c))
	}
	if id.EmailVerified != nil && !*id.EmailVerified {
		return nil, "", fmt.Errorf("the email address %s is not verified at the identity provider", id.Email)
	}
	if !ssoDomainAllowed(c.AllowedDomains, id.Email) {
		return nil, "", fmt.Errorf("%s may not sign in: single sign-on is limited to %s", id.Email, strings.Join(ssoDomains(c.AllowedDomains), ", "))
	}
	role := ""
	if c.RoleClaim != "" {
		if role = ssoRole(c, id.Claims.Strings(c.RoleClaim)); role == "" {
			return nil, "", fmt.Errorf("%s is in none of the groups allowed to sign in (claim %q)", id.Email, c.RoleClaim)
		}
	}
	key := id.Issuer + " " + id.Subject
	u, err := s.Store.GetUserBySSOSubject(key)
	changed := false
	outcome := ""
	switch {
	case err == nil:
		outcome = "uses the account " + u.Username
	case errors.Is(err, store.ErrNotFound):
		list, err := s.Store.FindUsersByEmail(id.Email)
		if err != nil {
			return nil, "", err
		}
		switch len(list) {
		case 0:
			if !c.AutoCreate {
				return nil, "", fmt.Errorf("there is no account with the email address %s: ask an admin to create one", id.Email)
			}
			r := role
			if r == "" {
				r = c.DefaultRole
			}
			if !store.ValidRole(r) {
				r = store.RoleViewer
			}
			u = &store.User{Username: s.ssoUsername(id), Email: id.Email, Role: r, SSOSubject: key}
			if dry {
				return u, fmt.Sprintf("creates the account %s with the role %s", u.Username, u.Role), nil
			}
			if err := s.Store.CreateUser(u); err != nil {
				return nil, "", err
			}
			s.Store.Audit(&u.ID, u.Username, "user_create", "user:"+u.Username, "created at the first SSO sign-in, role="+u.Role, ip)
			return u, "created", nil
		case 1:
			u = &list[0]
			if u.SSOSubject != "" {
				return nil, "", fmt.Errorf("the account %s is linked to another identity provider account: an admin can unlink it in Users", u.Username)
			}
			u.SSOSubject, changed = key, true
			outcome = "uses the account " + u.Username + " (found by its email address, linked from now on)"
		default:
			return nil, "", fmt.Errorf("several accounts have the email address %s: keep it on one account", id.Email)
		}
	default:
		return nil, "", err
	}
	if u.Disabled {
		return nil, "", fmt.Errorf("the account %s is locked", u.Username)
	}
	if role != "" && role != u.Role {
		if n, _ := s.Store.CountAdmins(); u.Role == store.RoleAdmin && n <= 1 {
			outcome += "; keeps the role admin (the last active admin)"
		} else {
			outcome += fmt.Sprintf("; role %s → %s", u.Role, role)
			if !dry {
				s.Store.Audit(&u.ID, u.Username, "user_update", "user:"+u.Username, fmt.Sprintf("role %s → %s (SSO role mapping)", u.Role, role), ip)
			}
			u.Role, changed = role, true
		}
	}
	if changed && !dry {
		if err := s.Store.UpdateUser(u); err != nil {
			return nil, "", err
		}
	}
	return u, outcome, nil
}

func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) error {
	ip := s.clientIP(r)
	ipKey := "ip:" + ip
	if d := s.IPLimit.Take(ipKey); d > 0 {
		return s.ssoPage(w, http.StatusTooManyRequests, "Sign-in failed",
			fmt.Sprintf("<p>Too many failed sign-in attempts from this address: try again in %d minutes.</p>", int(d.Minutes())+1), "/", "Back to sign in")
	}
	f := s.ssoTake(r)
	http.SetCookie(w, &http.Cookie{Name: ssoStateCookie, Value: "", Path: "/api/auth/sso", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	fail := func(who, msg string) error {
		s.Store.Audit(nil, who, "sso_login_failed", "", msg, ip)
		return s.ssoPage(w, http.StatusForbidden, "Sign-in failed", "<p>"+html.EscapeString(msg)+"</p>", "/", "Back to sign in")
	}
	if f == nil {
		return fail("", "This sign-in has expired or was started in another browser: start again from the sign-in page.")
	}
	q := r.URL.Query()
	var id *ssoIdentity
	var err error
	if e := q.Get("error"); e != "" {
		err = fmt.Errorf("the identity provider did not sign you in: %s %s", e, q.Get("error_description"))
	} else {
		id, err = s.ssoIdentify(r.Context(), f, q.Get("code"))
	}
	if f.test {
		s.IPLimit.Release(ipKey)
		return s.ssoTestPage(w, f, id, err, ip)
	}
	if err != nil {
		return fail("", err.Error())
	}
	u, _, err := s.ssoUser(f.cfg, id, false, ip)
	if err != nil {
		return fail(id.Email, err.Error())
	}
	s.IPLimit.Release(ipKey)
	if u.TOTPEnabled {
		s.ssoPend(w, r, &ssoPending{userID: u.ID, remember: f.remember, issuer: f.cfg.Issuer})
		http.Redirect(w, r, "/", http.StatusFound)
		return nil
	}
	if _, err := s.startSession(w, r, u, f.remember); err != nil {
		return err
	}
	s.Store.Audit(&u.ID, u.Username, "sso_login", "", "via "+f.cfg.Issuer, ip)
	http.Redirect(w, r, "/", http.StatusFound)
	return nil
}

// ---- two-factor code after SSO ----

func (s *Server) ssoPend(w http.ResponseWriter, r *http.Request, p *ssoPending) {
	tok := auth.RandomToken(24)
	p.expires = time.Now().Add(ssoTOTPTTL)
	st := &s.sso
	st.mu.Lock()
	if st.pending == nil {
		st.pending = map[string]*ssoPending{}
	}
	for k, v := range st.pending {
		if time.Now().After(v.expires) || len(st.pending) >= ssoMaxFlows {
			delete(st.pending, k)
		}
	}
	st.pending[auth.SHA256Hex(tok)] = p
	st.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: ssoTOTPCookie, Value: tok, Path: "/api/auth", MaxAge: int(ssoTOTPTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r)})
}

// ssoPendingFrom finds the SSO sign-in of this browser that waits for a two-factor code.
func (s *Server) ssoPendingFrom(r *http.Request) (*ssoPending, string) {
	c, err := r.Cookie(ssoTOTPCookie)
	if err != nil || c.Value == "" {
		return nil, ""
	}
	key := auth.SHA256Hex(c.Value)
	s.sso.mu.Lock()
	defer s.sso.mu.Unlock()
	p := s.sso.pending[key]
	if p == nil || time.Now().After(p.expires) {
		return nil, ""
	}
	return p, key
}

func (s *Server) ssoTOTP(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		TOTP string `json:"totp"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if len(in.TOTP) > 16 {
		return badRequest("incorrect two-factor code")
	}
	ip := s.clientIP(r)
	ipKey := "ip:" + ip
	if d := s.IPLimit.Take(ipKey); d > 0 {
		return errf(429, "too many failed sign-in attempts, try again in %d minutes", int(d.Minutes())+1)
	}
	p, key := s.ssoPendingFrom(r)
	if p == nil {
		return errf(401, "this sign-in has expired: sign in again")
	}
	drop := func() {
		s.sso.mu.Lock()
		delete(s.sso.pending, key)
		s.sso.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: ssoTOTPCookie, Value: "", Path: "/api/auth", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	u, err := s.Store.GetUser(p.userID)
	if err != nil || u.Disabled {
		drop()
		return errf(403, "account is locked")
	}
	userKey := "u:" + strings.ToLower(u.Username) + "|" + ip
	if d := s.Limiter.Take(userKey); d > 0 {
		s.IPLimit.Release(ipKey)
		return errf(429, "too many failed sign-in attempts, try again in %d minutes", int(d.Minutes())+1)
	}
	if u.TOTPEnabled && !s.useTOTP(u.ID, u.TOTPSecret, in.TOTP) {
		s.Store.Audit(&u.ID, u.Username, "login_failed", "", "incorrect two-factor code after SSO", ip)
		return errf(401, "incorrect two-factor code")
	}
	drop()
	s.Limiter.Reset(userKey)
	s.IPLimit.Release(ipKey)
	se, err := s.startSession(w, r, u, p.remember)
	if err != nil {
		return err
	}
	s.Store.Audit(&u.ID, u.Username, "sso_login", "", "via "+p.issuer+", with a two-factor code", ip)
	return ok(w, map[string]any{"user": s.userView(u), "csrf": se.CSRF})
}

// ssoStatus is what the sign-in page needs to know about SSO.
func (s *Server) ssoStatus(r *http.Request, out map[string]any) {
	if c := s.Store.GetSSO(); c.Enabled {
		label := c.ButtonLabel
		if label == "" {
			label = "Sign in with SSO"
		}
		out["sso"] = map[string]any{"enabled": true, "label": label}
	}
	if p, _ := s.ssoPendingFrom(r); p != nil && out["authenticated"] == false {
		out["sso_totp"] = true
	}
}

// ---- pages ----

// ssoPage is a small page for the browser coming back from the provider.
func (s *Server) ssoPage(w http.ResponseWriter, code int, title, body, link, linkText string) error {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	name := html.EscapeString(s.Store.GetSettings().Brand())
	t := html.EscapeString(title)
	_, err := fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`+
		`<title>%s · %s</title><link rel="stylesheet" href="/assets/app.css"></head><body class="oauth-done"><div class="card sso-page"><h2>%s</h2>%s`+
		`<p><a class="btn btn-primary" href="%s">%s</a></p></div></body></html>`, t, name, t, body, html.EscapeString(link), html.EscapeString(linkText))
	return err
}

// ssoTestPage shows an admin what the provider sent and what a sign-in would do.
func (s *Server) ssoTestPage(w http.ResponseWriter, f *ssoFlow, id *ssoIdentity, err error, ip string) error {
	esc := html.EscapeString
	var b strings.Builder
	row := func(k, v string) { fmt.Fprintf(&b, "<tr><th>%s</th><td>%s</td></tr>", esc(k), esc(v)) }
	title, result := "SSO test passed", ""
	if err != nil {
		title = "SSO test failed"
		fmt.Fprintf(&b, `<p class="alert bad">%s</p>`, esc(err.Error()))
		result = err.Error()
	} else {
		_, outcome, uerr := s.ssoUser(f.cfg, id, true, ip)
		if uerr != nil {
			title = "SSO works, but this account would be refused"
			result = uerr.Error()
			fmt.Fprintf(&b, `<p class="alert warn">%s</p>`, esc(uerr.Error()))
		} else {
			result = "signing in " + outcome
			fmt.Fprintf(&b, `<p class="alert good">The ID token was verified. Signing in with this provider account %s.</p>`, esc(outcome))
		}
		b.WriteString(`<table>`)
		row("Issuer", id.Issuer)
		row("Subject", id.Subject)
		row("Email ("+ssoEmailClaim(f.cfg)+")", id.Email)
		if id.EmailVerified != nil {
			row("Email verified", fmt.Sprint(*id.EmailVerified))
		}
		row("Name", id.Name)
		row("Username", id.Username)
		if f.cfg.RoleClaim != "" {
			row("Claim "+f.cfg.RoleClaim, strings.Join(id.Claims.Strings(f.cfg.RoleClaim), ", "))
		}
		b.WriteString(`</table>`)
		names := make([]string, 0, len(id.Claims))
		for k := range id.Claims {
			names = append(names, k)
		}
		sort.Strings(names)
		js, _ := json.MarshalIndent(id.Claims, "", "  ")
		fmt.Fprintf(&b, `<details><summary>All claims (%s)</summary><pre class="code">%s</pre></details>`, esc(strings.Join(names, ", ")), esc(string(js)))
	}
	b.WriteString(`<p class="muted">Nobody was signed in. You can close this tab.</p>`)
	s.Store.Audit(nil, f.tester, "sso_test", f.cfg.Issuer, result, ip)
	return s.ssoPage(w, http.StatusOK, title, b.String(), "/#/settings", "Back to Settings")
}

// ---- settings ----

type ssoSettingsView struct {
	store.SSOConfig
	RedirectURL string `json:"redirect_url"`
}

func (s *Server) ssoSettingsGet(w http.ResponseWriter, r *http.Request) error {
	c := s.Store.GetSSO()
	if c.ClientSecret != "" {
		c.ClientSecret = storage.SecretMask
	}
	if c.Scopes == "" {
		c.Scopes = ssoDefaultScope
	}
	return ok(w, ssoSettingsView{SSOConfig: c, RedirectURL: s.ssoRedirectURL(r)})
}

var claimRe = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,120}$`)
var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// ssoConfigFrom reads SSO settings from a request: fields it leaves out keep their saved value,
// and the masked client secret stands for the saved one.
func (s *Server) ssoConfigFrom(r *http.Request) (store.SSOConfig, error) {
	old := s.Store.GetSSO()
	c := old
	if err := readJSON(r, &c); err != nil {
		return c, err
	}
	if c.ClientSecret == storage.SecretMask {
		c.ClientSecret = old.ClientSecret
	}
	c.Issuer = strings.TrimSpace(c.Issuer)
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	if c.Issuer != "" {
		if err := oidc.CheckURL(c.Issuer); err != nil {
			return c, badRequest("issuer URL: %v", err)
		}
		if u, _ := url.Parse(c.Issuer); u.RawQuery != "" || u.Fragment != "" {
			return c, badRequest("the issuer URL cannot have a query or a fragment")
		}
	}
	sc := strings.Fields(c.Scopes)
	hasOpenID := false
	for _, v := range sc {
		hasOpenID = hasOpenID || v == "openid"
	}
	if !hasOpenID {
		sc = append([]string{"openid"}, sc...)
	}
	c.Scopes = strings.Join(sc, " ")
	c.ButtonLabel = strings.TrimSpace(c.ButtonLabel)
	if len([]rune(c.ButtonLabel)) > 60 || strings.ContainsFunc(c.ButtonLabel, unicode.IsControl) {
		return c, badRequest("the button label must be one line of at most 60 characters")
	}
	c.EmailClaim, c.RoleClaim = strings.TrimSpace(c.EmailClaim), strings.TrimSpace(c.RoleClaim)
	if c.EmailClaim == "email" {
		c.EmailClaim = ""
	}
	for _, v := range []string{c.EmailClaim, c.RoleClaim} {
		if v != "" && !claimRe.MatchString(v) {
			return c, badRequest("invalid claim name %q", v)
		}
	}
	ds := ssoDomains(c.AllowedDomains)
	for _, d := range ds {
		if !domainRe.MatchString(d) {
			return c, badRequest("invalid email domain %q", d)
		}
	}
	c.AllowedDomains = strings.Join(ds, ", ")
	if c.DefaultRole == "" {
		c.DefaultRole = store.RoleViewer
	}
	if !store.ValidRole(c.DefaultRole) {
		return c, badRequest("invalid default role")
	}
	for _, v := range []*string{&c.AdminValues, &c.OperatorValues, &c.ViewerValues} {
		*v = strings.Join(ssoList(*v), ", ")
		if len(*v) > 2000 {
			return c, badRequest("the role values are too long")
		}
	}
	return c, nil
}

func (s *Server) ssoSettingsPut(w http.ResponseWriter, r *http.Request) error {
	c, err := s.ssoConfigFrom(r)
	if err != nil {
		return err
	}
	if c.Enabled {
		if c.Issuer == "" || c.ClientID == "" {
			return badRequest("enter the issuer URL and the client ID to turn single sign-on on")
		}
		if _, err := s.ssoProvider(r.Context(), c.Issuer, true); err != nil {
			return badRequest("the identity provider cannot be used: %v", err)
		}
	}
	if err := s.Store.SaveSSO(c); err != nil {
		return err
	}
	s.audit(r, "sso_settings_update", "settings", fmt.Sprintf("enabled=%v issuer=%s", c.Enabled, c.Issuer))
	if c.ClientSecret != "" {
		c.ClientSecret = storage.SecretMask
	}
	return ok(w, ssoSettingsView{SSOConfig: c, RedirectURL: s.ssoRedirectURL(r)})
}

// ssoTest starts a test sign-in with the settings of the form (saved or not); the admin opens the
// URL it answers, and the callback shows the result instead of signing anyone in.
func (s *Server) ssoTest(w http.ResponseWriter, r *http.Request) error {
	c, err := s.ssoConfigFrom(r)
	if err != nil {
		return err
	}
	if c.Issuer == "" || c.ClientID == "" {
		return badRequest("enter the issuer URL and the client ID first")
	}
	p, err := s.ssoProvider(r.Context(), c.Issuer, true)
	if err != nil {
		return badRequest("the identity provider cannot be used: %v", err)
	}
	u := s.ssoBegin(w, r, &ssoFlow{cfg: c, provider: p, test: true, tester: userOf(r).Username})
	return ok(w, map[string]string{"url": u, "redirect_url": s.ssoRedirectURL(r)})
}
