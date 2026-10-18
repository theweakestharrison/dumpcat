package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// manifestFor returns latest.json for a release whose only binary is bin.
func manifestFor(version string, bin []byte, extra ...string) []byte {
	h := sha256.Sum256(bin)
	m := map[string]any{"version": version, "files": map[string]any{
		Platform(): map[string]any{"url": version + "/dumpcat-bin", "sha256": hex.EncodeToString(h[:]), "size": len(bin)}}}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return b
}

// signedServer serves manifest as latest.json, sig as latest.json.sig (404 when
// nil) and bin; it counts the requests for the signature.
func signedServer(t *testing.T, version string, bin, manifest, sig []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("/rel/latest.json", func(w http.ResponseWriter, r *http.Request) { w.Write(manifest) })
	mux.HandleFunc("/rel/latest.json.sig", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if sig == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(sig)
	})
	mux.HandleFunc("/rel/"+version+"/dumpcat-bin", func(w http.ResponseWriter, r *http.Request) { w.Write(bin) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hits
}

func noRestart(t *testing.T, e *env) {
	t.Helper()
	select {
	case <-e.restarts:
		t.Fatal("restarted into a refused release")
	default:
	}
	if read(t, e.exe) != "old binary" {
		t.Fatal("the binary was replaced")
	}
	if _, err := os.Stat(NewPath(e.exe)); err == nil {
		t.Fatal("the refused binary was downloaded")
	}
}

func TestSignedReleaseInstalls(t *testing.T) {
	pub, priv := newKey(t)
	bin := []byte("new binary")
	m := manifestFor("1.1.0", bin)
	srv, _ := signedServer(t, "1.1.0", bin, m, Sign(priv, m))
	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	e.m.PublicKeys = []ed25519.PublicKey{pub}
	if st := e.m.Status(); !st.Signing.Required || len(st.Signing.Keys) != 1 || st.Signing.Keys[0] != EncodePublicKey(pub) {
		t.Fatalf("signing status %+v", st.Signing)
	}
	if err := e.m.Install("", false, "admin"); err != nil {
		t.Fatal(err)
	}
	p := e.waitRestart(t)
	if got := e.m.Status().Latest.SignedBy; got != EncodePublicKey(pub) {
		t.Fatalf("signed_by = %q", got)
	}
	if err := Apply(p); err != nil {
		t.Fatal(err)
	}
	if read(t, e.exe) != "new binary" {
		t.Fatal("binary not replaced")
	}

	// the same through the offline path
	o := &Offline{Version: "1.0.0", PublicKeys: []ed25519.PublicKey{pub}}
	rel, err := o.Check(context.Background(), srv.URL+"/rel")
	if err != nil || rel.SignedBy != EncodePublicKey(pub) {
		t.Fatalf("offline check: %v %+v", err, rel)
	}
}

func TestUnsignedReleaseRefused(t *testing.T) {
	pub, _ := newKey(t)
	keys := []ed25519.PublicKey{pub}
	bin := []byte("evil binary")
	m := manifestFor("2.0.0", bin)
	srv, _ := signedServer(t, "2.0.0", bin, m, nil)

	_, err := Fetch(context.Background(), srv.URL+"/rel", keys)
	if err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("Fetch: %v", err)
	}

	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	e.m.PublicKeys = keys
	if err := e.m.Install("", false, "admin"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e.m, "failed")
	if st := e.m.Status(); !strings.Contains(st.State.Error, "not signed") || st.Latest != nil {
		t.Fatalf("state %+v latest %+v", st.State, st.Latest)
	}
	noRestart(t, e)

	o := &Offline{Version: "1.0.0", DataDir: e.data, DBPath: e.db, Exe: e.exe, PublicKeys: keys,
		Snapshot: func(string) error { t.Fatal("snapshot of a refused release"); return nil }}
	if _, err := o.Check(context.Background(), srv.URL+"/rel"); err == nil {
		t.Fatal("offline check accepted an unsigned release")
	}
	// a release that was fetched without verification cannot be installed either
	rel, err := Fetch(context.Background(), srv.URL+"/rel", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Install(context.Background(), rel, "test", nil); err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("offline install of an unverified release: %v", err)
	}
	noRestart(t, e)
}

func TestInvalidSignatureRefused(t *testing.T) {
	pub, priv := newKey(t)
	_, other := newKey(t)
	bin := []byte("evil binary")
	m := manifestFor("2.0.0", bin)
	good := Sign(priv, m)
	cases := map[string][]byte{
		"signed with another key": Sign(other, m),
		"signature of other data": Sign(priv, []byte(`{"version":"2.0.0"}`)),
		"not base64":              []byte("this is not a signature\n"),
		"truncated":               good[:40],
		"empty":                   {},
		"too many lines":          []byte(strings.Repeat(string(Sign(other, m)), maxSignatures) + string(good)),
	}
	for name, sig := range cases {
		srv, _ := signedServer(t, "2.0.0", bin, m, sig)
		rel, err := Fetch(context.Background(), srv.URL+"/rel", []ed25519.PublicKey{pub})
		if err == nil || !strings.HasPrefix(err.Error(), "release refused") {
			t.Errorf("%s: rel %+v, err %v", name, rel, err)
		}
	}
	// the good signature next to a bad one is fine
	srv, _ := signedServer(t, "2.0.0", bin, m, append(Sign(other, m), good...))
	if _, err := Fetch(context.Background(), srv.URL+"/rel", []ed25519.PublicKey{pub}); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedManifestRefused(t *testing.T) {
	pub, priv := newKey(t)
	keys := []ed25519.PublicKey{pub}
	good, evil := []byte("new binary"), []byte("evil binary")
	m := manifestFor("1.1.0", good)
	sig := Sign(priv, m)
	// a compromised server swaps the binary and its checksum but cannot re-sign
	tampered := manifestFor("1.1.0", evil)
	srv, _ := signedServer(t, "1.1.0", evil, tampered, sig)

	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	e.m.PublicKeys = keys
	if err := e.m.Install("", false, "admin"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e.m, "failed")
	if err := e.m.Status().State.Error; !strings.Contains(err, "not signed by a trusted key") {
		t.Fatalf("error %q", err)
	}
	noRestart(t, e)

	o := &Offline{Version: "1.0.0", PublicKeys: keys}
	if _, err := o.Check(context.Background(), srv.URL+"/rel"); err == nil {
		t.Fatal("offline check accepted a tampered latest.json")
	}
	// any byte counts, even white space
	for _, mod := range [][]byte{append(append([]byte{}, m...), '\n'), []byte(strings.Replace(string(m), "1.1.0", "1.1.1", 1))} {
		if _, err := Verify(mod, sig, keys); err == nil {
			t.Fatalf("modified manifest verified: %s", mod)
		}
	}
	if _, err := Verify(m, sig, keys); err != nil {
		t.Fatal(err)
	}
}

func TestKeyRotation(t *testing.T) {
	oldPub, oldPriv := newKey(t)
	newPub, newPriv := newKey(t)
	bin := []byte("new binary")
	m := manifestFor("1.1.0", bin)
	fetch := func(sig []byte, keys ...ed25519.PublicKey) (*Release, error) {
		srv, _ := signedServer(t, "1.1.0", bin, m, sig)
		return Fetch(context.Background(), srv.URL+"/rel", keys)
	}

	// binaries built with both keys accept releases signed with either
	for _, k := range []ed25519.PrivateKey{oldPriv, newPriv} {
		rel, err := fetch(Sign(k, m), oldPub, newPub)
		if err != nil || rel.SignedBy != EncodePublicKey(k.Public().(ed25519.PublicKey)) {
			t.Fatalf("rel %+v err %v", rel, err)
		}
	}
	// one signature file with both signatures serves old and new installations
	both := append(Sign(oldPriv, m), Sign(newPriv, m)...)
	for _, k := range []ed25519.PublicKey{oldPub, newPub} {
		rel, err := fetch(both, k)
		if err != nil || rel.SignedBy != EncodePublicKey(k) {
			t.Fatalf("rel %+v err %v", rel, err)
		}
	}
	// once the old key is dropped, its signatures no longer count
	if _, err := fetch(Sign(oldPriv, m), newPub); err == nil {
		t.Fatal("signature of a retired key accepted")
	}
}

func TestNoKeyModeUnchanged(t *testing.T) {
	bin := []byte("new binary")
	// latest.json cannot claim to be signed, and a bogus .sig is never looked at
	m := manifestFor("1.1.0", bin, "signed_by", "forged")
	srv, hits := signedServer(t, "1.1.0", bin, m, []byte("garbage"))
	rel, err := Fetch(context.Background(), srv.URL+"/rel", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel.SignedBy != "" || hits.Load() != 0 {
		t.Fatalf("signed_by %q, signature requests %d", rel.SignedBy, hits.Load())
	}

	e := newEnv(t, "1.0.0", srv.URL+"/rel")
	if st := e.m.Status(); st.Signing.Required || len(st.Signing.Keys) != 0 {
		t.Fatalf("signing status %+v", st.Signing)
	}
	if err := e.m.Install("", false, "admin"); err != nil {
		t.Fatal(err)
	}
	p := e.waitRestart(t)
	if err := Apply(p); err != nil || read(t, e.exe) != "new binary" {
		t.Fatalf("not installed: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the signature was requested without a key")
	}
	// and the checksum is still enforced
	bad, _ := signedServer(t, "2.0.0", []byte("evil"), manifestFor("2.0.0", []byte("good")), nil)
	rel, err = Fetch(context.Background(), bad.URL+"/rel", nil)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := rel.For(Platform())
	if err := Download(context.Background(), f, filepath.Join(t.TempDir(), "dl"), nil); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("download: %v", err)
	}
}

func TestParseKeys(t *testing.T) {
	pub1, priv1 := newKey(t)
	pub2, _ := newKey(t)
	b1, b2 := EncodePublicKey(pub1), EncodePublicKey(pub2)
	keys, err := ParsePublicKeys(" "+b1+",\n"+b2+"; "+b1, "")
	if err != nil || len(keys) != 2 || !keys[0].Equal(pub1) || !keys[1].Equal(pub2) {
		t.Fatalf("keys %v err %v", keys, err)
	}
	if keys, err := ParsePublicKeys("", " , "); err != nil || len(keys) != 0 {
		t.Fatalf("empty list: %v %v", keys, err)
	}
	privB64 := base64.StdEncoding.EncodeToString(priv1)
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString(make([]byte, 16)), b1 + "," + privB64} {
		_, err := ParsePublicKeys(bad)
		if err == nil {
			t.Errorf("ParsePublicKeys(%q) accepted", bad)
		} else if strings.Contains(err.Error(), privB64[:6]) {
			t.Errorf("the error shows part of a private key: %v", err)
		}
	}

	k, err := ParsePrivateKey(EncodePrivateKey(priv1))
	if err != nil || !k.Equal(priv1) {
		t.Fatalf("private key round trip: %v", err)
	}
	// the .pub file given as signing key must not turn into a key anyone can derive
	if _, err := ParsePrivateKey([]byte(b1 + "\n")); err == nil || !strings.Contains(err.Error(), "public key") {
		t.Fatalf("public key accepted as private key: %v", err)
	}
	corrupt := append(ed25519.PrivateKey{}, priv1...)
	corrupt[40] ^= 1
	if _, err := ParsePrivateKey(EncodePrivateKey(corrupt)); err == nil {
		t.Fatal("corrupt private key accepted")
	}
	if _, err := Verify([]byte("x"), Sign(priv1, []byte("x")), nil); err == nil {
		t.Fatal("verified without keys")
	}
}
