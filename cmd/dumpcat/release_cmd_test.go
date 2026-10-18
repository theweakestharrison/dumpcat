package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dumpcat/internal/update"
)

// keygen → sign latest.json → an installation trusting the .pub key fetches,
// verifies and downloads the release; changed or unsigned manifests are refused.
func TestReleaseSigningEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "keys")
	keyFile, pubFile, pub, err := keygen(keyDir, "dumpcat-release")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(keyFile); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("private key mode %v (%v)", fi.Mode(), err)
		}
	}
	if _, _, _, err := keygen(keyDir, "dumpcat-release"); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
	pubData, err := os.ReadFile(pubFile)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := update.ParsePublicKeys(string(pubData))
	if err != nil || len(keys) != 1 || !keys[0].Equal(pub) {
		t.Fatalf("public key file: %v %v", keys, err)
	}

	// a release folder as build.sh writes it
	rel := filepath.Join(dir, "update")
	os.MkdirAll(filepath.Join(rel, "1.1.0"), 0o755)
	bin := []byte("new binary")
	os.WriteFile(filepath.Join(rel, "1.1.0", "dumpcat-bin"), bin, 0o755)
	sum := sha256.Sum256(bin)
	manifest := fmt.Sprintf("{\n  \"version\": \"1.1.0\",\n  \"files\": {%q: {\"url\": \"1.1.0/dumpcat-bin\", \"sha256\": %q, \"size\": %d}}\n}\n",
		update.Platform(), hex.EncodeToString(sum[:]), len(bin))
	latest := filepath.Join(rel, "latest.json")
	os.WriteFile(latest, []byte(manifest), 0o644)
	if _, err := signManifest(latest, latest+update.SignatureSuffix, []string{keyFile}); err != nil {
		t.Fatal(err)
	}
	if _, err := signManifest(latest, filepath.Join(dir, "x.sig"), []string{pubFile}); err == nil {
		t.Fatal("signed with the public key file")
	}

	srv := httptest.NewServer(http.FileServer(http.Dir(rel)))
	defer srv.Close()
	r, err := update.Fetch(ctx, srv.URL, keys)
	if err != nil {
		t.Fatal(err)
	}
	if r.SignedBy != update.EncodePublicKey(pub) {
		t.Fatalf("signed_by %q", r.SignedBy)
	}
	f, err := r.For(update.Platform())
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Download(ctx, f, filepath.Join(dir, "dl"), nil); err != nil {
		t.Fatal(err)
	}

	// latest.json changed after signing
	os.WriteFile(latest, []byte(strings.Replace(manifest, "1.1.0/dumpcat-bin", "1.1.0/other-bin", 1)), 0o644)
	if _, err := update.Fetch(ctx, srv.URL, keys); err == nil || !strings.Contains(err.Error(), "not signed by a trusted key") {
		t.Fatalf("changed manifest: %v", err)
	}
	// signature missing
	os.WriteFile(latest, []byte(manifest), 0o644)
	os.Remove(latest + update.SignatureSuffix)
	if _, err := update.Fetch(ctx, srv.URL, keys); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("missing signature: %v", err)
	}
	// without a key the unsigned release is accepted as before
	if _, err := update.Fetch(ctx, srv.URL, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedUpdateKeys(t *testing.T) {
	pub1, _, _ := update.GenerateKey()
	pub2, _, _ := update.GenerateKey()
	b1, b2 := update.EncodePublicKey(pub1), update.EncodePublicKey(pub2)
	saved := updatePublicKeys
	t.Cleanup(func() { updatePublicKeys = saved })

	updatePublicKeys = ""
	t.Setenv("DUMPCAT_UPDATE_PUBLIC_KEY", "")
	if keys, err := trustedUpdateKeys(); err != nil || len(keys) != 0 {
		t.Fatalf("no keys: %v %v", keys, err)
	}
	updatePublicKeys = b1
	t.Setenv("DUMPCAT_UPDATE_PUBLIC_KEY", b2+", "+b1)
	if keys, err := trustedUpdateKeys(); err != nil || len(keys) != 2 {
		t.Fatalf("build + env keys: %v %v", keys, err)
	}
	t.Setenv("DUMPCAT_UPDATE_PUBLIC_KEY", "garbage")
	if _, err := trustedUpdateKeys(); err == nil || !strings.Contains(err.Error(), "DUMPCAT_UPDATE_PUBLIC_KEY") {
		t.Fatalf("invalid env key: %v", err)
	}
	t.Setenv("DUMPCAT_UPDATE_PUBLIC_KEY", "")
	updatePublicKeys = "bad"
	if _, err := trustedUpdateKeys(); err == nil || !strings.Contains(err.Error(), "built into") {
		t.Fatalf("invalid built-in key: %v", err)
	}
}
