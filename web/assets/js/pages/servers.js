import { get, post, put, del } from '../api.js';
import { html, setHTML, icon, on, fmtBytes, fmtDur, toast, toastErr, confirmBox, modal, busy, empty, formData, copyText, bindModalForm } from '../ui.js';
import { canOperate } from '../app.js';

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
};

function infoView(res) {
  if (!res.ok) return html`<div class="alert bad">${icon('alert')}<div><b>Connection failed:</b> ${res.error}</div></div>`;
  const i = res.info;
  const user = i.databases.filter((d) => !d.system);
  return html`
    <div class="alert good" style="margin-bottom:14px">${icon('check')}<div>Connected — <b>${i.version}</b> ${i.version_comment ? html`<span class="muted">(${i.version_comment})</span>` : ''}</div></div>
    <div class="kv" style="margin-bottom:14px">
      <div>Flavor</div><div>${i.flavor}</div><div>Hostname</div><div>${i.hostname}</div>
      <div>Datadir</div><div class="mono small">${i.datadir}</div>
      <div>Binlog</div><div>${i.log_bin ? `on (${i.binlog_format})` : 'off'}${i.gtid_mode ? ' · GTID: ' + i.gtid_mode : ''}</div>
      <div>Replication</div><div>${i.is_replica ? 'This is a replica' : 'Not a replica'}${i.read_only ? ' · read_only' : ''}</div>
      <div>Current user</div><div class="mono small">${i.current_user}</div>
      <div>Uptime</div><div>${fmtDur(i.uptime)}</div>
      <div>Total data</div><div>${fmtBytes(i.total_bytes)} · ${user.length} database(s)</div>
    </div>
    ${res.support ? html`<div class="section-title">Backup methods available on the DumpCat host</div>
      ${res.support.map((s) => html`<div class="alert ${s.ok ? 'good' : 'warn'}">${icon(s.ok ? 'check' : 'alert')}<div><b>${s.method === 'physical' ? 'Physical (xtrabackup/mariabackup)' : s.method}</b> — ${s.message}</div></div>`)}` : ''}
    ${i.warnings && i.warnings.length ? html`<div class="section-title" style="margin-top:14px">Notes</div>${i.warnings.map((w) => html`<div class="alert warn">${icon('alert')}<div>${w}</div></div>`)}` : ''}
    ${user.length ? html`<div class="section-title" style="margin-top:14px">Databases</div><div class="table-wrap" style="max-height:300px;overflow:auto"><table class="table"><thead><tr><th>Name</th><th class="right">Size</th><th class="right">Tables</th><th>Charset</th></tr></thead><tbody>
      ${user.map((d) => html`<tr><td><b>${d.name}</b>${d.non_innodb ? html` <span class="badge warn" title="Non-InnoDB tables">${d.non_innodb} non-InnoDB</span>` : ''}</td><td class="right num">${fmtBytes(d.size_bytes)}</td><td class="right num">${d.tables}</td><td class="small">${d.charset}</td></tr>`)}</tbody></table></div>` : ''}`;
}

// Help under the Connect select: where the address is seen from.
function connectHelp(ag) {
  if (!ag) return html`DumpCat connects to the address below. If the database host accepts no inbound connections, add an <a href="#/agents">agent</a> on it.`;
  const st = ag.status;
  return html`The agent opens the connection: enter the address <b>as seen from the agent host</b>, usually 127.0.0.1 port 3306 or a socket. Physical backups are not available through an agent.
    <div style="margin-top:6px">${st.online ? html`<span class="badge good">${icon('check')}Online</span>${st.hostname ? ` on ${st.hostname}` : ''} — may connect to <span class="mono">${st.allow.join(', ')}</span>` : html`<span class="badge warn">${icon('alert')}Offline</span> start the agent on the database host`}</div>`;
}

function serverForm(s = {}, agents = []) {
  const v = (k, d = '') => s[k] ?? d;
  const ag = agents.find((a) => a.agent.id === s.agent_id);
  return html`<form id="srv-form" autocomplete="off">
  ${!s.id ? html`<div class="alert info" style="margin-bottom:14px">${icon('shield')}<div>Use a dedicated, least-privilege MySQL user for backups (not root).
    <button type="button" class="btn btn-sm" id="show-grant" style="margin-left:4px">Show the SQL to create it</button></div></div>` : ''}
  <div class="form-grid">
    <div class="field full"><label class="req">Display name</label><input type="text" name="name" value="${v('name')}" required placeholder="e.g. db-prod-01"></div>
    <div class="field full"><label>Connect</label><select name="agent_id"><option value="0">Directly from DumpCat</option>
      ${agents.map(({ agent: a, status: st }) => html`<option value="${a.id}" ${s.agent_id === a.id ? 'selected' : ''}>Through agent: ${a.name}${st.online ? '' : ' (offline)'}</option>`)}</select>
      <div class="help" id="connect-help">${connectHelp(ag)}</div></div>
    <div class="field"><label>Host</label><input type="text" name="host" value="${v('host', '127.0.0.1')}" placeholder="127.0.0.1 or db.example.com"></div>
    <div class="field"><label>Port</label><input type="number" name="port" value="${v('port', 3306)}" min="1" max="65535"></div>
    <div class="field full"><label>Unix socket <span class="muted">(optional — used when host is localhost)</span></label><input type="text" name="socket" value="${v('socket')}" placeholder="/var/run/mysqld/mysqld.sock"></div>
    <div class="field"><label class="req">User</label><input type="text" name="username" value="${v('username', 'dumpcat')}" required autocomplete="off"></div>
    <div class="field"><label>Password</label><input type="password" name="password" placeholder="${s.has_password ? '•••••• saved — leave blank to keep' : ''}" autocomplete="new-password">
      <div class="help">Stored encrypted with AES-256-GCM; never appears on a command line.</div></div>
    <div class="field"><label>Server flavor</label><select name="flavor">${[['auto', 'Auto-detect'], ['mysql', 'MySQL'], ['percona', 'Percona Server'], ['mariadb', 'MariaDB']].map(([a, b]) => html`<option value="${a}" ${v('flavor', 'auto') === a ? 'selected' : ''}>${b}</option>`)}</select></div>
    <div class="field"><label>TLS</label><select name="tls_mode">${[['', 'Client default'], ['disabled', 'Disabled'], ['required', 'Required (no certificate check)'], ['verify', 'Required + verify certificate']].map(([a, b]) => html`<option value="${a}" ${v('tls_mode') === a ? 'selected' : ''}>${b}</option>`)}</select></div>
    <div class="field full ${v('tls_mode') === 'verify' ? '' : 'hidden'}" id="ca-field"><label>CA certificate (PEM)</label><textarea name="ca_cert" rows="3">${v('ca_cert')}</textarea></div>
  </div>
  <details class="adv" style="margin-top:14px" ${s.restore_username ? 'open' : ''}><summary>Separate account for restores (recommended)</summary>
    <div class="help" style="margin-bottom:10px">Keep the backup user read-only; restores use a user with write privileges (CREATE, DROP, INSERT, ALTER, CREATE ROUTINE, EVENT, TRIGGER… or ALL PRIVILEGES). Leave blank to use the account above.</div>
    <div class="form-grid">
      <div class="field"><label>Restore user</label><input type="text" name="restore_username" value="${v('restore_username')}" autocomplete="off"></div>
      <div class="field"><label>Restore password</label><input type="password" name="restore_password" placeholder="${s.has_restore_password ? '•••••• saved — leave blank to keep' : ''}" autocomplete="new-password"></div>
    </div></details>
  <details class="adv ${ag ? 'hidden' : ''}" id="physical-box" style="margin-top:14px"><summary>For physical backup/restore (only when DumpCat runs on the database server itself)</summary>
    <div class="form-grid">
      <div class="field"><label>Datadir</label><input type="text" name="datadir" value="${v('datadir')}" placeholder="auto-detected from @@datadir"></div>
      <div class="field"><label>systemd service name</label><input type="text" name="service_name" value="${v('service_name')}" placeholder="mysql / mariadb / mysqld"></div>
      <div class="field"><label>OS user that owns the datadir</label><input type="text" name="os_user" value="${v('os_user')}" placeholder="mysql"></div>
    </div></details>
  <div class="field" style="margin-top:14px"><label>Notes</label><input type="text" name="notes" value="${v('notes')}"></div>
  <div id="test-result" style="margin-top:14px"></div>
  <details class="adv" id="grant-box" style="margin-top:14px"><summary>SQL to create a least-privilege backup user</summary>
    <div class="tabs" style="margin-bottom:8px"><button type="button" class="active" data-grant="mysql">MySQL / Percona</button><button type="button" data-grant="mariadb">MariaDB</button></div>
    <pre class="code" id="grant-sql">${GRANT_SQL.mysql}</pre><button type="button" class="btn btn-sm" id="copy-grant" style="margin-top:8px">${icon('copy')}Copy</button>
  </details></form>`;
}

// Add/edit dialog. The restore form uses it too, to add a target instance
// without leaving the page; onSaved receives the saved server.
export async function openServerForm(s, onSaved) {
  const agents = await get('/agents').then((r) => r.items).catch(() => []);
  const m = modal({
    title: s ? `Edit server: ${s.name}` : 'Add database server', size: 'lg', body: serverForm(s || {}, agents),
    footer: html`<button class="btn" id="test">${icon('zap')}Test connection</button><span style="flex:1"></span><button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="save">${icon('check')}Save</button>`,
  });
  const form = m.el.querySelector('#srv-form');
  const payload = () => {
    const d = formData(form);
    d.agent_id = +d.agent_id || 0;
    if (s) d.id = s.id;
    return d;
  };
  form.agent_id.addEventListener('change', () => {
    const ag = agents.find((a) => a.agent.id === +form.agent_id.value);
    setHTML(m.el.querySelector('#connect-help'), connectHelp(ag));
    m.el.querySelector('#physical-box').classList.toggle('hidden', !!ag);
  });
  form.tls_mode.addEventListener('change', () => m.el.querySelector('#ca-field').classList.toggle('hidden', form.tls_mode.value !== 'verify'));
  on(m.el, 'click', '[data-grant]', (e, b) => {
    m.el.querySelectorAll('[data-grant]').forEach((x) => x.classList.toggle('active', x === b));
    m.el.querySelector('#grant-sql').textContent = GRANT_SQL[b.dataset.grant];
  });
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
  const { el } = ctx;
  let [list, agents] = await Promise.all([get('/servers'), get('/agents').then((r) => r.items).catch(() => [])]);
  const agentBadge = (id) => {
    const a = agents.find((x) => x.agent.id === id);
    if (!a) return '';
    return html` <a class="badge ${a.status.online ? 'outline' : 'warn'}" href="#/agents" title="${a.status.online ? 'Through agent' : 'Through agent (offline)'}">${icon('link')}${a.agent.name}</a>`;
  };
  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>Database servers</h1><div class="sub">The MySQL / MariaDB / Percona servers to back up</div></div>
        ${canOperate() ? html`<button class="btn btn-primary" id="add">${icon('plus')}Add server</button>` : ''}</div>
      <div class="card"><div class="table-wrap">${list.length ? html`<table class="table"><thead><tr><th>Name</th><th>Connection</th><th>Version</th><th class="hide-sm">User</th><th class="hide-sm">Notes</th><th></th></tr></thead><tbody>
        ${list.map((s) => html`<tr><td><b>${s.name}</b></td><td class="mono small">${s.socket && (!s.host || s.host === 'localhost') ? s.socket : `${s.host}:${s.port}`}${s.tls_mode ? html` <span class="badge outline">${icon('lock')}TLS</span>` : ''}${s.agent_id ? agentBadge(s.agent_id) : ''}</td>
          <td>${s.version || html`<span class="muted">unknown</span>`}</td><td class="hide-sm">${s.username}</td><td class="hide-sm muted">${s.notes}</td>
          <td class="actions"><button class="btn btn-sm" data-info="${s.id}">${icon('eye')}Check</button>
            ${canOperate() ? html`<button class="btn btn-sm btn-ghost btn-icon" data-edit="${s.id}" title="Edit">${icon('edit')}</button><button class="btn btn-sm btn-ghost btn-icon" data-del="${s.id}" title="Delete">${icon('trash')}</button>` : ''}</td></tr>`)}
      </tbody></table>` : empty('No servers yet. Add your first MySQL/MariaDB server.', 'database')}</div></div>`);
  };
  draw();
  const reload = async () => {
    [list, agents] = await Promise.all([get('/servers'), get('/agents').then((r) => r.items).catch(() => [])]);
    draw();
  };

  const openForm = (s) => openServerForm(s, reload);

  on(el, 'click', '#add', () => openForm(null));
  on(el, 'click', '[data-edit]', async (e, b) => openForm(await get(`/servers/${b.dataset.edit}`)));
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
