// Package oidctest is a fake OpenID Connect provider for tests: discovery, a key set, a token
// endpoint that checks the client and the PKCE verifier, and userinfo.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Issuer is a running fake provider.
type Issuer struct {
	*httptest.Server
	ClientID, ClientSecret string
	Key                    *rsa.PrivateKey
	Kid                    string

	mu     sync.Mutex
	grants map[string]*grant // by code
	tokens map[string]*grant // by access token
	// Exchanges counts the token requests that succeeded.
	Exchanges int
}

type grant struct {
	challenge, redirect string
	idToken             string
	userinfo            map[string]any
}

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
)

// testKey is generated once: RSA key generation is slow.
func testKey() *rsa.PrivateKey {
	keyOnce.Do(func() {
		var err error
		if key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return key
}

// New starts a provider for one client; Close stops it.
func New(clientID, clientSecret string) *Issuer {
	is := &Issuer{ClientID: clientID, ClientSecret: clientSecret, Key: testKey(), Kid: "test-key-1",
		grants: map[string]*grant{}, tokens: map[string]*grant{}}
	mux := http.NewServeMux()
	is.Server = httptest.NewServer(mux)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"issuer": is.URL, "authorization_endpoint": is.URL + "/authorize",
			"token_endpoint": is.URL + "/token", "userinfo_endpoint": is.URL + "/userinfo", "jwks_uri": is.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := is.Key.PublicKey
		writeJSON(w, 200, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": is.Kid, "use": "sig", "alg": "RS256",
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes())}}})
	})
	mux.HandleFunc("GET /authorize", is.authorize)
	mux.HandleFunc("POST /token", is.token)
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		is.mu.Lock()
		g := is.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		is.mu.Unlock()
		if g == nil {
			writeJSON(w, 401, map[string]any{"error": "invalid_token"})
			return
		}
		writeJSON(w, 200, g.userinfo)
	})
	return is
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Sign makes a JWT signed with the provider's key.
func (is *Issuer) Sign(claims map[string]any) string {
	return SignRS256(is.Key, is.Kid, claims)
}

// SignRS256 makes a JWT with any RSA key.
func SignRS256(k *rsa.PrivateKey, kid string, claims map[string]any) string {
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	signed := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signed + "." + b64(sig)
}

// Claims are the claims of a valid ID token for a sign-in: overrides replace them (nil deletes one).
func (is *Issuer) Claims(nonce string, overrides map[string]any) map[string]any {
	now := time.Now()
	c := map[string]any{"iss": is.URL, "aud": is.ClientID, "sub": "user-1", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"nonce": nonce, "email": "alice@example.com", "email_verified": true, "name": "Alice Example", "preferred_username": "alice"}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// Authorize plays the user signing in at the provider: from the authorization URL the app
// redirected to, it returns the code and state the provider sends back. claims change the ID
// token (see Claims); userinfo is what the userinfo endpoint answers besides sub.
func (is *Issuer) Authorize(authURL string, claims, userinfo map[string]any) (code, state string, err error) {
	u, err := url.Parse(authURL)
	if err != nil {
		return "", "", err
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != is.ClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" || !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		return "", "", fmt.Errorf("bad authorization request: %s", u.RawQuery)
	}
	c := is.Claims(q.Get("nonce"), claims)
	ui := map[string]any{"sub": c["sub"]}
	for k, v := range userinfo {
		ui[k] = v
	}
	code = b64([]byte(fmt.Sprintf("code-%d-%s", time.Now().UnixNano(), q.Get("state")[:6])))
	is.mu.Lock()
	is.grants[code] = &grant{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), idToken: is.Sign(c), userinfo: ui}
	is.mu.Unlock()
	return code, q.Get("state"), nil
}

// authorize is the provider's sign-in page for a browser: a form asks who signs in (an email
// address and groups), then the browser goes back to the app with a code.
func (is *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	email := q.Get("as")
	if email == "" {
		var hidden strings.Builder
		for k, vs := range q {
			for _, v := range vs {
				fmt.Fprintf(&hidden, `<input type="hidden" name="%s" value="%s">`, html.EscapeString(k), html.EscapeString(v))
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Fake OpenID provider</title></head><body style="font-family:sans-serif;margin:40px">`+
			`<h1>Fake OpenID provider</h1><p>Client <b>%s</b> asks you to sign in.</p><form method="get" action="/authorize">%s`+
			`<p><label>Email <input name="as" value="alice@example.com" size="30"></label></p>`+
			`<p><label>Groups <input name="groups" placeholder="comma-separated" size="30"></label></p>`+
			`<p><button type="submit">Sign in</button></p></form></body></html>`, html.EscapeString(q.Get("client_id")), hidden.String())
		return
	}
	claims := map[string]any{"email": email, "sub": "sub-" + email, "preferred_username": strings.Split(email, "@")[0]}
	var info map[string]any
	if g := strings.TrimSpace(q.Get("groups")); g != "" {
		info = map[string]any{"groups": strings.Split(g, ",")}
	}
	q.Del("as")
	q.Del("groups")
	code, state, err := is.Authorize(is.URL+"/authorize?"+q.Encode(), claims, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || back.Scheme == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", state)
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (is *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid_request"})
		return
	}
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != is.ClientID || secret != is.ClientSecret {
		writeJSON(w, 401, map[string]any{"error": "invalid_client"})
		return
	}
	is.mu.Lock()
	g := is.grants[r.PostForm.Get("code")]
	is.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case r.PostForm.Get("grant_type") != "authorization_code" || g == nil:
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "unknown or used code"})
		return
	case g.redirect != r.PostForm.Get("redirect_uri"):
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "redirect_uri differs"})
		return
	case b64(sum[:]) != g.challenge:
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "PKCE verification failed"})
		return
	}
	at := b64([]byte(fmt.Sprintf("at-%d", time.Now().UnixNano())))
	is.mu.Lock()
	if is.grants[r.PostForm.Get("code")] == nil { // redeemed meanwhile: a code works once
		is.mu.Unlock()
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "unknown or used code"})
		return
	}
	delete(is.grants, r.PostForm.Get("code"))
	is.tokens[at] = g
	is.Exchanges++
	is.mu.Unlock()
	writeJSON(w, 200, map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": 300, "id_token": g.idToken})
}
