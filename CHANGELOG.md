# Changelog

The section of the version being built becomes the release notes shown in the
web UI and by `dumpcat update` (see `build.sh`), and every section becomes a
page at dumpcat.cc/changelog/ (see `scripts/build-site.py`). Newest version
first. Each section: `## <version> (<date>)`, a one-line summary, then `###`
groups of bullets.

## 1.8.0

Microsoft SQL Server is the third database engine: native full and differential backups, restores under new names or
onto another server, restore drills with `DBCC CHECKDB`, and scheduled log backups for point-in-time recovery.

### SQL Server

- Choose **Microsoft SQL Server** when adding a server (SQL Server 2012 and newer on Windows, Linux and in containers;
  tested with SQL Server 2022 on Linux). SQL logins or Windows authentication (`DOMAIN\user`, NTLM), named instances
  through the SQL Server Browser, always-encrypted connections. No client tool: DumpCat speaks TDS itself
  ([go-mssqldb](https://github.com/microsoft/go-mssqldb), BSD-3-Clause).
- **Check** shows the version and edition, the service account, the recovery model of every database, what the
  account may not do, and whether SQL Server reads the server's **backup folder**.
- The **backup folder**: `BACKUP DATABASE` writes on the SQL Server host, so every file passes through a folder both
  SQL Server and DumpCat reach — the same path when DumpCat runs on the SQL Server host, otherwise a share or mount,
  entered as SQL Server sees it and as DumpCat sees it. DumpCat reads each `.bak` into the usual compression,
  encryption and SHA-256 pipeline, uploads it to every storage of the job and deletes it; restores pass through it the
  other way. The connection test writes a file through one path and has SQL Server read it through the other.
- One method, **native backup**: full backups on the job's schedule, **differential** backups on its second schedule
  (or *Run → Differential backup*), `WITH CHECKSUM`, `COMPRESSION` where the edition has it (then DumpCat stores the
  file without compressing it again), `RESTORE VERIFYONLY` on the server before each file is read, copy-only jobs,
  databases backed up in parallel, extra `BACKUP … WITH` options for admins. A differential whose base moved (a full
  backup taken by another tool) becomes a full backup, and says why.
- **Restores** go through the target's backup folder: every file is checked there first (`RESTORE HEADERONLY` must
  show the backup set the catalog recorded, `RESTORE VERIFYONLY` must pass), then `RESTORE DATABASE … WITH MOVE` into
  the instance's data and log folders under the new name, a differential on top of its full backup. A database that
  exists is only overwritten with *Replace existing databases* (`WITH REPLACE`, after closing its sessions); system
  databases never. Orphaned users and an unknown owner are listed in the log.
- **Restore drills** restore the full backup and the differential under temporary names, run `DBCC CHECKDB` on every
  copy, compare the tables and their row counts (`sys.partitions`, read right after the backup) and run the
  assertions in a transaction that is rolled back.
- **Point-in-time recovery**: with log archiving on, DumpCat runs `BACKUP LOG` for every database of the job on a
  schedule (every 15 minutes by default; *Back up the log now* on the archive page) and archives each log backup with
  its LSNs and the marked transactions it holds. Recovery to a **time** (`STOPAT`, converted to the server's time
  zone), a **marked transaction** (`STOPATMARK`) or an **LSN** (decimal or `00000025:00000080:0001`), or the latest log
  backup: the backup is restored `WITH NORECOVERY`, the log backups follow until one reaches the target. The plan
  checks the LSN chain first: a log backup taken by another tool is reported as a gap that recovery cannot cross,
  databases in the SIMPLE recovery model are named and refused.

### Everything else

- A backup can be a **differential**: it belongs to the chain of its full backup for retention, deletion, restores,
  drills and point-in-time recovery, and imported catalogs keep the link. The API and MCP take
  `type: differential` (an incremental schedule is a differential one for SQL Server jobs).
- Servers have `backup_dir` and `backup_dir_local` (store migration 11), in the API and in MCP's `create_server` /
  `update_server`; jobs have `copy_only`, `native_compression` and `verify_only`.
- The server list no longer shows a TLS badge for servers with TLS disabled.

## 1.7.0 (2026-10-20)

AI assistants can set backups up: MCP tools that add and change database servers, storages, jobs and restore drills,
for API tokens with the new Manage access. Passwords, keys and passphrases still never go through MCP.

### AI assistants (MCP)

- A third kind of API token, **Manage**: full access, and over MCP an assistant can also set up and change backups.
  Read-only and full-access tokens keep doing what they did: an assistant given a full-access token still cannot
  change the configuration. Over the REST API, Manage is the same as full access.
- New tools for Manage tokens: `create_server`, `update_server`, `create_storage`, `update_storage`, `create_job`,
  `update_job`, `create_drill` and `update_drill`. A new job or drill starts from the web UI form's defaults, and a
  change touches only the settings it names (`update_job` with `enabled: false` pauses a job).
- Secrets never go through MCP: no tool takes a database password, storage key, OAuth token or encryption passphrase,
  and pre/post commands and extra tool arguments stay in the web UI. A tool that leaves a secret unset answers with
  `finish_url`, the page where the user enters it (the server and storage pages open their form with `?edit=<id>`);
  a job asked to be encrypted is created paused until its passphrase is set there. Moving a server or storage with a
  saved secret to another host or endpoint still needs the secret again, in the web UI.
- New tools: `list_storages`, `get_storage`, `list_storage_types` and `preview_schedule` for every token, and
  `test_storage` for full-access tokens.
- Deleting, restoring and DumpCat's settings remain out of MCP.
- The **API & MCP** page offers Manage when creating a token and groups the tools by the access they need.

## 1.6.0 (2026-10-20)

An API & MCP page to connect AI assistants and scripts, the API reference in Markdown, single sign-on with OpenID
Connect, an instance name and logo, and installed instances kept out of search engines.

### API & MCP

- A new **API & MCP** page puts API tokens, AI assistants and scripts in one place: create and revoke tokens, copy
  ready-made setup for Claude Code, Claude Desktop, other MCP clients and curl with the server's address and the new
  token filled in, test a token against the MCP endpoint (version, account, access and the tools it gets), and see
  what an assistant can do with your role. API tokens moved there from *Profile & security*.
- `GET /api/docs.md`: the API reference in Markdown, for people and AI agents. The server writes it from its own
  routes (authentication, conventions, curl examples, the MCP tools, every route with the role and token it needs),
  and a test keeps every route documented. `GET /api/mcp/tools` lists the MCP tools as JSON.

### Single sign-on (OpenID Connect)

- Sign in with Google, Microsoft Entra ID, Keycloak, Authentik or any OpenID Connect provider: **Settings →
  Authentication** takes the issuer URL (discovery), client ID and secret (encrypted with the master key) and scopes,
  and shows the redirect URL to register. The sign-in page gets an SSO button under the password form.
- Authorization code flow with PKCE, a state bound to the browser and a nonce; the ID token's signature is checked
  with the provider's keys (no `none` or HS* tokens), with its issuer, audience, expiry and nonce.
- Users are found by the provider account they used before, else by their verified email address, and are linked
  from then on (**SSO** badge in Users, where an admin can unlink). Optional: allowed email domains, creating
  unknown users with a default role, and roles from a group or role claim (also nested ones such as
  `realm_access.roles`); the last admin is never demoted. While SSO is on, accounts may be created without a password;
  such an account creates API tokens or turns its 2FA off within 10 minutes of signing in, in place of the password.
- Password sign-in stays as the fallback, optionally for admins only. Accounts with 2FA still enter their code after
  SSO, locked accounts are refused, and failures count toward the per-address sign-in limit. The audit log records
  SSO sign-ins, failures with their reason, tests and accounts created by SSO.
- **Test SSO** runs a sign-in with the settings of the form and shows the verified claims and what a sign-in would do,
  without signing anyone in.

### Branding

- **Settings → Branding**: an instance name for the sidebar, page titles, the sign-in page, the authenticator app and
  notifications (email subjects start with `[name]`), and a logo (PNG, SVG or WebP, at most 256 KB) for the sidebar,
  the sign-in page, the tab's icon and, as a PNG, emails. Uploads are checked by their content; SVGs with scripts,
  event handlers or links to other files are refused, and the logo is served with a policy that keeps anything in it
  from running. **Reset to default** brings back DumpCat and the cat.

### Search engines

- Installed instances stay out of search results by default: `/robots.txt` disallows everything, every answer
  carries `X-Robots-Tag: noindex, nofollow`, and the page has a robots meta tag. **Settings → Security** can allow
  indexing (for a public demo).

## 1.5.0 (2026-10-20)

PostgreSQL: backups with pg_dump and pg_basebackup, continuous WAL archiving and point-in-time recovery, restores under
new names or onto another server, roles, and restore drills. MySQL and MariaDB work as before, now as one of several
database engines.

### PostgreSQL

- Choose **PostgreSQL** as the database engine of a server (port 5432, or a socket directory). The server check shows
  the version, WAL level, the backup role's attributes and what it is missing, and each database with its size,
  tables, encoding and locale.
- New backup method **pg_dump**: each database is dumped in directory format with its tables in parallel
  (`--jobs`), from one consistent snapshot, then stored as one archive through the usual compression, encryption,
  checksums, storages, retention and verification. *Roles and tablespaces* adds `pg_dumpall --globals-only`
  (passwords when the backup account is a superuser). Tables can be excluded as `database.table` or
  `database.schema.table`.
- Restores with `pg_restore --jobs`, into the same database (the backup's objects are replaced, others stay), under a
  new name, or onto another PostgreSQL server: the database is created with the source's encoding and locale, roles
  the target lacks can be created from the backup, and *Skip ownership* / *Skip privileges* are suggested for another
  server.
- Restore drills for PostgreSQL backups: every table and its row count compared with the backup (`count(*)`, or the
  server's estimates after `ANALYZE` for very large tables), SQL assertions in a read-only session.
- DumpCat finds every installed version of the client tools and uses the server's major version (or the nearest
  newer one); the Linux installer installs them with `--postgres 16` (several: `--postgres 15,16`).
- Passwords go through a private password file, never the command line; database names are passed inside a quoted
  connection string, so a name like `a=b` cannot become connection options.
- New backup method **pg_basebackup**: the whole cluster over a replication connection (nothing to install on the
  database host), written as one tar stream with the WAL of the backup straight into the storage pipeline; a temporary
  replication slot keeps that WAL until it is fetched. Full backups; clusters with additional tablespaces are not
  supported yet.
- Restoring a pg_basebackup backup prepares a data directory on the DumpCat host and recovers it with a temporary
  PostgreSQL server of the backup's version (its own configuration: a local port, no archiving); without the server
  binaries the directory is left ready to recover. Admins on Linux can replace a local cluster's data directory with it:
  recovered next to it, swapped in while the service is stopped, rolled back on any error. As root the recovery runs as
  the OS user `postgres`, and the folders DumpCat creates for the directory let it in. A directory whose configuration
  lived in `/etc/postgresql` (Debian/Ubuntu) gets a minimal `postgresql.conf`, `pg_hba.conf` and `pg_ident.conf`, so
  that it starts on its own.

### Point-in-time recovery for PostgreSQL

- **Continuous WAL archiving** for pg_basebackup jobs: `pg_receivewal` streams the WAL through a replication slot of
  the job, so the server keeps it while DumpCat is down; every completed segment is compressed, encrypted and uploaded
  to every storage of the job like the binlogs of MySQL jobs. Turning archiving off, disabling or deleting the job drops
  the slot. A slot that lost WAL (`max_slot_wal_keep_size`) is reported as a gap and streaming starts again.
  When the server's timeline left the archived one before its end (the cluster was restored to an earlier point, or
  a standby was promoted behind DumpCat), the archive keeps the old timeline and continues on the new one.
- DumpCat reads the WAL itself (no `pg_waldump` needed): the LSN range of every segment, the times of its commits and the
  restore points created in it, CRC-checked.
- **Recover to a point in time, a restore point or a WAL position (LSN)**, or the latest state including the segment
  still being received: the newest pg_basebackup before the target is picked, every segment is downloaded and checked
  before anything is changed, and PostgreSQL replays the WAL to the target and promotes. Restore points found in the
  archive are suggested.
- *Close segment now* and *close the current segment after N minutes* (`pg_switch_wal()`), lag and slot monitoring on
  the **Binlog / WAL & PITR** page, lag alerts and dashboard warnings, retention after the oldest kept backup
  (timeline history files are kept).

### Several database engines

- Everything that is specific to a database engine (inspection, backup methods, restores, the queries of restore
  drills) now sits behind one driver interface; jobs, schedules, storages, encryption, retention, verification,
  notifications, monitoring, agents and WireGuard are shared. Backup methods and restore options shown in the UI come
  from the driver. See `docs/design-multidb.md`.
- Servers have an engine (existing ones are MySQL), `manifest.json` records it, and a job's method must belong to its
  server's engine. Restoring a backup onto a server of another engine is refused.
- Log archiving and point-in-time recovery sit behind one interface too: the archiver of each job, the upload to its
  storages, the catalog of archived files, retention, lag monitoring and the recovery plan are shared by MySQL binlogs
  and PostgreSQL WAL. The page is now **Binlog / WAL & PITR**; the API reports the server's position as
  `server_position` (instead of `server_file` / `server_pos`) and the start of a plan as `base.start`.
- A restarted binlog stream no longer counts the previous session's last check as caught up, so a recovery planned
  right after a restart does not miss the binlog still being received.

### AI assistants (MCP)

- DumpCat is an MCP server for Claude Desktop, Claude Code and other MCP clients: `POST /api/mcp` (streamable
  HTTP) and `dumpcat mcp`, a stdio bridge that reads its address and token from `DUMPCAT_MCP_*` variables or an env
  file and checks certificates like the agent. Tools give the overview, monitoring and alerts, servers (with a live
  connection check), jobs, backups, restores, restore drills, the end of any task's log and a point-in-time recovery
  preview; with a full-access token of an operator they also run a backup, verify a backup or start a drill. Each
  tool runs the matching API request with the caller's token, so roles, audit log and secret-free answers are the
  API's; restores, deletions, credentials and settings are not exposed.
- API tokens are **read-only** or **full**. New tokens are read-only unless *Full* is chosen; a read-only token can
  look but not change anything, download a backup or create a token, whatever its user's role. Existing tokens keep
  full access. Token names refuse control characters.

### Other fixes

- Physical restores that only prepare a directory (MySQL and PostgreSQL) refuse one in `/tmp` or `/var/tmp` when DumpCat
  runs as its systemd service: these are the service's private folders (`PrivateTmp`), invisible to other users and
  deleted when DumpCat stops.

## 1.4.2 (2026-10-20)

Security fixes from an audit of DumpCat: stricter checks on everything that reaches the backup tools, encryption that
fails closed, and harder sign-in. Updating is recommended.

### Backup tools and restores

- A line break in a server's user, host or socket could add options to the option file the MySQL tools read (DumpCat
  runs them as root). Servers now refuse control characters in these fields and in the datadir, service and OS user,
  and the option file writer refuses them too, on DumpCat and on agents.
- Replacing a datadir (physical restore, admins only) now refuses system directories, DumpCat's own data directory
  and any non-empty directory that does not look like a MySQL/MariaDB datadir; service and OS user names are checked.
- Restores run the mysql client with `--local-infile=0`, and the password goes through the option file only
  (`MYSQL_PWD` just when the file cannot hold it).
- Backups imported from a storage: the job and backup directory names in `manifest.json` must be plain names (they
  could point the extraction of a physical restore outside DumpCat's data directory), character sets and collations
  must be identifiers, a manifest reusing a known backup id is linked only when its files match, and dates in the
  future are not accepted.
- A checksum mismatch now fails a logical restore (and a restore drill) instead of only being logged.

### Encryption fails closed

- DumpCat refuses to start with a master key that does not open its database (`dumpcat.db` copied without its
  `master.key`, or the key lost and recreated). Before, every stored secret read as empty and jobs that encrypt
  uploaded unencrypted backups and binlogs. A job that encrypts and cannot read its passphrase now fails.

### Sign-in and accounts

- Sign-in attempts are counted before the password is checked, so parallel requests get no extra tries. The lock
  after 5 failures applies per account and address: failures from one place no longer lock an admin out everywhere.
  A two-factor code works once. Password confirmations (changing the password, turning 2FA off) are rate limited.
- Behind a reverse proxy, the client address is `X-Real-IP` or the last `X-Forwarded-For` entry, not the first one,
  which the client chooses.
- Creating an API token asks for the account password. An admin resetting a password or disabling an account, and
  `dumpcat user passwd` / `disable`, revoke that user's API tokens.
- Re-enter a server's or storage's password when you weaken its TLS mode, change its CA certificate or turn
  certificate checks off, as when you change its address.

### Other fixes

- S3 storage without an access key, which signs in with the host's own cloud identity, is admin-only.
- Rotating or deleting an agent's key also cuts off a connection authenticated with the old key just before.
- Request bodies must arrive within 30 seconds.
- The WireGuard endpoint must be a host name or address with an optional port: a line break could add lines to the
  peers' `wg-quick` file.
- Notification errors no longer show webhook or Telegram bot URLs; Slack titles and Discord fields are escaped; the
  agent install commands quote values from Settings; the release notes link must be http(s); the agent help no
  longer suggests `--insecure` on untrusted networks.

## 1.4.1 (2026-10-20)

A redesigned point-in-time recovery screen, and release notes one click away from the update notice.

### Point-in-time recovery screen

- A timeline of the job's backups and archived binlogs, with the part that cannot be recovered hatched. Drag it to
  pick the moment; once checked, the backup that is restored lights up and the binlogs replayed after it are drawn up
  to the recovery point.
- The kind of target (point in time, before a GTID, binlog position, latest available) is a segmented choice, with
  **5 min ago**, **1 hour ago** and **1 day ago** shortcuts for a time. Date and time inputs look like the other inputs.
- The plan reads as three steps: the backup that is restored, the binlog files replayed (how many, how large, from
  which file to which) and where the replay stops, in your local time. The restore form shows the same plan when it
  replays binlogs.

### Release notes

- The update notice on the **System** page links to the release notes of the new version on dumpcat.cc.

## 1.4.0 (2026-10-19)

Physical backups through the agent, restore drills that prove a backup restores, alerts when backups go stale,
Prometheus metrics, a weekly email digest, and a one-command install.

### Physical backups through the agent

- An agent started with `--allow-physical` (`install.sh --agent --allow-physical`, `install.ps1 -AllowPhysical`,
  `DUMPCAT_AGENT_ALLOW_PHYSICAL=1`) runs MariaBackup / XtraBackup on the database host when a job asks and streams the
  backup back over its connection. DumpCat compresses, encrypts, uploads, verifies and applies retention exactly as for
  a local physical backup; incrementals work, the tool's log lines appear in the run log, and cancelling the run stops
  the tool on the database host.
- The credentials travel with each backup and only live in a private option file while the tool runs. The agent only
  backs up databases on its allow list, builds the command line itself and passes a job's extra arguments only when
  they name no file, directory, program or server.
- On Linux, `--allow-physical` runs the agent as the owner of the datadir (a systemd drop-in) instead of a throwaway user.
- Agents report what they can do when they connect: the Agents page has a **Physical backups** column (tool and version,
  OS user, datadir readable), and the server page and the job editor say why physical backups are not available (agent
  without `--allow-physical`, tool missing, datadir not readable, tool not matching the server, agent too old). Older
  agents keep working and report no physical capability.
- Physical restores still run where DumpCat runs: backups taken through an agent are prepared there, and the restore
  log shows how to copy the prepared directory to the database host.

### Restore drills

- On a schedule (cron, like jobs) or on demand, DumpCat restores the newest backup of a job (or one you pick) into a
  sandbox server under temporary names (`<database>__drill_<id>`), checks the copy and drops it. The checks: the
  restore succeeds, every table of the backup exists in the copy, each table holds exactly the rows the backup
  recorded, optional SQL assertions (one read-only `SELECT` each, passing when its first column is true) and an
  optional maximum restore time. Every run records the restore time (RTO), its checks and a log.
- New **Restore drills** page and editor, a result page with live progress, a **Restore drills** card on the backup page
  (**Drill this backup**) and in the job editor, **Restore-tested** / **Drill failed** badges on backups and jobs, and
  dashboard warnings. Failed drills (and copies that could not be dropped) notify the channels as event kind `drill`.
- Safety: a drill refuses to run when a temporary name already exists on the sandbox and only ever drops databases it
  created; their names are recorded first, so the copies of a drill interrupted by a restart are dropped at the next
  start. Drills never restore users and grants or change global settings; on the backup's own source server they stay
  out of its binary log. Supported: mysqldump with one file per database, and mydumper.
- Logical backups count the rows of every table while the dump is written (mysqldump, mydumper) and keep them in the
  catalog and in `manifest.json`; the backup page shows the rows per database.

### Monitoring and alerts

- Freshness alerts: every minute DumpCat checks each job against its own schedule. When the newest successful backup
  is older than 1.5 × the time between two scheduled runs plus a grace period (60 minutes, **Settings → Monitoring**),
  or than the job's new **Max age of the newest backup**, an alert goes once to the channels that send failures, and a
  second message follows when a backup succeeds again. A max age also watches jobs without a schedule (backups started
  through the API). Open alerts survive a restart.
- Binlog lag alerts: when binlog archiving has been behind the server for more than 30 minutes (configurable, 0 = off),
  because the stream is down or closed binlogs wait for upload, once per incident, with a message when it catches up.
  The archiver status has a new `archive_lag_seconds`.
- The dashboard's fixed "no successful backup in more than 8 days" warning uses the same per-job rule, and overdue jobs
  get an *overdue* badge.
- Prometheus metrics at `GET /metrics`, off by default: last success, status, duration and size per job, integrity
  checks, freshness, binlog lag, storage use, agents and WireGuard peers online, version. Access with a bearer token
  (shown once, stored hashed) or only from the DumpCat host (not through a reverse proxy).
- Weekly digest by email, opt-in per email channel: backups per job, sizes and growth, integrity checks, overdue jobs,
  restore drill results and storage use over the last 7 days, in the design of the other emails with a plain-text
  version. Day and hour in Settings, with **Preview**; **Send digest now** on the channel.
- Webhooks: new `kind` values `stale` and `binlog_lag`, new `status` `recovered`.
- API: `POST /api/settings/metrics-token`, `POST /api/notifications/digest`, `GET /api/digest/preview`; settings
  `freshness_alerts`, `stale_grace_minutes`, `binlog_lag_minutes`, `metrics_access`, `digest_weekday`, `digest_hour`;
  job option `max_age_hours`. `PUT /api/settings` keeps the fields a request leaves out.

### One-command install

- Install with one command: `curl -fsSL https://dumpcat.cc/install.sh | sudo sh` on Linux and, in PowerShell as
  Administrator, `irm https://dumpcat.cc/install.ps1 | iex` on Windows. Both download the latest release, check its
  SHA-256 against `SHA256SUMS` and run the package's installer (options after `sh -s --`, e.g. `--agent ...`).
- The Windows package has **Install DumpCat** to double-click: it asks for administrator rights, installs the service
  and opens the web UI. Every package has a `START-HERE.txt` (English and Vietnamese) with the steps, and dumpcat.cc has
  a step-by-step guide for beginners at [dumpcat.cc/install](https://dumpcat.cc/install/).

### Fixes

- A restore under a new database name no longer creates triggers, routines or events in the original database when
  their definitions name it (`CREATE TRIGGER shop.t ... ON shop.items`); such names now point at the new database.
- Physical backups now record the binlog file, position and GTID they are consistent with (read from the backup tool's
  info file), so point-in-time recovery can start from a physical backup.

## 1.3.1 (2026-10-19)

Every backup is checked right after it is written, notifications are redesigned, and the web UI no longer runs stale
code after an update.

### Verified after every backup

- New jobs read one copy back (the cheapest, a local directory first), decrypt and decompress it and check its SHA-256.
  The job's **Verify integrity after each backup** setting offers one copy, every copy or off; existing jobs keep their
  setting. The notification of a backup includes the result.

### Redesigned notifications

- Emails are HTML (with a plain-text version) with the DumpCat logo, a status band, the details as a table, storage
  copies as badges, the error in its own box and an **Open in DumpCat** button; they follow the dark mode of mail apps.
  Slack gets a colored card with the fields and a button, Discord an embed, Telegram a cleaner message. The webhook
  JSON is unchanged. Every message names the machine DumpCat runs on.

### No stale web UI after an update

- Asset URLs carry a hash of the UI files (`/assets/<build>/…`) and are cached for a year, while the page itself is
  checked on every load. A browser tab opened before the update offers to reload and reloads on its next navigation,
  instead of running old and new code together.

## 1.3.0 (2026-10-19)

Built-in WireGuard for database hosts that accept no inbound connections, connection illustrations, and a clearer
storage type picker.

### Built-in WireGuard

- Off by default: database hosts that accept no inbound connections can join DumpCat's VPN with the standard WireGuard
  tools instead of running the agent. WireGuard runs inside DumpCat (wireguard-go with a userspace network stack):
  nothing is installed and no network interface, route or firewall rule is created on the DumpCat host; DumpCat reaches
  only the database addresses routed to each peer.
- New **WireGuard** page: turn it on, choose the UDP port, VPN subnet and the endpoint peers connect to; add peers
  (address assigned automatically, optional networks behind a gateway), get each peer's `dumpcat.conf` with the
  install commands and the MySQL grant, rotate keys, see handshakes, endpoints and traffic.
- The server form's **Connect** choice now also offers **Through WireGuard**; backups, restores, binlog archiving and
  point-in-time recovery work through it (physical backups still need DumpCat on the database host).
- DumpCat remembers where each peer was last seen, so it reconnects at once after a restart; changing one peer never
  drops the sessions of the others.
- Clear errors through WireGuard: peer not connected yet, no answer, or nothing listening on the database address.
- New API endpoints: `GET/PUT /api/wireguard`, `POST /api/wireguard/peers`, `PUT/DELETE /api/wireguard/peers/{id}`,
  `POST /api/wireguard/peers/{id}/rotate`; servers have a `wg_peer_id` field.

### Interface

- Connection illustrations on the Agents and WireGuard pages (and diagrams in the manual): both machines, the firewall
  between them and the tunnel the database host opens, with the ports each side needs. They are shown first until the
  first agent or peer exists, with a tall version on narrow screens.
- The **Choose storage type** dialog is redesigned: storage types are grouped (on this server, object storage, cloud
  drives, file servers, other) as cards with an icon and a short description.

## 1.2.0 (2026-10-19)

Agents for database hosts behind a firewall or NAT: the database host connects out to DumpCat, and nothing has to be
opened to the database.

### Agents

- `dumpcat agent` runs on the database host, connects out to DumpCat over HTTPS (WebSocket on the web UI's port) and
  carries the database connections DumpCat asks for, so no port has to be opened to the database. mysqldump and
  mydumper backups, restores, binlog archiving and point-in-time recovery work through it; physical backups still need
  DumpCat on the database host.
- New **Agents** page (admins add agents, see install commands, rotate keys; everyone sees which agents are online,
  their host, version and allow list). The server form has a **Connect** choice: directly, or through an agent.
- Each agent connects only to the database addresses on its own allow list (by default the local MySQL/MariaDB port
  and sockets; `--allow` adds more). Keys are shown once and stored hashed; rotating a key disconnects the agent.
- One-command install on the database host: `sudo ./install.sh --agent --server URL --key KEY` (systemd unit
  `dumpcat-agent`, runs as an unprivileged dynamic user) and `.\install.ps1 -Agent -Server URL -Key KEY` on Windows
  (service `DumpCatAgent`). The nginx sample forwards WebSocket upgrades on `/api/agent/`.
- When a tunnel cannot be opened (agent offline, address not allowed, database refused), mysql, mysqldump and the
  connection test show the reason instead of "lost connection".
- New API endpoints: `GET/POST /api/agents`, `PUT/DELETE /api/agents/{id}`, `POST /api/agents/{id}/rotate`; servers
  have an `agent_id` field.

## 1.1.1 (2026-10-19)

Fixes the size and alignment of icons in buttons.

### Fixes

- Icons in buttons were misaligned or oversized: the "Go to backup jobs" / "Create a job" buttons of empty pages showed
  a 34px icon, buttons inside alerts and warnings got the alert's icon size, and icons in small text (job list, help
  lines) sat above the text. Icons are now the same size in every button and centred on their text.
- Small buttons inside an alert no longer push its text below the alert icon; the backup method cards keep their
  icon on the first line when the title wraps.

## 1.1.0 (2026-10-19)

Restore onto another MySQL or MariaDB instance, with compatibility options suggested from the target.

### Restore onto another instance

- The restore form connects to the chosen server first and shows its version, flavor and the databases already on it,
  warns about every database that would be overwritten (not only on the source server), and lets you add a new target
  server without leaving the page.
- Compatibility options for logical restores to another instance or version, suggested from the target: remove
  `DEFINER` from views, routines, triggers and events; convert MySQL 8 `utf8mb4_0900_*`, MariaDB `uca1400` and
  `utf8mb3_*` collations (MySQL 8 → MariaDB / 5.7 and back); skip `SET @@GLOBAL.GTID_PURGED`. Only schema statements
  are rewritten, row data is never changed; mydumper schema files are rewritten after extraction.
- Physical restores: choosing the instance now fills its datadir, service and owner, and the form warns when the
  target runs another version series or flavor.
- New API endpoint `POST /api/runs/{id}/restore-check`.

### Fixes

- The GTID set of MySQL 8 mysqldump backups (`GTID_PURGED=/*!80000 '+'*/ '…'`) is now recorded, so point-in-time
  recovery to a GTID can tell when the target is already inside the backup.

## 1.0.0 (2026-10-18)

First release of DumpCat: MySQL and MariaDB backup and restore with a web UI.

- Lock-free logical backups (mysqldump/mariadb-dump, mydumper) and hot physical backups (XtraBackup/mariabackup).
- Storages: local/NFS, S3, Google Drive, OneDrive, FTP/FTPS, SFTP, WebDAV and any rclone backend.
- Binlog archiving and point-in-time recovery.
- Linux and Windows (service) installers; built-in updates with automatic rollback.
