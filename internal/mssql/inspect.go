package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/store"
)

// SystemDatabases are not part of "all databases"; tempdb cannot be backed
// up at all, the others can when a job names them.
var SystemDatabases = map[string]bool{"master": true, "model": true, "msdb": true, "tempdb": true}

// Engine editions (SERVERPROPERTY('EngineEdition')).
const (
	edStandard   = 2 // also Web and Business Intelligence
	edEnterprise = 3 // also Developer and Evaluation
	edExpress    = 4
	edAzureDB    = 5
	edSynapse    = 6
	edManaged    = 8
	edEdge       = 9
	edServerless = 11
)

// versionName names a major version: 16 → SQL Server 2022.
func versionName(major int) string {
	names := map[int]string{10: "2008", 11: "2012", 12: "2014", 13: "2016", 14: "2017", 15: "2019", 16: "2022", 17: "2025"}
	if n, ok := names[major]; ok {
		return "SQL Server " + n
	}
	return fmt.Sprintf("SQL Server (version %d)", major)
}

// Inspect reads the version, edition, account, databases and the backup
// folder of a server; withSizes adds sizes and table counts.
func (Driver) Inspect(ctx context.Context, s *store.Server, withSizes bool) (*dbconn.ServerInfo, error) {
	db, err := Open(ctx, s, "")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	in, err := inspect(ctx, db, s, withSizes)
	if err != nil {
		return nil, err
	}
	return in.ServerInfo, nil
}

// instance is what DumpCat knows of a server after inspecting it.
type instance struct {
	*dbconn.ServerInfo
	engineEdition int
	sysadmin      bool
	createDB      bool
	defaultBackup string
	dataDir       string
	logDir        string
	offset        time.Duration // the server's local time minus UTC (BACKUP and STOPAT times are local)
	folder        folder
	folderErr     error
}

func inspect(ctx context.Context, db *sql.DB, s *store.Server, withSizes bool) (*instance, error) {
	info := &dbconn.ServerInfo{Engine: store.EngineMSSQL, Flavor: "mssql"}
	in := &instance{ServerInfo: info}
	info.Version = queryString(ctx, db, "SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128))")
	if info.Version == "" {
		if err := db.QueryRowContext(ctx, "SELECT @@VERSION").Scan(new(string)); err != nil {
			return nil, err
		}
	}
	parts := strings.Split(info.Version, ".")
	if len(parts) > 0 {
		info.Major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) > 2 {
		info.Minor, _ = strconv.Atoi(parts[2]) // the build: 16.0.4135.4 → 4135
	}
	full := queryString(ctx, db, "SELECT @@VERSION")
	info.VersionComment, _, _ = strings.Cut(full, "\n")
	info.VersionComment = strings.TrimSpace(info.VersionComment)
	info.Edition = queryString(ctx, db, "SELECT CAST(SERVERPROPERTY('Edition') AS nvarchar(128))")
	if n, ok := queryInt(ctx, db, "SELECT CAST(SERVERPROPERTY('EngineEdition') AS int)"); ok {
		in.engineEdition = int(n)
	}
	info.Platform = queryString(ctx, db, "SELECT host_platform FROM sys.dm_os_host_info") // 2017+
	if info.Platform == "" {
		info.Platform = "Windows"
	}
	info.Hostname = queryString(ctx, db, "SELECT CAST(SERVERPROPERTY('ServerName') AS nvarchar(256))")
	info.CurrentUser = queryString(ctx, db, "SELECT SUSER_SNAME()")
	if up, ok := queryInt(ctx, db, "SELECT DATEDIFF(SECOND, sqlserver_start_time, SYSDATETIME()) FROM sys.dm_os_sys_info"); ok {
		info.Uptime = up
	}
	if m, ok := queryInt(ctx, db, "SELECT DATEDIFF(MINUTE, GETUTCDATE(), GETDATE())"); ok {
		in.offset = time.Duration(m) * time.Minute
	}
	info.IsReplica = queryString(ctx, db, "SELECT CAST(SERVERPROPERTY('IsHadrEnabled') AS nvarchar(8))") == "1" &&
		queryString(ctx, db, "SELECT TOP 1 role_desc FROM sys.dm_hadr_availability_replica_states WHERE is_local = 1") == "SECONDARY"
	info.ServiceAccount = queryString(ctx, db, "SELECT TOP 1 service_account FROM sys.dm_server_services WHERE servicename LIKE 'SQL Server (%'")
	if info.ServiceAccount == "" && info.Platform == "Linux" {
		info.ServiceAccount = "mssql" // the user of the packages and container images (sys.dm_server_services is Windows only)
	}
	in.dataDir = queryString(ctx, db, "SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(1024))")
	in.logDir = queryString(ctx, db, "SELECT CAST(SERVERPROPERTY('InstanceDefaultLogPath') AS nvarchar(1024))")
	if in.dataDir == "" { // before 2012: the folder of master's data file
		in.dataDir = parentDir(queryString(ctx, db, "SELECT physical_name FROM sys.master_files WHERE database_id = 1 AND file_id = 1"))
	}
	if in.logDir == "" {
		in.logDir = in.dataDir
	}
	info.Datadir = in.dataDir
	in.defaultBackup = queryString(ctx, db, "SELECT CAST(SERVERPROPERTY('InstanceDefaultBackupPath') AS nvarchar(1024))")
	if in.defaultBackup == "" {
		in.defaultBackup = queryString(ctx, db, `DECLARE @d nvarchar(1024); EXEC master.dbo.xp_instance_regread N'HKEY_LOCAL_MACHINE', N'Software\Microsoft\MSSQLServer\MSSQLServer', N'BackupDirectory', @d OUTPUT; SELECT @d`)
	}
	in.sysadmin = queryString(ctx, db, "SELECT CAST(IS_SRVROLEMEMBER('sysadmin') AS nvarchar(4))") == "1"
	in.createDB = in.sysadmin || queryString(ctx, db, "SELECT CAST(HAS_PERMS_BY_NAME(NULL, NULL, 'CREATE ANY DATABASE') AS nvarchar(4))") == "1"
	if in.sysadmin {
		info.Grants = append(info.Grants, "sysadmin")
	} else {
		for _, p := range []string{"CREATE ANY DATABASE", "VIEW SERVER STATE", "VIEW ANY DEFINITION"} {
			if queryString(ctx, db, "SELECT CAST(HAS_PERMS_BY_NAME(NULL, NULL, @p1) AS nvarchar(4))", p) == "1" {
				info.Grants = append(info.Grants, p)
			}
		}
	}
	info.Compression = compressionEdition(in.engineEdition, info.Edition, info.Major)
	switch in.engineEdition {
	case edAzureDB, edSynapse, edServerless, edManaged, edEdge:
		info.Warnings = append(info.Warnings, fmt.Sprintf("%s does not write backups to a disk (BACKUP … TO DISK): DumpCat backs up SQL Server instances (Windows, Linux, containers), not Azure SQL Database or Managed Instance.", nonEmpty(info.Edition, "This edition")))
	}
	if info.Major > 0 && info.Major < 11 {
		info.Warnings = append(info.Warnings, versionName(info.Major)+" is older than SQL Server 2012, the oldest version DumpCat is tested with.")
	}
	if info.IsReplica {
		info.Warnings = append(info.Warnings, "This is a secondary replica of an availability group: only copy-only full backups and log backups run here (turn on \"Copy-only\" in the job).")
	}
	dbs, err := listDatabases(ctx, db, withSizes)
	if err != nil {
		return nil, err
	}
	info.Databases = dbs
	var noBackup, simple []string
	for _, d := range dbs {
		info.TotalBytes += d.SizeBytes
		if d.System {
			continue
		}
		if d.RecoveryModel == "SIMPLE" {
			simple = append(simple, d.Name)
		}
		if d.Tables < 0 {
			noBackup = append(noBackup, d.Name)
		}
	}
	for i := range info.Databases {
		info.Databases[i].Tables = max(info.Databases[i].Tables, 0)
	}
	if len(noBackup) > 0 {
		info.Warnings = append(info.Warnings, fmt.Sprintf("The account may not back up %s: make it a member of db_backupoperator there (or sysadmin).", listNames(noBackup)))
	}
	if len(simple) > 0 {
		info.Warnings = append(info.Warnings, fmt.Sprintf("%s use%s the SIMPLE recovery model: full and differential backups work, log backups and point-in-time recovery need FULL (ALTER DATABASE … SET RECOVERY FULL, then a full backup).", listNames(simple), map[bool]string{true: "s", false: ""}[len(simple) == 1]))
	}
	if !in.createDB {
		info.Warnings = append(info.Warnings, "The account may not create databases: restores need a restore account with CREATE ANY DATABASE (dbcreator) or sysadmin, and DumpCat cannot read the headers of its backup files (log chains are then not checked).")
	}
	in.folder, in.folderErr = resolveFolder(s, in.defaultBackup)
	if in.folderErr == nil {
		info.BackupDir, info.BackupDirVia = in.folder.Server, in.folder.via()
		if why, err := probe(ctx, db, in.folder, info.ServiceAccount); err != nil {
			in.folderErr = err
		} else if why != "" {
			info.Warnings = append(info.Warnings, "DumpCat could not check that SQL Server reads the backup folder ("+why+"): the first backup tells.")
		}
	}
	if in.folderErr != nil {
		info.BackupDirError = in.folderErr.Error()
	}
	return in, nil
}

// compressionEdition tells whether BACKUP … WITH COMPRESSION runs: Standard
// (2008 R2+), Enterprise and Developer; not Express or Web.
func compressionEdition(engineEdition int, edition string, major int) bool {
	switch engineEdition {
	case edEnterprise:
		return true
	case edStandard:
		return major >= 11 && !strings.HasPrefix(strings.ToLower(edition), "web")
	}
	return false
}

func listDatabases(ctx context.Context, db *sql.DB, withSizes bool) ([]dbconn.DBInfo, error) {
	rows, err := db.QueryContext(ctx, `SELECT d.name, d.collation_name, d.recovery_model_desc, d.state_desc, d.source_database_id,
		CAST(ISNULL(HAS_PERMS_BY_NAME(QUOTENAME(d.name), 'DATABASE', 'BACKUP DATABASE'), 0) AS int),
		(SELECT SUM(CAST(mf.size AS bigint)) * 8192 FROM sys.master_files mf WHERE mf.database_id = d.database_id AND mf.type <> 1)
		FROM sys.databases d ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	var out []dbconn.DBInfo
	for rows.Next() {
		var d dbconn.DBInfo
		var coll, model, state sql.NullString
		var src sql.NullInt64
		var canBackup int
		var size sql.NullInt64
		if err := rows.Scan(&d.Name, &coll, &model, &state, &src, &canBackup, &size); err != nil {
			rows.Close()
			return nil, err
		}
		d.Collation, d.RecoveryModel, d.State, d.SizeBytes = coll.String, model.String, state.String, size.Int64
		// snapshots, tempdb and databases that are not online cannot be backed up
		d.System = SystemDatabases[d.Name] || src.Valid || d.State != "ONLINE"
		if canBackup == 0 && !d.System {
			d.Tables = -1 // marks "no permission" for the warning above
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !withSizes {
		return out, nil
	}
	for i := range out {
		if out[i].System || out[i].Tables < 0 || ctx.Err() != nil {
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if n, ok := queryInt(tctx, db, "SELECT COUNT(*) FROM "+QuoteName(out[i].Name)+".sys.tables WHERE is_ms_shipped = 0"); ok {
			out[i].Tables = int(n)
		}
		cancel()
	}
	return out, nil
}

func listNames(names []string) string {
	if len(names) > 5 {
		return strings.Join(names[:5], ", ") + fmt.Sprintf(" and %d more", len(names)-5)
	}
	return strings.Join(names, ", ")
}

func nonEmpty(v, alt string) string {
	if v != "" {
		return v
	}
	return alt
}

// parentDir is the folder of a file on the SQL Server host.
func parentDir(p string) string {
	i := strings.LastIndexAny(p, `\/`)
	if i <= 0 {
		return ""
	}
	return p[:i]
}
