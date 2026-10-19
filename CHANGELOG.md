# Changelog

The section of the version being built becomes the release notes shown in the
web UI and by `dumpcat update` (see `build.sh`), and every section becomes a
page at dumpcat.cc/changelog/ (see `scripts/build-site.py`). Newest version
first. Each section: `## <version> (<date>)`, a one-line summary, then `###`
groups of bullets.

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
