package api

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestNoindexByDefault(t *testing.T) {
	s, _, _ := newSetupServer(t)
	s.WebFS = fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><html><head><meta charset=\"utf-8\">\n  " + robotsMeta + "\n  <title>DumpCat</title></head><body></body></html>")},
		"assets/favicon.svg": {Data: []byte("<svg/>")},
	}
	h := s.Handler()

	// robots.txt keeps every crawler out
	rec := get(t, h, "/robots.txt")
	if rec.Code != 200 || rec.Body.String() != "User-agent: *\nDisallow: /\n" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("robots.txt: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	// every answer says noindex: the page, files, the API (also errors), robots.txt itself
	for _, p := range []string{"/", "/some/route", "/assets/favicon.svg", "/api/auth/status", "/api/missing", "/robots.txt"} {
		if got := get(t, h, p).Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
			t.Fatalf("%s: X-Robots-Tag %q", p, got)
		}
	}
	if body := get(t, h, "/").Body.String(); !strings.Contains(body, robotsMeta) {
		t.Fatalf("index without the robots meta tag: %s", body)
	}

	// Settings allow indexing: no header, no meta tag, robots.txt only keeps crawlers out of the API
	st := s.Store.GetSettings()
	st.AllowIndexing = true
	if err := s.Store.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	s.allowIndexing.Store(true)
	rec = get(t, h, "/")
	if rec.Header().Get("X-Robots-Tag") != "" || strings.Contains(rec.Body.String(), "robots") || !strings.Contains(rec.Body.String(), "<title>DumpCat</title>") {
		t.Fatalf("indexing allowed: %q %s", rec.Header().Get("X-Robots-Tag"), rec.Body.String())
	}
	if body := get(t, h, "/robots.txt").Body.String(); body != "User-agent: *\nDisallow: /api/\n" {
		t.Fatalf("robots.txt with indexing: %q", body)
	}
	// the setting is read when the server starts
	s2 := &Server{Cfg: s.Cfg, Store: s.Store, WebFS: s.WebFS}
	if get(t, s2.Handler(), "/api/auth/status").Header().Get("X-Robots-Tag") != "" {
		t.Fatal("a restarted server forgot that indexing is allowed")
	}
}
