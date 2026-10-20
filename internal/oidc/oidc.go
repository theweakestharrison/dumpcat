// Package oidc signs users in with an OpenID Connect provider: discovery, the provider's signing
// keys (JWKS) and ID token verification, with the standard library only. The authorization code
// exchange itself (with PKCE) goes through golang.org/x/oauth2.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Provider is an issuer's discovery document and its signing keys.
type Provider struct {
	Issuer      string `json:"issuer"`
	AuthURL     string `json:"authorization_endpoint"`
	TokenURL    string `json:"token_endpoint"`
	UserinfoURL string `json:"userinfo_endpoint"`
	JWKSURL     string `json:"jwks_uri"`

	client *http.Client
	mu     sync.Mutex
	keys   []JWK
	keysAt time.Time
}

// Skew is the clock difference allowed between DumpCat and the provider.
const Skew = 2 * time.Minute

const maxBody = 1 << 20

// CheckURL accepts https URLs, and http only on this machine (a test provider).
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || net.ParseIP(h).IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("%s must use https", raw)
}

func getJSON(ctx context.Context, client *http.Client, u string, header http.Header, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d %s", u, resp.StatusCode, strings.TrimSpace(string(b[:min(len(b), 200)])))
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %v", u, err)
	}
	return nil
}

// Discover reads issuer/.well-known/openid-configuration. The document must name the same issuer
// (a trailing slash aside): ID tokens are then checked against the issuer it names.
func Discover(ctx context.Context, client *http.Client, issuer string) (*Provider, error) {
	issuer = strings.TrimSpace(issuer)
	if err := CheckURL(issuer); err != nil {
		return nil, fmt.Errorf("issuer: %v", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	p := &Provider{client: client}
	if err := getJSON(ctx, client, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil, p); err != nil {
		return nil, fmt.Errorf("discovery: %v", err)
	}
	if strings.TrimRight(p.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return nil, fmt.Errorf("discovery: the provider says its issuer is %q, not %q", p.Issuer, issuer)
	}
	for name, u := range map[string]string{"authorization_endpoint": p.AuthURL, "token_endpoint": p.TokenURL, "jwks_uri": p.JWKSURL} {
		if u == "" {
			return nil, fmt.Errorf("discovery: no %s", name)
		}
		if err := CheckURL(u); err != nil {
			return nil, fmt.Errorf("discovery: %s: %v", name, err)
		}
	}
	if p.UserinfoURL != "" && CheckURL(p.UserinfoURL) != nil {
		p.UserinfoURL = ""
	}
	return p, nil
}

// JWK is one key of the provider's key set.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// PublicKey decodes the key.
func (k JWK) PublicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err1 := b64(k.N)
		e, err2 := b64(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad RSA key")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("RSA key shorter than 2048 bits")
		}
		return pub, nil
	case "EC":
		var c elliptic.Curve
		switch k.Crv {
		case "P-256":
			c = elliptic.P256()
		case "P-384":
			c = elliptic.P384()
		case "P-521":
			c = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err1 := b64(k.X)
		y, err2 := b64(k.Y)
		size := (c.Params().BitSize + 7) / 8
		if err1 != nil || err2 != nil || len(x) != size || len(y) != size {
			return nil, errors.New("bad EC key")
		}
		return ecdsa.ParseUncompressedPublicKey(c, append(append([]byte{4}, x...), y...))
	case "OKP":
		x, err := b64(k.X)
		if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("bad OKP key")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, fmt.Errorf("unsupported key type %q", k.Kty)
}

// signing algorithms accepted for ID tokens: never "none" or a shared-secret (HS*) one
var algs = map[string]bool{"RS256": true, "RS384": true, "RS512": true, "PS256": true, "PS384": true, "PS512": true,
	"ES256": true, "ES384": true, "ES512": true, "EdDSA": true}

func verifySig(alg string, key crypto.PublicKey, signed, sig []byte) error {
	var h crypto.Hash
	switch alg[2:] {
	case "256":
		h = crypto.SHA256
	case "384":
		h = crypto.SHA384
	case "512":
		h = crypto.SHA512
	}
	digest := func() []byte {
		switch h {
		case crypto.SHA384:
			d := sha512.Sum384(signed)
			return d[:]
		case crypto.SHA512:
			d := sha512.Sum512(signed)
			return d[:]
		}
		d := sha256.Sum256(signed)
		return d[:]
	}
	bad := errors.New("the ID token's signature is invalid")
	switch alg[:2] {
	case "RS", "PS":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("the key does not match the token's algorithm")
		}
		var err error
		if alg[0] == 'R' {
			err = rsa.VerifyPKCS1v15(pub, h, digest(), sig)
		} else {
			err = rsa.VerifyPSS(pub, h, digest(), sig, nil)
		}
		if err != nil {
			return bad
		}
		return nil
	case "ES":
		pub, ok := key.(*ecdsa.PublicKey)
		want := map[string]int{"ES256": 256, "ES384": 384, "ES512": 521}[alg]
		if !ok || pub.Curve.Params().BitSize != want {
			return errors.New("the key does not match the token's algorithm")
		}
		size := (want + 7) / 8
		if len(sig) != 2*size {
			return bad
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(pub, digest(), r, s) {
			return bad
		}
		return nil
	case "Ed":
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return errors.New("the key does not match the token's algorithm")
		}
		if !ed25519.Verify(pub, signed, sig) {
			return bad
		}
		return nil
	}
	return fmt.Errorf("unsupported algorithm %s", alg)
}

// key finds the signing key of a token, fetching the key set again (at most once a minute) when
// the provider has rotated its keys.
func (p *Provider) key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 || p.keys == nil {
			if attempt == 1 && time.Since(p.keysAt) < time.Minute {
				break
			}
			var set struct {
				Keys []JWK `json:"keys"`
			}
			if err := getJSON(ctx, p.client, p.JWKSURL, nil, &set); err != nil {
				return nil, fmt.Errorf("signing keys: %v", err)
			}
			p.keys, p.keysAt = set.Keys, time.Now()
		}
		var found []JWK
		for _, k := range p.keys {
			if (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != alg) {
				continue
			}
			if kid != "" && k.Kid != kid {
				continue
			}
			want := map[string]string{"RS": "RSA", "PS": "RSA", "ES": "EC", "Ed": "OKP"}[alg[:2]]
			if k.Kty == want {
				found = append(found, k)
			}
		}
		if len(found) == 1 || (kid != "" && len(found) > 0) {
			return found[0].PublicKey()
		}
		if len(found) > 1 {
			return nil, errors.New("the ID token names no key (kid) and the provider has several")
		}
	}
	return nil, fmt.Errorf("the provider has no key %q for %s", kid, alg)
}

// Claims are the claims of an ID token (or of the userinfo answer).
type Claims map[string]any

// lookup finds a claim; a dotted name looks into objects (Keycloak's realm_access.roles).
func (c Claims) lookup(name string) (any, bool) {
	if v, ok := c[name]; ok {
		return v, true
	}
	var cur any = map[string]any(c)
	for _, part := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// String is a claim's text ("" when missing or not a string).
func (c Claims) String(name string) string {
	v, _ := c.lookup(name)
	s, _ := v.(string)
	return s
}

// Strings is a claim holding one or several values (a group list).
func (c Claims) Strings(name string) []string {
	v, _ := c.lookup(name)
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Bool is a boolean claim; ok is false when it is missing (some providers send "true").
func (c Claims) Bool(name string) (v, ok bool) {
	switch t := c[name].(type) {
	case bool:
		return t, true
	case string:
		return t == "true", t == "true" || t == "false"
	}
	return false, false
}

func (c Claims) time(name string) (time.Time, bool) {
	switch t := c[name].(type) {
	case float64:
		return time.Unix(int64(t), 0), true
	case json.Number:
		n, err := t.Int64()
		return time.Unix(n, 0), err == nil
	}
	return time.Time{}, false
}

// Verify checks an ID token: its signature with the provider's key, the issuer, that it is meant
// for this client, its validity period and the nonce of the sign-in it ends.
func (p *Provider) Verify(ctx context.Context, raw, clientID, nonce string, now time.Time) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("the ID token is not a signed JWT")
	}
	hb, err := b64(parts[0])
	if err != nil {
		return nil, errors.New("the ID token's header cannot be read")
	}
	var hdr struct {
		Alg  string `json:"alg"`
		Kid  string `json:"kid"`
		Crit []any  `json:"crit"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, errors.New("the ID token's header cannot be read")
	}
	if !algs[hdr.Alg] {
		return nil, fmt.Errorf("the ID token is signed with %q, which is not accepted", hdr.Alg)
	}
	if len(hdr.Crit) > 0 {
		return nil, errors.New("the ID token has critical header parameters")
	}
	sig, err := b64(parts[2])
	if err != nil {
		return nil, errors.New("the ID token's signature cannot be read")
	}
	key, err := p.key(ctx, hdr.Kid, hdr.Alg)
	if err != nil {
		return nil, err
	}
	if err := verifySig(hdr.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	pb, err := b64(parts[1])
	if err != nil {
		return nil, errors.New("the ID token's claims cannot be read")
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, errors.New("the ID token's claims cannot be read")
	}
	if iss := c.String("iss"); iss != p.Issuer {
		return nil, fmt.Errorf("the ID token was issued by %q, not %q", iss, p.Issuer)
	}
	aud := c.Strings("aud")
	found := false
	for _, a := range aud {
		found = found || a == clientID
	}
	if !found {
		return nil, fmt.Errorf("the ID token is meant for %v, not this client (%s)", aud, clientID)
	}
	if azp := c.String("azp"); (len(aud) > 1 || azp != "") && azp != clientID {
		return nil, fmt.Errorf("the ID token was requested by %q, not this client", azp)
	}
	exp, ok := c.time("exp")
	if !ok {
		return nil, errors.New("the ID token has no expiry")
	}
	if now.After(exp.Add(Skew)) {
		return nil, fmt.Errorf("the ID token expired at %s", exp.UTC().Format(time.RFC3339))
	}
	if iat, ok := c.time("iat"); !ok || iat.After(now.Add(Skew)) {
		return nil, errors.New("the ID token has no or a future issue time (check the clocks)")
	}
	if nbf, ok := c.time("nbf"); ok && nbf.After(now.Add(Skew)) {
		return nil, errors.New("the ID token is not valid yet (check the clocks)")
	}
	if got := c.String("nonce"); subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 || nonce == "" {
		return nil, errors.New("the ID token's nonce does not match this sign-in")
	}
	if c.String("sub") == "" {
		return nil, errors.New("the ID token has no subject")
	}
	return c, nil
}

// Userinfo asks the provider for the user's claims with the access token.
func (p *Provider) Userinfo(ctx context.Context, accessToken string) (Claims, error) {
	if p.UserinfoURL == "" || accessToken == "" {
		return nil, nil
	}
	var c Claims
	h := http.Header{"Authorization": {"Bearer " + accessToken}}
	if err := getJSON(ctx, p.client, p.UserinfoURL, h, &c); err != nil {
		return nil, fmt.Errorf("userinfo: %v", err)
	}
	return c, nil
}
