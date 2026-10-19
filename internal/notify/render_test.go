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
	stale := Event{Kind: "stale", Status: "failed", Title: "No recent backup: crm-hourly", URL: "https://backup.example.com/#/runs?job_id=3", Time: t,
		Error: "mysqldump: Got error: 2013: Lost connection to server during query"}
	stale.Add("Server", "db-crm")
	stale.Add("Last successful backup", "19 Oct 2026, 14:00 (4h43m ago)")
	stale.Add("Expected", "every 1h (schedule 0 * * * *), alert after 2h30m")
	stale.Add("Last run", "failed 43m ago")
	back := Event{Kind: "stale", Status: StatusRecovered, Title: "Backups are fresh again: crm-hourly", URL: "https://backup.example.com/#/runs?job_id=3", Time: t}
	back.Add("Newest backup", "19 Oct 2026, 18:40")
	back.Add("Overdue for", "2h13m")
	lag := Event{Kind: "binlog_lag", Status: "failed", Title: "Binlog archiving is behind: shop-nightly", URL: "https://backup.example.com/#/binlogs", Time: t}
	lag.Add("Server", "db-prod-01")
	lag.Add("Behind by", "47m")
	lag.Add("Alert after", "30m")
	return map[string]Event{"success": ok, "failed": bad, "partial": part, "restore": rs, "stale": stale, "recovered": back, "binlog-lag": lag}
}

// sampleDigest tells the story of the web demo (site-src/demo), whose
// Preview shows it (scripts/build-demo.py).
func sampleDigest() *Digest {
	t := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	ago := func(h int) *time.Time { x := t.Add(-time.Duration(h) * time.Hour); return &x }
	return &Digest{From: t.AddDate(0, 0, -7), To: t, Time: t, URL: "https://backup.example.com/#/dashboard",
		Written: 61 << 30, PrevWritten: 58 << 30,
		Jobs: []DigestJob{
			{Name: "crm-every-6h", Server: "crm-mariadb", OK: 27, Failed: 1, VerifyOK: 26, VerifyFailed: 1, Size: 512 << 20, PrevSize: 498 << 20, LastSuccess: ago(2)},
			{Name: "erp-nightly", Server: "erp-db", OK: 5, Failed: 2, VerifyOK: 5, Size: 1454 << 20, PrevSize: 1430 << 20, LastSuccess: ago(54), Overdue: true},
			{Name: "prod-physical", Server: "db-prod-01", OK: 7, VerifyOK: 7, Size: 9523 << 20, PrevSize: 9318 << 20, LastSuccess: ago(7)},
			{Name: "shop-nightly", Server: "db-prod-01", OK: 6, Failed: 1, VerifyOK: 6, Size: 2150 << 20, PrevSize: 2090 << 20, LastSuccess: ago(6)},
			{Name: "warehouse-weekly", Server: "analytics", OK: 1, VerifyOK: 1, Size: 42 << 30, PrevSize: 41 << 30, LastSuccess: ago(29)},
		},
		Overdue: []DigestOverdue{{Job: "erp-nightly", LastSuccess: ago(54), Due: t.Add(-17*time.Hour - 30*time.Minute)}},
		Storages: []DigestStorage{
			{Name: "nas-backups", Type: "local", Backups: 61, Bytes: 1126 << 30, Free: 7066 << 30, Total: 8192 << 30},
			{Name: "r2-offsite", Type: "s3", Backups: 44, Bytes: 846 << 30},
			{Name: "gdrive-archive", Type: "gdrive", Backups: 8, Bytes: 312 << 30},
			{Name: "dr-site", Type: "sftp", Backups: 14, Bytes: 140 << 30},
		},
		Drills: []DigestDrill{
			{Name: "crm daily drill", Job: "crm-every-6h", Passed: 6, Failed: 1, LastStatus: "failed", RTO: 2*time.Minute + 41*time.Second,
				LastError: "crm.tickets: 41233 rows in the copy, the backup recorded 41260\nmore lines"},
			{Name: "shop weekly drill", Job: "shop-nightly", Passed: 1, LastStatus: "passed", RTO: 4*time.Minute + 18*time.Second},
		},
	}
}

func TestDigestMessage(t *testing.T) {
	d := sampleDigest()
	raw, err := d.emailMessage("DumpCat <backup@example.com>", []string{"ops@example.com"}, "backup-01")
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
	if subj != "📊 Weekly digest: 46 of 50 backups succeeded (12 – 19 Oct 2026)" {
		t.Fatalf("subject: %q", subj)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	alt := multipart.NewReader(msg.Body, params["boundary"])
	p, err := alt.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(quotedprintable.NewReader(p))
	for _, want := range []string{"Succeeded  46 of 50 backups", "Written    61.00 GiB (+5.2% vs last week)", "Integrity  45 ok, 1 failed",
		"erp-nightly — last successful backup 17 Oct, 02:00 (2d ago), a new one was due by 18 Oct, 14:30",
		"shop-nightly      backups 6 ok, 1 failed · size 2.10 GiB (+2.9%) · checks 6 ok · last success 6h ago",
		"crm-every-6h      backups 27 ok, 1 failed · size 512.00 MiB (+2.8%) · checks 26 ok, 1 failed · last success 2h ago",
		"warehouse-weekly  backups 1 ok · size 42.00 GiB (+2.4%)", "Storage (2.37 TiB in total)", "nas-backups     61 backups · 1.10 TiB · free 6.90 TiB (86%)",
		"Restore drills", "  crm daily drill    crm-every-6h · 6 passed, 1 failed · restore 2m41s · last run FAILED: crm.tickets: 41233 rows in the copy, the backup recorded 41260",
		"  shop weekly drill  shop-nightly · 1 passed · restore 4m18s",
		"Open in DumpCat: https://backup.example.com/#/dashboard"} {
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
	for _, want := range []string{`src="cid:logo@dumpcat"`, "Weekly digest · 12 – 19 Oct 2026", "46 of 50 backups succeeded", "#9a6700", "erp-nightly",
		">2 failed<", ">1 failed<", ">26 ok, 1 failed<", "Restore drills", "crm-every-6h · restore 2m41s", "Last run failed: crm.tickets: 41233 rows",
		">6 passed<", "shop weekly drill", "Storage · 2.37 TiB in total", "free 6.90 TiB (86%)", `href="https://backup.example.com/#/notifications"`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	if lp, err := rel.NextPart(); err != nil || lp.Header.Get("Content-Id") != "<logo@dumpcat>" {
		t.Fatalf("logo part: %v", err)
	}
	// names are escaped
	odd := &Digest{From: d.From, To: d.To, Time: d.Time, Jobs: []DigestJob{{Name: "crm <prod>", OK: 1}}, Overdue: []DigestOverdue{{Job: "crm <prod>", Due: d.Time}}}
	if h, err := odd.emailHTML("h"); err != nil || !strings.Contains(h, "crm &lt;prod&gt;") || strings.Contains(h, "crm <prod>") {
		t.Fatalf("escaping: %v", err)
	}
	// a quiet week
	quiet := &Digest{From: d.From, To: d.To, Time: d.Time}
	if ev := quiet.event(); ev.Title != "No backups ran this week" || ev.Status != "success" {
		t.Fatalf("quiet week: %+v", ev)
	}
}

func TestAlertLook(t *testing.T) {
	ev := sampleEvents()
	if l := (&Event{Kind: "stale", Status: "failed"}).look(); l.Status != "Overdue" || l.Kind != "Backup freshness" {
		t.Fatalf("stale: %+v", l)
	}
	back := ev["recovered"]
	if l := back.look(); l.Status != "Recovered" || l.Color != "#1a7f37" || back.mark() != "✓" {
		t.Fatalf("recovered: %+v", l)
	}
	if !strings.Contains(back.text("h"), "Backup freshness · Recovered") {
		t.Fatal(back.text("h"))
	}
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
	pages := map[string]func() (string, error){"digest": func() (string, error) { return sampleDigest().emailHTML("backup-01") }}
	for name, ev := range sampleEvents() {
		pages[name] = func() (string, error) { return ev.emailHTML("backup-01") }
	}
	for name, render := range pages {
		page, err := render()
		if err != nil {
			t.Fatal(err)
		}
		page = strings.ReplaceAll(page, "cid:logo@dumpcat", logo)
		if err := os.WriteFile(filepath.Join(dir, "email-"+name+".html"), []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "email-digest.txt"), []byte(sampleDigest().text("backup-01")), 0o644); err != nil {
		t.Fatal(err)
	}
}
