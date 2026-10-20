package api

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"dumpcat/internal/auth"
	"dumpcat/internal/store"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.NRGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// hugePNG claims to be 10000 pixels wide in a valid header.
func hugePNG(t *testing.T) []byte {
	b := testPNG(t, 1, 1)
	ihdr := b[8+8 : 8+8+13] // after the signature and the chunk's length and type
	binary.BigEndian.PutUint32(ihdr[0:4], 10000)
	binary.BigEndian.PutUint32(b[8+8+13:], crc32.ChecksumIEEE(b[8+4:8+8+13]))
	return b
}

func webp(payload string) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WEBP" + payload)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	return b
}

const svgOK = `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape" viewBox="0 0 32 32">
  <style>.a { fill: url(#g); } .b { stroke: #333 }</style>
  <defs><linearGradient id="g"><stop offset="0" stop-color="#f60"/></linearGradient><path id="p" d="M0 0h8v8z"/></defs>
  <inkscape:namedview inkscape:zoom="2"/>
  <rect class="a" width="32" height="32" rx="6" style="fill: url('#g')"/>
  <use xlink:href="#p"/>
  <image width="4" height="4" href="data:image/png;base64,iVBORw0KGgo="/>
  <animate attributeName="opacity" from="0" to="1" dur="1s"/>
</svg>`

func TestCheckLogo(t *testing.T) {
	good := map[string]struct {
		data []byte
		ct   string
	}{
		"png":       {testPNG(t, 64, 64), "image/png"},
		"webp VP8L": {webp("VP8L\x05\x00\x00\x00\x2f\x00\x00\x00\x00\x00"), "image/webp"},
		"svg":       {[]byte(svgOK), "image/svg+xml"},
		"svg plain": {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>`), "image/svg+xml"},
	}
	for name, c := range good {
		ct, err := checkLogo(c.data)
		if err != nil || ct != c.ct {
			t.Errorf("%s: %q %v", name, ct, err)
		}
	}

	svg := func(body string) []byte {
		return []byte(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">` + body + `</svg>`)
	}
	bad := map[string]struct {
		data []byte
		want string
	}{
		"empty":            {nil, "empty"},
		"too big":          {append(testPNG(t, 1, 1), make([]byte, maxLogoBytes)...), "at most 256 KB"},
		"gif":              {[]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), "PNG, SVG or WebP"},
		"jpeg":             {[]byte("\xff\xd8\xff\xe0\x00\x10JFIF"), "PNG, SVG or WebP"},
		"html":             {[]byte("<html><body><script>alert(1)</script></body></html>"), "PNG, SVG or WebP"},
		"png truncated":    {testPNG(t, 2, 2)[:20], "PNG cannot be read"},
		"png huge":         {hugePNG(t), "4096"},
		"webp size":        {append(webp("VP8 \x00\x00\x00\x00"), 0, 0), "truncated"},
		"webp no image":    {webp("JUNK\x00\x00\x00\x00"), "no image"},
		"script":           {svg(`<script>alert(1)</script>`), "<script>"},
		"script any ns":    {svg(`<x:script xmlns:x="urn:x">alert(1)</x:script>`), "<script>"},
		"onload":           {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), "event handler"},
		"onclick upper":    {svg(`<rect ONCLICK="alert(1)"/>`), "event handler"},
		"foreignObject":    {svg(`<foreignObject><div xmlns="http://www.w3.org/1999/xhtml">x</div></foreignObject>`), "foreignObject"},
		"xhtml":            {svg(`<h:div xmlns:h="http://www.w3.org/1999/xhtml">x</h:div>`), "<div>"},
		"iframe":           {svg(`<iframe src="https://evil.example"/>`), "<iframe>"},
		"external href":    {svg(`<use xlink:href="https://evil.example/x.svg#a"/>`), "links to another file"},
		"relative href":    {svg(`<image href="other.png"/>`), "links to another file"},
		"javascript href":  {svg(`<a href="javascript:alert(1)"><rect/></a>`), "links to another file"},
		"javascript value": {svg(`<a xlink:title="x" target="java&#x09;script:alert(1)"><rect/></a>`), "javascript:"},
		"svg in svg":       {svg(`<image href="data:image/svg+xml;base64,PHN2Zy8+"/>`), "links to another file"},
		"animate href":     {svg(`<a href="#x"><set attributeName="href" to="javascript:alert(1)"/></a>`), "animates"},
		"animate handler":  {svg(`<animate attributeName="onbegin" values="alert(1)"/>`), "animates"},
		"doctype":          {[]byte(`<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY x "y">]><svg xmlns="http://www.w3.org/2000/svg">&x;</svg>`), "DOCTYPE"},
		"stylesheet pi":    {[]byte(`<?xml-stylesheet href="https://evil.example/a.css"?><svg xmlns="http://www.w3.org/2000/svg"/>`), "xml-stylesheet"},
		"style import":     {svg(`<style>@import url(https://evil.example/a.css);</style>`), "@import"},
		"style url":        {svg(`<style><![CDATA[rect { fill: url(https://evil.example/p.svg#a) }]]></style>`), "another file"},
		"style attr url":   {svg(`<rect style="background:url( 'https://evil.example/t.png' )"/>`), "another file"},
		"no namespace":     {[]byte(`<svg><rect/></svg>`), "root element"},
		"not xml":          {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect></svg>`), "not valid XML"},
		"not utf8":         {[]byte("<svg xmlns=\"http://www.w3.org/2000/svg\">\xff</svg>"), "UTF-8"},
	}
	for name, c := range bad {
		ct, err := checkLogo(c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %q %v, want an error with %q", name, ct, err, c.want)
		}
	}
}

func TestCheckInstanceName(t *testing.T) {
	for _, n := range []string{"", "Acme Backups", "Sao lưu dữ liệu – Prod #1", strings.Repeat("x", 60)} {
		if err := checkInstanceName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{strings.Repeat("x", 61), "a<b>", "line\nbreak"} {
		if err := checkInstanceName(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

// tokenFor creates a user with a role and returns a full-access API token for it.
func tokenFor(t *testing.T, s *Server, name, role string) string {
	t.Helper()
	u := &store.User{Username: name, Role: role, PasswordHash: "x"}
	if err := s.Store.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	raw := "mv_" + name + "_token"
	if err := s.Store.CreateToken(&store.APIToken{UserID: u.ID, Name: "t", Prefix: raw[:10], Hash: auth.SHA256Hex(raw), Access: store.TokenFull}); err != nil {
		t.Fatal(err)
	}
	return raw
}

func send(h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLogoUpload(t *testing.T) {
	s, _, _ := newSetupServer(t)
	s.WebFS = fstest.MapFS{
		"index.html":         {Data: []byte(`<!doctype html><html><head><meta charset="utf-8"><title>DumpCat: MySQL, MariaDB &amp; PostgreSQL Backup</title><link rel="icon" href="/assets/favicon.svg" type="image/svg+xml"></head><body></body></html>`)},
		"assets/favicon.svg": {Data: []byte("<svg/>")},
	}
	h := s.Handler()
	admin := tokenFor(t, s, "admin", store.RoleAdmin)
	viewer := tokenFor(t, s, "viewer", store.RoleViewer)

	// the default: the cat
	if rec := get(t, h, "/api/branding/logo"); rec.Code != http.StatusFound || rec.Header().Get("Location") != defaultLogo {
		t.Fatalf("default logo: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	svgB64 := base64.StdEncoding.EncodeToString([]byte(svgOK))
	if rec := send(h, "PUT", "/api/branding/logo", viewer, map[string]string{"data": svgB64}); rec.Code != 403 {
		t.Fatalf("a viewer uploaded a logo: %d", rec.Code)
	}
	if rec := send(h, "PUT", "/api/branding/logo", "", map[string]string{"data": svgB64}); rec.Code != 401 {
		t.Fatalf("anonymous upload: %d", rec.Code)
	}
	evil := base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`))
	if rec := send(h, "PUT", "/api/branding/logo", admin, map[string]string{"data": evil}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "event handler") {
		t.Fatalf("evil SVG: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.Store.GetLogo(); ok {
		t.Fatal("a refused logo was stored")
	}

	// a data: URL as the browser reads a file; the declared type does not matter, the content does
	rec := send(h, "PUT", "/api/branding/logo", admin, map[string]string{"data": "data:image/png;base64," + svgB64})
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var bv struct{ Name, Logo string }
	_ = json.Unmarshal(rec.Body.Bytes(), &bv)
	if bv.Name != "DumpCat" || !strings.HasPrefix(bv.Logo, "/api/branding/logo?v=") {
		t.Fatalf("brand view: %+v", bv)
	}
	rec = get(t, h, bv.Logo)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/svg+xml" || rec.Body.String() != svgOK ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("serve: %d %v", rec.Code, rec.Header())
	}
	if rec := get(t, h, "/api/branding/logo", "If-None-Match", rec.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation: %d", rec.Code)
	}
	// the sign-in page learns the logo; the page's icon and title follow
	var st struct {
		Brand struct{ Name, Logo string } `json:"brand"`
	}
	_ = json.Unmarshal(get(t, h, "/api/auth/status").Body.Bytes(), &st)
	if st.Brand.Logo != bv.Logo {
		t.Fatalf("status brand: %+v", st.Brand)
	}
	set := s.Store.GetSettings()
	set.InstanceName = "Acme <Backups> & Co"
	_ = s.Store.SaveSettings(set)
	page := get(t, h, "/").Body.String()
	if !strings.Contains(page, `<title>Acme &lt;Backups&gt; &amp; Co</title>`) || !strings.Contains(page, `<link rel="icon" href="`+bv.Logo+`" type="image/svg+xml">`) {
		t.Fatalf("index: %s", page)
	}

	// a PNG replaces it
	if rec := send(h, "PUT", "/api/branding/logo", admin, map[string]string{"data": base64.StdEncoding.EncodeToString(testPNG(t, 8, 8))}); rec.Code != 200 {
		t.Fatalf("png: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, "/api/branding/logo"); rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("png served as %q, %q", rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"))
	}

	// reset to the cat
	if rec := send(h, "DELETE", "/api/branding/logo", admin, nil); rec.Code != 200 {
		t.Fatalf("reset: %d", rec.Code)
	}
	if rec := get(t, h, "/api/branding/logo"); rec.Code != http.StatusFound {
		t.Fatalf("after reset: %d", rec.Code)
	}
	if page := get(t, h, "/").Body.String(); !strings.Contains(page, `href="/assets/`) {
		t.Fatalf("index icon after reset: %s", page)
	}
}
