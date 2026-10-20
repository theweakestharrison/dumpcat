package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/pgsql"
	"dumpcat/internal/secret"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

// TestWALPITRE2E archives the WAL of a real PostgreSQL server through a
// replication slot, takes a pg_basebackup, restarts the archiver (its
// session, then DumpCat itself) and recovers the cluster to a time between
// two transactions, to a restore point, to an LSN and to the latest state
// (with the segment still being received). Every recovered directory is
// started and its rows counted. It needs a disposable server whose account
// may replicate (database dumpcat_e2e_wal and slots dumpcat_* are created
// and dropped, pg_switch_wal() is run) and the PostgreSQL server binaries of
// its version (postgres, pg_ctl) besides the client tools. Run as root on
// Linux, it also checks the recovery as the OS user postgres:
//
//	DUMPCAT_TOOLS_DIR=<pg bin> DUMPCAT_E2E_PG='postgres:secret@127.0.0.1:5440' go test ./internal/runner -run WALPITRE2E -v
func TestWALPITRE2E(t *testing.T) {
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
	defer func() { r.Shutdown(time.Minute) }()

	cred, addr, _ := strings.Cut(dsn, "@")
	user, pass, _ := strings.Cut(cred, ":")
	host, portS, _ := strings.Cut(addr, ":")
	port, _ := strconv.Atoi(portS)
	src := &store.Server{Engine: store.EnginePostgres, Name: "source", Host: host, Port: port, Username: user, Password: pass, Flavor: "auto"}
	if err := st.CreateServer(src); err != nil {
		t.Fatal(err)
	}
	disk := &store.Storage{Name: "disk", Type: "local", Config: map[string]string{"path": filepath.Join(dir, "backups")}}
	if err := st.CreateStorage(disk); err != nil {
		t.Fatal(err)
	}
	query := func(s *store.Server, db, q string, args ...any) string {
		t.Helper()
		c, err := pgsql.Connect(ctx, s, db)
		if err != nil {
			t.Fatalf("connect %s: %v", db, err)
		}
		defer c.Close(ctx)
		var v *string
		if err := c.QueryRow(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if v == nil {
			return ""
		}
		return *v
	}
	const db = "dumpcat_e2e_wal"
	cleanup := func() {
		c, err := pgsql.Connect(ctx, src, "")
		if err != nil {
			return
		}
		defer c.Close(ctx)
		c.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		c.Exec(ctx, "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name LIKE 'dumpcat_job%' AND NOT active")
	}
	cleanup()
	defer cleanup()
	func() {
		c, err := pgsql.Connect(ctx, src, "")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatal(err)
		}
	}()
	sql := func(q string) {
		t.Helper()
		c, err := pgsql.Connect(ctx, src, db)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sql("CREATE TABLE t (id int PRIMARY KEY, note text)")
	sql("INSERT INTO t VALUES (1, 'before the backup')")

	job := &store.Job{UUID: NewUUID(), Name: "wal e2e", ServerID: src.ID, Method: store.MethodPgBase, DBMode: "all",
		Databases: []string{}, ExcludeTables: []string{}, Compression: "zstd", Threads: 1, Enabled: true, Encrypt: true, Passphrase: "e2e passphrase",
		Options: store.JobOptions{LogArchive: true, VerifyMode: store.VerifyOne}, Targets: []store.JobTarget{{StorageID: disk.ID}}}
	if err := st.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	slot := pgsql.SlotName(job.ID, job.UUID)
	r.SyncArchivers()
	since := time.Now()
	checkedAfter := func(r *Runner, at time.Time) func() bool {
		return func() bool {
			s, ok := r.ArchiverStatus(job.ID)
			return ok && s.State == "streaming" && s.Since != nil && s.Since.After(since) && s.CheckedAt != nil && s.CheckedAt.After(at) && s.LagBytes == 0
		}
	}
	waitFor(t, checkedAfter(r, since))
	if query(src, "", "SELECT count(*)::text FROM pg_replication_slots WHERE slot_name = $1", slot) != "1" {
		t.Fatalf("slot %s was not created", slot)
	}

	run, err := r.StartBackup(job.ID, store.TypeFull, "manual", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		x, _ := st.GetRun(run.ID)
		return x.Status != store.StatusQueued && x.Status != store.StatusRunning
	})
	if run, _ = st.GetRun(run.ID); run.Status != store.StatusSuccess || run.VerifyStatus == "failed" {
		t.Fatalf("backup: %s %s (verify %s)\n%s", run.Status, run.Error, run.VerifyStatus, readLog(r.RunLogPath(run.ID)))
	}
	if run.Meta["end_lsn"] == nil || run.Meta["system_id"] == nil {
		t.Fatalf("backup meta: %v", run.Meta)
	}

	time.Sleep(1100 * time.Millisecond)
	sql("INSERT INTO t VALUES (2, 'first change')")
	time.Sleep(1100 * time.Millisecond)
	between := time.Now()
	time.Sleep(1100 * time.Millisecond)
	sql("INSERT INTO t VALUES (3, 'second change')")
	query(src, "", "SELECT pg_create_restore_point('dumpcat_e2e_rp')::text")
	sql("INSERT INTO t VALUES (4, 'after the restore point')")
	lsn := query(src, "", "SELECT pg_current_wal_lsn()::text")
	sql("INSERT INTO t VALUES (5, 'after the LSN')")

	// close the segment and wait for its upload
	archived := func() int { return st.LogStats(job.UUID).Files }
	before := archived()
	if err := r.RotateLog(job.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return archived() > before })

	// restart the stream, then DumpCat itself (the slot keeps the WAL meanwhile)
	since = time.Now()
	if err := r.RestartArchiver(job.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, checkedAfter(r, since))
	r.Shutdown(time.Minute)
	sql("INSERT INTO t VALUES (6, 'while DumpCat was down')")
	r = New(st, sm, tools, nil, dir, "e2e")
	since = time.Now()
	r.SyncArchivers()
	waitFor(t, checkedAfter(r, since))
	sql("INSERT INTO t VALUES (7, 'after the restart')")
	waitFor(t, checkedAfter(r, time.Now()))

	pgctl, err := tools.PGExact("pg_ctl", 0)
	if err != nil {
		if l := tools.PGTools("pg_ctl"); len(l) > 0 {
			pgctl = l[0]
		} else {
			t.Fatal(err)
		}
	}
	// as root (Linux) DumpCat recovers as the OS user postgres: the restores
	// go where it can enter, and the restored clusters start as it
	asRoot := runtime.GOOS != "windows" && os.Geteuid() == 0
	pgLogs := dir
	if asRoot {
		os.Chmod(filepath.Dir(dir), 0o755)
		os.Chmod(dir, 0o755)
		pgLogs = filepath.Join(dir, "pglogs")
		os.Mkdir(pgLogs, 0o777)
		os.Chmod(pgLogs, 0o777)
	}
	asPostgres := func(c *exec.Cmd) *exec.Cmd {
		if asRoot {
			if err := execx.AsUser(c, "postgres"); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	// restore restores the backup through rr (to target when not nil), starts
	// the restored cluster and counts its rows. recovered: DumpCat ran the
	// recovery (else the directory recovers when it starts).
	restore := func(rr *Runner, name string, target *PITRTarget, recovered bool) int {
		t.Helper()
		if target != nil {
			plan, err := rr.PlanPITR(0, run.ID, *target)
			if err != nil || !plan.OK || !plan.RunOK {
				t.Fatalf("plan %+v: %v %+v", target, err, plan)
			}
		}
		out := filepath.Join(dir, "restore-"+name)
		rs, err := rr.StartRestore(run.ID, RestoreRequest{TargetDir: out, PITR: target}, 0, "e2e")
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { x, _ := st.GetRestore(rs.ID); return x.Status != store.StatusRunning })
		log := readLog(rr.RestoreLogPath(rs.ID))
		if x, _ := st.GetRestore(rs.ID); x.Status != store.StatusSuccess {
			t.Fatalf("restore %s: %s %s\n%s", name, x.Status, x.Error, log)
		}
		if os.Getenv("DUMPCAT_E2E_VERBOSE") != "" {
			t.Logf("restore %s\n%s", name, log)
		}
		if strings.Contains(log, "database system is ready to accept connections") != recovered {
			t.Fatalf("%s: recovered by DumpCat should be %v:\n%s", name, recovered, log)
		}
		if asRoot && recovered && !strings.Contains(log, " as postgres: ") {
			t.Fatalf("%s: the recovery did not run as postgres:\n%s", name, log)
		}
		// start the restored cluster and count its rows
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		logf, _ := os.Create(filepath.Join(dir, name+".log"))
		defer logf.Close()
		start := asPostgres(exec.Command(pgctl.Path, "-D", out, "-o", fmt.Sprintf("-p %d -c archive_mode=off", p), "-l", filepath.Join(pgLogs, name+"-pg.log"), "-w", "start"))
		start.Stdout, start.Stderr = logf, logf
		if err := start.Run(); err != nil {
			b, _ := os.ReadFile(filepath.Join(pgLogs, name+"-pg.log"))
			t.Fatalf("starting the restored cluster: %v\n%s", err, b)
		}
		defer func() {
			stop := asPostgres(exec.Command(pgctl.Path, "-D", out, "-m", "fast", "-w", "stop"))
			stop.Stdout, stop.Stderr = logf, logf
			stop.Run()
		}()
		restored := *src
		restored.Port = p
		// a standby reads before it reaches the target: wait for the promotion
		waitFor(t, func() bool { return query(&restored, "", "SELECT pg_is_in_recovery()::text") == "false" })
		n, _ := strconv.Atoi(query(&restored, db, "SELECT count(*)::text FROM t"))
		return n
	}
	if n := restore(r, "time", &PITRTarget{Type: "time", Time: &between}, true); n != 2 {
		t.Fatalf("recovered to the time between the changes: %d rows, want 2", n)
	}
	if n := restore(r, "name", &PITRTarget{Type: "name", Name: "dumpcat_e2e_rp"}, true); n != 3 {
		t.Fatalf("recovered to the restore point: %d rows, want 3", n)
	}
	if n := restore(r, "lsn", &PITRTarget{Type: "lsn", LSN: lsn}, true); n != 4 {
		t.Fatalf("recovered to LSN %s: %d rows, want 4", lsn, n)
	}
	if n := restore(r, "latest", &PITRTarget{Type: "latest"}, true); n != 7 {
		t.Fatalf("latest: %d rows, want 7", n)
	}
	if n := restore(r, "plain", nil, true); n != 1 {
		t.Fatalf("the backup itself: %d rows, want 1", n)
	}
	// without the server binaries on the DumpCat host, the directory is
	// left to recover when it starts (restore_command from postgresql.auto.conf)
	noServer := engine.NewTools(func() map[string]string { return map[string]string{"postgres": filepath.Join(dir, "master.key")} })
	manual := New(st, sm, noServer, nil, dir, "e2e")
	n := restore(manual, "manual", &PITRTarget{Type: "time", Time: &between}, false)
	manual.Shutdown(time.Minute)
	if n != 2 {
		t.Fatalf("recovered when started: %d rows, want 2", n)
	}

	// switching archiving off stops the archiver and drops the slot
	job.Options.LogArchive = false
	if err := st.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	r.SyncArchivers()
	if _, ok := r.ArchiverStatus(job.ID); ok {
		t.Fatal("the archiver still runs")
	}
	if n := query(src, "", "SELECT count(*)::text FROM pg_replication_slots WHERE slot_name = $1", slot); n != "0" {
		t.Fatalf("slot %s was not dropped", slot)
	}
}
