package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"

	"dumpcat/internal/auth"
)

// staticUI serves the web UI. index.html is rendered with versioned asset
// URLs (/assets/<build>/js/app.js...), where the build is a hash of every UI
// file: an update changes all the URLs, so versioned files can be cached for
// a year without ever being checked again. The ES modules import each other
// with relative paths, so a page only ever loads the files of one build — an
// open tab of an older version gets a 404 for the new build's files instead
// of a mix of old and new modules. index.html and unversioned paths are
// revalidated on every load.
type staticUI struct {
	fs    fs.FS
	dev   bool // DUMPCAT_WEB_DIR: files change while running, never cache them
	build string
	etags map[string]string
	files http.Handler
	page  func([]byte) []byte // adapts index.html to the settings (nil: as is)
}

const buildLen = 12

func newStaticUI(fsys fs.FS, dev bool) *staticUI {
	u := &staticUI{fs: fsys, dev: dev, etags: map[string]string{}, files: http.FileServer(http.FS(fsys))}
	h := sha256.New()
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := fs.ReadFile(fsys, p); err == nil {
				u.etags[p] = `"` + auth.SHA256Hex(string(b))[:20] + `"`
				h.Write([]byte(p + "\x00" + u.etags[p] + "\x00"))
			}
		}
		return nil
	})
	u.build = hex.EncodeToString(h.Sum(nil))[:buildLen]
	return u
}

func isBuild(s string) bool {
	if len(s) != buildLen {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func (u *staticUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if rest, ok := strings.CutPrefix(p, "assets/"); ok {
		if b, file, ok := strings.Cut(rest, "/"); ok && isBuild(b) {
			if b != u.build {
				// a page of another version: its app reloads on this error
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "this file belongs to another version of DumpCat: reload the page", http.StatusNotFound)
				return
			}
			u.serveFile(w, r, "assets/"+file, true)
			return
		}
	}
	if p == "" || p == "index.html" {
		u.serveIndex(w, r)
		return
	}
	if st, err := fs.Stat(u.fs, p); err != nil || st.IsDir() {
		u.serveIndex(w, r) // client-side routes
		return
	}
	u.serveFile(w, r, p, false)
}

func (u *staticUI) serveFile(w http.ResponseWriter, r *http.Request, p string, versioned bool) {
	if _, err := fs.Stat(u.fs, p); err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case u.dev:
		w.Header().Set("Cache-Control", "no-cache")
	case versioned:
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
	if et, ok := u.etags[p]; ok && !u.dev {
		w.Header().Set("ETag", et)
		if r.Header.Get("If-None-Match") == et {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + p
	u.files.ServeHTTP(w, r2)
}

// index renders index.html for this build: asset URLs carry the build, and
// the page knows it (the API reports the server's build, see api.js).
func (u *staticUI) index() ([]byte, error) {
	b, err := fs.ReadFile(u.fs, "index.html")
	if err != nil {
		return nil, err
	}
	b = bytes.ReplaceAll(b, []byte(`="/assets/`), []byte(`="/assets/`+u.build+`/`))
	meta := []byte(`<meta charset="utf-8">`)
	b = bytes.Replace(b, meta, append(meta, []byte("\n  <meta name=\"dumpcat-build\" content=\""+u.build+"\">")...), 1)
	if u.page != nil {
		b = u.page(b)
	}
	return b, nil
}

func (u *staticUI) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := u.index()
	if err != nil {
		http.Error(w, "web UI not found", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	if !u.dev {
		et := `"` + auth.SHA256Hex(string(b))[:20] + `"`
		h.Set("ETag", et)
		if r.Header.Get("If-None-Match") == et {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}
