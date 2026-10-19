# Changelog

The section of the version being built becomes the release notes shown in the
web UI and by `dumpcat update` (see `build.sh`). Newest version first.

## 1.3.1

- Backups are verified after every run: new jobs read one copy back (the cheapest, a local directory first),
  decrypt and decompress it and check its SHA-256. The job's **Verify integrity after each backup** setting offers
  one copy, every copy or off; existing jobs keep their setting. The notification of a backup includes the result.
- Notifications are redesigned: emails are HTML (with a plain-text version) with the DumpCat logo, a status band,
  the details as a table, storage copies as badges, the error in its own box and an **Open in DumpCat** button; they
  follow the dark mode of mail apps. Slack gets a colored card with the fields and a button, Discord an embed,
  Telegram a cleaner message. The webhook JSON is unchanged. Every message names the machine DumpCat runs on.
- No stale web UI after an update: asset URLs carry a hash of the UI files (`/assets/<build>/…`) and are cached
  for a year, while the page itself is checked on every load. A browser tab opened before the update offers to
  reload and reloads on its next navigation, instead of running old and new code together.

## 1.3.0

- Built-in WireGuard, off by default: database hosts that accept no inbound connections can join DumpCat's VPN with the
  standard WireGuard tools instead of running the agent. WireGuard runs inside DumpCat (wireguard-go with a userspace
  network stack): nothing is installed and no network interface, route or firewall rule is created on the DumpCat host;
  DumpCat reaches only the database addresses routed to each peer.
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
- Connection illustrations on the Agents and WireGuard pages (and diagrams in the manual): both machines, the firewall
  between them and the tunnel the database host opens, with the ports each side needs — shown first until the first
  agent or peer exists; a tall version on narrow screens.
- The **Choose storage type** dialog is redesigned: storage types are grouped (on this server, object storage, cloud
  drives, file servers, other) as cards with an icon and a short description.

## 1.2.0

- Agents for database hosts that accept no inbound connections: `dumpcat agent` runs on the database host, connects
  out to DumpCat over HTTPS (WebSocket on the web UI's port) and carries the database connections DumpCat asks for —
  no port has to be opened to the database. mysqldump and mydumper backups, restores, binlog archiving and
  point-in-time recovery work through it; physical backups still need DumpCat on the database host.
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

## 1.1.1

- Fix: icons in buttons were misaligned or oversized — the "Go to backup jobs" / "Create a job" buttons of empty
  pages showed a 34px icon, buttons inside alerts and warnings got the alert's icon size, and icons in small text
  (job list, help lines) sat above the text. Icons are now the same size in every button and centred on their text.
- Small buttons inside an alert no longer push its text below the alert icon; the backup method cards keep their
  icon on the first line when the title wraps.

## 1.1.0

- Restore onto another instance: the restore form connects to the chosen server first and shows its version, flavor
  and the databases already on it, warns about every database that would be overwritten (not only on the source
  server), and lets you add a new target server without leaving the page.
- Compatibility options for logical restores to another instance or version, suggested from the target: remove
  `DEFINER` from views, routines, triggers and events; convert MySQL 8 `utf8mb4_0900_*`, MariaDB `uca1400` and
  `utf8mb3_*` collations (MySQL 8 → MariaDB / 5.7 and back); skip `SET @@GLOBAL.GTID_PURGED`. Only schema statements
  are rewritten, row data is never changed; mydumper schema files are rewritten after extraction.
- Physical restores: choosing the instance now fills its datadir, service and owner, and the form warns when the
  target runs another version series or flavor.
- New API endpoint `POST /api/runs/{id}/restore-check`.
- Fix: the GTID set of MySQL 8 mysqldump backups (`GTID_PURGED=/*!80000 '+'*/ '…'`) is now recorded, so
  point-in-time recovery to a GTID can tell when the target is already inside the backup.

## 1.0.0

- First release of DumpCat: MySQL/MariaDB backup & restore with a web UI.
- Lock-free logical backups (mysqldump/mariadb-dump, mydumper) and hot physical backups (XtraBackup/mariabackup).
- Storages: local/NFS, S3, Google Drive, OneDrive, FTP/FTPS, SFTP, WebDAV and any rclone backend.
- Binlog archiving and point-in-time recovery.
- Linux and Windows (service) installers; built-in updates with automatic rollback.
