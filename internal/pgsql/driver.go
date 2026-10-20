package pgsql

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

// Driver is the PostgreSQL driver.
type Driver struct{}

var (
	_ engine.LogArchiver      = Driver{}
	_ engine.PhysicalRestorer = Driver{}
)

func (Driver) Engine() string   { return store.EnginePostgres }
func (Driver) Label() string    { return "PostgreSQL" }
func (Driver) Product() string  { return "PostgreSQL" }
func (Driver) DefaultPort() int { return 5432 }

var methods = []engine.Method{
	{ID: store.MethodPgDump, Title: "pg_dump (directory format)", Icon: "file", Drill: true, Estimates: true,
		Description: "Logical backup of each database with pg_dump --format=directory, tables dumped in parallel from one consistent snapshot. Restore individual databases, rename them, restore onto newer PostgreSQL versions with pg_restore (also in parallel).",
		Threads:     "pg_dump / pg_restore --jobs: tables dumped and restored in parallel (one database at a time)",
		Users:       "Roles and tablespaces (pg_dumpall --globals-only)", UsersHelp: "Saved as a separate _globals.sql file; restoring it creates the roles the target does not have. Passwords need a superuser backup account."},
	{ID: store.MethodPgBase, Title: "pg_basebackup (physical)", Icon: "hdd", Physical: true, LogArchive: true,
		Description: "Copies the whole cluster while it runs (every database, role and setting) over a replication connection: nothing to install on the database host. The base of WAL archiving and point-in-time recovery. A restore prepares a data directory on the DumpCat host and can replace a local cluster.",
		Threads:     "Not used: pg_basebackup copies the cluster in one stream"},
}

func (Driver) Methods() []engine.Method { return methods }

var restoreOptions = []engine.RestoreOption{
	{Key: "drop_existing", Group: "options", Label: "Drop target databases before restoring",
		Help: "DROP DATABASE, then create it again. Unchecked: a database that exists keeps its other objects; the objects in the backup are dropped and recreated (`pg_restore --clean --if-exists`). Sessions connected to a database stop the drop."},
	{Key: "restore_users", Group: "options", Needs: "users", Label: "Restore roles",
		Help: "Creates the roles of _globals.sql that the target does not have yet, with their attributes and memberships. Existing roles, the restore account and `pg_*` roles are left alone; tablespaces are not created."},
	{Key: "no_owner", Group: "compat", Label: "Skip ownership (--no-owner)",
		Help: "Objects belong to the restore account instead of their original owners. Needed when those roles don't exist on the target."},
	{Key: "no_privileges", Group: "compat", Label: "Skip privileges (--no-privileges)",
		Help: "No GRANT/REVOKE from the backup. Needed when the roles they name don't exist on the target."},
}

func (Driver) RestoreOptions() []engine.RestoreOption { return restoreOptions }

// CheckName refuses a database name a restore must not create: names are
// passed inside a quoted connection string, but a leading '-' or a control
// character has no business in one either, and PostgreSQL keeps 63 bytes.
func (Driver) CheckName(name string) error {
	if strings.HasPrefix(name, "-") || len(name) > 63 || !utf8.ValidString(name) ||
		strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("invalid target database name: %q", name)
	}
	return nil
}

// Support: pg_dump and pg_restore of the server's major version or newer;
// pg_basebackup and pg_receivewal with a REPLICATION account.
func (Driver) Support(sv *store.Server, info *dbconn.ServerInfo, t *engine.Tools) []engine.MethodSupport {
	return []engine.MethodSupport{dumpSupport(info, t), baseSupport(info, t)}
}

func baseSupport(info *dbconn.ServerInfo, t *engine.Tools) engine.MethodSupport {
	ms := engine.MethodSupport{Method: store.MethodPgBase}
	bb, err := t.PGFor("pg_basebackup", info.Major)
	if err != nil {
		ms.Message = err.Error()
		return ms
	}
	repl := false
	for _, g := range info.Grants {
		repl = repl || g == "SUPERUSER" || g == "REPLICATION"
	}
	switch {
	case !repl:
		ms.Message = bb.Version + " — the backup account needs the REPLICATION attribute (ALTER ROLE … REPLICATION) and a replication line in pg_hba.conf"
		return ms
	case info.WALLevel == "minimal":
		ms.Message = bb.Version + " — wal_level is minimal: set it to replica"
		return ms
	}
	ms.OK, ms.Message = true, bb.Version
	if _, err := t.PGFor("pg_receivewal", info.Major); err != nil {
		ms.Message += " — WAL archiving needs pg_receivewal"
	}
	return ms
}

func dumpSupport(info *dbconn.ServerInfo, t *engine.Tools) engine.MethodSupport {
	ms := engine.MethodSupport{Method: store.MethodPgDump}
	dump, err := t.PGFor("pg_dump", info.Major)
	if err != nil {
		ms.Message = err.Error()
		return ms
	}
	restore, err := t.PGFor("pg_restore", info.Major)
	if err != nil {
		ms.Message = dump.Version + " — restores need pg_restore: " + err.Error()
		return ms
	}
	ms.OK, ms.Message = true, dump.Version
	if restore.Major != dump.Major {
		ms.Message += " · " + restore.Version
	}
	if _, err := t.PGFor("pg_dumpall", info.Major); err != nil {
		ms.Message += " — roles are not backed up without pg_dumpall"
	}
	return ms
}

// Compat compares the backup's server with the target.
func (Driver) Compat(run *store.Run, target *store.Server, info *dbconn.ServerInfo, same bool) ([]string, engine.RestoreOpts) {
	var warn []string
	var sug engine.RestoreOpts
	srcVersion, _ := run.Meta["server_version"].(string)
	src := major(srcVersion)
	if src > 0 && info.Major > 0 && info.Major < src {
		warn = append(warn, fmt.Sprintf("The target (PostgreSQL %s) is older than the source (PostgreSQL %s): features of the newer version may fail to load.", info.Version, srcVersion))
	}
	if !same {
		// the roles owning the objects and named by GRANTs rarely exist on another server
		sug.NoOwner, sug.NoPrivileges = true, true
	}
	return warn, sug
}

// major reads 16 from "16.15", 9 from "9.6.24".
func major(v string) int {
	n := 0
	for _, c := range strings.TrimSpace(v) {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
