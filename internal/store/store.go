// Package store persists application state in an embedded SQLite database.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"dumpcat/internal/secret"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db  *sql.DB
	box *secret.Box
}

func Open(path string, box *secret.Box) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes access and avoids SQLITE_BUSY entirely.
	// Callers must never run a query while iterating another result set.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db, box: box}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Snapshot writes a consistent copy of the database to path while it is in use (VACUUM INTO).
func (s *Store) Snapshot(path string) error {
	os.Remove(path)
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

func (s *Store) Box() *secret.Box { return s.box }

var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE COLLATE NOCASE,
	email TEXT NOT NULL DEFAULT '',
	password_hash TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'viewer',
	totp_secret TEXT NOT NULL DEFAULT '',
	totp_enabled INTEGER NOT NULL DEFAULT 0,
	disabled INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	last_login_at INTEGER
);
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf TEXT NOT NULL,
	ip TEXT NOT NULL DEFAULT '',
	user_agent TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE api_tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	prefix TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	last_used_at INTEGER,
	expires_at INTEGER
);
CREATE TABLE servers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	host TEXT NOT NULL DEFAULT '',
	port INTEGER NOT NULL DEFAULT 3306,
	socket TEXT NOT NULL DEFAULT '',
	username TEXT NOT NULL DEFAULT '',
	password TEXT NOT NULL DEFAULT '',
	tls_mode TEXT NOT NULL DEFAULT '',
	ca_cert TEXT NOT NULL DEFAULT '',
	flavor TEXT NOT NULL DEFAULT 'auto',
	version TEXT NOT NULL DEFAULT '',
	datadir TEXT NOT NULL DEFAULT '',
	service_name TEXT NOT NULL DEFAULT '',
	os_user TEXT NOT NULL DEFAULT '',
	notes TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE storages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	type TEXT NOT NULL,
	config TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	last_test_at INTEGER,
	last_test_ok INTEGER,
	last_test_msg TEXT NOT NULL DEFAULT ''
);
CREATE TABLE jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	uuid TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
	method TEXT NOT NULL,
	db_mode TEXT NOT NULL DEFAULT 'all',
	databases TEXT NOT NULL DEFAULT '[]',
	exclude_tables TEXT NOT NULL DEFAULT '[]',
	schedule TEXT NOT NULL DEFAULT '',
	incr_schedule TEXT NOT NULL DEFAULT '',
	compression TEXT NOT NULL DEFAULT 'zstd',
	compression_level INTEGER NOT NULL DEFAULT 0,
	encrypt INTEGER NOT NULL DEFAULT 0,
	passphrase TEXT NOT NULL DEFAULT '',
	threads INTEGER NOT NULL DEFAULT 4,
	options TEXT NOT NULL DEFAULT '{}',
	retention TEXT NOT NULL DEFAULT '{}',
	enabled INTEGER NOT NULL DEFAULT 1,
	notify_success INTEGER NOT NULL DEFAULT 0,
	notify_failure INTEGER NOT NULL DEFAULT 1,
	timeout_minutes INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE job_targets (
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	storage_id INTEGER NOT NULL REFERENCES storages(id) ON DELETE RESTRICT,
	retention TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (job_id, storage_id)
);
CREATE TABLE runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	uuid TEXT NOT NULL UNIQUE,
	job_id INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
	job_name TEXT NOT NULL DEFAULT '',
	job_uuid TEXT NOT NULL DEFAULT '',
	job_slug TEXT NOT NULL DEFAULT '',
	server_id INTEGER REFERENCES servers(id) ON DELETE SET NULL,
	server_name TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'full',
	parent_id INTEGER,
	base_id INTEGER,
	parent_uuid TEXT NOT NULL DEFAULT '',
	base_uuid TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	trigger TEXT NOT NULL DEFAULT '',
	triggered_by TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	raw_bytes INTEGER NOT NULL DEFAULT 0,
	files TEXT NOT NULL DEFAULT '[]',
	databases TEXT NOT NULL DEFAULT '[]',
	meta TEXT NOT NULL DEFAULT '{}',
	error TEXT NOT NULL DEFAULT '',
	compression TEXT NOT NULL DEFAULT '',
	encrypted INTEGER NOT NULL DEFAULT 0,
	passphrase TEXT NOT NULL DEFAULT '',
	dir_name TEXT NOT NULL DEFAULT '',
	verify_status TEXT NOT NULL DEFAULT '',
	verified_at INTEGER,
	verify_msg TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runs_job ON runs(job_id, started_at);
CREATE INDEX runs_started ON runs(started_at);
CREATE TABLE run_locations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	storage_id INTEGER REFERENCES storages(id) ON DELETE CASCADE,
	storage_name TEXT NOT NULL DEFAULT '',
	storage_type TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL,
	status TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX run_locations_run ON run_locations(run_id);
CREATE TABLE restores (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER REFERENCES runs(id) ON DELETE SET NULL,
	run_uuid TEXT NOT NULL DEFAULT '',
	job_name TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	target_server_id INTEGER REFERENCES servers(id) ON DELETE SET NULL,
	target_server_name TEXT NOT NULL DEFAULT '',
	options TEXT NOT NULL DEFAULT '{}',
	status TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	error TEXT NOT NULL DEFAULT '',
	user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
	username TEXT NOT NULL DEFAULT ''
);
CREATE TABLE notify_channels (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	type TEXT NOT NULL,
	config TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	on_success INTEGER NOT NULL DEFAULT 1,
	on_failure INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	user_id INTEGER,
	username TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT '',
	details TEXT NOT NULL DEFAULT '',
	ip TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_ts ON audit_log(ts);
`,
	// 2: separate (write-capable) credentials used for restores
	`
ALTER TABLE servers ADD COLUMN restore_username TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN restore_password TEXT NOT NULL DEFAULT '';
`,
	// 3: archived binary logs (point-in-time recovery)
	`
CREATE TABLE binlog_files (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
	job_uuid TEXT NOT NULL DEFAULT '',
	job_name TEXT NOT NULL DEFAULT '',
	server_id INTEGER REFERENCES servers(id) ON DELETE SET NULL,
	server_name TEXT NOT NULL DEFAULT '',
	file_name TEXT NOT NULL,
	seq INTEGER NOT NULL DEFAULT 0,
	first_ts INTEGER NOT NULL DEFAULT 0,
	last_ts INTEGER NOT NULL DEFAULT 0,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	stored_bytes INTEGER NOT NULL DEFAULT 0,
	sha256 TEXT NOT NULL DEFAULT '',
	raw_sha256 TEXT NOT NULL DEFAULT '',
	compression TEXT NOT NULL DEFAULT '',
	encrypted INTEGER NOT NULL DEFAULT 0,
	passphrase TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX binlog_files_job ON binlog_files(job_uuid, id);
CREATE TABLE binlog_locations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	binlog_id INTEGER NOT NULL REFERENCES binlog_files(id) ON DELETE CASCADE,
	storage_id INTEGER REFERENCES storages(id) ON DELETE CASCADE,
	storage_name TEXT NOT NULL DEFAULT '',
	storage_type TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
CREATE INDEX binlog_locations_file ON binlog_locations(binlog_id);
`,
	// 4: agents — database hosts that connect out to DumpCat (no inbound port needed)
	`
CREATE TABLE agents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	key_hash TEXT NOT NULL UNIQUE,
	key_prefix TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	last_seen_at INTEGER,
	last_ip TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	hostname TEXT NOT NULL DEFAULT '',
	os TEXT NOT NULL DEFAULT ''
);
ALTER TABLE servers ADD COLUMN agent_id INTEGER REFERENCES agents(id) ON DELETE RESTRICT;
`,
	// 5: built-in WireGuard — database hosts that join DumpCat's VPN
	`
CREATE TABLE wg_peers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	public_key TEXT NOT NULL UNIQUE,
	preshared_key TEXT NOT NULL DEFAULT '',
	address TEXT NOT NULL UNIQUE,
	networks TEXT NOT NULL DEFAULT '[]',
	endpoint TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
ALTER TABLE servers ADD COLUMN wg_peer_id INTEGER REFERENCES wg_peers(id) ON DELETE RESTRICT;
`,
	// 6: restore drills — backups restored into a sandbox and checked on a schedule
	`
CREATE TABLE drills (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	name TEXT NOT NULL DEFAULT '',
	target_server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
	schedule TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	databases TEXT NOT NULL DEFAULT '[]',
	assertions TEXT NOT NULL DEFAULT '[]',
	options TEXT NOT NULL DEFAULT '{}',
	notify_success INTEGER NOT NULL DEFAULT 0,
	notify_failure INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX drills_job ON drills(job_id);
CREATE TABLE drill_runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	drill_id INTEGER REFERENCES drills(id) ON DELETE SET NULL,
	drill_name TEXT NOT NULL DEFAULT '',
	job_id INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
	job_name TEXT NOT NULL DEFAULT '',
	run_id INTEGER REFERENCES runs(id) ON DELETE SET NULL,
	run_dir TEXT NOT NULL DEFAULT '',
	target_server_id INTEGER REFERENCES servers(id) ON DELETE SET NULL,
	target_server_name TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	trigger TEXT NOT NULL DEFAULT '',
	triggered_by TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	restore_ms INTEGER NOT NULL DEFAULT 0,
	check_ms INTEGER NOT NULL DEFAULT 0,
	databases TEXT NOT NULL DEFAULT '[]',
	checks TEXT NOT NULL DEFAULT '[]',
	summary TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	leftover TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX drill_runs_drill ON drill_runs(drill_id, id);
CREATE INDEX drill_runs_run ON drill_runs(run_id, id);
ALTER TABLE runs ADD COLUMN table_stats TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN drill_status TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN drilled_at INTEGER;
ALTER TABLE runs ADD COLUMN drill_msg TEXT NOT NULL DEFAULT '';
`,
	// 7: several database engines — the driver of each server (methods name their engine)
	`
ALTER TABLE servers ADD COLUMN engine TEXT NOT NULL DEFAULT 'mysql';
`,
}

func (s *Store) migrate() error {
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	for i := ver; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---- helpers ----

func now() int64 { return time.Now().Unix() }

func fromTS(v int64) time.Time { return time.Unix(v, 0) }

func fromNullTS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

func nullTS(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

func nullID(p *int64) any {
	if p == nil || *p == 0 {
		return nil
	}
	return *p
}

func fromNullID(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func fromJSON(s string, v any) {
	if s == "" {
		return
	}
	_ = json.Unmarshal([]byte(s), v)
}

func (s *Store) enc(v string) (string, error) { return s.box.Encrypt(v) }

func (s *Store) dec(v string) string {
	out, err := s.box.Decrypt(v)
	if err != nil {
		return ""
	}
	return out
}

func (s *Store) encMap(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	return s.box.Encrypt(toJSON(m))
}

func (s *Store) decMap(v string) map[string]string {
	m := map[string]string{}
	fromJSON(s.dec(v), &m)
	return m
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ---- settings ----

func (s *Store) GetSettings() Settings {
	st := DefaultSettings()
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='app'`).Scan(&v); err == nil {
		fromJSON(v, &st)
	}
	if st.ToolPaths == nil {
		st.ToolPaths = map[string]string{}
	}
	if st.MaxConcurrent <= 0 {
		st.MaxConcurrent = 2
	}
	if st.SessionHours <= 0 {
		st.SessionHours = 12
	}
	if st.DefaultThreads <= 0 {
		st.DefaultThreads = 4
	}
	return st
}

func (s *Store) SaveSettings(st Settings) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES('app',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, toJSON(st))
	return err
}

func (s *Store) GetKV(key string) (string, bool) {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) SetKV(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---- audit ----

func (s *Store) Audit(userID *int64, username, action, target, details, ip string) {
	_, _ = s.db.Exec(`INSERT INTO audit_log(ts,user_id,username,action,target,details,ip) VALUES(?,?,?,?,?,?,?)`,
		now(), nullID(userID), username, action, target, details, ip)
}

func (s *Store) ListAudit(limit, offset int, q string) ([]AuditEntry, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := ""
	args := []any{}
	if q != "" {
		where = ` WHERE username LIKE ? OR action LIKE ? OR target LIKE ? OR details LIKE ?`
		p := "%" + q + "%"
		args = append(args, p, p, p, p)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT id,ts,user_id,username,action,target,details,ip FROM audit_log`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		var uid sql.NullInt64
		if err := rows.Scan(&e.ID, &ts, &uid, &e.Username, &e.Action, &e.Target, &e.Details, &e.IP); err != nil {
			return nil, 0, err
		}
		e.TS = fromTS(ts)
		e.UserID = fromNullID(uid)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func (s *Store) PurgeAudit(before time.Time) {
	_, _ = s.db.Exec(`DELETE FROM audit_log WHERE ts < ?`, before.Unix())
}

// keyCheck is a value encrypted with the master key when the database was first opened with it.
const keyCheckKV = "master_key_check"

// CheckMasterKey refuses a master key that does not open this database's secrets (master.key lost
// and recreated, or the database moved to another host without it). Running anyway would read every
// password and passphrase as empty, and jobs that encrypt backups would have no passphrase.
func (s *Store) CheckMasterKey() error {
	mismatch := errors.New("the master key (DATA_DIR/master.key or DUMPCAT_MASTER_KEY) does not match this database: " +
		"its passwords and passphrases cannot be decrypted. Put back the master.key that belongs to this database " +
		"(it was created next to dumpcat.db on its first start), then start DumpCat again")
	if v, ok := s.GetKV(keyCheckKV); ok {
		if out, err := s.box.Decrypt(v); err != nil || out != "dumpcat" {
			return mismatch
		}
		return nil
	}
	// a database from before this check: its stored secrets must decrypt
	for _, q := range []string{
		`SELECT password FROM servers WHERE password <> '' LIMIT 20`,
		`SELECT passphrase FROM jobs WHERE passphrase <> '' LIMIT 20`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			return err
		}
		bad := false
		for rows.Next() {
			var v string
			if rows.Scan(&v) == nil {
				if _, err := s.box.Decrypt(v); err != nil {
					bad = true
				}
			}
		}
		rows.Close()
		if bad {
			return mismatch
		}
	}
	v, err := s.box.Encrypt("dumpcat")
	if err != nil {
		return err
	}
	return s.SetKV(keyCheckKV, v)
}
