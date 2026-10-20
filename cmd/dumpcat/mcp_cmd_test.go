package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMCPSettings(t *testing.T) {
	env := func(kv map[string]string) (*mcpSettings, error) {
		for _, k := range []string{"DUMPCAT_MCP_URL", "DUMPCAT_MCP_TOKEN", "DUMPCAT_MCP_TOKEN_FILE", "DUMPCAT_MCP_CA", "DUMPCAT_MCP_INSECURE"} {
			t.Setenv(k, kv[k])
		}
		return mcpSettingsFromEnv()
	}
	for in, want := range map[string]string{
		"https://backup.example.com":           "https://backup.example.com/api/mcp",
		"https://backup.example.com/":          "https://backup.example.com/api/mcp",
		"https://h.example/dumpcat/":           "https://h.example/dumpcat/api/mcp",
		"https://h.example:8443/api/mcp?x=1#y": "https://h.example:8443/api/mcp",
		"http://127.0.0.1:6369":                "http://127.0.0.1:6369/api/mcp",
		"http://localhost:6369":                "http://localhost:6369/api/mcp",
	} {
		s, err := env(map[string]string{"DUMPCAT_MCP_URL": in, "DUMPCAT_MCP_TOKEN": "mv_abc"})
		if err != nil || s.URL != want {
			t.Fatalf("%s: %+v %v", in, s, err)
		}
	}
	for _, bad := range []map[string]string{
		{"DUMPCAT_MCP_TOKEN": "mv_abc"},
		{"DUMPCAT_MCP_URL": "https://h.example"},
		{"DUMPCAT_MCP_URL": "ftp://h.example", "DUMPCAT_MCP_TOKEN": "mv_abc"},
		{"DUMPCAT_MCP_URL": "https://user:pw@h.example", "DUMPCAT_MCP_TOKEN": "mv_abc"},
		{"DUMPCAT_MCP_URL": "https://h.example", "DUMPCAT_MCP_TOKEN": "mv_a b"},
		// the token would cross the network in clear text
		{"DUMPCAT_MCP_URL": "http://backup.example.com", "DUMPCAT_MCP_TOKEN": "mv_abc"},
	} {
		if s, err := env(bad); err == nil {
			t.Fatalf("%v accepted: %+v", bad, s)
		}
	}
	if s, err := env(map[string]string{"DUMPCAT_MCP_URL": "http://backup.example.com", "DUMPCAT_MCP_TOKEN": "mv_abc", "DUMPCAT_MCP_INSECURE": "1"}); err != nil || !s.Insecure {
		t.Fatalf("insecure on request: %+v %v", s, err)
	}
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("mv_fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := env(map[string]string{"DUMPCAT_MCP_URL": "https://h.example", "DUMPCAT_MCP_TOKEN_FILE": f}); err != nil || s.Token != "mv_fromfile" {
		t.Fatalf("token file: %+v %v", s, err)
	}
	if _, err := env(map[string]string{"DUMPCAT_MCP_URL": "https://h.example", "DUMPCAT_MCP_TOKEN": "mv_abc", "DUMPCAT_MCP_CA": f}); err != nil {
		t.Fatal(err)
	} else if _, err := (&mcpSettings{CAFile: f}).client(); err == nil {
		t.Fatal("a CA file without a certificate was accepted")
	}
}

// fakeMCP stands in for DumpCat's /api/mcp.
func fakeMCP(t *testing.T) (*httptest.Server, *[]http.Header) {
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		if r.URL.Path != "/api/mcp" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer mv_good" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"invalid API token"}`)
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &m)
		if len(m.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch m.Method {
		case "initialize":
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"protocolVersion":"2025-06-18","serverInfo":{"version":"1.5.0"},
				"instructions":"DumpCat.\nSigned in as op (role operator) with full access. More."}}`+"\n")
		case "tools/list":
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"tools":[{"name":"get_overview"},{"name":"run_backup"}]}}`)
		default:
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"error":{"code":-32601,"message":"method not found"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestMCPBridge(t *testing.T) {
	srv, seen := fakeMCP(t)
	var out bytes.Buffer
	var logs []string
	b := &mcpBridge{set: &mcpSettings{URL: srv.URL + "/api/mcp", Token: "mv_good"}, hc: srv.Client(), out: &out,
		logf: func(f string, a ...any) { logs = append(logs, f) }}
	// the client waits for the initialize answer before it sends more
	if err := b.serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`+"\n")); err != nil {
		t.Fatal(err)
	}
	in := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n\n" +
		`{"jsonrpc":"2.0","id":"two","method":"tools/list"}` + "\r\n" +
		"not json\n" +
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}` // no final newline
	if err := b.serve(context.Background(), strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	answers := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not one JSON message per line: %q", line)
		}
		id, _ := json.Marshal(m["id"])
		answers[string(id)] = m
	}
	if len(answers) != 4 || answers["1"]["result"] == nil || answers[`"two"`]["result"] == nil ||
		answers["3"]["error"] == nil || answers["null"]["error"] == nil {
		t.Fatalf("answers: %s", out.String())
	}
	if len(*seen) != 4 { // the parse error never reached the server
		t.Fatalf("%d requests", len(*seen))
	}
	for i, h := range *seen {
		if h.Get("Authorization") != "Bearer mv_good" || !strings.Contains(h.Get("Accept"), "text/event-stream") {
			t.Fatalf("request %d headers: %v", i, h)
		}
		if want := map[bool]string{true: "", false: "2025-06-18"}[i == 0]; h.Get("MCP-Protocol-Version") != want {
			t.Fatalf("request %d: protocol version %q, want %q", i, h.Get("MCP-Protocol-Version"), want)
		}
	}

	// a refused token is an error answer the client shows, not silence
	out.Reset()
	b = &mcpBridge{set: &mcpSettings{URL: srv.URL + "/api/mcp", Token: "mv_bad"}, hc: srv.Client(), out: &out, logf: func(string, ...any) {}}
	if err := b.serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":5`) || !strings.Contains(out.String(), "refused the API token: invalid API token") {
		t.Fatalf("401: %s", out.String())
	}
	out.Reset()
	b = &mcpBridge{set: &mcpSettings{URL: srv.URL + "/nope", Token: "mv_good"}, hc: srv.Client(), out: &out, logf: func(string, ...any) {}}
	b.serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":6,"method":"tools/list"}`+"\n"))
	if !strings.Contains(out.String(), "no MCP endpoint") {
		t.Fatalf("404: %s", out.String())
	}

	// dumpcat mcp -check
	out.Reset()
	b = &mcpBridge{set: &mcpSettings{URL: srv.URL + "/api/mcp", Token: "mv_good"}, hc: srv.Client(), out: io.Discard, logf: func(string, ...any) {}}
	if err := b.check(context.Background(), &out); err != nil || !strings.Contains(out.String(), "Signed in as op (role operator) with full access") ||
		!strings.Contains(out.String(), "2 tools: get_overview, run_backup") {
		t.Fatalf("check: %v\n%s", err, out.String())
	}
}
