package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dumpcat/internal/config"
	"dumpcat/internal/secret"
	"dumpcat/internal/store"
)

func newSetupServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "master.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "dumpcat.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{Cfg: &config.Config{}, Store: st}
	mux := http.NewServeMux()
	s.routes(mux)
	return s, mux, filepath.Join(dir, SetupTokenFile)
}

// postSetup sends the setup form from ip and returns the status and error message.
func postSetup(h http.Handler, ip, token string) (int, string) {
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "correct horse", "setup_token": token})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Error
}

func TestSetupRequiresToken(t *testing.T) {
	s, h, path := newSetupServer(t)
	tok, err := s.PrepareSetup(path)
	if err != nil || tok == "" {
		t.Fatalf("PrepareSetup: %q %v", tok, err)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != tok {
		t.Fatalf("token file: %q %v", b, err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v, want 0600", fi.Mode().Perm())
	}

	// the first visitor, local or not, cannot create the admin without the token
	for _, ip := range []string{"203.0.113.7", "127.0.0.1", "::1"} {
		if code, msg := postSetup(h, ip, ""); code != 400 || !strings.Contains(msg, "setup-token") {
			t.Fatalf("no token from %s: %d %q", ip, code, msg)
		}
	}
	if code, _ := postSetup(h, "203.0.113.7", strings.Repeat("A", 24)); code != 403 {
		t.Fatalf("wrong token: %d", code)
	}
	if n, _ := s.Store.CountUsers(); n != 0 {
		t.Fatal("an account was created without the token")
	}

	// case, spaces and dashes do not matter
	typed := strings.ToLower(strings.ReplaceAll(tok, "-", " "))
	if code, msg := postSetup(h, "203.0.113.7", typed); code != 200 {
		t.Fatalf("correct token: %d %q", code, msg)
	}
	if u, err := s.Store.GetUserByName("admin"); err != nil || u.Role != store.RoleAdmin {
		t.Fatalf("admin not created: %v", err)
	}

	// the token is gone: setup cannot run again
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file still there: %v", err)
	}
	if s.setupToken != "" {
		t.Fatal("token still active in memory")
	}
	if code, _ := postSetup(h, "203.0.113.7", tok); code != 409 {
		t.Fatalf("second setup: %d", code)
	}
}

func TestSetupTokenRateLimit(t *testing.T) {
	s, h, path := newSetupServer(t)
	tok, err := s.PrepareSetup(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if code, _ := postSetup(h, "198.51.100.1", "WRONG-TOKEN"); code != 403 {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	// locked out, even with the right token
	if code, _ := postSetup(h, "198.51.100.1", tok); code != 429 {
		t.Fatalf("after 5 wrong tokens: %d", code)
	}
	// other addresses are not affected
	if code, msg := postSetup(h, "198.51.100.2", tok); code != 200 {
		t.Fatalf("other IP: %d %q", code, msg)
	}
}

func TestSetupWithoutToken(t *testing.T) {
	// no PrepareSetup (or it failed): setup stays closed
	_, h, _ := newSetupServer(t)
	if code, _ := postSetup(h, "127.0.0.1", "anything"); code != 503 {
		t.Fatalf("got %d, want 503", code)
	}
}

func TestPrepareSetupWithAccount(t *testing.T) {
	s, h, path := newSetupServer(t)
	if err := s.Store.CreateUser(&store.User{Username: "root", PasswordHash: "x", Role: store.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("STALE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := s.PrepareSetup(path)
	if err != nil || tok != "" {
		t.Fatalf("PrepareSetup with an account: %q %v", tok, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale token file kept: %v", err)
	}
	if code, _ := postSetup(h, "127.0.0.1", "STALE"); code != 409 {
		t.Fatalf("got %d, want 409", code)
	}
}

func TestPrepareSetupNewTokenEachStart(t *testing.T) {
	s, _, path := newSetupServer(t)
	// a file with loose permissions from before is replaced
	if err := os.WriteFile(path, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := s.PrepareSetup(path)
	b, err := s.PrepareSetup(path)
	if err != nil || a == b {
		t.Fatalf("tokens %q %q %v", a, b, err)
	}
	if got, _ := os.ReadFile(path); strings.TrimSpace(string(got)) != b {
		t.Fatalf("file has %q, want %q", got, b)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	if s.setupToken != normalizeSetupToken(b) {
		t.Fatal("the old token is still active")
	}
}

func TestNewSetupToken(t *testing.T) {
	tok := newSetupToken()
	if len(tok) != 29 || strings.Count(tok, "-") != 5 {
		t.Fatalf("unexpected format %q", tok)
	}
	n := normalizeSetupToken(tok)
	if len(n) != 24 || strings.Trim(n, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
		t.Fatalf("normalized %q", n)
	}
	if newSetupToken() == tok {
		t.Fatal("tokens repeat")
	}
}
