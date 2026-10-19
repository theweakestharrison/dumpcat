import { get, post, put } from '../api.js';
import { html, setHTML, icon, on, $, $$, fmtBytes, fmtDate, toast, busy, debounce, describeCron, validate, formError } from '../ui.js';
import { navigate, isAdmin, canOperate, setTitle, setLeaveGuard } from '../app.js';
import { backBtn } from '../components.js';

export const title = 'Backup job';

const DEFAULT_JOB = {
  name: '', server_id: 0, method: 'mysqldump', db_mode: 'all', databases: [], exclude_tables: [],
  schedule: '0 2 * * *', incr_schedule: '', compression: 'zstd', compression_level: 0,
  encrypt: false, threads: 4,
  options: {
    single_file: false, routines: true, triggers: true, events: true, include_users: true, record_binlog: false, gtid_purged: 'OFF',
    chunk_rows: 500000, long_query_guard: 60, lock_mode: '', ftwrl_wait_timeout: 60, slave_info: false, safe_slave_backup: false,
    low_priority: true, stream_upload: false, verify_after: false, bwlimit: '', extra_args: '', pre_command: '', post_command: '',
    binlog_archive: false, binlog_rotate_minutes: 15,
  },
  retention: { keep_last: 7, keep_days: 0, keep_daily: 0, keep_weekly: 0, keep_monthly: 0 },
  targets: [], enabled: true, notify_success: false, notify_failure: true, timeout_minutes: 0,
};

const PRESETS = [
  ['', '— presets —'], ['0 2 * * *', 'Daily at 02:00'], ['0 1,13 * * *', 'Twice a day (01:00, 13:00)'], ['0 */6 * * *', 'Every 6 hours'],
  ['0 * * * *', 'Every hour'], ['30 3 * * 0', 'Weekly on Sunday at 03:30'], ['0 4 1 * *', 'Day 1 of every month at 04:00'], ['@manual', 'Manual only'],
];
const INCR_PRESETS = [['', '— presets —'], ['0 * * * *', 'Every hour'], ['0 */4 * * *', 'Every 4 hours'], ['0 8-20/2 * * *', 'Every 2 hours (08:00–20:00)'], ['0 2 * * 1-6', 'Daily at 02:00 (Mon–Sat)'], ['@manual', 'No incrementals']];

const METHODS = [
  { id: 'mysqldump', title: 'mysqldump / mariadb-dump', icon: 'file', desc: 'Single-threaded logical backup with --single-transaction (no InnoDB locking). Most flexible: restore individual databases, rename them, move across versions. Best for databases under ~50 GB.' },
  { id: 'mydumper', title: 'mydumper / myloader', icon: 'zap', desc: 'Multi-threaded logical backup that splits large tables into chunks; parallel restore is many times faster. Best for large databases that still need logical restores.' },
  { id: 'physical', title: 'Physical (XtraBackup / MariaBackup)', icon: 'hdd', desc: 'Copies data files while the database is running (hot backup, no InnoDB locking), with incremental support. Fastest for very large databases (100 GB–TB); restore is a file copy. Must run on the database server itself.' },
];

export async function render(ctx) {
  const { el, params, query } = ctx;
  if (!canOperate()) { setHTML(el, html`<div class="alert warn">${icon('lock')}<div>You have read-only access.</div></div>`); return; }
  const editing = params.id ? +params.id : 0;
  const [servers, storages] = await Promise.all([get('/servers'), get('/storages')]);
  let job = structuredClone(DEFAULT_JOB);
  let hasPass = false;
  if (editing || query.clone) {
    const src = (await get(`/jobs/${editing || query.clone}`)).job;
    job = { ...job, ...src, options: { ...job.options, ...src.options }, retention: { ...job.retention, ...src.retention } };
    hasPass = !!src.has_passphrase;
    if (!editing) { job.name = src.name + ' (copy)'; delete job.id; hasPass = false; job.encrypt = false; }
  }
  if (!job.server_id && servers.length) job.server_id = servers[0].id;
  if (!job.targets.length && storages.length === 1) job.targets = [{ storage_id: storages[0].id, retention: null }];
  const infoCache = {};

  const titleText = editing ? `Edit job: ${job.name}` : 'New backup job';
  setTitle([{ label: 'Backup jobs', href: '#/jobs' }, titleText]);
  const SECTIONS = [['sec-general', 'General'], ['sec-db', 'Databases'], ['sec-schedule', 'Schedule'], ['sec-compress', 'Compression'],
    ['sec-storage', 'Storage'], ['sec-retention', 'Retention'], ['sec-advanced', 'Advanced'], ['sec-binlog', 'Binlog & PITR'], ['sec-notify', 'Notifications']];
  const tgt = (id) => job.targets.find((t) => t.storage_id === id);
  const o = job.options;
  const r = job.retention;
  const num = (name, value, attrs = '') => html`<input type="number" name="${name}" value="${value}" ${attrs}>`;
  const chk = (name, checked, label, help = '') => html`<label class="check"><input type="checkbox" name="${name}" ${checked ? 'checked' : ''}><span>${label}${help ? html`<span class="help">${help}</span>` : ''}</span></label>`;

  setHTML(el, html`
    <div class="page-head">${backBtn('#/jobs')}<div><h1>${titleText}</h1></div></div>
    <nav class="jump" aria-label="Form sections">${SECTIONS.map(([id, l]) => html`<button type="button" class="jump-link" data-jump="${id}">${l}</button>`)}</nav>
    ${!servers.length || !storages.length ? html`<div class="alert warn" style="margin-bottom:16px">${icon('alert')}<div>You need at least one <a href="#/servers">database server</a> and one <a href="#/storages">storage</a> before creating a job.</div></div>` : ''}
    <form id="job-form" autocomplete="off">
      <div class="card" id="sec-general"><div class="card-head"><h2>1. General</h2></div><div class="card-body">
        <div class="form-grid">
          <div class="field"><label class="req">Job name</label><input type="text" name="name" value="${job.name}" required placeholder="e.g. shop-prod nightly"></div>
          <div class="field"><label class="req">Database server</label>
            <div class="inline"><select name="server_id">${servers.map((s) => html`<option value="${s.id}" ${s.id === job.server_id ? 'selected' : ''}>${s.name} (${s.host || s.socket}${s.agent_id ? ' via agent' : s.wg_peer_id ? ' via WireGuard' : ''}${s.version ? ' · ' + s.version : ''})</option>`)}</select>
            <button type="button" class="btn" id="load-info" title="Check server">${icon('refresh')}</button></div>
            <div class="help" id="server-msg"></div></div>
        </div>
        <div class="section-title" style="margin-top:18px">Backup method</div>
        <div class="radio-cards" id="methods">${METHODS.map((m) => html`<label class="radio-card ${job.method === m.id ? 'selected' : ''}">
          <input type="radio" name="method" value="${m.id}" ${job.method === m.id ? 'checked' : ''}>
          <div class="rc-title">${icon(m.icon)}${m.title}</div><div class="rc-desc">${m.desc}</div><div class="rc-meta" data-support="${m.id}"></div></label>`)}</div>
      </div></div>

      <div class="card" id="sec-db"><div class="card-head"><h2>2. Databases to back up</h2></div><div class="card-body">
        <div class="btn-group" style="margin-bottom:12px">
          ${[['all', 'All databases'], ['include', 'Only selected databases'], ['exclude', 'All except selected databases']].map(([v, l]) => html`<label class="check" style="margin-right:16px"><input type="radio" name="db_mode" value="${v}" ${job.db_mode === v ? 'checked' : ''}><span>${l}</span></label>`)}
        </div>
        <div class="help" style="margin-bottom:10px">"All" excludes system schemas (mysql, sys, information_schema, performance_schema). Users &amp; grants are backed up separately (option below).</div>
        <div id="db-list" class="db-pick hidden"></div>
        <div class="field" style="margin-top:14px"><label>Exclude tables (one per line, as <code>database.table</code>)</label>
          <textarea name="exclude_tables" rows="3" placeholder="shop.audit_log&#10;shop.sessions">${job.exclude_tables.join('\n')}</textarea>
          <div class="help">Useful for skipping very large log/cache tables that don't need backing up.</div></div>
      </div></div>

      <div class="card" id="sec-schedule"><div class="card-head"><h2>3. Schedule</h2></div><div class="card-body">
        <div class="form-grid">
          <div class="field"><label>${job.method === 'physical' ? 'FULL backup' : 'Backup'} schedule (cron)</label>
            <div class="inline"><input type="text" name="schedule" value="${job.schedule}" placeholder="minute hour day month weekday — e.g. 0 2 * * *">
            <select id="preset" style="max-width:190px">${PRESETS.map(([v, l]) => html`<option value="${v}">${l}</option>`)}</select></div>
            <div class="help" id="cron-desc"></div><div class="help" id="cron-next"></div></div>
          <div class="field" id="incr-field"><label>Incremental schedule (physical backups only)</label>
            <div class="inline"><input type="text" name="incr_schedule" value="${job.incr_schedule}" placeholder="leave blank if not used">
            <select id="incr-preset" style="max-width:190px">${INCR_PRESETS.map(([v, l]) => html`<option value="${v}">${l}</option>`)}</select></div>
            <div class="help" id="incr-desc">Incrementals copy only the data pages changed since the previous backup — fast and small. Restore = full + incremental chain.</div><div class="help" id="incr-next"></div></div>
        </div>
        <div class="help" style="margin-top:8px">5-field cron format: <code>minute hour day-of-month month weekday</code> (0=Sun). <code>@daily</code> and <code>@every 6h</code> are supported. Times use the time zone from Settings.</div>
      </div></div>

      <div class="card" id="sec-compress"><div class="card-head"><h2>4. Compression &amp; encryption</h2></div><div class="card-body">
        <div class="form-grid">
          <div class="field"><label>Compression</label><select name="compression">
            <option value="zstd" ${job.compression === 'zstd' ? 'selected' : ''}>zstd (recommended — fast, good ratio)</option>
            <option value="gzip" ${job.compression === 'gzip' ? 'selected' : ''}>gzip (multi-threaded, widely compatible)</option>
            <option value="none" ${job.compression === 'none' ? 'selected' : ''}>No compression</option></select></div>
          <div class="field"><label>Compression level</label><select name="compression_level">
            ${[[0, 'Default (balanced)'], [1, 'Fastest'], [3, 'Better compression'], [4, 'Maximum (slow)']].map(([v, l]) => html`<option value="${v}" ${job.compression_level === v ? 'selected' : ''}>${l}</option>`)}</select></div>
          <div class="field"><label>Threads</label>${num('threads', job.threads, 'min="1" max="64"')}<div class="help" id="threads-help"></div></div>
          <div class="field"><label>Timeout (minutes, 0 = no limit)</label>${num('timeout_minutes', job.timeout_minutes, 'min="0"')}</div>
          <div class="field full">${chk('encrypt', job.encrypt, 'Encrypt backups (age / ChaCha20-Poly1305, key derived from a passphrase)', 'Encrypted before leaving the server: not even the cloud provider can read the data.')}</div>
          <div class="field full" id="pass-field"><label>Encryption passphrase ${hasPass ? html`<span class="badge good">${icon('check')}set</span>` : ''}</label>
            <input type="password" name="passphrase" placeholder="${hasPass ? 'leave blank to keep the current passphrase' : 'at least 8 characters'}" autocomplete="new-password">
            <div class="alert warn" style="margin-top:8px">${icon('alert')}<div>Keep a copy of the passphrase somewhere safe (e.g. a password manager). Lose the passphrase = <b>no restore</b>. Each backup remembers the passphrase it was created with; changing it only affects new backups. Manual decryption: <code>dumpcat decode -in file.sql.zst.age</code> or <code>age -d</code>.</div></div></div>
        </div>
      </div></div>

      <div class="card" id="sec-storage"><div class="card-head"><h2>5. Storage</h2></div><div class="card-body">
        ${storages.length ? html`<div class="help" style="margin-bottom:10px">Pick one or more (3-2-1 recommended: a local copy for fast restores + a cloud/offsite copy). If a <b>local</b> storage is selected, the backup is written there first and then uploaded to the others.</div>
        <div class="table-wrap"><table class="table"><thead><tr><th>Storage</th><th>Retention here (overrides the job policy)</th></tr></thead><tbody>
        ${storages.map((s) => { const t = tgt(s.id); const rr = (t && t.retention) || {}; return html`<tr>
          <td><label class="check"><input type="checkbox" name="target" value="${s.id}" ${t ? 'checked' : ''}><span><b>${s.name}</b><span class="help">${s.type_label} · ${s.describe}</span></span></label></td>
          <td><div class="inline" style="gap:6px;max-width:420px"><input type="number" min="0" data-tret="${s.id}" data-k="keep_last" value="${rr.keep_last || ''}" placeholder="last N backups" aria-label="Keep the last N backups on ${s.name}" ${t ? '' : 'disabled'}><input type="number" min="0" data-tret="${s.id}" data-k="keep_days" value="${rr.keep_days || ''}" placeholder="N days" aria-label="Keep N days on ${s.name}" ${t ? '' : 'disabled'}></div></td></tr>`; })}
        </tbody></table></div>` : html`<div class="alert warn">${icon('alert')}<div>No storage configured yet. <a href="#/storages">Add storage</a></div></div>`}
        <div style="margin-top:12px">${chk('stream_upload', o.stream_upload, 'Stream directly to remote storage (no temporary disk writes)', 'Use when the server is short on disk space. Only applies when no local storage is selected. Note: slow uploads keep the snapshot open longer.')}</div>
        <div class="field" style="margin-top:12px;max-width:320px"><label>Upload bandwidth limit</label><input type="text" name="bwlimit" value="${o.bwlimit}" placeholder="e.g. 50M (50 MiB/s), blank = unlimited"></div>
      </div></div>

      <div class="card" id="sec-retention"><div class="card-head"><h2>6. Retention policy</h2></div><div class="card-body">
        <div class="help" style="margin-bottom:12px">A backup is kept if it matches <b>any</b> rule. All 0 → keep forever. The newest backup is always kept; incrementals follow their base full backup.</div>
        <div class="grid grid-5">
          <div class="field"><label>Keep last N backups</label>${num('keep_last', r.keep_last, 'min="0"')}</div>
          <div class="field"><label>Keep for N days</label>${num('keep_days', r.keep_days, 'min="0"')}</div>
          <div class="field"><label>Last backup of each day</label>${num('keep_daily', r.keep_daily, 'min="0"')}</div>
          <div class="field"><label>Last backup of each week</label>${num('keep_weekly', r.keep_weekly, 'min="0"')}</div>
          <div class="field"><label>Last backup of each month</label>${num('keep_monthly', r.keep_monthly, 'min="0"')}</div>
        </div>
        <div class="help" id="ret-summary" style="margin-top:10px" aria-live="polite"></div>
      </div></div>

      <details class="card adv-card" id="sec-advanced"><summary class="card-head"><h2>7. Advanced options</h2><span class="muted small">objects, tool options, priority, hooks</span></summary><div class="card-body">
        <div data-for="mysqldump mydumper">
          <div class="section-title">Objects</div>
          <div class="checks">
            ${chk('routines', o.routines, 'Stored procedures & functions')}
            ${chk('triggers', o.triggers, 'Triggers')}
            ${chk('events', o.events, 'Events')}
            ${chk('include_users', o.include_users, 'Users & grants (GRANT)', 'Saved as a separate _users.sql file; restoring it is optional.')}
          </div>
        </div>
        <div data-for="mysqldump" style="margin-top:14px">
          <div class="section-title">mysqldump</div>
          <div class="checks">
            ${chk('single_file', o.single_file, 'One file for all databases (consistent snapshot across databases)', 'Default is one file per database: restore databases individually, rename them, dump in parallel.')}
            ${chk('record_binlog', o.record_binlog, 'Record binlog/GTID position (--source-data=2)', 'MySQL: needs a very brief FLUSH TABLES WITH READ LOCK at the start. MariaDB: no locking.')}
          </div>
          <div class="field" style="max-width:260px;margin-top:10px"><label>--set-gtid-purged (MySQL)</label><select name="gtid_purged">${['OFF', 'AUTO', 'ON'].map((v) => html`<option ${o.gtid_purged === v ? 'selected' : ''}>${v}</option>`)}</select></div>
        </div>
        <div data-for="mydumper" style="margin-top:14px">
          <div class="section-title">mydumper</div>
          <div class="form-grid">
            <div class="field"><label>Split tables into chunks of N rows (--rows)</label>${num('chunk_rows', o.chunk_rows, 'min="0"')}<div class="help">Allows parallel dump/restore within a single large table.</div></div>
            <div class="field"><label>Long query guard (seconds)</label>${num('long_query_guard', o.long_query_guard, 'min="0"')}<div class="help">Abort if a query runs longer than this, so the global lock never hangs.</div></div>
            <div class="field full"><label>Snapshot sync between threads (--sync-thread-lock-mode)</label><select name="lock_mode">
              ${[['', 'AUTO — pick the best mode (FTWRL/backup lock held for milliseconds, combined with --trx-tables)'], ['SAFE_NO_LOCK', 'SAFE_NO_LOCK — no locking; stops if threads get inconsistent snapshots'], ['GTID', 'GTID — no locking (Percona Server)'], ['NO_LOCK', 'NO_LOCK — no locking, consistency not guaranteed'], ['FTWRL', 'FTWRL'], ['LOCK_ALL', 'LOCK_ALL']].map(([v, l]) => html`<option value="${v}" ${o.lock_mode === v ? 'selected' : ''}>${l}</option>`)}</select>
              <div class="help">Only applies to mydumper ≥ 0.15. By default the lock is held only for the instant the snapshot transactions start; the dump itself runs lock-free (InnoDB).</div></div>
          </div>
        </div>
        <div data-for="physical" style="margin-top:14px">
          <div class="section-title">Physical backup</div>
          <div class="form-grid">
            <div class="field"><label>Max wait for long queries (seconds, --ftwrl-wait-timeout)</label>${num('ftwrl_wait_timeout', o.ftwrl_wait_timeout, 'min="0"')}<div class="help">During the final brief lock, wait for long queries to finish instead of blocking writes. 0 = off.</div></div>
            <div class="field"><div class="checks" style="grid-template-columns:1fr">
              ${chk('slave_info', o.slave_info, 'Record replication info (--slave-info)', 'Use when backing up from a replica to build a new replica.')}
              ${chk('safe_slave_backup', o.safe_slave_backup, 'Pause the SQL thread on the replica (--safe-slave-backup)')}
            </div></div>
          </div>
        </div>
        <div style="margin-top:16px" class="section-title">General</div>
        <div class="checks">
          ${chk('low_priority', o.low_priority, 'Low priority (nice + ionice)', 'Reduces the impact on the running database.')}
          ${chk('verify_after', o.verify_after, 'Verify integrity after backup', 'Re-reads the files, decrypts/decompresses them and checks SHA-256.')}
        </div>
        <div class="form-grid" style="margin-top:14px">
          <div class="field full"><label>Extra arguments for the dump tool ${!isAdmin() ? html`<span class="muted">— admin only</span>` : ''}</label><input type="text" name="extra_args" value="${o.extra_args}" ${!isAdmin() ? 'disabled' : ''} placeholder="e.g. --skip-definer"><div class="help">Advanced — appended verbatim to the mysqldump/mydumper/xtrabackup command.</div></div>
          <div class="field"><label>Pre-command ${!isAdmin() ? html`<span class="muted">— admin only</span>` : ''}</label><input type="text" name="pre_command" value="${o.pre_command}" ${!isAdmin() ? 'disabled' : ''} placeholder="/usr/local/bin/before-backup.sh"></div>
          <div class="field"><label>Post-command</label><input type="text" name="post_command" value="${o.post_command}" ${!isAdmin() ? 'disabled' : ''} placeholder="receives DUMPCAT_STATUS, DUMPCAT_DIR..."></div>
        </div>
      </div></details>

      <div class="card" id="sec-binlog"><div class="card-head"><h2>8. Binlog &amp; point-in-time recovery</h2></div><div class="card-body">
        ${chk('binlog_archive', o.binlog_archive, 'Continuous binlog archiving', 'DumpCat streams the server\'s binary logs (mysqlbinlog --raw --stop-never, as a replication client) and stores every closed binlog — compressed and encrypted like the backups — on this job\'s storages. A restore can then stop at any moment: a time, a binlog position or just before a GTID. Retention removes binlogs older than the oldest kept backup.')}
        <div id="binlog-opts" class="${o.binlog_archive ? '' : 'hidden'}" style="margin-top:12px">
          <div class="form-grid">
            <div class="field"><label>Close &amp; upload the current binlog after (minutes)</label>${num('binlog_rotate_minutes', o.binlog_rotate_minutes, 'min="0" max="1440"')}
              <div class="help">Bounds how much could be lost if this server disappears: once the current binlog holds transactions this long, DumpCat runs FLUSH BINARY LOGS (needs RELOAD) so it is uploaded. 0 = only when the server rotates it (max_binlog_size). Changes not uploaded yet are also kept in the DumpCat staging folder.</div></div>
            <div class="field"><div class="alert info">${icon('info')}<div>Requirements: <code>log_bin</code> and <code>server_id</code> on the server; the backup user needs <code>REPLICATION SLAVE</code> and <code>REPLICATION CLIENT</code> (MariaDB: <code>BINLOG MONITOR</code>); <code>mysqlbinlog</code>/<code>mariadb-binlog</code> on the DumpCat host. mysqldump jobs then always record the binlog position. Binlogs contain changes to <b>every</b> database of the server.</div></div></div>
          </div>
        </div>
      </div></div>

      <div class="card" id="sec-notify"><div class="card-head"><h2>9. Notifications &amp; status</h2></div><div class="card-body">
        <div class="checks">
          ${chk('notify_failure', job.notify_failure, 'Notify on failure')}
          ${chk('notify_success', job.notify_success, 'Notify on success')}
          ${chk('enabled', job.enabled, 'Enable scheduled runs')}
        </div>
        <div class="help" style="margin-top:8px">Notification channels (Email, Telegram, Slack...) are configured on the <a href="#/notifications">Notifications</a> page.</div>
      </div></div>

      <div class="sticky-actions">
        <span class="dirty-note" id="dirty-note" aria-live="polite"></span>
        <a class="btn" href="#/jobs">Cancel</a>
        <button class="btn" type="button" id="save-run">${icon('play')}Save &amp; run now</button>
        <button class="btn btn-primary" type="submit">${icon('check')}Save job</button>
      </div>
    </form>`);

  const form = $('#job-form', el);
  const method = () => form.querySelector('input[name=method]:checked').value;
  const serverId = () => +form.server_id.value;

  const updateVisibility = () => {
    const m = method();
    $$('.radio-card', el).forEach((c) => c.classList.toggle('selected', c.querySelector('input').checked));
    $('#sec-db', el).classList.toggle('hidden', m === 'physical');
    $('[data-jump=sec-db]', el).classList.toggle('hidden', m === 'physical');
    $('#incr-field', el).classList.toggle('hidden', m !== 'physical');
    $$('[data-for]', el).forEach((d) => d.classList.toggle('hidden', !d.dataset.for.split(' ').includes(m)));
    const mode = form.querySelector('input[name=db_mode]:checked').value;
    $('#db-list', el).classList.toggle('hidden', mode === 'all');
    $('#pass-field', el).classList.toggle('hidden', !form.encrypt.checked);
    $('#binlog-opts', el).classList.toggle('hidden', !form.binlog_archive.checked);
    if (form.binlog_archive.checked && m === 'mysqldump') form.record_binlog.checked = true;
    $('#threads-help', el).textContent = m === 'mysqldump' ? 'mysqldump: number of databases dumped in parallel' : m === 'mydumper' ? 'Number of mydumper dump threads' : 'Number of files copied in parallel (--parallel)';
  };

  const renderDBs = (info) => {
    const box = $('#db-list', el);
    const dbs = info ? info.databases.filter((d) => !d.system) : [];
    const known = new Set(dbs.map((d) => d.name));
    const extra = job.databases.filter((d) => !known.has(d));
    const sel = new Set(job.databases);
    setHTML(box, html`${!info ? html`<div class="muted small">Loading databases…</div>` : ''}
      ${dbs.map((d) => html`<label class="check"><input type="checkbox" name="db" value="${d.name}" ${sel.has(d.name) ? 'checked' : ''}><span><b>${d.name}</b> <span class="muted small">${fmtBytes(d.size_bytes)} · ${d.tables} tables${d.non_innodb ? html` · <span style="color:var(--warning-ink)">${d.non_innodb} non-InnoDB tables</span>` : ''}</span></span></label>`)}
      ${extra.map((n) => html`<label class="check"><input type="checkbox" name="db" value="${n}" checked><span><b>${n}</b> <span class="muted small">(not found on server)</span></span></label>`)}
      ${info && !dbs.length ? html`<div class="muted">The server has no user databases.</div>` : ''}`);
  };

  const loadInfo = async (force) => {
    const id = serverId();
    const msg = $('#server-msg', el);
    if (!id) return;
    let res = infoCache[id];
    if (!res || force) {
      msg.textContent = 'Checking connection…';
      try { res = await get(`/servers/${id}/info`); } catch (e) { res = { ok: false, error: e.message }; }
      infoCache[id] = res;
    }
    if (serverId() !== id) return;
    if (!res.ok) {
      setHTML(msg, html`<span style="color:var(--critical-ink)">${icon('alert')} ${res.error}</span>`);
      renderDBs({ databases: [] });
      return;
    }
    const i = res.info;
    setHTML(msg, html`${icon('check')} ${i.version} · ${fmtBytes(i.total_bytes)} · binlog ${i.log_bin ? 'on' : 'off'}${i.is_replica ? ' · replica' : ''}`);
    for (const s of res.support) {
      const box = el.querySelector(`[data-support="${s.method}"]`);
      if (box) setHTML(box, html`<span class="badge ${s.ok ? 'good' : 'warn'}">${icon(s.ok ? 'check' : 'alert')}${s.ok ? 'Ready' : 'Not ready'}</span> <span class="muted">${s.message}</span>`);
    }
    renderDBs(i);
  };

  const cronPreview = async (input, descEl, nextEl) => {
    const v = input.value.trim();
    descEl.textContent = describeCron(v);
    if (!v) { nextEl.textContent = ''; return; }
    try {
      const r = await post('/cron/preview', { expr: v });
      if (!r.valid) setHTML(nextEl, html`<span style="color:var(--critical-ink)">${r.error}</span>`);
      else nextEl.textContent = 'Next runs: ' + r.next.slice(0, 3).map((t) => fmtDate(t)).join(' · ');
    } catch { /* ignore */ }
  };
  const pv1 = debounce(() => cronPreview(form.schedule, $('#cron-desc', el), $('#cron-next', el)), 300);
  const pv2 = debounce(() => cronPreview(form.incr_schedule, $('#incr-desc', el), $('#incr-next', el)), 300);
  form.schedule.addEventListener('input', pv1);
  form.incr_schedule.addEventListener('input', pv2);
  $('#preset', el).addEventListener('change', (e) => { if (e.target.value) { form.schedule.value = e.target.value === '@manual' ? '' : e.target.value; pv1(); e.target.value = ''; } });
  $('#incr-preset', el).addEventListener('change', (e) => { if (e.target.value) { form.incr_schedule.value = e.target.value === '@manual' ? '' : e.target.value; pv2(); e.target.value = ''; } });
  pv1();
  if (job.incr_schedule) pv2();

  form.addEventListener('change', (e) => {
    if (e.target.name === 'server_id') loadInfo(false);
    if (e.target.name === 'binlog_archive' && e.target.checked && !+form.binlog_rotate_minutes.value) form.binlog_rotate_minutes.value = 15;
    if (e.target.name === 'db') {
      job.databases = $$('input[name=db]:checked', el).map((x) => x.value);
    }
    updateVisibility();
  });
  $('#load-info', el).onclick = () => loadInfo(true);
  on(el, 'click', '[data-jump]', (e, b) => {
    const sec = el.querySelector('#' + b.dataset.jump);
    if (!sec) return;
    if (sec.tagName === 'DETAILS') sec.open = true;
    sec.scrollIntoView({ behavior: 'smooth', block: 'start' });
    sec.querySelector('input:not([type=hidden]):not([disabled]), select, textarea, summary')?.focus({ preventScroll: true });
  });
  // unsaved-changes guard
  let dirty = false;
  const markDirty = () => { dirty = true; $('#dirty-note', el).textContent = 'Unsaved changes'; };
  form.addEventListener('input', markDirty);
  form.addEventListener('change', (e) => { if (e.isTrusted) markDirty(); });
  setLeaveGuard(() => (dirty ? 'This job has unsaved changes. Leave without saving?' : null));
  // per-storage overrides only apply to selected storages
  on(el, 'change', 'input[name=target]', (e, c) => {
    $$(`[data-tret="${c.value}"]`, el).forEach((i) => { i.disabled = !c.checked; });
  });
  const retSummary = () => {
    const n = (k) => +form[k].value || 0;
    const parts = [];
    if (n('keep_last')) parts.push(`the last ${n('keep_last')} backup${n('keep_last') === 1 ? '' : 's'}`);
    if (n('keep_days')) parts.push(`everything from the last ${n('keep_days')} day${n('keep_days') === 1 ? '' : 's'}`);
    if (n('keep_daily')) parts.push(`one per day for ${n('keep_daily')} day${n('keep_daily') === 1 ? '' : 's'}`);
    if (n('keep_weekly')) parts.push(`one per week for ${n('keep_weekly')} week${n('keep_weekly') === 1 ? '' : 's'}`);
    if (n('keep_monthly')) parts.push(`one per month for ${n('keep_monthly')} month${n('keep_monthly') === 1 ? '' : 's'}`);
    $('#ret-summary', el).textContent = parts.length ? 'Keeps ' + parts.join(', ') + '. Older backups are deleted after each successful run.' : 'Keeps every backup forever (no automatic deletion).';
  };
  ['keep_last', 'keep_days', 'keep_daily', 'keep_weekly', 'keep_monthly'].forEach((k) => form[k].addEventListener('input', retSummary));
  retSummary();
  updateVisibility();
  renderDBs(null);
  loadInfo(false);

  const collect = () => {
    const f = form;
    const targets = $$('input[name=target]:checked', el).map((c) => {
      const id = +c.value;
      const ret = {};
      $$(`[data-tret="${id}"]`, el).forEach((i) => { if (i.value !== '') ret[i.dataset.k] = +i.value; });
      return { storage_id: id, retention: Object.keys(ret).length ? { keep_last: 0, keep_days: 0, keep_daily: 0, keep_weekly: 0, keep_monthly: 0, ...ret } : null };
    });
    return {
      name: f.name.value.trim(), server_id: serverId(), method: method(),
      db_mode: f.querySelector('input[name=db_mode]:checked').value,
      databases: $$('input[name=db]:checked', el).map((x) => x.value),
      exclude_tables: f.exclude_tables.value.split('\n').map((s) => s.trim()).filter(Boolean),
      schedule: f.schedule.value.trim(), incr_schedule: f.incr_schedule.value.trim(),
      compression: f.compression.value, compression_level: +f.compression_level.value,
      encrypt: f.encrypt.checked, passphrase: f.passphrase.value, threads: +f.threads.value || 1,
      options: {
        single_file: f.single_file.checked, routines: f.routines.checked, triggers: f.triggers.checked, events: f.events.checked,
        include_users: f.include_users.checked, record_binlog: f.record_binlog.checked, gtid_purged: f.gtid_purged.value,
        chunk_rows: +f.chunk_rows.value, long_query_guard: +f.long_query_guard.value, lock_mode: f.lock_mode.value, ftwrl_wait_timeout: +f.ftwrl_wait_timeout.value,
        slave_info: f.slave_info.checked, safe_slave_backup: f.safe_slave_backup.checked, low_priority: f.low_priority.checked,
        stream_upload: f.stream_upload.checked, verify_after: f.verify_after.checked, bwlimit: f.bwlimit.value.trim(),
        extra_args: f.extra_args.value.trim(), pre_command: f.pre_command.value.trim(), post_command: f.post_command.value.trim(),
        binlog_archive: f.binlog_archive.checked, binlog_rotate_minutes: +f.binlog_rotate_minutes.value,
      },
      retention: { keep_last: +f.keep_last.value, keep_days: +f.keep_days.value, keep_daily: +f.keep_daily.value, keep_weekly: +f.keep_weekly.value, keep_monthly: +f.keep_monthly.value },
      targets, enabled: f.enabled.checked, notify_success: f.notify_success.checked, notify_failure: f.notify_failure.checked,
      timeout_minutes: +f.timeout_minutes.value,
    };
  };

  const save = async (andRun, btn) => {
    formError(form, null);
    if (!validate(form)) return;
    const data = collect();
    await busy(btn, async () => {
      try {
        const res = editing ? await put(`/jobs/${editing}`, data) : await post('/jobs', data);
        dirty = false;
        toast(editing ? 'Job saved' : 'Job created');
        if (andRun) {
          const run = await post(`/jobs/${res.job.id}/run`, { type: 'full' });
          navigate(`/runs/${run.id}`);
        } else navigate('/jobs');
      } catch (e) {
        formError(form, e.message);
      }
    });
  };
  form.addEventListener('submit', (e) => { e.preventDefault(); save(false, form.querySelector('button[type=submit]')); });
  $('#save-run', el).onclick = (e) => save(true, e.currentTarget);
  void on;
}
