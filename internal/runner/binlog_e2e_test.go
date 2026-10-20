package runner

import (
	"context"
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

// TestBinlogPITRE2E archives the binary logs of a real server, backs up a
// database with mysqldump, restarts the archiver and recovers the database
// under a new name to a moment between two transactions, then to the
// latest state (with the binlog still being received). It needs a
// disposable MySQL/MariaDB server with log_bin (databases dumpcat_e2e_* are
// created and dropped; FLUSH BINARY LOGS is run) and its client tools:
//
//	DUMPCAT_E2E_MYSQL='root:secret@127.0.0.1:3312' go test ./internal/runner -run BinlogPITRE2E -v
func TestBinlogPITRE2E(t *testing.T) {
	dsn := os.Getenv("DUMPCAT_E2E_MYSQL")
	if dsn == "" {
		t.Skip("set DUMPCAT_E2E_MYSQL=user:password@host:port to run against a disposable server with log_bin")
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
	defer func() { r.Shutdown(time.Minute) }()

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
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	drop := func() {
		for _, n := range []string{"dumpcat_e2e_pitr", "dumpcat_e2e_pitr_t", "dumpcat_e2e_pitr_l"} {
			db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+n)
		}
	}
	drop()
	defer drop()
	exec("CREATE DATABASE dumpcat_e2e_pitr")
	exec("CREATE TABLE dumpcat_e2e_pitr.t (id INT PRIMARY KEY, note VARCHAR(50))")
	exec("INSERT INTO dumpcat_e2e_pitr.t VALUES (1, 'before the backup')")

	job := &store.Job{UUID: NewUUID(), Name: "binlog e2e", ServerID: src.ID, Method: store.MethodMysqldump, DBMode: "include",
		Databases: []string{"dumpcat_e2e_pitr"}, ExcludeTables: []string{}, Compression: "zstd", Threads: 1, Enabled: true,
		Encrypt: true, Passphrase: "e2e passphrase",
		Options: store.JobOptions{RecordBinlog: true, LogArchive: true, VerifyMode: store.VerifyOff},
		Targets: []store.JobTarget{{StorageID: disk.ID}}}
	if err := st.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	r.SyncArchivers()
	streaming := func() bool {
		s, ok := r.ArchiverStatus(job.ID)
		return ok && s.State == "streaming" && s.CheckedAt != nil
	}
	waitFor(t, streaming)

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
	time.Sleep(1100 * time.Millisecond)
	exec("INSERT INTO dumpcat_e2e_pitr.t VALUES (2, 'first change')")
	time.Sleep(1100 * time.Millisecond)
	between := time.Now()
	time.Sleep(1100 * time.Millisecond)
	exec("INSERT INTO dumpcat_e2e_pitr.t VALUES (3, 'second change')")

	// close the binlog, wait for its upload, then restart the stream
	archived := func() int { n := st.LogStats(job.UUID).Files; return n }
	before := archived()
	if err := r.RotateLog(job.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return archived() > before })
	restarted := time.Now()
	if err := r.RestartArchiver(job.ID); err != nil {
		t.Fatal(err)
	}
	// level with the server, as checked by the new session after at
	checkedAfter := func(at time.Time) func() bool {
		return func() bool {
			s, _ := r.ArchiverStatus(job.ID)
			return s.State == "streaming" && s.Since != nil && s.Since.After(restarted) && s.CheckedAt != nil && s.CheckedAt.After(at) && s.LagBytes == 0
		}
	}
	waitFor(t, checkedAfter(restarted))
	exec("INSERT INTO dumpcat_e2e_pitr.t VALUES (4, 'after the restart')")
	waitFor(t, checkedAfter(time.Now()))

	restore := func(rename string, target PITRTarget) {
		t.Helper()
		plan, err := r.PlanPITR(0, run.ID, target)
		if err != nil || !plan.OK || !plan.RunOK {
			t.Fatalf("plan %+v: %v %+v", target, err, plan)
		}
		rs, err := r.StartRestore(run.ID, RestoreRequest{TargetServerID: src.ID, Databases: []string{"dumpcat_e2e_pitr"},
			Rename: map[string]string{"dumpcat_e2e_pitr": rename}, Threads: 1, PITR: &target}, 0, "e2e")
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { x, _ := st.GetRestore(rs.ID); return x.Status != store.StatusRunning })
		if x, _ := st.GetRestore(rs.ID); x.Status != store.StatusSuccess {
			t.Fatalf("restore to %s: %s %s\n%s", target.Type, x.Status, x.Error, readLog(r.RestoreLogPath(rs.ID)))
		}
		if os.Getenv("DUMPCAT_E2E_VERBOSE") != "" {
			t.Logf("plan %+v\n%s", plan, readLog(r.RestoreLogPath(rs.ID)))
		}
	}
	restore("dumpcat_e2e_pitr_t", PITRTarget{Type: "time", Time: &between})
	if n := count("SELECT COUNT(*) FROM dumpcat_e2e_pitr_t.t"); n != 2 {
		t.Fatalf("recovered to the time between the changes: %d rows, want 2", n)
	}
	restore("dumpcat_e2e_pitr_l", PITRTarget{Type: "latest"})
	if n := count("SELECT COUNT(*) FROM dumpcat_e2e_pitr_l.t"); n != 4 {
		t.Fatalf("latest: %d rows, want 4", n)
	}
	// switching archiving off stops the archiver
	job.Options.LogArchive = false
	if err := st.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	r.SyncArchivers()
	if _, ok := r.ArchiverStatus(job.ID); ok {
		t.Fatal("the archiver still runs")
	}
}
