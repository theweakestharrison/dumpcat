import { get, post, del } from '../api.js';
import { html, patch, icon, on, fmtAgo, fmtDate, statusBadge, empty, toast, toastErr, confirmBox, describeCron, dropdown, modal } from '../ui.js';
import { poll, canOperate, navigate } from '../app.js';
import { methodBadge } from '../components.js';

export const title = 'Backup jobs';

export async function render(ctx) {
  const { el } = ctx;
  const draw = (list) => {
    patch(el, html`
      <div class="page-head"><div><h1>Backup jobs</h1><div class="sub">Scheduled backups: which server, how, where to store, and how long to keep</div></div>
        ${canOperate() ? html`<a class="btn btn-primary" href="#/jobs/new">${icon('plus')}New job</a>` : ''}</div>
      <div class="card"><div class="table-wrap">${list.length ? html`<table class="table">
        <thead><tr><th>Job name</th><th class="hide-sm">Server</th><th class="hide-sm">Method</th><th class="hide-sm">Schedule</th><th class="hide-md">Stored at</th><th>Last run</th><th class="hide-sm">Next run</th><th><span class="sr-only">Actions</span></th></tr></thead>
        <tbody>${list.map(({ job: j, last_run: lr, next_run: nr, running }) => html`<tr>
          <td style="min-width:170px"><a href="#/jobs/${j.id}/edit"><b>${j.name}</b></a>${!j.enabled ? html` <span class="badge">${icon('stop')}Disabled</span>` : ''}${j.options.binlog_archive ? html` <a class="badge info" href="#/binlogs" title="Continuous binlog archiving: point-in-time recovery">${icon('layers')}PITR</a>` : ''}
            <div class="sub">${j.encrypt ? html`${icon('lock')} encrypted · ` : ''}${j.compression}${j.retention.keep_last ? ` · keep ${j.retention.keep_last}` : ''}${j.retention.keep_days ? ` · ${j.retention.keep_days} days` : ''}</div></td>
          <td class="hide-sm">${j.server_name}</td>
          <td class="hide-sm">${methodBadge(j.method)}</td>
          <td class="hide-sm" style="min-width:160px"><div>${describeCron(j.schedule)}</div>${j.incr_schedule ? html`<div class="sub">Incr: ${describeCron(j.incr_schedule)}</div>` : ''}</td>
          <td class="hide-md"><span class="chips">${j.targets.map((t) => html`<span class="badge outline">${t.storage_name}</span>`)}</span></td>
          <td>${running ? statusBadge('running') : lr ? html`<a href="#/runs/${lr.id}">${statusBadge(lr.status)}</a><div class="sub">${fmtAgo(lr.started_at)}</div>` : html`<span class="muted">never run</span>`}</td>
          <td class="hide-sm">${nr && j.enabled ? html`${fmtDate(nr.at)}${nr.type === 'incremental' ? html` <span class="badge info">incr</span>` : ''}` : html`<span class="muted">—</span>`}</td>
          <td class="actions">${canOperate() ? html`
            <button class="btn btn-sm btn-primary" data-key="run-${j.id}" data-run="${j.id}" data-method="${j.method}" ${running ? 'disabled' : ''} title="Run now">${icon('play')}Run</button>
            <span class="dropdown"><button class="btn btn-sm btn-icon btn-ghost" data-key="more-${j.id}" data-more="${j.id}" title="More actions" aria-label="More actions for ${j.name}">${icon('dots')}</button></span>` : ''}
          </td></tr>`)}</tbody></table>` : empty('No backup jobs yet. A job defines which server to back up, how, where to store it and how long to keep it.', 'clock', canOperate() ? { label: 'Create your first job', href: '#/jobs/new' } : null)}</div></div>`);
  };
  let list = await get('/jobs');
  draw(list);
  const byId = (id) => list.find((x) => x.job.id === +id);

  const runJob = async (id, type) => {
    try {
      const run = await post(`/jobs/${id}/run`, { type });
      toast(`Started ${type === 'incremental' ? 'incremental' : 'full'} backup — #${run.id}`);
      navigate(`/runs/${run.id}`);
    } catch (e) { toastErr(e); }
  };

  on(el, 'click', '[data-run]', (e, b) => {
    if (b.dataset.method === 'physical') {
      e.stopPropagation();
      dropdown(b, [
        { label: 'Full backup', icon: 'database', onClick: () => runJob(b.dataset.run, 'full') },
        { label: 'Incremental backup', icon: 'layers', onClick: () => runJob(b.dataset.run, 'incremental') },
      ]);
      return;
    }
    runJob(b.dataset.run, 'full');
  });

  on(el, 'click', '[data-more]', (e, b) => {
    e.stopPropagation();
    const it = byId(b.dataset.more);
    const j = it.job;
    dropdown(b, [
      { label: 'Edit', icon: 'edit', onClick: () => navigate(`/jobs/${j.id}/edit`) },
      { label: 'Duplicate', icon: 'copy', onClick: () => navigate(`/jobs/new?clone=${j.id}`) },
      { label: 'View backups', icon: 'archive', onClick: () => navigate(`/runs?job_id=${j.id}`) },
      { label: j.enabled ? 'Pause schedule' : 'Enable schedule', icon: j.enabled ? 'stop' : 'play', onClick: async () => {
        try { await post(`/jobs/${j.id}/toggle`); toast(j.enabled ? 'Schedule paused' : 'Schedule enabled'); list = await get('/jobs'); draw(list); } catch (er) { toastErr(er); }
      } },
      { label: 'Delete job', icon: 'trash', danger: true, onClick: () => deleteJob(j) },
    ]);
  });

  const deleteJob = (j) => {
    const m = modal({
      title: 'Delete job',
      body: html`<p>Delete job <b>${j.name}</b>? Its schedule will stop.</p>
        <label class="check"><input type="checkbox" id="del-backups"><span>Also delete <b>all backups</b> of this job from every storage<span class="help">Unchecked: existing backups are kept and can still be restored from the "Backups" page.</span></span></label>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-danger-solid" id="do-del">Delete job</button>`,
    });
    m.el.querySelector('#do-del').onclick = async (e) => {
      const btn = e.currentTarget;
      const withBackups = m.el.querySelector('#del-backups').checked;
      if (withBackups && !(await confirmBox({ title: 'Delete all backups', message: html`This cannot be undone. Every backup of <b>${j.name}</b> will be deleted from storage.`, danger: true, okText: 'Delete permanently', typeText: j.name }))) return;
      btn.disabled = true;
      try {
        const r = await del(`/jobs/${j.id}${withBackups ? '?delete_backups=1' : ''}`);
        m.close();
        toast(withBackups ? `Job deleted along with ${r.deleted_backups} backups` : 'Job deleted');
        if (r.errors && r.errors.length) toast('Some backups could not be deleted: ' + r.errors.join('; '), 'bad', 10000);
        list = await get('/jobs');
        draw(list);
      } catch (er) { toastErr(er); btn.disabled = false; }
    };
  };

  poll(ctx, async () => { list = await get('/jobs'); draw(list); }, 10000); // patch() skips while a menu/field is in use
}
