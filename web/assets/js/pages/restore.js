import { get, post } from '../api.js';
import { html, setHTML, icon, on, $, $$, fmtBytes, fmtDate, toast, busy, confirmBox, debounce } from '../ui.js';
import { navigate, isAdmin, canOperate, setTitle, app } from '../app.js';
import { methodBadge, backBtn, pitrTargetFields, bindPITRFields, readPITRTarget, pitrPlanView, engines, methodOf, helpText, productName, logInfo } from '../components.js';
import { openServerForm } from './servers.js';

// replacing the datadir (stop service, copy-back or swap, start) is Linux-only
const canApply = () => isAdmin() && app.platform !== 'windows';

const flavorName = productName;

// short names of the compatibility options, for the confirmation
const SHORT = { strip_definer: 'remove DEFINER', convert_collations: 'convert collations', skip_gtid_purged: 'skip GTID_PURGED', no_owner: 'skip ownership', no_privileges: 'skip privileges' };

export const title = 'Restore';

export async function render(ctx) {
  const { el, params, query } = ctx;
  if (!canOperate()) { setHTML(el, html`<div class="alert warn">${icon('lock')}<div>You don't have permission to restore.</div></div>`); return; }
  const id = +params.id;
  const [{ run: r }, srvList, engs] = await Promise.all([get(`/runs/${id}`), get('/servers'), engines()]);
  // the backup's engine: its targets, options and features
  const { engine: eng, method: mi } = methodOf(engs, r.method) || { engine: engs[0], method: {} };
  const ofEngine = (list) => list.filter((s) => (s.engine || 'mysql') === eng.engine);
  let servers = ofEngine(srvList);
  const physical = !!mi.physical;
  const pg = eng.engine === 'postgres';
  const L = logInfo(eng.log);
  const single = r.meta && r.meta.single_file === true;
  const hasUsers = r.files.some((f) => f.kind === 'users');
  const okLocs = r.locations.filter((l) => l.status === 'ok');
  const needPass = r.encrypted && !r.has_passphrase;
  const srcServer = servers.find((s) => s.id === r.server_id);
  // where replaying the archived log starts: a binlog position, the end of a pg_basebackup's WAL
  const hasPos = pg ? !!(r.meta && r.meta.end_lsn) : !!(r.meta && r.meta.binlog_file) || r.databases.some((d) => d.binlog_file);
  // MySQL replays binlogs into the server it applied a physical backup to; PostgreSQL recovers the directory itself
  const canPITR = hasPos && (!physical || pg || isAdmin());
  // the driver's restore options for this backup: "options" and "compat" (ticked from the target check)
  const optList = (group) => eng.restore_options.filter((o) => o.group === group && (!o.methods || o.methods.includes(r.method)) && (o.needs !== 'users' || hasUsers));
  const PORTABLE = optList('compat').map((o) => o.key);
  const optBox = (o) => html`<label class="check"><input type="checkbox" name="${o.key}" ${o.default ? 'checked' : ''}><span>${o.key === 'drop_existing' ? html`<b>${o.label}</b>` : o.label}<span class="help">${helpText(o.help)}</span></span></label>`;
  setTitle([{ label: 'Backups', href: '#/runs' }, { label: `${r.job_name} #${r.id}`, href: `#/runs/${r.id}` }, 'Restore']);
  const srcVersion = r.meta && r.meta.server_version ? flavorName(r.meta.flavor, r.meta.server_version) : '';
  const serverOptions = (blank) => html`${blank ? html`<option value="0">—</option>` : ''}${servers.map((s) => html`<option value="${s.id}" ${s.id === r.server_id ? 'selected' : ''}>${s.name} (${s.host || s.socket})${s.id === r.server_id ? ' — source server' : ''}</option>`)}`;
  const newServerBtn = html`<button type="button" class="btn" id="add-server" title="Add another ${eng.product} instance as a restore target">${icon('plus')}New server</button>`;
  let pitrPre = null;
  try { pitrPre = query.pitr ? JSON.parse(query.pitr) : null; } catch { pitrPre = null; }
  // restore points of the job's archived WAL, suggested as targets
  let points = [];
  if (mi.log_archive && L.targets.includes('name') && r.job_id) {
    try { points = [...new Set((await get(`/jobs/${r.job_id}/binlogs?limit=1000`)).items.flatMap((f) => ((f.meta && f.meta.points) || []).map((x) => x.name)))]; } catch { points = []; }
  }

  setHTML(el, html`
    <div class="page-head">${backBtn(`#/runs/${r.id}`)}<div><h1>Restore backup</h1><div class="sub">${r.job_name} · ${r.dir_name} · ${fmtDate(r.started_at)}</div></div></div>
    <form id="rs-form" autocomplete="off">
    <div class="card"><div class="card-head"><h2>Source</h2>${methodBadge(r.method, r.type)}</div><div class="card-body">
      <div class="form-grid">
        <div class="field"><label>Read data from</label><select name="location_id">
          <option value="0">Automatic (prefer local storage)</option>
          ${okLocs.map((l) => html`<option value="${l.id}">${l.storage_name} (${l.storage_type})</option>`)}</select>
          <div class="help">Data is streamed directly: download → decrypt → decompress → load into the database, with no temporary files (except mydumper, pg_dump and physical backups, which are extracted to a directory first).</div></div>
        <div class="field"><label>Size</label><div style="padding-top:8px">${fmtBytes(r.size_bytes)} stored · ${fmtBytes(r.raw_bytes)} raw data</div></div>
        ${needPass ? html`<div class="field full"><label class="req">Decryption passphrase</label><input type="password" name="passphrase" required autocomplete="off"><div class="help">This backup was imported from storage, so its passphrase isn't stored in DumpCat.</div></div>` : ''}
      </div>
    </div></div>

    ${physical && pg ? html`
    <div class="card"><div class="card-head"><h2>Physical restore</h2></div><div class="card-body">
      <div class="alert info" style="margin-bottom:14px">${icon('info')}<div>Steps: download &amp; extract the cluster to a directory → recover it with a temporary PostgreSQL ${r.meta && r.meta.pg_major ? r.meta.pg_major : ''} on <b>the machine running DumpCat</b> (to the point in time chosen below, then it is promoted) → (optional) stop PostgreSQL here and swap the data directory. Without the PostgreSQL server of that version on this machine, the directory is left ready to recover: copy it to the database host and start PostgreSQL on it.</div></div>
      <div class="form-grid">
        <div class="field full"><label>Extraction directory ${isAdmin() ? '' : html`<span class="muted">— only admins can change this</span>`}</label><input type="text" name="target_dir" ${isAdmin() ? '' : 'disabled'} placeholder="default: <DumpCat data directory>/restore/${r.job_slug}-${r.dir_name}"><div class="help">Needs free space ≥ the size of the cluster (${fmtBytes(r.raw_bytes)}). The directory must be empty. When DumpCat runs as root, the recovery runs as the OS user below, who must be able to reach the directory (e.g. under <code>/var/tmp</code>).</div></div>
        <div class="field"><label>OS user running the recovery</label><input type="text" name="os_user" value="${srcServer?.os_user || 'postgres'}" ${isAdmin() ? '' : 'disabled'}><div class="help">PostgreSQL never runs as root (Linux only).</div></div>
      </div>
      <hr>
      ${canApply() ? html`
        <label class="check"><input type="checkbox" name="apply"><span><b>Replace PostgreSQL's data directory on this machine</b><span class="help">The backup is recovered next to it (<code>&lt;datadir&gt;.dumpcat-restore-&lt;time&gt;</code>), then: stop the service → rename the old directory to <code>*.before-restore-&lt;time&gt;</code> → move the recovered one in → start. Errors roll back to the old directory.</span></span></label>
        <div id="apply-box" class="form-grid hidden" style="margin-top:14px">
          <div class="field"><label>Instance on this machine (fills data directory, service, owner)</label><div style="display:flex;gap:8px"><select name="target_server_id" style="flex:1;min-width:0">${serverOptions(true)}</select>${newServerBtn}</div></div>
          <div class="field"><label class="req">Data directory</label><input type="text" name="datadir" value="${srcServer?.datadir || ''}" placeholder="/var/lib/postgresql/${r.meta && r.meta.pg_major ? r.meta.pg_major : '16'}/main"></div>
          <div class="field"><label>systemd service name</label><input type="text" name="service_name" value="${srcServer?.service_name || ''}" placeholder="auto-detect: postgresql@${r.meta && r.meta.pg_major ? r.meta.pg_major : '16'}-main / postgresql-${r.meta && r.meta.pg_major ? r.meta.pg_major : '16'}"></div>
          <div class="field full" id="target-info"></div>
          <div class="field full"><div class="alert bad">${icon('alert')}<div><b>PostgreSQL will be stopped</b> while the directories are swapped. Anything written after the recovery point will not be in the new data directory (the old one is kept for comparison).</div></div></div>
        </div>` : html`<div class="muted small">${app.platform === 'windows'
          ? 'On Windows, DumpCat prepares and recovers the data directory; start PostgreSQL on it yourself.'
          : 'Only admins can replace the data directory; the log says how to use the prepared one.'}</div>`}
    </div></div>` : physical ? html`
    <div class="card"><div class="card-head"><h2>Physical restore</h2></div><div class="card-body">
      <div class="alert info" style="margin-bottom:14px">${icon('info')}<div>Steps: download &amp; extract ${r.type === 'incremental' ? html`<b>the full backup + incremental chain</b>` : 'the full backup'} to a directory → <code>--prepare</code> (apply the redo log) → (optional) stop MySQL, replace the datadir, start it again. Runs on <b>the machine running DumpCat</b>. To restore onto another machine, install DumpCat there, add the same storage and use <i>Import backup catalog</i> — or restore a logical backup to any instance from here.${srcServer && srcServer.agent_id ? html` This backup was taken through an agent: DumpCat cannot replace that host's datadir. Prepare it here, then copy the prepared directory to the database host and run <code>--copy-back</code> there (the log shows the commands).` : ''}</div></div>
      <div class="form-grid">
        <div class="field full"><label>Extraction directory ${isAdmin() ? '' : html`<span class="muted">— only admins can change this</span>`}</label><input type="text" name="target_dir" ${isAdmin() ? '' : 'disabled'} placeholder="default: <DumpCat data directory>/restore/${r.job_slug}-${r.dir_name}"><div class="help">Needs free space ≥ the raw data size (${fmtBytes(r.raw_bytes)}). The directory must be empty.</div></div>
        <div class="field"><label>Threads</label><input type="number" name="threads" value="4" min="1" max="64"></div>
        <div class="field"><label>Memory for prepare (--use-memory)</label><input type="text" name="use_memory" value="1G"></div>
      </div>
      <hr>
      ${canApply() ? html`
        <label class="check"><input type="checkbox" name="apply"><span><b>Apply to MySQL/MariaDB on this machine right away</b><span class="help">Stop the service → rename the old datadir to <code>*.before-restore-&lt;timestamp&gt;</code> → copy-back → chown → start. Errors roll back to the old datadir automatically.</span></span></label>
        <div id="apply-box" class="form-grid hidden" style="margin-top:14px">
          <div class="field"><label>Instance on this machine (fills datadir, service, owner)</label><div style="display:flex;gap:8px"><select name="target_server_id" style="flex:1;min-width:0">${serverOptions(true)}</select>${newServerBtn}</div></div>
          <div class="field"><label class="req">Datadir</label><input type="text" name="datadir" value="${srcServer?.datadir || ''}" placeholder="/var/lib/mysql"></div>
          <div class="field"><label>systemd service name</label><input type="text" name="service_name" value="${srcServer?.service_name || ''}" placeholder="auto-detect: mariadb / mysql / mysqld"></div>
          <div class="field"><label>Datadir owner (OS user)</label><input type="text" name="os_user" value="${srcServer?.os_user || 'mysql'}"></div>
          <div class="field full" id="target-info"></div>
          <div class="field full"><label class="check"><input type="checkbox" name="move_back"><span>Use --move-back (move instead of copy — faster and doesn't need double the space, but the extraction directory is empty afterwards)</span></label></div>
          <div class="field full"><div class="alert bad">${icon('alert')}<div><b>MySQL will be stopped</b> during the restore. Anything written after the backup was taken will not be in the new datadir (the old datadir is kept for comparison).</div></div></div>
        </div>` : html`<div class="muted small">${app.platform === 'windows'
          ? 'On Windows, DumpCat only prepares the backup. Once prepare finishes, the log explains how to copy it into the datadir.'
          : 'Only admins can apply directly to the datadir. Once prepare finishes, the log explains the manual steps.'}</div>`}
    </div></div>` : html`
    <div class="card"><div class="card-head"><h2>Target</h2></div><div class="card-body">
      <div class="form-grid">
        <div class="field"><label class="req">Restore to server</label><div style="display:flex;gap:8px"><select name="target_server_id" style="flex:1;min-width:0">${serverOptions(false)}</select>${newServerBtn}</div>
          <div class="help">The source server or any other instance DumpCat can reach: another host, port or version${srcVersion ? html` (backup taken from ${srcVersion})` : ''}.</div></div>
        <div class="field"><label>${r.method === 'pg_dump' ? 'pg_restore jobs' : 'Databases restored in parallel'}</label><input type="number" name="threads" value="${r.method === 'mydumper' || r.method === 'pg_dump' ? 4 : 1}" min="1" max="32">
          <div class="help">${r.method === 'mydumper' ? 'Number of myloader threads (loads many tables/chunks in parallel).' : r.method === 'pg_dump' ? 'Tables and indexes restored in parallel (pg_restore --jobs), one database at a time.' : 'Each database uses its own mysql process.'}</div></div>
      </div>
      <div id="target-info" style="margin-top:12px"></div>
      <div class="section-title" style="margin-top:16px">Databases</div>
      ${single ? html`<div class="alert info" style="margin-bottom:10px">${icon('info')}<div>Single-file backup (consistent snapshot) — restored in full, keeping the original database names.</div></div>` : html`<div class="help" style="margin-bottom:8px">You can restore a subset and <b>rename</b> the target databases (e.g. restore into <code>shop_restore</code> to compare without touching the live database).
        <div class="btn-group" style="margin-top:8px"><button type="button" class="btn btn-sm" id="suffix">${icon('copy')}Restore under new names (_restore)</button> <button type="button" class="btn btn-sm btn-ghost" id="nosuffix">Keep original names</button></div></div>`}
      <div class="table-wrap"><table class="table"><thead><tr><th style="width:36px"><input type="checkbox" id="all-db" checked ${single ? 'disabled' : ''}></th><th>Source database</th><th class="right">Size</th><th>Restore as</th></tr></thead><tbody>
        ${r.databases.map((d) => html`<tr><td><input type="checkbox" name="db" value="${d.name}" checked ${single ? 'disabled' : ''}></td><td><b>${d.name}</b></td><td class="right num">${fmtBytes(d.size_bytes)}</td>
          <td><input type="text" data-rename="${d.name}" value="${d.name}" ${single ? 'disabled' : ''} aria-label="Restore ${d.name} as" style="max-width:320px"></td></tr>`)}
      </tbody></table></div>
      <div id="overwrite-warn" style="margin-top:12px"></div>
      <div class="section-title" style="margin-top:16px">Options</div>
      <div class="checks">${optList('options').map(optBox)}</div>
      ${PORTABLE.length ? html`<div class="section-title" style="margin-top:16px">Compatibility with the target</div>
      <div class="help" style="margin-bottom:8px">${eng.engine === 'mysql' ? 'For restores onto another instance or version. Schema statements are rewritten while streaming; row data is never changed.' : 'For restores onto another server, where the roles of the source may not exist.'} Ticked automatically from what DumpCat finds on the target.</div>
      <div class="checks">${optList('compat').map(optBox)}</div>` : ''}
    </div></div>`}
    <div class="card ${mi.log_archive ? '' : 'hidden'}"><div class="card-head"><h2>Point-in-time recovery</h2>${hasPos ? '' : html`<span class="badge">no ${pg ? 'WAL' : 'binlog'} position</span>`}</div><div class="card-body">
      <label class="check"><input type="checkbox" name="pitr" ${canPITR && pitrPre ? 'checked' : ''} ${canPITR ? '' : 'disabled'}><span><b>Replay the archived ${pg ? 'WAL' : 'binlogs'} after the restore</b><span class="help">${!hasPos
        ? (pg ? 'This backup does not record where its WAL ends, so it cannot be a starting point.' : 'This backup does not record its binlog position (mysqldump: enable "Record binlog position", or turn on binlog archiving for the job), so it cannot be a starting point.')
        : pg ? html`PostgreSQL recovers the restored cluster from the archived WAL up to the moment you choose — a time, a restore point or a WAL position — e.g. just before an accidental <code>DROP TABLE</code>, then it is promoted on a new timeline. Every WAL segment is downloaded and checked <b>before</b> anything is changed.`
        : html`Restores this backup, then applies the archived binary logs up to the moment you choose — e.g. just before an accidental DROP TABLE. All binlogs are downloaded and checked <b>before</b> anything is changed.${physical ? html` For a physical backup this needs <b>Apply right away</b> and a server (to connect to after it starts); the whole instance is replayed.${isAdmin() ? '' : ' Only admins can do this.'}` : ' Only the restored databases are replayed, each from its own snapshot position, under their new names.'}`}</span></span></label>
      <div id="pitr-box" class="${canPITR && pitrPre ? '' : 'hidden'}" style="margin-top:14px">
        ${pitrTargetFields(pitrPre || {}, eng.log, points)}
        ${physical ? '' : html`<div class="help" style="margin-top:6px">With <code>binlog_format</code> MIXED/STATEMENT, statements are filtered by their default database: statements naming other databases explicitly (<code>UPDATE other.t …</code>) are not renamed. Use ROW format for exact renamed restores.</div>`}
        <div id="pitr-plan" style="margin-top:12px"></div>
      </div>
    </div></div>
    <div class="sticky-actions"><a class="btn" href="#/runs/${r.id}">Cancel</a><button class="btn btn-primary" type="submit">${icon('restore')}Start restore</button></div>
    </form>`);

  const form = $('#rs-form', el);
  if (physical && canApply()) {
    form.apply.addEventListener('change', () => {
      $('#apply-box', el).classList.toggle('hidden', !form.apply.checked);
      if (form.apply.checked && !check) checkTarget();
    });
  }
  // The selected target as /restore-check sees it: version, flavor, databases already there.
  let check = null;
  const targetId = () => +form.target_server_id.value;
  const sameTarget = () => (check && check.ok ? check.same_server : targetId() === r.server_id);
  const targetView = (c) => {
    if (!c.ok) return html`<div class="alert bad">${icon('alert')}<div><b>Can't connect to the target:</b> ${c.error}<div class="help">Check its connection and restore account in Servers.</div></div></div>`;
    return html`<div class="alert ${c.same_server ? 'warn' : 'info'}">${icon(c.same_server ? 'alert' : 'database')}<div>
        <b>${c.same_server ? 'Source server' : 'Another instance'}</b>: ${flavorName(c.flavor, c.version)}${c.hostname ? html` on <code>${c.hostname}</code>` : ''} · ${c.existing.length} database${c.existing.length === 1 ? '' : 's'}${srcVersion ? html` <span class="muted">(backup from ${srcVersion})</span>` : ''}</div></div>
      ${c.warnings.map((w) => html`<div class="alert warn" style="margin-top:8px">${icon('alert')}<div>${w}</div></div>`)}`;
  };
  const checkTarget = async () => {
    const box = $('#target-info', el);
    const sid = targetId();
    check = null;
    if (!box) return;
    if (!sid) { setHTML(box, ''); updateWarn(); return; }
    setHTML(box, html`<div class="muted small">${icon('loader', 'spin')} Connecting to the target…</div>`);
    let res;
    try { res = await post(`/runs/${r.id}/restore-check`, { target_server_id: sid }); } catch (er) { res = { ok: false, error: er.message, existing: [], warnings: [] }; }
    if (!ctx.alive() || targetId() !== sid) return;
    check = res;
    setHTML(box, targetView(res));
    if (res.ok && !physical) PORTABLE.forEach((k) => { if (form[k]) form[k].checked = !!res.suggest[k]; });
    updateWarn();
  };
  // Databases the restore would overwrite on the target (after renaming).
  const overwrites = () => {
    if (physical) return [];
    const picked = single ? r.databases.map((d) => d.name) : $$('input[name=db]:checked', el).map((c) => c.value);
    const names = picked.map((n) => {
      const i = el.querySelector(`[data-rename="${CSS.escape(n)}"]`);
      return single || !i || !i.value.trim() ? n : i.value.trim();
    });
    if (check && check.ok) return names.filter((n) => check.existing.includes(n));
    // until the target has answered, assume the source server still has its databases
    return targetId() === r.server_id ? names.filter((n) => r.databases.some((d) => d.name === n)) : [];
  };
  const updateWarn = () => {
    if (physical) return;
    $$('input[name=db]', el).forEach((c) => {
      const i = el.querySelector(`[data-rename="${CSS.escape(c.value)}"]`);
      if (i && !single) i.disabled = !c.checked;
    });
    const live = overwrites();
    const srv = servers.find((x) => x.id === targetId());
    setHTML($('#overwrite-warn', el), live.length ? html`<div class="banner-danger" role="alert">${icon('alert')}<div>
        <b>This will overwrite ${live.length} ${sameTarget() ? 'live' : 'existing'} database${live.length === 1 ? '' : 's'} on ${srv ? srv.name : 'the target server'}:</b> <code>${live.join(', ')}</code>.
        Current data in ${live.length === 1 ? 'it' : 'them'} is replaced by the backup from ${fmtDate(r.started_at)}.
        ${single ? '' : html`<div style="margin-top:6px">To compare or recover single rows, <button type="button" class="btn btn-sm" data-suffix>restore under new names</button> instead.</div>`}</div></div>` : '');
  };
  if (!physical && !single) {
    const suffix = () => { $$('[data-rename]', el).forEach((i) => { i.value = i.dataset.rename + '_restore'; }); updateWarn(); };
    $('#suffix', el).onclick = suffix;
    on(el, 'click', '[data-suffix]', suffix);
    $('#nosuffix', el).onclick = () => { $$('[data-rename]', el).forEach((i) => { i.value = i.dataset.rename; }); updateWarn(); };
    $('#all-db', el).onchange = (e) => { $$('input[name=db]', el).forEach((c) => { c.checked = e.target.checked; }); updateWarn(); };
  }
  if (!physical) {
    form.addEventListener('input', updateWarn);
    form.addEventListener('change', updateWarn);
    updateWarn();
  }
  // target instance: inspect it; a physical restore also takes its datadir / service / owner
  if (form.target_server_id) {
    form.target_server_id.addEventListener('change', () => {
      const s = servers.find((x) => x.id === targetId());
      if (physical && s) {
        form.datadir.value = s.datadir || '';
        form.service_name.value = s.service_name || '';
        form.os_user.value = s.os_user || (pg ? 'postgres' : 'mysql');
      }
      checkTarget();
    });
    on(el, 'click', '#add-server', () => openServerForm(null, async (sv) => {
      servers = ofEngine(await get('/servers'));
      const sel = form.target_server_id;
      setHTML(sel, serverOptions(physical));
      sel.value = String(sv.id);
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    }, { engine: eng.engine }));
    if (!physical) checkTarget();
  }

  // point-in-time recovery: live check of the chosen target
  const pitrBox = $('#pitr-box', el);
  let plan = null;
  const pitrOn = () => !!(form.pitr && form.pitr.checked);
  const checkPlan = async () => {
    const box = $('#pitr-plan', el);
    if (!pitrOn()) return;
    const target = readPITRTarget(pitrBox);
    if (!target) { plan = null; setHTML(box, html`<div class="muted small">Enter where to stop.</div>`); return; }
    setHTML(box, html`<div class="muted small">${icon('loader', 'spin')} Checking the archived ${pg ? 'WAL' : 'binlogs'}…</div>`);
    try { plan = await post('/pitr/plan', { run_id: r.id, target }); } catch (er) { plan = { ok: false, target: target.type, error: er.message }; }
    if (ctx.alive()) setHTML(box, pitrPlanView(plan, r.id, target));
  };
  const recheck = debounce(checkPlan, 400);
  if (form.pitr) {
    bindPITRFields(pitrBox);
    form.pitr.addEventListener('change', () => {
      pitrBox.classList.toggle('hidden', !pitrOn());
      if (pitrOn() && physical && !pg && form.apply && !form.apply.checked) {
        form.apply.checked = true;
        $('#apply-box', el).classList.remove('hidden');
      }
      checkPlan();
    });
    pitrBox.addEventListener('input', recheck);
    pitrBox.addEventListener('change', recheck);
    on(el, 'click', '[data-use-base]', (e, b) => navigate(`/runs/${b.dataset.useBase}/restore?pitr=${encodeURIComponent(JSON.stringify(readPITRTarget(pitrBox)))}`));
    if (pitrOn()) checkPlan();
  }

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = form;
    const req = { location_id: +f.location_id.value, threads: f.threads ? +f.threads.value || 1 : 1 };
    if (needPass) req.passphrase = f.passphrase.value;
    let dangerous = false;
    let summary;
    if (physical && pg) {
      Object.assign(req, { target_dir: f.target_dir.value.trim(), os_user: f.os_user.value.trim() });
      if (canApply() && f.apply.checked) {
        Object.assign(req, { apply: true, target_server_id: +f.target_server_id.value, datadir: f.datadir.value.trim(), service_name: f.service_name.value.trim() });
        if (!req.datadir) { toast('Data directory is required', 'bad'); return; }
        dangerous = true;
        summary = html`PostgreSQL on this machine will be <b>stopped</b> and data directory <code>${req.datadir}</code> replaced with the cluster of backup <b>${r.dir_name}</b>.`;
      }
    } else if (physical) {
      Object.assign(req, { target_dir: f.target_dir.value.trim(), use_memory: f.use_memory.value.trim() });
      if (canApply() && f.apply.checked) {
        Object.assign(req, { apply: true, target_server_id: +f.target_server_id.value, datadir: f.datadir.value.trim(), service_name: f.service_name.value.trim(), os_user: f.os_user.value.trim(), move_back: f.move_back.checked });
        if (!req.datadir) { toast('Datadir is required', 'bad'); return; }
        dangerous = true;
        summary = html`MySQL/MariaDB on this machine will be <b>stopped</b> and datadir <code>${req.datadir}</code> replaced with the data from backup <b>${r.dir_name}</b>.`;
      }
    } else {
      const dbs = single ? r.databases.map((d) => d.name) : $$('input[name=db]:checked', el).map((c) => c.value);
      if (!dbs.length) { toast('No databases selected', 'bad'); return; }
      const rename = {};
      if (!single) $$('[data-rename]', el).forEach((i) => { if (dbs.includes(i.dataset.rename) && i.value.trim() && i.value.trim() !== i.dataset.rename) rename[i.dataset.rename] = i.value.trim(); });
      Object.assign(req, { target_server_id: +f.target_server_id.value, databases: dbs, rename });
      [...optList('options'), ...optList('compat')].forEach((o) => { req[o.key] = !!(f[o.key] && f[o.key].checked); });
      if (check && !check.ok) { toast(`Can't connect to the target: ${check.error}`, 'bad', 9000); return; }
      const target = servers.find((s) => s.id === req.target_server_id);
      const names = dbs.map((d) => rename[d] || d);
      const fixes = PORTABLE.filter((k) => req[k]).map((k) => SHORT[k] || k);
      // overwriting existing databases on the target needs the typed confirmation too
      dangerous = req.drop_existing || overwrites().length > 0;
      summary = html`Restore ${dbs.length} database(s) to <b>${target ? target.name : ''}</b>${check && check.ok ? html` (${flavorName(check.flavor, check.version)}${check.same_server ? '' : ', another instance'})` : ''}: <code>${names.join(', ')}</code>.${req.drop_existing ? html`<br><b style="color:var(--critical-ink)">The target databases will be dropped first.</b>` : html`<br>Tables with the same name in the target databases will be overwritten.`}${fixes.length ? html`<br>Compatibility: ${fixes.join(', ')}.` : ''}`;
    }
    if (pitrOn()) {
      const target = readPITRTarget(pitrBox);
      if (!target) { toast('Enter where point-in-time recovery should stop', 'bad'); return; }
      if (physical && !pg && !req.apply) { toast(app.platform === 'windows' ? 'PITR from a physical backup is only available when DumpCat runs on Linux' : 'PITR from a physical backup needs "Apply right away"', 'bad'); return; }
      if (physical && !pg && !req.target_server_id) { toast('Choose the server to replay the binlogs into', 'bad'); return; }
      if (!plan || !plan.ok || !plan.run_ok) { toast(plan && (plan.run_error || plan.error) ? (plan.run_error || plan.error) : 'Point-in-time recovery is not possible from this backup', 'bad', 9000); return; }
      req.pitr = target;
      summary = html`${summary || html`Restore backup <b>${r.dir_name}</b>.`}<br>${pg ? html`Recovered from the archived WAL up to <b>${plan.target}</b>.` : html`Then replay binlogs up to <b>${plan.target}</b>.`}`;
    }
    const okd = await confirmBox({ title: 'Confirm restore', message: summary || 'Start the restore?', danger: dangerous, okText: 'Restore', typeText: dangerous ? 'RESTORE' : '' });
    if (!okd) return;
    if (dangerous) req.confirm = 'RESTORE';
    await busy(form.querySelector('button[type=submit]'), async () => {
      try {
        const rs = await post(`/runs/${r.id}/restore`, req);
        toast('Restore #' + rs.id + ' started');
        navigate(`/restores/${rs.id}`);
      } catch (er) { toast(er.message, 'bad', 9000); }
    });
  });
}
