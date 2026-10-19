import { get, post, del, qs } from '../api.js';
import { html, patch, icon, on, fmtAgo, fmtDate, statusBadge, empty, toast, toastErr, confirmBox, describeCron, dropdown, pager } from '../ui.js';
import { poll, canOperate, navigate } from '../app.js';
import { drillRunsTable, fmtRTO } from '../components.js';

export const title = 'Restore drills';

export async function render(ctx) {
  const { el, query } = ctx;
  const jobId = +query.job_id || 0;
  const f = { job_id: jobId, limit: 20, offset: 0 };
  let drills = [];
  let runs = { items: [], total: 0 };
  const load = async () => {
    [drills, runs] = await Promise.all([get('/drills' + qs({ job_id: jobId })), get('/drill-runs' + qs(f))]);
    draw();
  };
  const draw = () => {
    const job = jobId && drills[0] ? drills[0].drill.job_name : '';
    patch(el, html`
      <div class="page-head"><div><h1>Restore drills${job ? html` <span class="muted" style="font-weight:500">· ${job}</span>` : ''}</h1>
        <div class="sub">Proof that backups restore: the newest backup of a job is restored into a sandbox server under temporary names, its tables and rows are compared with the backup, your checks run, and the copies are dropped again</div></div>
        ${canOperate() ? html`<a class="btn btn-primary" href="#/drills/new${jobId ? `?job=${jobId}` : ''}">${icon('plus')}New drill</a>` : ''}</div>
      <div class="card"><div class="table-wrap">${drills.length ? html`<table class="table">
        <thead><tr><th>Drill</th><th class="hide-sm">Sandbox</th><th class="hide-sm">Schedule</th><th>Last result</th><th class="right hide-md">Restore time</th><th class="hide-sm">Next run</th><th><span class="sr-only">Actions</span></th></tr></thead>
        <tbody>${drills.map(({ drill: d, last_run: lr, next_at: next, running, same_server: same, problem }) => html`<tr>
          <td style="min-width:170px"><a href="#/drills/${d.id}/edit"><b>${d.name}</b></a>${!d.enabled ? html` <span class="badge">${icon('stop')}Disabled</span>` : ''}
            <div class="sub"><a href="#/jobs/${d.job_id}/edit">${d.job_name}</a> · ${d.databases.length ? d.databases.join(', ') : 'all databases'}${d.assertions.length ? ` · ${d.assertions.length} assertion${d.assertions.length === 1 ? '' : 's'}` : ''}</div>
            ${problem ? html`<div class="sub" style="color:var(--critical-ink)">${problem}</div>` : ''}</td>
          <td class="hide-sm">${d.target_server_name}${same ? html` <span class="badge warn" title="The sandbox is the job's own source server: the copies get temporary names and stay out of its binary log">source server</span>` : ''}</td>
          <td class="hide-sm">${describeCron(d.schedule)}</td>
          <td>${running ? statusBadge('running') : lr ? html`<a href="#/drill-runs/${lr.id}">${statusBadge(lr.status)}</a><div class="sub">${fmtAgo(lr.started_at)}${lr.run_dir ? ` · ${lr.run_dir}` : ''}</div>` : html`<span class="muted">never run</span>`}</td>
          <td class="right num hide-md">${lr ? fmtRTO(lr) : '—'}</td>
          <td class="hide-sm">${next ? fmtDate(next) : html`<span class="muted">—</span>`}</td>
          <td class="actions">${canOperate() ? html`
            <button class="btn btn-sm btn-primary" data-key="run-${d.id}" data-run="${d.id}" ${running || problem ? 'disabled' : ''} title="Drill the newest backup now">${icon('play')}Run</button>
            <span class="dropdown"><button class="btn btn-sm btn-icon btn-ghost" data-key="more-${d.id}" data-more="${d.id}" title="More actions" aria-label="More actions for ${d.name}">${icon('dots')}</button></span>` : ''}</td>
        </tr>`)}</tbody></table>`
    : empty('No restore drills yet. A drill restores a job\'s newest backup into a sandbox server on a schedule and checks it, so you know the backups work before you need them.', 'flask',
      canOperate() ? { label: 'Create a drill', href: `#/drills/new${jobId ? `?job=${jobId}` : ''}` } : null)}</div></div>
      <div class="card"><div class="card-head"><h2>Results</h2></div>
        ${runs.items.length ? html`${drillRunsTable(runs.items)}${pager(runs.total, f.limit, f.offset)}` : html`<div class="card-body muted">No drill has run yet.</div>`}</div>`);
  };
  await load();
  const byId = (id) => drills.find((x) => x.drill.id === +id);

  on(el, 'click', '[data-run]', async (e, b) => {
    b.disabled = true;
    try {
      const x = await post(`/drills/${b.dataset.run}/run`);
      toast(`Drill started — #${x.id}`);
      navigate(`/drill-runs/${x.id}`);
    } catch (er) { toastErr(er); b.disabled = false; }
  });
  on(el, 'click', '[data-more]', (e, b) => {
    e.stopPropagation();
    const d = byId(b.dataset.more).drill;
    dropdown(b, [
      { label: 'Edit', icon: 'edit', onClick: () => navigate(`/drills/${d.id}/edit`) },
      { label: 'Backups of the job', icon: 'archive', onClick: () => navigate(`/runs?job_id=${d.job_id}`) },
      { label: 'Delete drill', icon: 'trash', danger: true, onClick: async () => {
        if (!(await confirmBox({ title: 'Delete drill', message: html`Delete the drill <b>${d.name}</b>? Its results are kept.`, danger: true, okText: 'Delete' }))) return;
        try { await del(`/drills/${d.id}`); toast('Drill deleted'); await load(); } catch (er) { toastErr(er); }
      } },
    ]);
  });
  on(el, 'click', '[data-page]', (e, b) => { f.offset = Math.max(0, +b.dataset.page); load(); });
  poll(ctx, load, 5000); // patch() skips while a menu is open
}
