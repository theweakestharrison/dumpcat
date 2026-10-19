package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/secret"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

// TestDrillE2E backs up two databases of a real server and drills the
// backup into the same server under temporary names. It needs a disposable
// MySQL/MariaDB server (databases named dumpcat_e2e_* are created and
// dropped) and its client tools (found like DumpCat does, or in
// DUMPCAT_TOOLS_DIR):
//
//	DUMPCAT_E2E_MYSQL='root:secret@127.0.0.1:3312' go test ./internal/runner -run DrillE2E -v
func TestDrillE2E(t *testing.T) {
	dsn := os.Getenv("DUMPCAT_E2E_MYSQL")
	if dsn == "" {
		t.Skip("set DUMPCAT_E2E_MYSQL=user:password@host:port to run against a disposable server")
	}
	cred, addr, _ := strings.Cut(dsn, "@")
	user, pass, _ := strings.Cut(cred, ":")
	host, portS, _ := strings.Cut(addr, ":")
	port, _ := strconv.Atoi(portS)
	ctx := context.Background()

	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "master.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "dumpcat.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tools := engine.NewTools(func() map[string]string { return nil })
	sm := storage.NewManager(st, filepath.Join(dir, "rclone"), func() string { return "" }, func() []string { return nil })
	r := New(st, sm, tools, nil, dir, "e2e")
	defer r.Shutdown(time.Minute)

	src := &store.Server{Name: "source", Host: host, Port: port, Username: user, Password: pass, Flavor: "auto"}
	if err := st.CreateServer(src); err != nil {
		t.Fatal(err)
	}
	disk := &store.Storage{Name: "disk", Type: "local", Config: map[string]string{"path": filepath.Join(dir, "backups")}}
	if err := st.CreateStorage(disk); err != nil {
		t.Fatal(err)
	}
	db, err := dbconn.Open(ctx, src, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	schemas := func() map[string]bool {
		rows, err := db.QueryContext(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'dumpcat_e2e%'")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var n string
			rows.Scan(&n)
			out[n] = true
		}
		return out
	}
	dropAll := func() {
		for n := range schemas() {
			db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbconn.QuoteIdent(n))
		}
	}
	dropAll()
	defer dropAll()
	exec("CREATE DATABASE dumpcat_e2e_shop")
	exec("CREATE TABLE dumpcat_e2e_shop.orders (id INT PRIMARY KEY, note VARCHAR(100), body BLOB)")
	exec("CREATE TABLE dumpcat_e2e_shop.items (id INT AUTO_INCREMENT PRIMARY KEY, order_id INT, qty INT)")
	exec("CREATE TABLE dumpcat_e2e_shop.empty_one (id INT)")
	exec("CREATE VIEW dumpcat_e2e_shop.big_orders AS SELECT id FROM dumpcat_e2e_shop.orders WHERE id > 50")
	exec("CREATE TRIGGER dumpcat_e2e_shop.item_ai AFTER INSERT ON dumpcat_e2e_shop.items FOR EACH ROW UPDATE dumpcat_e2e_shop.orders SET note = CONCAT(note, '+') WHERE id = NEW.order_id")
	var vals []string
	for i := 1; i <= 120; i++ {
		vals = append(vals, fmt.Sprintf("(%d, 'order (%d); it''s \\\\ \"quoted\"', 0x%02x00ff)", i, i, i))
	}
	exec("INSERT INTO dumpcat_e2e_shop.orders VALUES " + strings.Join(vals, ","))
	vals = vals[:0]
	for i := 1; i <= 100; i++ {
		vals = append(vals, fmt.Sprintf("(%d, %d)", 1+i%120, i))
	}
	exec("INSERT INTO dumpcat_e2e_shop.items (order_id, qty) VALUES " + strings.Join(vals, ","))
	exec("CREATE DATABASE dumpcat_e2e_crm")
	exec("CREATE TABLE dumpcat_e2e_crm.customers (id INT PRIMARY KEY, name VARCHAR(50))")
	exec("INSERT INTO dumpcat_e2e_crm.customers VALUES (1,'Ann'),(2,'Bo'),(3,'Cy')")

	job := &store.Job{UUID: NewUUID(), Name: "e2e", ServerID: src.ID, Method: store.MethodMysqldump, DBMode: "include",
		Databases: []string{"dumpcat_e2e_shop", "dumpcat_e2e_crm"}, ExcludeTables: []string{}, Compression: "zstd", Threads: 2, Enabled: true,
		Options: store.JobOptions{Routines: true, Triggers: true, Events: true, VerifyMode: store.VerifyOff},
		Targets: []store.JobTarget{{StorageID: disk.ID}}}
	if err := st.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	run, err := r.StartBackup(job.ID, store.TypeFull, "manual", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		x, _ := st.GetRun(run.ID)
		return x.Status != store.StatusQueued && x.Status != store.StatusRunning
	})
	if run, _ = st.GetRun(run.ID); run.Status != store.StatusSuccess {
		t.Fatalf("backup: %s %s\n%s", run.Status, run.Error, readLog(r.RunLogPath(run.ID)))
	}
	stats, _ := st.RunTables(run.ID)
	want := store.TableStats{"dumpcat_e2e_shop": {"orders": 120, "items": 100, "empty_one": 0}, "dumpcat_e2e_crm": {"customers": 3}}
	if fmt.Sprint(stats) != fmt.Sprint(want) {
		t.Fatalf("row counts of the backup: %v, want %v", stats, want)
	}

	drill := &store.Drill{JobID: job.ID, Name: "e2e drill", TargetServerID: src.ID, Enabled: true, Databases: []string{},
		Assertions: []store.DrillAssertion{
			{Name: "orders restored", Database: "dumpcat_e2e_shop", SQL: "SELECT COUNT(*) = 120 FROM orders"},
			{Name: "quantities", Database: "dumpcat_e2e_shop", SQL: "SELECT SUM(qty) = 5050 FROM items"},
			{Name: "view works", Database: "dumpcat_e2e_shop", SQL: "SELECT COUNT(*) FROM big_orders"},
		},
		NotifyFailure: true}
	if err := st.CreateDrill(drill); err != nil {
		t.Fatal(err)
	}
	drillOnce := func() *store.DrillRun {
		t.Helper()
		x, err := r.StartDrill(drill.ID, 0, "manual", "e2e")
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { y, _ := st.GetDrillRun(x.ID); return y.Status != store.StatusRunning })
		x, _ = st.GetDrillRun(x.ID)
		for n := range schemas() {
			if strings.Contains(n, "__drill_") {
				t.Fatalf("drill #%d left %s behind\n%s", x.ID, n, readLog(r.DrillLogPath(x.ID)))
			}
		}
		if !schemas()["dumpcat_e2e_shop"] || !schemas()["dumpcat_e2e_crm"] {
			t.Fatal("the source databases must be untouched")
		}
		return x
	}
	x := drillOnce()
	if x.Status != store.DrillPassed {
		t.Fatalf("drill: %s %s\n%s", x.Status, x.Error, readLog(r.DrillLogPath(x.ID)))
	}
	if x.RestoreMs <= 0 || len(x.Databases) != 2 || x.Databases[0].Rows+x.Databases[1].Rows != 223 {
		t.Fatalf("drill result: %+v", x)
	}
	if run, _ = st.GetRun(run.ID); run.DrillStatus != store.DrillPassed {
		t.Fatalf("the backup must be marked restore-tested: %q", run.DrillStatus)
	}
	var n int64
	if db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dumpcat_e2e_shop.orders WHERE note LIKE '%+'").Scan(&n); n == 0 {
		t.Fatal("fixture: the trigger should have run on the source")
	}

	// a backup whose rows differ from what it recorded fails the drill
	bad := store.TableStats{"dumpcat_e2e_shop": {"orders": 121, "items": 100, "empty_one": 0, "gone": 5}, "dumpcat_e2e_crm": {"customers": 3}}
	st.SetRunTables(run.ID, bad)
	x = drillOnce()
	if x.Status != store.DrillFailed || !hasCheck(x, "rows", store.CheckFailed) || !hasCheck(x, "tables", store.CheckFailed) {
		t.Fatalf("tampered counts must fail: %s %+v", x.Status, x.Checks)
	}

	// a backup made before row counting: tables compared with information_schema, rows skipped
	st.SetRunTables(run.ID, nil)
	x = drillOnce()
	if x.Status != store.DrillPassed || !hasCheck(x, "rows", store.CheckSkipped) {
		t.Fatalf("legacy backup: %s %s %+v", x.Status, x.Error, x.Checks)
	}
	st.SetRunTables(run.ID, stats)

	// a false assertion fails it
	drill.Assertions = append(drill.Assertions, store.DrillAssertion{Name: "wrong", Database: "dumpcat_e2e_crm", SQL: "SELECT COUNT(*) = 99 FROM customers"})
	drill.Databases = []string{"dumpcat_e2e_crm"}
	st.UpdateDrill(drill)
	x = drillOnce()
	if x.Status != store.DrillFailed || len(x.Databases) != 1 || !hasCheck(x, "sql", store.CheckFailed) {
		t.Fatalf("false assertion: %s %+v", x.Status, x.Checks)
	}

	// a temporary name that already exists is never touched
	next := x.ID + 1
	squat := fmt.Sprintf("dumpcat_e2e_crm__drill_%d", next)
	exec("CREATE DATABASE " + squat)
	exec("CREATE TABLE " + squat + ".keep (id INT)")
	y, err := r.StartDrill(drill.ID, 0, "manual", "e2e")
	if err != nil || y.ID != next {
		t.Fatalf("start: %v (id %d, want %d)", err, y.ID, next)
	}
	waitFor(t, func() bool { z, _ := st.GetDrillRun(y.ID); return z.Status != store.StatusRunning })
	y, _ = st.GetDrillRun(y.ID)
	if y.Status != store.DrillFailed || !strings.Contains(y.Error, "already exists") || !schemas()[squat] {
		t.Fatalf("collision: %s %q, kept=%v", y.Status, y.Error, schemas()[squat])
	}
}

func hasCheck(x *store.DrillRun, kind, status string) bool {
	for _, c := range x.Checks {
		if c.Kind == kind && c.Status == status {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	for i := 0; i < 1800; i++ {
		if done() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func readLog(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
