import { get, post, put, del } from '../api.js';
import { html, setHTML, icon, on, fmtBytes, fmtDur, toast, toastErr, confirmBox, modal, busy, empty, formData, copyText, bindModalForm } from '../ui.js';
import { canOperate } from '../app.js';
import { agentPhysical, agentFolder, engines, engineOf } from '../components.js';

export const title = 'Database servers';

const GRANT_SQL = {
  mysql: `CREATE USER 'dumpcat'@'localhost' IDENTIFIED BY 'a_strong_password';
-- logical backups (mysqldump/mydumper)
GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, PROCESS, RELOAD,
      REPLICATION CLIENT, SHOW DATABASES ON *.* TO 'dumpcat'@'localhost';
-- additionally for physical backups (XtraBackup 8.x)
GRANT BACKUP_ADMIN ON *.* TO 'dumpcat'@'localhost';
GRANT SELECT ON performance_schema.log_status TO 'dumpcat'@'localhost';
GRANT SELECT ON performance_schema.keyring_component_status TO 'dumpcat'@'localhost';
-- additionally for continuous binlog archiving (point-in-time recovery)
GRANT REPLICATION SLAVE ON *.* TO 'dumpcat'@'localhost';
-- restores need write privileges (CREATE, DROP, INSERT, ALTER, INDEX, CREATE ROUTINE...)
-- or a separate restore user with ALL PRIVILEGES (PITR replays binlogs: needs
-- SYSTEM_VARIABLES_ADMIN / REPLICATION_APPLIER as well).`,
  mariadb: `CREATE USER 'dumpcat'@'localhost' IDENTIFIED BY 'a_strong_password';
GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, PROCESS, RELOAD,
      BINLOG MONITOR, SHOW DATABASES ON *.* TO 'dumpcat'@'localhost';
-- additionally for mariabackup
GRANT RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR, CONNECTION ADMIN,
      SLAVE MONITOR ON *.* TO 'dumpcat'@'localhost';
-- additionally for continuous binlog archiving (point-in-time recovery)
GRANT REPLICATION SLAVE ON *.* TO 'dumpcat'@'localhost';
-- restores need write privileges (or a separate restore user with ALL PRIVILEGES;
-- PITR replays binlogs: needs SUPER or BINLOG REPLAY as well).`,
  postgres: `CREATE ROLE dumpcat LOGIN PASSWORD 'a_strong_password';
-- read every table, sequence and schema of every database (PostgreSQL 14+)
GRANT pg_read_all_data TO dumpcat;
-- PostgreSQL 13 and older: make dumpcat the owner of the databases, or GRANT
-- USAGE on every schema and SELECT on every table and sequence, in each database.
-- Roles are saved with their passwords only when the account is a superuser.
-- Restores create databases and own what they restore: use a separate account
CREATE ROLE dumpcat_restore LOGIN CREATEDB CREATEROLE PASSWORD 'another_password';
-- pg_hba.conf must let the DumpCat host (or the agent) in, e.g.
--   host  all  dumpcat,dumpcat_restore  10.0.0.5/32  scram-sha-256`,
  mssql: `-- a SQL Server login for backups (or a Windows login: CREATE LOGIN [DOMAIN\\dumpcat] FROM WINDOWS)
CREATE LOGIN dumpcat WITH PASSWORD = 'a_strong_password';
-- reading backup headers (the log chain) needs CREATE DATABASE permission;
-- server state and file locations for the checks
GRANT CREATE ANY DATABASE, VIEW SERVER STATE, VIEW ANY DEFINITION TO dumpcat;
-- in every database to back up:
USE [shop];
CREATE USER dumpcat FOR LOGIN dumpcat;
ALTER ROLE db_backupoperator ADD MEMBER dumpcat;
ALTER ROLE db_datareader ADD MEMBER dumpcat; -- row counts for restore drills
-- (or simply: ALTER SERVER ROLE sysadmin ADD MEMBER dumpcat;)
-- restores create and replace databases: a separate login
CREATE LOGIN dumpcat_restore WITH PASSWORD = 'another_password';
ALTER SERVER ROLE dbcreator ADD MEMBER dumpcat_restore;
-- replacing an existing database needs its owner or sysadmin.
-- Windows: the SQL Server service account (NT Service\\MSSQLSERVER) needs Modify
-- permission on the backup folder; Linux: chown mssql:mssql <folder>`,
};

// The engine a server form is about.
const isPG = (e) => e === 'postgres';
const isMS = (e) => e === 'mssql';
const PRODUCT = { postgres: 'PostgreSQL', mssql: 'SQL Server' };

function infoView(res) {
  if (!res.ok) return html`<div class="alert bad">${icon('alert')}<div><b>Connection failed:</b> ${res.error}</div></div>`;
  const i = res.info;
  const user = i.databases.filter((d) => !d.system);
  const pg = isPG(i.engine);
  const ms = isMS(i.engine);
  return html`
    <div class="alert good" style="margin-bottom:14px">${icon('check')}<div>Connected — <b>${pg ? 'PostgreSQL ' : ms ? (i.edition || 'SQL Server') + ' ' : ''}${i.version}</b> ${i.version_comment ? html`<span class="muted">(${i.version_comment})</span>` : ''}</div></div>
    <div class="kv" style="margin-bottom:14px">
      ${ms ? html`<div>Server</div><div>${i.hostname} · ${i.platform}</div>
        <div>Service account</div><div class="mono small">${i.service_account || html`<span class="muted">not visible to this account (VIEW SERVER STATE)</span>`}</div>
        <div>Backup folder</div><div class="mono small">${i.backup_dir || html`<span class="muted">—</span>`}${i.backup_dir_via ? html` <span class="muted">· ${i.backup_dir_via}</span>` : ''}</div>
        <div>Data files</div><div class="mono small">${i.datadir || '—'}</div>
        <div>Compression</div><div>${i.backup_compression ? 'Available in this edition' : 'Not in this edition: DumpCat compresses'}</div>
        <div>Replication</div><div>${i.is_replica ? 'Secondary replica of an availability group' : 'Primary'}</div>
        <div>Current login</div><div class="mono small">${i.current_user}${i.grants && i.grants.length ? html` <span class="muted">(${i.grants.join(', ')})</span>` : ''}</div>`
    : pg ? html`<div>Cluster / address</div><div>${i.hostname}</div>
        <div>Data directory</div><div class="mono small">${i.datadir || html`<span class="muted">not readable by this account</span>`}</div>
        <div>WAL level</div><div>${i.wal_level || '—'}</div>
        <div>Replication</div><div>${i.is_replica ? 'This is a standby' : 'Primary'}${i.read_only ? ' · default_transaction_read_only' : ''}</div>
        <div>Current role</div><div class="mono small">${i.current_user}${i.grants && i.grants.length ? html` <span class="muted">(${i.grants.join(', ')})</span>` : ''}</div>`
    : html`<div>Flavor</div><div>${i.flavor}</div><div>Hostname</div><div>${i.hostname}</div>
        <div>Datadir</div><div class="mono small">${i.datadir}</div>
        <div>Binlog</div><div>${i.log_bin ? `on (${i.binlog_format})` : 'off'}${i.gtid_mode ? ' · GTID: ' + i.gtid_mode : ''}</div>
        <div>Replication</div><div>${i.is_replica ? 'This is a replica' : 'Not a replica'}${i.read_only ? ' · read_only' : ''}</div>
        <div>Current user</div><div class="mono small">${i.current_user}</div>`}
      <div>Uptime</div><div>${fmtDur(i.uptime)}</div>
      <div>Total data</div><div>${fmtBytes(i.total_bytes)} · ${user.length} database(s)</div>
    </div>
    ${i.backup_dir_error ? html`<div class="alert bad" style="margin-bottom:14px">${icon('alert')}<div><b>Backup folder:</b> ${i.backup_dir_error}</div></div>` : ''}
    ${res.support ? html`<div class="section-title">Backup methods</div>
      ${res.support.map((s) => html`<div class="alert ${s.ok ? 'good' : 'warn'}">${icon(s.ok ? 'check' : 'alert')}<div><b>${s.method === 'physical' ? 'Physical (xtrabackup/mariabackup)' : s.method === 'mssql_native' ? 'Native backup (BACKUP DATABASE)' : s.method}</b> — ${s.message}</div></div>`)}` : ''}
    ${i.warnings && i.warnings.length ? html`<div class="section-title" style="margin-top:14px">Notes</div>${i.warnings.map((w) => html`<div class="alert warn">${icon('alert')}<div>${w}</div></div>`)}` : ''}
    ${user.length ? html`<div class="section-title" style="margin-top:14px">Databases</div><div class="table-wrap" style="max-height:300px;overflow:auto"><table class="table"><thead><tr><th>Name</th><th class="right">Size</th><th class="right">Tables</th><th>${pg ? 'Encoding · locale' : ms ? 'Recovery model · collation' : 'Charset'}</th></tr></thead><tbody>
      ${user.map((d) => html`<tr><td><b>${d.name}</b>${d.non_innodb ? html` <span class="badge warn" title="Non-InnoDB tables">${d.non_innodb} non-InnoDB</span>` : ''}</td><td class="right num">${fmtBytes(d.size_bytes)}</td><td class="right num">${d.tables}</td><td class="small">${ms ? html`<span class="${d.recovery_model === 'SIMPLE' ? 'badge warn' : ''}">${d.recovery_model}</span> · ${d.collation}` : d.charset}${pg && d.collation ? ' · ' + d.collation : ''}</td></tr>`)}</tbody></table></div>` : ''}`;
}

// Ways to reach a server: directly, through an agent or a WireGuard peer.
async function loadRoutes() {
  const [agents, wgd] = await Promise.all([get('/agents').then((r) => r.items).catch(() => []), get('/wireguard').catch(() => null)]);
  return { agents, peers: wgd ? wgd.peers : [], wgOn: !!(wgd && wgd.state.running) };
}
const routeOf = (s) => (s.agent_id ? `agent:${s.agent_id}` : s.wg_peer_id ? `wg:${s.wg_peer_id}` : '');
function findRoute(routes, value) {
  const [kind, id] = (value || '').split(':');
  if (kind === 'agent') { const a = routes.agents.find((x) => x.agent.id === +id); return a ? { kind, a } : null; }
  if (kind === 'wg') { const p = routes.peers.find((x) => x.peer.id === +id); return p ? { kind, p } : null; }
  return null;
}

// Help under the Connect select: where the address is seen from.
function connectHelp(rt, wgOn, engine) {
  const pg = isPG(engine);
  if (isMS(engine)) {
    if (!rt) return html`DumpCat connects to the address below (TCP; <span class="mono">host\\INSTANCE</span> with an empty port asks the SQL Server Browser). SQL Server writes its backups on its own disk: see <b>Backup folder</b> below.`;
    if (rt.kind === 'wg') return html`DumpCat connects through its built-in WireGuard: enter the SQL Server host's WireGuard address <span class="mono">${rt.p.peer.address}</span> and the instance's TCP port. The backup folder must be shared with the DumpCat host (below).`;
    const fo = agentFolder(rt.a.status);
    return html`The agent opens the connection: enter the address <b>as seen from the agent host</b>, usually 127.0.0.1 port 1433; the agent must allow it (<span class="mono">-allow 127.0.0.1:1433</span>). An agent started with <span class="mono">--allow-mssql-dir</span> and SQL Server's backup folder moves the backup files: enter that folder below and leave <b>As DumpCat sees it</b> empty. Otherwise share the folder with the DumpCat host.
      <div style="margin-top:6px">${rt.a.status.online ? html`<span class="badge good">${icon('check')}Online</span>${rt.a.status.hostname ? ` on ${rt.a.status.hostname}` : ''}` : html`<span class="badge warn">${icon('alert')}Offline</span> start the agent on the database host`}</div>
      ${fo ? html`<div style="margin-top:6px">SQL Server backup folder: <span class="badge ${fo.ok ? 'good' : fo.off ? '' : 'warn'}">${fo.ok ? html`<span class="mono">${fo.short}</span>` : fo.short}</span> <span class="muted">${fo.detail}</span></div>` : ''}`;
  }
  if (!rt) return html`DumpCat connects to the address below. If the database host accepts no inbound connections, add an <a href="#/agents">agent</a> or a <a href="#/wireguard">WireGuard peer</a> on it.`;
  if (rt.kind === 'wg') {
    const { peer: p, status: st } = rt.p;
    return html`DumpCat connects through its built-in WireGuard: enter the database host's WireGuard address <span class="mono">${p.address}</span>${p.networks.length ? html` or an address in <span class="mono">${p.networks.join(', ')}</span>` : ''}. ${pg ? 'PostgreSQL there must listen on that address (listen_addresses) and let it in (pg_hba.conf).' : 'MySQL there must listen on that address. Physical backups are not available through WireGuard.'}
      <div style="margin-top:6px">${!wgOn ? html`<span class="badge warn">${icon('alert')}WireGuard is off</span> turn it on on the <a href="#/wireguard">WireGuard</a> page` : st.online ? html`<span class="badge good">${icon('check')}Connected</span> ${st.endpoint ? `from ${st.endpoint}` : ''}` : html`<span class="badge warn">${icon('alert')}Not connected</span> start WireGuard on the database host`}</div>`;
  }
  const st = rt.a.status;
  const ph = agentPhysical(st);
  return html`The agent opens the connection: enter the address <b>as seen from the agent host</b>, usually 127.0.0.1 port ${pg ? '5432' : '3306'} or a socket${pg ? html` directory. The agent must allow it (<span class="mono">-allow 127.0.0.1:5432</span>).` : html`. Physical backups run on that host when the agent is started with <span class="mono">--allow-physical</span>.`}
    <div style="margin-top:6px">${st.online ? html`<span class="badge good">${icon('check')}Online</span>${st.hostname ? ` on ${st.hostname}` : ''} — may connect to <span class="mono">${st.allow.join(', ')}</span>` : html`<span class="badge warn">${icon('alert')}Offline</span> start the agent on the database host`}</div>
    ${ph && !pg ? html`<div style="margin-top:6px">Physical backups: <span class="badge ${ph.ok ? 'good' : ph.off ? '' : 'warn'}">${ph.short}</span> <span class="muted">${ph.detail}</span></div>` : ''}`;
}

// An agent that moves SQL Server's backup files (--allow-mssql-dir) names their folder.
function prefillFolder(form, rt) {
  const fo = rt && rt.kind === 'agent' && rt.a.status.caps && rt.a.status.caps.mssql;
  if (isMS(form.engine.value) && fo && fo.dir && !form.backup_dir.value.trim() && !form.backup_dir_local.value.trim()) form.backup_dir.value = fo.dir;
}

function serverForm(s = {}, routes = { agents: [], peers: [], wgOn: false }, engs = []) {
  const v = (k, d = '') => s[k] ?? d;
  const cur = routeOf(s);
  const rt = findRoute(routes, cur);
  const engine = engineOf(engs, s.engine).engine;
  const pg = isPG(engine);
  const ms = isMS(engine);
  return html`<form id="srv-form" autocomplete="off">
  ${!s.id ? html`<div class="alert info" style="margin-bottom:14px">${icon('shield')}<div>Use a dedicated, least-privilege database account for backups (not root or postgres).
    <button type="button" class="btn btn-sm" id="show-grant" style="margin-left:4px">Show the SQL to create it</button></div></div>` : ''}
  <div class="form-grid">
    <div class="field"><label class="req">Display name</label><input type="text" name="name" value="${v('name')}" required placeholder="e.g. db-prod-01"></div>
    <div class="field"><label for="srv-engine">Database engine</label><select name="engine" id="srv-engine">${engs.map((e) => html`<option value="${e.engine}" ${e.engine === engine ? 'selected' : ''}>${e.label}</option>`)}</select>
      ${s.id ? html`<div class="help">A server with jobs or drills keeps its engine.</div>` : ''}</div>
    <div class="field full"><label>Connect</label><select name="connect"><option value="">Directly from DumpCat</option>
      ${routes.agents.length ? html`<optgroup label="Agents">${routes.agents.map(({ agent: a, status: st }) => html`<option value="agent:${a.id}" ${cur === `agent:${a.id}` ? 'selected' : ''}>Through agent: ${a.name}${st.online ? '' : ' (offline)'}</option>`)}</optgroup>` : ''}
      ${routes.peers.length ? html`<optgroup label="WireGuard">${routes.peers.map(({ peer: p, status: st }) => html`<option value="wg:${p.id}" ${cur === `wg:${p.id}` ? 'selected' : ''}>Through WireGuard: ${p.name} (${p.address})${!routes.wgOn ? ' — WireGuard off' : st.online ? '' : ' (not connected)'}</option>`)}</optgroup>` : ''}</select>
      <div class="help" id="connect-help">${connectHelp(rt, routes.wgOn, engine)}</div></div>
    <div class="field"><label>Host</label><input type="text" name="host" value="${v('host', '127.0.0.1')}" placeholder="${ms ? 'sql01.example.com or sql01\\SQLEXPRESS' : '127.0.0.1 or db.example.com'}"></div>
    <div class="field"><label>Port</label><input type="number" name="port" value="${v('port', engineOf(engs, engine).default_port)}" min="1" max="65535"></div>
    <div class="field full ${ms ? 'hidden' : ''}" id="socket-field"><label>Unix socket <span class="muted" id="socket-note">(optional — used when host is localhost${pg ? '; the socket directory' : ''})</span></label><input type="text" name="socket" value="${v('socket')}" placeholder="${pg ? '/var/run/postgresql' : '/var/run/mysqld/mysqld.sock'}"></div>
    <div class="field"><label class="req">User</label><input type="text" name="username" value="${v('username', 'dumpcat')}" required autocomplete="off">
      <div class="help ${ms ? '' : 'hidden'}" id="user-help">A SQL Server login, or <span class="mono">DOMAIN\\user</span> for Windows authentication (NTLM).</div></div>
    <div class="field"><label>Password</label><input type="password" name="password" placeholder="${s.has_password ? '•••••• saved — leave blank to keep' : ''}" autocomplete="new-password">
      <div class="help">Stored encrypted with AES-256-GCM; never appears on a command line.</div></div>
    <div class="field ${pg || ms ? 'hidden' : ''}" id="flavor-field"><label>Server flavor</label><select name="flavor">${[['auto', 'Auto-detect'], ['mysql', 'MySQL'], ['percona', 'Percona Server'], ['mariadb', 'MariaDB']].map(([a, b]) => html`<option value="${a}" ${v('flavor', 'auto') === a ? 'selected' : ''}>${b}</option>`)}</select></div>
    <div class="field"><label>TLS</label><select name="tls_mode">${[['', 'Client default'], ['disabled', 'Disabled'], ['required', 'Required (no certificate check)'], ['verify', 'Required + verify certificate']].map(([a, b]) => html`<option value="${a}" ${v('tls_mode') === a ? 'selected' : ''}>${b}</option>`)}</select></div>
    <div class="field full ${v('tls_mode') === 'verify' ? '' : 'hidden'}" id="ca-field"><label>CA certificate (PEM)</label><textarea name="ca_cert" rows="3">${v('ca_cert')}</textarea></div>
  </div>
  <details class="adv ${ms ? '' : 'hidden'}" id="folder-box" style="margin-top:14px" open><summary>Backup folder (SQL Server writes its backups on its own host)</summary>
    <div class="help" style="margin-bottom:10px">BACKUP DATABASE writes on the SQL Server host. DumpCat reads each file from this folder, stores it on the job's storages and deletes it; restores pass through it the other way. When DumpCat runs on the SQL Server host the two paths are the same: leave the second one empty. Otherwise share the folder (an SMB share, or a mount on Linux) and enter how DumpCat sees it — or connect through an agent started with <span class="mono">--allow-mssql-dir</span> on the SQL Server host, which moves the files: then the second path stays empty.</div>
    <div class="form-grid">
      <div class="field"><label>As SQL Server sees it</label><input type="text" name="backup_dir" value="${v('backup_dir')}" placeholder="empty: the instance's default backup folder">
        <div class="help">e.g. <span class="mono">D:\\Backup\\DumpCat</span>, <span class="mono">\\\\nas\\sql-backup</span>, <span class="mono">/var/opt/mssql/dumpcat</span>. SQL Server's service account needs Modify permission there.</div></div>
      <div class="field"><label>As DumpCat sees it</label><input type="text" name="backup_dir_local" value="${v('backup_dir_local')}" placeholder="empty: the same path">
        <div class="help">e.g. <span class="mono">\\\\sql01\\DumpCat$</span> (Windows), <span class="mono">/mnt/sql01-backup</span> (Linux, a CIFS mount); empty through an agent with <span class="mono">--allow-mssql-dir</span>. "Test connection" checks that both name the same folder.</div></div>
    </div></details>
  <details class="adv" style="margin-top:14px" ${s.restore_username ? 'open' : ''}><summary>Separate account for restores (recommended)</summary>
    <div class="help" style="margin-bottom:10px" id="restore-help">${restoreHelp(engine)}</div>
    <div class="form-grid">
      <div class="field"><label>Restore user</label><input type="text" name="restore_username" value="${v('restore_username')}" autocomplete="off"></div>
      <div class="field"><label>Restore password</label><input type="password" name="restore_password" placeholder="${s.has_restore_password ? '•••••• saved — leave blank to keep' : ''}" autocomplete="new-password"></div>
    </div></details>
  <details class="adv ${(rt && rt.kind === 'wg') || pg || ms ? 'hidden' : ''}" id="physical-box" style="margin-top:14px"><summary>For physical backup/restore (DumpCat on the database server itself, or an agent with --allow-physical)</summary>
    <div class="form-grid">
      <div class="field"><label>Datadir</label><input type="text" name="datadir" value="${v('datadir')}" placeholder="auto-detected from @@datadir"></div>
      <div class="field ${rt ? 'hidden' : ''}" data-local><label>systemd service name</label><input type="text" name="service_name" value="${v('service_name')}" placeholder="mysql / mariadb / mysqld"></div>
      <div class="field ${rt ? 'hidden' : ''}" data-local><label>OS user that owns the datadir</label><input type="text" name="os_user" value="${v('os_user')}" placeholder="mysql"></div>
    </div></details>
  <div class="field" style="margin-top:14px"><label>Notes</label><input type="text" name="notes" value="${v('notes')}"></div>
  <div id="test-result" style="margin-top:14px"></div>
  <details class="adv" id="grant-box" style="margin-top:14px"><summary>SQL to create a least-privilege backup user</summary>
    <div class="tabs" style="margin-bottom:8px"><button type="button" class="${pg || ms ? '' : 'active'}" data-grant="mysql">MySQL / Percona</button><button type="button" data-grant="mariadb">MariaDB</button><button type="button" class="${pg ? 'active' : ''}" data-grant="postgres">PostgreSQL</button><button type="button" class="${ms ? 'active' : ''}" data-grant="mssql">SQL Server</button></div>
    <pre class="code" id="grant-sql">${GRANT_SQL[pg ? 'postgres' : ms ? 'mssql' : 'mysql']}</pre><button type="button" class="btn btn-sm" id="copy-grant" style="margin-top:8px">${icon('copy')}Copy</button>
  </details></form>`;
}

function restoreHelp(engine) {
  if (isMS(engine)) return 'Keep the backup login read-only; restores use a login that may create databases (dbcreator) — to replace an existing database it must own it, or be sysadmin. Leave blank to use the account above.';
  return isPG(engine)
    ? 'Keep the backup account read-only; restores use an account that may create databases (CREATEDB) and, to restore roles, CREATEROLE — or a superuser. Leave blank to use the account above.'
    : 'Keep the backup user read-only; restores use a user with write privileges (CREATE, DROP, INSERT, ALTER, CREATE ROUTINE, EVENT, TRIGGER… or ALL PRIVILEGES). Leave blank to use the account above.';
}

// Add/edit dialog. The restore form uses it too, to add a target instance
// without leaving the page; onSaved receives the saved server. defaults
// presets a new server (e.g. its engine).
export async function openServerForm(s, onSaved, defaults = {}) {
  const [routes, engs] = await Promise.all([loadRoutes(), engines()]);
  const m = modal({
    title: s ? `Edit server: ${s.name}` : 'Add database server', size: 'lg', body: serverForm(s || defaults, routes, engs),
    footer: html`<button class="btn" id="test">${icon('zap')}Test connection</button><span style="flex:1"></span><button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="save">${icon('check')}Save</button>`,
  });
  const form = m.el.querySelector('#srv-form');
  const payload = () => {
    const d = formData(form);
    const [kind, id] = (d.connect || '').split(':');
    delete d.connect;
    d.agent_id = kind === 'agent' ? +id : 0;
    d.wg_peer_id = kind === 'wg' ? +id : 0;
    if (s) d.id = s.id;
    return d;
  };
  const showGrant = (key) => {
    m.el.querySelectorAll('[data-grant]').forEach((x) => x.classList.toggle('active', x.dataset.grant === key));
    m.el.querySelector('#grant-sql').textContent = GRANT_SQL[key];
  };
  // the engine decides the port, the flavor field, physical backups and the help
  form.engine.addEventListener('change', () => {
    const e = form.engine.value;
    const pg = isPG(e);
    const ms = isMS(e);
    const ports = engs.map((x) => String(x.default_port));
    if (!form.port.value || ports.includes(form.port.value)) form.port.value = engineOf(engs, e).default_port;
    m.el.querySelector('#flavor-field').classList.toggle('hidden', pg || ms);
    m.el.querySelector('#socket-field').classList.toggle('hidden', ms);
    m.el.querySelector('#folder-box').classList.toggle('hidden', !ms);
    m.el.querySelector('#user-help').classList.toggle('hidden', !ms);
    form.host.placeholder = ms ? 'sql01.example.com or sql01\\SQLEXPRESS' : '127.0.0.1 or db.example.com';
    form.socket.placeholder = pg ? '/var/run/postgresql' : '/var/run/mysqld/mysqld.sock';
    m.el.querySelector('#socket-note').textContent = `(optional — used when host is localhost${pg ? '; the socket directory' : ''})`;
    m.el.querySelector('#restore-help').textContent = restoreHelp(e);
    const rt = findRoute(routes, form.connect.value);
    setHTML(m.el.querySelector('#connect-help'), connectHelp(rt, routes.wgOn, e));
    prefillFolder(form, rt);
    m.el.querySelector('#physical-box').classList.toggle('hidden', (!!rt && rt.kind === 'wg') || pg || ms);
    showGrant(pg ? 'postgres' : ms ? 'mssql' : 'mysql');
  });
  form.connect.addEventListener('change', () => {
    const rt = findRoute(routes, form.connect.value);
    setHTML(m.el.querySelector('#connect-help'), connectHelp(rt, routes.wgOn, form.engine.value));
    // through an agent the backup tool runs on the database host; replacing
    // its datadir (service, owner) only works where DumpCat runs
    m.el.querySelector('#physical-box').classList.toggle('hidden', (!!rt && rt.kind === 'wg') || isPG(form.engine.value) || isMS(form.engine.value));
    m.el.querySelectorAll('[data-local]').forEach((f) => f.classList.toggle('hidden', !!rt));
    prefillFolder(form, rt);
    // through WireGuard the host is the peer's VPN address, not localhost
    if (rt && rt.kind === 'wg' && ['', '127.0.0.1', 'localhost', '::1'].includes(form.host.value.trim())) form.host.value = rt.p.peer.address;
  });
  form.tls_mode.addEventListener('change', () => m.el.querySelector('#ca-field').classList.toggle('hidden', form.tls_mode.value !== 'verify'));
  on(m.el, 'click', '[data-grant]', (e, b) => showGrant(b.dataset.grant));
  m.el.querySelector('#copy-grant').onclick = () => copyText(m.el.querySelector('#grant-sql').textContent);
  on(m.el, 'click', '#show-grant', () => {
    const g = m.el.querySelector('#grant-box');
    g.open = true;
    g.scrollIntoView({ behavior: 'smooth', block: 'start' });
  });
  m.el.querySelector('#test').onclick = (e) => busy(e.currentTarget, async () => {
    const box = m.el.querySelector('#test-result');
    try { setHTML(box, infoView(await post('/servers/test', payload()))); } catch (er) { setHTML(box, html`<div class="alert bad">${icon('alert')}<div>${er.message}</div></div>`); }
  });
  bindModalForm(form, m.el.querySelector('#save'), async () => {
    const saved = s ? await put(`/servers/${s.id}`, payload()) : await post('/servers', payload());
    m.close();
    toast('Server saved');
    await onSaved(saved);
  });
}

export async function render(ctx) {
  const { el, query } = ctx;
  let [list, routes] = await Promise.all([get('/servers'), loadRoutes()]);
  const routeBadge = (s) => {
    const rt = findRoute(routes, routeOf(s));
    if (!rt) return '';
    if (rt.kind === 'wg') {
      const up = routes.wgOn && rt.p.status.online;
      return html` <a class="badge ${up ? 'outline' : 'warn'}" href="#/wireguard" title="${up ? 'Through WireGuard' : 'Through WireGuard (not connected)'}">${icon('shield')}${rt.p.peer.name}</a>`;
    }
    return html` <a class="badge ${rt.a.status.online ? 'outline' : 'warn'}" href="#/agents" title="${rt.a.status.online ? 'Through agent' : 'Through agent (offline)'}">${icon('link')}${rt.a.agent.name}</a>`;
  };
  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>Database servers</h1><div class="sub">The MySQL, MariaDB, Percona, PostgreSQL and SQL Server servers to back up</div></div>
        ${canOperate() ? html`<button class="btn btn-primary" id="add">${icon('plus')}Add server</button>` : ''}</div>
      <div class="card"><div class="table-wrap">${list.length ? html`<table class="table"><thead><tr><th>Name</th><th>Connection</th><th>Version</th><th class="hide-sm">User</th><th class="hide-sm">Notes</th><th></th></tr></thead><tbody>
        ${list.map((s) => html`<tr><td><b>${s.name}</b></td><td class="mono small">${s.socket && (!s.host || s.host === 'localhost') ? s.socket : s.port ? `${s.host}:${s.port}` : s.host}${s.tls_mode && s.tls_mode !== 'disabled' ? html` <span class="badge outline">${icon('lock')}TLS</span>` : ''}${routeBadge(s)}</td>
          <td>${PRODUCT[s.engine] ? html`<span class="badge outline">${PRODUCT[s.engine]}</span> ` : ''}${s.version || html`<span class="muted">unknown</span>`}</td><td class="hide-sm">${s.username}</td><td class="hide-sm muted">${s.notes}</td>
          <td class="actions"><button class="btn btn-sm" data-info="${s.id}">${icon('eye')}Check</button>
            ${canOperate() ? html`<button class="btn btn-sm btn-ghost btn-icon" data-edit="${s.id}" title="Edit">${icon('edit')}</button><button class="btn btn-sm btn-ghost btn-icon" data-del="${s.id}" title="Delete">${icon('trash')}</button>` : ''}</td></tr>`)}
      </tbody></table>` : empty('No servers yet. Add your first MySQL, MariaDB, PostgreSQL or SQL Server server.', 'database')}</div></div>`);
  };
  draw();
  const reload = async () => {
    [list, routes] = await Promise.all([get('/servers'), loadRoutes()]);
    draw();
  };

  const openForm = (s) => openServerForm(s, reload);

  on(el, 'click', '#add', () => openForm(null));
  on(el, 'click', '[data-edit]', async (e, b) => openForm(await get(`/servers/${b.dataset.edit}`)));
  // #/servers?edit=<id>: where an AI assistant sends the user to enter a password (MCP never takes one)
  if (query.edit && canOperate() && list.some((x) => x.id === +query.edit)) openForm(await get(`/servers/${+query.edit}`));
  on(el, 'click', '[data-del]', async (e, b) => {
    const s = list.find((x) => x.id === +b.dataset.del);
    if (!(await confirmBox({ title: 'Delete server', message: html`Delete server <b>${s.name}</b>? (Existing backups are kept.)`, danger: true, okText: 'Delete' }))) return;
    try { await del(`/servers/${s.id}`); toast('Deleted'); await reload(); } catch (er) { toastErr(er); }
  });
  on(el, 'click', '[data-info]', async (e, b) => {
    const s = list.find((x) => x.id === +b.dataset.info);
    const m = modal({ title: `Check: ${s.name}`, size: 'lg', body: html`<div class="loading-page">${icon('loader', 'spin')}Connecting…</div>`, footer: html`<button class="btn" data-close>Close</button>` });
    try { setHTML(m.body, infoView(await get(`/servers/${s.id}/info`))); } catch (er) { setHTML(m.body, html`<div class="alert bad">${icon('alert')}<div>${er.message}</div></div>`); }
    reload();
  });
}
