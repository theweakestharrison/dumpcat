import { get, qs } from '../api.js';
import { html, setHTML, patch, icon, on, fmtDate, fmtBytes, fmtDur, runDuration, statusBadge, empty, pager, modal } from '../ui.js';
import { poll, canOperate, navigate } from '../app.js';
import { methodBadge } from '../components.js';

export const title = 'Restores';

export async function render(ctx) {
  const { el } = ctx;
  const f = { limit: 50, offset: 0 };
  setHTML(el, html`<div class="page-head"><div><h1>Restores</h1><div class="sub">Restore history — start a new restore from a backup of any job</div></div>
      ${canOperate() ? html`<button class="btn btn-primary" id="new-restore">${icon('restore')}New restore</button>` : ''}</div>
    <div class="card" id="list"></div>`);
  const box = el.querySelector('#list');
  let running = false;
  const load = async () => {
    const res = await get('/restores' + qs(f));
    running = res.items.some((x) => x.status === 'running');
    patch(box, res.items.length ? html`<div class="table-wrap"><table class="table"><thead><tr><th>Restore</th><th>Backup</th><th class="hide-sm">Kind</th><th>Target server</th><th class="hide-sm">User</th><th class="right hide-sm">Duration</th><th>Status</th></tr></thead>
      <tbody>${res.items.map((x) => html`<tr class="clickable">
        <td class="nowrap"><a class="row-link" href="#/restores/${x.id}"><span class="muted num">#${x.id}</span> ${fmtDate(x.started_at)}</a></td>
        <td>${x.run_id ? html`<a href="#/runs/${x.run_id}">${x.job_name}</a>` : x.job_name}</td>
        <td class="hide-sm"><span class="badge outline">${x.kind === 'physical' ? 'Physical' : 'Logical'}</span>${x.options && x.options.pitr ? html` <span class="badge info">PITR</span>` : ''}</td>
        <td>${x.target_server_name || '—'}</td><td class="hide-sm">${x.username}</td>
        <td class="right num hide-sm">${fmtDur(runDuration(x))}</td>
        <td>${statusBadge(x.status)}${x.error ? html`<div class="sub" style="max-width:300px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${x.error}">${x.error}</div>` : ''}</td></tr>`)}</tbody></table></div>${pager(res.total, f.limit, f.offset)}`
      : empty('No restores yet. Test your backups regularly: restore one into a new database name and check the data.', 'restore'));
  };
  await load();
  on(el, 'click', '[data-page]', (e, b) => { f.offset = Math.max(0, +b.dataset.page); load(); });
  on(el, 'click', '#new-restore', () => pickBackup());
  poll(ctx, async () => { if (running) await load(); }, 4000);
}

/** New restore: pick a job, then one of its restorable backups. */
async function pickBackup() {
  const jobs = await get('/jobs');
  const m = modal({
    title: 'New restore', size: 'lg',
    body: html`<div class="field"><label for="pick-job">Backup job</label><select id="pick-job">
        <option value="0">All jobs</option>${jobs.map(({ job }) => html`<option value="${job.id}">${job.name} (${job.server_name})</option>`)}</select></div>
      <div id="pick-list" style="margin-top:14px"></div>
      <p class="muted small" style="margin-bottom:0">For point-in-time recovery use <a href="#/pitr">Binlog &amp; PITR</a>.</p>`,
    footer: html`<button class="btn" data-close>Cancel</button>`,
  });
  const box = m.el.querySelector('#pick-list');
  const load = async () => {
    const jid = +m.el.querySelector('#pick-job').value;
    setHTML(box, html`<div class="loading-page" style="min-height:120px">${icon('loader', 'spin')}</div>`);
    const res = await get('/runs' + qs({ job_id: jid, live: 1, limit: 30 }));
    const items = res.items.filter((r) => r.status === 'success' || r.status === 'partial');
    setHTML(box, items.length ? html`<div class="table-wrap" style="max-height:50vh;overflow:auto"><table class="table"><thead><tr><th>Backup</th><th>Job</th><th class="hide-sm">Type</th><th class="right">Size</th><th><span class="sr-only">Action</span></th></tr></thead><tbody>
      ${items.map((r) => html`<tr><td class="nowrap"><span class="muted num">#${r.id}</span> ${fmtDate(r.started_at)}</td><td>${r.job_name}</td>
        <td class="hide-sm">${methodBadge(r.method, r.type)}</td><td class="right num">${fmtBytes(r.size_bytes)}</td>
        <td class="actions"><a class="btn btn-sm btn-primary" href="#/runs/${r.id}/restore" data-pick>${icon('restore')}Restore</a></td></tr>`)}</tbody></table></div>`
      : empty('No restorable backups for this job yet.', 'archive'));
  };
  m.el.querySelector('#pick-job').onchange = load;
  on(m.el, 'click', '[data-pick]', (e, a) => { e.preventDefault(); m.close(); navigate(a.getAttribute('href').slice(1)); });
  on(m.el, 'click', 'a[href="#/pitr"]', () => m.close());
  load();
}
