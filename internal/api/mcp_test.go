package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"dumpcat/internal/auth"
	"dumpcat/internal/config"
	"dumpcat/internal/engine"
	"dumpcat/internal/runner"
	"dumpcat/internal/scheduler"
	"dumpcat/internal/secret"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

type mcpFixture struct {
	t     *testing.T
	s     *Server
	st    *store.Store
	h     http.Handler
	job   *store.Job
	sv    *store.Server
	run   *store.Run
	users map[string]*store.User
}

// secrets planted in servers, jobs, backups and logs: no MCP answer may contain one
var mcpPlanted = []string{"db-pass-Xq81", "restore-pass-Zr42", "passphrase-Kd93", "hook-secret-Wm55", "url-pass-Pt27", "mv_leakedTokenAbcdefghijklmnop"}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "master.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "dumpcat.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tools := engine.NewTools(func() map[string]string { return nil })
	sm := storage.NewManager(st, filepath.Join(dir, "rclone"), func() string { return "" }, func() []string { return nil })
	run := runner.New(st, sm, tools, nil, dir, "1.5.0")
	t.Cleanup(func() { run.Shutdown(time.Minute) })
	s := &Server{Cfg: &config.Config{DataDir: dir}, Store: st, Runner: run, Sched: scheduler.New(st, run), Tools: tools,
		Storage: sm, Limiter: auth.NewLimiter(), IPLimit: auth.NewLimiter(), Version: "1.5.0"}
	mux := http.NewServeMux()
	s.routes(mux)
	f := &mcpFixture{t: t, s: s, st: st, h: mux, users: map[string]*store.User{}}

	for _, role := range []string{store.RoleViewer, store.RoleOperator, store.RoleAdmin} {
		hash, _ := auth.HashPassword("correct horse " + role)
		u := &store.User{Username: role + "1", PasswordHash: hash, Role: role}
		if err := st.CreateUser(u); err != nil {
			t.Fatal(err)
		}
		f.users[role] = u
	}
	// nothing listens on port 1: a backup of this server fails at once
	f.sv = &store.Server{Name: "db1", Host: "127.0.0.1", Port: 1, Username: "backup", Password: mcpPlanted[0],
		RestoreUsername: "restorer", RestorePassword: mcpPlanted[1], Flavor: "auto", Notes: "primary"}
	if err := st.CreateServer(f.sv); err != nil {
		t.Fatal(err)
	}
	disk := &store.Storage{Name: "disk", Type: "local", Config: map[string]string{"path": filepath.Join(dir, "backups")}}
	if err := st.CreateStorage(disk); err != nil {
		t.Fatal(err)
	}
	f.job = &store.Job{UUID: runner.NewUUID(), Name: "nightly", ServerID: f.sv.ID, Method: store.MethodMysqldump, DBMode: "all",
		Databases: []string{}, ExcludeTables: []string{}, Compression: "zstd", Encrypt: true, Passphrase: mcpPlanted[2], Enabled: true,
		Options: store.JobOptions{VerifyMode: store.VerifyOff,
			PreCommand: "curl -fsS -H 'X-Token: " + mcpPlanted[3] + "' https://hooks.example/start"},
		Targets: []store.JobTarget{{StorageID: disk.ID}}}
	if err := st.CreateJob(f.job); err != nil {
		t.Fatal(err)
	}
	jid, sid := f.job.ID, f.sv.ID
	f.run = &store.Run{UUID: runner.NewUUID(), JobID: &jid, JobName: f.job.Name, JobUUID: f.job.UUID, JobSlug: "nightly-1", ServerID: &sid,
		ServerName: f.sv.Name, Method: store.MethodMysqldump, Type: store.TypeFull, Status: store.StatusFailed, Trigger: "schedule",
		StartedAt: time.Now().Add(-time.Hour), Error: "upload to https://backup:" + mcpPlanted[4] + "@files.example/x failed", Meta: map[string]any{},
		DirName: "2026-10-20_0100"}
	if err := st.CreateRun(f.run); err != nil {
		t.Fatal(err)
	}
	logPath := run.RunLogPath(f.run.ID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	logText := "2026-10-20 01:00:00 starting\n2026-10-20 01:00:01 Authorization: Bearer " + mcpPlanted[5] +
		"\n2026-10-20 01:00:02 rclone https://backup:" + mcpPlanted[4] + "@files.example/x\n2026-10-20 01:00:03 FAILED\n"
	if err := os.WriteFile(logPath, []byte(logText), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// token makes an API token of role's user with the given access.
func (f *mcpFixture) token(role, access string) string {
	f.t.Helper()
	raw := "mv_" + auth.RandomToken(30)
	tk := &store.APIToken{UserID: f.users[role].ID, Name: role + "-" + access, Prefix: raw[:10], Hash: auth.SHA256Hex(raw), Access: access}
	if err := f.st.CreateToken(tk); err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func (f *mcpFixture) post(token, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = "192.0.2.10:50000"
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (f *mcpFixture) rpc(token, method string, params any) rpcReply {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	rec := f.post(token, string(b), nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		f.t.Fatalf("%s: HTTP %d %s", method, rec.Code, rec.Body)
	}
	f.checkNoSecrets(rec.Body.String())
	var out rpcReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || string(out.ID) != "7" {
		f.t.Fatalf("%s: bad reply %s (%v)", method, rec.Body, err)
	}
	return out
}

func (f *mcpFixture) checkNoSecrets(body string) {
	f.t.Helper()
	for _, sec := range mcpPlanted {
		if strings.Contains(body, sec) {
			f.t.Fatalf("an MCP answer contains the secret %q:\n%s", sec, body)
		}
	}
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Structured map[string]any `json:"structuredContent"`
	IsError    bool           `json:"isError"`
}

func (f *mcpFixture) call(token, tool string, args map[string]any) toolResult {
	f.t.Helper()
	r := f.rpc(token, "tools/call", map[string]any{"name": tool, "arguments": args})
	if r.Error != nil {
		f.t.Fatalf("%s: JSON-RPC error %d %s", tool, r.Error.Code, r.Error.Message)
	}
	var tr toolResult
	if err := json.Unmarshal(r.Result, &tr); err != nil || len(tr.Content) != 1 || tr.Content[0].Type != "text" {
		f.t.Fatalf("%s: bad tool result %s", tool, r.Result)
	}
	return tr
}

func (f *mcpFixture) toolNames(token string) []string {
	f.t.Helper()
	r := f.rpc(token, "tools/list", map[string]any{})
	var out struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if r.Error != nil || json.Unmarshal(r.Result, &out) != nil {
		f.t.Fatalf("tools/list: %s %v", r.Result, r.Error)
	}
	var names []string
	for _, t := range out.Tools {
		if t.Description == "" || t.InputSchema["type"] != "object" {
			f.t.Fatalf("tool %s: no description or schema", t.Name)
		}
		names = append(names, t.Name)
	}
	return names
}

// toolsBy lists the tools that start something (full access) and those that change the
// configuration (Manage access).
func toolsBy() (writes, manages []string) {
	for _, tl := range mcpTools() {
		switch {
		case tl.Manage:
			manages = append(manages, tl.Name)
		case tl.Write:
			writes = append(writes, tl.Name)
		}
	}
	return writes, manages
}

func TestMCPToolList(t *testing.T) {
	f := newMCPFixture(t)
	nameRe := regexp.MustCompile(`^[a-z_]{1,64}$`)
	seen := map[string]bool{}
	for _, tool := range mcpTools() {
		if !nameRe.MatchString(tool.Name) || seen[tool.Name] {
			t.Fatalf("tool name %q is invalid or used twice", tool.Name)
		}
		seen[tool.Name] = true
		if _, ok := f.s.routeRoles[tool.Route]; !ok {
			t.Fatalf("tool %s names the route %q, which does not exist", tool.Name, tool.Route)
		}
		method, _, _ := strings.Cut(tool.Route, " ")
		if tool.Write != (method != http.MethodGet && !readOnlyPOST[tool.Route]) {
			t.Fatalf("tool %s: Write does not match its route %s", tool.Name, tool.Route)
		}
		if tool.Manage && !tool.Write {
			t.Fatalf("tool %s changes the configuration but is not a write tool", tool.Name)
		}
	}
	all := len(mcpTools())
	writeTools, manageTools := toolsBy()
	if len(manageTools) != 8 || !slices.Contains(writeTools, "test_storage") {
		t.Fatalf("write tools %v, manage tools %v", writeTools, manageTools)
	}

	manage := f.toolNames(f.token(store.RoleOperator, store.TokenManage))
	if len(manage) != all {
		t.Fatalf("an operator with a Manage token sees %d of %d tools: %v", len(manage), all, manage)
	}
	full := f.toolNames(f.token(store.RoleOperator, store.TokenFull))
	if len(full) != all-len(manageTools) || slices.Contains(full, "create_job") || !slices.Contains(full, "run_backup") {
		t.Fatalf("an operator with a full token sees %d of %d tools: %v", len(full), all, full)
	}
	readOnly := f.toolNames(f.token(store.RoleAdmin, store.TokenRead))
	viewer := f.toolNames(f.token(store.RoleViewer, store.TokenManage))
	for _, w := range append(writeTools, manageTools...) {
		if slices.Contains(readOnly, w) || slices.Contains(viewer, w) {
			t.Fatalf("%s is offered to a read-only token or a viewer", w)
		}
	}
	if !slices.Contains(readOnly, "plan_pitr") || slices.Contains(viewer, "plan_pitr") {
		t.Fatalf("plan_pitr (operator, changes nothing): read-only admin %v, viewer %v", readOnly, viewer)
	}
	if !slices.Contains(readOnly, "preview_schedule") || !slices.Contains(viewer, "list_storages") {
		t.Fatalf("read tools: read-only admin %v, viewer %v", readOnly, viewer)
	}
	writes := len(writeTools) + len(manageTools)
	if len(readOnly) != all-writes || len(viewer) != all-writes-1 {
		t.Fatalf("read-only %d, viewer %d of %d tools", len(readOnly), len(viewer), all)
	}

	// the protocol around the tools
	tok := f.token(store.RoleViewer, store.TokenRead)
	r := f.rpc(tok, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}})
	var init struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      map[string]any `json:"serverInfo"`
		Instructions    string         `json:"instructions"`
	}
	if r.Error != nil || json.Unmarshal(r.Result, &init) != nil || init.ProtocolVersion != "2025-06-18" ||
		init.Capabilities["tools"] == nil || init.ServerInfo["name"] != "dumpcat" || !strings.Contains(init.Instructions, "read-only") {
		t.Fatalf("initialize: %s %v", r.Result, r.Error)
	}
	r = f.rpc(tok, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if json.Unmarshal(r.Result, &init) != nil || init.ProtocolVersion != mcpVersions[0] {
		t.Fatalf("unknown version: %s", r.Result)
	}
	if r := f.rpc(tok, "ping", nil); r.Error != nil || string(r.Result) != "{}" {
		t.Fatalf("ping: %s %v", r.Result, r.Error)
	}
	if r := f.rpc(tok, "resources/list", nil); r.Error == nil || r.Error.Code != rpcMethodNotFound {
		t.Fatalf("unknown method: %s %v", r.Result, r.Error)
	}
	if r := f.rpc(tok, "tools/call", map[string]any{"name": "drop_everything"}); r.Error == nil || r.Error.Code != rpcInvalidParams {
		t.Fatalf("unknown tool: %s %v", r.Result, r.Error)
	}
	if rec := f.post(tok, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("notification: %d %q", rec.Code, rec.Body)
	}
	if rec := f.post(tok, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, nil); !strings.Contains(rec.Body.String(), `"code":-32600`) {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body)
	}
	if rec := f.post(tok, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "2000-01-01"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported protocol version header: %d", rec.Code)
	}
	if rec := f.post(tok, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "2025-11-25"}); rec.Code != 200 {
		t.Fatalf("protocol version header: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET (no SSE stream): %d", rec.Code)
	}
}

func TestMCPAuth(t *testing.T) {
	f := newMCPFixture(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if rec := f.post("", ping, nil); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := f.post("mv_not-a-real-token-at-all", ping, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	// a signed-in browser's session cookie does not work: tokens only
	cookie := auth.RandomToken(32)
	if err := f.st.CreateSession(&store.Session{ID: auth.SHA256Hex(cookie), UserID: f.users[store.RoleAdmin].ID, CSRF: "csrf",
		CreatedAt: time.Now(), LastSeenAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if rec := f.post("", ping, map[string]string{"Cookie": sessionCookie + "=" + cookie, "X-CSRF-Token": "csrf"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session cookie: %d", rec.Code)
	}
	tok := f.token(store.RoleOperator, store.TokenFull)
	if rec := f.post(tok, ping, map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", rec.Code)
	}
	if rec := f.post(tok, ping, map[string]string{"Origin": "http://example.com"}); rec.Code != 200 {
		t.Fatalf("same origin (httptest host example.com): %d %s", rec.Code, rec.Body)
	}
	// an expired token, a token of a locked account
	raw := "mv_" + auth.RandomToken(30)
	past := time.Now().Add(-time.Hour)
	if err := f.st.CreateToken(&store.APIToken{UserID: f.users[store.RoleOperator].ID, Name: "old", Prefix: raw[:10], Hash: auth.SHA256Hex(raw),
		Access: store.TokenFull, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if rec := f.post(raw, ping, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", rec.Code)
	}
	viewerTok := f.token(store.RoleViewer, store.TokenFull)
	u := f.users[store.RoleViewer]
	u.Disabled = true
	if err := f.st.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	if rec := f.post(viewerTok, ping, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("locked account: %d", rec.Code)
	}
}

func TestMCPRoleRefusal(t *testing.T) {
	f := newMCPFixture(t)
	runs := func() int {
		_, n, err := f.st.ListRuns(store.RunFilter{JobID: f.job.ID})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := runs()
	args := map[string]any{"job_id": f.job.ID}
	// a viewer cannot run a backup, even when calling a tool tools/list did not offer
	res := f.call(f.token(store.RoleViewer, store.TokenFull), "run_backup", args)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "requires role operator") {
		t.Fatalf("viewer: %+v", res)
	}
	// nor can an admin's read-only token
	readTok := f.token(store.RoleAdmin, store.TokenRead)
	for tool, a := range map[string]map[string]any{"run_backup": args, "verify_backup": {"backup_id": f.run.ID}, "start_drill": {"drill_id": 1}} {
		res = f.call(readTok, tool, a)
		if !res.IsError || !strings.Contains(res.Content[0].Text, "read-only") {
			t.Fatalf("read-only token, %s: %+v", tool, res)
		}
	}
	// the same rule over plain REST: the token may look, not change anything or download backups
	rest := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+readTok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := rest(http.MethodPost, fmt.Sprintf("/api/jobs/%d/run", f.job.ID), "{}"); c != http.StatusForbidden {
		t.Fatalf("REST run with a read-only token: %d", c)
	}
	if c := rest(http.MethodGet, fmt.Sprintf("/api/runs/%d/download", f.run.ID), ""); c != http.StatusForbidden {
		t.Fatalf("REST download with a read-only token: %d", c)
	}
	if c := rest(http.MethodPost, "/api/me/tokens", `{"name":"x","access":"full","password":"correct horse admin"}`); c != http.StatusForbidden {
		t.Fatalf("a read-only token made another token: %d", c)
	}
	if c := rest(http.MethodGet, "/api/runs", ""); c != 200 {
		t.Fatalf("REST list with a read-only token: %d", c)
	}
	if c := rest(http.MethodPost, "/api/pitr/plan", `{"job_id":999999,"target":{"type":"latest"}}`); c == http.StatusForbidden || c == 200 {
		t.Fatalf("PITR preview with a read-only token should reach the handler (and fail there): %d", c)
	}
	if n := runs(); n != before {
		t.Fatalf("a refused call started a backup: %d → %d", before, n)
	}
	// wrong arguments are a tool error the model can correct
	res = f.call(f.token(store.RoleOperator, store.TokenFull), "run_backup", map[string]any{"job": 1})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "unknown field") {
		t.Fatalf("unknown argument: %+v", res)
	}
	res = f.call(readTok, "list_backups", map[string]any{"search": "a\nb"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "control character") {
		t.Fatalf("control character: %+v", res)
	}
}

func TestMCPRunBackup(t *testing.T) {
	f := newMCPFixture(t)
	res := f.call(f.token(store.RoleOperator, store.TokenFull), "run_backup", map[string]any{"job_id": f.job.ID})
	if res.IsError {
		t.Fatalf("run_backup: %s", res.Content[0].Text)
	}
	backup, _ := res.Structured["backup"].(map[string]any)
	idF, _ := backup["id"].(float64)
	id := int64(idF)
	if res.Structured["started"] != true || id <= 0 || id == f.run.ID {
		t.Fatalf("run_backup result: %s", res.Content[0].Text)
	}
	run, err := f.st.GetRun(id)
	if err != nil || run.TriggeredBy != f.users[store.RoleOperator].Username || run.Trigger != "manual" {
		t.Fatalf("the new backup: %+v %v", run, err)
	}
	// the REST handler wrote the audit entry, as the token's user, from the MCP client's address
	entries, _, err := f.st.ListAudit(20, 0, "job_run")
	if err != nil || len(entries) != 1 || entries[0].Username != f.users[store.RoleOperator].Username || entries[0].IP != "192.0.2.10" {
		t.Fatalf("audit: %+v %v", entries, err)
	}
	// nothing listens on the server's port: the backup fails, and get_backup / get_log say why
	deadline := time.Now().Add(30 * time.Second)
	for f.s.Runner.IsJobRunning(f.job.ID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	tok := f.token(store.RoleViewer, store.TokenRead)
	got := f.call(tok, "get_backup", map[string]any{"backup_id": id})
	r, _ := got.Structured["run"].(map[string]any)
	if got.IsError || r["status"] != store.StatusFailed || r["error"] == "" {
		t.Fatalf("get_backup after the failure: %s", got.Content[0].Text)
	}
	lg := f.call(tok, "get_log", map[string]any{"kind": "backup", "id": id, "lines": 50})
	if lg.IsError || lg.Structured["log"] == "" {
		t.Fatalf("get_log: %s", lg.Content[0].Text)
	}
}

func TestMCPRedaction(t *testing.T) {
	f := newMCPFixture(t)
	tok := f.token(store.RoleAdmin, store.TokenFull)
	calls := map[string]map[string]any{
		"get_overview":    {},
		"get_monitoring":  {},
		"list_servers":    {},
		"get_server":      {"server_id": f.sv.ID},
		"list_jobs":       {},
		"get_job":         {"job_id": f.job.ID},
		"list_backups":    {},
		"get_backup":      {"backup_id": f.run.ID},
		"get_log":         {"kind": "backup", "id": f.run.ID},
		"list_restores":   {},
		"list_drills":     {},
		"list_drill_runs": {},
	}
	for tool, args := range calls {
		res := f.call(tok, tool, args) // f.rpc checks every answer for the planted secrets
		if res.IsError {
			t.Fatalf("%s: %s", tool, res.Content[0].Text)
		}
	}
	// what stays: the facts a model needs
	srv := f.call(tok, "get_server", map[string]any{"server_id": f.sv.ID}).Structured["server"].(map[string]any)
	if srv["has_password"] != true || srv["username"] != "backup" {
		t.Fatalf("server: %v", srv)
	}
	job := f.call(tok, "get_job", map[string]any{"job_id": f.job.ID}).Structured["job"].(map[string]any)
	if job["encrypt"] != true || job["has_passphrase"] != true || asMap(job["options"])["pre_command"] != "[redacted]" {
		t.Fatalf("job: %v", job)
	}
	lg := f.call(tok, "get_log", map[string]any{"kind": "backup", "id": f.run.ID}).Structured["log"].(string)
	if !strings.Contains(lg, "FAILED") || !strings.Contains(lg, "Bearer [redacted]") || !strings.Contains(lg, "backup:[redacted]@files.example") {
		t.Fatalf("log: %s", lg)
	}

	if got := mcpScrub("mysqldump --password=hunter2 PGPASSWORD=x1 postgres://u:p4ss@h/db AGE-SECRET-KEY-1QQQQ"); strings.Contains(got, "hunter2") ||
		strings.Contains(got, "x1") || strings.Contains(got, "p4ss") || strings.Contains(got, "QQQQ") {
		t.Fatalf("scrub: %s", got)
	}
	red := mcpRedact("", map[string]any{"config": map[string]any{"webhook_url": "https://h/x", "secret": "s", "headers": map[string]any{"A": "b"},
		"url": "https://ok.example"}, "has_password": true}).(map[string]any)
	if c := asMap(red["config"]); c["webhook_url"] != "[redacted]" || c["secret"] != "[redacted]" || c["headers"] != "[redacted]" ||
		c["url"] != "https://ok.example" || red["has_password"] != true {
		t.Fatalf("redact: %v", red)
	}
}

func TestTokenAccess(t *testing.T) {
	f := newMCPFixture(t)
	full := f.token(store.RoleOperator, store.TokenFull)
	create := func(body string) (int, store.APIToken) {
		req := httptest.NewRequest(http.MethodPost, "/api/me/tokens", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+full)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		var out struct {
			Item store.APIToken `json:"item"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Item
	}
	if c, tk := create(`{"name":"assistant","password":"correct horse operator"}`); c != 200 || tk.Access != store.TokenRead {
		t.Fatalf("default access: %d %+v", c, tk)
	}
	if c, tk := create(`{"name":"ci","access":"full","password":"correct horse operator"}`); c != 200 || tk.Access != store.TokenFull {
		t.Fatalf("full access: %d %+v", c, tk)
	}
	if c, tk := create(`{"name":"assistant-setup","access":"manage","password":"correct horse operator"}`); c != 200 || tk.Access != store.TokenManage {
		t.Fatalf("manage access: %d %+v", c, tk)
	}
	if c, _ := create(`{"name":"x","access":"admin","password":"correct horse operator"}`); c != http.StatusBadRequest {
		t.Fatalf("invalid access: %d", c)
	}
	if c, _ := create(`{"name":"a\nb","password":"correct horse operator"}`); c != http.StatusBadRequest {
		t.Fatalf("control character in the name: %d", c)
	}
	list, err := f.st.ListTokens(f.users[store.RoleOperator].ID)
	if err != nil || len(list) != 4 {
		t.Fatalf("tokens: %+v %v", list, err)
	}
	// unknown values fail closed; only manage manages
	if (&store.APIToken{Access: ""}).ReadOnly() != true || (&store.APIToken{Access: "write"}).ReadOnly() != true {
		t.Fatal("an unknown access value must be read-only")
	}
	if (&store.APIToken{Access: store.TokenManage}).ReadOnly() || !(&store.APIToken{Access: store.TokenManage}).Manages() ||
		(&store.APIToken{Access: store.TokenFull}).Manages() || (&store.APIToken{Access: "Manage"}).Manages() {
		t.Fatal("manage: full access that also manages, and nothing else manages")
	}
}
