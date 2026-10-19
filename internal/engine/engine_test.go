package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dumpcat/internal/store"
)

func TestMydumperRegex(t *testing.T) {
	// PCRE lookarounds (not testable with Go's RE2): compare the generated text.
	got := mydumperRegex([]string{"shop", "my-app.db"}, []string{"shop.logs"})
	want := `^(?=(?:shop|my-app\.db)\.)(?!(?:shop\.logs$))`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestSafeFileName(t *testing.T) {
	cases := map[string]string{"shop": "shop", "my-app.db": "my-app.db", "a b/c": "a@20b@2fc", ".hidden": "_.hidden"}
	for in, want := range cases {
		if got := SafeFileName(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestCnf(t *testing.T) {
	dir := t.TempDir()
	s := &store.Server{Username: "bk", Password: `p#ss"w\rd`, Host: "db1", Port: 3307}
	p, env, err := WriteCnf(dir, s, CnfOptions{Groups: []string{"client"}, GlibGroups: []string{"mydumper"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	txt := string(b)
	if strings.Contains(txt, "[client]\nuser=bk\npassword") {
		t.Fatal("risky password must not be written to [client]")
	}
	if !strings.Contains(txt, `[mydumper]`) || !strings.Contains(txt, `password=p#ss"w\\rd`) {
		t.Fatalf("glib group: %s", txt)
	}
	if len(env) != 1 || env[0] != `MYSQL_PWD=p#ss"w\rd` {
		t.Fatal("MYSQL_PWD fallback")
	}
	if filepath.Dir(p) != dir {
		t.Fatal("dir")
	}
	s2 := &store.Server{Username: "bk", Password: "simple#pw", Host: "localhost", Socket: "/run/mysqld.sock"}
	p2, _, _ := WriteCnf(dir, s2, CnfOptions{Groups: []string{"client"}})
	b2, _ := os.ReadFile(p2)
	if !strings.Contains(string(b2), `password="simple#pw"`) || !strings.Contains(string(b2), "socket=/run/mysqld.sock") {
		t.Fatalf("quoted: %s", b2)
	}
}

func TestParseKV(t *testing.T) {
	kv := parseKV("backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 123\n")
	if kv["backup_type"] != "full-backuped" || kv["to_lsn"] != "123" {
		t.Fatal(kv)
	}
	m := map[string]any{}
	parseMydumperMetadata("[source]\nFile = mysql-bin.000003\nPosition = 4567\nExecuted_Gtid_Set = abc:1-5\n", m)
	if m["binlog_file"] != "mysql-bin.000003" || m["binlog_pos"] != int64(4567) || m["gtid"] != "abc:1-5" {
		t.Fatal(m)
	}
}

func TestParseBinlogHeaderGTID(t *testing.T) {
	for in, want := range map[string]string{
		"SET @@GLOBAL.GTID_PURGED='abc:1-5';\n":                              "abc:1-5",
		"SET @@GLOBAL.GTID_PURGED=/*!80000 '+'*/ 'abc:1-5,\ndef:1-3';\n":     "abc:1-5,def:1-3",
		"-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='binlog.000002';\n": "",
	} {
		m := map[string]any{}
		parseBinlogHeader(in, m)
		if got, _ := m["gtid"].(string); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}
