package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/store"
)

// MySQL is the driver of MySQL, MariaDB and Percona Server.
type MySQL struct{}

const EngineMySQL = "mysql"

func (MySQL) Engine() string   { return EngineMySQL }
func (MySQL) Label() string    { return "MySQL / MariaDB" }
func (MySQL) Product() string  { return "MySQL" }
func (MySQL) DefaultPort() int { return 3306 }

var mysqlMethods = []Method{
	{ID: store.MethodMysqldump, Title: "mysqldump / mariadb-dump", Icon: "file", Drill: true, LogArchive: true,
		Description: "Single-threaded logical backup with --single-transaction (no InnoDB locking). Most flexible: restore individual databases, rename them, move across versions. Best for databases under ~50 GB.",
		Threads:     "mysqldump: number of databases dumped in parallel",
		Users:       "Users & grants (GRANT)", UsersHelp: "Saved as a separate _users.sql file; restoring it is optional."},
	{ID: store.MethodMydumper, Title: "mydumper / myloader", Icon: "zap", Drill: true, LogArchive: true,
		Description: "Multi-threaded logical backup that splits large tables into chunks; parallel restore is many times faster. Best for large databases that still need logical restores.",
		Threads:     "Number of mydumper dump threads",
		Users:       "Users & grants (GRANT)", UsersHelp: "Saved as a separate _users.sql file; restoring it is optional."},
	{ID: store.MethodPhysical, Title: "Physical (XtraBackup / MariaBackup)", Icon: "hdd", Physical: true, Incremental: true, LogArchive: true,
		Description: "Copies data files while the database is running (hot backup, no InnoDB locking), with incremental support. Fastest for very large databases (100 GB–TB); restore is a file copy. Runs on the database server: DumpCat installed there, or an agent started with --allow-physical.",
		Threads:     "Number of files copied in parallel (--parallel)"},
}

func (MySQL) Methods() []Method { return mysqlMethods }

var mysqlRestoreOptions = []RestoreOption{
	{Key: "drop_existing", Group: "options", Label: "Drop target databases before restoring",
		Help: "DROP DATABASE, then recreate. Unchecked: tables in the dump are recreated (DROP TABLE IF EXISTS) but other tables are left untouched."},
	{Key: "fast_mode", Group: "options", Default: true, Label: "Fast mode",
		Help: "Temporarily sets innodb_flush_log_at_trx_commit=2 & sync_binlog=0 during the restore, then restores them — much faster for large databases."},
	{Key: "no_binlog", Group: "options", Label: "Don't write binlog (sql_log_bin=0)",
		Help: "The restore (including DROP/CREATE DATABASE) is not replicated to replicas. Requires SUPER / SYSTEM_VARIABLES_ADMIN — without it the restore stops before changing anything."},
	{Key: "restore_users", Group: "options", Needs: "users", Label: "Restore users & grants",
		Help: "Runs _users.sql (CREATE USER IF NOT EXISTS + GRANT)."},
	{Key: "strip_definer", Group: "compat", Label: "Remove DEFINER from views, routines, triggers and events",
		Help: "They are created with the restore account as definer. Needed when the original accounts don't exist on the target, or the restore user lacks SET_USER_ID / SUPER."},
	{Key: "convert_collations", Group: "compat", Label: "Convert collations the target may not know",
		Help: "MySQL 8 `utf8mb4_0900_*` and MariaDB `*_uca1400_*` → `utf8mb4_unicode_ci` / `utf8mb4_bin`; `utf8mb3_*` → `utf8_*`. Needed for MySQL 8 → MariaDB or MySQL 5.7."},
	{Key: "skip_gtid_purged", Group: "compat", Methods: []string{store.MethodMysqldump}, Label: "Skip GTID_PURGED",
		Help: "Drops `SET @@GLOBAL.GTID_PURGED` from the dump: it fails on a server with its own GTID history and needs SUPER. Keep it only to build a replica of the source."},
}

func (MySQL) RestoreOptions() []RestoreOption { return mysqlRestoreOptions }

func (MySQL) Inspect(ctx context.Context, s *store.Server, withSizes bool) (*dbconn.ServerInfo, error) {
	info, err := dbconn.Inspect(ctx, s, withSizes)
	if info != nil {
		info.Engine = EngineMySQL
	}
	return info, err
}

// Flavor is the server's flavor: as configured, or as detected.
func Flavor(s *store.Server, info *dbconn.ServerInfo) string {
	if s.Flavor != "" && s.Flavor != "auto" {
		return s.Flavor
	}
	return info.Flavor
}

func (MySQL) Backup(env *Env, spec *BackupSpec, sink Sink) (*Result, error) {
	switch spec.Job.Method {
	case store.MethodMysqldump:
		return Mysqldump(env, spec, sink)
	case store.MethodMydumper:
		return Mydumper(env, spec, sink)
	case store.MethodPhysical:
		return Physical(env, spec, sink)
	}
	return nil, fmt.Errorf("unsupported method: %s", spec.Job.Method)
}

// CheckName refuses a target database name: a leading '-' would be parsed
// as a client option.
func (MySQL) CheckName(name string) error {
	if strings.HasPrefix(name, "-") || len(name) > 64 || strings.ContainsRune(name, 0) {
		return fmt.Errorf("invalid target database name: %q", name)
	}
	return nil
}

// Support tells which methods the tools of this host can run. Physical
// backups through an agent are judged by the caller (it knows the agents).
func (MySQL) Support(sv *store.Server, info *dbconn.ServerInfo, t *Tools) []MethodSupport {
	flavor := Flavor(sv, info)
	var out []MethodSupport
	dump := t.DumpTool(flavor)
	client := t.Get(ClientToolName(flavor))
	ms := MethodSupport{Method: store.MethodMysqldump, OK: dump.Found && client.Found}
	if ms.OK {
		ms.Message = dump.Version
	} else if !dump.Found {
		ms.Message = "Missing " + dump.Name + ": " + dump.Hint
	} else {
		ms.Message = "Missing client " + client.Name + " (needed for restores): " + client.Hint
	}
	out = append(out, ms)
	md := t.Get("mydumper")
	ml := t.Get("myloader")
	ms = MethodSupport{Method: store.MethodMydumper, OK: md.Found && ml.Found}
	if ms.OK {
		ms.Message = md.Version
	} else {
		ms.Message = "Missing mydumper/myloader: " + md.Hint
	}
	out = append(out, ms)
	bname, sname := PhysicalToolNames(flavor)
	bt := t.Get(bname)
	st := t.Get(sname)
	ms = MethodSupport{Method: store.MethodPhysical, OK: bt.Found && st.Found}
	dd := sv.Datadir
	if dd == "" {
		dd = info.Datadir
	}
	switch {
	case sv.WGPeerID != nil && *sv.WGPeerID > 0:
		ms.OK = false
		ms.Message = "Not through WireGuard: physical backups copy the datadir, so the backup tool must run on the database host — connect through an agent started with --allow-physical, or install DumpCat there"
	case !bt.Found:
		ms.Message = "Missing " + bname + ": " + bt.Hint
	case !st.Found:
		ms.Message = "Missing " + sname + ": " + st.Hint
	default:
		var note string
		ms.OK, note = PhysicalToolMatches(flavor, bt.Version, info.Version)
		ms.Message = bt.Version + note
		if dd != "" {
			if _, err := os.Stat(dd); err != nil {
				ms.OK = false
				ms.Message += " — datadir " + dd + " is not accessible from this machine (physical backups must run on the database server itself, with read access to the datadir, or through an agent started with --allow-physical)"
			}
		}
	}
	out = append(out, ms)
	return out
}

var reVer = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func verParts(s string) (int, int, int) {
	m := reVer.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return a, b, c
}

// PhysicalToolMatches checks the backup tool's version against the server's;
// note explains a mismatch (or a patch-level advice).
func PhysicalToolMatches(flavor, toolVersion, serverVersion string) (ok bool, note string) {
	sMaj, sMin, sPatch := verParts(serverVersion)
	if flavor == "mariadb" {
		tMaj, tMin, tPatch := verParts(toolVersion)
		if tMaj != sMaj || tMin != sMin {
			return false, fmt.Sprintf(" — does NOT match server version %d.%d (mariabackup must match the MariaDB version)", sMaj, sMin)
		} else if tPatch < sPatch {
			return true, " — use a mariabackup with the same patch level as the server"
		}
		return true, ""
	}
	// "xtrabackup version 8.0.35-30 based on MySQL server 8.0.35"
	tMaj, tMin, _ := verParts(toolVersion)
	if sMaj == 5 && sMin == 7 && !(tMaj == 2 && tMin == 4) {
		return false, " — MySQL 5.7 requires Percona XtraBackup 2.4"
	} else if sMaj >= 8 && (tMaj != sMaj || tMin != sMin) {
		return false, fmt.Sprintf(" — MySQL %d.%d requires XtraBackup %d.%d", sMaj, sMin, sMaj, sMin)
	}
	return true, ""
}

var reVersion = regexp.MustCompile(`^(\d+)\.(\d+)`)

func parseMajorMinor(v string) (major, minor int) {
	m := reVersion.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, 0
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return
}

func isMariaDB(flavor, version string) bool {
	return flavor == "mariadb" || strings.Contains(strings.ToLower(version), "mariadb")
}

// Compat compares the source of a backup with the target instance and
// suggests the rewrites a logical restore needs to load there.
func (MySQL) Compat(run *store.Run, target *store.Server, info *dbconn.ServerInfo, same bool) ([]string, RestoreOpts) {
	return MySQLCompat(run, Flavor(target, info), info.Version, same)
}

// MySQLCompat compares a MySQL backup with a target of the given flavor and version.
func MySQLCompat(run *store.Run, tgtFlavor, tgtVersion string, same bool) ([]string, RestoreOpts) {
	var warn []string
	var sug RestoreOpts
	srcVersion, _ := run.Meta["server_version"].(string)
	srcFlavor, _ := run.Meta["flavor"].(string)
	srcMaria, tgtMaria := isMariaDB(srcFlavor, srcVersion), isMariaDB(tgtFlavor, tgtVersion)
	sMaj, sMin := parseMajorMinor(srcVersion)
	tMaj, tMin := parseMajorMinor(tgtVersion)
	name := func(maria bool, v string) string {
		if maria {
			return "MariaDB " + v
		}
		return "MySQL " + v
	}
	src, tgt := name(srcMaria, srcVersion), name(tgtMaria, tgtVersion)
	known := srcVersion != "" && tgtVersion != ""

	if run.Method == store.MethodPhysical {
		switch {
		case !known:
		case srcMaria != tgtMaria:
			warn = append(warn, fmt.Sprintf("A physical backup of %s cannot be started by %s — use a logical backup to move between MySQL and MariaDB.", src, tgt))
		case sMaj != tMaj || sMin != tMin:
			warn = append(warn, fmt.Sprintf("The backup comes from %s but the target runs %s: a physical backup needs the same version series (the target's binaries will start the restored datadir).", src, tgt))
		}
		return warn, sug
	}

	if known {
		switch {
		case srcMaria != tgtMaria:
			warn = append(warn, fmt.Sprintf("Moving from %s to %s: tables, views and routines usually load once collations are converted, but engine- or flavor-specific features (JSON columns, sequences, system-versioned tables, auth plugins of restored users) may need manual fixes. Point-in-time recovery cannot replay binlogs across MySQL and MariaDB.", src, tgt))
			sug.ConvertCollations = true
		case sMaj > tMaj || (sMaj == tMaj && sMin > tMin):
			warn = append(warn, fmt.Sprintf("The target (%s) is older than the source (%s): syntax or features of the newer version may fail to load.", tgt, src))
			// utf8mb4_0900_* and utf8mb3_* collations are MySQL 8.0+
			if !srcMaria && sMaj >= 8 && tMaj < 8 {
				sug.ConvertCollations = true
			}
		}
	}
	if !same {
		// definer accounts of views/routines/triggers rarely exist on another instance
		sug.StripDefiner = true
	}
	if gtid, _ := run.Meta["gtid"].(string); gtid != "" && run.Method == store.MethodMysqldump && !srcMaria {
		warn = append(warn, "The dump sets GTID_PURGED: this fails on a server that already has a GTID history and needs SUPER. Skip it unless you are building a replica of the source.")
		sug.SkipGTIDPurged = true
	}
	return warn, sug
}

var errNoMethod = errors.New("this backup cannot be restored by the MySQL driver")
