package runner

import (
	"strings"
	"testing"

	"dumpcat/internal/store"
)

func TestParseVersion(t *testing.T) {
	cases := map[string][2]int{"8.0.36": {8, 0}, "10.11.6-MariaDB-log": {10, 11}, " 5.7.44-log": {5, 7}, "": {0, 0}, "abc": {0, 0}}
	for in, want := range cases {
		if a, b := parseVersion(in); a != want[0] || b != want[1] {
			t.Errorf("%q -> %d.%d want %d.%d", in, a, b, want[0], want[1])
		}
	}
}

func TestCompatNotes(t *testing.T) {
	logical := func(flavor, version string, meta map[string]any) *store.Run {
		m := map[string]any{"flavor": flavor, "server_version": version}
		for k, v := range meta {
			m[k] = v
		}
		return &store.Run{Method: store.MethodMysqldump, Meta: m}
	}

	// same server, same version: nothing to do
	w, s := compatNotes(logical("mysql", "8.0.36", nil), "mysql", "8.0.36", true)
	if len(w) != 0 || s.Any() {
		t.Fatalf("same server: %v %+v", w, s)
	}
	// another instance of the same version: only DEFINER
	w, s = compatNotes(logical("mysql", "8.0.36", nil), "mysql", "8.0.40", false)
	if len(w) != 0 || !s.StripDefiner || s.ConvertCollations || s.SkipGTIDPurged {
		t.Fatalf("other instance: %v %+v", w, s)
	}
	// MySQL 8 → MariaDB: collations
	w, s = compatNotes(logical("mysql", "8.0.36", nil), "mariadb", "10.11.6-MariaDB", false)
	if len(w) != 1 || !strings.Contains(w[0], "MariaDB") || !s.ConvertCollations {
		t.Fatalf("mysql → mariadb: %v %+v", w, s)
	}
	// MySQL 8.4 → 5.7: older target, collations
	w, s = compatNotes(logical("mysql", "8.4.2", nil), "mysql", "5.7.44", false)
	if len(w) != 1 || !strings.Contains(w[0], "older") || !s.ConvertCollations {
		t.Fatalf("8.4 → 5.7: %v %+v", w, s)
	}
	// 8.4 → 8.0 is older but shares the collations
	_, s = compatNotes(logical("mysql", "8.4.2", nil), "mysql", "8.0.36", false)
	if s.ConvertCollations {
		t.Fatal("8.4 → 8.0 must not convert collations")
	}
	// a dump that sets GTID_PURGED
	w, s = compatNotes(logical("mysql", "8.0.36", map[string]any{"gtid": "uuid:1-5"}), "mysql", "8.0.36", true)
	if len(w) != 1 || !s.SkipGTIDPurged {
		t.Fatalf("gtid: %v %+v", w, s)
	}
	// MariaDB gtid_slave_pos is only a comment in the dump
	_, s = compatNotes(logical("mariadb", "11.4.2-MariaDB", map[string]any{"gtid": "0-1-100"}), "mariadb", "11.4.2-MariaDB", true)
	if s.SkipGTIDPurged {
		t.Fatal("mariadb gtid must not be skipped")
	}

	phys := &store.Run{Method: store.MethodPhysical, Meta: map[string]any{"flavor": "mysql", "server_version": "8.0.36"}}
	if w, s := compatNotes(phys, "mysql", "8.0.40", false); len(w) != 0 || s.Any() {
		t.Fatalf("physical, same series: %v %+v", w, s)
	}
	if w, _ := compatNotes(phys, "mysql", "8.4.2", false); len(w) != 1 {
		t.Fatalf("physical, other series: %v", w)
	}
	if w, _ := compatNotes(phys, "mariadb", "10.11.6-MariaDB", false); len(w) != 1 || !strings.Contains(w[0], "logical") {
		t.Fatalf("physical, other flavor: %v", w)
	}
}
