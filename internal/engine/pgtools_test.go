package engine

import (
	"strings"
	"testing"
)

func TestPGMajor(t *testing.T) {
	for in, want := range map[string]int{"pg_dump (PostgreSQL) 16.15": 16, "pg_restore (PostgreSQL) 9.6.24": 9,
		"pg_dump (PostgreSQL) 18beta1": 18, "pg_dump (PostgreSQL) 17.2 (Debian 17.2-1.pgdg120+1)": 17, "mysqldump 8.0": 0} {
		if got := PGMajor(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

func TestPGFor(t *testing.T) {
	tl := NewTools(nil)
	tl.pg["pg_dump"] = []PGTool{{Path: "/17/pg_dump", Major: 17}, {Path: "/16/pg_dump", Major: 16}, {Path: "/14/pg_dump", Major: 14}}
	for want, path := range map[int]string{16: "/16/pg_dump", 15: "/16/pg_dump", 13: "/14/pg_dump", 17: "/17/pg_dump", 0: "/17/pg_dump"} {
		got, err := tl.PGFor("pg_dump", want)
		if err != nil || got.Path != path {
			t.Errorf("server %d: %v %v, want %s", want, got.Path, err, path)
		}
	}
	if _, err := tl.PGFor("pg_dump", 18); err == nil || !strings.Contains(err.Error(), "older than PostgreSQL 18") {
		t.Fatalf("a server newer than every tool: %v", err)
	}
	tl.pg["pg_restore"] = nil
	if _, err := tl.PGFor("pg_restore", 16); err == nil {
		t.Fatal("no tool at all must fail")
	}
}
