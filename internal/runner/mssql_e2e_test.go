package runner

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/mssql"
	"dumpcat/internal/secret"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

// TestMSSQLE2E backs up two databases of a real SQL Server with native full
// and differential backups, restores them under new names, over themselves
// and through a drill, notices a full backup taken outside DumpCat, then
// archives the transaction log on a schedule and recovers to a time, a
// marked transaction, an LSN and the latest log backup, and finds a log
// chain broken by a log backup taken elsewhere. It needs a disposable
// server (databases dumpcat_e2e_ms* are created and dropped) whose backup
// folder the test can read and write:
//
//	DUMPCAT_E2E_MSSQL='sa:secret@127.0.0.1:1433' DUMPCAT_E2E_MSSQL_DIR=/var/opt/mssql/dumpcat-e2e go test ./internal/runner -run MSSQLE2E -v
//
// DUMPCAT_E2E_MSSQL_LOCAL is the same folder seen from the test when the two
// paths differ (a share, a bind mount). DUMPCAT_E2E_KEEP keeps the backups in
// that directory, DUMPCAT_E2E_VERBOSE logs the backup and PITR logs.
func TestMSSQLE2E(t *testing.T) {
	dsn := os.Getenv("DUMPCAT_E2E_MSSQL")
	if dsn == "" {
		t.Skip("set DUMPCAT_E2E_MSSQL=user:password@host:port to run against a disposable SQL Server")
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
	defer func() { r.Shutdown(time.Minute) }()

	at := strings.LastIndex(dsn, "@")
	user, pass, _ := strings.Cut(dsn[:at], ":")
	host, portS, _ := strings.Cut(dsn[at+1:], ":")
	port, _ := strconv.Atoi(portS)
	src := &store.Server{Engine: store.EngineMSSQL, Name: "source", Host: host, Port: port, Username: user, Password: pass, Flavor: "auto",
		BackupDir: os.Getenv("DUMPCAT_E2E_MSSQL_DIR"), BackupDirLocal: os.Getenv("DUMPCAT_E2E_MSSQL_LOCAL")}
	if err := st.CreateServer(src); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, "backups")
	if keep := os.Getenv("DUMPCAT_E2E_KEEP"); keep != "" {
		backups = keep // the backups stay there (e.g. to import their catalog elsewhere)
	}
	disk := &store.Storage{Name: "disk", Type: "local", Config: map[string]string{"path": backups}}
	if err := st.CreateStorage(disk); err != nil {
		t.Fatal(err)
	}
	pool, err := mssql.Open(ctx, src, "")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// a connection per batch: restores WITH REPLACE close the sessions of their database
	pool.SetMaxIdleConns(0)
	// q runs a batch in database db ("" = master); a SELECT returns its first value
	q := func(db, batch string) string {
		t.Helper()
		sel := strings.HasPrefix(strings.ToUpper(strings.TrimSpace(batch)), "SELECT")
		if db != "" {
			batch = "USE " + mssql.QuoteName(db) + ";\n" + batch
		}
		if sel {
			var v sql.NullString
			if err := pool.QueryRowContext(ctx, batch).Scan(&v); err != nil {
				t.Fatalf("%s: %v", batch, err)
			}
			return v.String
		}
		if _, err := pool.ExecContext(ctx, batch); err != nil {
			t.Fatalf("%s: %v", batch, err)
		}
		return ""
	}
	count := func(db, table string) int {
		t.Helper()
		n, _ := strconv.Atoi(q(db, "SELECT COUNT(*) FROM "+table))
		return n
	}
	const shop, odd = "dumpcat_e2e_ms_shop", "dumpcat_e2e_ms_o'd]d"
	cleanup := func() {
		rows, err := pool.QueryContext(ctx, "SELECT name FROM sys.databases WHERE name LIKE 'dumpcat[_]e2e[_]ms%'")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		rows.Close()
		for _, n := range names {
			pool.ExecContext(ctx, "ALTER DATABASE "+mssql.QuoteName(n)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
			if _, err := pool.ExecContext(ctx, "DROP DATABASE "+mssql.QuoteName(n)); err != nil {
				t.Logf("drop %s: %v", n, err)
			}
		}
	}
	cleanup()
	defer cleanup()
	q("", "CREATE DATABASE "+mssql.QuoteName(shop))
	q("", "ALTER DATABASE "+mssql.QuoteName(shop)+" SET RECOVERY FULL")
	q("", "CREATE DATABASE "+mssql.QuoteName(odd))
	q("", "ALTER DATABASE "+mssql.QuoteName(odd)+" SET RECOVERY SIMPLE")
	q(shop, `EXEC('CREATE SCHEMA sales')`)
	q(shop, `CREATE TABLE dbo.items (id int IDENTITY PRIMARY KEY, name nvarchar(100) NOT NULL, note nvarchar(max), price decimal(10,2));
		INSERT INTO dbo.items (name, note, price) SELECT TOP (2500) N'item ' + CAST(n AS nvarchar(10)), CASE WHEN n % 7 = 0 THEN N'Tiếng Việt ' + REPLICATE(N'x', n % 300) END, n * 1.5
		FROM (SELECT ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS n FROM sys.all_objects a CROSS JOIN sys.all_objects b) g;
		CREATE TABLE sales.orders (id bigint IDENTITY PRIMARY KEY, item_id int REFERENCES dbo.items(id), qty int);
		INSERT INTO sales.orders (item_id, qty) SELECT (id % 2500) + 1, id % 5 FROM dbo.items CROSS JOIN (VALUES (1), (2)) v(x);
		CREATE TABLE dbo.[Mixed Case] (k nvarchar(10) PRIMARY KEY, v nvarchar(50));
		INSERT INTO dbo.[Mixed Case] VALUES (N'a', N'{"x":1}'), (N'', NULL);
		CREATE TABLE dbo.empty_t (id int);
		CREATE TABLE dbo.events (id int IDENTITY, note nvarchar(50), at datetime2 DEFAULT SYSUTCDATETIME())`)
	q(shop, `EXEC('CREATE VIEW dbo.cheap AS SELECT * FROM dbo.items WHERE price < 10')`)
	q(odd, `CREATE TABLE t (id int); INSERT INTO t SELECT TOP (10) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) FROM sys.all_objects`)

	job := &store.Job{UUID: NewUUID(), Name: "e2e-mssql", ServerID: src.ID, Method: store.MethodMSSQL, DBMode: "include",
		Databases: []string{shop, odd}, Compression: "zstd", Threads: 2, Enabled: true,
		Options: store.JobOptions{VerifyOnly: true, VerifyMode: store.VerifyAll, LogRotateMinutes: 60}, Targets: []store.JobTarget{{StorageID: disk.ID}}}
	if err := st.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	backup := func(typ string) *store.Run {
		t.Helper()
		run, err := r.StartBackup(job.ID, typ, "manual", "e2e")
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool {
			x, _ := st.GetRun(run.ID)
			return x.Status != store.StatusQueued && x.Status != store.StatusRunning && (x.VerifyStatus == "ok" || x.VerifyStatus == "failed" || x.Status != store.StatusSuccess)
		})
		run, _ = st.GetRun(run.ID)
		if run.Status != store.StatusSuccess || run.VerifyStatus != "ok" {
			t.Fatalf("%s backup: %s %s, verify %s %s\n%s", typ, run.Status, run.Error, run.VerifyStatus, run.VerifyMsg, readLog(r.RunLogPath(run.ID)))
		}
		return run
	}
	full := backup(store.TypeFull)
	log := readLog(r.RunLogPath(full.ID))
	if os.Getenv("DUMPCAT_E2E_VERBOSE") != "" {
		t.Logf("full backup:\n%s", log)
	}
	for _, want := range []string{"RESTORE VERIFYONLY", "WITH CHECKSUM", "BACKUP DATABASE [dumpcat_e2e_ms_o'd]]d]"} {
		if !strings.Contains(log, want) {
			t.Errorf("the backup log has no %q", want)
		}
	}
	hdrs, _ := full.Meta["mssql"].(map[string]any)
	if len(hdrs) != 2 {
		t.Fatalf("headers of the full backup: %v", full.Meta["mssql"])
	}
	stats, _ := st.RunTables(full.ID)
	if stats[shop]["dbo.items"] != 2500 || stats[shop]["sales.orders"] != 5000 || stats[odd]["dbo.t"] != 10 || stats[shop]["dbo.empty_t"] != 0 {
		t.Fatalf("row counts of the backup: %v", stats)
	}
	// files are gone from the backup folder
	folder := src.BackupDirLocal
	if folder == "" {
		folder = src.BackupDir
	}
	leftovers := func() []string {
		if folder == "" {
			return nil
		}
		m, _ := filepath.Glob(filepath.Join(folder, "dumpcat-*"))
		return m
	}
	if l := leftovers(); len(l) > 0 {
		t.Fatalf("files left in the backup folder: %v", l)
	}

	// a differential after more rows
	q(shop, "INSERT INTO dbo.events (note) VALUES (N'after the full backup')")
	diff := backup(store.TypeDifferential)
	if diff.Type != store.TypeDifferential || diff.ParentID == nil || *diff.ParentID != full.ID || diff.BaseID == nil || *diff.BaseID != full.ID {
		t.Fatalf("differential: type %s parent %v base %v\n%s", diff.Type, diff.ParentID, diff.BaseID, readLog(r.RunLogPath(diff.ID)))
	}
	if !strings.Contains(readLog(r.RunLogPath(diff.ID)), "DIFFERENTIAL") {
		t.Fatal("no WITH DIFFERENTIAL in the log")
	}

	restore := func(run *store.Run, req RestoreRequest) *store.Restore {
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
	// the differential under new names: full + differential
	rs := restore(diff, RestoreRequest{TargetServerID: src.ID, Databases: []string{shop, odd}, Rename: map[string]string{shop: shop + "_copy", odd: odd + "_copy"}})
	if rs.Status != store.StatusSuccess {
		t.Fatalf("restore: %s %s\n%s", rs.Status, rs.Error, readLog(r.RestoreLogPath(rs.ID)))
	}
	rlog := readLog(r.RestoreLogPath(rs.ID))
	if !strings.Contains(rlog, "NORECOVERY") || !strings.Contains(rlog, "MOVE") {
		t.Errorf("the restore log has no NORECOVERY / MOVE:\n%s", rlog)
	}
	if n := count(shop+"_copy", "sales.orders"); n != 5000 {
		t.Fatalf("restored orders: %d", n)
	}
	if n := count(shop+"_copy", "dbo.events"); n != 1 {
		t.Fatalf("the differential's row: %d", n)
	}
	if n, _ := strconv.Atoi(q(odd+"_copy", "SELECT SUM(id) FROM t")); n != 55 {
		t.Fatalf("odd name: %d", n)
	}
	if l := leftovers(); len(l) > 0 {
		t.Fatalf("files left in the backup folder after the restore: %v", l)
	}
	// over the database itself: refused without Replace, nothing changed
	q(shop, "DELETE FROM sales.orders WHERE id > 100")
	rs = restore(full, RestoreRequest{TargetServerID: src.ID, Databases: []string{shop}})
	if rs.Status != store.StatusFailed || !strings.Contains(rs.Error, "exists") || count(shop, "sales.orders") != 100 {
		t.Fatalf("a restore over an existing database without Replace: %s %s", rs.Status, rs.Error)
	}
	rs = restore(full, RestoreRequest{TargetServerID: src.ID, Databases: []string{shop}, DropExisting: true})
	if rs.Status != store.StatusSuccess || count(shop, "sales.orders") != 5000 || count(shop, "dbo.events") != 0 {
		t.Fatalf("replace: %s %s\n%s", rs.Status, rs.Error, readLog(r.RestoreLogPath(rs.ID)))
	}
	if _, err := r.StartRestore(full.ID, RestoreRequest{TargetServerID: src.ID, Databases: []string{shop}, Rename: map[string]string{shop: "msdb"}}, 0, "e2e"); err == nil {
		t.Fatal("restoring over a system database must be refused")
	}

	// a restore drill of the differential: full + differential under temporary names, DBCC CHECKDB
	drill := &store.Drill{JobID: job.ID, Name: "e2e drill", TargetServerID: src.ID, Enabled: true, Databases: []string{},
		Assertions: []store.DrillAssertion{{Name: "items", Database: shop, SQL: "SELECT CASE WHEN COUNT(*) = 2500 THEN 1 ELSE 0 END FROM dbo.items"}}}
	if err := st.CreateDrill(drill); err != nil {
		t.Fatal(err)
	}
	x, err := r.StartDrill(drill.ID, diff.ID, "manual", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { y, _ := st.GetDrillRun(x.ID); return y.Status != store.StatusRunning })
	x, _ = st.GetDrillRun(x.ID)
	// the differential's row arrived after the counts of the full backup: the counts are the differential's own
	if x.Status != store.DrillPassed || !hasCheck(x, "integrity", store.CheckOK) {
		t.Fatalf("drill: %s %s %+v\n%s", x.Status, x.Error, x.Checks, readLog(r.DrillLogPath(x.ID)))
	}
	if n := q("", "SELECT COUNT(*) FROM sys.databases WHERE name LIKE '%[_][_]drill[_]%'"); n != "0" {
		t.Fatalf("the drill left %s copies", n)
	}

	// a full backup taken by another tool moves the differential base: DumpCat takes a full one
	q("", "BACKUP DATABASE "+mssql.QuoteName(shop)+" TO DISK = N'NUL'")
	moved, err := r.StartBackup(job.ID, store.TypeDifferential, "manual", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		y, _ := st.GetRun(moved.ID)
		return y.Status != store.StatusQueued && y.Status != store.StatusRunning
	})
	moved, _ = st.GetRun(moved.ID)
	if moved.Status != store.StatusSuccess || moved.Type != store.TypeFull || !strings.Contains(readLog(r.RunLogPath(moved.ID)), "outside DumpCat") {
		t.Fatalf("differential after a foreign full backup: %s %s %s\n%s", moved.Status, moved.Type, moved.Error, readLog(r.RunLogPath(moved.ID)))
	}

	// ---- log backups and point-in-time recovery ----
	job.Options.LogArchive = true
	if err := st.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	r.SyncArchivers()
	archived := func() int {
		files, _ := st.ListLogFiles(job.UUID)
		return len(files)
	}
	waitFor(t, func() bool { return archived() >= 1 }) // the first round runs at once
	rotate := func() {
		t.Helper()
		before := archived()
		if err := r.RotateLog(job.ID); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return archived() > before })
	}
	pause := func() { time.Sleep(1500 * time.Millisecond) }
	q(shop, "INSERT INTO dbo.events (note) VALUES (N'a')")
	pause()
	between := time.Now()
	pause()
	q(shop, "INSERT INTO dbo.events (note) VALUES (N'b')")
	q(shop, "BEGIN TRANSACTION dumpcat_e2e_mark WITH MARK N'before the c row'; INSERT INTO dbo.events (note) VALUES (N'c'); COMMIT TRANSACTION dumpcat_e2e_mark")
	q(shop, "INSERT INTO dbo.events (note) VALUES (N'd')")
	// the LSN of the newest log record, as fn_dblog writes it (hex): d is committed before it
	lsnD := q(shop, "SELECT MAX([Current LSN]) FROM fn_dblog(NULL, NULL)")
	rotate()
	q(shop, "INSERT INTO dbo.events (note) VALUES (N'e')")
	rotate()
	files, _ := st.ListLogFiles(job.UUID)
	var lsnAfterD string
	for _, f := range files {
		if f.Meta["database"] == shop {
			if m, ok := f.Meta["marks"].([]any); ok && len(m) > 0 {
				lsnAfterD, _ = f.Meta["last_lsn"].(string) // the log backup holding the mark ends after d
			}
		}
	}
	if lsnAfterD == "" {
		t.Fatalf("no log backup recorded the mark: %v", files)
	}
	if lsnD == "" {
		lsnD = lsnAfterD
	}
	pitr := func(name string, target PITRTarget) int {
		t.Helper()
		plan, err := r.PlanPITR(0, moved.ID, target)
		if err != nil || !plan.OK {
			t.Fatalf("plan %s: %+v %v", name, plan, err)
		}
		as := shop + "_pitr_" + name
		rs := restore(moved, RestoreRequest{TargetServerID: src.ID, Databases: []string{shop}, Rename: map[string]string{shop: as}, PITR: &target})
		if rs.Status != store.StatusSuccess {
			t.Fatalf("PITR %s: %s %s\n%s", name, rs.Status, rs.Error, readLog(r.RestoreLogPath(rs.ID)))
		}
		if os.Getenv("DUMPCAT_E2E_VERBOSE") != "" {
			t.Logf("PITR %s:\n%s", name, readLog(r.RestoreLogPath(rs.ID)))
		}
		return count(as, "dbo.events")
	}
	if n := pitr("time", PITRTarget{Type: "time", Time: &between}); n != 1 {
		t.Errorf("PITR to a time: %d rows, want 1 (a)", n)
	}
	if n := pitr("mark", PITRTarget{Type: "mark", Name: "dumpcat_e2e_mark"}); n != 3 {
		t.Errorf("PITR to the mark: %d rows, want 3 (a, b, c)", n)
	}
	if n := pitr("lsn", PITRTarget{Type: "lsn", LSN: lsnD}); n != 4 {
		t.Errorf("PITR to an LSN: %d rows, want 4 (a…d)", n)
	}
	if n := pitr("latest", PITRTarget{Type: "latest"}); n != 5 {
		t.Errorf("PITR to the latest log backup: %d rows, want 5", n)
	}
	// the SIMPLE database cannot follow, and nothing is restored
	simple := restore(moved, RestoreRequest{TargetServerID: src.ID, Databases: []string{odd}, Rename: map[string]string{odd: odd + "_p"}, PITR: &PITRTarget{Type: "latest"}})
	if simple.Status != store.StatusFailed || !strings.Contains(simple.Error, "SIMPLE") {
		t.Errorf("PITR of a SIMPLE database: %s %s", simple.Status, simple.Error)
	}
	if n := q("", "SELECT COUNT(*) FROM sys.databases WHERE name = N'"+strings.ReplaceAll(odd+"_p", "'", "''")+"'"); n != "0" {
		t.Error("the SIMPLE database was restored")
	}
	// a log backup taken elsewhere breaks the chain: later points cannot be reached
	q(shop, "INSERT INTO dbo.events (note) VALUES (N'f')")
	q("", "BACKUP LOG "+mssql.QuoteName(shop)+" TO DISK = N'NUL'")
	q(shop, "INSERT INTO dbo.events (note) VALUES (N'g')")
	rotate()
	later := time.Now()
	if v, _ := r.PlanPITR(0, moved.ID, PITRTarget{Type: "time", Time: &later}); v == nil || v.OK || !strings.Contains(v.Error, "breaks") {
		t.Errorf("a broken chain must stop the plan: %+v", v)
	}
	if v, _ := r.PlanPITR(0, moved.ID, PITRTarget{Type: "latest"}); v == nil || !v.OK || !strings.Contains(fmt.Sprint(v.Warnings), "break") {
		t.Errorf("latest across a broken chain: %+v", v)
	}
	job.Options.LogArchive = false
	st.UpdateJob(job)
	r.SyncArchivers()
	if _, ok := r.ArchiverStatus(job.ID); ok {
		t.Error("the archiver still runs")
	}
}
