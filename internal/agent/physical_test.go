package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"dumpcat/internal/engine"
)

// The tests run their own binary as the backup tool: with DUMPCAT_FAKE_TOOL
// set (only the tool processes see it: TestMain has run by then) it behaves
// like mariabackup.
func TestMain(m *testing.M) {
	if mode := os.Getenv("DUMPCAT_FAKE_TOOL"); mode != "" {
		os.Exit(fakeTool(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeStream(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>9)
	}
	return b
}

func fakeTool(mode string, args []string) int {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("mariabackup based on MariaDB server 11.4.13-MariaDB Linux (x86_64)")
		return 0
	}
	if len(args) == 0 || !strings.HasPrefix(args[0], "--defaults-extra-file=") {
		fmt.Fprintln(os.Stderr, "--defaults-extra-file must come first")
		return 2
	}
	cnf := strings.TrimPrefix(args[0], "--defaults-extra-file=")
	var lsnDir string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--extra-lsndir="); ok {
			lsnDir = v
		}
	}
	if b, err := os.ReadFile(cnf); err != nil || !bytes.Contains(b, []byte("password=s3cret")) || !bytes.Contains(b, []byte("[mariabackup]")) {
		fmt.Fprintf(os.Stderr, "no credentials in %s: %v\n", cnf, err)
		return 2
	}
	if f := os.Getenv("DUMPCAT_FAKE_REPORT"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(append([]string{cnf}, args...), "\n")), 0o600)
	}
	fmt.Fprintln(os.Stderr, "[00] Connecting to MariaDB server host: 127.0.0.1")
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "[ERROR] mariabackup: cannot open ./ibdata1")
		return 1
	case "hang": // a large table: stdout keeps flowing until the tool is killed
		for i := 0; i < 3000; i++ {
			_, _ = os.Stdout.Write(make([]byte, 64<<10))
			time.Sleep(20 * time.Millisecond)
		}
		return 0
	}
	size, _ := strconv.Atoi(os.Getenv("DUMPCAT_FAKE_SIZE"))
	w := bufio.NewWriterSize(os.Stdout, 100000)
	_, _ = w.Write(fakeStream(size))
	_ = w.Flush()
	fmt.Fprintln(os.Stderr, "[01] Streaming ./ibdata1\n[01] ...done")
	_ = os.WriteFile(filepath.Join(lsnDir, "mariadb_backup_checkpoints"), []byte("backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 12345\nlast_lsn = 12345\n"), 0o600)
	fmt.Fprintln(os.Stderr, "[00] completed OK!")
	return 0
}

type physicalEnv struct {
	mgr     *Manager
	cl      *Client
	clog    *logs
	db      string // the database address (on the allow list)
	datadir string
	report  string // the fake tool writes its option file path and arguments here
	cancel  context.CancelFunc
}

func physicalSetup(t *testing.T, mode string) *physicalEnv {
	t.Helper()
	db := echoServer(t)
	mgr, cl, _, clog, done := setup(t, db)
	t.Cleanup(done)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUMPCAT_FAKE_TOOL", mode)
	report := filepath.Join(t.TempDir(), "report")
	t.Setenv("DUMPCAT_FAKE_REPORT", report)
	dd := t.TempDir()
	if err := os.Mkdir(filepath.Join(dd, "mysql"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dd, "ibdata1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cl.AllowPhysical = true
	cl.Datadir = dd
	cl.Tools = engine.NewTools(func() map[string]string { return map[string]string{"mariabackup": exe} })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cl.Run(ctx)
	waitOnline(t, mgr, 7)
	return &physicalEnv{mgr: mgr, cl: cl, clog: clog, db: db, datadir: dd, report: report, cancel: cancel}
}

func (e *physicalEnv) request() *PhysicalRequest {
	return &PhysicalRequest{PhysicalOptions: engine.PhysicalOptions{Tool: "mariabackup", Datadir: e.datadir, Parallel: 2},
		Net: "tcp", Addr: e.db, User: "backup", Password: "s3cret"}
}

// optionFileGone waits until the agent removed the option file holding the password.
func (e *physicalEnv) optionFileGone(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(e.report)
	if err != nil {
		t.Fatalf("the tool did not run: %v", err)
	}
	cnf := strings.SplitN(string(b), "\n", 2)[0]
	for i := 0; i < 250; i++ {
		if _, err := os.Stat(cnf); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the option file with the credentials is still there: %s", cnf)
}

func (e *physicalEnv) waitLog(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if e.clog.has(s) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent log has no %q: %v", s, e.clog.buf)
}

func TestPhysicalStream(t *testing.T) {
	const size = 3<<20 + 12345 // several frames, not a multiple of their size
	t.Setenv("DUMPCAT_FAKE_SIZE", strconv.Itoa(size))
	e := physicalSetup(t, "ok")
	st := e.mgr.Status(7)
	if st.Caps == nil || st.Caps.Physical == nil || !st.Caps.Physical.Allowed {
		t.Fatalf("capabilities not reported: %+v", st.Caps)
	}
	p := st.Caps.Physical
	if mb := p.Tool("mariabackup"); mb == nil || !mb.Found || !strings.Contains(mb.Version, "11.4.13") {
		t.Fatalf("tool not reported: %+v", p.Tools)
	}
	if !SameDir(p.Datadir, e.datadir) || p.DatadirError != "" || st.PhysicalProblem("mariabackup") != "" || st.DatadirProblem(e.datadir) != "" {
		t.Fatalf("datadir: %+v / %q", p, st.PhysicalProblem("mariabackup"))
	}
	if !strings.Contains(st.PhysicalProblem("xtrabackup"), "not installed") && p.Tool("xtrabackup").Found == false {
		t.Fatalf("missing xtrabackup: %q", st.PhysicalProblem("xtrabackup"))
	}

	var out, logb bytes.Buffer
	res, err := e.mgr.Physical(context.Background(), 7, e.request(), &out, &logb)
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != "" || res.Bytes != size || !strings.Contains(res.Tool, "11.4.13") {
		t.Fatalf("result: %+v", res)
	}
	if !bytes.Equal(out.Bytes(), fakeStream(size)) {
		t.Fatalf("stream differs: got %d bytes, want %d", out.Len(), size)
	}
	if !strings.Contains(logb.String(), "Streaming ./ibdata1") || !strings.Contains(logb.String(), "completed OK!") {
		t.Fatalf("tool log: %q", logb.String())
	}
	if !strings.Contains(res.Files["mariadb_backup_checkpoints"], "to_lsn = 12345") {
		t.Fatalf("checkpoints: %v", res.Files)
	}
	b, _ := os.ReadFile(e.report)
	args := string(b) + "\n"
	for _, want := range []string{"--backup", "--stream=xbstream", "--datadir=" + e.datadir, "--parallel=2"} {
		if !strings.Contains(args, "\n"+want+"\n") {
			t.Errorf("argument %s missing: %q", want, args)
		}
	}
	e.optionFileGone(t)
	e.waitLog(t, "finished")
	if n := e.mgr.Status(7).Streams; n != 0 {
		t.Fatalf("%d streams still counted", n)
	}
}

func TestPhysicalToolFails(t *testing.T) {
	e := physicalSetup(t, "fail")
	var out, logb bytes.Buffer
	res, err := e.mgr.Physical(context.Background(), 7, e.request(), &out, &logb)
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == "" || !strings.Contains(logb.String(), "cannot open ./ibdata1") {
		t.Fatalf("a failing tool must be reported: %+v, log %q", res, logb.String())
	}
	e.optionFileGone(t)
}

// cancelWriter calls fn on the first write (the backup is running).
type cancelWriter struct {
	once sync.Once
	fn   func()
	n    int
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	w.once.Do(w.fn)
	return len(p), nil
}

func TestPhysicalCancel(t *testing.T) {
	e := physicalSetup(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelWriter{fn: cancel}
	start := time.Now()
	_, err := e.mgr.Physical(ctx, 7, e.request(), w, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	// the agent kills the tool: its run ends long before the tool's minute
	e.waitLog(t, "canceled")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the tool was not stopped (took %s)", time.Since(start))
	}
	e.optionFileGone(t)
	if n := e.mgr.Status(7).Streams; n != 0 {
		t.Fatalf("%d streams still counted", n)
	}
}

func TestPhysicalStoppedByDisconnect(t *testing.T) {
	e := physicalSetup(t, "hang")
	w := &cancelWriter{}
	w.fn = func() { go e.mgr.Disconnect(7) } // key rotated or agent deleted
	_, err := e.mgr.Physical(context.Background(), 7, e.request(), w, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("want a lost connection, got %v", err)
	}
	e.waitLog(t, "canceled")
	e.optionFileGone(t)
}

func TestPhysicalRefused(t *testing.T) {
	e := physicalSetup(t, "ok")
	run := func(mod func(*PhysicalRequest)) error {
		req := e.request()
		mod(req)
		_, err := e.mgr.Physical(context.Background(), 7, req, &bytes.Buffer{}, &bytes.Buffer{})
		return err
	}
	cases := map[string]func(*PhysicalRequest){
		"not on this agent's allow list": func(r *PhysicalRequest) { r.Addr = "10.1.2.3:3306" },
		"does not pass":                  func(r *PhysicalRequest) { r.ExtraArgs = []string{"--defaults-file=x"} },
		"cannot read the datadir":        func(r *PhysicalRequest) { r.Datadir = filepath.Join(e.datadir, "missing") },
		"not installed":                  func(r *PhysicalRequest) { r.Tool = "xtrabackup" },
		"invalid LSN":                    func(r *PhysicalRequest) { r.IncrementalLSN = "1; rm -rf /" },
	}
	if p := e.mgr.Status(7).Caps.Physical.Tool("xtrabackup"); p != nil && p.Found {
		delete(cases, "not installed") // a real xtrabackup on this machine
	}
	for want, mod := range cases {
		if err := run(mod); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	if _, err := os.Stat(e.report); err == nil {
		t.Fatal("the tool ran for a refused request")
	}

	// an agent without --allow-physical: the server does not even ask...
	e.cl.AllowPhysical = false
	e.mgr.Disconnect(7)
	waitOnline(t, e.mgr, 7)
	if err := run(func(*PhysicalRequest) {}); err == nil || !strings.Contains(err.Error(), "--allow-physical") {
		t.Fatalf("not allowed: %v", err)
	}
	// ...and the agent refuses when asked anyway
	if _, err := e.mgr.request(7, Message{Type: "physical", Physical: e.request()}); err == nil || !strings.Contains(err.Error(), "--allow-physical") {
		t.Fatalf("the agent must refuse: %v", err)
	}
}

// An agent from before physical backups sends no capabilities: the server
// treats it as one that cannot run them, and it keeps forwarding connections.
func TestPhysicalOldAgent(t *testing.T) {
	mgr, cl, _, _, done := setup(t, "127.0.0.1:3306")
	defer done()
	cfg, err := cl.config(PathConnect, "")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := websocket.Message.Send(ws, `{"type":"hello","version":"1.3.1","hostname":"db1","os":"linux/amd64","allow":["127.0.0.1:3306"]}`); err != nil {
		t.Fatal(err)
	}
	waitOnline(t, mgr, 7)
	st := mgr.Status(7)
	if st.Caps != nil || !strings.Contains(st.PhysicalProblem("mariabackup"), "update it") {
		t.Fatalf("old agent: %+v %q", st.Caps, st.PhysicalProblem("mariabackup"))
	}
	req := &PhysicalRequest{PhysicalOptions: engine.PhysicalOptions{Tool: "mariabackup", Parallel: 1}, Net: "tcp", Addr: "127.0.0.1:3306"}
	if _, err := mgr.Physical(context.Background(), 7, req, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Fatalf("old agent: %v", err)
	}
}

func TestSafeExtraArg(t *testing.T) {
	for _, ok := range []string{"--galera-info", "--kill-long-queries-timeout=30", "--no-backup-locks", "--use-memory=1G",
		"--lock-ddl-per-table", "--throttle=100", "--ftwrl-wait-query-type=update"} {
		if err := SafeExtraArg(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"--defaults-file=/etc/shadow", "--tmpdir=x", "--datadir=/", "--plugin-load=x", "--target-dir=x",
		"--host=10.0.0.5", "--socket=x", "--user=root", "--stream=tar", "--log-bin=x", "--keyring-file-data=x", "--innobackupex",
		"--rsync", "--use-memory=../x", "--throttle=/1", "-x", "foo", "--", "--a b", "--copy-back", "--incremental-basedir=x"} {
		if err := SafeExtraArg(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestSameDir(t *testing.T) {
	same := [][2]string{{"/var/lib/mysql/", "/var/lib/mysql"}, {`C:\Program Files\MariaDB 11.4\data\`, `c:/program files/mariadb 11.4/data`}}
	for _, c := range same {
		if !SameDir(c[0], c[1]) {
			t.Errorf("%q != %q", c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"/var/lib/mysql", "/var/lib/MySQL"}, {"", ""}, {"/var/lib/mysql", "/var/lib/mysql2"}} {
		if SameDir(c[0], c[1]) {
			t.Errorf("%q == %q", c[0], c[1])
		}
	}
}
