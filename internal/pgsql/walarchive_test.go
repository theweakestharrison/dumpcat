package pgsql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dumpcat/internal/engine"
	"dumpcat/internal/tasklog"
)

func TestWALStaging(t *testing.T) {
	dir := t.TempDir()
	a := &walStream{h: &engine.LogHost{Dir: dir, Log: tasklog.Discard()}, segSize: seg16}
	touch := func(names ...string) {
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(n)), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }
	if a.resumeLSN() != 0 || len(a.Closed()) != 0 {
		t.Fatal("empty staging")
	}
	// segments 9 and 10 complete, 11 being received; an older timeline's partial
	touch("00000001000000000000000A", "000000010000000000000009", "00000001000000000000000B.partial", "000000010000000000000008.partial", "00000002.history")
	got := strings.Join(a.Closed(), " ")
	if got != "00000002.history 000000010000000000000008.partial 000000010000000000000009 00000001000000000000000A" {
		t.Fatalf("closed: %s", got)
	}
	if a.resumeLSN() != LSN(11*seg16) {
		t.Fatalf("resume at %s", a.resumeLSN())
	}
	// archived: the newest complete segment stays for pg_receivewal to resume after
	for _, n := range a.Closed() {
		a.Archived(n)
	}
	if !exists("00000001000000000000000A") || exists("000000010000000000000009") || exists("00000002.history") || exists("000000010000000000000008.partial") {
		t.Fatal("staged copies after the upload")
	}
	if c := a.Closed(); len(c) != 0 {
		t.Fatalf("closed after the upload: %v", c)
	}
	// the stream moved on: 11 complete, 12 received; an older upload keeps the newest
	os.Rename(filepath.Join(dir, "00000001000000000000000B.partial"), filepath.Join(dir, "00000001000000000000000B"))
	touch("00000001000000000000000C.partial")
	a.Archived("00000001000000000000000B")
	if exists("00000001000000000000000A") || !exists("00000001000000000000000B") {
		t.Fatal("kept segment not replaced")
	}
	// a gap: everything staged moves out of pg_receivewal's sight, still archived
	if err := a.hold(); err != nil {
		t.Fatal(err)
	}
	if a.resumeLSN() != 0 || !exists("hold/00000001000000000000000C.partial") || exists("00000001000000000000000B") {
		t.Fatalf("hold: resume %s", a.resumeLSN())
	}
	if c := strings.Join(a.Closed(), " "); c != "hold/00000001000000000000000C.partial" {
		t.Fatalf("closed after the gap: %s", c)
	}
	a.Archived("hold/00000001000000000000000C.partial")
	if exists("hold/00000001000000000000000C.partial") {
		t.Fatal("held file not removed")
	}
	// after a timeline switch pg_receivewal resumes on the newest timeline of the newest segment
	touch("00000001000000000000000D", "00000002000000000000000D.partial")
	if l, tli := a.resumePoint(); l != LSN(13*seg16) || tli != 2 {
		t.Fatalf("resume at %s on timeline %d", l, tli)
	}
}
