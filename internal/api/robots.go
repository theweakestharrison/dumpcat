package api

import (
	"bytes"
	"io"
	"net/http"
)

// An installed DumpCat is a private admin page: search engines are told to stay away unless
// Settings allow indexing (robots.txt, an X-Robots-Tag header on every answer and a robots meta
// tag in the page).

const robotsMeta = `<meta name="robots" content="noindex, nofollow">`

func (s *Server) noindex() bool { return !s.allowIndexing.Load() }

func (s *Server) robotsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if s.noindex() {
		_, _ = io.WriteString(w, "User-agent: *\nDisallow: /\n")
		return
	}
	_, _ = io.WriteString(w, "User-agent: *\nDisallow: /api/\n")
}

// robotsHeader adds X-Robots-Tag to every answer (pages, API, files) while indexing is off.
func (s *Server) robotsHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.noindex() {
			w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		}
		next.ServeHTTP(w, r)
	})
}

// renderIndex adapts index.html to the settings: the robots meta tag goes once indexing is
// allowed, and the title and icon follow the branding (branding.go).
func (s *Server) renderIndex(b []byte) []byte {
	if !s.noindex() {
		b = bytes.Replace(b, []byte("\n  "+robotsMeta), nil, 1)
		b = bytes.Replace(b, []byte(robotsMeta), nil, 1)
	}
	return s.brandIndex(b)
}
