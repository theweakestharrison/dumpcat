package oidc_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dumpcat/internal/oidc"
	"dumpcat/internal/oidc/oidctest"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwt(alg, kid string, claims map[string]any, sign func(signed []byte) []byte) string {
	h, _ := json.Marshal(map[string]any{"alg": alg, "kid": kid})
	p, _ := json.Marshal(claims)
	s := b64(h) + "." + b64(p)
	return s + "." + b64(sign([]byte(s)))
}

// keyServer serves discovery and a key set that a test can change.
type keyServer struct {
	*httptest.Server
	mu   sync.Mutex
	keys []map[string]any
	hits int
}

func newKeyServer(t *testing.T) *keyServer {
	ks := &keyServer{}
	mux := http.NewServeMux()
	ks.Server = httptest.NewServer(mux)
	t.Cleanup(ks.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": ks.URL, "authorization_endpoint": ks.URL + "/a", "token_endpoint": ks.URL + "/t", "jwks_uri": ks.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		ks.hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": ks.keys})
	})
	return ks
}

func (ks *keyServer) add(k map[string]any) {
	ks.mu.Lock()
	ks.keys = append(ks.keys, k)
	ks.mu.Unlock()
}

func rsaJWK(k *rsa.PrivateKey, kid string) map[string]any {
	return map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}
}

func TestVerify(t *testing.T) {
	ks := newKeyServer(t)
	rk := oidctest.New("x", "y").Key // the shared test RSA key
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, edKey, _ := ed25519.GenerateKey(rand.Reader)
	ks.add(rsaJWK(rk, "rsa"))
	ks.add(map[string]any{"kty": "EC", "kid": "ec", "crv": "P-256", "x": b64(ek.X.FillBytes(make([]byte, 32))), "y": b64(ek.Y.FillBytes(make([]byte, 32)))})
	ks.add(map[string]any{"kty": "OKP", "kid": "ed", "crv": "Ed25519", "x": b64(edPub)})

	ctx := context.Background()
	p, err := oidc.Discover(ctx, nil, ks.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := func(over map[string]any) map[string]any {
		c := map[string]any{"iss": ks.URL, "aud": "client", "sub": "u1", "nonce": "n0nce", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
		for k, v := range over {
			if v == nil {
				delete(c, k)
			} else {
				c[k] = v
			}
		}
		return c
	}
	rs256 := func(c map[string]any) string { return oidctest.SignRS256(rk, "rsa", c) }
	ps256 := func(signed []byte) []byte {
		sum := sha256.Sum256(signed)
		sig, _ := rsa.SignPSS(rand.Reader, rk, crypto.SHA256, sum[:], nil)
		return sig
	}
	es256 := func(signed []byte) []byte {
		sum := sha256.Sum256(signed)
		r, s, _ := ecdsa.Sign(rand.Reader, ek, sum[:])
		return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	}

	good := map[string]string{
		"RS256":             rs256(claims(nil)),
		"PS256":             jwt("PS256", "rsa", claims(nil), ps256),
		"ES256":             jwt("ES256", "ec", claims(nil), es256),
		"EdDSA":             jwt("EdDSA", "ed", claims(nil), func(s []byte) []byte { return ed25519.Sign(edKey, s) }),
		"aud list with azp": rs256(claims(map[string]any{"aud": []string{"client", "other"}, "azp": "client"})),
		"clock skew":        rs256(claims(map[string]any{"exp": now.Add(-time.Minute).Unix(), "iat": now.Add(time.Minute).Unix()})),
	}
	for name, tok := range good {
		c, err := p.Verify(ctx, tok, "client", "n0nce", now)
		if err != nil || c.String("sub") != "u1" {
			t.Errorf("%s: %v", name, err)
		}
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	bad := map[string]struct{ tok, want string }{
		"alg none":         {jwt("none", "rsa", claims(nil), func([]byte) []byte { return nil }), `"none"`},
		"HS256 with key":   {jwt("HS256", "rsa", claims(nil), func(s []byte) []byte { m := hmac.New(sha256.New, rk.N.Bytes()); m.Write(s); return m.Sum(nil) }), `"HS256"`},
		"other key":        {oidctest.SignRS256(other, "rsa", claims(nil)), "signature is invalid"},
		"alg/key mix":      {jwt("ES256", "rsa", claims(nil), es256), "no key"},
		"tampered":         {strings.Replace(rs256(claims(nil)), ".", ".e30", 1), "signature"},
		"wrong issuer":     {rs256(claims(map[string]any{"iss": "https://evil.example"})), "issued by"},
		"wrong audience":   {rs256(claims(map[string]any{"aud": "someone-else"})), "meant for"},
		"aud list, no azp": {rs256(claims(map[string]any{"aud": []string{"client", "other"}})), "requested by"},
		"other azp":        {rs256(claims(map[string]any{"azp": "other"})), "requested by"},
		"expired":          {rs256(claims(map[string]any{"exp": now.Add(-time.Hour).Unix()})), "expired"},
		"no expiry":        {rs256(claims(map[string]any{"exp": nil})), "no expiry"},
		"future iat":       {rs256(claims(map[string]any{"iat": now.Add(time.Hour).Unix()})), "future issue time"},
		"not yet valid":    {rs256(claims(map[string]any{"nbf": now.Add(time.Hour).Unix()})), "not valid yet"},
		"other nonce":      {rs256(claims(map[string]any{"nonce": "replayed"})), "nonce"},
		"no nonce":         {rs256(claims(map[string]any{"nonce": nil})), "nonce"},
		"no subject":       {rs256(claims(map[string]any{"sub": nil})), "no subject"},
		"not a JWT":        {"abc.def", "not a signed JWT"},
	}
	for name, c := range bad {
		if _, err := p.Verify(ctx, c.tok, "client", "n0nce", now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}

	// key rotation: an unknown kid reads the key set again, at most once a minute
	rot, _ := rsa.GenerateKey(rand.Reader, 2048)
	ks.add(rsaJWK(rot, "rotated"))
	hits := ks.hits
	if _, err := p.Verify(ctx, oidctest.SignRS256(rot, "rotated", claims(nil)), "client", "n0nce", now); err == nil || ks.hits != hits {
		t.Fatalf("refetched within a minute: %v, %d hits", err, ks.hits-hits)
	}
	p2, _ := oidc.Discover(ctx, nil, ks.URL)
	if _, err := p2.Verify(ctx, oidctest.SignRS256(rot, "rotated", claims(nil)), "client", "n0nce", now); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	ks := newKeyServer(t)
	ctx := context.Background()
	if _, err := oidc.Discover(ctx, nil, "http://idp.example.com"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http to another host: %v", err)
	}
	if _, err := oidc.Discover(ctx, nil, ks.URL+"/realms/other"); err == nil {
		t.Fatal("a document of another issuer was accepted")
	}
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": "https://accounts.google.com", "authorization_endpoint": "x", "token_endpoint": "x", "jwks_uri": "x"})
	}))
	defer evil.Close()
	if _, err := oidc.Discover(ctx, nil, evil.URL); err == nil || !strings.Contains(err.Error(), "says its issuer") {
		t.Fatalf("issuer mismatch: %v", err)
	}
}

func TestClaims(t *testing.T) {
	var c oidc.Claims
	_ = json.Unmarshal([]byte(`{"groups":["a","b",3],"role":"admin","realm_access":{"roles":["dumpcat-admin"]},"email_verified":"true","n":1}`), &c)
	if g := c.Strings("groups"); len(g) != 2 || g[1] != "b" {
		t.Fatalf("groups %v", g)
	}
	if r := c.Strings("role"); len(r) != 1 || r[0] != "admin" {
		t.Fatalf("role %v", r)
	}
	if r := c.Strings("realm_access.roles"); len(r) != 1 || r[0] != "dumpcat-admin" {
		t.Fatalf("nested %v", r)
	}
	if v, ok := c.Bool("email_verified"); !v || !ok {
		t.Fatal("string bool")
	}
	if _, ok := c.Bool("missing"); ok || c.String("n") != "" || c.Strings("missing") != nil {
		t.Fatal("missing claims")
	}
}
