package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestFolderFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backup")
	for _, ok := range []string{"dumpcat-0a1b2c3d4e5f-shop.bak", "dumpcat-0a1b2c3d4e5f-my_20shop@2fdb.trn", "dumpcat-0a1b2c3d4e5f-probe.txt",
		"dumpcat-0a1b2c3d4e5f-_.hidden.bak", "dumpcat-x"} {
		p, err := FolderFile(dir, ok)
		if err != nil || p != filepath.Join(dir, ok) {
			t.Errorf("%q: %q %v", ok, p, err)
		}
	}
	for _, bad := range []string{"", "dumpcat-", "shop.bak", "Dumpcat-x.bak", "DUMPCAT-x", ".dumpcat-x", " dumpcat-x", "dumpcat-x ",
		"../dumpcat-x.bak", "dumpcat-x/../../etc/passwd", "dumpcat-../x", "dumpcat-a..b", "dumpcat-x/y.bak", `dumpcat-x\y.bak`,
		`..\dumpcat-x`, "dumpcat-x.bak:stream", "dumpcat-x.", "dumpcat-x\x00.bak", "dumpcat-x\n", "dumpcat-.bak", "dumpcat-é.bak",
		"/etc/dumpcat-x", `C:\dumpcat-x`, "dumpcat-" + strings.Repeat("x", 220)} {
		if p, err := FolderFile(dir, bad); err == nil {
			t.Errorf("%q accepted as %s", bad, p)
		}
	}
}

type folderEnv struct {
	mgr  *Manager
	cl   *Client
	clog *logs
	dir  string
	f    *Folder
}

func folderSetup(t *testing.T) *folderEnv {
	t.Helper()
	mgr, cl, _, clog, done := setup(t, "127.0.0.1:1433")
	t.Cleanup(done)
	dir := filepath.Join(t.TempDir(), "mssql backup")
	if err := os.Mkdir(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	cl.MSSQLDir = dir
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cl.Run(ctx)
	waitOnline(t, mgr, 7)
	f, err := mgr.Folder(7, "sql01", dir)
	if err != nil {
		t.Fatal(err)
	}
	return &folderEnv{mgr: mgr, cl: cl, clog: clog, dir: dir, f: f}
}

func (e *folderEnv) waitLog(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if e.clog.has(s) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent log has no %q: %v", s, e.clog.buf)
}

func (e *folderEnv) idle(t *testing.T) {
	t.Helper()
	for i := 0; i < 250; i++ {
		if e.mgr.Status(7).Streams == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d streams still counted", e.mgr.Status(7).Streams)
}

// names lists the files of the folder.
func (e *folderEnv) names() []string {
	entries, _ := os.ReadDir(e.dir)
	var out []string
	for _, d := range entries {
		out = append(out, d.Name())
	}
	return out
}

func TestFolderRoundTrip(t *testing.T) {
	e := folderSetup(t)
	st := e.mgr.Status(7)
	if st.Caps == nil || st.Caps.MSSQL == nil || st.Caps.MSSQL.Dir != e.dir || st.Caps.MSSQL.Error != "" || st.FolderProblem("") != "" {
		t.Fatalf("capabilities: %+v", st.Caps)
	}
	if e.f.Dir() != e.dir || !strings.Contains(e.f.String(), "agent sql01 ("+e.dir) {
		t.Fatalf("folder: %s %s", e.f.Dir(), e.f)
	}
	ctx := context.Background()
	if free, err := e.f.Free(ctx); err != nil || free == 0 {
		t.Fatalf("free: %d %v", free, err)
	}
	// a restore writes, SQL Server reads (several frames, not a multiple of their size)
	const size = 3<<20 + 12345
	data := fakeStream(size)
	n, err := e.f.Create(ctx, "dumpcat-0a1b2c3d4e5f-shop.bak", bytes.NewReader(data))
	if err != nil || n != size {
		t.Fatalf("create: %d %v", n, err)
	}
	p := filepath.Join(e.dir, "dumpcat-0a1b2c3d4e5f-shop.bak")
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("written file: %d bytes, %v", len(got), err)
	}
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v: SQL Server must read it", fi.Mode().Perm())
	}
	if _, err := e.f.Create(ctx, "dumpcat-0a1b2c3d4e5f-shop.bak", bytes.NewReader(data)); err == nil {
		t.Fatal("an existing file was overwritten")
	}
	// SQL Server writes, a backup reads
	rd, sz, err := e.f.Open(ctx, "dumpcat-0a1b2c3d4e5f-shop.bak")
	if err != nil || sz != size {
		t.Fatalf("open: %d %v", sz, err)
	}
	back, err := io.ReadAll(rd)
	rd.Close()
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("read back: %d bytes, %v", len(back), err)
	}
	// an empty file, then removing
	if n, err := e.f.Create(ctx, "dumpcat-0a1b2c3d4e5f-probe.txt", strings.NewReader("")); err != nil || n != 0 {
		t.Fatalf("empty file: %d %v", n, err)
	}
	if rd, sz, err := e.f.Open(ctx, "dumpcat-0a1b2c3d4e5f-probe.txt"); err != nil || sz != 0 {
		t.Fatalf("open empty: %v", err)
	} else if b, err := io.ReadAll(rd); err != nil || len(b) != 0 {
		t.Fatalf("read empty: %v", err)
	} else {
		rd.Close()
	}
	for _, name := range []string{"dumpcat-0a1b2c3d4e5f-shop.bak", "dumpcat-0a1b2c3d4e5f-probe.txt", "dumpcat-gone.bak"} {
		if err := e.f.Remove(ctx, name); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
	}
	if l := e.names(); len(l) != 0 {
		t.Fatalf("left in the folder: %v", l)
	}
	e.waitLog(t, "received dumpcat-0a1b2c3d4e5f-shop.bak")
	e.waitLog(t, "sent dumpcat-0a1b2c3d4e5f-shop.bak")
	e.idle(t)
}

func TestFolderRefused(t *testing.T) {
	e := folderSetup(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(e.dir), "dumpcat-secret.bak")
	if err := os.WriteFile(outside, []byte("not DumpCat's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "master.bak"), []byte("SQL Server's own"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.dir, "dumpcat-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../dumpcat-secret.bak", "master.bak", "dumpcat-dir/../../dumpcat-secret.bak", "dumpcat-missing.bak", "dumpcat-dir"} {
		if rd, _, err := e.f.Open(ctx, name); err == nil {
			rd.Close()
			t.Errorf("read %q", name)
		}
		if name != "dumpcat-missing.bak" {
			if err := e.f.Remove(ctx, name); err == nil {
				t.Errorf("removed %q", name)
			}
		}
	}
	if _, err := e.f.Create(ctx, "../dumpcat-new.bak", strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "dumpcat-") {
		t.Errorf("wrote outside the folder: %v", err)
	}
	if _, err := e.f.Create(ctx, "master.bak", strings.NewReader("x")); err == nil {
		t.Error("overwrote SQL Server's own file")
	}
	if b, _ := os.ReadFile(outside); string(b) != "not DumpCat's" {
		t.Fatal("the file outside the folder changed")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "master.bak")); err != nil {
		t.Fatal("SQL Server's own file is gone")
	}
	// a link in the folder is not followed
	link := filepath.Join(e.dir, "dumpcat-link.bak")
	if err := os.Symlink(outside, link); err == nil {
		if rd, _, err := e.f.Open(ctx, "dumpcat-link.bak"); err == nil {
			rd.Close()
			t.Error("followed a symbolic link out of the folder")
		}
		if err := e.f.Remove(ctx, "dumpcat-link.bak"); err == nil {
			t.Error("removed a link")
		}
		if _, err := e.f.Create(ctx, "dumpcat-link.bak", strings.NewReader("x")); err == nil {
			t.Error("wrote through a link")
		}
		if b, _ := os.ReadFile(outside); string(b) != "not DumpCat's" {
			t.Fatal("the link's target changed")
		}
	} else {
		t.Logf("no symbolic links here: %v", err)
	}
	// the server asks for anything anyway: the agent checks for itself
	for _, req := range []FileRequest{{Op: "read", Name: "../dumpcat-secret.bak"}, {Op: "write", Name: "x"}, {Op: "chmod", Name: "dumpcat-x"}, {Op: "read"}} {
		if _, err := e.mgr.request(7, Message{Type: "file", File: &req}); err == nil {
			t.Errorf("the agent accepted %+v", req)
		}
	}
	e.idle(t)

	// a folder that is gone
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.f.Free(ctx); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("free of a missing folder: %v", err)
	}
	if _, err := e.f.Create(ctx, "dumpcat-x.bak", strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("create in a missing folder: %v", err)
	}
	e.mgr.Disconnect(7) // reconnects: the folder is reported as missing
	waitOnline(t, e.mgr, 7)
	if st := e.mgr.Status(7); !strings.Contains(st.Caps.MSSQL.Error, "does not exist") || st.FolderProblem("") != "" {
		t.Errorf("a missing folder is reported, not refused: %+v", st.Caps.MSSQL)
	}

	// an agent without --allow-mssql-dir: the server does not even ask...
	e.cl.MSSQLDir = ""
	e.mgr.Disconnect(7)
	waitOnline(t, e.mgr, 7)
	if _, err := e.mgr.Folder(7, "sql01", "/var/opt/mssql/backup"); err == nil || !strings.Contains(err.Error(), "--allow-mssql-dir /var/opt/mssql/backup") {
		t.Fatalf("not allowed: %v", err)
	}
	// ...and the agent refuses when asked anyway
	if _, err := e.f.Free(ctx); err == nil || !strings.Contains(err.Error(), "--allow-mssql-dir") {
		t.Fatalf("the agent must refuse: %v", err)
	}
	if _, err := e.mgr.Folder(8, "other", ""); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline agent: %v", err)
	}
}

// failingReader breaks after some bytes, like a download that fails.
type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("download failed")
	}
	k := min(len(p), r.n)
	r.n -= k
	return k, nil
}

// cancelingReader yields zeros, cancels after n bytes and then fails like
// a download of a canceled task.
type cancelingReader struct {
	ctx    context.Context
	n      int
	cancel func()
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.n <= 0 {
		r.cancel()
		time.Sleep(50 * time.Millisecond)
		return 0, r.ctx.Err()
	}
	k := min(len(p), r.n)
	r.n -= k
	clear(p[:k])
	return k, nil
}

// slowWriter stops the copy of a file by calling fn on the first write.
type slowWriter struct {
	fn   func()
	done bool
}

func (w *slowWriter) Write(p []byte) (int, error) {
	if !w.done {
		w.done = true
		w.fn()
	}
	return len(p), nil
}

func TestFolderAborted(t *testing.T) {
	e := folderSetup(t)
	ctx := context.Background()
	// a failed download leaves no file behind
	if _, err := e.f.Create(ctx, "dumpcat-0a1b2c3d4e5f-half.bak", &failingReader{n: 1 << 20}); err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("create from a failing source: %v", err)
	}
	e.waitLog(t, "receiving dumpcat-0a1b2c3d4e5f-half.bak stopped")
	if l := e.names(); len(l) != 0 {
		t.Fatalf("a partial file stayed: %v", l)
	}
	// a canceled restore too (its download stops with the task)
	cctx, cancel := context.WithCancel(ctx)
	if _, err := e.f.Create(cctx, "dumpcat-0a1b2c3d4e5f-cancel.bak", &cancelingReader{ctx: cctx, n: 600 << 10, cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create: %v", err)
	}
	for i := 0; i < 250 && len(e.names()) > 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if l := e.names(); len(l) != 0 {
		t.Fatalf("a canceled file stayed: %v", l)
	}
	// a canceled read stops the agent
	if err := os.WriteFile(filepath.Join(e.dir, "dumpcat-big.bak"), make([]byte, 64<<20), 0o640); err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	rd, _, err := e.f.Open(rctx, "dumpcat-big.bak")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(&slowWriter{fn: rcancel}, rd)
	rd.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	e.waitLog(t, "sending dumpcat-big.bak canceled")
	// a revoked key stops a transfer
	rd, _, err = e.f.Open(ctx, "dumpcat-big.bak")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(&slowWriter{fn: func() { go e.mgr.Disconnect(7) }}, rd)
	rd.Close()
	if err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("read after a disconnect: %v", err)
	}
	e.idle(t)
	if _, err := os.Stat(filepath.Join(e.dir, "dumpcat-big.bak")); err != nil {
		t.Fatal("reading must not remove the file: DumpCat removes it")
	}
}

// An agent from before SQL Server's backup folder: the server asks it for nothing.
func TestFolderOldAgent(t *testing.T) {
	mgr, cl, _, _, done := setup(t, "127.0.0.1:1433")
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
	if err := websocket.Message.Send(ws, `{"type":"hello","version":"1.7.0","hostname":"sql01","os":"linux/amd64","allow":["127.0.0.1:1433"],"caps":{"physical":{"allowed":false}}}`); err != nil {
		t.Fatal(err)
	}
	waitOnline(t, mgr, 7)
	if _, err := mgr.Folder(7, "sql01", "/var/opt/mssql/backup"); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Fatalf("old agent: %v", err)
	}
}

func TestFolderBadDir(t *testing.T) {
	_, cl, _, _, done := setup(t, "127.0.0.1:1433")
	defer done()
	cl.MSSQLDir = "relative/backup"
	if err := cl.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("a relative folder: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	for dir, want := range map[string]string{filepath.Join(t.TempDir(), "missing"): "does not exist", file: "not a directory"} {
		if err := checkFolder(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", dir, err)
		}
	}
}
