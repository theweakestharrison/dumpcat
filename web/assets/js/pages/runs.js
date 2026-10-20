import { get, qs } from '../api.js';
import { html, setHTML, patch, icon, on, fmtBytes, fmtDate, fmtDur, runDuration, statusBadge, verifyBadge, empty, pager, debounce } from '../ui.js';
import { poll } from '../app.js';
import { methodBadge, locationChips, drillBadge } from '../components.js';

export const title = 'Backups';

export async function render(ctx) {
  const { el, query } = ctx;
  const jobs = await get('/jobs');
  const f = { job_id: +query.job_id || 0, status: query.status || '', method: '', q: '', limit: 50, offset: 0 };

  setHTML(el, html`
    <div class="page-head"><div><h1>Backups</h1><div class="sub">Run history, storage locations, restore &amp; integrity checks</div></div></div>
    <div class="card">
      <div class="filters">
        <select id="f-job"><option value="0">All jobs</option>${jobs.map(({ job }) => html`<option value="${job.id}" ${job.id === f.job_id ? 'selected' : ''}>${job.name}</option>`)}</select>
        <select id="f-status"><option value="">All statuses</option>
          ${[['success', 'Success'], ['partial', 'Partial'], ['failed', 'Failed'], ['running', 'Running'], ['canceled', 'Canceled'], ['deleted', 'Deleted']].map(([v, l]) => html`<option value="${v}" ${f.status === v ? 'selected' : ''}>${l}</option>`)}</select>
        <select id="f-method"><option value="">All methods</option><option value="mysqldump">mysqldump</option><option value="mydumper">mydumper</option><option value="physical">Physical</option><option value="pg_dump">pg_dump</option></select>
        <input type="search" id="f-q" placeholder="Search by job, server, directory...">
      </div>
      <div id="list"></div>
    </div>`);

  const listEl = el.querySelector('#list');
  let hasRunning = false;
  const load = async () => {
    const res = await get('/runs' + qs(f));
    hasRunning = res.items.some((r) => r.status === 'running' || r.status === 'queued');
    const filtered = f.job_id || f.status || f.method || f.q;
    patch(listEl, res.items.length ? html`<div class="table-wrap"><table class="table">
      <thead><tr><th>Backup</th><th>Job</th><th class="hide-sm">Type</th><th class="right hide-sm">Size</th><th class="right hide-lg">Duration</th><th>Status</th><th class="hide-md">Stored at</th><th class="hide-sm">Integrity</th></tr></thead>
      <tbody>${res.items.map((r) => html`<tr class="clickable">
        <td class="nowrap"><a class="row-link" href="#/runs/${r.id}"><span class="muted num">#${r.id}</span> ${fmtDate(r.started_at)}</a><div class="sub">${r.trigger === 'schedule' ? 'scheduled' : r.trigger === 'import' ? 'imported from storage' : r.triggered_by || r.trigger}</div></td>
        <td class="nowrap"><b>${r.job_name}</b><div class="sub">${r.server_name}</div></td>
        <td class="nowrap hide-sm">${methodBadge(r.method, r.type)}${r.encrypted ? html` <span title="Encrypted" aria-label="Encrypted">${icon('lock')}</span>` : ''}</td>
        <td class="right num nowrap hide-sm">${r.size_bytes ? fmtBytes(r.size_bytes) : '—'}${r.raw_bytes ? html`<div class="sub">${fmtBytes(r.raw_bytes)} raw</div>` : ''}</td>
        <td class="right num hide-lg">${fmtDur(runDuration(r))}</td>
        <td>${statusBadge(r.status)}${r.error && r.status !== 'success' ? html`<span class="sub cell-ellipsis" title="${r.error}">${r.error}</span>` : ''}</td>
        <td class="hide-md">${locationChips(r.locations)}</td>
        <td class="hide-sm">${r.status === 'success' || r.status === 'partial' ? html`${verifyBadge(r.verify_status, r.verify_msg)} ${drillBadge(r.drill_status, r.drill_msg)}` : ''}</td>
      </tr>`)}</tbody></table></div>${pager(res.total, f.limit, f.offset)}`
      : filtered ? empty('No backups match the filters') : empty('No backups yet — run a backup job to create one.', 'archive', { label: 'Go to backup jobs', href: '#/jobs', icon: 'clock' }));
  };
  await load();

  const reload = () => { f.offset = 0; load(); };
  el.querySelector('#f-job').onchange = (e) => { f.job_id = +e.target.value; reload(); };
  el.querySelector('#f-status').onchange = (e) => { f.status = e.target.value; reload(); };
  el.querySelector('#f-method').onchange = (e) => { f.method = e.target.value; reload(); };
  el.querySelector('#f-q').addEventListener('input', debounce((e) => { f.q = e.target.value; reload(); }, 350));
  on(el, 'click', '[data-page]', (e, b) => { f.offset = Math.max(0, +b.dataset.page); load(); });
  poll(ctx, async () => { if (hasRunning) await load(); }, 4000);
}
