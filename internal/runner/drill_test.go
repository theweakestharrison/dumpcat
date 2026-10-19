package runner

import (
	"database/sql"
	"strings"
	"testing"

	"dumpcat/internal/store"
)

func TestDrillTempName(t *testing.T) {
	taken := map[string]bool{"shop": true, "shop__drill_7": true}
	if got := drillTempName("shop", 7, taken); got != "shop__drill_7_1" {
		t.Fatalf("a taken name must not be reused, got %s", got)
	}
	if got := drillTempName("crm", 7, taken); got != "crm__drill_7" {
		t.Fatalf("got %s", got)
	}
	long := strings.Repeat("đ", 70)
	a := drillTempName(long, 12345, taken)
	b := drillTempName(long, 12345, taken)
	if n := len([]rune(a)); n != 64 || !strings.HasSuffix(a, "__drill_12345") {
		t.Fatalf("long name: %d runes, %s", n, a)
	}
	if a == b || len([]rune(b)) != 64 || !strings.HasSuffix(b, "__drill_12345_1") {
		t.Fatalf("names cut to the same prefix must differ: %s / %s", a, b)
	}
	for _, n := range []string{a, b, "shop__drill_7_1", "Shop__DRILL_7"} {
		id := int64(12345)
		if strings.Contains(strings.ToLower(n), "drill_7") {
			id = 7
		}
		if !isDrillTemp(n, id) {
			t.Errorf("%s should be a copy of drill run %d", n, id)
		}
	}
	for _, n := range []string{"shop", "shop__drill_71", "shop__drill_7x", "shop__drill_7_1_prod", "x__drill_17"} {
		if isDrillTemp(n, 7) {
			t.Errorf("%s must never be dropped by drill run 7", n)
		}
	}
}

func TestValidateAssertion(t *testing.T) {
	good := []string{"SELECT COUNT(*) > 0 FROM orders", " select 1;", "WITH x AS (SELECT 1 a) SELECT a FROM x", "(SELECT 1)"}
	for _, q := range good {
		if err := ValidateAssertion(q); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	bad := []string{"", "DELETE FROM orders", "DROP DATABASE shop", "SELECT 1; DROP DATABASE shop", "SELECT * FROM t INTO OUTFILE '/tmp/x'", "UPDATE t SET a=1", "selector"}
	for _, q := range bad {
		if err := ValidateAssertion(q); err == nil {
			t.Errorf("%q must be refused", q)
		}
	}
}

func TestTruthy(t *testing.T) {
	cases := map[string]bool{"1": true, "0": false, "0.0": false, "-3": true, "": false, " ": false, "false": false, "FALSE": false, "yes": true, "2026-10-19": true}
	for v, want := range cases {
		if got := truthy(sql.NullString{String: v, Valid: true}); got != want {
			t.Errorf("%q: got %v", v, got)
		}
	}
	if truthy(sql.NullString{}) {
		t.Error("NULL must be false")
	}
}

func TestDrillRunnable(t *testing.T) {
	sid := int64(1)
	ok := store.Run{ID: 1, Status: store.StatusSuccess, Method: store.MethodMysqldump, Meta: map[string]any{},
		Locations: []store.RunLocation{{Status: store.LocOK, StorageID: &sid}}}
	if err := drillRunnable(&ok); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(r *store.Run){
		"physical": func(r *store.Run) { r.Method = store.MethodPhysical },
		"single":   func(r *store.Run) { r.Meta["single_file"] = true },
		"failed":   func(r *store.Run) { r.Status = store.StatusFailed },
		"nopass":   func(r *store.Run) { r.Encrypted = true },
		"nocopy":   func(r *store.Run) { r.Locations = nil },
	} {
		r := ok
		r.Meta = map[string]any{}
		mod(&r)
		if drillRunnable(&r) == nil {
			t.Errorf("%s: must not be drilled", name)
		}
	}
}

func TestHumanCount(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := humanCount(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}
