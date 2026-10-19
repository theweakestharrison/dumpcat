package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"dumpcat/internal/engine"
	"dumpcat/internal/tunnel"
)

// Client is `dumpcat agent`: it keeps a control connection to the DumpCat
// server and opens the database connections the server asks for.
type Client struct {
	Server   string // DumpCat base URL, e.g. https://backup.example.com
	Key      string
	Allow    *AllowList
	Version  string
	Insecure bool // skip TLS certificate verification (self-signed server)
	Logf     func(format string, args ...any)
	// AllowPhysical lets the server run physical backups (mariabackup,
	// xtrabackup) on this host and stream them back (--allow-physical).
	AllowPhysical bool
	Datadir       string        // checked for read access when connecting ("": the usual location)
	Tools         *engine.Tools // finds the backup tools (nil: the default search)
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Run connects and reconnects until ctx ends.
func (c *Client) Run(ctx context.Context) error {
	if _, err := c.config(PathConnect, ""); err != nil {
		return err
	}
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(began) > time.Minute {
			backoff = 2 * time.Second
		}
		c.logf("disconnected: %v — reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

func (c *Client) config(p, query string) (*websocket.Config, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.Server), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid server URL %q (expected https://host[:port])", c.Server)
	}
	loc := *u
	loc.Scheme = map[string]string{"https": "wss", "http": "ws"}[u.Scheme]
	loc.Path = strings.TrimRight(u.Path, "/") + p
	loc.RawQuery = query
	origin := url.URL{Scheme: u.Scheme, Host: u.Host}
	cfg, err := websocket.NewConfig(loc.String(), origin.String())
	if err != nil {
		return nil, err
	}
	cfg.Header = http.Header{"Authorization": {"Bearer " + c.Key}, "User-Agent": {"dumpcat-agent/" + c.Version}}
	cfg.Dialer = &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	if u.Scheme == "https" {
		cfg.TlsConfig = &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: c.Insecure, MinVersion: tls.VersionTLS12}
	}
	return cfg, nil
}

func dial(cfg *websocket.Config) (*websocket.Conn, error) {
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		var de *websocket.DialError
		if errors.As(err, &de) && errors.Is(de.Err, websocket.ErrBadStatus) {
			return nil, errors.New("the server refused the agent (wrong or revoked key, or the URL is not a DumpCat server)")
		}
		return nil, err
	}
	return ws, nil
}

func (c *Client) session(ctx context.Context) error {
	cfg, err := c.config(PathConnect, "")
	if err != nil {
		return err
	}
	ws, err := dial(cfg)
	if err != nil {
		return err
	}
	defer ws.Close()
	var wmu sync.Mutex
	send := func(m Message) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return websocket.JSON.Send(ws, m)
	}
	host, _ := os.Hostname()
	caps := c.caps()
	if err := send(Message{Type: "hello", Version: c.Version, Hostname: host, OS: runtime.GOOS + "/" + runtime.GOARCH, Allow: c.Allow.Strings(), Caps: caps}); err != nil {
		return err
	}
	c.logf("connected to %s (may connect to: %s; %s)", c.Server, strings.Join(c.Allow.Strings(), ", "), caps.Physical.describe())
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-stop:
		}
	}()
	for {
		_ = ws.SetReadDeadline(time.Now().Add(idleTimeout))
		var m Message
		if err := websocket.JSON.Receive(ws, &m); err != nil {
			return err
		}
		switch m.Type {
		case "ping":
			if err := send(Message{Type: "pong"}); err != nil {
				return err
			}
		case "open":
			go c.open(m, send)
		case "physical":
			go c.physical(ctx, m, send)
		}
	}
}

// open dials the requested database and connects it to a new data stream.
func (c *Client) open(m Message, send func(Message) error) {
	fail := func(err error) {
		c.logf("connection to %s refused: %v", Target{Net: m.Net, Addr: m.Addr}, err)
		_ = send(Message{Type: "error", ID: m.ID, Error: err.Error()})
	}
	t, err := NormalizeTarget(Target{Net: m.Net, Addr: m.Addr})
	if err != nil {
		fail(err)
		return
	}
	if !c.Allow.Allows(t) {
		fail(fmt.Errorf("%s is not on this agent's allow list (start the agent with -allow %s)", t, t))
		return
	}
	db, err := net.DialTimeout(t.Net, t.Addr, 10*time.Second)
	if err != nil {
		fail(err)
		return
	}
	cfg, err := c.config(PathStream, "id="+url.QueryEscape(m.ID))
	if err != nil {
		db.Close()
		fail(err)
		return
	}
	ws, err := dial(cfg)
	if err != nil {
		db.Close()
		fail(err)
		return
	}
	ws.PayloadType = websocket.BinaryFrame
	tunnel.Pipe(db, ws)
}
