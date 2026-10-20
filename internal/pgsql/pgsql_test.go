package pgsql

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dumpcat/internal/store"
)

func TestSplitSQL(t *testing.T) {
	script := `--
-- PostgreSQL database cluster dump
--

\restrict abc123

SET default_transaction_read_only = off;
CREATE ROLE "odd;name";
ALTER ROLE "odd;name" WITH LOGIN PASSWORD 'it''s; secret';
COMMENT ON ROLE x IS E'line\'s; two';
/* block ; comment /* nested ; */ */
DO $body$ BEGIN PERFORM 1; END $body$;
\unrestrict abc123
GRANT a TO b`
	got := SplitSQL(script)
	want := []string{
		"SET default_transaction_read_only = off",
		`CREATE ROLE "odd;name"`,
		`ALTER ROLE "odd;name" WITH LOGIN PASSWORD 'it''s; secret'`,
		`COMMENT ON ROLE x IS E'line\'s; two'`,
		"DO $body$ BEGIN PERFORM 1; END $body$",
		"GRANT a TO b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct{ st, kind, role string }{
		{"CREATE ROLE shop_owner", "create role", "shop_owner"},
		{`CREATE ROLE "Mixed ""Q"" Role"`, "create role", `Mixed "Q" Role`},
		{"ALTER ROLE shop_owner WITH LOGIN", "alter role", "shop_owner"},
		{"ALTER ROLE x IN DATABASE y SET search_path = a", "alter role", "x"},
		{"CREATE TABLESPACE fast OWNER x LOCATION '/ssd'", "tablespace", ""},
		{"GRANT CREATE ON TABLESPACE fast TO x", "tablespace", ""},
		{"GRANT reader TO owner GRANTED BY postgres", "", ""},
	}
	for _, c := range cases {
		if k, r := classify(c.st); k != c.kind || r != c.role {
			t.Errorf("%s: %q %q", c.st, k, r)
		}
	}
}

func TestLineCounter(t *testing.T) {
	for in, want := range map[string]int64{
		"1\ta\n2\tb\n\\.\n\n\n":  2,
		"\\.\n\n":                0,
		"\n\n\\\\.\n\\.\n":       3, // two empty strings and a literal "\." are rows
		"a\nb\n":                 2, // no terminator
		"x\r\n\\.\r\ny\n":        1,
		"1\tmulti\\nline\n\\.\n": 1,
	} {
		c := &lineCounter{}
		// byte by byte: the terminator may be split across writes
		for i := 0; i < len(in); i++ {
			c.Write([]byte{in[i]})
		}
		if c.rows() != want {
			t.Errorf("%q: %d rows, want %d", in, c.rows(), want)
		}
	}
}

func TestConnInfoQuoting(t *testing.T) {
	s := &store.Server{Engine: store.EnginePostgres, Name: "s", Host: "db.example.com", Port: 5433, Username: `o'brien\x`, TLSMode: "verify", CACert: "PEM"}
	dir := t.TempDir()
	ci, err := ConnInfo(s, "a=b host=evil", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`host='db.example.com'`, `port='5433'`, `user='o\'brien\\x'`, `dbname='a=b host=evil'`, `sslmode='verify-full'`, `sslrootcert=` + conninfoValue(filepath.Join(dir, "pg-ca.pem"))} {
		if !strings.Contains(ci, want) {
			t.Errorf("%s: missing %s", ci, want)
		}
	}
	if strings.Contains(ci, "password") {
		t.Fatal("the password must not be in the connection string")
	}
	if _, err := ConnInfo(s, "x\ny", dir); err == nil {
		t.Fatal("a line break in a name must be refused")
	}
	s.Socket, s.Host = "/var/run/postgresql/.s.PGSQL.5434", ""
	ci, _ = ConnInfo(s, "d", dir)
	if !strings.Contains(ci, `host='/var/run/postgresql'`) || !strings.Contains(ci, `port='5434'`) || strings.Contains(ci, "sslmode") {
		t.Fatalf("socket file: %s", ci)
	}
}

func TestWritePassFile(t *testing.T) {
	dir := t.TempDir()
	env, err := WritePassFile(dir, &store.Server{Username: "u:1", Password: `p\a:ss`})
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PGPASSFILE="); ok {
			path = v
		}
		if strings.HasPrefix(e, "PGPASSWORD=") && e != "PGPASSWORD=" {
			t.Fatal("the password must not be in the environment")
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `*:*:*:u\:1:p\\a\:ss`+"\n" {
		t.Fatalf("passfile: %q", b)
	}
	if _, err := WritePassFile(dir, &store.Server{Name: "s", Username: "u", Password: "a\nb"}); err == nil {
		t.Fatal("a password with a line break cannot be stored")
	}
}

func TestNames(t *testing.T) {
	d := Driver{}
	for _, bad := range []string{"-x", strings.Repeat("a", 64), "a\x00b", "a\tb", "\xff"} {
		if d.CheckName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"shop_restore", "a=b", "Ünïcode", strings.Repeat("a", 63)} {
		if err := d.CheckName(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	if n := d.TempName(strings.Repeat("é", 40), "__drill_12"); len(n) > 63 || !strings.HasSuffix(n, "__drill_12") || !strings.HasPrefix(n, "éé") {
		t.Fatalf("temp name %q (%d bytes)", n, len(n))
	}
	if n := d.TempName("shop", "__drill_1"); n != "shop__drill_1" {
		t.Fatal(n)
	}
}

func TestTablesFor(t *testing.T) {
	got := tablesFor([]string{"shop.audit", "shop.sales.big", "crm.x", "shop."}, "shop")
	if !reflect.DeepEqual(got, []string{"audit", "sales.big"}) {
		t.Fatal(got)
	}
}
