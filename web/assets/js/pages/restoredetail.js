import { get, post } from '../api.js';
import { html, setHTML, patch, icon, on, fmtDate, fmtDur, runDuration, statusBadge, toast, toastErr, confirmBox, copyText } from '../ui.js';
import { poll, canOperate, setTitle } from '../app.js';
import { taskList, attachLog, backBtn } from '../components.js';

export const title = 'Restore details';

const OPT_LABELS = {
  location_id: 'Source location', target_server_id: 'Target server (id)', databases: 'Databases', rename: 'Rename', drop_existing: 'Drop target databases first',
  no_binlog: 'No binlog', fast_mode: 'Fast mode', restore_users: 'Restore users', threads: 'Threads', target_dir: 'Extraction directory',
  apply: 'Replace the data directory', service_name: 'Service', datadir: 'Data directory', move_back: 'Move-back', use_memory: 'use-memory', os_user: 'OS user',
  pitr: 'Point-in-time recovery', strip_definer: 'Remove DEFINER', convert_collations: 'Convert collations', skip_gtid_purged: 'Skip GTID_PURGED',
  no_owner: 'Skip ownership (--no-owner)', no_privileges: 'Skip privileges (--no-privileges)',
};

function fmtPITR(t) {
  switch (t.type) {
    case 'time': return `up to ${fmtDate(t.time, true)}`;
    case 'gtid': return `just before GTID ${t.gtid}`;
    case 'position': return `up to ${t.file}:${t.pos}`;
    case 'lsn': return `up to WAL position ${t.lsn}`;
    case 'name': return `up to restore point ${t.name}`;
    default: return 'latest available';
  }
}

function fmtOpt(v) {
  if (v && v.type && ['time', 'gtid', 'position', 'lsn', 'name', 'latest'].includes(v.type)) return fmtPITR(v);
  if (v === true) return 'yes';
  if (v === false) return 'no';
  if (Array.isArray(v)) return v.length ? v.join(', ') : 'all';
  if (v && typeof v === 'object') { const e = Object.entries(v); return e.length ? e.map(([a, b]) => `${a} → ${b}`).join(', ') : '—'; }
  return v === '' || v === 0 || v === null || v === undefined ? '—' : String(v);
}

export async function render(ctx) {
  const { el, params } = ctx;
  const id = +params.id;
  let data = await get(`/restores/${id}`);
  const live = () => data.restore.status === 'running';
  setTitle([{ label: 'Restores', href: '#/restores' }, `Restore #${id}`]);
  setHTML(el, html`<div id="detail"></div>
    <div class="card"><div class="card-head"><h2>Log</h2><button class="btn btn-sm btn-ghost" id="copy-log">${icon('copy')}Copy</button></div><div class="card-body"><pre class="log" id="log"></pre></div></div>`);
  const draw = () => {
    const { restore: x, task } = data;
    const opts = Object.entries(x.options || {}).filter(([k, v]) => OPT_LABELS[k] && fmtOpt(v) !== '—' && v !== false);
    patch(el.querySelector('#detail'), html`
      <div class="page-head">${backBtn('#/restores')}<div style="flex:1"><h1 class="h1-status">Restore #${x.id} ${statusBadge(x.status)}</h1><div class="sub">${x.job_name} → ${x.target_server_name || 'local machine'}</div></div>
        ${canOperate() && live() ? html`<button class="btn btn-danger" id="cancel">${icon('stop')}Cancel</button>` : ''}</div>
      ${x.error ? html`<div class="alert bad" style="margin-bottom:16px">${icon('alert')}<div>${x.error}</div></div>` : ''}
      ${x.status === 'success' ? html`<div class="alert good" style="margin-bottom:16px">${icon('check')}<div>Restore complete. Please verify the data on the target server.</div></div>` : ''}
      ${task ? html`<div class="card" style="margin-bottom:16px"><div class="card-head"><h2>Progress</h2></div>${taskList([task])}</div>` : ''}
      <div class="card" style="margin-bottom:16px"><div class="card-head"><h2>Details</h2></div><div class="card-body"><div class="kv">
        <div>Backup</div><div>${x.run_id ? html`<a href="#/runs/${x.run_id}">#${x.run_id} ${x.job_name}</a>` : x.job_name}</div>
        <div>Kind</div><div>${x.kind === 'physical' ? 'Physical' : 'Logical'}</div>
        <div>Target server</div><div>${x.target_server_name || '—'}</div>
        <div>User</div><div>${x.username}</div>
        <div>Time</div><div>${fmtDate(x.started_at, true)} → ${fmtDate(x.finished_at, true)} (${fmtDur(runDuration(x))})</div>
        ${opts.map(([k, v]) => html`<div>${OPT_LABELS[k]}</div><div>${fmtOpt(v)}</div>`)}
      </div></div></div>`);
  };
  draw();
  const logView = attachLog(ctx, el.querySelector('#log'), `/restores/${id}/log`, live);
  poll(ctx, async () => {
    if (!live()) return;
    data = await get(`/restores/${id}`);
    draw();
    if (!live()) {
      logView.reload();
      const st = data.restore.status;
      toast(st === 'success' ? `Restore #${id} finished successfully` : `Restore #${id} ended: ${st}`, st === 'success' ? 'good' : 'bad');
    }
  }, 3000);
  on(el, 'click', '#cancel', async () => {
    if (!(await confirmBox({ title: 'Cancel restore', message: 'Stop the running restore? The target database may be left partially restored.', danger: true, okText: 'Cancel restore' }))) return;
    try { await post(`/restores/${id}/cancel`); toast('Cancellation requested', 'info'); } catch (e) { toastErr(e); }
  });
  on(el, 'click', '#copy-log', () => copyText(el.querySelector('#log').textContent));
}
