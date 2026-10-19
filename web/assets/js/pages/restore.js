import { get, post } from '../api.js';
import { html, setHTML, icon, on, $, $$, fmtBytes, fmtDate, toast, busy, confirmBox, debounce } from '../ui.js';
import { navigate, isAdmin, canOperate, setTitle, app } from '../app.js';
import { methodBadge, backBtn, pitrTargetFields, bindPITRFields, readPITRTarget, pitrPlanView } from '../components.js';
import { openServerForm } from './servers.js';

// replacing the datadir (stop service, copy-back, chown, start) is Linux-only
const canApply = () => isAdmin() && app.platform !== 'windows';

// rewrites of a logical dump for another instance (engine.PortableOpts)
const PORTABLE = ['strip_definer', 'convert_collations', 'skip_gtid_purged'];

const flavorName = (flavor, version) => `${flavor === 'mariadb' || /mariadb/i.test(version || '') ? 'MariaDB' : 'MySQL'} ${version || ''}`.trim();

export const title = 'Restore';

export async function render(ctx) {
  const { el, params, query } = ctx;
  if (!canOperate()) { setHTML(el, html`<div class="alert warn">${icon('lock')}<div>You don't have permission to restore.</div></div>`); return; }
  const id = +params.id;
  const [{ run: r }, srvList] = await Promise.all([get(`/runs/${id}`), get('/servers')]);
  let servers = srvList;
  const physical = r.method === 'physical';
  const single = r.meta && r.meta.single_file === true;
  const hasUsers = r.files.some((f) => f.kind === 'users');
  const okLocs = r.locations.filter((l) => l.status === 'ok');
  const needPass = r.encrypted && !r.has_passphrase;
  const srcServer = servers.find((s) => s.id === r.server_id);
  const hasPos = !!(r.meta && r.meta.binlog_file) || r.databases.some((d) => d.binlog_file);
  const canPITR = hasPos && (!physical || isAdmin());
  setTitle([{ label: 'Backups', href: '#/runs' }, { label: `${r.job_name} #${r.id}`, href: `#/runs/${r.id}` }, 'Restore']);
  const srcVersion = r.meta && r.meta.server_version ? flavorName(r.meta.flavor, r.meta.server_version) : '';
  const serverOptions = (blank) => html`${blank ? html`<option value="0">—</option>` : ''}${servers.map((s) => html`<option value="${s.id}" ${s.id === r.server_id ? 'selected' : ''}>${s.name} (${s.host || s.socket})${s.id === r.server_id ? ' — source server' : ''}</option>`)}`;
  const newServerBtn = html`<button type="button" class="btn" id="add-server" title="Add another MySQL/MariaDB instance as a restore target">${icon('plus')}New server</button>`;
  let pitrPre = null;
  try { pitrPre = query.pitr ? JSON.parse(query.pitr) : null; } catch { pitrPre = null; }

  setHTML(el, html`
    <div class="page-head">${backBtn(`#/runs/${r.id}`)}<div><h1>Restore backup</h1><div class="sub">${r.job_name} · ${r.dir_name} · ${fmtDate(r.started_at)}</div></div></div>
    <form id="rs-form" autocomplete="off">
    <div class="card"><div class="card-head"><h2>Source</h2>${methodBadge(r.method, r.type)}</div><div class="card-body">
      <div class="form-grid">
        <div class="field"><label>Read data from</label><select name="location_id">
          <option value="0">Automatic (prefer local storage)</option>
          ${okLocs.map((l) => html`<option value="${l.id}">${l.storage_name} (${l.storage_type})</option>`)}</select>
          <div class="help">Data is streamed directly: download → decrypt → decompress → load into the database, with no temporary files (except mydumper / physical, which must be extracted to a directory).</div></div>
        <div class="field"><label>Size</label><div style="padding-top:8px">${fmtBytes(r.size_bytes)} stored · ${fmtBytes(r.raw_bytes)} raw data</div></div>
        ${needPass ? html`<div class="field full"><label class="req">Decryption passphrase</label><input type="password" name="passphrase" required autocomplete="off"><div class="help">This backup was imported from storage, so its passphrase isn't stored in DumpCat.</div></div>` : ''}
      </div>
    </div></div>

    ${physical ? html`
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
        <div class="field"><label>Databases restored in parallel</label><input type="number" name="threads" value="${r.method === 'mydumper' ? 4 : 1}" min="1" max="32">
          <div class="help">${r.method === 'mydumper' ? 'Number of myloader threads (loads many tables/chunks in parallel).' : 'Each database uses its own mysql process.'}</div></div>
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
      <div class="checks">
        <label class="check"><input type="checkbox" name="drop_existing"><span><b>Drop target databases before restoring</b><span class="help">DROP DATABASE, then recreate. Unchecked: tables in the dump are recreated (DROP TABLE IF EXISTS) but other tables are left untouched.</span></span></label>
        <label class="check"><input type="checkbox" name="fast_mode" checked><span>Fast mode<span class="help">Temporarily sets innodb_flush_log_at_trx_commit=2 &amp; sync_binlog=0 during the restore, then restores them — much faster for large databases.</span></span></label>
        <label class="check"><input type="checkbox" name="no_binlog"><span>Don't write binlog (sql_log_bin=0)<span class="help">The restore (including DROP/CREATE DATABASE) is not replicated to replicas. Requires SUPER / SYSTEM_VARIABLES_ADMIN — without it the restore stops before changing anything.</span></span></label>
        ${hasUsers ? html`<label class="check"><input type="checkbox" name="restore_users"><span>Restore users &amp; grants<span class="help">Runs _users.sql (CREATE USER IF NOT EXISTS + GRANT).</span></span></label>` : ''}
      </div>
      <div class="section-title" style="margin-top:16px">Compatibility with the target</div>
      <div class="help" style="margin-bottom:8px">For restores onto another instance or version. Schema statements are rewritten while streaming; row data is never changed. Ticked automatically from what DumpCat finds on the target.</div>
      <div class="checks">
        <label class="check"><input type="checkbox" name="strip_definer"><span>Remove DEFINER from views, routines, triggers and events<span class="help">They are created with the restore account as definer. Needed when the original accounts don't exist on the target, or the restore user lacks SET_USER_ID / SUPER.</span></span></label>
        <label class="check"><input type="checkbox" name="convert_collations"><span>Convert collations the target may not know<span class="help">MySQL 8 <code>utf8mb4_0900_*</code> and MariaDB <code>*_uca1400_*</code> → <code>utf8mb4_unicode_ci</code> / <code>utf8mb4_bin</code>; <code>utf8mb3_*</code> → <code>utf8_*</code>. Needed for MySQL 8 → MariaDB or MySQL 5.7.</span></span></label>
        ${r.method === 'mysqldump' ? html`<label class="check"><input type="checkbox" name="skip_gtid_purged"><span>Skip GTID_PURGED<span class="help">Drops <code>SET @@GLOBAL.GTID_PURGED</code> from the dump: it fails on a server with its own GTID history and needs SUPER. Keep it only to build a replica of the source.</span></span></label>` : ''}
      </div>
    </div></div>`}
    <div class="card"><div class="card-head"><h2>Point-in-time recovery</h2>${hasPos ? '' : html`<span class="badge">no binlog position</span>`}</div><div class="card-body">
      <label class="check"><input type="checkbox" name="pitr" ${canPITR && pitrPre ? 'checked' : ''} ${canPITR ? '' : 'disabled'}><span><b>Replay archived binlogs after the restore</b><span class="help">${hasPos
        ? html`Restores this backup, then applies the archived binary logs up to the moment you choose — e.g. just before an accidental DROP TABLE. All binlogs are downloaded and checked <b>before</b> anything is changed.${physical ? html` For a physical backup this needs <b>Apply right away</b> and a server (to connect to after it starts); the whole instance is replayed.${isAdmin() ? '' : ' Only admins can do this.'}` : ' Only the restored databases are replayed, each from its own snapshot position, under their new names.'}`
        : 'This backup does not record its binlog position (mysqldump: enable "Record binlog position", or turn on binlog archiving for the job), so it cannot be a starting point.'}</span></span></label>
      <div id="pitr-box" class="${canPITR && pitrPre ? '' : 'hidden'}" style="margin-top:14px">
        ${pitrTargetFields(pitrPre || {})}
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
        form.os_user.value = s.os_user || 'mysql';
      }
      checkTarget();
    });
    on(el, 'click', '#add-server', () => openServerForm(null, async (sv) => {
      servers = await get('/servers');
      const sel = form.target_server_id;
      setHTML(sel, serverOptions(physical));
      sel.value = String(sv.id);
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    }));
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
    setHTML(box, html`<div class="muted small">${icon('loader', 'spin')} Checking the archived binlogs…</div>`);
    try { plan = await post('/pitr/plan', { run_id: r.id, target }); } catch (er) { plan = { ok: false, target: target.type, error: er.message }; }
    if (ctx.alive()) setHTML(box, pitrPlanView(plan, r.id, target));
  };
  const recheck = debounce(checkPlan, 400);
  if (form.pitr) {
    bindPITRFields(pitrBox);
    form.pitr.addEventListener('change', () => {
      pitrBox.classList.toggle('hidden', !pitrOn());
      if (pitrOn() && physical && form.apply && !form.apply.checked) {
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
    const req = { location_id: +f.location_id.value, threads: +f.threads.value || 1 };
    if (needPass) req.passphrase = f.passphrase.value;
    let dangerous = false;
    let summary;
    if (physical) {
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
      Object.assign(req, {
        target_server_id: +f.target_server_id.value, databases: dbs, rename, drop_existing: f.drop_existing.checked,
        fast_mode: f.fast_mode.checked, no_binlog: f.no_binlog.checked, restore_users: !!(f.restore_users && f.restore_users.checked),
      });
      PORTABLE.forEach((k) => { req[k] = !!(f[k] && f[k].checked); });
      if (check && !check.ok) { toast(`Can't connect to the target: ${check.error}`, 'bad', 9000); return; }
      const target = servers.find((s) => s.id === req.target_server_id);
      const names = dbs.map((d) => rename[d] || d);
      const fixes = [req.strip_definer && 'remove DEFINER', req.convert_collations && 'convert collations', req.skip_gtid_purged && 'skip GTID_PURGED'].filter(Boolean);
      // overwriting existing databases on the target needs the typed confirmation too
      dangerous = req.drop_existing || overwrites().length > 0;
      summary = html`Restore ${dbs.length} database(s) to <b>${target ? target.name : ''}</b>${check && check.ok ? html` (${flavorName(check.flavor, check.version)}${check.same_server ? '' : ', another instance'})` : ''}: <code>${names.join(', ')}</code>.${req.drop_existing ? html`<br><b style="color:var(--critical-ink)">The target databases will be dropped first.</b>` : html`<br>Tables with the same name in the target databases will be overwritten.`}${fixes.length ? html`<br>Compatibility: ${fixes.join(', ')}.` : ''}`;
    }
    if (pitrOn()) {
      const target = readPITRTarget(pitrBox);
      if (!target) { toast('Enter where point-in-time recovery should stop', 'bad'); return; }
      if (physical && !req.apply) { toast(app.platform === 'windows' ? 'PITR from a physical backup is only available when DumpCat runs on Linux' : 'PITR from a physical backup needs "Apply right away"', 'bad'); return; }
      if (physical && !req.target_server_id) { toast('Choose the server to replay the binlogs into', 'bad'); return; }
      if (!plan || !plan.ok || !plan.run_ok) { toast(plan && (plan.run_error || plan.error) ? (plan.run_error || plan.error) : 'Point-in-time recovery is not possible from this backup', 'bad', 9000); return; }
      req.pitr = target;
      summary = html`${summary || html`Restore backup <b>${r.dir_name}</b>.`}<br>Then replay binlogs up to <b>${plan.target}</b>.`;
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
