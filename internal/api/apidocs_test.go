package api

import (
	"encoding/json"
	"strings"
	"testing"

	"dumpcat/internal/store"
)

// Every route has an entry in the Markdown reference, and every entry is a route.
func TestAPIReferenceCoversRoutes(t *testing.T) {
	f := newMCPFixture(t)
	seen := map[string]bool{}
	for _, sec := range apiSections {
		for _, d := range sec.Routes {
			if seen[d.Route] {
				t.Errorf("%s is documented twice in apiSections", d.Route)
			}
			seen[d.Route] = true
			if _, ok := f.s.routeRoles[d.Route]; !ok {
				t.Errorf("apiSections documents %q, which is not a route", d.Route)
			}
			if strings.TrimSpace(d.Doc) == "" {
				t.Errorf("%s has an empty description", d.Route)
			}
		}
	}
	for pattern := range f.s.routeRoles {
		if !seen[pattern] {
			t.Errorf("route %q has no entry in apiSections (internal/api/apidocs.go): describe it for GET /api/docs.md", pattern)
		}
	}
}

func TestAPIReferenceMarkdown(t *testing.T) {
	f := newMCPFixture(t)
	if rec := get(t, f.h, "/api/docs.md"); rec.Code != 401 {
		t.Fatalf("without a token: %d, want 401", rec.Code)
	}
	tok := f.token(store.RoleViewer, store.TokenRead)
	rec := get(t, f.h, "/api/docs.md", "Authorization", "Bearer "+tok)
	if rec.Code != 200 {
		t.Fatalf("read-only token: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	md := rec.Body.String()
	for _, want := range []string{
		"# DumpCat API reference",
		"DumpCat 1.5.0 at http://example.com",
		"`Authorization: Bearer mv_…`",
		"#### `POST /api/jobs/{id}/run`\n\noperator · full-access token\n\n",
		"#### `GET /api/runs`\n\nviewer · read-only or full-access token\n\n",
		"#### `POST /api/pitr/plan`\n\noperator · read-only or full-access token\n\n",
		"#### `GET /api/runs/{id}/download`\n\noperator · full-access token\n\n",
		"#### `POST /api/auth/login`\n\npublic · no token needed\n\n",
		"#### `POST /api/mcp`\n\npublic · any API token\n\n",
		"| `run_backup` | Start a backup of a job now",
		"`http://example.com/api/mcp`",
		"#### `GET /metrics`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the reference lacks %q", want)
		}
	}
	// each MCP tool is a row of the table: a pipe in a description would break it
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| `") && strings.Count(line, "|") != 6 {
			t.Errorf("table row with a stray pipe: %s", line)
		}
	}
	// the configured address wins over the request's
	set := f.st.GetSettings()
	set.BaseURL = "https://backup.example.org"
	if err := f.st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	md = get(t, f.h, "/api/docs.md", "Authorization", "Bearer "+tok).Body.String()
	if !strings.Contains(md, "https://backup.example.org/api/mcp") || strings.Contains(md, "http://example.com") {
		t.Errorf("the reference does not use the base URL")
	}
}

// The demo builds the reference without a running server.
func TestDemoReference(t *testing.T) {
	md, tools := DemoReference("9.9.9", "https://backup.example.com")
	if !strings.Contains(md, "DumpCat 9.9.9 at https://backup.example.com") || !strings.Contains(md, "#### `GET /api/docs.md`") {
		t.Errorf("unexpected reference:\n%.300s", md)
	}
	if l, _ := tools.([]mcpToolInfo); len(l) != len(mcpTools()) || !l[0].Available {
		t.Errorf("tools: %+v", tools)
	}
}

func TestMCPToolsInfo(t *testing.T) {
	f := newMCPFixture(t)
	var out struct {
		Endpoint string        `json:"endpoint"`
		Docs     string        `json:"docs"`
		Tools    []mcpToolInfo `json:"tools"`
	}
	tools := func(role string) map[string]mcpToolInfo {
		rec := get(t, f.h, "/api/mcp/tools", "Authorization", "Bearer "+f.token(role, store.TokenRead))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", role, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		m := map[string]mcpToolInfo{}
		for _, x := range out.Tools {
			m[x.Name] = x
		}
		return m
	}
	v := tools(store.RoleViewer)
	if out.Endpoint != "http://example.com/api/mcp" || out.Docs != "http://example.com/api/docs.md" {
		t.Errorf("endpoint %q, docs %q", out.Endpoint, out.Docs)
	}
	if len(v) != len(mcpTools()) {
		t.Errorf("%d tools, want %d", len(v), len(mcpTools()))
	}
	if x := v["get_overview"]; !x.Available || x.Write || x.Role != store.RoleViewer {
		t.Errorf("get_overview for a viewer: %+v", x)
	}
	if x := v["run_backup"]; x.Available || !x.Write || x.Role != store.RoleOperator {
		t.Errorf("run_backup for a viewer: %+v", x)
	}
	if x := v["plan_pitr"]; x.Available || x.Write {
		t.Errorf("plan_pitr for a viewer: %+v", x)
	}
	if x := tools(store.RoleOperator)["run_backup"]; !x.Available {
		t.Errorf("run_backup for an operator: %+v", x)
	}
}
