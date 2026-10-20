package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/config"
)

func defaultMCPEnvFile() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "dumpcat", "mcp.env")
	}
	return ""
}

// mcpCmd runs `dumpcat mcp`: a stdio MCP server for AI assistants that start a local process
// (Claude Desktop, Claude Code, other MCP clients). It forwards each JSON-RPC message from stdin
// to the /api/mcp endpoint of a DumpCat server and writes the answers to stdout; what the tools
// may do is decided there, by the API token. The address and the token come from the
// environment or an env file, never from the command line, where other users could read them.
func mcpCmd(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	envFile := fs.String("config", defaultMCPEnvFile(), "env file with the DUMPCAT_MCP_* settings (KEY=VALUE lines)")
	check := fs.Bool("check", false, "connect, print the account and the tools it can use, and exit")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: dumpcat mcp [-check] [-config FILE]

Serves DumpCat's tools to an AI assistant over stdio (Model Context Protocol):
the assistant starts this command, which forwards its requests to a DumpCat
server's /api/mcp endpoint with an API token.

Settings (environment, or KEY=VALUE lines in the env file, default %s):
  DUMPCAT_MCP_URL         DumpCat address, e.g. https://backup.example.com
  DUMPCAT_MCP_TOKEN       API token (API & MCP page → New token), or
  DUMPCAT_MCP_TOKEN_FILE  a file that holds it
  DUMPCAT_MCP_CA          PEM file to trust instead of the system's CAs, e.g. the
                          server's self-signed cert.pem
  DUMPCAT_MCP_INSECURE=1  accept any certificate, or plain HTTP to another host

Flags:
`, defaultMCPEnvFile())
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	log.SetOutput(os.Stderr) // stdout carries the protocol only
	log.SetFlags(0)
	log.SetPrefix("dumpcat mcp: ")
	if fs.NArg() > 0 {
		fs.Usage()
		log.Fatal("no arguments: the address and the token come from the environment or the env file")
	}
	if *envFile != "" {
		if err := config.LoadEnvFile(*envFile); err != nil {
			log.Fatalf("cannot read %s: %v", *envFile, err)
		}
		warnReadableByOthers(*envFile)
	}
	set, err := mcpSettingsFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	hc, err := set.client()
	if err != nil {
		log.Fatal(err)
	}
	if set.Insecure {
		log.Printf("warning: the connection to %s is not verified (DUMPCAT_MCP_INSECURE)", set.URL)
	}
	b := &mcpBridge{set: set, hc: hc, out: os.Stdout, logf: log.Printf}
	if *check {
		if err := b.check(context.Background(), os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := b.serve(context.Background(), os.Stdin); err != nil {
		log.Fatal(err)
	}
}

// warnReadableByOthers: a file with a token should be private (chmod 600).
func warnReadableByOthers(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		log.Printf("warning: other users can read %s: chmod 600 %s", path, path)
	}
}

type mcpSettings struct {
	URL      string // the /api/mcp endpoint
	Token    string
	CAFile   string
	Insecure bool
}

func mcpSettingsFromEnv() (*mcpSettings, error) {
	raw := strings.TrimSpace(os.Getenv("DUMPCAT_MCP_URL"))
	if raw == "" {
		return nil, errors.New("DUMPCAT_MCP_URL is not set: the DumpCat address, e.g. https://backup.example.com (dumpcat mcp -h)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, fmt.Errorf("DUMPCAT_MCP_URL %q is not an address like https://backup.example.com", raw)
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/api/mcp") {
		u.Path += "/api/mcp"
	}
	s := &mcpSettings{URL: u.String(), CAFile: strings.TrimSpace(os.Getenv("DUMPCAT_MCP_CA"))}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DUMPCAT_MCP_INSECURE"))) {
	case "1", "true", "yes", "on":
		s.Insecure = true
	}
	s.Token = strings.TrimSpace(os.Getenv("DUMPCAT_MCP_TOKEN"))
	if f := strings.TrimSpace(os.Getenv("DUMPCAT_MCP_TOKEN_FILE")); s.Token == "" && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("DUMPCAT_MCP_TOKEN_FILE: %v", err)
		}
		warnReadableByOthers(f)
		s.Token = strings.TrimSpace(string(b))
	}
	if s.Token == "" {
		return nil, errors.New("DUMPCAT_MCP_TOKEN (or DUMPCAT_MCP_TOKEN_FILE) is not set: create an API token in DumpCat on the API & MCP page")
	}
	if strings.ContainsFunc(s.Token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return nil, errors.New("the API token contains a space or a line break")
	}
	if u.Scheme == "http" && !s.Insecure && !loopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%s would send the API token unencrypted: use https://, or set DUMPCAT_MCP_INSECURE=1 to accept that", u.Redacted())
	}
	return s, nil
}

func loopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// client checks certificates like the agent: the system's CAs (or DUMPCAT_MCP_CA), TLS 1.2 or
// newer, and no check only when asked. Redirects are not followed: the token goes to one address.
func (s *mcpSettings) client() (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.Insecure}
	if s.CAFile != "" {
		pem, err := os.ReadFile(s.CAFile)
		if err != nil {
			return nil, fmt.Errorf("DUMPCAT_MCP_CA: %v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("DUMPCAT_MCP_CA: no PEM certificate in %s", s.CAFile)
		}
		tc.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	return &http.Client{Transport: tr, Timeout: 5 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

// mcpBridge forwards newline-delimited JSON-RPC messages to the HTTP endpoint, several at a
// time; answers are written as they come, one per line.
type mcpBridge struct {
	set  *mcpSettings
	hc   *http.Client
	out  io.Writer
	logf func(string, ...any)

	outMu    sync.Mutex
	mu       sync.Mutex
	version  string // protocol version from initialize, sent as MCP-Protocol-Version
	session  string // Mcp-Session-Id, when a server hands one out
	inflight map[string]context.CancelFunc
}

func (b *mcpBridge) serve(ctx context.Context, in io.Reader) error {
	rd := bufio.NewReaderSize(in, 64<<10)
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for {
		line, rerr := rd.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					RequestID json.RawMessage `json:"requestId"`
				} `json:"params"`
			}
			if err := json.Unmarshal(msg, &m); err != nil {
				b.writeError(nil, -32700, "parse error: "+err.Error())
			} else {
				request := m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null"
				if m.Method == "notifications/cancelled" {
					b.cancel(string(m.Params.RequestID))
				}
				rctx, cancel := context.WithCancel(ctx)
				if request {
					b.track(string(m.ID), cancel)
				}
				slots <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() { <-slots; wg.Done() }()
					defer cancel()
					if request {
						defer b.track(string(m.ID), nil)
					}
					b.forward(rctx, msg, m.ID, m.Method, request)
				}()
			}
		}
		if rerr != nil {
			wg.Wait() // the client closed stdin: finish what was asked
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

func (b *mcpBridge) track(id string, cancel context.CancelFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflight == nil {
		b.inflight = map[string]context.CancelFunc{}
	}
	if cancel == nil {
		delete(b.inflight, id)
	} else {
		b.inflight[id] = cancel
	}
}

func (b *mcpBridge) cancel(id string) {
	b.mu.Lock()
	c := b.inflight[id]
	b.mu.Unlock()
	if c != nil {
		c()
	}
}

func (b *mcpBridge) forward(ctx context.Context, msg, id json.RawMessage, method string, request bool) {
	answer, err := b.post(ctx, msg)
	if err != nil {
		if ctx.Err() != nil && request {
			return // canceled by the client, which expects no answer
		}
		b.logf("%s: %v", method, err)
		if request {
			b.writeError(id, -32000, err.Error())
		}
		return
	}
	if len(answer) == 0 {
		return
	}
	if method == "initialize" {
		var r struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(answer, &r) == nil && r.Result.ProtocolVersion != "" {
			b.mu.Lock()
			b.version = r.Result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	b.writeLine(answer)
}

// post sends one message and returns the answer (nil when the server accepted a notification).
func (b *mcpBridge) post(ctx context.Context, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.set.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+b.set.Token)
	req.Header.Set("User-Agent", "dumpcat-mcp/"+version)
	b.mu.Lock()
	if b.version != "" {
		req.Header.Set("MCP-Protocol-Version", b.version)
	}
	if b.session != "" {
		req.Header.Set("Mcp-Session-Id", b.session)
	}
	b.mu.Unlock()
	resp, err := b.hc.Do(req)
	if err != nil {
		var ue x509.UnknownAuthorityError
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("cannot reach DumpCat at %s: %v (a self-signed certificate: set DUMPCAT_MCP_CA to the server's cert.pem)", b.set.URL, err)
		}
		return nil, fmt.Errorf("cannot reach DumpCat at %s: %v", b.set.URL, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		b.mu.Lock()
		b.session = sid
		b.mu.Unlock()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("reading DumpCat's answer: %v", err)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil, nil
	case resp.StatusCode == http.StatusOK && ct == "application/json":
		var buf bytes.Buffer
		if err := json.Compact(&buf, body); err != nil { // one message per line
			return nil, fmt.Errorf("DumpCat sent an unreadable answer: %v", err)
		}
		return buf.Bytes(), nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fmt.Errorf("%s redirects to %s: set DUMPCAT_MCP_URL to the final address", b.set.URL, resp.Header.Get("Location"))
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("DumpCat refused the API token: %s", e.Error)
	case http.StatusNotFound:
		return nil, fmt.Errorf("no MCP endpoint at %s (it needs DumpCat 1.5.0 or later)", b.set.URL)
	}
	return nil, fmt.Errorf("DumpCat answered %s: %s", resp.Status, e.Error)
}

func (b *mcpBridge) writeLine(line []byte) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	if _, err := b.out.Write(append(line, '\n')); err != nil {
		b.logf("writing to the client: %v", err)
	}
}

func (b *mcpBridge) writeError(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	b.writeLine(line)
}

// check connects like a client would and reports who the token belongs to and its tools.
func (b *mcpBridge) check(ctx context.Context, w io.Writer) error {
	call := func(id int, method string, params any) (json.RawMessage, error) {
		msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		answer, err := b.post(ctx, msg)
		if err != nil {
			return nil, err
		}
		var r struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(answer, &r); err != nil {
			return nil, err
		}
		if r.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		return r.Result, nil
	}
	res, err := call(1, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "dumpcat-mcp-check", "version": version}})
	if err != nil {
		return err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		return err
	}
	b.version = init.ProtocolVersion
	res, err = call(2, "tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return err
	}
	var names []string
	for _, t := range list.Tools {
		names = append(names, t.Name)
	}
	who := ""
	for _, l := range strings.Split(init.Instructions, "\n") {
		if strings.HasPrefix(l, "Signed in as ") {
			who, _, _ = strings.Cut(l, ". ")
		}
	}
	fmt.Fprintf(w, "OK: DumpCat %s at %s (MCP %s)\n%s\n%d tools: %s\n", init.ServerInfo.Version, b.set.URL, init.ProtocolVersion,
		who, len(names), strings.Join(names, ", "))
	return nil
}
