# Design: several database engines (drivers)

Status: phase 1 implemented (MySQL/MariaDB moved behind the interface, PostgreSQL logical backups and restores).

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
| `LogArchiver` (optional, later) | continuous log archiving and point-in-time recovery | binlog archiver (still in the runner) | WAL archiving (phase 2) |

`RestoreSource` is the runner's side of a restore: the backup's files, a reader that decrypts, decompresses and
checks the SHA-256 of a file, the pre-restore check (every file opens with the passphrase before anything is
dropped), progress and phases. So a driver never sees storages or the catalog.

Registry: `internal/drivers` (`For(engine)`, `ForMethod(method)`, `All()`). It imports `engine` (MySQL) and
`pgsql` (PostgreSQL), which only import `engine` and `store`, so there is no cycle.

What stays MySQL-only for now, gated by the method's features instead of an interface: physical backups and
their restore (extract, prepare, replace the datadir), binlog archiving and PITR (`runner/binlog.go`,
`runner/pitr.go`), the restore "fast mode". They move behind `LogArchiver` when the second implementation (WAL)
exists to shape that interface; abstracting from one implementation would guess wrong.

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
- **Not in phase 1**: WAL archiving and PITR (phase 2), `pg_basebackup` (phase 3), database-level settings
  (`ALTER DATABASE … SET`, database GRANTs: `pg_dump` writes them only with `--create`).

## Later engines

Redis (RDB snapshot through `redis-cli --rdb`, restore = file + restart), MongoDB (`mongodump --oplog`, oplog
replay as its `LogArchiver`), SQL Server (`BACKUP DATABASE … TO URL/DISK`). Each is a new package registered in
`internal/drivers`, a new engine choice in the server form, and its method ids.
