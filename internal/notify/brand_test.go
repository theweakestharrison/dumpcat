package notify

import (
	"bytes"
	"mime"
	"net/mail"
	"strings"
	"testing"
)

func TestBrandInMessages(t *testing.T) {
	ev := sampleEvents()["failed"]

	// the default stays as it was
	msg, err := ev.emailMessage("backup@example.com", []string{"ops@example.com"}, "backup-01")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := mail.ReadMessage(bytes.NewReader(msg))
	subj, _ := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if strings.HasPrefix(subj, "[") || !strings.Contains(ev.text("h"), "DumpCat on h") || ev.discord("h")["username"] != "DumpCat" {
		t.Fatalf("default brand: %q", subj)
	}

	ev.Brand = Brand{Name: "Acme Backups"}
	msg, err = ev.emailMessage("backup@example.com", []string{"ops@example.com"}, "backup-01")
	if err != nil {
		t.Fatal(err)
	}
	m, _ = mail.ReadMessage(bytes.NewReader(msg))
	subj, _ = new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if !strings.HasPrefix(subj, "[Acme Backups] ") {
		t.Fatalf("subject %q", subj)
	}
	page, _ := ev.emailHTML("backup-01")
	for _, want := range []string{`alt="Acme Backups"`, "Open in Acme Backups →", "Sent by Acme Backups on backup-01"} {
		if !strings.Contains(page, want) {
			t.Errorf("email lacks %q", want)
		}
	}
	if strings.Contains(page, "DumpCat") && !strings.Contains(page, "logo@dumpcat") {
		t.Error("the email still names DumpCat")
	}
	if txt := ev.text("h"); !strings.Contains(txt, "Open in Acme Backups: ") || !strings.Contains(txt, "Acme Backups on h") {
		t.Errorf("text: %s", txt)
	}
	if tg := ev.telegram(); !strings.Contains(tg, "Open in Acme Backups →") {
		t.Errorf("telegram: %s", tg)
	}
	if d := ev.discord("h"); d["username"] != "Acme Backups" {
		t.Errorf("discord username %v", d["username"])
	}
	// names Discord refuses fall back to DumpCat
	for _, n := range []string{"Discord backups", "a@b", "x:y"} {
		if got := (Brand{Name: n}).discordName(); got != "DumpCat" {
			t.Errorf("discord name for %q: %q", n, got)
		}
	}

	// the digest too, with a custom PNG logo attached
	d := sampleDigest()
	d.Brand = Brand{Name: "Acme Backups", Logo: []byte("\x89PNG custom")}
	msg, err = d.emailMessage("backup@example.com", []string{"ops@example.com"}, "backup-01")
	if err != nil {
		t.Fatal(err)
	}
	m, _ = mail.ReadMessage(bytes.NewReader(msg))
	subj, _ = new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if !strings.HasPrefix(subj, "[Acme Backups] 📊 Weekly digest") || !strings.Contains(d.text("h"), "Acme Backups on h") {
		t.Fatalf("digest subject %q", subj)
	}
	if (Brand{}).EmailLogo()[0] != 0x89 || !bytes.Equal(d.Brand.EmailLogo(), []byte("\x89PNG custom")) {
		t.Fatal("email logo")
	}
}
