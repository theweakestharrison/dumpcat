<p align="center">
  <img src="web/assets/favicon.svg" width="112" alt="DumpCat logo">
</p>

<h1 align="center">DumpCat</h1>

<p align="center">
  <strong>Backup, restore and point-in-time recovery for MySQL and MariaDB, PostgreSQL and SQL Server.</strong><br>
  One self-hosted binary with a web UI. No database locks, no runtime to install, works offline.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <img alt="Go 1.26 or newer" src="https://img.shields.io/badge/go-1.26%2B-00ADD8">
  <img alt="Runs on Linux and Windows" src="https://img.shields.io/badge/platform-Linux%20%7C%20Windows-555">
  <img alt="MySQL 5.7 to 8.4, MariaDB 10 and 11" src="https://img.shields.io/badge/MySQL%205.7--8.4%20%C2%B7%20MariaDB%2010--11-4479A1">
  <img alt="PostgreSQL 10 and newer" src="https://img.shields.io/badge/PostgreSQL-10%2B-336791">
  <img alt="SQL Server 2012 and newer" src="https://img.shields.io/badge/SQL%20Server-2012%2B-CC2927">
</p>

<p align="center">
  <a href="https://dumpcat.cc"><b>dumpcat.cc</b></a> · <a href="https://dumpcat.cc/#download">Download</a> ·
  <a href="https://dumpcat.cc/demo/">Live demo</a> ·
  <a href="https://github.com/theweakestharrison/DumpCat">GitHub</a> ·
  <b>English</b> · <a href="README.vi.md">Tiếng Việt</a>
</p>

---

DumpCat takes care of the whole backup lifecycle of MySQL, MariaDB, PostgreSQL and SQL Server servers from your browser. It schedules backups,
compresses and encrypts them, copies them to the storages you choose, checks that they can be read back, and restores
them when you need to, down to the second with binlog archiving. It runs on Linux or Windows next to your database, as a
single Go binary with the UI built in.

## Highlights

- **No locking.** InnoDB is backed up from a consistent snapshot while your application keeps reading and writing.
- **Three backup engines.** `mysqldump` / `mariadb-dump`, parallel `mydumper` / `myloader`, and hot physical backups with
  Percona XtraBackup or MariaBackup, including incrementals.
- **PostgreSQL too.** `pg_dump` in directory format with tables dumped and restored in parallel, roles from
  `pg_dumpall --globals-only`, restores under new names or onto another server, and restore drills; `pg_basebackup` for
  the whole cluster. The client tools of each server's major version are picked automatically.
- **And SQL Server.** Native full and differential backups (`WITH CHECKSUM`, compressed where the edition allows,
  checked with `RESTORE VERIFYONLY`), copy-only when another tool owns the chain, restores under new names or onto
  another server through SQL Server's backup folder, restore drills with `DBCC CHECKDB`, and scheduled log backups for
  recovery to a time, a marked transaction or an LSN. No client tool to install.
- **Point-in-time recovery.** Continuous binlog archiving restores to a timestamp, a binlog position or the moment just
  before a GTID, for example just before an accidental `DROP TABLE`; continuous WAL archiving (`pg_receivewal` through a
  replication slot) does the same for PostgreSQL, to a timestamp, a restore point or a WAL position, and scheduled
  `BACKUP LOG` for SQL Server (`STOPAT` / `STOPATMARK`), with the log chain checked before anything is restored.
- **Storage anywhere.** Local disk or NFS, S3 and compatibles (Cloudflare R2, MinIO, Wasabi…), Google Drive,
  OneDrive / SharePoint, FTP / FTPS, SFTP, WebDAV, Backblaze B2, Azure Blob, Dropbox and every rclone backend. One job can
  write to several storages, each with its own retention.
- **Databases behind a firewall.** A small agent on the database host connects out to DumpCat, or the host joins
  DumpCat's built-in WireGuard (off by default), so no port has to be opened to the database. Backups, restores and
  point-in-time recovery work through either.
- **Encrypted and verified.** zstd compression, [age](https://age-encryption.org) encryption before anything leaves the
  server, SHA-256 for every file, and every backup read back right after it is written: decrypted, decompressed and
  checked, so a broken copy shows up at once instead of at restore time.
- **Knows when backups stop.** Every job is checked against its own schedule: when no backup has succeeded in time, or
  binlog archiving falls behind, DumpCat alerts you once and again when it recovers. A weekly email digest sums up the
  week, and a Prometheus `/metrics` endpoint (off by default) feeds your dashboards and alert rules.
- **Restore drills.** On a schedule, DumpCat restores the newest backup into a sandbox server under temporary names,
  compares every table and its row count with what the backup recorded, runs your SQL assertions, records how long the
  restore took (RTO) and drops the copies. Backups that passed are marked **Restore-tested**; a failed drill alerts you.
- **Careful restores.** Restore under new database names or onto another instance or version (with `DEFINER`,
  collation and `GTID_PURGED` rewrites), test-decrypt every file before anything is dropped, fast load mode, and
  automatic rollback when a physical restore replaces the datadir.
- **Built for teams.** Admin, operator and viewer roles, TOTP two-factor login, read-only, full or Manage API tokens, an audit
  log, and notifications by HTML email, Telegram, Slack, Discord or HMAC-signed webhook.
- **Works with AI assistants.** An [MCP](docs/manual.md#ai-assistants-mcp) endpoint lets Claude Desktop, Claude Code
  and other MCP clients check status, read backups and logs and investigate failures, read-only by default; with a
  full-access token they can also run backups, verifications and restore drills, and with a Manage token set up and
  change servers, storages, jobs and drills. Passwords, keys and passphrases never go through the assistant.
- **Standard formats.** Every backup carries a `manifest.json` and uses zstd / gzip / age / xbstream, so you can restore
  it with `age`, `zstd` and `mysql` alone, or import the catalog into a fresh DumpCat.
- **Safe self-updates.** `dumpcat update` waits for running jobs, backs up its own database, can require signed
  releases, and rolls back if the new version fails to start.

## What DumpCat supports

✅&nbsp;supported · ❌ not supported · — does not apply to this database

| | <img src="docs/img/db/mysql.svg" width="110" alt="MySQL logo"><br>**MySQL** | <img src="docs/img/db/percona.svg" width="110" alt="Percona logo"><br>**Percona&nbsp;Server** | <img src="docs/img/db/mariadb.svg" width="110" alt="MariaDB logo"><br>**MariaDB** | <img src="docs/img/db/postgresql.svg" width="110" alt="PostgreSQL logo"><br>**PostgreSQL** | <img src="docs/img/db/sqlserver.svg" width="110" alt="SQL Server"><br>**SQL Server** |
|---|:---:|:---:|:---:|:---:|:---:|
| **Versions** | 5.7 · 8.0 · 8.4 | 5.7 · 8.0 · 8.4 | 10.x · 11.x | 10 and newer | 2012 and newer<sup>1</sup> |
| ***Backup*** | | | | | |
| Logical backup | ✅&nbsp;mysqldump | ✅&nbsp;mysqldump | ✅&nbsp;mariadb&#8209;dump | ✅&nbsp;pg_dump | —<sup>2</sup> |
| Parallel dump and restore | ✅&nbsp;mydumper | ✅&nbsp;mydumper | ✅&nbsp;mydumper | ✅&nbsp;pg_dump&nbsp;-j | — |
| Hot physical backup | ✅&nbsp;XtraBackup<sup>3</sup> | ✅&nbsp;XtraBackup<sup>3</sup> | ✅&nbsp;MariaBackup<sup>3</sup> | ✅&nbsp;pg_basebackup | ✅&nbsp;BACKUP&nbsp;DATABASE |
| Incremental / differential | ✅&nbsp;incremental | ✅&nbsp;incremental | ✅&nbsp;incremental | ❌<sup>4</sup> | ✅&nbsp;differential |
| Users, roles and grants | ✅ | ✅ | ✅ | ✅&nbsp;roles | ❌<sup>5</sup> |
| Point-in-time recovery | ✅&nbsp;binlog | ✅&nbsp;binlog | ✅&nbsp;binlog | ✅&nbsp;WAL | ✅&nbsp;log backups |
| ***Restore*** | | | | | |
| Restore under a new name | ✅<sup>6</sup> | ✅<sup>6</sup> | ✅<sup>6</sup> | ✅<sup>6</sup> | ✅ |
| Restore onto another server or version | ✅<sup>7</sup> | ✅<sup>7</sup> | ✅<sup>7</sup> | ✅<sup>7</sup> | ✅<sup>8</sup> |
| Every file checked before any data is replaced | ✅ | ✅ | ✅ | ✅ | ✅ |
| Scheduled restore drills | ✅<sup>6</sup> | ✅<sup>6</sup> | ✅<sup>6</sup> | ✅<sup>6</sup> | ✅&nbsp;+&nbsp;DBCC&nbsp;CHECKDB |
| ***Around every backup*** | | | | | |
| Compression, age encryption, SHA-256 | ✅ | ✅ | ✅ | ✅ | ✅ |
| Read back and verified after each backup | ✅ | ✅ | ✅ | ✅ | ✅ |
| Every storage, schedules, GFS retention | ✅ | ✅ | ✅ | ✅ | ✅ |
| Overdue-backup alerts, weekly digest, Prometheus metrics | ✅ | ✅ | ✅ | ✅ | ✅ |
| Through the agent (no inbound port) | ✅<sup>3</sup> | ✅<sup>3</sup> | ✅<sup>3</sup> | ✅ | ✅<sup>9</sup> |
| Through the built-in WireGuard | ✅<sup>3</sup> | ✅<sup>3</sup> | ✅<sup>3</sup> | ✅ | ✅<sup>9</sup> |
| REST API and MCP for AI assistants | ✅ | ✅ | ✅ | ✅ | ✅ |
| ***DumpCat runs on*** | | | | | |
| Linux (amd64, arm64) | ✅ | ✅ | ✅ | ✅ | ✅ |
| Windows | ✅<sup>10</sup> | ✅<sup>10</sup> | ✅<sup>11</sup> | ✅<sup>12</sup> | ✅ |

1. On Windows, Linux and in containers; not Azure SQL Database or Managed Instance.
2. SQL Server has no logical dump tool; DumpCat uses SQL Server's own native backups.
3. XtraBackup and MariaBackup run on the database host: install DumpCat there, or start the agent there with
   `--allow-physical`. Logical backups work over any connection.
4. `pg_basebackup` takes full backups only.
5. Logins are server objects, outside a database backup: create them on the target server (the restore log lists the
   users that have no login there).
6. From logical backups (mysqldump with one file per database, mydumper, pg_dump). Physical backups restore the whole
   instance and are not drilled yet.
7. Logical backups onto any server and version (MySQL / MariaDB with DEFINER, collation and GTID rewrites); physical
   backups onto the same version.
8. The same or a newer SQL Server version.
9. SQL Server writes backups on its own host: an agent started with `--allow-mssql-dir` moves the files; through
   WireGuard the backup folder must be shared with the DumpCat host.
10. mysqldump, binlog archiving and point-in-time recovery; mydumper and XtraBackup exist only for Linux.
11. mariadb-dump, MariaBackup (copied into the datadir by hand) and binlog point-in-time recovery; no mydumper.
12. Everything except replacing the data directory of a local cluster.

## How it works

```mermaid
flowchart LR
  db[("MySQL / MariaDB")]
  pipe["compress (zstd)<br/>encrypt (age)<br/>SHA-256"]
  db -- "mysqldump · mydumper · XtraBackup / MariaBackup" --> pipe
  db -- "binlog stream" --> pipe
  pipe --> local["Local disk / NFS"]
  pipe --> s3["S3 · R2 · MinIO · Wasabi"]
  pipe --> drive["Google Drive · OneDrive"]
  pipe --> remote["SFTP · FTP · WebDAV · rclone"]
```

Restores run the same pipeline in reverse. Logical backups stream straight into `mysql` with no temporary files;
mydumper and physical backups are unpacked first, then loaded with `myloader` or prepared with XtraBackup / MariaBackup.

## Connecting to your databases

DumpCat reaches a database in one of three ways, chosen per server (**Database servers → Add server → Connect**). The
backups, restores and point-in-time recovery work the same through all three.

**1. Direct, through the MySQL port.** DumpCat connects to the database like any client: `host:3306`, TLS optional, or a
unix socket when DumpCat runs on the database host itself. Use it when both machines are on the same network or VPC.

```mermaid
flowchart LR
  dc["DumpCat"] == "MySQL protocol · TCP 3306<br/>opened by DumpCat" ==> fw{{"firewall<br/>3306 open for DumpCat"}}
  subgraph D["Database host"]
    db[("MySQL / MariaDB<br/>listens on the network")]
  end
  fw ==> db
```

**2. Built-in VPN (WireGuard).** DumpCat runs a WireGuard endpoint itself — nothing to install on the backup server, off
until you turn it on. The database host joins with the standard WireGuard tools and keeps the tunnel open from its side,
so it can stay behind NAT and accept no inbound connections. [How to set it up](docs/manual.md#38-built-in-wireguard)

```mermaid
flowchart RL
  subgraph D["Database host"]
    db[("MySQL / MariaDB<br/>listens on 10.66.0.2")] <-- "10.66.0.2:3306" --> wg["wg-quick@dumpcat"]
  end
  wg == "WireGuard · UDP 51820<br/>opened by the database host" ==> fw{{"firewall / NAT<br/>no inbound port"}}
  fw ==> dc["DumpCat<br/>built-in WireGuard 10.66.0.1"]
```

**3. Agent.** A small `dumpcat agent` on the database host connects out to DumpCat over HTTPS and opens the database
connections DumpCat asks for — only to the addresses on its allow list. MySQL stays exactly as it is, even if it listens
on 127.0.0.1 only. [How to set it up](docs/manual.md#37-database-hosts-that-accept-no-inbound-connections-agent)

```mermaid
flowchart RL
  subgraph D["Database host"]
    db[("MySQL / MariaDB<br/>127.0.0.1:3306")] <-- "local connection" --> ag["dumpcat agent"]
  end
  ag == "HTTPS · WebSocket<br/>opened by the agent" ==> fw{{"firewall / NAT<br/>no inbound port"}}
  fw ==> dc["DumpCat"]
```

| | 1. Direct | 2. Built-in VPN (WireGuard) | 3. Agent |
|---|---|---|---|
| Who opens the connection | DumpCat → database | The database host → DumpCat | The database host → DumpCat |
| Port to open on the database host | MySQL (3306) for DumpCat's address | None | None |
| The database host reaches DumpCat on | — (DumpCat connects to it) | UDP 51820 | HTTPS (the web UI port) |
| To install on the database host | Nothing | WireGuard (`wg-quick`) and the file DumpCat generates | `dumpcat agent` (one installer command) |
| MySQL settings | Backup user allowed from DumpCat's address | Listens on the VPN address; user allowed from 10.66.0.1 | Unchanged |
| Physical backups (XtraBackup / MariaBackup) | Yes, when DumpCat runs on the database host | No (needs DumpCat on the database host) | Yes, with `--allow-physical`: the agent runs the tool on the database host and streams the backup back |
| SQL Server backup files | DumpCat on the SQL Server host, or a shared folder | A shared folder (SMB, a mount) | Yes, with `--allow-mssql-dir`: the agent moves them, nothing shared |
| Best for | Same network or VPC, or DumpCat on the database host | Teams that already use WireGuard; one gateway to a whole private network | Hosts behind NAT or a strict firewall; no change to MySQL or the network |
| Set up in | Database servers | WireGuard page, then Database servers | Agents page, then Database servers |

## Supported platforms

| | |
|---|---|
| **Databases** | MySQL 5.7, 8.0, 8.4 · Percona Server · MariaDB 10.x, 11.x · PostgreSQL 10 and newer · Microsoft SQL Server 2012 and newer on Windows, Linux and in containers (not Azure SQL Database / Managed Instance) |
| **DumpCat host** | Linux amd64 / arm64 with systemd · Windows Server 2016+ / Windows 10+ 64-bit, as a Windows service |
| **Backup tools** | mysqldump / mariadb-dump · mydumper / myloader · Percona XtraBackup · MariaBackup · pg_dump / pg_restore / pg_dumpall / pg_basebackup / pg_receivewal (the Linux installer picks the versions that match your server; `--postgres 16` adds the PostgreSQL 16 client) · SQL Server: none, SQL Server writes the backups into a folder DumpCat reads ([backup folder](docs/manual.md#sql-server)) |
| **Storage** | Local directories and NFS / SMB shares, plus everything [rclone](https://rclone.org) supports |
| **Databases behind a firewall** | The agent (`dumpcat agent`) on Linux with systemd or on Windows, outbound HTTPS only · or standard WireGuard (`wg-quick`), outbound UDP |

mydumper and XtraBackup exist only for Linux. For databases of hundreds of GB and for physical MySQL backups, run
DumpCat on Linux ([what works on Windows](docs/manual.md#35-windows)).

## Quick start

Want to look around first? The **[demo](https://dumpcat.cc/demo/)** runs the real web UI with sample data, right in your
browser.

### 1. Install

New to servers? The **[step-by-step install guide](https://dumpcat.cc/install/)** covers everything, from opening a
terminal to the first backup.

**Linux**: one command on the server, as a user with `sudo`:

```bash
curl -fsSL https://dumpcat.cc/install.sh | sudo sh
```

It downloads the latest package from `https://downloads.dumpcat.cc`, checks its SHA-256 against the release's
`SHA256SUMS` and runs the `install.sh` inside it. The installer puts the binary in `/opt/dumpcat`, creates the `dumpcat`
systemd service, and installs rclone, the MySQL / MariaDB client and the backup tools that match the database server on
the machine. Options go after `sh -s --`, for example `curl -fsSL https://dumpcat.cc/install.sh | sudo sh -s -- --port 9090`:
`--port 9090`, `--listen 127.0.0.1`, `--tls-auto`, `--no-deps`, `--uninstall [--purge]`.
On a database host that accepts no inbound connections, the same installer sets up only the [agent](docs/manual.md#37-database-hosts-that-accept-no-inbound-connections-agent):
`--agent --server https://backup.example.com --key dca_…` (the **Agents** page shows the command with the key).

**Windows**: download the `.zip` from [dumpcat.cc](https://dumpcat.cc/#download), unzip it (right-click, **Extract All**) and
double-click **Install DumpCat**; Windows asks for administrator rights. Or, in PowerShell run as Administrator:

```powershell
irm https://dumpcat.cc/install.ps1 | iex
```

**Downloading the package yourself**: the latest version, its release notes and checksums are on
[dumpcat.cc](https://dumpcat.cc/#download), and the packages are served from `https://downloads.dumpcat.cc/<version>/`.
Every package has a `START-HERE.txt`; on Linux, unpack it and run `sudo ./install.sh`. To check a download, compare it
with the release's `SHA256SUMS` (`sha256sum -c --ignore-missing SHA256SUMS`, or `Get-FileHash` on Windows). To build the
packages yourself, see [Building from source](#building-from-source).

### 2. Run your first backup

1. Open `http://<server-ip>:6369` and create the admin account. The setup page asks for the one-time **setup token**
   that the installer prints, so nobody else who reaches the port can claim the admin account first. To show it again,
   run `sudo dumpcat setup-token` (Windows, as Administrator: `& "C:\Program Files\DumpCat\dumpcat.exe" setup-token`)
   or look in the service log. (The default port spells *MEOW* on a phone keypad.)
2. Create a MySQL user with read-only backup privileges ([the grants](docs/manual.md#4-getting-started)).
3. **Database servers → Add server → Test connection.** Choose how DumpCat connects: directly, through WireGuard or
   through an agent ([the three ways](#connecting-to-your-databases)). DumpCat shows the version, the size of each database, missing privileges and which backup methods
   work.
4. **Storage → Add storage → Test.**
5. **Backup jobs → New job.** Pick the server, method, databases, schedule, encryption, storage and retention, then
   **Save & run now**. Each backup is verified right after it is written (one copy by default).

For production, put DumpCat behind HTTPS with the [nginx sample or the built-in TLS](docs/manual.md#34-behind-nginx--https-recommended-for-production)
and restrict who can reach it.

## Choosing a backup method

| | mysqldump | mydumper | XtraBackup / MariaBackup |
|---|---|---|---|
| Locks InnoDB | No | A few milliseconds | No (a very brief backup lock at the end) |
| Restore speed | Slowest | Fast, in parallel | Very fast: file copy + prepare |
| Incremental | No | No | Yes |
| Restore one database or rename it | Yes | Yes | No, whole instance |
| Good for | Under 20–50 GB | Large databases restored one at a time | 100 GB to several TB, low RTO |

PostgreSQL servers are backed up with `pg_dump` (directory format, tables in parallel, no locking of writers) or
`pg_basebackup` (the whole cluster, the base of WAL archiving and point-in-time recovery): see
[PostgreSQL](docs/manual.md#postgresql). SQL Server databases get SQL Server's own full and differential backups
(and log backups for point-in-time recovery), written into a backup folder that DumpCat reads: on the SQL Server host,
a share both machines reach, or the SQL Server host's own folder that the agent moves the files from — see
[SQL Server](docs/manual.md#sql-server).

More detail and recommendations: [Choosing a backup method](docs/manual.md#2-choosing-a-backup-method).

## Documentation

The full manual is in [docs/manual.md](docs/manual.md) ([Tiếng Việt](docs/manual.vi.md)) and ships in every package.

| Topic | |
|---|---|
| Installing on Linux and Windows, HTTPS | [Installation](docs/manual.md#3-installation) |
| Database hosts that accept no inbound connections | [Agent](docs/manual.md#37-database-hosts-that-accept-no-inbound-connections-agent) · [WireGuard](docs/manual.md#38-built-in-wireguard) |
| S3, Google Drive, OneDrive, FTP, SFTP… | [Configuring storage](docs/manual.md#5-configuring-storage) |
| Schedules, incrementals, retention | [Jobs, schedules, retention](docs/manual.md#6-jobs-schedules-retention) |
| Logical and physical restores | [Restore](docs/manual.md#7-restore) |
| Binlog archiving, recovering to a point in time | [PITR](docs/manual.md#8-log-archiving--point-in-time-recovery-pitr) |
| Restoring without DumpCat, reinstalling DumpCat | [Manual recovery](docs/manual.md#9-manual-recovery-without-dumpcat) · [Disaster recovery](docs/manual.md#10-disaster-recovery) |
| Roles, 2FA, encryption at rest | [Security](docs/manual.md#11-security) |
| REST API, webhooks, AI assistants (MCP) | [API & automation](docs/manual.md#12-api--automation) · [MCP](docs/manual.md#ai-assistants-mcp) |
| Environment variables, command line | [System configuration](docs/manual.md#13-system-configuration) |
| Updates, release server, signed releases | [Updating](docs/manual.md#36-updating) |
| Common errors | [Troubleshooting](docs/manual.md#14-troubleshooting) |

## Building from source

You need Go 1.26 or newer. No C compiler is required: binaries are built with `CGO_ENABLED=0` and SQLite is pure Go.

```bash
go vet ./...
go test ./...
go build -o dumpcat ./cmd/dumpcat
```

Run it locally, serving the UI from disk so UI changes need no rebuild:

```bash
DUMPCAT_DATA_DIR=./data DUMPCAT_LISTEN=127.0.0.1:6369 DUMPCAT_WEB_DIR=./web ./dumpcat serve
```

Build the release packages (on Windows: `.\build.ps1`):

```bash
./build.sh
```

This writes Linux amd64 / arm64 and Windows amd64 binaries, `dumpcat-<version>-linux.tar.gz`,
`dumpcat-<version>-windows-amd64.zip` and the update feed in `dist/update/`. The version comes from `VERSION` and the
release notes from `CHANGELOG.md`. Publishing updates and signing releases are covered in
[Updating](docs/manual.md#36-updating).

```
cmd/dumpcat    command line: serve, agent, user, decode, update, release
internal/      API, backup runner, engines, storage, compression and encryption pipeline, SQLite store, agent and
               WireGuard tunnels, notifications, updater…
web/           UI in plain HTML, CSS and JavaScript, embedded into the binary
scripts/       Linux and Windows installers (also for the agent), release upload (publish-release.sh),
               site generators (build-site.py, build-demo.py)
deploy/        systemd units for DumpCat and the agent, sample env files, nginx config
site/          dumpcat.cc: landing page, comparison pages and the demo (site/demo, generated from web/), served by the
               Cloudflare Worker in wrangler.jsonc
site-src/      sources of the generated pages: comparison data, the demo's mock API and sample data
docs/          manual in English and Vietnamese, website runbook (website.vi.md)
```

## Contributing

Bug reports, fixes, documentation and translations are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md) first.
Report security problems privately, as described in [SECURITY.md](SECURITY.md).

## License

DumpCat is licensed under the [Apache License 2.0](LICENSE).
Copyright 2026 [theweakestharrison](https://github.com/theweakestharrison); see [NOTICE](NOTICE).

The binaries contain Go modules under their own licenses, listed with their full license texts in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md); the MPL-2.0 driver github.com/go-sql-driver/mysql is used unmodified,
and its source is available upstream at https://github.com/go-sql-driver/mysql.

DumpCat runs external tools such as mysqldump, mydumper, Percona XtraBackup, MariaBackup and rclone. They are installed
separately and keep their own licenses.
