package api

// MCP (Model Context Protocol) lets AI assistants (Claude Desktop, Claude Code, other MCP
// clients) look at a running DumpCat: POST /api/mcp is a stateless streamable-HTTP endpoint that
// answers each JSON-RPC message with one application/json response (no sessions, no SSE).
// "dumpcat mcp" bridges clients that start a local stdio server to it.
//
// MCP has no access of its own. It takes API tokens only (Bearer, never the session cookie), and
// every tool runs REST API requests in-process with the caller's token (apiCall): the same route,
// authentication, role check, read-only token check, handler and secret-free views as an HTTP
// client gets, and the same audit log entries. Tools only trim what the API returns for a model,
// and scrub it once more for anything that looks like a secret (mcpRedact).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"dumpcat/internal/drivers"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

// mcpVersions are the protocol versions this endpoint speaks, newest first.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// mcpNoStream answers GET (a server-initiated stream) and DELETE (end of a session): neither exists.
func mcpNoStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodPost)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "the MCP endpoint takes POST requests only"})
}

func (s *Server) mcpEndpoint(w http.ResponseWriter, r *http.Request) error {
	// a page of another site must not reach it, e.g. through DNS rebinding; MCP clients send no
	// Origin, or this server's
	if o := r.Header.Get("Origin"); o != "" && !s.sameOrigin(o, r) {
		return errf(403, "cross-origin requests are not allowed")
	}
	// API tokens only: a browser's session cookie must not work here
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="DumpCat"`)
		return errf(401, "an API token is required: Authorization: Bearer <token> (API & MCP → New token)")
	}
	a, err := s.authenticate(w, r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="DumpCat", error="invalid_token"`)
		return err
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcpVersions, v) {
		return badRequest("unsupported MCP protocol version %q (supported: %s)", v, strings.Join(mcpVersions, ", "))
	}
	var raw json.RawMessage
	if err := readJSON(r, &raw); err != nil {
		return err
	}
	reply := func(id json.RawMessage, result any, e *rpcError) error {
		if id == nil {
			id = json.RawMessage("null")
		}
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
		return nil
	}
	if b := bytes.TrimSpace(raw); len(b) > 0 && b[0] == '[' {
		return reply(nil, nil, &rpcError{rpcInvalidRequest, "JSON-RPC batches are not supported: send one message per request"})
	}
	var m rpcMessage
	if err := json.Unmarshal(raw, &m); err != nil || m.JSONRPC != "2.0" {
		return reply(nil, nil, &rpcError{rpcInvalidRequest, "not a JSON-RPC 2.0 message"})
	}
	if m.Method == "" || len(m.ID) == 0 {
		// a notification, or the client's answer to a request: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return nil
	}
	if c := m.ID[0]; c != '"' && c != '-' && (c < '0' || c > '9') {
		return reply(nil, nil, &rpcError{rpcInvalidRequest, "the id must be a string or a number"})
	}
	result, e := s.mcpDispatch(r, a, &m)
	return reply(m.ID, result, e)
}

// sameOrigin tells whether a browser's Origin is this server, as the request reached it, the
// address a reverse proxy forwarded, or the public URL in Settings.
func (s *Server) sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	hosts := []string{r.Host}
	if s.Cfg != nil && s.Cfg.TrustProxy {
		hosts = append(hosts, r.Header.Get("X-Forwarded-Host"))
	}
	if b, err := url.Parse(s.baseURL()); err == nil {
		hosts = append(hosts, b.Host)
	}
	for _, h := range hosts {
		if h != "" && strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

func (s *Server) mcpDispatch(r *http.Request, a *authInfo, m *rpcMessage) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "dumpcat", "title": "DumpCat", "version": s.Version},
			"instructions":    s.mcpInstructions(a),
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := []map[string]any{}
		for _, t := range mcpTools() {
			if s.mcpVisible(a, &t) {
				list = append(list, t.describe())
			}
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		return s.mcpCallTool(r, a, m.Params)
	}
	return nil, &rpcError{rpcMethodNotFound, "method not found: " + m.Method}
}

func (s *Server) mcpInstructions(a *authInfo) string {
	var engines []string
	for _, d := range drivers.All() {
		engines = append(engines, d.Label())
	}
	access := "with full access: you can also start backups, verifications and restore drills (ask the user first)"
	switch {
	case a.Token != nil && a.Token.ReadOnly():
		access = "with a read-only token: you can see what the account can see and change nothing"
	case store.RoleLevel(a.User.Role) < store.RoleLevel(store.RoleOperator):
		access = "as a viewer: you can look and change nothing"
	case mcpManages(a):
		access = "with Manage access: you can also start backups, verifications and restore drills, and set up and change jobs, " +
			"restore drills, database servers and storages (ask the user first)"
	}
	return fmt.Sprintf(`DumpCat %s backs up databases (%s) on schedules to local and cloud storage, archives their logs (MySQL binlogs, PostgreSQL WAL) for point-in-time recovery, and tests backups with restore drills.
Start with get_overview, and get_monitoring for what needs attention. A backup is one run of a job: list_backups and get_backup show backups, get_log shows the end of a log (backups, restores, drill runs, log archiving). To investigate a failure, read its error, then its log.
Signed in as %s (role %s) %s. Restores, deletions and DumpCat's settings are not available over MCP: they are done in the web UI. Secrets (passwords, keys, passphrases) are never returned or taken: when a setup needs one, give the user the finish_url the tool returns, where they enter it.`,
		s.Version, strings.Join(engines, ", "), a.User.Username, a.User.Role, access)
}

// ---- tools ----

type mcpTool struct {
	Name        string
	Title       string
	Description string
	// Route is the REST route the tool calls: its role and the read-only token rule decide who
	// sees the tool in tools/list. Calls are checked by the routes themselves.
	Route       string
	Write       bool // starts or changes something
	Destructive bool // may remove something (retention after a backup)
	// Manage changes the configuration (mcp_manage.go): only for tokens with Manage access,
	// which the route cannot tell from full access
	Manage bool
	Schema map[string]any
	Run    func(c *mcpCall, args json.RawMessage) (any, error)
}

func (t *mcpTool) describe() map[string]any {
	return map[string]any{
		"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": t.Schema,
		"annotations": map[string]any{
			"title": t.Title, "readOnlyHint": !t.Write, "destructiveHint": t.Destructive, "idempotentHint": !t.Write, "openWorldHint": false,
		},
	}
}

// mcpVisible tells whether tools/list offers the tool to the caller: what its route allows.
func (s *Server) mcpVisible(a *authInfo, t *mcpTool) bool {
	role, ok := s.routeRoles[t.Route]
	if !ok || store.RoleLevel(a.User.Role) < store.RoleLevel(role) || (t.Manage && !mcpManages(a)) {
		return false
	}
	return a.Token == nil || !a.Token.ReadOnly() || readOnlyAllows(t.Route)
}

// mcpManages tells whether the caller may use the tools that change the configuration.
func mcpManages(a *authInfo) bool { return a.Token != nil && a.Token.Manages() }

func mcpToolNamed(name string) *mcpTool {
	for _, t := range mcpTools() {
		if t.Name == name {
			return &t
		}
	}
	return nil
}

func (s *Server) mcpCallTool(r *http.Request, a *authInfo, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{rpcInvalidParams, "tools/call needs the name of a tool"}
	}
	t := mcpToolNamed(p.Name)
	if t == nil {
		return nil, &rpcError{rpcInvalidParams, "unknown tool: " + p.Name}
	}
	if t.Manage && !mcpManages(a) {
		// the routes take any full-access token: this is the one check MCP makes itself
		return mcpToolError(p.Name + " changes DumpCat's configuration: it needs an API token with Manage access (API & MCP → New token)"), nil
	}
	args := bytes.TrimSpace(p.Arguments)
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	if err := mcpCheckArgs(args); err != nil {
		return mcpToolError(err.Error()), nil
	}
	out, err := t.Run(&mcpCall{s: s, r: r}, args)
	if err != nil {
		return mcpToolError(mcpScrub(err.Error())), nil
	}
	out = mcpRedact("", out)
	res := map[string]any{"content": []any{map[string]any{"type": "text", "text": mcpJSON(out)}}}
	if m, ok := out.(map[string]any); ok {
		res["structuredContent"] = m
	}
	return res, nil
}

func mcpToolError(msg string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": msg}}, "isError": true}
}

func mcpJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}

// mcpCheckArgs refuses control characters in string arguments, as the API does in names.
func mcpCheckArgs(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("the arguments are not valid JSON: %v", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return errors.New("the arguments must be a JSON object")
	}
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case string:
			if strings.ContainsFunc(x, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				return errors.New("an argument contains a line break or another control character")
			}
		case map[string]any:
			for _, e := range x {
				if err := walk(e); err != nil {
					return err
				}
			}
		case []any:
			for _, e := range x {
				if err := walk(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(v)
}

// mcpRun decodes a tool's arguments into its input type (unknown fields are an error the model can fix).
func mcpRun[T any](f func(c *mcpCall, in T) (any, error)) func(*mcpCall, json.RawMessage) (any, error) {
	return func(c *mcpCall, raw json.RawMessage) (any, error) {
		var in T
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("invalid arguments: %s", strings.TrimPrefix(err.Error(), "json: "))
		}
		return f(c, in)
	}
}

// ---- in-process API calls ----

// mcpCall is one tool call: its API requests carry the MCP request's token and client address.
type mcpCall struct {
	s *Server
	r *http.Request
}

func (c *mcpCall) get(path string, q url.Values) (any, error) {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(http.MethodGet, path, nil)
}

func (c *mcpCall) post(path string, body any) (any, error) {
	if body == nil {
		body = map[string]any{}
	}
	return c.do(http.MethodPost, path, body)
}

func (c *mcpCall) do(method, target string, body any) (any, error) {
	code, b := c.s.apiCall(c.r, method, target, body)
	if code < 200 || code > 299 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = http.StatusText(code)
		}
		path, _, _ := strings.Cut(target, "?")
		return nil, fmt.Errorf("%s (%s %s)", e.Error, method, path)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("unreadable answer from %s %s: %v", method, target, err)
	}
	return v, nil
}

const mcpMaxAnswer = 16 << 20

// apiCall runs one REST API request in-process, as the client of r: its token, address and
// proxy headers go along, so the route authenticates and checks it exactly as over HTTP.
func (s *Server) apiCall(r *http.Request, method, target string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 500, []byte(`{"error":"cannot encode the request"}`)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, target, rd)
	if err != nil || s.api == nil {
		return 500, []byte(`{"error":"cannot build the request"}`)
	}
	for _, h := range []string{"Authorization", "User-Agent", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Proto"} {
		for _, v := range r.Header.Values(h) {
			req.Header.Add(h, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr, req.Host, req.TLS = r.RemoteAddr, r.Host, r.TLS
	rec := &mcpRecorder{header: http.Header{}}
	s.api.ServeHTTP(rec, req)
	if rec.tooLarge {
		return 500, []byte(`{"error":"the answer is too large"}`)
	}
	if rec.code == 0 {
		rec.code = http.StatusOK
	}
	return rec.code, rec.body.Bytes()
}

type mcpRecorder struct {
	header   http.Header
	code     int
	body     bytes.Buffer
	tooLarge bool
}

func (w *mcpRecorder) Header() http.Header { return w.header }

func (w *mcpRecorder) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *mcpRecorder) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.body.Len()+len(p) > mcpMaxAnswer {
		w.tooLarge = true
		return 0, errors.New("answer too large")
	}
	return w.body.Write(p)
}

// ---- redaction ----

// Keys whose values never go to a model, whatever a view returns: the API leaves secrets out
// already, this is the second line. Pre/post commands may carry credentials of their own.
var mcpSecretKey = regexp.MustCompile(`(?i)(^|_)(password|passphrase|secret|token|private_key|preshared_key|api_key|access_key|secret_key|key_hash|token_hash|webhook_url|authorization|cookie|headers|pre_command|post_command)$`)

var mcpScrubs = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s/:@]+):[^\s/@]+@`), "$1:[redacted]@"}, // user:password@ in a URL
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[a-z0-9._~+/=_-]{8,}`), "$1 [redacted]"},
	{regexp.MustCompile(`\bmv_[A-Za-z0-9_-]{16,}`), "mv_[redacted]"}, // DumpCat API tokens
	{regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]+`), "AGE-SECRET-KEY-[redacted]"},
	{regexp.MustCompile(`(?i)(--password|--passphrase|\bPGPASSWORD|\bMYSQL_PWD|\bDUMPCAT_PASSPHRASE|\bDUMPCAT_MASTER_KEY|\bDUMPCAT_MCP_TOKEN)(=|:\s*)\S+`), "$1$2[redacted]"},
}

// mcpScrub masks what looks like a credential in free text (logs, error messages).
func mcpScrub(s string) string {
	for _, x := range mcpScrubs {
		s = x.re.ReplaceAllString(s, x.repl)
	}
	return s
}

// mcpRedact returns v with secret-looking keys masked and every string scrubbed.
func mcpRedact(key string, v any) any {
	secret := key != "" && mcpSecretKey.MatchString(key)
	switch x := v.(type) {
	case string:
		if secret && x != "" {
			return "[redacted]"
		}
		return mcpScrub(x)
	case map[string]any:
		if secret && len(x) > 0 {
			return "[redacted]"
		}
		for k, e := range x {
			x[k] = mcpRedact(k, e)
		}
		return x
	case []map[string]any:
		for i := range x {
			x[i] = mcpRedact("", x[i]).(map[string]any)
		}
		return x
	case []any:
		if secret && len(x) > 0 {
			return "[redacted]"
		}
		for i, e := range x {
			x[i] = mcpRedact("", e)
		}
		return x
	}
	return v
}

// ---- shaping: what a model needs, not every field of the views ----

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asList(v any) []any         { l, _ := v.([]any); return l }

// pick copies the keys of m that hold something.
func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		switch v := m[k]; v {
		case nil, "":
		default:
			out[k] = v
		}
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… (cut)"
}

func briefRun(v any) map[string]any {
	m := asMap(v)
	out := pick(m, "id", "job_id", "job_name", "server_name", "method", "type", "status", "trigger", "triggered_by",
		"started_at", "finished_at", "size_bytes", "encrypted", "verify_status", "verified_at", "drill_status", "drilled_at", "parent_id")
	for _, k := range []string{"error", "verify_msg", "drill_msg"} {
		if e, _ := m[k].(string); e != "" {
			out[k] = clip(e, 1000)
		}
	}
	if dbs := asList(m["databases"]); len(dbs) > 0 {
		names := []any{}
		for _, d := range dbs {
			names = append(names, asMap(d)["name"])
		}
		out["databases"] = names
	}
	if locs := asList(m["locations"]); len(locs) > 0 {
		l := []any{}
		for _, x := range locs {
			l = append(l, pick(asMap(x), "id", "storage_name", "storage_type", "status", "error"))
		}
		out["locations"] = l
	}
	return out
}

func briefRuns(v any) []any {
	out := []any{}
	for _, r := range asList(v) {
		out = append(out, briefRun(r))
	}
	return out
}

func briefServer(v any) map[string]any {
	m := asMap(v)
	out := pick(m, "id", "engine", "name", "host", "port", "socket", "username", "tls_mode", "flavor", "version", "datadir",
		"service_name", "os_user", "notes", "agent_id", "wg_peer_id", "restore_username", "has_password", "has_restore_password",
		"created_at", "updated_at")
	if ca, _ := m["ca_cert"].(string); ca != "" {
		out["has_ca_cert"] = true
	}
	return out
}

// briefJob keeps a job view (job, running, last_run, next_run, last_drill) without its uuid.
func briefJob(v any) map[string]any {
	m := asMap(v)
	out := pick(m, "running", "last_run", "next_run", "last_drill")
	if j := asMap(m["job"]); j != nil {
		delete(j, "uuid")
		out["job"] = j
	}
	return out
}

// ---- schemas ----

func mcpObject(required []string, props map[string]any) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func mcpID(desc string) map[string]any {
	return map[string]any{"type": "integer", "minimum": 1, "description": desc}
}

func mcpInt(desc string, min, max, def int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max, "default": def, "description": desc}
}

func mcpString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func mcpEnum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

// backup method ids of every driver ("mysqldump", "pg_dump", …)
func mcpMethods() []string {
	var ids []string
	for _, d := range drivers.All() {
		for _, m := range d.Methods() {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// point-in-time recovery target types of every engine that archives its log
func mcpPITRTargets() []string {
	set := map[string]bool{}
	for _, d := range drivers.All() {
		if la, ok := d.(engine.LogArchiver); ok {
			for _, t := range la.LogInfo().Targets {
				set[t] = true
			}
		}
	}
	if len(set) == 0 {
		return []string{"latest", "time"}
	}
	var out []string
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func idPath(format string, id int64) (string, error) {
	if id <= 0 {
		return "", errors.New("the id must be a positive number")
	}
	return fmt.Sprintf(format, id), nil
}

func addInt(q url.Values, k string, v int64) {
	if v > 0 {
		q.Set(k, strconv.FormatInt(v, 10))
	}
}

// clampInt keeps v within lo..hi; 0 (not given) is def.
func clampInt(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	return max(lo, min(v, hi))
}

var runStatuses = []string{store.StatusQueued, store.StatusRunning, store.StatusSuccess, store.StatusPartial,
	store.StatusFailed, store.StatusCanceled, store.StatusDeleted}

// mcpTools is the tool set. Read tools need what the matching GET route needs (viewer);
// run_backup, verify_backup, start_drill and test_storage need an operator and a token with full
// access; the tools of mcp_manage.go that change the configuration a token with Manage access.
func mcpTools() []mcpTool {
	return append(mcpBaseTools(), mcpSetupTools()...)
}

func mcpBaseTools() []mcpTool {
	return []mcpTool{
		{
			Name: "get_overview", Title: "DumpCat overview", Route: "GET /api/dashboard",
			Description: "Start here. A summary of this DumpCat: version; how many servers, jobs and backups; every job's last result, " +
				"last successful backup, freshness and next run; the tasks running now (backups, verifications, restores, drills) with " +
				"progress; the 12 most recent backups; and the current warnings (failed jobs, overdue backups, log archiving problems, " +
				"failed restore drills).",
			Schema: mcpObject(nil, map[string]any{}),
			Run: mcpRun(func(c *mcpCall, _ struct{}) (any, error) {
				d, err := c.get("/api/dashboard", nil)
				if err != nil {
					return nil, err
				}
				m := asMap(d)
				return map[string]any{"version": c.s.Version, "stats": m["stats"], "jobs": m["jobs"], "running_tasks": m["tasks"],
					"recent_backups": briefRuns(m["recent"]), "warnings": m["warnings"]}, nil
			}),
		},
		{
			Name: "get_monitoring", Title: "Monitoring status and alerts", Route: "GET /api/dashboard",
			Description: "Monitoring status and alerts: for every job, whether its newest successful backup is fresh or overdue (stale, " +
				"due_at: the same rule as DumpCat's freshness alerts and /metrics); the state of continuous log archiving (MySQL " +
				"binlogs, PostgreSQL WAL): streaming or failing, files waiting for upload, archive lag; and the active warnings. " +
				"Use it to answer \"is everything backed up?\".",
			Schema: mcpObject(nil, map[string]any{}),
			Run: mcpRun(func(c *mcpCall, _ struct{}) (any, error) {
				d, err := c.get("/api/dashboard", nil)
				if err != nil {
					return nil, err
				}
				m := asMap(d)
				jobs := []any{}
				for _, j := range asList(m["jobs"]) {
					jobs = append(jobs, pick(asMap(j), "id", "name", "enabled", "server_name", "method", "running", "last_status",
						"last_at", "last_success_at", "due_at", "stale", "next_at"))
				}
				out := map[string]any{"warnings": m["warnings"], "jobs": jobs}
				if l, err := c.get("/api/binlog", nil); err == nil {
					arch := []any{}
					for _, x := range asList(l) {
						arch = append(arch, pick(asMap(x), "job_id", "job_name", "server_name", "engine", "archive", "enabled", "status", "stats"))
					}
					out["log_archiving"] = arch
				}
				return out, nil
			}),
		},
		{
			Name: "list_servers", Title: "List database servers", Route: "GET /api/servers",
			Description: "The database servers DumpCat backs up or restores to: engine (mysql = MySQL, MariaDB, Percona; postgres = " +
				"PostgreSQL), address, user, TLS mode, detected version, and how DumpCat reaches them (directly, or through an agent " +
				"or WireGuard peer by id). Passwords are never returned (has_password tells whether one is saved).",
			Schema: mcpObject(nil, map[string]any{}),
			Run: mcpRun(func(c *mcpCall, _ struct{}) (any, error) {
				l, err := c.get("/api/servers", nil)
				if err != nil {
					return nil, err
				}
				out := []any{}
				for _, sv := range asList(l) {
					out = append(out, briefServer(sv))
				}
				return map[string]any{"servers": out}, nil
			}),
		},
		{
			Name: "get_server", Title: "Get a database server", Route: "GET /api/servers/{id}",
			Description: "One database server. With check_connection, DumpCat also connects to it now (a few seconds) and reports " +
				"whether it can, the version, databases with sizes, privileges, warnings, and which backup methods work from here — " +
				"the first thing to check when backups of a server fail to connect.",
			Schema: mcpObject([]string{"server_id"}, map[string]any{
				"server_id":        mcpID("Server id from list_servers."),
				"check_connection": map[string]any{"type": "boolean", "default": false, "description": "Connect to the server now and inspect it."},
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				ServerID        int64 `json:"server_id"`
				CheckConnection bool  `json:"check_connection"`
			}) (any, error) {
				p, err := idPath("/api/servers/%d", in.ServerID)
				if err != nil {
					return nil, err
				}
				sv, err := c.get(p, nil)
				if err != nil {
					return nil, err
				}
				out := map[string]any{"server": briefServer(sv)}
				if in.CheckConnection {
					info, err := c.get(p+"/info", nil)
					if err != nil {
						return nil, err
					}
					out["connection"] = info
				}
				return out, nil
			}),
		},
		{
			Name: "list_jobs", Title: "List backup jobs", Route: "GET /api/jobs",
			Description: "The backup jobs: server, method, databases, schedules (cron), compression, encryption, storage targets, " +
				"retention, options (verification, log archiving, max age), enabled or running, the last run, the next run and the " +
				"last restore drill.",
			Schema: mcpObject(nil, map[string]any{}),
			Run: mcpRun(func(c *mcpCall, _ struct{}) (any, error) {
				l, err := c.get("/api/jobs", nil)
				if err != nil {
					return nil, err
				}
				out := []any{}
				for _, j := range asList(l) {
					out = append(out, briefJob(j))
				}
				return map[string]any{"jobs": out}, nil
			}),
		},
		{
			Name: "get_job", Title: "Get a backup job", Route: "GET /api/jobs/{id}",
			Description: "One backup job with its configuration, whether it is running, its last run, next run and last restore drill.",
			Schema:      mcpObject([]string{"job_id"}, map[string]any{"job_id": mcpID("Job id from list_jobs.")}),
			Run: mcpRun(func(c *mcpCall, in struct {
				JobID int64 `json:"job_id"`
			}) (any, error) {
				p, err := idPath("/api/jobs/%d", in.JobID)
				if err != nil {
					return nil, err
				}
				j, err := c.get(p, nil)
				if err != nil {
					return nil, err
				}
				return briefJob(j), nil
			}),
		},
		{
			Name: "list_backups", Title: "List backups", Route: "GET /api/runs",
			Description: "Backups (each run of a job), newest first, with total for paging. status success or partial: a backup " +
				"that can be restored or verified; failed and canceled: error says why; queued and running: in progress. The id is " +
				"what get_backup, get_log, verify_backup and plan_pitr take.",
			Schema: mcpObject(nil, map[string]any{
				"job_id": mcpID("Only this job's backups."),
				"status": mcpEnum("Only backups with this status.", runStatuses...),
				"method": mcpEnum("Only this backup method.", mcpMethods()...),
				"search": mcpString("Text to find in the job, server or folder name, or the error."),
				"limit":  mcpInt("How many (1-200).", 1, 200, 20),
				"offset": mcpInt("How many to skip.", 0, 1000000, 0),
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				JobID  int64  `json:"job_id"`
				Status string `json:"status"`
				Method string `json:"method"`
				Search string `json:"search"`
				Limit  int    `json:"limit"`
				Offset int    `json:"offset"`
			}) (any, error) {
				if in.Status != "" && !slices.Contains(runStatuses, in.Status) {
					return nil, fmt.Errorf("status must be one of %s", strings.Join(runStatuses, ", "))
				}
				q := url.Values{}
				addInt(q, "job_id", in.JobID)
				if in.Status != "" {
					q.Set("status", in.Status)
				}
				if in.Method != "" {
					q.Set("method", in.Method)
				}
				if s := strings.TrimSpace(in.Search); s != "" {
					q.Set("q", clip(s, 200))
				}
				q.Set("limit", strconv.Itoa(clampInt(in.Limit, 1, 200, 20)))
				q.Set("offset", strconv.Itoa(max(in.Offset, 0)))
				l, err := c.get("/api/runs", q)
				if err != nil {
					return nil, err
				}
				m := asMap(l)
				return map[string]any{"total": m["total"], "backups": briefRuns(m["items"])}, nil
			}),
		},
		{
			Name: "get_backup", Title: "Get a backup", Route: "GET /api/runs/{id}",
			Description: "One backup in detail: status, error, timings, sizes, its databases and files, where it is stored and the " +
				"state of each copy, its verification and restore drill results, its incremental chain, and live progress while it runs.",
			Schema: mcpObject([]string{"backup_id"}, map[string]any{"backup_id": mcpID("Backup (run) id from list_backups.")}),
			Run: mcpRun(func(c *mcpCall, in struct {
				BackupID int64 `json:"backup_id"`
			}) (any, error) {
				p, err := idPath("/api/runs/%d", in.BackupID)
				if err != nil {
					return nil, err
				}
				v, err := c.get(p, nil)
				if err != nil {
					return nil, err
				}
				m := asMap(v)
				if run := asMap(m["run"]); run != nil {
					delete(run, "uuid")
					delete(run, "job_uuid")
					if files := asList(run["files"]); len(files) > 50 {
						run["files"], run["files_total"] = files[:50], len(files)
					}
				}
				return m, nil
			}),
		},
		{
			Name: "get_log", Title: "Read the end of a log", Route: "GET /api/runs/{id}/log",
			Description: "The last lines of a task's log, to see why something failed or how far it got. kind backup takes a backup " +
				"id, restore a restore id, drill_run a drill run id, log_archive a job id (its binlog/WAL archiver).",
			Schema: mcpObject([]string{"kind", "id"}, map[string]any{
				"kind":  mcpEnum("Whose log.", "backup", "restore", "drill_run", "log_archive"),
				"id":    mcpID("Id of the backup, restore, drill run or job."),
				"lines": mcpInt("How many lines from the end (1-2000).", 1, 2000, 200),
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				Kind  string `json:"kind"`
				ID    int64  `json:"id"`
				Lines int    `json:"lines"`
			}) (any, error) {
				format := map[string]string{"backup": "/api/runs/%d/log", "restore": "/api/restores/%d/log",
					"drill_run": "/api/drill-runs/%d/log", "log_archive": "/api/jobs/%d/binlog/log"}[in.Kind]
				if format == "" {
					return nil, errors.New("kind must be backup, restore, drill_run or log_archive")
				}
				p, err := idPath(format, in.ID)
				if err != nil {
					return nil, err
				}
				v, err := c.get(p, url.Values{"tail": {"1"}})
				if err != nil {
					return nil, err
				}
				m := asMap(v)
				data, _ := m["data"].(string)
				n := clampInt(in.Lines, 1, 2000, 200)
				lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
				num, _ := m["next"].(json.Number)
				next, _ := num.Int64()
				more := next > int64(len(data)) // the API read the end of a longer log
				if len(lines) > n {
					lines, more = lines[len(lines)-n:], true
				}
				text := strings.Join(lines, "\n")
				for len(text) > 100<<10 && len(lines) > 1 {
					lines, more = lines[1:], true
					text = strings.Join(lines, "\n")
				}
				if data == "" {
					text = "(the log is empty or does not exist)"
				}
				return map[string]any{"kind": in.Kind, "id": in.ID, "lines": len(lines), "earlier_lines_omitted": more, "log": text}, nil
			}),
		},
		{
			Name: "list_restores", Title: "List restores", Route: "GET /api/restores",
			Description: "Restores that were run from the web UI or API, newest first: backup, target server, status and error. " +
				"Restore drills are listed by list_drill_runs.",
			Schema: mcpObject(nil, map[string]any{
				"limit":  mcpInt("How many (1-200).", 1, 200, 20),
				"offset": mcpInt("How many to skip.", 0, 1000000, 0),
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
			}) (any, error) {
				l, err := c.get("/api/restores", url.Values{"limit": {strconv.Itoa(clampInt(in.Limit, 1, 200, 20))}, "offset": {strconv.Itoa(max(in.Offset, 0))}})
				if err != nil {
					return nil, err
				}
				m := asMap(l)
				return map[string]any{"total": m["total"], "restores": m["items"]}, nil
			}),
		},
		{
			Name: "get_restore", Title: "Get a restore", Route: "GET /api/restores/{id}",
			Description: "One restore: backup, target server, options, status, error, and live progress while it runs.",
			Schema:      mcpObject([]string{"restore_id"}, map[string]any{"restore_id": mcpID("Restore id from list_restores.")}),
			Run: mcpRun(func(c *mcpCall, in struct {
				RestoreID int64 `json:"restore_id"`
			}) (any, error) {
				p, err := idPath("/api/restores/%d", in.RestoreID)
				if err != nil {
					return nil, err
				}
				return c.get(p, nil)
			}),
		},
		{
			Name: "list_drills", Title: "List restore drills", Route: "GET /api/drills",
			Description: "Restore drills: tests that restore a job's backup into a sandbox server under temporary names and check " +
				"it (tables, row counts, SQL assertions, time to restore). Each with its schedule, last result, next run, and " +
				"configuration problems.",
			Schema: mcpObject(nil, map[string]any{"job_id": mcpID("Only this job's drills.")}),
			Run: mcpRun(func(c *mcpCall, in struct {
				JobID int64 `json:"job_id"`
			}) (any, error) {
				q := url.Values{}
				addInt(q, "job_id", in.JobID)
				l, err := c.get("/api/drills", q)
				if err != nil {
					return nil, err
				}
				return map[string]any{"drills": l}, nil
			}),
		},
		{
			Name: "list_drill_runs", Title: "List restore drill runs", Route: "GET /api/drill-runs",
			Description: "Results of restore drills, newest first: passed, failed or partial (temporary databases left behind), " +
				"with summary and error.",
			Schema: mcpObject(nil, map[string]any{
				"drill_id":  mcpID("Only this drill's runs."),
				"job_id":    mcpID("Only drills of this job."),
				"backup_id": mcpID("Only drills of this backup."),
				"limit":     mcpInt("How many (1-200).", 1, 200, 20),
				"offset":    mcpInt("How many to skip.", 0, 1000000, 0),
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				DrillID  int64 `json:"drill_id"`
				JobID    int64 `json:"job_id"`
				BackupID int64 `json:"backup_id"`
				Limit    int   `json:"limit"`
				Offset   int   `json:"offset"`
			}) (any, error) {
				q := url.Values{"limit": {strconv.Itoa(clampInt(in.Limit, 1, 200, 20))}, "offset": {strconv.Itoa(max(in.Offset, 0))}}
				addInt(q, "drill_id", in.DrillID)
				addInt(q, "job_id", in.JobID)
				addInt(q, "run_id", in.BackupID)
				l, err := c.get("/api/drill-runs", q)
				if err != nil {
					return nil, err
				}
				m := asMap(l)
				return map[string]any{"total": m["total"], "drill_runs": m["items"]}, nil
			}),
		},
		{
			Name: "get_drill_run", Title: "Get a restore drill run", Route: "GET /api/drill-runs/{id}",
			Description: "One restore drill run: every check with its result, restore and check durations, the databases, temporary " +
				"databases left behind, the error, and live progress while it runs.",
			Schema: mcpObject([]string{"drill_run_id"}, map[string]any{"drill_run_id": mcpID("Drill run id from list_drill_runs or start_drill.")}),
			Run: mcpRun(func(c *mcpCall, in struct {
				DrillRunID int64 `json:"drill_run_id"`
			}) (any, error) {
				p, err := idPath("/api/drill-runs/%d", in.DrillRunID)
				if err != nil {
					return nil, err
				}
				return c.get(p, nil)
			}),
		},
		{
			Name: "plan_pitr", Title: "Preview a point-in-time recovery", Route: "POST /api/pitr/plan",
			Description: "Preview a point-in-time recovery; nothing is changed or restored. DumpCat picks the base backup for the " +
				"target and checks that the archived log (MySQL binlogs, PostgreSQL WAL) reaches it, with warnings. Give job_id (or " +
				"backup_id for a given base). target_type latest or time (RFC 3339, e.g. 2026-10-20T09:30:00+07:00) work for every " +
				"engine; position (file and pos) and gtid for MySQL; lsn and name (a restore point) for PostgreSQL. The recovery " +
				"itself is started in the web UI. Needs the operator role.",
			Schema: mcpObject([]string{"target_type"}, map[string]any{
				"job_id":      mcpID("Job whose log is archived."),
				"backup_id":   mcpID("Use this backup as the base instead of the best one."),
				"target_type": mcpEnum("Where to recover to.", mcpPITRTargets()...),
				"time":        mcpString("For target_type time: RFC 3339 time."),
				"file":        mcpString("For target_type position (MySQL): binlog file name, e.g. mysql-bin.000012."),
				"pos":         map[string]any{"type": "integer", "minimum": 0, "description": "For target_type position (MySQL): position in the file."},
				"gtid":        mcpString("For target_type gtid (MySQL): GTID set or MariaDB GTID to stop at."),
				"lsn":         mcpString("For target_type lsn (PostgreSQL): WAL location, e.g. 0/3000148."),
				"name":        mcpString("For target_type name (PostgreSQL): restore point name."),
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				JobID      int64  `json:"job_id"`
				BackupID   int64  `json:"backup_id"`
				TargetType string `json:"target_type"`
				Time       string `json:"time"`
				File       string `json:"file"`
				Pos        int64  `json:"pos"`
				GTID       string `json:"gtid"`
				LSN        string `json:"lsn"`
				Name       string `json:"name"`
			}) (any, error) {
				if in.JobID <= 0 && in.BackupID <= 0 {
					return nil, errors.New("job_id or backup_id is required")
				}
				target := map[string]any{"type": in.TargetType}
				if in.Time != "" {
					t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.Time))
					if err != nil {
						return nil, errors.New("time must be an RFC 3339 time such as 2026-10-20T09:30:00Z")
					}
					target["time"] = t
				}
				for k, v := range map[string]string{"file": in.File, "gtid": in.GTID, "lsn": in.LSN, "name": in.Name} {
					if v != "" {
						target[k] = v
					}
				}
				if in.Pos > 0 {
					target["pos"] = in.Pos
				}
				return c.post("/api/pitr/plan", map[string]any{"job_id": in.JobID, "run_id": in.BackupID, "target": target})
			}),
		},
		{
			Name: "run_backup", Title: "Run a backup now", Route: "POST /api/jobs/{id}/run", Write: true, Destructive: true,
			Description: "Start a backup of a job now, like Run now in the web UI, and return the new backup (its id is for " +
				"get_backup and get_log; it runs in the background). type incremental is taken only by methods that have " +
				"incremental backups, otherwise a full backup runs. Afterwards the job's retention may remove old backups, as after " +
				"a scheduled run. Needs the operator role and a token with full access.",
			Schema: mcpObject([]string{"job_id"}, map[string]any{
				"job_id": mcpID("Job id from list_jobs."),
				"type":   mcpEnum("full (default) or incremental.", store.TypeFull, store.TypeIncremental),
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				JobID int64  `json:"job_id"`
				Type  string `json:"type"`
			}) (any, error) {
				p, err := idPath("/api/jobs/%d/run", in.JobID)
				if err != nil {
					return nil, err
				}
				if in.Type != "" && in.Type != store.TypeFull && in.Type != store.TypeIncremental {
					return nil, errors.New("type must be full or incremental")
				}
				v, err := c.post(p, map[string]any{"type": in.Type})
				if err != nil {
					return nil, err
				}
				return map[string]any{"started": true, "backup": briefRun(v)}, nil
			}),
		},
		{
			Name: "verify_backup", Title: "Verify a backup", Route: "POST /api/runs/{id}/verify", Write: true,
			Description: "Check a stored backup: read every copy (or one location) back from storage and compare each file's " +
				"checksum. Runs in the background; get_backup shows verify_status and verify_msg when it ends. Only successful or " +
				"partial backups. Needs the operator role and a token with full access.",
			Schema: mcpObject([]string{"backup_id"}, map[string]any{
				"backup_id":   mcpID("Backup (run) id from list_backups."),
				"location_id": mcpID("Only this storage location (locations[].id in get_backup); default: every copy."),
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				BackupID   int64 `json:"backup_id"`
				LocationID int64 `json:"location_id"`
			}) (any, error) {
				p, err := idPath("/api/runs/%d/verify", in.BackupID)
				if err != nil {
					return nil, err
				}
				if _, err := c.post(p, map[string]any{"location_id": in.LocationID}); err != nil {
					return nil, err
				}
				return map[string]any{"started": true, "backup_id": in.BackupID}, nil
			}),
		},
		{
			Name: "start_drill", Title: "Start a restore drill", Route: "POST /api/drills/{id}/run", Write: true,
			Description: "Run a restore drill now: restore a backup (the newest usable one, or backup_id) into the drill's sandbox " +
				"server under temporary names, run its checks, then drop the temporary databases. Nothing on the source server is " +
				"touched. Returns the drill run; follow it with get_drill_run and get_log. Needs the operator role and a token with " +
				"full access.",
			Schema: mcpObject([]string{"drill_id"}, map[string]any{
				"drill_id":  mcpID("Drill id from list_drills."),
				"backup_id": mcpID("Restore this backup instead of the newest one."),
			}),
			Run: mcpRun(func(c *mcpCall, in struct {
				DrillID  int64 `json:"drill_id"`
				BackupID int64 `json:"backup_id"`
			}) (any, error) {
				p, err := idPath("/api/drills/%d/run", in.DrillID)
				if err != nil {
					return nil, err
				}
				v, err := c.post(p, map[string]any{"run_id": in.BackupID})
				if err != nil {
					return nil, err
				}
				return map[string]any{"started": true, "drill_run": v}, nil
			}),
		},
	}
}
