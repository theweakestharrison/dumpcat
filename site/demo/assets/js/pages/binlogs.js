import { get, post } from '../api.js';
import { html, setHTML, icon, on, fmtBytes, fmtDate, fmtAgo, fmtDur, toast, toastErr, empty, modal, pager, copyText } from '../ui.js';
import { poll, canOperate } from '../app.js';
import { logHTML, logInfo } from '../components.js';

export const title = 'Log archiving & PITR';

// "binlog" / "WAL segment", for buttons and messages
const fileWord = (L) => L.file || L.name;

const STATE = {
  streaming: ['good', 'check', 'Streaming'],
  starting: ['info', 'loader', 'Starting'],
  retrying: ['bad', 'alert', 'Retrying'],
  stopped: ['', 'stop', 'Stopped'],
  off: ['', 'stop', 'Archiving off'],
};

function stateBadge(st) {
  const [cls, ic, label] = STATE[st] || ['', 'info', st];
  return html`<span class="badge ${cls}">${icon(ic, ic === 'loader' ? 'spin' : '')}${label}</span>`;
}

function lagText(s) {
  if (!s || !s.checked_at) return html`<span class="muted">—</span>`;
  if (!s.lag_bytes) return html`<span class="badge good">${icon('check')}caught up</span> <span class="muted small">checked ${fmtAgo(s.checked_at)}</span>`;
  return html`<b class="num">${fmtBytes(s.lag_bytes)}</b> behind${s.lag_seconds ? html` · ${fmtDur(s.lag_seconds)}` : ''}`;
}

function card(it) {
  const s = it.status;
  const st = it.stats || {};
  const L = logInfo(it.log);
  const word = fileWord(L);
  const state = s ? s.state : (it.archive && it.enabled ? 'stopped' : 'off');
  return html`<div class="card"><div class="card-head">
      <h2><a href="#/jobs/${it.job_id}/edit">${it.job_name}</a> <span class="muted" style="font-weight:500">· ${it.server_name}</span> <span class="badge outline">${L.name}</span></h2>
      ${stateBadge(state)}
    </div><div class="card-body">
      ${s && s.error ? html`<div class="alert bad" style="margin-bottom:12px">${icon('alert')}<div>${s.error}${s.error_at ? html` <span class="muted small">(${fmtAgo(s.error_at)})</span>` : ''}${s.next_try ? html`<div class="small">Next attempt ${fmtAgo(s.next_try)}</div>` : ''}</div></div>` : ''}
      ${s && s.upload_error ? html`<div class="alert warn" style="margin-bottom:12px">${icon('alert')}<div><b>Upload:</b> ${s.upload_error}</div></div>` : ''}
      <div class="kv">
        ${s ? html`
          <div>Receiving</div><div class="mono small">${s.current_file || '—'}${s.received_bytes ? html` · ${fmtBytes(s.received_bytes)}` : ''}${s.received_at ? html` <span class="muted">· last event ${fmtDate(s.received_at, true)}</span>` : ''}</div>
          <div>Server position</div><div class="mono small">${s.server_position || '—'}</div>
          <div>Lag</div><div>${lagText(s)}</div>
          <div>Waiting for upload</div><div>${s.pending_files ? html`<b>${s.pending_files}</b> file(s) · ${fmtBytes(s.pending_bytes)}` : html`<span class="muted">none</span>`}</div>
          ${s.slot ? html`<div>Replication slot</div><div><span class="mono small">${s.slot}</span>${s.slot_retained_bytes ? html` <span class="muted small">· the server keeps ${fmtBytes(s.slot_retained_bytes)} of WAL for it</span>` : ''}</div>` : ''}
          <div>Last archived</div><div>${s.last_archived ? html`<span class="mono small">${s.last_archived}</span> ${s.archived_at ? html`<span class="muted small">${fmtAgo(s.archived_at)}</span>` : ''}` : html`<span class="muted">—</span>`}</div>
          <div>Streaming since</div><div>${s.since ? html`${fmtDate(s.since, true)}${s.restarts ? html` <span class="muted small">· ${s.restarts} restart(s)</span>` : ''}` : '—'}</div>` : ''}
        <div>Archive</div><div>${st.files ? html`<b class="num">${st.files}</b> file(s) · ${fmtBytes(st.raw_bytes)} ${L.name} → ${fmtBytes(st.stored_bytes)} stored` : html`<span class="muted">nothing archived yet</span>`}</div>
        <div>Events covered</div><div>${st.first ? html`${fmtDate(st.first, true)} → ${fmtDate(st.last, true)}` : '—'}</div>
        <div>Close ${word}</div><div>${it.rotate_minutes ? `every ${it.rotate_minutes} min when it holds transactions` : `only when the server ${L.name === 'WAL' ? 'fills it' : 'rotates it'}`}</div>
      </div>
      <div class="btn-group" style="margin-top:14px">
        ${canOperate() ? html`<a class="btn btn-primary btn-sm" href="#/pitr?job_id=${it.job_id}">${icon('restore')}Restore to a point in time</a>` : ''}
        <button class="btn btn-sm" data-files="${it.job_id}">${icon('list')}Files</button>
        <button class="btn btn-sm" data-log="${it.job_id}">${icon('file')}Log</button>
        ${canOperate() && s ? html`<button class="btn btn-sm" data-rotate="${it.job_id}" title="${L.close}: the current ${word} is closed and uploaded now">${icon('upload')}Close ${L.name === 'WAL' ? 'segment' : 'binlog'} now</button>
          <button class="btn btn-sm btn-ghost" data-restart="${it.job_id}">${icon('refresh')}Restart</button>` : ''}
      </div>
    </div></div>`;
}

// what a WAL segment holds besides its times: LSN range, restore points, timeline files
function walNote(f) {
  const m = f.meta || {};
  if (m.history) return html`<div class="sub">timeline ${m.tli} history</div>`;
  const pts = m.points || [];
  return html`${m.start ? html`<div class="sub">${m.start} → ${m.end}${m.partial ? ' · partial' : ''}</div>` : ''}${pts.map((p) => html`<div class="sub">${icon('check')} restore point <code>${p.name}</code></div>`)}`;
}

function filesModal(jobId, name, L) {
  const lim = 50;
  let offset = 0;
  const m = modal({ title: `Archived ${L.files}: ${name}`, size: 'xl', body: html`<div id="bl-files"><div class="loading-page">${icon('loader', 'spin')}</div></div>`, footer: html`<button class="btn" data-close>Close</button>` });
  const load = async () => {
    const box = m.el.querySelector('#bl-files');
    try {
      const res = await get(`/jobs/${jobId}/binlogs?limit=${lim}&offset=${offset}`);
      setHTML(box, res.items.length ? html`<div class="table-wrap"><table class="table"><thead><tr><th>File</th><th>Events</th><th class="right">Size</th><th>Stored on</th><th class="hide-sm">Archived</th></tr></thead><tbody>
        ${res.items.map((f) => html`<tr><td class="mono small">${f.file_name}<div class="sub">${f.encrypted ? html`${icon('lock')} ` : ''}${f.compression}</div>${L.name === 'WAL' ? walNote(f) : ''}</td>
          <td class="small nowrap">${f.first_ts ? html`${fmtDate(f.first_ts, true)}<div class="sub">→ ${fmtDate(f.last_ts, true)}</div>` : html`<span class="muted">no commit</span>`}</td>
          <td class="right num">${fmtBytes(f.size_bytes)}<div class="sub">${fmtBytes(f.stored_bytes)}</div></td>
          <td><span class="chips">${f.locations.map((l) => html`<span class="badge ${l.status === 'ok' ? 'good' : 'bad'}" title="${l.error || f.path}">${l.storage_name}</span>`)}</span></td>
          <td class="small hide-sm">${fmtDate(f.created_at, true)}</td></tr>`)}</tbody></table></div>${pager(res.total, lim, offset)}`
        : empty(`No archived ${L.files} yet.`, 'layers'));
    } catch (e) { setHTML(box, html`<div class="alert bad">${icon('alert')}<div>${e.message}</div></div>`); }
  };
  on(m.el, 'click', '[data-page]', (e, b) => { offset = Math.max(0, +b.dataset.page); load(); });
  load();
}

function logModal(ctx, jobId, name, L) {
  let open = true;
  const m = modal({ title: `${L.name === 'WAL' ? 'WAL' : 'Binlog'} archiving log: ${name}`, size: 'xl', body: html`<pre class="log" id="bl-log" style="max-height:60vh"></pre>`,
    footer: html`<button class="btn btn-ghost" id="bl-copy">${icon('copy')}Copy</button><span style="flex:1"></span><button class="btn" data-close>Close</button>`,
    onClose: () => { open = false; } });
  const pre = m.el.querySelector('#bl-log');
  let text = '';
  let offset = -200 * 1024;
  const load = async () => {
    const r = await get(`/jobs/${jobId}/binlog/log?offset=${offset}`);
    if (r.data) {
      text = (text + r.data).slice(-1_000_000);
      const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 40;
      pre.innerHTML = logHTML(text).s || '';
      if (atBottom) pre.scrollTop = pre.scrollHeight;
    } else if (!text) pre.textContent = '(no log yet)';
    offset = r.next;
  };
  load().then(() => { pre.scrollTop = pre.scrollHeight; }).catch(() => {});
  const t = setInterval(() => { if (!open || !ctx.alive()) return clearInterval(t); load().catch(() => {}); }, 3000);
  m.el.querySelector('#bl-copy').onclick = () => copyText(pre.textContent);
  ctx.cleanup(() => { open = false; clearInterval(t); m.close(); });
}

export async function render(ctx) {
  const { el } = ctx;
  let list = await get('/binlog');
  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>Log archiving &amp; point-in-time recovery</h1>
        <div class="sub">The server's transaction log (MySQL binary logs, PostgreSQL WAL) is streamed continuously and archived next to the backups, so a restore can stop at any moment — not just when a backup ran.</div></div>
        ${canOperate() ? html`<a class="btn btn-primary" href="#/pitr">${icon('restore')}Restore to a point in time</a>` : ''}</div>
      ${list.length ? list.map(card) : html`<div class="card">${empty(html`No job archives its server's log yet. Edit a job and turn on <b>continuous log archiving</b> (section "Log archiving &amp; point-in-time recovery"): binlogs for MySQL / MariaDB jobs, WAL for PostgreSQL <code>pg_basebackup</code> jobs.`, 'layers')}</div>`}`);
  };
  draw();
  const byId = (id) => list.find((x) => x.job_id === +id);
  const logOf = (id) => logInfo(byId(id).log);
  on(el, 'click', '[data-files]', (e, b) => filesModal(+b.dataset.files, byId(b.dataset.files).job_name, logOf(b.dataset.files)));
  on(el, 'click', '[data-log]', (e, b) => logModal(ctx, +b.dataset.log, byId(b.dataset.log).job_name, logOf(b.dataset.log)));
  on(el, 'click', '[data-rotate]', async (e, b) => {
    const word = fileWord(logOf(b.dataset.rotate));
    try { await post(`/jobs/${b.dataset.rotate}/binlog/rotate`); toast(`The server was asked to close the current ${word} — it is uploaded within a few seconds`, 'info'); } catch (er) { toastErr(er); }
  });
  on(el, 'click', '[data-restart]', async (e, b) => {
    try { await post(`/jobs/${b.dataset.restart}/binlog/restart`); toast(`Restarting the ${logOf(b.dataset.restart).name} stream`, 'info'); } catch (er) { toastErr(er); }
  });
  poll(ctx, async () => {
    list = await get('/binlog');
    if (!document.querySelector('.modal-backdrop')) {
      const sc = window.scrollY;
      draw();
      window.scrollTo(0, sc);
    }
  }, 5000);
}
