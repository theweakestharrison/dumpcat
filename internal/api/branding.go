package api

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"image/png"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"dumpcat/internal/auth"
	"dumpcat/internal/store"
)

// Branding: an instance name (a setting) and a logo uploaded by an admin. The logo is served from
// the API with the Content-Type its content was checked as; an SVG must be a plain drawing (no
// scripts, event handlers or references to other files), and is served sandboxed anyway.

const (
	maxLogoBytes  = 256 << 10
	maxLogoPixels = 4096
	maxNameRunes  = 60
)

func checkInstanceName(n string) error {
	if utf8.RuneCountInString(n) > maxNameRunes {
		return badRequest("the instance name must be at most %d characters", maxNameRunes)
	}
	if strings.ContainsFunc(n, func(r rune) bool { return unicode.IsControl(r) || r == '<' || r == '>' }) {
		return badRequest("the instance name cannot contain < > or control characters")
	}
	return nil
}

// logo returns the uploaded logo (nil: the cat), read from the database once.
func (s *Server) logo() *store.Logo {
	s.logoMu.Lock()
	defer s.logoMu.Unlock()
	if !s.logoLoaded {
		s.logoCache, _ = s.Store.GetLogo()
		s.logoLoaded = true
	}
	return s.logoCache
}

func (s *Server) setLogo(l *store.Logo) {
	s.logoMu.Lock()
	s.logoCache, s.logoLoaded = l, true
	s.logoMu.Unlock()
}

const defaultLogo = "/assets/favicon.svg"

// logoURL is where the page loads the logo from; the hash changes the URL with every upload.
func (s *Server) logoURL() (string, string) {
	if l := s.logo(); l != nil {
		return "/api/branding/logo?v=" + l.Hash, l.Type
	}
	return "", "image/svg+xml"
}

var (
	indexTitle = regexp.MustCompile(`<title>[^<]*</title>`)
	indexIcon  = regexp.MustCompile(`<link rel="icon" href="[^"]*" type="[^"]*">`)
)

// brandIndex gives index.html the instance name as its title and the logo as its icon.
func (s *Server) brandIndex(b []byte) []byte {
	if name := s.Store.GetSettings().Brand(); name != store.DefaultBrand {
		b = indexTitle.ReplaceAllLiteral(b, []byte("<title>"+html.EscapeString(name)+"</title>"))
	}
	if url, ct := s.logoURL(); url != "" {
		b = indexIcon.ReplaceAllLiteral(b, []byte(`<link rel="icon" href="`+url+`" type="`+ct+`">`))
	}
	return b
}

// brandView tells the page (also the sign-in page) the instance name and logo.
func (s *Server) brandView() map[string]any {
	name := s.Store.GetSettings().Brand()
	logo, _ := s.logoURL()
	return map[string]any{"name": name, "custom_name": name != store.DefaultBrand, "logo": logo}
}

func (s *Server) logoGet(w http.ResponseWriter, r *http.Request) error {
	l := s.logo()
	if l == nil {
		http.Redirect(w, r, defaultLogo, http.StatusFound)
		return nil
	}
	h := w.Header()
	h.Set("Content-Type", l.Type)
	// opened on its own, an SVG is a document: nothing in it may run or load anything
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox")
	h.Set("Content-Disposition", `inline; filename="logo`+logoExt(l.Type)+`"`)
	if r.URL.Query().Get("v") == l.Hash {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	et := `"` + l.Hash + `"`
	h.Set("ETag", et)
	if r.Header.Get("If-None-Match") == et {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(l.Data)
	}
	return nil
}

func logoExt(ct string) string {
	switch ct {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	}
	return ".svg"
}

func (s *Server) logoPut(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Data string `json:"data"` // base64, or a data: URL
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	raw := in.Data
	if strings.HasPrefix(raw, "data:") {
		_, raw, _ = strings.Cut(raw, ",")
	}
	if base64.StdEncoding.DecodedLen(len(raw)) > maxLogoBytes+3 {
		return badRequest("the logo must be at most %d KB", maxLogoBytes>>10)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return badRequest("the logo is not valid base64")
	}
	ct, err := checkLogo(data)
	if err != nil {
		return badRequest("%v", err)
	}
	l := &store.Logo{Type: ct, Data: data, Hash: auth.SHA256Hex(string(data))[:16]}
	if err := s.Store.SetLogo(l); err != nil {
		return err
	}
	s.setLogo(l)
	s.audit(r, "branding_logo", "settings", fmt.Sprintf("%s, %d bytes", ct, len(data)))
	return ok(w, s.brandView())
}

func (s *Server) logoDelete(w http.ResponseWriter, r *http.Request) error {
	if err := s.Store.DeleteLogo(); err != nil {
		return err
	}
	s.setLogo(nil)
	s.audit(r, "branding_logo_reset", "settings", "")
	return ok(w, s.brandView())
}

// checkLogo tells what an uploaded logo is from its content (never from a name or a type the
// browser sent) and refuses anything but a PNG, a WebP or a plain SVG drawing.
func checkLogo(b []byte) (string, error) {
	switch {
	case len(b) == 0:
		return "", errors.New("the logo is empty")
	case len(b) > maxLogoBytes:
		return "", fmt.Errorf("the logo must be at most %d KB", maxLogoBytes>>10)
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		c, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			return "", fmt.Errorf("the PNG cannot be read: %v", err)
		}
		if c.Width < 1 || c.Height < 1 || c.Width > maxLogoPixels || c.Height > maxLogoPixels {
			return "", fmt.Errorf("the logo must be at most %d×%d pixels", maxLogoPixels, maxLogoPixels)
		}
		return "image/png", nil
	case len(b) >= 20 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		if size := binary.LittleEndian.Uint32(b[4:8]); int64(size)+8 != int64(len(b)) {
			return "", errors.New("the WebP file is truncated or has extra data")
		}
		switch string(b[12:16]) {
		case "VP8 ", "VP8L", "VP8X":
			return "image/webp", nil
		}
		return "", errors.New("the WebP file holds no image")
	}
	if looksLikeSVG(b) {
		if err := checkSVG(b); err != nil {
			return "", err
		}
		return "image/svg+xml", nil
	}
	return "", errors.New("the logo must be a PNG, SVG or WebP image")
}

func looksLikeSVG(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 4096)]))
	return strings.Contains(head, "<svg")
}

const svgNS = "http://www.w3.org/2000/svg"

// SVG elements that run code, embed other documents or load other files (<image> may stay: its
// href must be an embedded raster image).
var svgDenied = map[string]bool{
	"script": true, "foreignobject": true, "iframe": true, "embed": true, "object": true, "handler": true,
	"listener": true, "audio": true, "video": true, "frame": true, "frameset": true, "base": true, "link": true,
	"meta": true,
}

// url(...) in an attribute or a stylesheet
var cssURL = regexp.MustCompile(`(?i)url\(\s*['"]?\s*([^'")\s]*)`)

// embeddedImage is the only kind of reference to something else an SVG logo may hold.
func embeddedImage(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, t := range []string{"data:image/png;", "data:image/jpeg;", "data:image/gif;", "data:image/webp;"} {
		if strings.HasPrefix(v, t) {
			return true
		}
	}
	return false
}

// localRef: a reference inside the drawing (#id), or an embedded raster image.
func localRef(v string) bool {
	v = strings.TrimSpace(v)
	return strings.HasPrefix(v, "#") || embeddedImage(v)
}

// checkCSS refuses styles that load something or run code.
func checkCSS(v string) error {
	l := strings.ToLower(v)
	for _, bad := range []string{"@import", "expression(", "javascript:", "behavior:", "-moz-binding"} {
		if strings.Contains(strings.ReplaceAll(l, " ", ""), bad) {
			return fmt.Errorf("the SVG has a style with %s", strings.TrimSuffix(bad, ":"))
		}
	}
	for _, m := range cssURL.FindAllStringSubmatch(v, -1) {
		if !localRef(m[1]) {
			return fmt.Errorf("the SVG refers to another file (url(%s))", m[1])
		}
	}
	return nil
}

// checkSVG accepts a plain drawing: an <svg> root, no DOCTYPE or entities, no scripts, event
// handlers, embedded documents or references to other files (links, stylesheets, images).
func checkSVG(b []byte) error {
	if !utf8.Valid(b) {
		return errors.New("the SVG is not UTF-8 text")
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	root := false
	inStyle := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("the SVG is not valid XML: %v", err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			return errors.New("the SVG has a DOCTYPE or entity declarations")
		case xml.ProcInst:
			if t.Target != "xml" {
				return fmt.Errorf("the SVG has a <?%s?> instruction", t.Target)
			}
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if !root {
				if local != "svg" || t.Name.Space != svgNS {
					return errors.New("the file is not an SVG image (its root element must be <svg xmlns=\"http://www.w3.org/2000/svg\">)")
				}
				root = true
			}
			if svgDenied[local] || t.Name.Space == "http://www.w3.org/1999/xhtml" {
				return fmt.Errorf("the SVG has a <%s> element: logos cannot contain scripts or other documents", t.Name.Local)
			}
			if local == "style" {
				inStyle++
			}
			for _, a := range t.Attr {
				n := strings.ToLower(a.Name.Local)
				v := a.Value
				switch {
				case strings.HasPrefix(n, "on"):
					return fmt.Errorf("the SVG has an event handler (%s): logos cannot contain scripts", a.Name.Local)
				case n == "href" || n == "src":
					if !localRef(v) {
						return fmt.Errorf("the SVG links to another file (%s=%q)", a.Name.Local, v)
					}
				case (local == "animate" || local == "set") && n == "attributename":
					if l := strings.ToLower(v); strings.HasSuffix(l, "href") || strings.HasPrefix(l, "on") {
						return fmt.Errorf("the SVG animates %s", v)
					}
				}
				if strings.Contains(strings.ToLower(strings.Join(strings.Fields(v), "")), "javascript:") {
					return errors.New("the SVG has a javascript: link")
				}
				if err := checkCSS(v); err != nil {
					return err
				}
			}
		case xml.EndElement:
			if strings.EqualFold(t.Name.Local, "style") && inStyle > 0 {
				inStyle--
			}
		case xml.CharData:
			if inStyle > 0 {
				if err := checkCSS(string(t)); err != nil {
					return err
				}
			}
		}
	}
	if !root {
		return errors.New("the file is not an SVG image")
	}
	return nil
}
