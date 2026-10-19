import { get, post } from '../api.js';
import { html, setHTML, patch, icon, on, fmtDate, fmtDur, runDuration, statusBadge, toast, toastErr, confirmBox, copyText } from '../ui.js';
import { poll, canOperate, navigate, setTitle } from '../app.js';
import { taskList, attachLog, backBtn, fmtRTO } from '../components.js';

export const title = 'Drill result';

const KIND = { restore: 'Restore', tables: 'Tables', rows: 'Row counts', sql: 'Assertion', rto: 'Restore time', cleanup: 'Cleanup' };

function checkRows(checks) {
  return checks.map((c) => html`<tr>
    <td><b>${c.name || KIND[c.kind] || c.kind}</b>${c.kind === 'sql' ? html`<div class="sub">assertion</div>` : ''}</td>
    <td class="hide-sm">${c.database || '—'}</td>
    <td class="right num hide-sm">${c.expected || ''}</td>
    <td class="right num">${c.actual || ''}</td>
    <td>${statusBadge(c.status)}${c.message ? html`<div class="sub" style="max-width:460px;white-space:normal">${c.message}</div>` : ''}
      ${c.tables && c.tables.length ? html`<details style="margin-top:6px"><summary class="small">${c.tables.length} table(s)</summary>
        <table class="table" style="margin-top:6px"><thead><tr><th>Table</th><th class="right">Backup</th><th class="right">Copy</th></tr></thead>
        <tbody>${c.tables.map((t) => html`<tr><td class="mono small">${t.table}</td><td class="right num">${t.expected}</td><td class="right num">${t.actual < 0 ? html`<span class="badge bad">missing</span>` : t.actual}</td></tr>`)}</tbody></table></details>` : ''}</td></tr>`);
}

export async function render(ctx) {
  const { el, params } = ctx;
  const id = +params.id;
  let data = await get(`/drill-runs/${id}`);
  const live = () => data.drill_run.status === 'running';
  setTitle([{ label: 'Restore drills', href: '#/drills' }, `Drill #${id}`]);
  setHTML(el, html`<div id="detail"></div>
    <div class="card"><div class="card-head"><h2>Log</h2><button class="btn btn-sm btn-ghost" id="copy-log">${icon('copy')}Copy</button></div><div class="card-body"><pre class="log" id="log"></pre></div></div>`);
  const draw = () => {
    const { drill_run: x, task } = data;
    const failed = x.checks.filter((c) => c.status === 'failed').length;
    const passed = x.checks.filter((c) => c.status === 'ok').length;
    patch(el.querySelector('#detail'), html`
      <div class="page-head">${backBtn('#/drills')}
        <div style="flex:1;min-width:240px"><h1 class="h1-status">${x.drill_name} <span class="muted" style="font-weight:500">#${x.id}</span> ${statusBadge(x.status)}</h1>
          <div class="sub">${x.job_name} ${x.run_dir} → ${x.target_server_name} · ${fmtDate(x.started_at, true)}</div></div>
        <div class="btn-group">
          ${canOperate() && live() ? html`<button class="btn btn-danger" id="cancel">${icon('stop')}Cancel</button>` : ''}
          ${canOperate() && !live() && x.drill_id ? html`<a class="btn" href="#/drills/${x.drill_id}/edit">${icon('edit')}Edit drill</a>${x.run_id ? html`<button class="btn btn-primary" id="again" title="Drill the same backup again">${icon('play')}Run again</button>` : ''}` : ''}
        </div>
      </div>
      ${x.status === 'passed' ? html`<div class="alert good" style="margin-bottom:16px">${icon('check')}<div><b>The backup restores.</b> ${x.summary}</div></div>` : ''}
      ${x.error ? html`<div class="alert ${x.status === 'passed' ? 'warn' : x.status === 'canceled' ? 'info' : 'bad'}" style="margin-bottom:16px">${icon('alert')}<div style="white-space:pre-line">${x.error}</div></div>` : ''}
      ${x.leftover.length && !live() ? html`<div class="alert warn" style="margin-bottom:16px">${icon('alert')}<div>Temporary databases still on <b>${x.target_server_name}</b>: <code>${x.leftover.join(', ')}</code>. DumpCat retries when it restarts; you can also drop them yourself.</div></div>` : ''}
      ${task ? html`<div class="card" style="margin-bottom:16px"><div class="card-head"><h2>Progress</h2></div>${taskList([task])}</div>` : ''}
      <div class="grid grid-2">
        <div class="card"><div class="card-head"><h2>Details</h2></div><div class="card-body"><div class="kv">
          <div>Drill</div><div>${x.drill_id ? html`<a href="#/drills/${x.drill_id}/edit">${x.drill_name}</a>` : x.drill_name}</div>
          <div>Backup</div><div>${x.run_id ? html`<a href="#/runs/${x.run_id}">#${x.run_id} ${x.run_dir}</a>` : x.run_dir} <span class="muted">· ${x.job_name}</span></div>
          <div>Sandbox</div><div>${x.target_server_name}</div>
          <div>Triggered by</div><div>${x.trigger === 'schedule' ? 'Schedule' : `Manual · ${x.triggered_by}`}</div>
          <div>Time</div><div>${fmtDate(x.started_at, true)} → ${fmtDate(x.finished_at, true)} (${fmtDur(runDuration(x))})</div>
          <div>Restore time (RTO)</div><div class="num"><b>${fmtRTO(x)}</b> <span class="muted small">download &amp; load</span></div>
          <div>Checks</div><div class="num">${x.check_ms > 0 ? fmtDur(x.check_ms / 1000) : '—'}${x.checks.length ? html` · ${passed} passed${failed ? html`, <b style="color:var(--critical-ink)">${failed} failed</b>` : ''}` : ''}</div>
        </div></div></div>
        <div class="card"><div class="card-head"><h2>Databases</h2></div>
          ${x.databases.length ? html`<div class="table-wrap"><table class="table"><thead><tr><th>Database</th><th>Temporary copy</th><th class="right">Tables</th><th class="right">Rows</th></tr></thead>
            <tbody>${x.databases.map((m) => html`<tr><td><b>${m.name}</b></td><td class="mono small">${m.temp}</td><td class="right num">${m.tables || '—'}</td><td class="right num">${m.rows ? m.rows.toLocaleString('en-US') : '—'}</td></tr>`)}</tbody></table></div>`
            : html`<div class="card-body muted">—</div>`}</div>
      </div>
      <div class="card"><div class="card-head"><h2>Checks</h2></div>
        ${x.checks.length ? html`<div class="table-wrap"><table class="table"><thead><tr><th>Check</th><th class="hide-sm">Database</th><th class="right hide-sm">Expected</th><th class="right">Result</th><th>Status</th></tr></thead>
          <tbody>${checkRows(x.checks)}</tbody></table></div>` : html`<div class="card-body muted">${live() ? 'The checks run once the backup is restored.' : 'No check ran.'}</div>`}</div>`);
  };
  draw();
  const logView = attachLog(ctx, el.querySelector('#log'), `/drill-runs/${id}/log`, live);
  poll(ctx, async () => {
    if (!live()) return;
    data = await get(`/drill-runs/${id}`);
    const sc = window.scrollY;
    draw();
    window.scrollTo(0, sc);
    if (!live()) {
      logView.reload();
      const st = data.drill_run.status;
      toast(st === 'passed' ? `Drill #${id} passed` : `Drill #${id} ended: ${st}`, st === 'passed' ? 'good' : 'bad');
    }
  }, 3000);
  on(el, 'click', '#cancel', async () => {
    if (!(await confirmBox({ title: 'Cancel drill', message: 'Stop the running drill? Its temporary databases are dropped.', danger: true, okText: 'Cancel drill' }))) return;
    try { await post(`/drill-runs/${id}/cancel`); toast('Cancellation requested', 'info'); } catch (e) { toastErr(e); }
  });
  on(el, 'click', '#again', async (e, b) => {
    b.disabled = true;
    try {
      const x = await post(`/drills/${data.drill_run.drill_id}/run`, { run_id: data.drill_run.run_id });
      navigate(`/drill-runs/${x.id}`);
    } catch (er) { toastErr(er); b.disabled = false; }
  });
  on(el, 'click', '#copy-log', () => copyText(el.querySelector('#log').textContent));
}
