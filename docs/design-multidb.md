# Design: several database engines (drivers)

Status: phase 1 implemented (MySQL/MariaDB moved behind the interface, PostgreSQL logical backups and restores);
phase 2 implemented (the `LogArchiver` interface, MySQL binlogs moved behind it, PostgreSQL WAL archiving,
`pg_basebackup` and point-in-time recovery); SQL Server implemented as the third engine (native backups,
differentials, log backups and PITR, see [SQL Server](#sql-server-third-engine)).

## Goal

DumpCat backs up MySQL and MariaDB. Most of it is not about MySQL at all: jobs, the scheduler, the
compression / encryption / SHA-256 pipeline, storages, retention, verification, notifications, monitoring,
agents and WireGuard (plain TCP tunnels), the catalog and the UI shell. A **driver** owns only what is specific
to one database engine, so PostgreSQL (and later Redis, MongoDB, SQL Server) reuses everything else.

## The interface

`internal/engine/driver.go`:

| Part | What the driver does | MySQL | PostgreSQL |
|---|---|---|---|
| `Inspect(ctx, server, sizes)` | version, databases + sizes, privileges / warnings, data directory, replica | `dbconn` (go-sql-driver/mysql) | `internal/pgsql` (pgx) |
| `Methods()` | the backup methods: id, title, description, icon, what they support (incremental, drills, binlog, physical) | mysqldump, mydumper, physical | pg_dump |
| `Support(server, info, tools)` | which methods work on this host now (tools found, versions) | as before | pg_dump / pg_restore ≥ server major |
| `Backup(env, spec, sink) → Result` | writes the backup files into the shared sink (compression, encryption, hashing are not its business) | mysqldump.go, mydumper.go, physical.go | `pg_dump -Fd -j N` per database + `pg_dumpall --globals-only` |
| `Restore(env, source, spec)` | loads a logical backup into a server, incl. a rename to a new database | mysql / myloader (moved out of the runner) | `pg_restore -j N` |
| `RestoreOptions()` | the options the restore page shows (label, help, which methods) | fast mode, no binlog, DEFINER, collations, GTID_PURGED | `--no-owner`, `--no-privileges` |
| `Compat(run, target, same)` | notes and suggested options for a restore onto another instance | version / flavor checks | another server: suggest no owner / privileges |
| `Sandbox` (optional) | restore drills: tables and rows of a restored copy, assertions, drop the copies | information_schema, `COUNT(*)` | `pg_class`, `COUNT(*) FROM ONLY` or `reltuples` after `ANALYZE` |
| `LogArchiver` (optional) | continuous log archiving and point-in-time recovery (see below) | binlogs: `mysqlbinlog --raw --stop-never`, replay with `mysqlbinlog \| mysql` | WAL: `pg_receivewal` + replication slot, replay by PostgreSQL's own recovery |
| `PhysicalRestorer` (optional) | prepares a physical backup on the DumpCat host, optionally replaces a local data directory | (xtrabackup/mariabackup, still in the runner) | `pg_basebackup` tar → data directory → recovery with a temporary server |

`RestoreSource` is the runner's side of a restore: the backup's files, a reader that decrypts, decompresses and
checks the SHA-256 of a file, the pre-restore check (every file opens with the passphrase before anything is
dropped), progress and phases. So a driver never sees storages or the catalog.

Registry: `internal/drivers` (`For(engine)`, `ForMethod(method)`, `All()`). It imports `engine` (MySQL) and
`pgsql` (PostgreSQL), which only import `engine` and `store`, so there is no cycle.

What stays MySQL-only, gated by the method's features instead of an interface: xtrabackup/mariabackup backups
and their restore (extract, prepare, replace the datadir: `runner/restore.go`), incremental backups, the restore
"fast mode". Binlog archiving and PITR moved behind `LogArchiver` in phase 2, shaped by the second
implementation (WAL).

## Store and formats

- `servers.engine` (migration 7, default `mysql`). The server form asks for it (MySQL / MariaDB, PostgreSQL) and
  defaults the port (3306 / 5432). The flavor field stays MySQL's (auto, mysql, mariadb, percona).
- Method ids are global and unique, so `jobs.method` and `runs.method` need no engine column: the existing
  `mysqldump`, `mydumper`, `physical` keep meaning MySQL, PostgreSQL adds `pg_dump` (later `pg_basebackup`). A job's
  method must belong to its server's engine; a server with jobs cannot change engine.
- `manifest.json` gets `"engine"`; manifests without it are MySQL. Import accepts every known method.

## PostgreSQL, phase 1

- **Connection: pgx** (github.com/jackc/pgx/v5, MIT) rather than running `psql`: connection tests and inspection
  work before any client tool is installed, results are typed, TLS is configured the same way as for MySQL, and
  no process sees a password. Cost: one dependency tree (pgx, pgpassfile, pgservicefile, puddle, x/text), listed in
  THIRD_PARTY_NOTICES.md.
- **Tools**: `pg_dump`, `pg_restore`, `pg_dumpall` found per major version (`/usr/lib/postgresql/<n>/bin`,
  `/usr/pgsql-<n>/bin`, `C:\Program Files\PostgreSQL\<n>\bin`, DUMPCAT_TOOLS_DIR, the tools folder, a path set in
  Settings). The backup uses the tool of the server's major version or the closest newer one; an older one is
  refused. A restore needs a `pg_restore` at least as new as the `pg_dump` that wrote the archive.
- **Passwords** go into a 0600 `PGPASSFILE` in the task's private directory, never on a command line or in the
  environment. The connection is a conninfo string DumpCat builds and quotes itself (`dbname='a=b' host=…`), so a
  database name can never become connection options (`pg_dump -d "a=b"` would) or a command-line option.
- **Backup**: per database `pg_dump --format=directory --jobs=N --compress=0` into the staging directory, then
  tarred into `<db>.pgdump.tar` through the shared pipeline (zstd/gzip + age + SHA-256). Rows per table come from
  the archive itself (`pg_restore --list` maps TABLE DATA entries to their `NNNN.dat` files, whose lines are counted
  up to `\.`), the same numbers the MySQL row counter gives drills. `Users & grants` becomes
  `pg_dumpall --globals-only` → `_globals.sql` (roles, memberships, tablespaces; `--no-role-passwords` when the
  backup user is not a superuser). Excluded tables: `database.table` or `database.schema.table`.
- **Restore**: extract the tar to the staging directory, create the database when missing (`TEMPLATE template0`,
  the source's encoding and locale, without the locale if the target lacks it), then
  `pg_restore --jobs=N --exit-on-error`, with `--clean --if-exists` when the database already existed (objects of
  the dump are replaced, others stay, like MySQL), `--no-owner` / `--no-privileges` when chosen (suggested for
  another server). "Drop target databases first" drops and recreates. Roles are applied statement by statement
  over pgx (no psql, so no meta-commands from a backup file), only for roles the target does not have yet, never for
  the restore account itself or `pg_*` roles.
- **Names**: database names are checked like MySQL's (no leading `-`, no control characters, at most 63 bytes).
- **Drills**: temporary copies `<db>__drill_<id>` (63 bytes max), `COUNT(*) FROM ONLY` per table, or, with the
  drill option *Estimate row counts*, `ANALYZE` + `pg_class.reltuples` (differences over 10 % are warnings).
  Copies are dropped `WITH (FORCE)` (PostgreSQL 13+); they are DumpCat's own databases.
- **Agents / WireGuard**: unchanged, the tunnel carries TCP to 5432 (agents need `-allow host:5432`).
- **Not in phase 1**: WAL archiving and PITR (phase 2), `pg_basebackup` (phase 2), database-level settings
  (`ALTER DATABASE … SET`, database GRANTs: `pg_dump` writes them only with `--create`).

## Log archiving and point-in-time recovery (phase 2)

Two implementations shaped one interface. What both need, and the runner now owns once:

| Shared (runner) | Where |
|---|---|
| one supervised archiver per job: session loop with back-off, restart / close-now requests, failure notification after 3 failures, a log file per job | `runner/logarchive.go` |
| staging: a private folder per job (`binlog-<job>` / `wal-<job>`), removed when the job is deleted | same |
| upload of every closed file to every storage of the job through the job's compression + age pipeline, size check after upload, retry with back-off, re-upload to storages added later | same |
| the catalog: one row per archived file (`binlog_files` + `binlog_locations`, the Go types are `store.LogFile` / `store.LogLocation`), engine details in a JSON `meta` column (migration 8) | `store/logfiles.go` |
| retention: per storage, files archived before the oldest kept backup started are deleted (archive order, never event times) | `runner/retention.go` |
| live status and lag: bytes behind the server, how long the archive has been behind, files waiting for upload; the monitor's lag alert and the dashboard warnings | `engine.LogStatus` |
| the PITR plan: base backup selection (newest usable backup consistent before the target), the chain of archived files after it, the file still being received, coverage (events are complete up to), the timeline of the UI | `runner/pitr.go` |
| the restore: every file is downloaded, both SHA-256 checked and handed to the driver to check **before** anything is changed on the target | same |

What a driver implements (`internal/engine/logarchive.go`; MySQL: `engine/binlog_archive.go`, PostgreSQL:
`pgsql/walarchive.go` and `pgsql/walpitr.go`):

```go
type LogArchiver interface {
	LogInfo() LogInfo                // names ("binlog", "WAL segment"), staging prefix, PITR target types, Recovery
	NewStream(h *LogHost) LogStream  // receiving side of one job, kept between sessions
	CheckTarget(t *PITRTarget) error // the engine's own target types (time and latest are shared)
	Base(run *store.Run, t *PITRTarget) (*LogBase, error) // where replay starts from a backup, or why it cannot
	Plan(in *PlanInput) (*LogPlan, error)                 // the chain after the base, the file being received, coverage
	NewCheck(env *Env, plan *LogPlan) LogCheck            // fetched files, in order: resolve the stop, refuse mismatches
	Replay(env *Env, rp *Replay, spec *ReplaySpec) error  // after the base restore (MySQL); not called when LogInfo().Recovery
}

type LogStream interface {
	Session(ctx context.Context, job *store.Job, server *store.Server, rotate <-chan struct{}) error
	Closed() []string                                     // staged files ready for upload, in order
	Seal(ctx context.Context, job *store.Job, name string) (*LogFileInfo, error) // read (and repair) before upload
	Archived(name string)                                 // every storage has it: drop (or keep) the staged copy
	Current() LogCurrent                                  // the file being received
	Snapshot(dst string) (*LogSnapshot, error)            // its complete part, for a restore
	Release(ctx context.Context, server *store.Server, lg *tasklog.Logger) // archiving switched off for good
}
```

The runner's `LogHost` gives a stream its staging folder, log, tools, the catalog and two callbacks: change the
live status, and kick the uploader. A restart request cancels the session's context; a new session forgets the
last one's "level with the server" check. `engine.PhysicalRestorer` (PostgreSQL) gets the checked `Replay` with
the physical restore, so its recovery replays the WAL.

| | MySQL / MariaDB (binlog) | PostgreSQL (WAL) |
|---|---|---|
| receiving | `mysqlbinlog --read-from-remote-server --raw --stop-never`, server id 3900000000+job | `pg_receivewal --slot=<slot> --no-loop` over a replication connection, not synchronous |
| position kept by | the file names (resume at the newest staged or the next after the last archived) | the physical replication slot `dumpcat_job<id>_<uuid>` (`pg_create_physical_replication_slot(…, true)`: WAL reserved at once) and the newest archived segment kept in staging |
| closed file | every file but the current one | complete 16 MB segments and timeline `.history` files; `<seg>.partial` is the current one |
| scan | binlog events: first/last event time, GTIDs, truncated tail | WAL pages and records (`pgsql/wal.go`): CRC-checked records, commit/abort and restore point times, restore point names, system identifier, LSN range, `XLOG_SWITCH` |
| close now | `FLUSH BINARY LOGS` (RELOAD) | `pg_switch_wal()` (superuser or `GRANT EXECUTE`) |
| PITR targets | time, position, GTID, latest | time, LSN, restore point name, latest |
| base | logical (per-database positions) or physical | `pg_basebackup` only: WAL needs a physical base |
| replay | `mysqlbinlog --start/--stop-position \| mysql` after the base restore | PostgreSQL recovery: `recovery.signal`, `restore_command` reading the fetched WAL, `recovery_target_*`, `recovery_target_action = promote` |
| switched off | nothing on the server | the slot is dropped (otherwise the server keeps WAL for ever) |

Decisions:

- **pg_basebackup is part of phase 2** (method `pg_basebackup`, full only), because WAL can only be replayed on
  a physical copy of the cluster: a pg_dump base could at most give "latest consistent", which is what pg_dump
  already is. WAL archiving is offered for `pg_basebackup` jobs only; pg_dump stays logical-only.
- **Backup**: `pg_basebackup --format=tar --pgdata=- --wal-method=fetch --checkpoint=fast` writes one tar
  stream (data directory, the WAL of the backup in `pg_wal/`, `backup_manifest`) straight into the sink:
  nothing is staged. Except on Windows, where pg_basebackup writes stdout in text mode (every LF becomes CRLF and
  the tar is corrupt): there it writes `base.tar` (and `backup_manifest`, stored as a file of its own) into the
  staging folder first. A temporary physical slot, held by DumpCat's connection while the backup runs, keeps the
  server from removing that WAL before it is fetched. While the stream passes, DumpCat reads `backup_label`,
  `backup_manifest` and `global/pg_control` from it: start and end LSN, timeline, system identifier. Clusters with
  additional tablespaces cannot be written to one stream and are refused (later: one file per tablespace).
- **The slot** reserves WAL from the moment it is created, so nothing is lost while DumpCat restarts, and
  `pg_receivewal` resumes after the newest segment in staging (DumpCat keeps the newest archived one there for
  that). Disabling the job, turning archiving off, deleting the job or moving it to another server drops the
  slot; stopping DumpCat does not. A slot left behind by a deleted DumpCat installation must be dropped by hand.
  When the slot lost WAL (`wal_status = lost`, `max_slot_wal_keep_size`), the gap is logged and streaming starts
  again from the server's position: PITR cannot cross it, the next base backup starts a new chain. When the
  server's timeline left the staged one before the staged WAL ends (`TIMELINE_HISTORY`: the cluster was restored
  to an earlier point, e.g. by DumpCat's own *replace*, or a standby was promoted behind DumpCat),
  `pg_receivewal` could never resume there ("not in this server's history"): the staged files are archived as
  they are and streaming starts again from the slot on the new timeline, whose history file is archived too.
- **Recovery** runs PostgreSQL itself on the prepared directory: base extracted, the fetched WAL in
  `<datadir>/dumpcat_wal`, `restore_command` relative to the data directory. When the PostgreSQL server
  binaries of the backup's major version are on the DumpCat host, DumpCat starts a temporary server (own config
  file, 127.0.0.1 on a free port, no unix socket, `archive_mode=off`, `hot_standby=off`, no preloaded
  libraries; as the data directory's owner when DumpCat runs as root), waits until it has promoted, stops it and
  removes its own files: the directory is then a clean data directory at the target, on a new timeline. Without
  the binaries the directory is left ready to recover (settings in `postgresql.auto.conf`, `recovery.signal`)
  and the log says how to start it. As root, the owner (the OS user, `postgres` by default; `execx.AsUser`) must
  be able to enter the directory: the parents DumpCat creates get mode 0755 whatever the umask (the service's is
  0027), `execx.Reachable` checks the others and otherwise leaves the directory ready to recover with a log line
  (the default under `/var/lib/dumpcat/restore` is never reachable), and `/tmp` / `/var/tmp` are refused when
  they are the service's private ones (`PrivateTmp=true` in `dumpcat.service`: invisible to other users, deleted
  when DumpCat stops; `execx.PrivateTmp` reads `/proc/self/mountinfo`). A data directory without
  `postgresql.conf` / `pg_hba.conf` / `pg_ident.conf` (Debian/Ubuntu keep them in `/etc/postgresql`) gets
  minimal ones, local connections only, so that `pg_ctl -D` starts it. A target time after the last commit in the WAL, or an LSN at its very end,
  replays everything (PostgreSQL 13+ would otherwise stop with "recovery ended before the target was reached").
- **Replacing a local data directory** (Linux, admins, the same checks as the MySQL datadir: `engine.CheckDatadir`
  with `PG_VERSION` as the marker, and the existing directory's version must match):
  the backup is prepared and recovered in a sibling `<datadir>.dumpcat-restore-<time>` (same filesystem), then
  the service is stopped, the old directory renamed to `<datadir>.before-restore-<time>`, the new one moved in
  and the service started; any error moves the old one back and keeps the restored one as `….failed`. The
  service is detected as `postgresql@<major>-main` (Debian/Ubuntu, configuration in `/etc/postgresql`, which the
  swap does not touch), `postgresql-<major>` (PGDG on RHEL) or `postgresql`.
- Timeline history files are never removed by retention; a plan takes every archived segment of the chain's
  segment numbers on timelines ≥ the base's, so a promotion of the source after the base is followed.
- **Not in phase 2**: incremental `pg_basebackup` (PostgreSQL 17), tablespaces, recovery on another host than
  DumpCat's (copy the prepared directory), archiving from `archive_command` instead of streaming, restore drills of
  physical backups.

Verified (Windows dev box, PostgreSQL 16.15, MariaDB 11.4.13): `TestWALPITRE2E` archives the WAL of a real server,
restarts the stream and DumpCat itself, and recovers to a time between two transactions, to a restore point, to an
LSN, to the latest state (with rows written while DumpCat was down) and to the backup itself, also without the
server binaries; `TestBinlogPITRE2E` does the same for MariaDB binlogs; unit tests cover the WAL reader (records
across pages and segments, split headers, damaged records, switches) and the plans.

Verified on Linux (Ubuntu 22.04, systemd 249, PostgreSQL 16.15 from PGDG, DumpCat installed by `install.sh` as
the root service): a pg_basebackup job with WAL archiving (the tar on a pipe straight into the storage, no staging
folder; `pg_receivewal --slot=dumpcat_job<id>_<uuid>`); restores to a time and to a restore point recovered by a
temporary server running as `postgres`, a refused default directory under `/var/lib/dumpcat` (logged, left ready to
recover), new nested folders under `/srv`; replacing `/var/lib/postgresql/16/main` of `postgresql@16-main` to a
restore point (checked with `systemctl` and `psql`) and the rollback when the service does not start with the
restored data; archiving across that replace (timeline 1 → 2, forked before the archive's end) and a recovery to
the latest state through both timelines. `TestWALPITRE2E` passes there as `postgres` and as root, and
`TestPostgresE2E` as `postgres`, against a Debian cluster (`pg_createcluster`). Four bugs this found are fixed: the
service's private `/tmp`, parents created 0700 under the umask, no configuration files in a Debian directory, and the
archiver stuck after a timeline fork.

Not verified: RHEL-family hosts (`postgresql-<major>` service, `/var/lib/pgsql/<major>/data`, SELinux and
`restorecon`), PostgreSQL other than 16, standbys as the source, tablespaces (refused), incremental pg_basebackup
(PostgreSQL 17).

## SQL Server (third engine)

Package `internal/mssql`, engine `mssql`, one method `mssql_native` (Physical: false — it backs up databases, which
can be selected, renamed and restored one by one — Differential, Drill, Estimates, LogArchive).

- **Connection: go-mssqldb** (github.com/microsoft/go-mssqldb, BSD-3-Clause; with golang-sql/civil, golang-sql/sqlexp,
  shopspring/decimal). No client tool at all: `BACKUP` and `RESTORE` are T-SQL statements that SQL Server runs itself.
  The connection is built field by field (`msdsn.Config`), never from a connection string with the password in it.
  Encryption is always on except with *disabled*; *verify* checks the certificate against the server's real name, also
  through a tunnel. `host\INSTANCE` with no port asks the SQL Server Browser; SQL logins or `DOMAIN\user` (NTLM, and
  SSPI on Windows). Informational messages of a batch (`STATS = 10`, "Processed N pages") are read through
  `sqlexp.ReturnMessage` and logged; errors keep every message of the batch (`3201` + "Operating system error 5", then
  `3013`).
- **Getting the files: the backup folder.** `BACKUP … TO DISK` writes on the SQL Server host and `RESTORE` reads
  there, so the design question is how a `.bak` reaches DumpCat. Options considered:

  | Approach | Verdict |
  |---|---|
  | A folder both reach: the same path (DumpCat on the SQL Server host), a share, a mount, a container volume | **Chosen.** Two paths per server (`servers.backup_dir` as SQL Server sees it, `backup_dir_local` as DumpCat sees it; migration 11). Works with every edition and version, on Windows and Linux; the files touch a disk (the folder needs room for the databases backed up at once). |
  | `BACKUP … TO URL` against an S3 endpoint served by DumpCat | SQL Server 2022+ only, needs a certificate the SQL Server host trusts and an S3 server in DumpCat; no. |
  | VDI (virtual device interface) | Streams without a file, but only on the SQL Server host, through a COM/shared-memory API: no pure-Go client; no. |
  | Reading the file back through T-SQL (`OPENROWSET(BULK …, SINGLE_BLOB)`) | Whole file in one value, 2 GB at most; no. |
  | The agent on the SQL Server host reads the folder and streams the file over its connection | The natural next step for hosts DumpCat cannot share a folder with: the agent's framing (`frameStdout`…) already streams physical MySQL backups. **Follow-up.** |

  File names are `dumpcat-<12 hex>-<database>.bak`/`.trn`, random so that tasks and DumpCat installations sharing a
  folder never meet; DumpCat opens only regular files it named, and removes them when done (also after errors). As
  root on Linux, files DumpCat writes there get the folder's owner (`mssql`), mode 0640. The connection test writes
  a probe file through the local path and asks SQL Server to read it through its own (`RESTORE HEADERONLY`: "not a
  backup" or "the volume is empty" means it was read; "Operating system error 2/3/5" tells which mistake).
- **Backup**: per database (in parallel, *Threads*) `BACKUP DATABASE … WITH [DIFFERENTIAL,] CHECKSUM, FORMAT, INIT,
  SKIP, STATS = 10, [NO_]COMPRESSION, [COPY_ONLY,] NAME` (+ validated extra options), then `RESTORE HEADERONLY`
  (fallback: msdb's backup history), optionally `RESTORE VERIFYONLY WITH CHECKSUM`, the row counts of
  `sys.partitions`, then the file is copied into the sink (`Compress: false` when SQL Server compressed it) and
  deleted. The headers go into `run.Meta["mssql"]` per database (type, LSN range, checkpoint, database backup LSN,
  differential base LSN/GUID, backup set GUID, recovery model, copy-only, checksums, bulk-logged, start/finish in UTC —
  SQL Server writes local times, converted with the server's offset).
- **Differentials**: new run type `differential` (`store.Chained` = incremental or differential: chains, retention,
  deletion, restores, drills, PITR bases, import). `ParentID = BaseID` = the newest full backup of the job on the same
  server, complete, not copy-only; `engine.Differ` lets the driver refuse it when `sys.database_files.differential_base_guid`
  no longer is that full backup's backup set (a full backup taken elsewhere) — the run becomes a full backup. The
  header of the new differential is checked once more against the base.
- **Restore** (`Driver.Restore`, the logical path of the runner; `RestoreSpec.Chain` carries the full backup's run and
  source, `RestoreSpec.Replay` the checked PITR): plan every database first (names, existing databases, the files in
  the chain), preflight every file, then per database stage the files in the target's folder (SHA-256 checked while
  copying), `RESTORE HEADERONLY` (the backup set must be the catalog's) and `RESTORE VERIFYONLY`; when the folder has
  room for every database, all are staged and checked before the first is replaced. `RESTORE FILELISTONLY` →
  `MOVE` every logical file: the replaced database's own files, else the instance default data/log folders named
  after the target (`<name>.mdf`, `<name>_<logical>.ndf`, `<name>_log.ldf`, a folder for FILESTREAM/memory-optimized),
  avoiding files of other databases. Replace = `SET SINGLE_USER WITH ROLLBACK IMMEDIATE` + `WITH REPLACE`.
- **Drills** (`engine.Sandbox` + the new optional `engine.SandboxChecker`): `TempName` (128 characters), tables from
  `sys.tables`/`sys.partitions`, `COUNT_BIG(*)`, assertions in a transaction that is rolled back, copies dropped after
  `SET SINGLE_USER`; `CheckCopies` runs `DBCC CHECKDB … WITH NO_INFOMSGS, ALL_ERRORMSGS` on every copy (check kind
  `integrity`). Row counts come from `sys.partitions` right after each database's backup — exact when nothing wrote
  during the backup; the estimate mode tolerates 10 %.
- **Log archiving** (`LogInfo` name "transaction log", prefix `trn`, targets time / mark / lsn / latest,
  `Recovery: true`): the stream's `Session` runs a round of `BACKUP LOG` for every database of the job in the FULL or
  BULK_LOGGED model every `binlog_rotate_minutes` (0 = 15) or when asked to rotate, keeping its schedule across
  sessions; each file is moved into staging as `<seq 8 digits>-<database>.trn` with its header in a `.json` beside it,
  marked transactions from `msdb.dbo.logmarkhistory` (only since the database's `create_date`: msdb keeps the history
  of every database that had the name, and a new one starts at similar LSNs). `Seal` gives `First` = the previous log
  backup's start, `Last` = this one's start (what it surely holds). Nothing is being received (`Current` empty,
  `Snapshot` refuses), `Release` only logs that the log now grows.
- **PITR**: `Base` accepts full and differential native backups with at least one database in FULL/BULK_LOGGED;
  `Plan` builds, per database, the chain of log backups after the base's last LSN (the first must start at or before
  it, each next one exactly where the previous ended), cuts it at the target (time: the first log backup that started
  at or after it; LSN: the one whose range holds it; mark: the one that recorded it, or the whole chain when msdb was
  not readable), and reports per database why it cannot (SIMPLE, a gap, the archive too short) — the plan succeeds when
  one database can, and the restore refuses the selected ones that cannot. `NewCheck` re-checks the LSN continuity of
  the fetched files. The driver's `Restore` applies them: `NORECOVERY` for the base, then each log backup
  `WITH STOPAT|STOPATMARK, RECOVERY` until `sys.databases.state_desc` is ONLINE (SQL Server leaves the database
  restoring when the target lies beyond a log backup); a mark never reached is an error, a time beyond the archive
  recovers at its end.
- **Not in this version**: the file transport through the agent; logins (server objects: a `_logins.sql` like
  `pg_dumpall --globals-only` would be the equivalent); file/filegroup and partial backups; Azure SQL Database and
  Managed Instance (`BACKUP TO URL` only); TDE-encrypted databases need their certificate on the target (SQL Server
  says so); availability group secondaries take copy-only full and log backups only (warned).

Verified: `TestMSSQLE2E` against SQL Server 2022 Developer (16.0.4305.5) on Ubuntu 22.04, time zone UTC+7, with the
backup folder under the same path and under a second path (bind mount): full and differential backups of a FULL and a
SIMPLE database with odd names (`o'd]d`), verification, restore under new names (full + differential), refusal and
replace of an existing database, a drill with `DBCC CHECKDB`, the switch to a full backup after a foreign full backup,
log archiving and recovery to a time, a mark, an LSN given in fn_dblog's hex form and the latest log backup, the
SIMPLE database refused, and a chain broken by `BACKUP LOG … TO DISK = 'NUL'`. `TestInspectE2E` checks the inspection
(no warning for a sysadmin, the folder check, a wrong mapping found). Unit tests cover LSNs, the plans, the file
placement, the options check and the folder paths. Not verified: SQL Server on Windows, older versions than 2022,
shares across machines (SMB), Windows authentication, named instances, TLS verify.

## Later engines

Redis (RDB snapshot through `redis-cli --rdb`, restore = file + restart), MongoDB (`mongodump --oplog`, oplog
replay as its `LogArchiver`). Each is a new package registered in `internal/drivers`, a new engine choice in the
server form, and its method ids.
