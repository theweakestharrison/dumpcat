import { get, post, put, qs } from '../api.js';
import { html, setHTML, icon, on, $, $$, fmtDate, toast, busy, debounce, describeCron, validate, formError, patch } from '../ui.js';
import { navigate, canOperate, setTitle, setLeaveGuard } from '../app.js';
import { backBtn, drillRunsTable, engines, engineOf, methodOf } from '../components.js';

export const title = 'Restore drill';

const PRESETS = [
  ['', '— presets —'], ['0 5 * * 0', 'Weekly on Sunday at 05:00'], ['0 4 * * *', 'Daily at 04:00'], ['0 5 1 * *', 'Day 1 of every month at 05:00'], ['@manual', 'Manual only'],
];

export async function render(ctx) {
  const { el, params, query } = ctx;
  if (!canOperate()) { setHTML(el, html`<div class="alert warn">${icon('lock')}<div>You have read-only access.</div></div>`); return; }
  const editing = params.id ? +params.id : 0;
  const [jobs, allServers, engs] = await Promise.all([get('/jobs'), get('/servers'), engines()]);
  let d = { name: '', job_id: +query.job || 0, target_server_id: 0, schedule: '0 5 * * 0', enabled: true, databases: [], assertions: [],
    options: { threads: 2, no_binlog: false, max_rto_minutes: 0, row_counts: '' }, notify_success: false, notify_failure: true };
  let view = null;
  if (editing) {
    view = await get(`/drills/${editing}`);
    d = { ...d, ...view.drill, options: { ...d.options, ...view.drill.options } };
  }
  const methodInfo = (j) => (methodOf(engs, j.method) || { engine: engs[0], method: {} });
  const usable = (j) => !!methodInfo(j).method.drill && !(j.method === 'mysqldump' && j.options.single_file);
  if (!d.job_id) d.job_id = (jobs.find(({ job }) => usable(job)) || { job: {} }).job.id || 0;
  // a sandbox runs the job's engine
  const jobEngine = (id) => { const j = jobs.find(({ job }) => job.id === id); return j ? methodInfo(j.job).engine : engs[0]; };
  const serversFor = (id) => allServers.filter((s) => engineOf(engs, s.engine).engine === jobEngine(id).engine);
  let servers = serversFor(d.job_id);
  if (!d.target_server_id) {
    const job = jobs.find(({ job }) => job.id === d.job_id);
    d.target_server_id = (servers.find((s) => !job || s.id !== job.job.server_id) || servers[0] || {}).id || 0;
  }
  const serverOptions = (sel) => html`${servers.map((s) => html`<option value="${s.id}" ${s.id === sel ? 'selected' : ''}>${s.name} (${s.host || s.socket}${s.version ? ' · ' + s.version : ''})</option>`)}`;
  if (!d.name) {
    const job = jobs.find(({ job }) => job.id === d.job_id);
    d.name = job ? `${job.job.name} weekly drill` : '';
  }
  const titleText = editing ? `Edit drill: ${d.name}` : 'New restore drill';
  setTitle([{ label: 'Restore drills', href: '#/drills' }, titleText]);

  setHTML(el, html`
    <div class="page-head">${backBtn('#/drills')}<div><h1>${titleText}</h1>
      <div class="sub">Restores the newest backup of a job into a sandbox server under temporary names (<code>&lt;database&gt;__drill_&lt;id&gt;</code>), checks it and drops the copies</div></div></div>
    ${!servers.length || !jobs.length ? html`<div class="alert warn" style="margin-bottom:16px">${icon('alert')}<div>A drill needs a <a href="#/jobs">backup job</a> and a <a href="#/servers">database server</a> to restore into.</div></div>` : ''}
    <form id="drill-form" autocomplete="off">
      <div class="card"><div class="card-head"><h2>1. What to restore, and where</h2></div><div class="card-body">
        <div class="form-grid">
          <div class="field"><label class="req">Drill name</label><input type="text" name="name" value="${d.name}" required maxlength="100" placeholder="e.g. shop weekly drill"></div>
          <div class="field"><label class="req">Backup job</label><select name="job_id">${jobs.map(({ job: j }) => html`<option value="${j.id}" ${j.id === d.job_id ? 'selected' : ''} ${usable(j) ? '' : 'disabled'}>${j.name} (${j.method}${!methodInfo(j).method.drill ? ' — not supported' : usable(j) ? '' : ' one file — not supported'})</option>`)}</select>
            <div class="help">Each run takes the newest successful backup of this job. Logical backups (mysqldump with one file per database, mydumper, pg_dump) are supported.</div></div>
          <div class="field"><label class="req">Sandbox server</label><select name="target_server_id">${serverOptions(d.target_server_id)}</select>
            <div class="help">The drill connects with the server's restore account (or its backup account), creates the temporary databases and drops them afterwards. A staging or spare server is best.</div>
            <div id="same-warn"></div></div>
          <div class="field" id="threads-field"><label id="threads-label">Restore threads (databases in parallel)</label><input type="number" name="threads" min="0" max="32" value="${d.options.threads}"></div>
        </div>
        <div class="section-title" style="margin-top:16px">Databases</div>
        <div class="help" style="margin-bottom:8px">None selected: every database of the backup.</div>
        <div id="db-list" class="db-pick"></div>
      </div></div>

      <div class="card"><div class="card-head"><h2>2. Schedule</h2></div><div class="card-body">
        <div class="form-grid">
          <div class="field"><label>Drill schedule (cron)</label>
            <div class="inline"><input type="text" name="schedule" value="${d.schedule}" placeholder="minute hour day month weekday — e.g. 0 5 * * 0">
            <select id="preset" style="max-width:220px">${PRESETS.map(([v, l]) => html`<option value="${v}">${l}</option>`)}</select></div>
            <div class="help" id="cron-desc"></div><div class="help" id="cron-next"></div></div>
          <div class="field"><label>Maximum restore time (minutes, 0 = no limit)</label><input type="number" name="max_rto_minutes" min="0" value="${d.options.max_rto_minutes}">
            <div class="help">Your recovery time objective: the drill fails when downloading and loading the backup takes longer.</div></div>
        </div>
        <div class="help">Pick a quiet time after the backup: a drill takes one execution slot (Settings → Max concurrent backup jobs) and loads the sandbox like a restore.</div>
      </div></div>

      <div class="card"><div class="card-head"><h2>3. Checks</h2></div><div class="card-body">
        <p class="help" style="margin-top:0" id="checks-help">Always checked: the restore succeeds, every table of the backup exists in the copy, and each table holds exactly the rows the backup recorded (DumpCat counts them while it writes the dump).</p>
        <div class="field hidden" id="rows-field" style="max-width:560px;margin-bottom:14px"><label for="row_counts">Row counts</label><select name="row_counts" id="row_counts">
          <option value="" ${d.options.row_counts ? '' : 'selected'}>Count every row (COUNT(*) per table)</option>
          <option value="estimate" id="estimate-opt" ${d.options.row_counts === 'estimate' ? 'selected' : ''}>Estimate after ANALYZE (fast for very large tables; more than 10 % off is a warning)</option></select>
          <div class="help" id="estimate-help">The server's statistics are exact for tables up to about 30,000 rows and close beyond.</div></div>
        <div class="section-title">Assertions (optional)</div>
        <div class="help" style="margin-bottom:10px" id="assert-help">One <code>SELECT</code> each, run inside the copy of the chosen database in a read-only session. It passes when the first column of the first row is true (not NULL, 0 or empty), e.g. <code>SELECT MAX(created_at) &gt; NOW() - INTERVAL 2 DAY FROM orders</code>.</div>
        <div id="asserts"></div>
        <button type="button" class="btn btn-sm" id="add-assert">${icon('plus')}Add assertion</button>
      </div></div>

      <div class="card"><div class="card-head"><h2>4. Notifications &amp; status</h2></div><div class="card-body">
        <div class="checks">
          <label class="check"><input type="checkbox" name="notify_failure" ${d.notify_failure ? 'checked' : ''}><span>Notify when a drill fails (or leaves databases behind)</span></label>
          <label class="check"><input type="checkbox" name="notify_success" ${d.notify_success ? 'checked' : ''}><span>Notify when a drill passes</span></label>
          <label class="check" id="no-binlog-box"><input type="checkbox" name="no_binlog" ${d.options.no_binlog ? 'checked' : ''}><span>Keep the drill out of the sandbox's binary log<span class="help">For a sandbox that is a replication source or archives its binlogs. Needs SUPER / BINLOG ADMIN. Always on when the sandbox is the job's own server.</span></span></label>
          <label class="check"><input type="checkbox" name="enabled" ${d.enabled ? 'checked' : ''}><span>Enable scheduled runs</span></label>
        </div>
      </div></div>

      <div class="sticky-actions">
        <span class="dirty-note" id="dirty-note" aria-live="polite"></span>
        <a class="btn" href="#/drills">Cancel</a>
        <button class="btn" type="button" id="save-run">${icon('play')}Save &amp; run now</button>
        <button class="btn btn-primary" type="submit">${icon('check')}Save drill</button>
      </div>
    </form>
    ${editing ? html`<div class="card" id="history"><div class="card-head"><h2>Results</h2></div><div id="history-list"><div class="card-body muted">Loading…</div></div></div>` : ''}`);

  const form = $('#drill-form', el);
  const jobOf = () => jobs.find(({ job }) => job.id === +form.job_id.value);
  let dbNames = [];

  const renderAsserts = () => {
    const rows = d.assertions.length ? d.assertions : [];
    setHTML($('#asserts', el), rows.length ? rows.map((a, i) => html`<div class="assert-row card-body" data-i="${i}" style="border:1px solid var(--border);border-radius:10px;margin-bottom:10px;padding:12px">
        <div class="form-grid">
          <div class="field"><label>Name</label><input type="text" data-a="name" value="${a.name}" placeholder="e.g. recent orders"></div>
          <div class="field"><label>Database</label><select data-a="database"><option value="">(none — qualify names yourself)</option>
            ${[...new Set([...dbNames, a.database].filter(Boolean))].map((n) => html`<option value="${n}" ${n === a.database ? 'selected' : ''}>${n}</option>`)}</select></div>
          <div class="field full"><label>Query</label><textarea data-a="sql" rows="2" spellcheck="false" class="mono" placeholder="SELECT COUNT(*) > 0 FROM orders">${a.sql}</textarea></div>
        </div>
        <button type="button" class="btn btn-sm btn-ghost" data-del-assert="${i}">${icon('trash')}Remove</button></div>`)
      : html`<div class="muted small" style="margin-bottom:10px">No assertions.</div>`);
  };
  const readAsserts = () => $$('.assert-row', el).map((r) => ({ name: $('[data-a=name]', r).value.trim(), database: $('[data-a=database]', r).value, sql: $('[data-a=sql]', r).value.trim() }));

  const renderDBs = () => {
    const sel = new Set(d.databases);
    const names = [...new Set([...dbNames, ...d.databases])];
    setHTML($('#db-list', el), names.length ? names.map((n) => html`<label class="check"><input type="checkbox" name="db" value="${n}" ${sel.has(n) ? 'checked' : ''}><span><b>${n}</b>${dbNames.includes(n) ? '' : html` <span class="muted small">(not in the newest backup)</span>`}</span></label>`)
      : html`<div class="muted small">The job has no successful backup yet: every database of its backups will be drilled.</div>`);
  };
  const loadDBs = async () => {
    const j = jobOf();
    dbNames = [];
    if (j) {
      try {
        const res = await get('/runs' + qs({ job_id: j.job.id, status: 'success', limit: 1 }));
        if (res.items[0]) dbNames = res.items[0].databases.map((x) => x.name);
      } catch { /* keep empty */ }
    }
    d.databases = d.databases.filter((n) => !dbNames.length || dbNames.includes(n) || editing);
    renderDBs();
    d.assertions = readAsserts().length ? readAsserts() : d.assertions;
    renderAsserts();
  };
  const sameWarn = () => {
    const j = jobOf();
    const same = j && +form.target_server_id.value === j.job.server_id;
    const mysql = !j || methodInfo(j.job).engine.engine === 'mysql';
    setHTML($('#same-warn', el), same ? html`<div class="alert warn" style="margin-top:8px">${icon('alert')}<div>This is the server the job backs up. The copies get temporary names that cannot collide with its databases, ${mysql ? 'nothing is written to its binary log (the restore account needs SUPER / BINLOG ADMIN), ' : ''}and only the drill's own copies are dropped — but the restore loads the production server. A separate sandbox is safer.</div></div>` : '');
  };
  // what the job's method offers: row estimates, a binary log to stay out of
  const engineFields = () => {
    const j = jobOf();
    const mi = j ? methodInfo(j.job) : { engine: engs[0], method: {} };
    $('#rows-field', el).classList.toggle('hidden', !mi.method.estimates);
    $('#no-binlog-box', el).classList.toggle('hidden', mi.engine.engine !== 'mysql');
    $('#threads-label', el).textContent = j && j.job.method === 'pg_dump' ? 'Restore threads (pg_restore --jobs)' : 'Restore threads (databases in parallel)';
    // SQL Server: native restores one database at a time, counts from sys.partitions, DBCC CHECKDB
    const ms = mi.engine.engine === 'mssql';
    $('#threads-field', el).classList.toggle('hidden', ms);
    setHTML($('#checks-help', el), ms
      ? html`Always checked: the restore succeeds (the full backup, then the differential), <code>DBCC CHECKDB</code> finds no allocation or consistency error in each copy, every table of the backup exists in the copy, and each table holds the rows SQL Server counted right after the backup (<code>sys.partitions</code>). A database written to while the backup ran can differ by those rows: use the estimate below for it.`
      : html`Always checked: the restore succeeds, every table of the backup exists in the copy, and each table holds exactly the rows the backup recorded (DumpCat counts them while it writes the dump).`);
    $('#estimate-opt', el).textContent = ms ? 'The server\'s row counts (sys.partitions; more than 10 % off is a warning)' : 'Estimate after ANALYZE (fast for very large tables; more than 10 % off is a warning)';
    $('#estimate-help', el).textContent = ms ? 'Fast for very large tables: no COUNT(*) on the copy.' : 'The server\'s statistics are exact for tables up to about 30,000 rows and close beyond.';
    setHTML($('#assert-help', el), ms
      ? html`One <code>SELECT</code> each, run inside the copy of the chosen database in a transaction that is rolled back. It passes when the first column of the first row is true (not NULL, 0 or empty), e.g. <code>SELECT CASE WHEN MAX(created_at) &gt; DATEADD(day, -2, SYSDATETIME()) THEN 1 ELSE 0 END FROM orders</code>.`
      : html`One <code>SELECT</code> each, run inside the copy of the chosen database in a read-only session. It passes when the first column of the first row is true (not NULL, 0 or empty), e.g. <code>SELECT MAX(created_at) &gt; NOW() - INTERVAL 2 DAY FROM orders</code>.`);
  };

  const cronPreview = async () => {
    const v = form.schedule.value.trim();
    $('#cron-desc', el).textContent = describeCron(v);
    if (!v) { $('#cron-next', el).textContent = ''; return; }
    try {
      const r = await post('/cron/preview', { expr: v });
      if (!r.valid) setHTML($('#cron-next', el), html`<span style="color:var(--critical-ink)">${r.error}</span>`);
      else $('#cron-next', el).textContent = 'Next runs: ' + r.next.slice(0, 3).map((t) => fmtDate(t)).join(' · ');
    } catch { /* ignore */ }
  };
  const pv = debounce(cronPreview, 300);
  form.schedule.addEventListener('input', pv);
  $('#preset', el).addEventListener('change', (e) => { if (e.target.value) { form.schedule.value = e.target.value === '@manual' ? '' : e.target.value; pv(); e.target.value = ''; } });
  cronPreview();

  form.addEventListener('change', (e) => {
    if (e.target.name === 'job_id') {
      d.databases = [];
      loadDBs();
      engineFields();
      // the sandbox list follows the job's engine
      const keep = +form.target_server_id.value;
      servers = serversFor(+form.job_id.value);
      setHTML(form.target_server_id, serverOptions(servers.some((s) => s.id === keep) ? keep : (servers[0] || {}).id));
    }
    if (e.target.name === 'job_id' || e.target.name === 'target_server_id') sameWarn();
    if (e.target.name === 'db') d.databases = $$('input[name=db]:checked', el).map((x) => x.value);
  });
  $('#add-assert', el).onclick = () => {
    d.assertions = [...readAsserts(), { name: '', database: d.databases[0] || dbNames[0] || '', sql: '' }];
    renderAsserts();
    $$('.assert-row', el).at(-1)?.querySelector('[data-a=name]').focus();
  };
  on(el, 'click', '[data-del-assert]', (e, b) => {
    const list = readAsserts();
    list.splice(+b.dataset.delAssert, 1);
    d.assertions = list;
    renderAsserts();
    markDirty();
  });

  let dirty = false;
  const markDirty = () => { dirty = true; $('#dirty-note', el).textContent = 'Unsaved changes'; };
  form.addEventListener('input', markDirty);
  form.addEventListener('change', (e) => { if (e.isTrusted) markDirty(); });
  setLeaveGuard(() => (dirty ? 'This drill has unsaved changes. Leave without saving?' : null));

  renderDBs();
  renderAsserts();
  sameWarn();
  engineFields();
  loadDBs();

  const collect = () => ({
    name: form.name.value.trim(), job_id: +form.job_id.value, target_server_id: +form.target_server_id.value,
    schedule: form.schedule.value.trim(), enabled: form.enabled.checked,
    databases: $$('input[name=db]:checked', el).map((x) => x.value), assertions: readAsserts().filter((a) => a.sql || a.name),
    options: { threads: +form.threads.value || 0, no_binlog: form.no_binlog.checked, max_rto_minutes: +form.max_rto_minutes.value || 0, row_counts: form.row_counts.value },
    notify_success: form.notify_success.checked, notify_failure: form.notify_failure.checked,
  });
  const save = async (andRun, btn) => {
    formError(form, null);
    if (!validate(form)) return;
    await busy(btn, async () => {
      try {
        const res = editing ? await put(`/drills/${editing}`, collect()) : await post('/drills', collect());
        dirty = false;
        toast(editing ? 'Drill saved' : 'Drill created');
        if (andRun) {
          const x = await post(`/drills/${res.drill.id}/run`);
          navigate(`/drill-runs/${x.id}`);
        } else navigate('/drills');
      } catch (e) {
        formError(form, e.message);
      }
    });
  };
  form.addEventListener('submit', (e) => { e.preventDefault(); save(false, form.querySelector('button[type=submit]')); });
  $('#save-run', el).onclick = (e) => save(true, e.currentTarget);

  if (editing) {
    const res = await get('/drill-runs' + qs({ drill_id: editing, limit: 20 }));
    patch($('#history-list', el), res.items.length ? drillRunsTable(res.items, { drill: false }) : html`<div class="card-body muted">This drill has not run yet.</div>`);
  }
}
