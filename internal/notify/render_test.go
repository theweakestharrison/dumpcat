package notify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleEvents() map[string]Event {
	t := time.Date(2026, 10, 19, 18, 43, 0, 0, time.UTC)
	ok := Event{Kind: "backup", Status: "success", Title: "Backup succeeded: shop-nightly", URL: "https://backup.example.com/#/runs/42", Time: t}
	ok.Add("Server", "db-prod-01")
	ok.Add("Method", "mysqldump / full")
	ok.Add("Duration", "3m 12s")
	ok.Add("Size", "1.24 GiB")
	ok.Add("Storage", "local-nas=ok, s3-offsite=ok")
	ok.Add("Integrity", "Verified: 4 files OK on local-nas (1 of 2 copies), 9s")
	bad := Event{Kind: "backup", Status: "failed", Title: "Backup FAILED: crm-hourly <prod>", URL: "https://backup.example.com/#/runs/43", Time: t,
		Error: "mysqldump: Got error: 1045: Access denied for user 'dumpcat'@'10.0.0.5' (using password: YES) when trying to connect"}
	bad.Add("Server", "db-crm")
	bad.Add("Method", "mysqldump / full")
	bad.Add("Duration", "2s")
	bad.Add("Storage", "local-nas=failed")
	part := Event{Kind: "backup", Status: "partial", Title: "Backup partially completed: shop-nightly", Time: t, Error: "s3-offsite: upload failed: 403 Forbidden"}
	part.Add("Storage", "local-nas=ok, s3-offsite=failed")
	rs := Event{Kind: "restore", Status: "success", Title: "Restore succeeded: shop-nightly", URL: "https://backup.example.com/#/restores/7", Time: t}
	rs.Add("Backup", "20261019-020000-full")
	rs.Add("Target server", "db-staging")
	rs.Add("User", "admin")
	rs.Add("Duration", "1m 4s")
	return map[string]Event{"success": ok, "failed": bad, "partial": part, "restore": rs}
}

func TestEmailMessage(t *testing.T) {
	ev := sampleEvents()["failed"]
	raw, err := ev.emailMessage("DumpCat <backup@example.com>", []string{"ops@example.com", "dba@example.com"}, "backup-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range bytes.Split(raw, []byte("\r\n")) {
		if len(ln) > 998 {
			t.Fatalf("line longer than SMTP allows: %d", len(ln))
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subj != "❌ Backup FAILED: crm-hourly <prod>" || msg.Header.Get("Auto-Submitted") != "auto-generated" {
		t.Fatalf("headers: %q %q", subj, msg.Header.Get("Auto-Submitted"))
	}
	mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mt != "multipart/alternative" {
		t.Fatal(mt)
	}
	alt := multipart.NewReader(msg.Body, params["boundary"])
	p, err := alt.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(quotedprintable.NewReader(p))
	for _, want := range []string{"❌ Backup FAILED: crm-hourly <prod>", "Backup · Failed", "Server    db-crm", "Access denied", "Open in DumpCat: https://backup.example.com/#/runs/43", "DumpCat on backup-01"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("text part lacks %q:\n%s", want, text)
		}
	}
	p, err = alt.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	_, rp, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
	rel := multipart.NewReader(p, rp["boundary"])
	hp, err := rel.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(quotedprintable.NewReader(hp))
	for _, want := range []string{`src="cid:logo@dumpcat"`, "Backup FAILED: crm-hourly &lt;prod&gt;", "Backup · Failed", "#cf222e", "Access denied for user &#39;dumpcat&#39;",
		`href="https://backup.example.com/#/runs/43"`, `href="https://backup.example.com/#/notifications"`, "local-nas", ">failed<"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	if strings.Contains(string(page), "<prod>") {
		t.Error("the title is not escaped")
	}
	lp, err := rel.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if lp.Header.Get("Content-Id") != "<logo@dumpcat>" {
		t.Fatalf("logo part: %v", lp.Header)
	}
	png, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, lp))
	if !bytes.Equal(png, logoPNG) {
		t.Fatal("logo differs")
	}
}

func TestChatFormats(t *testing.T) {
	ev := sampleEvents()["failed"]
	tg := ev.telegram()
	if !strings.Contains(tg, "<b>Backup FAILED: crm-hourly &lt;prod&gt;</b>") || !strings.Contains(tg, "<pre>mysqldump: Got error") || !strings.Contains(tg, `<a href="https://backup.example.com/#/runs/43">`) {
		t.Fatalf("telegram:\n%s", tg)
	}
	sl, _ := json.Marshal(ev.slack("backup-01"))
	for _, want := range []string{`"color":"#cf222e"`, `crm-hourly \u0026lt;prod\u0026gt;`, `"type":"button"`, `backup-01`} {
		if !strings.Contains(string(sl), want) {
			t.Errorf("slack lacks %s: %s", want, sl)
		}
	}
	dc := ev.discord("backup-01")
	embed := dc["embeds"].([]any)[0].(map[string]any)
	if embed["color"] != 0xcf222e || embed["url"] != "https://backup.example.com/#/runs/43" || len(embed["fields"].([]any)) != 4 {
		t.Fatalf("discord: %v", embed)
	}
	// no link without a base URL
	noURL := sampleEvents()["partial"]
	if strings.Contains(noURL.telegram(), "href") || noURL.discord("x")["embeds"].([]any)[0].(map[string]any)["url"] != nil {
		t.Fatal("a link without a URL")
	}
}

// DUMPCAT_EMAIL_PREVIEW=<dir> go test ./internal/notify -run TestEmailPreview
// writes the sample emails as HTML files to look at in a browser.
func TestEmailPreview(t *testing.T) {
	dir := os.Getenv("DUMPCAT_EMAIL_PREVIEW")
	if dir == "" {
		t.Skip("set DUMPCAT_EMAIL_PREVIEW to a directory")
	}
	logo := "data:image/png;base64," + base64.StdEncoding.EncodeToString(logoPNG)
	for name, ev := range sampleEvents() {
		page, err := ev.emailHTML("backup-01")
		if err != nil {
			t.Fatal(err)
		}
		page = strings.ReplaceAll(page, "cid:logo@dumpcat", logo)
		if err := os.WriteFile(filepath.Join(dir, "email-"+name+".html"), []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
