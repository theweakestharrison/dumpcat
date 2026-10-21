package api

// MCP tools that set backups up and change them. With an API token of Manage access
// (store.TokenManage) an assistant creates and changes jobs, restore drills, database servers and
// storages; a full-access token still only starts things, a read-only token only looks.
//
// Secrets never pass through MCP: no tool takes a password, key, token or passphrase (mcpArgs
// refuses every key a tool's schema leaves out, and the storage tools refuse secret fields). A
// tool that leaves one unset answers with finish_url, the page of the web UI where the user
// enters it. Deleting, restoring, pre/post commands, extra tool arguments and DumpCat's settings
// stay out of MCP. Like every tool, each runs the REST request with the caller's token
// (apiCall): roles, validation and the audit log are the API's.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"dumpcat/internal/drivers"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

func (c *mcpCall) put(path string, body any) (any, error) { return c.do(http.MethodPut, path, body) }

// webURL is a page of the web UI (fragment "/jobs/3/edit"): where the user finishes what MCP
// does not do, such as entering a password.
func (c *mcpCall) webURL(fragment string) string { return c.s.apiOrigin(c.r) + "/#" + fragment }

// mcpArgs decodes a tool's arguments as an object and refuses any key its schema does not list,
// also inside nested objects: what the schema leaves out (a password, a pre/post command) cannot
// be sent at all. Objects without properties in the schema (a storage's config) are checked by
// their tool.
func mcpArgs(raw json.RawMessage, schema map[string]any) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, errors.New("invalid arguments: a JSON object is expected")
	}
	return m, schemaKeys("", m, schema)
}

func schemaKeys(prefix string, v any, schema map[string]any) error {
	switch x := v.(type) {
	case map[string]any:
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			return nil
		}
		for k, e := range x {
			ps, ok := props[k].(map[string]any)
			if !ok {
				return fmt.Errorf("unknown argument %s%s: leave it out (the tool's input schema lists what it takes)", prefix, k)
			}
			if err := schemaKeys(prefix+k+".", e, ps); err != nil {
				return err
			}
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		for i, e := range x {
			if err := schemaKeys(fmt.Sprintf("%s[%d].", strings.TrimSuffix(prefix, "."), i), e, items); err != nil {
				return err
			}
		}
	}
	return nil
}

// takeID removes the id argument key from args.
func takeID(args map[string]any, key string) (int64, error) {
	n, _ := args[key].(json.Number)
	delete(args, key)
	id, err := n.Int64()
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", key)
	}
	return id, nil
}

// overlay sets src's keys on dst, merging objects (options, retention) key by key; lists and
// values are replaced.
func overlay(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				overlay(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// inputOf keeps the keys of a view that its PUT route takes.
func inputOf(view map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := view[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

func mcpBool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func mcpCount(desc string, lo, hi int) map[string]any {
	return map[string]any{"type": "integer", "minimum": lo, "maximum": hi, "description": desc}
}

func mcpStrings(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func mcpObj(desc string, props map[string]any) map[string]any {
	m := mcpObject(nil, props)
	m["description"] = desc
	return m
}

// ---- jobs ----

var jobInputKeys = []string{"name", "server_id", "method", "db_mode", "databases", "exclude_tables", "schedule", "incr_schedule",
	"compression", "compression_level", "encrypt", "threads", "options", "retention", "targets", "enabled", "notify_success",
	"notify_failure", "timeout_minutes"}

// jobDefaults is what the web UI's new-job form starts from (web/assets/js/pages/jobedit.js), so
// a job made over MCP is the job the form would make.
func jobDefaults() map[string]any {
	return map[string]any{
		"name": "", "server_id": 0, "method": store.MethodMysqldump, "db_mode": "all", "databases": []any{}, "exclude_tables": []any{},
		"schedule": "0 2 * * *", "incr_schedule": "", "compression": "zstd", "compression_level": 0, "encrypt": false, "threads": 4,
		"options": map[string]any{
			"single_file": false, "routines": true, "triggers": true, "events": true, "include_users": true, "record_binlog": false,
			"gtid_purged": "OFF", "chunk_rows": 500000, "long_query_guard": 60, "lock_mode": "", "ftwrl_wait_timeout": 60,
			"slave_info": false, "safe_slave_backup": false, "low_priority": true, "stream_upload": false, "verify_mode": store.VerifyOne,
			"bwlimit": "", "binlog_archive": false, "binlog_rotate_minutes": 15, "max_age_hours": 0,
			"copy_only": false, "native_compression": "", "verify_only": true,
		},
		"retention": map[string]any{"keep_last": 7, "keep_days": 0, "keep_daily": 0, "keep_weekly": 0, "keep_monthly": 0},
		"targets":   []any{}, "enabled": true, "notify_success": false, "notify_failure": true, "timeout_minutes": 0,
	}
}

func retentionSchema(desc string) map[string]any {
	n := func(d string) map[string]any {
		return map[string]any{"type": "integer", "minimum": 0, "description": d}
	}
	return mcpObj(desc, map[string]any{
		"keep_last": n("Keep the newest N backups."), "keep_days": n("Keep every backup of the last N days."),
		"keep_daily": n("Keep the last backup of each of the last N days."), "keep_weekly": n("The same per week."),
		"keep_monthly": n("The same per month."),
	})
}

// jobProps are the job settings an assistant may set. Left out on purpose: the encryption
// passphrase, pre/post commands and extra tool arguments (they run as the service user).
// retention describes what a given retention does (create: replaces the default; update: merges).
func jobProps(retention string) map[string]any {
	return map[string]any{
		"name":      mcpString("Job name, unique."),
		"server_id": mcpID("Database server to back up (list_servers)."),
		"method": mcpEnum("Backup method of the server's engine: mysqldump, mydumper or physical (XtraBackup / MariaBackup) for "+
			"MySQL and MariaDB; pg_dump or pg_basebackup for PostgreSQL; mssql_native (BACKUP DATABASE) for SQL Server. get_server with check_connection tells which work.", mcpMethods()...),
		"db_mode":        mcpEnum("all: every database but the system ones (default); include: only databases; exclude: all but databases.", "all", "include", "exclude"),
		"databases":      mcpStrings("Database names for db_mode include or exclude."),
		"exclude_tables": mcpStrings("Tables to leave out, as database.table."),
		"schedule": mcpString("When full backups run: a cron expression (minute hour day-of-month month day-of-week, in DumpCat's " +
			"time zone; default 0 2 * * *, daily at 02:00) or @manual. Check it with preview_schedule."),
		"incr_schedule":     mcpString("Incremental backups, for methods that have them (physical), or differential backups (mssql_native): a cron expression, or empty for none."),
		"compression":       mcpEnum("zstd (default), gzip or none.", "zstd", "gzip", "none"),
		"compression_level": mcpCount("Compression level (0: the default).", 0, 22),
		"threads":           mcpCount("Parallel threads (default 4).", 1, 64),
		"options": mcpObj("Method options; only the ones given change. Pre/post commands and extra tool arguments are set in the web UI.", map[string]any{
			"verify_mode":           mcpEnum("After each backup: off, one (read back the cheapest copy; default) or all copies.", store.VerifyOff, store.VerifyOne, store.VerifyAll),
			"binlog_archive":        mcpBool("Archive the server's log continuously (MySQL binlogs, PostgreSQL WAL, SQL Server log backups) for point-in-time recovery; methods that support it."),
			"binlog_rotate_minutes": mcpCount("Close the current archived log file after N minutes (default 15; 0: never). SQL Server: a log backup every N minutes (0: 15).", 0, 1440),
			"copy_only":             mcpBool("SQL Server: COPY_ONLY backups, which leave the server's own backup chain alone (full backups only)."),
			"native_compression":    mcpEnum("SQL Server: BACKUP ... WITH COMPRESSION: empty (when the edition has it; default), on or off.", "", "on", "off"),
			"verify_only":           mcpBool("SQL Server: RESTORE VERIFYONLY on the server before DumpCat reads each file (default on)."),
			"max_age_hours":         mcpCount("Alert when the newest successful backup is older than this (0: worked out from the schedule).", 0, 24*366),
			"routines":              mcpBool("MySQL logical: stored procedures and functions (default on)."),
			"triggers":              mcpBool("MySQL logical: triggers (default on)."),
			"events":                mcpBool("MySQL logical: scheduled events (default on)."),
			"include_users":         mcpBool("Back up users and grants (PostgreSQL: roles) separately (default on)."),
			"single_file":           mcpBool("mysqldump: one file for all databases instead of one per database."),
			"record_binlog":         mcpBool("mysqldump: record the binlog position of the dump."),
			"chunk_rows":            mcpCount("mydumper: split tables into chunks of N rows (default 500000).", 0, 1<<30),
			"long_query_guard":      mcpCount("mydumper: wait at most N seconds for long queries before the dump (default 60).", 0, 86400),
			"lock_mode":             mcpString("mydumper --sync-thread-lock-mode (empty: AUTO)."),
			"ftwrl_wait_timeout":    mcpCount("physical: seconds to wait for the backup lock (default 60).", 0, 86400),
			"slave_info":            mcpBool("physical: record the replication source position."),
			"safe_slave_backup":     mcpBool("physical: stop the replica's SQL thread while backing up."),
			"low_priority":          mcpBool("Run the backup tools at low CPU and disk priority (default on)."),
			"stream_upload":         mcpBool("Stream straight to the storages instead of writing a local file first."),
			"bwlimit":               mcpString("Upload bandwidth limit, e.g. 10M (empty: none)."),
		}),
		"retention": retentionSchema("How many backups to keep on every storage (all 0: keep every backup). Applied after each backup: " +
			"lowering it removes older backups then. " + retention),
		"targets": map[string]any{"type": "array", "minItems": 1, "description": "Storages each backup is written to (list_storages): the " +
			"whole list, it replaces the current one. Each may keep its own number of backups.",
			"items": mcpObject([]string{"storage_id"}, map[string]any{
				"storage_id": mcpID("Storage id from list_storages."),
				"retention":  retentionSchema("Retention on this storage instead of the job's."),
			})},
		"enabled":         mcpBool("Back up on the schedule (false: paused; run_backup still works)."),
		"notify_success":  mcpBool("Notify after successful backups (default off)."),
		"notify_failure":  mcpBool("Notify after failed backups (default on)."),
		"timeout_minutes": mcpCount("Cancel a backup that runs longer (0: no limit).", 0, 10080),
	}
}

func jobIDOf(v any) int64 {
	n, _ := asMap(asMap(v)["job"])["id"].(json.Number)
	id, _ := n.Int64()
	return id
}

// ---- servers ----

var serverInputKeys = []string{"engine", "name", "host", "port", "socket", "username", "tls_mode", "ca_cert", "flavor", "datadir",
	"service_name", "os_user", "notes", "restore_username", "agent_id", "wg_peer_id", "backup_dir", "backup_dir_local"}

// serverProps are the server settings an assistant may set: everything but the passwords and
// the CA certificate.
func serverProps() map[string]any {
	var engines []string
	for _, d := range drivers.All() {
		engines = append(engines, d.Engine())
	}
	return map[string]any{
		"engine":           mcpEnum("mysql (MySQL, MariaDB, Percona Server; default), postgres (PostgreSQL) or mssql (Microsoft SQL Server).", engines...),
		"name":             mcpString("Server name, unique."),
		"host":             mcpString(`Host name or IP address (or socket). SQL Server: host\INSTANCE for a named instance found by the SQL Server Browser (then no port).`),
		"port":             mcpCount("Port (default 3306 for MySQL, 5432 for PostgreSQL, 1433 for SQL Server).", 1, 65535),
		"socket":           mcpString("Unix socket path instead of host and port, for a database on the DumpCat host."),
		"username":         mcpString("Database user that DumpCat backs up with."),
		"tls_mode":         mcpEnum("TLS to the database: disabled, required or verify (the certificate against the system's CAs; a CA certificate of its own is added in the web UI).", "disabled", "required", "verify"),
		"flavor":           mcpEnum("MySQL only: auto (default; detected), mysql, mariadb or percona.", "auto", "mysql", "mariadb", "percona"),
		"datadir":          mcpString("Data directory, for physical backups and restores on this host."),
		"service_name":     mcpString("Service name of the database (e.g. mysql, mariadb, postgresql@16-main), for physical restores."),
		"os_user":          mcpString("Operating system user that owns the data directory (e.g. mysql)."),
		"notes":            mcpString("Free notes."),
		"restore_username": mcpString("Another database user for restores (empty: the backup user)."),
		"agent_id":         mcpID("Reach the server through this agent (the server connects out to DumpCat; agents are set up in the web UI)."),
		"wg_peer_id":       mcpID("Reach the server through this WireGuard peer (set up in the web UI)."),
		"backup_dir": mcpString(`SQL Server only: the folder SQL Server writes its backups to, as SQL Server names it on its own host (D:\Backup\DumpCat, ` +
			`\\nas\sql, /var/opt/mssql/dumpcat; empty: the instance's default backup folder). Its service account needs Modify permission there.`),
		"backup_dir_local": mcpString("SQL Server only: the same folder as DumpCat opens it (empty: the same path, i.e. DumpCat runs on the SQL Server host, " +
			"or the server is reached through an agent started with --allow-mssql-dir, which moves the files; otherwise the share or mount on the DumpCat host)."),
	}
}

// ---- storages ----

// storageConfigArgs turns the config argument into strings and refuses settings the type does
// not have and secret ones.
func storageConfigArgs(typ string, v any) (map[string]string, error) {
	td, ok := storage.GetType(typ)
	if !ok {
		return nil, fmt.Errorf("unknown storage type %q (list_storage_types)", typ)
	}
	out := map[string]string{}
	if v == nil {
		return out, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("config must be an object of settings")
	}
	for k, x := range m {
		i := slices.IndexFunc(td.Fields, func(f storage.Field) bool { return f.Key == k })
		if i < 0 {
			var keys []string
			for _, f := range td.Fields {
				if !f.Secret {
					keys = append(keys, f.Key)
				}
			}
			return nil, fmt.Errorf("a %s storage has no setting %q; its settings: %s", td.Label, k, strings.Join(keys, ", "))
		}
		if f := td.Fields[i]; f.Secret {
			return nil, fmt.Errorf("%s (%s) is a secret, and secrets never go through MCP: leave it out, the user enters it in "+
				"the web UI (the answer gives finish_url)", f.Key, f.Label)
		}
		switch y := x.(type) {
		case string:
			out[k] = y
		case bool:
			out[k] = strconv.FormatBool(y)
		case json.Number:
			out[k] = y.String()
		default:
			return nil, fmt.Errorf("config.%s must be a string, a number or true/false", k)
		}
	}
	return out, nil
}

// storageAnswer shows a storage to a model: its settings without the secrets, which secrets
// are set, which are missing, and where the user enters them.
func (c *mcpCall) storageAnswer(v any) map[string]any {
	m := asMap(v)
	out := pick(m, "id", "name", "type", "type_label", "describe", "config_error", "last_test_at", "last_test_ok", "last_test_msg", "in_use")
	typ, _ := m["type"].(string)
	cfg := map[string]string{}
	for k, x := range asMap(m["config"]) {
		s, _ := x.(string)
		cfg[k] = s
	}
	// secrets as lists of labels: under their own keys the final redaction would mask "set"
	settings, set, missing := map[string]any{}, []string{}, []string{}
	sec := storage.SecretKeys(typ)
	for k, s := range cfg {
		if !sec[k] && s != "" {
			settings[k] = s
		}
	}
	for _, f := range storage.SecretFields(typ, cfg) {
		if cfg[f.Key] != "" {
			set = append(set, f.Label)
		} else {
			missing = append(missing, f.Label)
		}
	}
	if m["config"] != nil {
		out["settings"], out["secrets_set"], out["secrets_not_set"] = settings, set, missing
	}
	n, _ := m["id"].(json.Number)
	id, _ := n.Int64()
	out["finish_url"] = c.webURL(fmt.Sprintf("/storages?edit=%d", id))
	if len(missing) > 0 {
		out["next_step"] = "Secrets not set: " + strings.Join(missing, ", ") + " (enter those this storage needs; an OAuth storage is " +
			"authorized with its button). They never go through MCP: ask the user to open finish_url and enter them, then check " +
			"the storage with test_storage."
	} else {
		out["next_step"] = "Check the storage with test_storage."
	}
	return out
}

// moveError explains a change the API refused because it would send a saved secret elsewhere.
func (c *mcpCall) moveError(err error, fragment string) error {
	if err != nil && strings.Contains(err.Error(), "re-enter") {
		return fmt.Errorf("%v. Secrets never go through MCP, so the user makes this change in the web UI: %s", err, c.webURL(fragment))
	}
	return err
}

// ---- drills ----

var drillInputKeys = []string{"name", "job_id", "target_server_id", "schedule", "enabled", "databases", "assertions", "options",
	"notify_success", "notify_failure"}

// drillDefaults is the web UI's new-drill form (web/assets/js/pages/drilledit.js).
func drillDefaults() map[string]any {
	return map[string]any{"name": "", "job_id": 0, "target_server_id": 0, "schedule": "0 5 * * 0", "enabled": true,
		"databases": []any{}, "assertions": []any{},
		"options":        map[string]any{"threads": 2, "no_binlog": false, "max_rto_minutes": 0, "row_counts": ""},
		"notify_success": false, "notify_failure": true}
}

func drillProps() map[string]any {
	return map[string]any{
		"name":             mcpString("Drill name."),
		"job_id":           mcpID("Job whose backups the drill restores (list_jobs)."),
		"target_server_id": mcpID("Sandbox server the backup is restored into, under temporary database names that are dropped afterwards (list_servers); same engine as the job's server."),
		"schedule":         mcpString("When the drill runs: a cron expression (default 0 5 * * 0, Sundays at 05:00) or @manual."),
		"enabled":          mcpBool("Run on the schedule (default true)."),
		"databases":        mcpStrings("Only these databases of the backup (default: all)."),
		"assertions": map[string]any{"type": "array", "maxItems": 50, "description": "SQL checks on the restored copy: the whole list, it " +
			"replaces the current one. Each passes when the first column of its first row is true.",
			"items": mcpObject([]string{"sql"}, map[string]any{
				"name":     mcpString("What it checks."),
				"database": mcpString("Database whose restored copy it runs in."),
				"sql":      mcpString("One SELECT (or WITH … SELECT) query on one line, e.g. SELECT COUNT(*) > 0 FROM orders."),
			})},
		"options": mcpObj("Drill options; only the ones given change.", map[string]any{
			"threads":         mcpCount("Restore threads (default 2).", 0, 32),
			"no_binlog":       mcpBool("MySQL: keep the drill out of the sandbox's binary log."),
			"max_rto_minutes": mcpCount("Fail when the restore takes longer (0: no limit).", 0, 100000),
			"row_counts":      mcpEnum("exact (COUNT(*); default) or estimate (the server's statistics, for big tables).", "exact", "estimate"),
		}),
		"notify_success": mcpBool("Notify when the drill passes (default off)."),
		"notify_failure": mcpBool("Notify when it fails (default on)."),
	}
}

// drillBody fixes what the API spells differently from the schema.
func drillBody(body map[string]any) map[string]any {
	if o := asMap(body["options"]); o != nil && o["row_counts"] == "exact" {
		o["row_counts"] = ""
	}
	return body
}

func briefDrill(v any) map[string]any {
	m := asMap(v)
	out := pick(m, "drill", "running", "last_run", "next_at", "problem")
	if m["same_server"] == true {
		out["note"] = "The sandbox is the job's own server: the drill restores there under temporary database names and drops them afterwards."
	}
	return out
}

func with(m map[string]any, kv ...any) map[string]any {
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// mcpSetupTools look at storages and schedules and test a storage; with a Manage token they
// create and change jobs, restore drills, database servers and storages.
func mcpSetupTools() []mcpTool {
	createJob := mcpObject([]string{"name", "server_id", "method", "targets"}, with(jobProps("Default keep_last 7; a retention given replaces it."),
		"encrypt", mcpBool("Encrypt the backups (age). The passphrase never goes through MCP: with true the job is created paused and "+
			"unencrypted, and the user turns encryption on with a passphrase and enables the job at finish_url. Default false.")))
	updateJob := mcpObject([]string{"job_id"}, with(jobProps("The keys given change, the others stay."), "job_id", mcpID("Job id from list_jobs.")))
	createServer := mcpObject([]string{"name", "username"}, serverProps())
	updateServer := mcpObject([]string{"server_id"}, with(serverProps(), "server_id", mcpID("Server id from list_servers.")))
	storageConfig := map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
		"description": "Settings of the storage type (list_storage_types), by key: every setting that is not secret. Secrets " +
			"(passwords, keys, tokens) are refused: the user enters them in the web UI."}
	createStorage := mcpObject([]string{"name", "type"}, map[string]any{
		"name":   mcpString("Storage name, unique."),
		"type":   mcpEnum("Storage type (list_storage_types).", storageTypeNames()...),
		"config": storageConfig,
	})
	updateStorage := mcpObject([]string{"storage_id"}, map[string]any{
		"storage_id": mcpID("Storage id from list_storages."),
		"name":       mcpString("New name."),
		"config":     with(maps(storageConfig), "description", "Settings to change, by key; the others stay. Secrets are refused."),
	})
	createDrill := mcpObject([]string{"name", "job_id", "target_server_id"}, drillProps())
	updateDrill := mcpObject([]string{"drill_id"}, with(drillProps(), "drill_id", mcpID("Drill id from list_drills.")))

	return []mcpTool{
		{
			Name: "list_storages", Title: "List storages", Route: "GET /api/storages",
			Description: "Where backups are stored: local folders, S3, Google Drive, OneDrive, SFTP and the others, with the result " +
				"of their last test and the jobs that use them. The id is what jobs' targets take.",
			Schema: mcpObject(nil, map[string]any{}),
			Run: mcpRun(func(c *mcpCall, _ struct{}) (any, error) {
				l, err := c.get("/api/storages", nil)
				if err != nil {
					return nil, err
				}
				out := []any{}
				for _, x := range asList(l) {
					out = append(out, pick(asMap(x), "id", "name", "type", "type_label", "describe", "config_error", "last_test_at",
						"last_test_ok", "last_test_msg", "in_use"))
				}
				return map[string]any{"storages": out}, nil
			}),
		},
		{
			Name: "get_storage", Title: "Get a storage", Route: "GET /api/storages/{id}",
			Description: "One storage with its settings (secrets never: secrets_set and secrets_not_set name them) and the result of its last test.",
			Schema:      mcpObject([]string{"storage_id"}, map[string]any{"storage_id": mcpID("Storage id from list_storages.")}),
			Run: mcpRun(func(c *mcpCall, in struct {
				StorageID int64 `json:"storage_id"`
			}) (any, error) {
				p, err := idPath("/api/storages/%d", in.StorageID)
				if err != nil {
					return nil, err
				}
				v, err := c.get(p, nil)
				if err != nil {
					return nil, err
				}
				out := c.storageAnswer(v)
				delete(out, "next_step")
				return out, nil
			}),
		},
		{
			Name: "list_storage_types", Title: "List storage types", Route: "GET /api/storage-types",
			Description: "The kinds of storage DumpCat writes to and the settings of each (for create_storage): key, label, whether " +
				"required, default, choices, and show_if (the setting applies only when another has one of these values). Settings " +
				"marked secret are entered by the user in the web UI, never over MCP. local and rclone storages need the admin role.",
			Schema: mcpObject(nil, map[string]any{}),
			Run: mcpRun(func(c *mcpCall, _ struct{}) (any, error) {
				v, err := c.get("/api/storage-types", nil)
				if err != nil {
					return nil, err
				}
				out := []any{}
				for _, t := range asList(asMap(v)["types"]) {
					tm := asMap(t)
					fields := []any{}
					for _, f := range asList(tm["fields"]) {
						fm := asMap(f)
						x := pick(fm, "key", "label", "type", "required", "default", "help", "secret", "show_if")
						if opts := asList(fm["options"]); len(opts) > 0 {
							vals := []any{}
							for _, o := range opts {
								vals = append(vals, asMap(o)["value"])
							}
							x["choices"] = vals
						}
						fields = append(fields, x)
					}
					out = append(out, with(pick(tm, "type", "label", "description", "oauth"), "fields", fields))
				}
				return map[string]any{"types": out}, nil
			}),
		},
		{
			Name: "preview_schedule", Title: "Preview a schedule", Route: "POST /api/cron/preview",
			Description: "Check a cron expression for a job or drill schedule and show its next five times in DumpCat's time zone; " +
				"changes nothing.",
			Schema: mcpObject([]string{"expr"}, map[string]any{"expr": mcpString("Cron expression (minute hour day-of-month month day-of-week), e.g. 30 1 * * 1-5.")}),
			Run: mcpRun(func(c *mcpCall, in struct {
				Expr string `json:"expr"`
			}) (any, error) {
				return c.post("/api/cron/preview", map[string]any{"expr": in.Expr})
			}),
		},
		{
			Name: "test_storage", Title: "Test a storage", Route: "POST /api/storages/{id}/test", Write: true,
			Description: "Check a storage's settings and credentials now: write, read back and delete a small test file. Returns " +
				"ok, or the error. Needs the operator role and a token with full access.",
			Schema: mcpObject([]string{"storage_id"}, map[string]any{"storage_id": mcpID("Storage id from list_storages.")}),
			Run: mcpRun(func(c *mcpCall, in struct {
				StorageID int64 `json:"storage_id"`
			}) (any, error) {
				p, err := idPath("/api/storages/%d/test", in.StorageID)
				if err != nil {
					return nil, err
				}
				return c.post(p, nil)
			}),
		},
		{
			Name: "create_server", Title: "Add a database server", Route: "POST /api/servers", Write: true, Manage: true,
			Description: "Add a database server to back up (or to restore drills into). No password is taken: when the account " +
				"needs one, the user enters it at finish_url in the web UI; then get_server with check_connection shows the " +
				"version, databases, missing privileges and which backup methods work. Needs the operator role and a token with " +
				"Manage access.",
			Schema: createServer,
			Run: func(c *mcpCall, raw json.RawMessage) (any, error) {
				args, err := mcpArgs(raw, createServer)
				if err != nil {
					return nil, err
				}
				v, err := c.post("/api/servers", args)
				if err != nil {
					return nil, err
				}
				srv := briefServer(v)
				delete(srv, "created_at") // the create answer has no times yet
				delete(srv, "updated_at")
				out := map[string]any{"server": srv}
				id, _ := asMap(v)["id"].(json.Number)
				out["finish_url"] = c.webURL("/servers?edit=" + id.String())
				out["next_step"] = "No password is set: passwords never go through MCP. If the account needs one, ask the user to open " +
					"finish_url and enter it (and the restore user's, if any); then check the server with get_server and check_connection."
				return out, nil
			},
		},
		{
			Name: "update_server", Title: "Change a database server", Route: "PUT /api/servers/{id}", Write: true, Manage: true,
			Description: "Change a database server's settings; only the ones given change. Moving a server with a saved password to " +
				"another host, port, socket or user, or weakening its TLS, needs the password again: that change is made in the web " +
				"UI. Needs the operator role and a token with Manage access.",
			Schema: updateServer,
			Run: func(c *mcpCall, raw json.RawMessage) (any, error) {
				args, err := mcpArgs(raw, updateServer)
				if err != nil {
					return nil, err
				}
				id, err := takeID(args, "server_id")
				if err != nil {
					return nil, err
				}
				p := fmt.Sprintf("/api/servers/%d", id)
				cur, err := c.get(p, nil)
				if err != nil {
					return nil, err
				}
				body := inputOf(asMap(cur), serverInputKeys...)
				overlay(body, args)
				v, err := c.put(p, body)
				if err != nil {
					return nil, c.moveError(err, fmt.Sprintf("/servers?edit=%d", id))
				}
				return map[string]any{"server": briefServer(v)}, nil
			},
		},
		{
			Name: "create_storage", Title: "Add a storage", Route: "POST /api/storages", Write: true, Manage: true,
			Description: "Add a storage that backups are written to, with its settings from list_storage_types. Secrets (access " +
				"keys, passwords, OAuth tokens) are never taken: the answer lists the ones not set and finish_url, where the user " +
				"enters them. Then test_storage. local and rclone storages need the admin role. Needs the operator role and a token " +
				"with Manage access.",
			Schema: createStorage,
			Run: func(c *mcpCall, raw json.RawMessage) (any, error) {
				args, err := mcpArgs(raw, createStorage)
				if err != nil {
					return nil, err
				}
				typ, _ := args["type"].(string)
				cfg, err := storageConfigArgs(typ, args["config"])
				if err != nil {
					return nil, err
				}
				for _, f := range storage.SecretFields(typ, cfg) {
					if f.Required {
						return nil, fmt.Errorf("a %s storage cannot be saved without %s, a secret that never goes through MCP: ask "+
							"the user to add it in the web UI: %s", typeLabel(typ), f.Label, c.webURL("/storages"))
					}
				}
				v, err := c.post("/api/storages", map[string]any{"name": args["name"], "type": typ, "config": cfg})
				if err != nil {
					return nil, err
				}
				return c.storageAnswer(v), nil
			},
		},
		{
			Name: "update_storage", Title: "Change a storage", Route: "PUT /api/storages/{id}", Write: true, Manage: true,
			Description: "Rename a storage or change its settings; only the ones given change and saved secrets stay. Pointing a " +
				"storage with saved secrets at another host, endpoint, account or user needs the secrets again: that change is made " +
				"in the web UI. Needs the operator role and a token with Manage access.",
			Schema: updateStorage,
			Run: func(c *mcpCall, raw json.RawMessage) (any, error) {
				args, err := mcpArgs(raw, updateStorage)
				if err != nil {
					return nil, err
				}
				id, err := takeID(args, "storage_id")
				if err != nil {
					return nil, err
				}
				p := fmt.Sprintf("/api/storages/%d", id)
				cur, err := c.get(p, nil)
				if err != nil {
					return nil, err
				}
				cm := asMap(cur)
				typ, _ := cm["type"].(string)
				change, err := storageConfigArgs(typ, args["config"])
				if err != nil {
					return nil, err
				}
				// the saved config, secrets as the API's mask (kept), with the changes over it
				cfg := map[string]any{}
				for k, x := range asMap(cm["config"]) {
					cfg[k] = x
				}
				for k, x := range change {
					cfg[k] = x
				}
				name := cm["name"]
				if n, ok := args["name"].(string); ok {
					name = n
				}
				v, err := c.put(p, map[string]any{"name": name, "type": typ, "config": cfg})
				if err != nil {
					return nil, c.moveError(err, fmt.Sprintf("/storages?edit=%d", id))
				}
				return c.storageAnswer(v), nil
			},
		},
		{
			Name: "create_job", Title: "Create a backup job", Route: "POST /api/jobs", Write: true, Manage: true,
			Description: "Create a backup job: which server, method, databases, when (cron), where to (storages) and how many to keep. " +
				"Settings not given are those of the web UI's new-job form (daily at 02:00, zstd, keep 7, verify one copy, notify " +
				"on failure). The job backs up on its schedule from now on unless enabled is false; run_backup starts one now. " +
				"Pre/post commands, extra tool arguments and the encryption passphrase are set in the web UI (finish_url). Needs " +
				"the operator role and a token with Manage access.",
			Schema: createJob,
			Run: func(c *mcpCall, raw json.RawMessage) (any, error) {
				args, err := mcpArgs(raw, createJob)
				if err != nil {
					return nil, err
				}
				encrypt := args["encrypt"] == true
				delete(args, "encrypt")
				body := jobDefaults()
				if _, ok := args["retention"]; ok {
					body["retention"] = map[string]any{} // the assistant's retention, not the default's keep_last on top
				}
				overlay(body, args)
				if encrypt {
					body["enabled"] = false // until the user sets the passphrase
				}
				v, err := c.post("/api/jobs", body)
				if err != nil {
					return nil, err
				}
				out := briefJob(v)
				out["finish_url"] = c.webURL(fmt.Sprintf("/jobs/%d/edit", jobIDOf(v)))
				if encrypt {
					out["next_step"] = "The job is paused and not encrypted yet: the passphrase never goes through MCP. Ask the user to " +
						"open finish_url, turn on encryption with a passphrase they keep safe (backups cannot be restored without it) " +
						"and enable the job."
				}
				return out, nil
			},
		},
		{
			Name: "update_job", Title: "Change a backup job", Route: "PUT /api/jobs/{id}", Write: true, Destructive: true, Manage: true,
			Description: "Change a backup job; only the settings given change (options and retention key by key; targets is the whole " +
				"list). enabled false pauses it. Lowering retention removes older backups after the next backup. Encryption, the " +
				"passphrase, pre/post commands and extra tool arguments are changed in the web UI. Needs the operator role and a " +
				"token with Manage access.",
			Schema: updateJob,
			Run: func(c *mcpCall, raw json.RawMessage) (any, error) {
				args, err := mcpArgs(raw, updateJob)
				if err != nil {
					return nil, err
				}
				id, err := takeID(args, "job_id")
				if err != nil {
					return nil, err
				}
				p := fmt.Sprintf("/api/jobs/%d", id)
				cur, err := c.get(p, nil)
				if err != nil {
					return nil, err
				}
				// the saved job (its passphrase stays, pre/post commands go back unchanged) with the changes over it
				body := inputOf(asMap(asMap(cur)["job"]), jobInputKeys...)
				overlay(body, args)
				v, err := c.put(p, body)
				if err != nil {
					return nil, err
				}
				changed := make([]string, 0, len(args))
				for k := range args {
					changed = append(changed, k)
				}
				sort.Strings(changed)
				return with(briefJob(v), "changed", changed), nil
			},
		},
		{
			Name: "create_drill", Title: "Create a restore drill", Route: "POST /api/drills", Write: true, Manage: true,
			Description: "Create a restore drill: on a schedule, restore a job's newest backup into a sandbox server under temporary " +
				"names, check tables, row counts and SQL assertions, then drop the copies. Settings not given are those of the web " +
				"UI's form (Sundays at 05:00, notify on failure). start_drill runs it now. Needs the operator role and a token with " +
				"Manage access.",
			Schema: createDrill,
			Run: func(c *mcpCall, raw json.RawMessage) (any, error) {
				args, err := mcpArgs(raw, createDrill)
				if err != nil {
					return nil, err
				}
				body := drillDefaults()
				overlay(body, args)
				v, err := c.post("/api/drills", drillBody(body))
				if err != nil {
					return nil, err
				}
				return briefDrill(v), nil
			},
		},
		{
			Name: "update_drill", Title: "Change a restore drill", Route: "PUT /api/drills/{id}", Write: true, Manage: true,
			Description: "Change a restore drill; only the settings given change (options key by key; assertions is the whole list). " +
				"enabled false pauses it. Needs the operator role and a token with Manage access.",
			Schema: updateDrill,
			Run: func(c *mcpCall, raw json.RawMessage) (any, error) {
				args, err := mcpArgs(raw, updateDrill)
				if err != nil {
					return nil, err
				}
				id, err := takeID(args, "drill_id")
				if err != nil {
					return nil, err
				}
				p := fmt.Sprintf("/api/drills/%d", id)
				cur, err := c.get(p, nil)
				if err != nil {
					return nil, err
				}
				body := inputOf(asMap(asMap(cur)["drill"]), drillInputKeys...)
				overlay(body, args)
				v, err := c.put(p, drillBody(body))
				if err != nil {
					return nil, err
				}
				return briefDrill(v), nil
			},
		},
	}
}

func storageTypeNames() []string {
	var out []string
	for _, t := range storage.Types {
		out = append(out, t.Type)
	}
	return out
}

// maps copies a schema so a tool can change its description without touching another's.
func maps(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
