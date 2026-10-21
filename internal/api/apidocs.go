package api

// The API reference in Markdown (GET /api/docs.md): what a script or an AI agent needs to call the
// REST API of this DumpCat, written by the server itself so that it lists exactly the routes it
// serves, with the role each one needs and whether a read-only token may call it. Every route
// registered with route() has an entry in apiSections: TestAPIReferenceCoversRoutes fails on a
// route without one, so a new route gets its documentation in the same change.

import (
	"fmt"
	"net/http"
	"strings"

	"dumpcat/internal/store"
)

type apiDoc struct {
	Route string // the pattern given to route(), e.g. "GET /api/jobs/{id}"
	Doc   string // what it does, its parameters and its answer (Markdown)
}

type apiSection struct {
	Title  string
	Intro  string
	Routes []apiDoc
}

var apiSections = []apiSection{
	{
		Title: "Overview and monitoring",
		Intro: "Start here to answer \"is everything backed up?\".",
		Routes: []apiDoc{
			{"GET /api/dashboard", "Counts of servers, jobs and backups; every job's last result, last successful backup, freshness (`stale`, `due_at`) and next run; the tasks running now; the 12 newest backups; and the current `warnings` (failed jobs, overdue backups, log archiving problems, failed restore drills)."},
			{"GET /api/activity", "The tasks running now (backups, verifications, copies, restores, drills) with their progress."},
		},
	},
	{
		Title: "Backup jobs",
		Intro: "A job is what to back up (a server and its databases), how, where to and when. Create and update take a job object as `GET /api/jobs/{id}` returns it.",
		Routes: []apiDoc{
			{"GET /api/jobs", "List the jobs with their schedule, storages, retention, last result and next run."},
			{"POST /api/jobs", "Create a job. Answers the job with its `id`."},
			{"GET /api/jobs/{id}", "One job. The encryption passphrase is never returned."},
			{"PUT /api/jobs/{id}", "Update a job (send the whole object)."},
			{"DELETE /api/jobs/{id}", "Delete a job. `?delete_backups=1` also deletes its backups from every storage; without it they stay listed under Backups."},
			{"POST /api/jobs/{id}/run", "Start a backup now. Body (optional): `{\"type\": \"full\"}` or `{\"type\": \"incremental\"}` (physical backups). Answers the new backup; follow it with `GET /api/runs/{id}`. 409 while the job is already running."},
			{"POST /api/jobs/{id}/toggle", "Turn the job's schedule on or off."},
			{"POST /api/cron/preview", "Check a cron expression and list its next run times. Body: `{\"expr\": \"0 2 * * *\"}`. Changes nothing."},
		},
	},
	{
		Title: "Backups",
		Intro: "A backup (a *run*) is one execution of a job: its files with SHA-256, its copies on each storage, its verification and drills.",
		Routes: []apiDoc{
			{"GET /api/runs", "List backups, newest first. Query: `job_id`, `status` (queued, running, success, partial, failed, canceled, deleted), `method`, `q` (search), `live=1` (running only), `limit` (default 50), `offset`. Answers `{\"items\": [...], \"total\": n}`."},
			{"GET /api/runs/{id}", "One backup: status, error, files with checksums, copies per storage, binlog / WAL position, verification, drills and progress."},
			{"GET /api/runs/{id}/log", "The backup's log. Query: `offset` (bytes) or `tail=1` (the last 200 KB). Answers `{\"data\": \"…\", \"next\": n}`: pass `next` as `offset` to follow a running backup."},
			{"POST /api/runs/{id}/cancel", "Cancel a queued or running backup."},
			{"POST /api/runs/{id}/verify", "Read the copies back and compare every checksum. Body (optional): `{\"location_id\": n}` for one copy only. Runs in the background: the result appears on the backup."},
			{"POST /api/runs/{id}/delete", "Delete a backup. Body (optional): `{\"location_id\": n}` deletes one copy only; `{\"cascade\": true}` also deletes the incremental backups based on it."},
			{"POST /api/runs/{id}/restore", "Start a restore. Body: `target_server_id`, `location_id` (optional), `databases` (a subset), `rename` (`{\"old\": \"new\"}`), `drop_existing` (needs `\"confirm\": \"RESTORE\"`), `restore_users`, `threads`, portability options (`strip_definer`, `convert_collations`, `skip_gtid_purged`, `no_owner`, `no_privileges`) and `pitr` (a point-in-time target, see `POST /api/pitr/plan`). Physical backups: `target_dir` (admin), `apply` (replace the datadir: admin, needs `\"confirm\": \"RESTORE\"`). Answers the restore; follow it with `GET /api/restores/{id}`."},
			{"POST /api/runs/{id}/restore-check", "Compare a backup with the server it would be restored to (version, flavor, databases already there) and suggest options. Body: `{\"target_server_id\": n}`. Changes nothing."},
			{"POST /api/runs/{id}/copy", "Copy the backup to another storage. Body: `{\"storage_id\": n}`."},
			{"GET /api/runs/{id}/download", "Download one file of the backup. Query: `file` (a name from the backup's files), `location_id` (optional), `decode=1` (decrypt and decompress on the way)."},
		},
	},
	{
		Title: "Restores",
		Routes: []apiDoc{
			{"GET /api/restores", "List the restores that were run, with status and error."},
			{"GET /api/restores/{id}", "One restore: source backup, target, options, progress, result."},
			{"GET /api/restores/{id}/log", "The restore's log (`offset`, `tail=1`, as for backups)."},
			{"POST /api/restores/{id}/cancel", "Cancel a running restore."},
		},
	},
	{
		Title: "Log archiving and point-in-time recovery",
		Intro: "Jobs can archive the database's log continuously (MySQL / MariaDB binlogs, PostgreSQL WAL, SQL Server log backups) to restore to any moment after a backup.",
		Routes: []apiDoc{
			{"GET /api/binlog", "Every job that archives its log: streaming or failing, files waiting for upload, archive lag."},
			{"GET /api/jobs/{id}/binlogs", "The archived log files of a job (`offset` for paging)."},
			{"GET /api/jobs/{id}/binlog/log", "The log of the job's archiver (`offset`, `tail=1`)."},
			{"POST /api/jobs/{id}/binlog/rotate", "Close the current log file now, so that everything up to now is uploaded."},
			{"POST /api/jobs/{id}/binlog/restart", "Restart the job's archiver."},
			{"POST /api/pitr/plan", "Preview a point-in-time recovery: the base backup it would start from and whether the archived log reaches the target. Body: `{\"job_id\": n, \"run_id\": n (optional), \"target\": {\"type\": \"time\", \"time\": \"2026-10-20T14:37:11+07:00\"}}`. Target types: `latest`, `time`; MySQL `position` (`file`, `pos`) and `gtid`; PostgreSQL `lsn` and `name` (a restore point); SQL Server `lsn` and `mark` (`name`: a marked transaction). Changes nothing."},
		},
	},
	{
		Title: "Restore drills",
		Intro: "A drill restores a backup into a sandbox and runs checks on it, on a schedule or on demand.",
		Routes: []apiDoc{
			{"GET /api/drills", "List the drills (`job_id` to filter) with their last result."},
			{"POST /api/drills", "Create a drill."},
			{"GET /api/drills/{id}", "One drill."},
			{"PUT /api/drills/{id}", "Update a drill."},
			{"DELETE /api/drills/{id}", "Delete a drill."},
			{"POST /api/drills/{id}/run", "Run the drill now. Body (optional): `{\"run_id\": n}` to test one backup (default: the newest). Answers the drill run."},
			{"GET /api/drill-runs", "List drill runs. Query: `drill_id`, `job_id`, `run_id`, `limit`, `offset`."},
			{"GET /api/drill-runs/{id}", "One drill run: every check with its result, the restore time (RTO)."},
			{"GET /api/drill-runs/{id}/log", "The drill run's log (`offset`, `tail=1`)."},
			{"POST /api/drill-runs/{id}/cancel", "Cancel a running drill."},
		},
	},
	{
		Title: "Database servers",
		Intro: "Passwords are never returned; on update, an empty password keeps the stored one.",
		Routes: []apiDoc{
			{"GET /api/servers", "List the database servers."},
			{"POST /api/servers", "Add a server (name, engine, host, port, username, password, connection mode, …)."},
			{"POST /api/servers/test", "Test the connection of unsaved settings (same body as creating): version, missing privileges, usable backup methods."},
			{"GET /api/servers/{id}", "One server."},
			{"PUT /api/servers/{id}", "Update a server."},
			{"DELETE /api/servers/{id}", "Delete a server."},
			{"GET /api/servers/{id}/info", "Connect now and inspect: version, datadir, databases and their sizes (`sizes=0` skips the sizes), usable methods."},
			{"GET /api/engines", "The supported database engines with their backup methods, restore options and log archiving."},
		},
	},
	{
		Title: "Agents",
		Intro: "Database hosts that connect out to DumpCat over HTTPS instead of opening a port.",
		Routes: []apiDoc{
			{"GET /api/agents", "List the agents and whether they are connected."},
			{"POST /api/agents", "Create an agent. Answers its key and install commands once."},
			{"PUT /api/agents/{id}", "Update an agent (name, allowed addresses, physical backups)."},
			{"POST /api/agents/{id}/rotate", "Give the agent a new key; the old one stops working."},
			{"DELETE /api/agents/{id}", "Delete an agent."},
		},
	},
	{
		Title: "WireGuard",
		Intro: "The built-in WireGuard server that database hosts can join.",
		Routes: []apiDoc{
			{"GET /api/wireguard", "WireGuard state, settings and peers."},
			{"PUT /api/wireguard", "Turn WireGuard on or off and change its settings."},
			{"POST /api/wireguard/peers", "Add a peer. Answers its configuration file once."},
			{"PUT /api/wireguard/peers/{id}", "Update a peer."},
			{"POST /api/wireguard/peers/{id}/rotate", "Give the peer new keys. Answers its new configuration once."},
			{"DELETE /api/wireguard/peers/{id}", "Delete a peer."},
		},
	},
	{
		Title: "Storage",
		Intro: "Where backups go. Secret fields come back as `__SECRET_UNCHANGED__`: send that value back to keep the stored secret.",
		Routes: []apiDoc{
			{"GET /api/storage-types", "The storage types and their fields, and the OAuth callback URL."},
			{"GET /api/storages", "List the storages."},
			{"POST /api/storages", "Add a storage (`name`, `type`, `config`)."},
			{"POST /api/storages/test", "Test unsaved settings: write, read and delete a file."},
			{"GET /api/storages/{id}", "One storage."},
			{"PUT /api/storages/{id}", "Update a storage."},
			{"DELETE /api/storages/{id}", "Delete a storage."},
			{"POST /api/storages/{id}/test", "Test a saved storage: write, read and delete a file."},
			{"GET /api/storages/{id}/browse", "List the folders and files under `path`."},
			{"GET /api/storages/{id}/usage", "The space DumpCat's backups take there."},
			{"POST /api/storages/{id}/import", "Import the backup catalog: list again the backups found on the storage (after a reinstall)."},
			{"POST /api/oauth/start", "Start the OAuth sign-in of a Google Drive, OneDrive or Dropbox storage."},
			{"POST /api/oauth/exchange", "Finish an OAuth sign-in with a pasted code."},
			{"GET /api/oauth/result", "The result of an OAuth sign-in (`state`)."},
			{"GET /api/oauth/callback", "Where the provider sends the browser back after an OAuth sign-in."},
		},
	},
	{
		Title: "Notifications",
		Intro: "Channels (email, Telegram, Slack, Discord, webhooks) that report failures, successes and alerts.",
		Routes: []apiDoc{
			{"GET /api/notify-types", "The channel types and their fields."},
			{"GET /api/notifications", "List the channels."},
			{"POST /api/notifications", "Add a channel."},
			{"PUT /api/notifications/{id}", "Update a channel."},
			{"DELETE /api/notifications/{id}", "Delete a channel."},
			{"POST /api/notifications/{id}/test", "Send a test message through a saved channel."},
			{"POST /api/notifications/test", "Send a test message with unsaved settings."},
			{"POST /api/notifications/digest", "Send the weekly digest now through a channel."},
			{"GET /api/digest/preview", "The weekly digest as it would be sent now."},
		},
	},
	{
		Title: "Users",
		Routes: []apiDoc{
			{"GET /api/users", "List the accounts."},
			{"POST /api/users", "Create an account. Body: `{\"username\", \"email\", \"password\", \"role\"}` (viewer, operator or admin)."},
			{"PUT /api/users/{id}", "Change an account: any of `email`, `role`, `disabled`, `password`. Locking it or setting a password ends its sessions and revokes its tokens."},
			{"DELETE /api/users/{id}", "Delete an account."},
			{"POST /api/users/{id}/reset-2fa", "Turn two-factor sign-in off for an account that lost its authenticator."},
		},
	},
	{
		Title: "Your account and API tokens",
		Routes: []apiDoc{
			{"GET /api/me", "The account the request is made as: id, username, email, role, two-factor state."},
			{"POST /api/me/password", "Change your password. Body: `{\"current\", \"new\"}`. Signs out your other sessions."},
			{"POST /api/me/totp/setup", "Start two-factor setup: answers the secret, the otpauth URL and a QR code."},
			{"POST /api/me/totp/enable", "Finish two-factor setup. Body: `{\"code\": \"123456\"}`."},
			{"POST /api/me/totp/disable", "Turn two-factor sign-in off. Body: `{\"password\"}`."},
			{"GET /api/me/tokens", "Your API tokens: name, prefix, access, created, last used, expiry. Never the token itself."},
			{"POST /api/me/tokens", "Create a token. Body: `{\"name\", \"access\": \"read\", \"full\" or \"manage\", \"expires_days\" (0 = never), \"password\"}`. Answers `{\"token\": \"mv_…\", \"item\": {…}}`: the token is shown only in this answer."},
			{"DELETE /api/me/tokens/{id}", "Revoke one of your tokens."},
			{"POST /api/me/logout-others", "Sign out your other browser sessions."},
		},
	},
	{
		Title: "Settings and system",
		Routes: []apiDoc{
			{"GET /api/settings", "The settings (secrets masked)."},
			{"PUT /api/settings", "Change the settings (send the whole object)."},
			{"POST /api/settings/metrics-token", "Create or replace the bearer token of `GET /metrics`. Answers it once."},
			{"GET /api/branding/logo", "The instance's logo with its own Content-Type (PNG, WebP or SVG); redirects to the cat when none is set. The instance name is the `instance_name` setting."},
			{"PUT /api/branding/logo", "Upload a logo. Body: `{\"data\": \"<base64 or a data: URL>\"}`; PNG, WebP or a plain SVG drawing (no scripts, links or external files), at most 256 KB. Answers the name and the logo URL."},
			{"DELETE /api/branding/logo", "Go back to the default logo."},
			{"GET /api/settings/sso", "The single sign-on (OpenID Connect) settings, the client secret masked, and `redirect_url`: the callback to register at the provider."},
			{"PUT /api/settings/sso", "Change the SSO settings: `{\"enabled\", \"issuer\", \"client_id\", \"client_secret\", \"scopes\", \"allowed_domains\", \"auto_create\", \"default_role\", \"role_claim\", \"admin_values\", \"operator_values\", \"viewer_values\", \"password_admins_only\", ...}`. Turning it on checks that the provider answers."},
			{"POST /api/settings/sso/test", "Start a test sign-in with the SSO settings in the body (saved or not). Answers `{\"url\"}` to open in a browser: the page it ends on shows the verified claims and what a sign-in would do, and signs nobody in."},
			{"GET /api/system", "Version, platform, uptime, data directory and the backup tools found with their versions."},
			{"POST /api/system/tools/refresh", "Look for the backup tools again."},
			{"GET /api/update", "The installed and the latest version, and the state of an update."},
			{"POST /api/update/check", "Check for a new version now."},
			{"POST /api/update/install", "Install the latest version (waits for running backups, rolls back on failure)."},
			{"POST /api/update/cancel", "Cancel an update that has not started installing."},
			{"POST /api/update/rollback", "Go back to the version before the last update."},
			{"GET /api/audit", "The audit log, newest first. Query: `q` (search), `limit` (default 100), `offset`."},
		},
	},
	{
		Title: "AI assistants and this reference",
		Routes: []apiDoc{
			{"POST /api/mcp", "The MCP endpoint (streamable HTTP, JSON-RPC, no sessions). API tokens only, see *AI assistants (MCP)* above."},
			{"GET /api/mcp/tools", "The MCP tools with what each does and the role and token access it needs."},
			{"GET /api/docs.md", "This reference, in Markdown."},
		},
	},
	{
		Title: "Signing in (web UI)",
		Intro: "The browser's sign-in. Scripts and assistants use an API token instead.",
		Routes: []apiDoc{
			{"GET /api/auth/status", "Whether setup is needed and whether this browser is signed in."},
			{"POST /api/auth/setup", "Create the first admin with the one-time setup token, while no account exists."},
			{"POST /api/auth/login", "Sign in and get a session cookie. Body: `{\"username\", \"password\", \"totp\", \"remember\"}`; answers `{\"totp_required\": true}` when a two-factor code is needed."},
			{"POST /api/auth/logout", "End the browser session."},
			{"GET /api/auth/sso/start", "Sign in with single sign-on: redirects the browser to the identity provider (`?remember=1` keeps the session 30 days)."},
			{"GET /api/auth/sso/callback", "Where the identity provider sends the browser back: checks the state, the code (PKCE) and the ID token, then signs in."},
			{"POST /api/auth/sso/totp", "Finish an SSO sign-in with the two-factor code of an account that has 2FA. Body: `{\"totp\"}`."},
		},
	},
}

// roleLabel names who may call a route.
func roleLabel(role string) string {
	if role == "" {
		return "public"
	}
	return role
}

// tokenLabel tells which API tokens may call a route.
func tokenLabel(pattern, role string) string {
	switch {
	case pattern == "POST /api/mcp":
		return "any API token"
	case role == "":
		return "no token needed"
	case readOnlyAllows(pattern):
		return "read-only or full-access token"
	}
	return "full-access token"
}

// apiOrigin is the address clients reach this DumpCat at: the configured base URL, or the
// address of the request.
func (s *Server) apiOrigin(r *http.Request) string {
	if b := s.baseURL(); b != "" {
		return strings.TrimRight(b, "/")
	}
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// mcpToolInfo describes an MCP tool for the API & MCP page and the reference.
type mcpToolInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Route       string `json:"route"`
	Role        string `json:"role"`        // lowest role that sees the tool
	Write       bool   `json:"write"`       // starts or changes something: needs a full-access token
	Manage      bool   `json:"manage"`      // changes the configuration: needs a token with Manage access
	Destructive bool   `json:"destructive"` // may remove something
	Available   bool   `json:"available"`   // the signed-in account's role is enough
}

func (s *Server) mcpToolInfos(u *store.User) []mcpToolInfo {
	var out []mcpToolInfo
	for _, t := range mcpTools() {
		role := s.routeRoles[t.Route]
		out = append(out, mcpToolInfo{Name: t.Name, Title: t.Title, Description: t.Description, Route: t.Route, Role: role,
			Write: t.Write || !readOnlyAllows(t.Route), Manage: t.Manage, Destructive: t.Destructive,
			Available: u != nil && store.RoleLevel(u.Role) >= store.RoleLevel(role)})
	}
	return out
}

// DemoReference returns the Markdown reference and the MCP tools as an admin sees them, for a
// DumpCat reached at origin, without a running server: the demo on dumpcat.cc shows them
// (scripts/demotypes).
func DemoReference(version, origin string) (string, any) {
	s := &Server{Version: version}
	s.routes(http.NewServeMux())
	return s.apiReference(origin), s.mcpToolInfos(&store.User{Role: store.RoleAdmin})
}

func (s *Server) mcpToolsInfo(w http.ResponseWriter, r *http.Request) error {
	origin := s.apiOrigin(r)
	return ok(w, map[string]any{
		"endpoint": origin + "/api/mcp", "docs": origin + "/api/docs.md", "origin": origin,
		"version": s.Version, "protocol_versions": mcpVersions, "tools": s.mcpToolInfos(userOf(r)),
	})
}

func (s *Server) apiDocsMarkdown(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="dumpcat-api.md"`)
	w.Header().Set("Cache-Control", "no-store")
	_, err := w.Write([]byte(s.apiReference(s.apiOrigin(r))))
	return err
}

// apiReference writes the Markdown reference for clients reaching DumpCat at origin.
func (s *Server) apiReference(origin string) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	p("# DumpCat API reference\n\n")
	p("DumpCat %s at %s backs up MySQL, MariaDB, PostgreSQL and SQL Server databases on schedules, archives their logs for "+
		"point-in-time recovery, restores them and tests backups with restore drills. This reference is written by the server "+
		"itself and lists every route it serves: `GET %s/api/docs.md`.\n\n", s.Version, origin, origin)

	p("## Authentication\n\n")
	p("- Create an API token in the web UI: **API & MCP → New token** (or `POST /api/me/tokens`). Send it with every request:\n" +
		"  `Authorization: Bearer mv_…`. A token acts as the account that created it.\n")
	p("- **Read-only** tokens (the default) can call every `GET` route except downloads and OAuth, plus `POST /api/cron/preview` and " +
		"`POST /api/pitr/plan`: they look and change nothing. **Full-access** tokens can do everything the account's role can. " +
		"**Manage** tokens are full-access tokens that MCP also lets set up and change jobs, drills, servers and storages; over " +
		"the REST API they are the same as full access.\n")
	p("- Roles, from least to most: viewer (looks), operator (runs backups, restores, manages servers, storages, jobs and drills), " +
		"admin (users, settings, notifications, agents, WireGuard, updates). Each route below names the lowest role it needs.\n")
	p("- Requests with a token need no CSRF header (the web UI's session sends `X-CSRF-Token` on changes).\n\n")

	p("## Conventions\n\n")
	p("- JSON in and out. A request with a body must send `Content-Type: application/json`.\n")
	p("- Errors answer an HTTP status and `{\"error\": \"message\"}`: 400 invalid input, 401 no or invalid token, 403 role too low " +
		"or read-only token, 404 not found, 409 conflict (e.g. the job is already running), 415 not JSON, 429 too many attempts, " +
		"503 while DumpCat updates itself.\n")
	p("- Ids are numbers. Times are RFC 3339 (`2026-10-20T14:37:11+07:00`).\n")
	p("- Paged lists answer `{\"items\": [...], \"total\": n}` and take `limit` and `offset`.\n")
	p("- Logs answer `{\"data\": \"…\", \"next\": n}`; pass `next` back as `offset` to follow a running task, or `tail=1` to start at the end.\n")
	p("- Secrets (database passwords, passphrases, storage keys, webhook addresses, tokens) are never returned.\n")
	p("- Long tasks (backups, verifications, restores, drills) start in the background: the answer is the new object; poll it until " +
		"`status` is no longer `queued` or `running`.\n")
	p("- Every change is written to the audit log with the account's name.\n\n")

	p("## Examples\n\n```bash\n")
	p("TOKEN=mv_xxxxxxxx\n")
	p("# what needs attention\ncurl -H \"Authorization: Bearer $TOKEN\" %s/api/dashboard\n", origin)
	p("# the failed backups of job 1\ncurl -H \"Authorization: Bearer $TOKEN\" \"%s/api/runs?job_id=1&status=failed\"\n", origin)
	p("# why backup 42 failed: its error, then the end of its log\ncurl -H \"Authorization: Bearer $TOKEN\" %s/api/runs/42\n", origin)
	p("curl -H \"Authorization: Bearer $TOKEN\" \"%s/api/runs/42/log?tail=1\"\n", origin)
	p("# run job 1 now (full-access token)\ncurl -X POST -H \"Authorization: Bearer $TOKEN\" -H \"Content-Type: application/json\" "+
		"-d '{\"type\":\"full\"}' %s/api/jobs/1/run\n", origin)
	p("```\n\n")

	p("## AI assistants (MCP)\n\n")
	p("DumpCat is an MCP server: `%s/api/mcp` (streamable HTTP, header `Authorization: Bearer <token>`). For clients that start a "+
		"local command, `dumpcat mcp` forwards stdio to it (settings `DUMPCAT_MCP_URL`, `DUMPCAT_MCP_TOKEN`). Each tool call runs "+
		"the REST request named below with the token: the same role check, read-only rule and audit log. A full-access token "+
		"starts backups, verifications, drills and storage tests; the tools that set up and change jobs, drills, servers and "+
		"storages need a token with Manage access. Restores, deletions and settings are not available over MCP, and no tool "+
		"takes or returns a secret (password, key, passphrase): a tool that leaves one unset answers `finish_url`, the web UI "+
		"page where the user enters it.\n\n", origin)
	p("| Tool | What it does | Role | Token | REST request |\n|---|---|---|---|---|\n")
	for _, t := range s.mcpToolInfos(nil) {
		tok := "read-only or full"
		switch {
		case t.Manage:
			tok = "Manage access"
		case t.Write:
			tok = "full access"
		}
		desc, _, _ := strings.Cut(t.Description, ". ")
		p("| `%s` | %s | %s | %s | `%s` |\n", t.Name, strings.TrimSuffix(desc, "."), t.Role, tok, t.Route)
	}
	p("\n")

	p("## Routes\n\n")
	p("Paths are relative to %s. `{id}` is a number.\n\n", origin)
	for _, sec := range apiSections {
		p("### %s\n\n", sec.Title)
		if sec.Intro != "" {
			p("%s\n\n", sec.Intro)
		}
		for _, d := range sec.Routes {
			role, ok := s.routeRoles[d.Route]
			if !ok {
				continue // not served by this build
			}
			p("#### `%s`\n\n%s · %s\n\n%s\n\n", d.Route, roleLabel(role), tokenLabel(d.Route, role), d.Doc)
		}
	}
	p("### Outside /api\n\n")
	p("#### `GET /metrics`\n\nPrometheus metrics, off until enabled in **Settings → Monitoring** (its own bearer token, or this machine only).\n")
	return b.String()
}
