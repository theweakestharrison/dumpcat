package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dumpcat/internal/config"
	"dumpcat/internal/monitor"
	"dumpcat/internal/runner"
	"dumpcat/internal/secret"
	"dumpcat/internal/store"
)

func TestMetricsAccess(t *testing.T) {
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
	run := runner.New(st, nil, nil, nil, dir, "1.4.0")
	s := &Server{Cfg: &config.Config{DataDir: dir}, Store: st, Runner: run, Version: "1.4.0", Monitor: &monitor.Checker{Store: st, Runner: run}}
	mux := http.NewServeMux()
	s.routes(mux)
	get := func(remote string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = remote
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	setAccess := func(mode string) {
		set := st.GetSettings()
		set.MetricsAccess = mode
		if err := st.SaveSettings(set); err != nil {
			t.Fatal(err)
		}
	}
	if rec := get("127.0.0.1:5000", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("off by default: %d", rec.Code)
	}

	setAccess(store.MetricsLocalhost)
	rec := get("127.0.0.1:5000", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != monitor.ContentType {
		t.Fatalf("localhost: %d %s", rec.Code, rec.Body)
	}
	if n, err := monitor.CheckExposition(rec.Body.String()); err != nil || !strings.Contains(rec.Body.String(), `dumpcat_build_info{version="1.4.0"`) {
		t.Fatalf("exposition (%d samples): %v\n%s", n, err, rec.Body)
	}
	if rec := get("[::1]:5000", nil); rec.Code != 200 {
		t.Fatalf("IPv6 localhost: %d", rec.Code)
	}
	if rec := get("10.0.0.7:5000", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("remote client: %d", rec.Code)
	}
	if rec := get("127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.9"}); rec.Code != http.StatusForbidden {
		t.Fatalf("through a reverse proxy: %d", rec.Code)
	}

	setAccess(store.MetricsToken)
	if rec := get("127.0.0.1:5000", nil); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("token mode without a token: %d", rec.Code)
	}
	tr := httptest.NewRecorder()
	if err := s.metricsTokenCreate(tr, httptest.NewRequest(http.MethodPost, "/api/settings/metrics-token", nil)); err != nil {
		t.Fatal(err)
	}
	var tok struct{ Token, Prefix string }
	if err := json.Unmarshal(tr.Body.Bytes(), &tok); err != nil || !strings.HasPrefix(tok.Token, "dcm_") || tok.Prefix != tok.Token[:10] {
		t.Fatalf("token: %s", tr.Body)
	}
	if h, _ := st.GetKV(metricsTokenHashKey); h == "" || strings.Contains(h, tok.Token) {
		t.Fatal("the token is not stored hashed")
	}
	if v := s.settingsView(st.GetSettings()); v.MetricsTokenPrefix != tok.Prefix {
		t.Fatalf("settings: %+v", v)
	}
	if rec := get("10.0.0.7:5000", map[string]string{"Authorization": "Bearer " + tok.Token}); rec.Code != 200 {
		t.Fatalf("with the token: %d %s", rec.Code, rec.Body)
	}
	if rec := get("10.0.0.7:5000", map[string]string{"Authorization": "Bearer " + tok.Token + "x"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %d", rec.Code)
	}
	if rec := get("10.0.0.7:5000", map[string]string{"Authorization": tok.Token}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("not a bearer token: %d", rec.Code)
	}
}
