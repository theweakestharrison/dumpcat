// Package update checks a release server for new DumpCat versions, downloads
// and verifies them, and swaps the running binary with automatic rollback.
//
// The release server only needs static files:
//
//	<base>/latest.json
//	<base>/<version>/dumpcat-linux-amd64 ...
//
// latest.json:
//
//	{"version": "1.1.0", "released_at": "...", "notes": "...",
//	 "files": {"linux/amd64": {"url": "1.1.0/dumpcat-linux-amd64", "sha256": "...", "size": 123}}}
//
// File URLs may be absolute or relative to latest.json.
//
// When the installation trusts release signing keys (see sign.go), the server
// must also serve <base>/latest.json.sig, a signature over the exact bytes of
// latest.json; releases without a valid signature are refused.
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	maxManifest = 1 << 20   // latest.json
	maxBinary   = 512 << 20 // a release binary
)

// File is one downloadable binary of a release.
type File struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
}

// Release is the content of latest.json.
type Release struct {
	Version    string          `json:"version"`
	ReleasedAt string          `json:"released_at,omitempty"`
	Notes      string          `json:"notes,omitempty"`
	NotesURL   string          `json:"notes_url,omitempty"`
	Files      map[string]File `json:"files"`
	// SignedBy is the trusted public key that signed latest.json (set by Fetch,
	// empty when no key is configured; whatever latest.json says is ignored).
	SignedBy string `json:"signed_by,omitempty"`

	base *url.URL
}

// Platform is the key of this build in Release.Files, e.g. "linux/amd64".
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// ManifestURL validates a release server address and returns the URL of latest.json.
// HTTPS is required except for loopback addresses (or DUMPCAT_UPDATE_ALLOW_HTTP=1),
// because the checksums travel over the same connection as the binaries.
func ManifestURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("no release server configured")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid release server URL %q", raw)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) && os.Getenv("DUMPCAT_UPDATE_ALLOW_HTTP") != "1" {
		return nil, fmt.Errorf("the release server must use HTTPS (%s)", raw)
	}
	if !strings.HasSuffix(strings.ToLower(u.Path), ".json") {
		u.Path = strings.TrimRight(u.Path, "/") + "/latest.json"
	}
	return u, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HTTPClient is used for every request; tests may replace it.
var HTTPClient = &http.Client{Transport: http.DefaultTransport}

// Fetch downloads and parses latest.json. With keys, latest.json.sig is
// downloaded too and the release is refused unless one of keys signed the exact
// bytes of latest.json; without keys only HTTPS and the SHA-256 values in
// latest.json protect the download.
func Fetch(ctx context.Context, server string, keys []ed25519.PublicKey) (*Release, error) {
	u, err := ManifestURL(server)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, _, err := getSmall(ctx, u, maxManifest)
	if err != nil {
		return nil, err
	}
	var signer ed25519.PublicKey
	if len(keys) > 0 {
		su := SignatureURL(u)
		sig, code, err := getSmall(ctx, su, maxSignature)
		if code == http.StatusNotFound {
			return nil, fmt.Errorf("release refused: it is not signed (%s not found) and this installation only accepts signed releases", path.Base(su.Path))
		}
		if err != nil {
			return nil, err
		}
		if signer, err = Verify(body, sig, keys); err != nil {
			return nil, fmt.Errorf("release refused: %w", err)
		}
	}
	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("latest.json is not valid JSON: %w", err)
	}
	rel.Version = strings.TrimPrefix(strings.TrimSpace(rel.Version), "v")
	if _, ok := parseVersion(rel.Version); !ok {
		return nil, fmt.Errorf("latest.json has an invalid version %q", rel.Version)
	}
	rel.SignedBy = ""
	if signer != nil {
		rel.SignedBy = EncodePublicKey(signer)
	}
	rel.base = u
	return &rel, nil
}

// SignatureURL returns the URL of the signature of a manifest (latest.json.sig).
func SignatureURL(manifest *url.URL) *url.URL {
	s := *manifest
	s.Path += SignatureSuffix
	s.RawPath = ""
	return &s
}

// getSmall GETs u and returns its body (at most max bytes) and the status code.
func getSmall(ctx context.Context, u *url.URL, max int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot reach the release server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("release server: %s returned %s", u.Redacted(), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(body)) > max {
		return nil, resp.StatusCode, fmt.Errorf("%s is too large", path.Base(u.Path))
	}
	return body, resp.StatusCode, nil
}

// For returns the binary for a platform with its URL resolved.
func (r *Release) For(platform string) (File, error) {
	f, ok := r.Files[platform]
	if !ok || f.URL == "" {
		return File{}, fmt.Errorf("release %s has no build for %s", r.Version, platform)
	}
	sum := strings.ToLower(strings.TrimSpace(f.SHA256))
	if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
		return File{}, fmt.Errorf("release %s: invalid SHA-256 for %s", r.Version, platform)
	}
	f.SHA256 = sum
	ref, err := url.Parse(f.URL)
	if err != nil {
		return File{}, fmt.Errorf("release %s: invalid URL for %s", r.Version, platform)
	}
	if r.base != nil {
		ref = r.base.ResolveReference(ref)
	}
	if ref.Scheme != "https" && !(ref.Scheme == "http" && (isLoopback(ref.Hostname()) || os.Getenv("DUMPCAT_UPDATE_ALLOW_HTTP") == "1")) {
		return File{}, fmt.Errorf("release %s: the download URL must use HTTPS", r.Version)
	}
	f.URL = ref.String()
	return f, nil
}

// Download writes the file to dest and checks its size and SHA-256; on any
// error dest is removed. progress (optional) receives bytes done and total.
func Download(ctx context.Context, f File, dest string, progress func(done, total int64)) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s returned %s", path.Base(req.URL.Path), resp.Status)
	}
	total := f.Size
	if total <= 0 {
		total = resp.ContentLength
	}
	if total > maxBinary {
		return errors.New("the release binary is too large")
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dest)
		}
	}()
	h := sha256.New()
	var done int64
	buf := make([]byte, 256<<10)
	src := io.LimitReader(resp.Body, maxBinary+1)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if done > maxBinary {
				return errors.New("the release binary is too large")
			}
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("download failed: %w", rerr)
		}
	}
	if f.Size > 0 && done != f.Size {
		return fmt.Errorf("download is incomplete: got %d of %d bytes", done, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s — the file was not installed", f.SHA256, got)
	}
	return out.Sync()
}

// Compare returns -1, 0 or 1 comparing two versions like "1.2.3" or "1.3.0-rc.1".
// Anything unparsable (e.g. "dev") sorts before every real version.
func Compare(a, b string) int {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < 3; i++ {
		if va.n[i] != vb.n[i] {
			if va.n[i] < vb.n[i] {
				return -1
			}
			return 1
		}
	}
	// a pre-release sorts before the release itself
	switch {
	case va.pre == vb.pre:
		return 0
	case va.pre == "":
		return 1
	case vb.pre == "":
		return -1
	case va.pre < vb.pre:
		return -1
	}
	return 1
}

type version struct {
	n   [3]int
	pre string
}

func parseVersion(s string) (version, bool) {
	var v version
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "+"); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, v.pre = s[:i], s[i+1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v.n[i] = n
	}
	return v, true
}
