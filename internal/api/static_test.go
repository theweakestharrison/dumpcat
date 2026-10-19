package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func uiFS(app string) fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(`<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/assets/app.css"></head><body><script type="module" src="/assets/js/app.js"></script></body></html>`)},
		"assets/app.css":       {Data: []byte("body{}")},
		"assets/js/app.js":     {Data: []byte(app)},
		"assets/js/pages/x.js": {Data: []byte("export const x = 1;")},
		"assets/favicon.svg":   {Data: []byte("<svg/>")},
	}
}

func get(t *testing.T, h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStaticUIVersionedAssets(t *testing.T) {
	u := newStaticUI(uiFS("import './ui.js';"), false)
	if !isBuild(u.build) {
		t.Fatalf("build %q", u.build)
	}
	if u2 := newStaticUI(uiFS("import './ui.js'; // changed"), false); u2.build == u.build {
		t.Fatal("a changed file must change the build")
	}

	// index.html: versioned URLs, the build, revalidated every time
	rec := get(t, u, "/")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `src="/assets/`+u.build+`/js/app.js"`) || !strings.Contains(body, `href="/assets/`+u.build+`/app.css"`) ||
		!strings.Contains(body, `<meta name="dumpcat-build" content="`+u.build+`">`) {
		t.Fatalf("index: %d %s", rec.Code, body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index Cache-Control %q", cc)
	}
	if rec2 := get(t, u, "/", "If-None-Match", rec.Header().Get("ETag")); rec2.Code != http.StatusNotModified {
		t.Fatalf("index revalidation: %d", rec2.Code)
	}
	// client-side routes get the page too
	if rec := get(t, u, "/some/route"); rec.Code != 200 || !strings.Contains(rec.Body.String(), u.build) {
		t.Fatalf("fallback: %d", rec.Code)
	}

	// files of this build: cached for a year, relative imports stay in the build
	rec = get(t, u, "/assets/"+u.build+"/js/pages/x.js")
	if rec.Code != 200 || rec.Body.String() != "export const x = 1;" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("versioned: %d %q %q", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("content type %q", ct)
	}
	// files of another build: an error, never a mix of versions
	rec = get(t, u, "/assets/0123456789ab/js/pages/x.js")
	if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("old build: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get(t, u, "/assets/"+u.build+"/js/missing.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing versioned file: %d", rec.Code)
	}
	// unversioned paths (favicon, the OAuth page's stylesheet) still work, revalidated
	rec = get(t, u, "/assets/favicon.svg")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") == "" {
		t.Fatalf("unversioned: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}

	// development: nothing is cached for long
	d := newStaticUI(uiFS("x"), true)
	if rec := get(t, d, "/assets/"+d.build+"/app.css"); rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("dev: %q", rec.Header().Get("Cache-Control"))
	}
}
