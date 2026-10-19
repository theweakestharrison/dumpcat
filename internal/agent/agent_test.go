package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoServer stands in for the database.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

type logs struct {
	mu  sync.Mutex
	buf []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	l.buf = append(l.buf, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logs) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range l.buf {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

func setup(t *testing.T, allow ...string) (*Manager, *Client, *logs, *logs, func()) {
	t.Helper()
	mgr := NewManager()
	mgr.Auth = func(key string) (int64, error) {
		if key == "dca_good" {
			return 7, nil
		}
		return 0, errors.New("bad key")
	}
	slog, clog := &logs{}, &logs{}
	mgr.Logf = slog.logf
	mux := http.NewServeMux()
	mux.HandleFunc(PathConnect, func(w http.ResponseWriter, r *http.Request) { mgr.ServeControl(w, r, r.RemoteAddr) })
	mux.HandleFunc(PathStream, mgr.ServeStream)
	ts := httptest.NewServer(mux)
	al, err := ParseAllowList(allow)
	if err != nil {
		t.Fatal(err)
	}
	cl := &Client{Server: ts.URL + "/", Key: "dca_good", Allow: al, Version: "test", Logf: clog.logf}
	return mgr, cl, slog, clog, func() { mgr.Close(); ts.Close() }
}

func waitOnline(t *testing.T, mgr *Manager, id int64) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if mgr.Status(id).Online {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("agent did not connect")
}

func TestTunnelRoundTrip(t *testing.T) {
	db := echoServer(t)
	mgr, cl, _, _, done := setup(t, db)
	defer done()
	if _, _, err := mgr.Endpoint(7, Target{Net: "tcp", Addr: db}); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline agent: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cl.Run(ctx)
	waitOnline(t, mgr, 7)
	st := mgr.Status(7)
	if st.Version != "test" || len(st.Allow) != 1 {
		t.Fatalf("status: %+v", st)
	}
	host, port, err := mgr.Endpoint(7, Target{Net: "tcp", Addr: db})
	if err != nil {
		t.Fatal(err)
	}
	// same target, same port
	if _, p2, _ := mgr.Endpoint(7, Target{Net: "tcp", Addr: db}); p2 != port {
		t.Fatal("endpoint must be stable")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			payload := make([]byte, 512*1024+i)
			_, _ = rand.Read(payload)
			go func() { _, _ = c.Write(payload) }()
			got := make([]byte, len(payload))
			_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
			if _, err := io.ReadFull(c, got); err != nil {
				t.Errorf("stream %d: %v", i, err)
				return
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("stream %d: data differs", i)
			}
		}(i)
	}
	wg.Wait()
}

func TestTunnelAllowList(t *testing.T) {
	db := echoServer(t)
	other := echoServer(t)
	mgr, cl, slog, clog, done := setup(t, db)
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cl.Run(ctx)
	waitOnline(t, mgr, 7)
	host, port, err := mgr.Endpoint(7, Target{Net: "tcp", Addr: other})
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	// the client gets a MySQL error packet with the reason, then EOF
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 8 || got[4] != 0xff || !bytes.Contains(got, []byte("not on this agent's allow list")) {
		t.Fatalf("a target outside the allow list must not be reachable; got %q", got)
	}
	if n := int(got[0]) | int(got[1])<<8 | int(got[2])<<16; n != len(got)-4 {
		t.Fatalf("packet length %d, payload %d", n, len(got)-4)
	}
	time.Sleep(100 * time.Millisecond)
	if !clog.has("not on this agent's allow list") || !slog.has("not on this agent's allow list") {
		t.Fatalf("refusal not reported: %v / %v", clog.buf, slog.buf)
	}
}

func TestBadKey(t *testing.T) {
	mgr, cl, _, clog, done := setup(t, "127.0.0.1:3306")
	defer done()
	cl.Key = "dca_bad"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cl.Run(ctx)
	time.Sleep(500 * time.Millisecond)
	if mgr.Status(7).Online {
		t.Fatal("a wrong key must not connect")
	}
	if !clog.has("refused the agent") {
		t.Fatalf("client log: %v", clog.buf)
	}
}

func TestDisconnectAndReconnect(t *testing.T) {
	db := echoServer(t)
	mgr, cl, _, _, done := setup(t, db)
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cl.Run(ctx)
	waitOnline(t, mgr, 7)
	mgr.Disconnect(7)
	if mgr.Status(7).Online {
		t.Fatal("still online after Disconnect")
	}
	waitOnline(t, mgr, 7) // the agent comes back by itself
	cancel()
	for i := 0; i < 200 && mgr.Status(7).Online; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.Status(7).Online {
		t.Fatal("still online after the agent stopped")
	}
}

func TestTargets(t *testing.T) {
	cases := map[string]string{
		"localhost:3306":   "127.0.0.1:3306",
		"LOCALHOST:3307":   "127.0.0.1:3307",
		"[::1]:3306":       "[::1]:3306",
		"db.internal:3306": "db.internal:3306",
		"unix:/var/run/mysqld/../mysqld/mysqld.sock": "unix:/var/run/mysqld/mysqld.sock",
		"/tmp/mysql.sock": "unix:/tmp/mysql.sock",
	}
	for in, want := range cases {
		got, err := ParseTarget(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseTarget(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "nohost", "host:abc", "unix:relative.sock"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	al, err := ParseAllowList([]string{"127.0.0.1:3306", "unix:/run/mysqld/mysqld.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if !al.Allows(Target{Net: "tcp", Addr: "localhost:3306"}) || al.Allows(Target{Net: "tcp", Addr: "127.0.0.1:3307"}) ||
		!al.Allows(Target{Net: "unix", Addr: "/run/mysqld/mysqld.sock"}) || al.Allows(Target{Net: "tcp", Addr: "10.0.0.5:3306"}) {
		t.Fatal("allow list")
	}
	any, _ := ParseAllowList([]string{"*"})
	if !any.Allows(Target{Net: "tcp", Addr: "10.0.0.5:3306"}) {
		t.Fatal("* allows everything")
	}
	if _, err := ParseAllowList([]string{" ", ""}); err == nil {
		t.Fatal("empty allow list accepted")
	}
	if _, err := ParseAllowList(DefaultAllow); err != nil {
		t.Fatal(err)
	}
}
