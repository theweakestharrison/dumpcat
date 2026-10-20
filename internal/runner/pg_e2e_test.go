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

	"dumpcat/internal/engine"
	"dumpcat/internal/pgsql"
	"dumpcat/internal/secret"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

// TestPostgresE2E backs up two databases of a real PostgreSQL server with
// pg_dump, restores them under new names and in place, and drills the
// backup. It needs a disposable server (databases dumpcat_e2e_pg* and roles
// dumpcat_e2e_* are created and dropped) and its client tools (found like
// DumpCat does, or in DUMPCAT_TOOLS_DIR). DUMPCAT_E2E_PG2 is an optional
// second server: the backup is restored there with its roles.
//
//	DUMPCAT_E2E_PG='postgres:secret@127.0.0.1:5440' go test ./internal/runner -run PostgresE2E -v
func TestPostgresE2E(t *testing.T) {
	dsn := os.Getenv("DUMPCAT_E2E_PG")
	if dsn == "" {
		t.Skip("set DUMPCAT_E2E_PG=user:password@host:port to run against a disposable PostgreSQL server")
	}
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

	server := func(name, dsn string) *store.Server {
		cred, addr, _ := strings.Cut(dsn, "@")
		user, pass, _ := strings.Cut(cred, ":")
		host, portS, _ := strings.Cut(addr, ":")
		port, _ := strconv.Atoi(portS)
		s := &store.Server{Engine: store.EnginePostgres, Name: name, Host: host, Port: port, Username: user, Password: pass, Flavor: "auto"}
		if err := st.CreateServer(s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	src := server("source", dsn)
	disk := &store.Storage{Name: "disk", Type: "local", Config: map[string]string{"path": filepath.Join(dir, "backups")}}
	if err := st.CreateStorage(disk); err != nil {
		t.Fatal(err)
	}
	type sqlRunner func(db, q string) int64
	runner := func(s *store.Server) sqlRunner {
		return func(db, q string) int64 {
			t.Helper()
			c, err := pgsql.Connect(ctx, s, db)
			if err != nil {
				t.Fatalf("connect %s: %v", db, err)
			}
			defer c.Close(ctx)
			if strings.HasPrefix(strings.ToUpper(q), "SELECT") {
				var n int64
				if err := c.QueryRow(ctx, q).Scan(&n); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return n
			}
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			return 0
		}
	}
	sq := runner(src)
	const shop, odd = "dumpcat_e2e_pg_shop", "dumpcat_e2e_pg_a=b 'odd'"
	databases := func(s *store.Server) map[string]bool {
		c, err := pgsql.Connect(ctx, s, "")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		rows, err := c.Query(ctx, "SELECT datname FROM pg_database WHERE datname LIKE 'dumpcat_e2e_pg%'")
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
	cleanup := func(s *store.Server) {
		q := runner(s)
		for n := range databases(s) {
			q("", "DROP DATABASE IF EXISTS "+pgsql.QuoteIdent(n)+" WITH (FORCE)")
		}
		q("", "DROP ROLE IF EXISTS dumpcat_e2e_owner")
		q("", "DROP ROLE IF EXISTS dumpcat_e2e_reader")
	}
	cleanup(src)
	defer cleanup(src)
	sq("", "CREATE ROLE dumpcat_e2e_owner LOGIN PASSWORD 'owner-pw'")
	sq("", "CREATE ROLE dumpcat_e2e_reader NOLOGIN")
	sq("", "GRANT dumpcat_e2e_reader TO dumpcat_e2e_owner")
	sq("", "CREATE DATABASE "+shop+" OWNER dumpcat_e2e_owner")
	sq("", "CREATE DATABASE "+pgsql.QuoteIdent(odd))
	for _, q := range []string{
		"SET ROLE dumpcat_e2e_owner; CREATE SCHEMA sales",
		`CREATE TABLE public.items (id serial PRIMARY KEY, name text NOT NULL, note text, price numeric(10,2))`,
		`INSERT INTO public.items (name, note, price) SELECT 'item ' || g, CASE WHEN g % 7 = 0 THEN E'multi\nline\ttab \\ back' END, g * 1.5 FROM generate_series(1, 2500) g`,
		`CREATE TABLE sales.orders (id bigserial PRIMARY KEY, item_id int REFERENCES public.items(id), qty int)`,
		`INSERT INTO sales.orders (item_id, qty) SELECT (g % 2500) + 1, g % 5 FROM generate_series(1, 4000) g`,
		`CREATE TABLE public."Mixed Case" (k text PRIMARY KEY, v jsonb)`,
		`INSERT INTO public."Mixed Case" VALUES ('a', '{"x":1}'), ('', '{"y":[1,2]}')`,
		`CREATE TABLE public.one_col (v text)`,
		`INSERT INTO public.one_col VALUES (''), (''), ('\.'), (NULL)`,
		`CREATE TABLE public.empty_t (id int)`,
		`CREATE VIEW public.cheap AS SELECT * FROM public.items WHERE price < 10`,
		`CREATE TABLE public.events (id int, at date) PARTITION BY RANGE (at)`,
		`CREATE TABLE public.events_2025 PARTITION OF public.events FOR VALUES FROM ('2025-01-01') TO ('2026-01-01')`,
		`CREATE TABLE public.events_2026 PARTITION OF public.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		`INSERT INTO public.events SELECT g, DATE '2025-06-01' + (g % 400) FROM generate_series(1, 1000) g`,
		`CREATE TABLE public.parent (id int)`,
		`CREATE TABLE public.child () INHERITS (public.parent)`,
		`INSERT INTO public.parent VALUES (1), (2)`,
		`INSERT INTO public.child VALUES (3)`,
		`CREATE MATERIALIZED VIEW public.mv AS SELECT count(*) AS n FROM public.items`,
		`CREATE TABLE public.skipped (id int)`,
		`INSERT INTO public.skipped VALUES (1)`,
		`GRANT SELECT ON public.items TO dumpcat_e2e_reader`,
	} {
		if !strings.HasPrefix(q, "SET ROLE") {
			q = "SET ROLE dumpcat_e2e_owner; " + q
		}
		sq(shop, q)
	}
	sq(odd, "CREATE TABLE t (id int)")
	sq(odd, "INSERT INTO t SELECT generate_series(1, 10)")

	job := &store.Job{UUID: NewUUID(), Name: "e2e-pg", ServerID: src.ID, Method: store.MethodPgDump, DBMode: "include",
		Databases: []string{shop, odd}, ExcludeTables: []string{shop + ".skipped"}, Compression: "zstd", Threads: 2, Enabled: true,
		Options: store.JobOptions{IncludeUsers: true, VerifyMode: store.VerifyAll}, Targets: []store.JobTarget{{StorageID: disk.ID}}}
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
	// verification follows the backup in the same task
	waitFor(t, func() bool { x, _ := st.GetRun(run.ID); return x.VerifyStatus == "ok" || x.VerifyStatus == "failed" })
	if run, _ = st.GetRun(run.ID); run.VerifyStatus != "ok" {
		t.Fatalf("verify: %s %s", run.VerifyStatus, run.VerifyMsg)
	}
	stats, _ := st.RunTables(run.ID)
	want := store.TableStats{
		shop: {"public.items": 2500, "sales.orders": 4000, "public.Mixed Case": 2, "public.one_col": 4, "public.empty_t": 0,
			"public.events_2025": sq(shop, "SELECT count(*) FROM ONLY public.events_2025"),
			"public.events_2026": sq(shop, "SELECT count(*) FROM ONLY public.events_2026"), "public.parent": 2, "public.child": 1},
		odd: {"public.t": 10},
	}
	if fmt.Sprint(stats) != fmt.Sprint(want) {
		t.Fatalf("row counts of the backup:\n got %v\nwant %v", stats, want)
	}
	var files []string
	for _, f := range run.Files {
		files = append(files, f.Name)
	}
	if !strings.Contains(strings.Join(files, " "), pgsql.GlobalsFile) {
		t.Fatalf("no roles file: %v", files)
	}

	restore := func(req RestoreRequest) *store.Restore {
		t.Helper()
		rs, err := r.StartRestore(run.ID, req, 0, "e2e")
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool {
			x, _ := st.GetRestore(rs.ID)
			return x.Status == store.StatusSuccess || x.Status == store.StatusFailed || x.Status == store.StatusCanceled
		})
		rs, _ = st.GetRestore(rs.ID)
		return rs
	}
	// under new names, objects owned by the restore account
	rs := restore(RestoreRequest{TargetServerID: src.ID, Databases: []string{shop, odd}, NoOwner: true, Threads: 2,
		Rename: map[string]string{shop: shop + "_copy", odd: odd + "_copy"}})
	if rs.Status != store.StatusSuccess {
		t.Fatalf("restore: %s %s\n%s", rs.Status, rs.Error, readLog(r.RestoreLogPath(rs.ID)))
	}
	if n := sq(shop+"_copy", `SELECT count(*) FROM sales.orders`); n != 4000 {
		t.Fatalf("restored orders: %d", n)
	}
	if n := sq(shop+"_copy", `SELECT count(*) FROM pg_tables WHERE tableowner = 'dumpcat_e2e_owner'`); n != 0 {
		t.Fatalf("--no-owner: %d tables still owned by the original role", n)
	}
	if n := sq(odd+"_copy", `SELECT sum(id) FROM t`); n != 55 {
		t.Fatalf("odd name: %d", n)
	}
	if n := sq(shop+"_copy", `SELECT count(*) FROM pg_tables WHERE tablename = 'skipped'`); n != 0 {
		t.Fatal("the excluded table was restored")
	}

	// in place: the backup's objects are replaced, others stay
	sq(shop, "DELETE FROM sales.orders WHERE id > 100")
	sq(shop, "CREATE TABLE public.kept (id int)")
	rs = restore(RestoreRequest{TargetServerID: src.ID, Databases: []string{shop}, RestoreUsers: true})
	if rs.Status != store.StatusSuccess {
		t.Fatalf("restore in place: %s %s\n%s", rs.Status, rs.Error, readLog(r.RestoreLogPath(rs.ID)))
	}
	if n := sq(shop, `SELECT count(*) FROM sales.orders`); n != 4000 {
		t.Fatalf("orders after the in-place restore: %d", n)
	}
	if n := sq(shop, `SELECT count(*) FROM pg_tables WHERE tablename = 'kept'`); n != 1 {
		t.Fatal("a table that is not in the backup must stay")
	}
	if n := sq(shop, `SELECT count(*) FROM pg_tables WHERE tableowner = 'dumpcat_e2e_owner' AND schemaname = 'public'`); n == 0 {
		t.Fatal("owners must be kept without --no-owner")
	}
	// drop first
	rs = restore(RestoreRequest{TargetServerID: src.ID, Databases: []string{shop}, DropExisting: true})
	if rs.Status != store.StatusSuccess || sq(shop, `SELECT count(*) FROM pg_tables WHERE tablename = 'kept'`) != 0 {
		t.Fatalf("drop and restore: %s %s\n%s", rs.Status, rs.Error, readLog(r.RestoreLogPath(rs.ID)))
	}
	// names that could become options or connection settings
	if _, err := r.StartRestore(run.ID, RestoreRequest{TargetServerID: src.ID, Rename: map[string]string{shop: "-x"}}, 0, "e2e"); err == nil {
		t.Fatal("a name starting with - must be refused")
	}

	// restore drills
	drill := &store.Drill{JobID: job.ID, Name: "e2e drill", TargetServerID: src.ID, Enabled: true, Databases: []string{},
		Assertions: []store.DrillAssertion{
			{Name: "items", Database: shop, SQL: "SELECT count(*) = 2500 FROM public.items"},
			{Name: "boolean", Database: shop, SQL: "SELECT EXISTS (SELECT 1 FROM sales.orders WHERE qty = 4)"},
			{Name: "view", Database: shop, SQL: "SELECT count(*) FROM public.cheap"},
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
		for n := range databases(src) {
			if strings.Contains(n, "__drill_") {
				t.Fatalf("drill #%d left %s behind\n%s", x.ID, n, readLog(r.DrillLogPath(x.ID)))
			}
		}
		return x
	}
	x := drillOnce()
	if x.Status != store.DrillPassed {
		t.Fatalf("drill: %s %s\n%s", x.Status, x.Error, readLog(r.DrillLogPath(x.ID)))
	}
	if len(x.Databases) != 2 || x.Databases[0].Rows+x.Databases[1].Rows != 7519 {
		t.Fatalf("drill result: %+v", x.Databases)
	}
	drill.Options.RowCounts = store.DrillRowsEstimate
	st.UpdateDrill(drill)
	if x = drillOnce(); x.Status != store.DrillPassed || !strings.Contains(fmt.Sprint(x.Checks), "estimated") {
		t.Fatalf("estimated drill: %s %s %+v\n%s", x.Status, x.Error, x.Checks, readLog(r.DrillLogPath(x.ID)))
	}
	drill.Options.RowCounts = ""
	st.UpdateDrill(drill)
	bad := store.TableStats{shop: {"public.items": 2501, "sales.orders": 4000, "public.gone": 1}, odd: {"public.t": 10}}
	st.SetRunTables(run.ID, bad)
	if x = drillOnce(); x.Status != store.DrillFailed || !hasCheck(x, "rows", store.CheckFailed) || !hasCheck(x, "tables", store.CheckFailed) {
		t.Fatalf("tampered counts must fail: %s %+v", x.Status, x.Checks)
	}
	st.SetRunTables(run.ID, stats)
	drill.Assertions = append(drill.Assertions, store.DrillAssertion{Name: "false", Database: shop, SQL: "SELECT 1 = 2"})
	st.UpdateDrill(drill)
	if x = drillOnce(); x.Status != store.DrillFailed || !hasCheck(x, "sql", store.CheckFailed) {
		t.Fatalf("false assertion: %s %+v", x.Status, x.Checks)
	}

	// another server: the roles come with the backup
	if dsn2 := os.Getenv("DUMPCAT_E2E_PG2"); dsn2 != "" {
		dst := server("target", dsn2)
		cleanup(dst)
		defer cleanup(dst)
		rs = restore(RestoreRequest{TargetServerID: dst.ID, Databases: []string{shop}, RestoreUsers: true})
		if rs.Status != store.StatusSuccess {
			t.Fatalf("restore to another server: %s %s\n%s", rs.Status, rs.Error, readLog(r.RestoreLogPath(rs.ID)))
		}
		dq := runner(dst)
		if n := dq(shop, `SELECT count(*) FROM pg_tables WHERE tableowner = 'dumpcat_e2e_owner'`); n == 0 {
			t.Fatal("the roles must have been created, and own the tables again")
		}
		if n := dq("", `SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid WHERE r.rolname = 'dumpcat_e2e_reader'`); n != 1 {
			t.Fatalf("role membership: %d", n)
		}
	}
}
