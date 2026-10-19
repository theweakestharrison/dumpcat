// Shared view components.
import { html, icon, fmtBytes, fmtDur, fmtDate, toDate, statusBadge, esc, raw } from './ui.js';

export const PHASE = {
  queued: 'Waiting for a slot', prepare: 'Preparing', dump: 'Dumping data', upload: 'Uploading',
  retention: 'Pruning old backups', verify: 'Verifying', download: 'Downloading & decompressing', restore: 'Restoring',
  prepare_physical: 'Prepare', apply: 'Applying to datadir',
  binlog_download: 'Fetching binlogs', binlog_apply: 'Replaying binlogs',
};

export function taskLink(t) {
  if (t.kind === 'restore') return `#/restores/${t.id}`;
  return `#/runs/${t.id}`;
}

export function taskList(tasks) {
  if (!tasks || !tasks.length) return html`<div class="empty">${icon('check')}No tasks running</div>`;
  return tasks.map((t) => {
    const pct = Math.max(0, Math.min(100, t.percent || 0));
    const indet = !(pct > 0);
    const kind = { backup: 'Backup', restore: 'Restore', verify: 'Verify', copy: 'Copy' }[t.kind] || t.kind;
    const elapsed = (Date.now() - toDate(t.started_at).getTime()) / 1000;
    return html`<div class="task">
      <div class="task-top">
        <span class="badge info">${icon('loader', 'spin')}${kind}</span>
        <a class="name" href="${taskLink(t)}">${t.title}</a>
        <span class="muted small">${PHASE[t.phase] || t.phase}${pct > 0 ? html` · <b class="num">${pct.toFixed(0)}%</b>` : ''}</span>
      </div>
      <div class="progress ${indet ? 'indeterminate' : ''}"><div style="width:${pct}%"></div></div>
      <div class="task-meta">
        <span>Processed: <b class="num">${fmtBytes(t.raw_bytes)}</b>${t.estimate_bytes > 0 ? html` / ~${fmtBytes(t.estimate_bytes)}` : ''}</span>
        ${t.out_bytes > 0 ? html`<span>Output: <b class="num">${fmtBytes(t.out_bytes)}</b></span>` : ''}
        ${t.rate > 0 ? html`<span>Speed: <b class="num">${fmtBytes(t.rate)}/s</b></span>` : ''}
        <span>Elapsed: ${fmtDur(elapsed)}</span>
      </div>
    </div>`;
  });
}

export function locationChips(locs) {
  if (!locs || !locs.length) return html`<span class="muted">—</span>`;
  return html`<span class="chips">${locs.map((l) => {
    // healthy copies stay neutral; only problems draw attention
    const cls = l.status === 'ok' ? 'outline' : l.status === 'failed' ? 'bad' : 'info';
    const ic = l.status === 'ok' ? 'check' : l.status === 'failed' ? 'x' : 'loader';
    return html`<span class="badge ${cls}" title="${l.storage_name}: ${l.status}${l.error ? ' — ' + l.error : ''}">${icon(ic, ic === 'loader' ? 'spin' : '')}${l.storage_name}</span>`;
  })}</span>`;
}

export function methodBadge(m, type) {
  const label = m === 'physical' ? 'Physical' : m;
  return html`<span class="badge outline">${label}</span>${type === 'incremental' ? html` <span class="badge info">incr</span>` : ''}`;
}

export function storageIcon(type) {
  return { local: 'hdd', s3: 'archive', gdrive: 'folder', onedrive: 'folder', ftp: 'server', sftp: 'lock', webdav: 'link', b2: 'archive', azureblob: 'archive', dropbox: 'folder', rclone: 'layers' }[type] || 'cloud';
}

// Loose ASCII patterns for the status markers in logs written by older, Vietnamese-language versions.
const LEGACY_ERR = /TH.T B.I|L.I: /;
const LEGACY_WARN = /C.NH B.O|C.nh b.o/;
const LEGACY_OK = /TH.NH C.NG|K.T QU.: SUCCESS/;

/** Colorized log rendering (escaped). */
export function logHTML(text) {
  return raw(text.split('\n').map((ln) => {
    const e = esc(ln);
    if ((/FAILED|ERROR|\[ERROR\]|error:|failed|✗/i.test(ln) || LEGACY_ERR.test(ln)) && !/0 error/i.test(ln)) return `<span class="l-err">${e}</span>`;
    if (/WARNING|Warning|\[Warning\]/.test(ln) || LEGACY_WARN.test(ln)) return `<span class="l-warn">${e}</span>`;
    if (/ \$ /.test(ln)) return `<span class="l-cmd">${e}</span>`;
    if (/SUCCEEDED|RESULT: SUCCESS|completed OK|✓| OK\b/.test(ln) || LEGACY_OK.test(ln)) return `<span class="l-ok">${e}</span>`;
    return e;
  }).join('\n'));
}

/** Live log viewer that tails a log endpoint. */
export function attachLog(ctx, el, url, isLive) {
  let offset = 0;
  let text = '';
  let first = true;
  const load = async () => {
    const { get } = await import('./api.js');
    const r = await get(`${url}?offset=${offset}`);
    if (r.data) {
      text += r.data;
      if (text.length > 2_000_000) text = text.slice(-1_500_000);
      const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 40;
      el.innerHTML = logHTML(text).s || '';
      if (atBottom || first) el.scrollTop = el.scrollHeight;
    } else if (first && !text) {
      el.textContent = '(no log yet)';
    }
    first = false;
    offset = r.next;
  };
  load();
  let timer = setInterval(async () => {
    if (!ctx.alive()) return clearInterval(timer);
    if (document.hidden) return;
    if (isLive()) { try { await load(); } catch { /* ignore */ } }
  }, 2000);
  ctx.cleanup(() => clearInterval(timer));
  return { reload: load };
}

export function backBtn(href, label = 'Back') {
  return html`<a class="btn btn-ghost" href="${href}">${icon('back')}${label}</a>`;
}

// ---------- point-in-time recovery ----------

const pad2 = (n) => String(n).padStart(2, '0');

/** Value for <input type=datetime-local step=1> in the browser's time zone. */
export function localInput(d) {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}

export const PITR_TYPES = [
  ['time', 'Point in time'], ['gtid', 'Before a GTID'], ['position', 'Binlog position'], ['latest', 'Latest available'],
];

/** Inputs choosing where a point-in-time recovery stops. */
export function pitrTargetFields(t = {}) {
  const type = t.type || 'time';
  const when = t.time ? toDate(t.time) : new Date(Date.now() - 60000);
  return html`<div class="btn-group" style="margin-bottom:12px">${PITR_TYPES.map(([v, l]) => html`<label class="check" style="margin-right:16px"><input type="radio" name="pitr_type" value="${v}" ${type === v ? 'checked' : ''}><span>${l}</span></label>`)}</div>
    <div class="form-grid">
      <div class="field" data-pt="time"><label>Recover up to (your local time)</label><input type="datetime-local" step="1" name="pitr_time" value="${localInput(when)}">
        <div class="help">Transactions committed before this moment are replayed, later ones are not — never half a transaction.</div></div>
      <div class="field" data-pt="gtid"><label>Stop just before GTID</label><input type="text" name="pitr_gtid" value="${t.gtid || ''}" placeholder="0-1-1234 (MariaDB) or 3E11FA47-…:23 (MySQL)" spellcheck="false">
        <div class="help">That transaction (e.g. the accidental DROP TABLE) and everything after it are not replayed.</div></div>
      <div class="field" data-pt="position"><label>Binlog file</label><input type="text" name="pitr_file" value="${t.file || ''}" placeholder="mysql-bin.000123" spellcheck="false"></div>
      <div class="field" data-pt="position"><label>Stop position</label><input type="number" name="pitr_pos" min="4" value="${t.pos || ''}">
        <div class="help">Events starting at or after this position are not replayed.</div></div>
      <div class="field full" data-pt="latest"><div class="help">Replays every archived binlog, plus the binlog DumpCat is still receiving — the most recent state that can be recovered.</div></div>
    </div>`;
}

/** Shows the inputs of the selected target type. */
export function bindPITRFields(root) {
  const apply = () => {
    const type = (root.querySelector('input[name=pitr_type]:checked') || {}).value || 'time';
    root.querySelectorAll('[data-pt]').forEach((x) => x.classList.toggle('hidden', x.dataset.pt !== type));
  };
  root.addEventListener('change', (e) => { if (e.target.name === 'pitr_type') apply(); });
  apply();
}

/** Reads the target from the inputs (null when incomplete). */
export function readPITRTarget(root) {
  const v = (n) => (root.querySelector(`[name=${n}]`) || {}).value || '';
  const type = (root.querySelector('input[name=pitr_type]:checked') || {}).value || 'time';
  switch (type) {
    case 'time': {
      const d = v('pitr_time') ? new Date(v('pitr_time')) : null;
      return d && !isNaN(d) ? { type, time: d.toISOString() } : null;
    }
    case 'gtid': return v('pitr_gtid').trim() ? { type, gtid: v('pitr_gtid').trim() } : null;
    case 'position': return v('pitr_file').trim() && +v('pitr_pos') ? { type, file: v('pitr_file').trim(), pos: +v('pitr_pos') } : null;
    default: return { type: 'latest' };
  }
}

/** Renders the answer of POST /api/pitr/plan. */
export function pitrPlanView(p, runId) {
  if (!p) return '';
  const warn = (p.warnings || []).map((w) => html`<div class="alert warn" style="margin-top:8px">${icon('alert')}<div>${w}</div></div>`);
  if (!p.ok) {
    return html`<div class="alert bad">${icon('alert')}<div><b>Cannot recover to ${p.target}:</b> ${p.error}</div></div>${warn}`;
  }
  const b = p.base;
  const other = runId && b.id !== runId;
  return html`<div class="alert good">${icon('check')}<div>
      Recover to <b>${p.target}</b>: restore backup <a href="#/runs/${b.id}">#${b.id} ${b.dir_name}</a> (data up to ${fmtDate(b.consistent_at, true)}),
      then replay <b>${p.files}</b> archived binlog file(s)${p.partial ? html` + the file still being received (<code>${p.partial}</code>)` : ''} — ${fmtBytes(p.bytes)}
      from <code>${b.binlog_file}:${b.binlog_pos}</code>.${p.coverage ? html` Binlogs are complete up to ${fmtDate(p.coverage, true)}.` : ''}
    </div></div>
    ${other ? html`<div class="alert ${p.run_ok ? 'info' : 'warn'}" style="margin-top:8px">${icon('info')}<div>
      The newest backup before this point is <b>#${b.id} ${b.dir_name}</b>${p.run_ok ? ' — starting from it replays fewer binlogs.' : html`; this backup cannot be used: ${p.run_error}`}
      <button type="button" class="btn btn-sm" data-use-base="${b.id}" style="margin-left:8px">Use #${b.id}</button></div></div>` : ''}
    ${warn}`;
}

export { statusBadge };

/**
 * How DumpCat reaches a database host that accepts no inbound connections:
 * kind 'agent' or 'wireguard'. The connection is opened by the database host;
 * the database traffic then flows inside it.
 */
export function topology(kind) {
  const wgKind = kind === 'wireguard';
  const left = wgKind
    ? html`<div class="topo-box accent"><b>WireGuard, built in</b><small>10.66.0.1 · no network interface</small></div>`
    : html`<div class="topo-box accent"><b>Agent endpoint</b><small>/api/agent/ on the web UI port</small></div>`;
  const right = wgKind
    ? html`<div class="topo-box accent"><b>wg-quick@dumpcat</b><small>10.66.0.2 · keepalive 25 s</small></div>`
    : html`<div class="topo-box accent"><b>dumpcat agent</b><small>allow list: 127.0.0.1:3306</small></div>`;
  const toDb = wgKind ? '10.66.0.2:3306' : '127.0.0.1:3306';
  return html`<figure class="topo" aria-label="${wgKind ? 'WireGuard' : 'Agent'} connection diagram">
    <div class="topo-node">
      <div class="topo-title">${icon('server')}DumpCat <span>backup server</span></div>
      <div class="topo-box"><b>mysqldump · mydumper · mysqlbinlog</b><small>connect to 127.0.0.1:&lt;local port&gt;</small></div>
      <div class="topo-step">↓ tunnel</div>
      ${left}
      <div class="topo-open">Reachable from the database host: ${wgKind ? 'UDP 51820' : 'HTTPS'}</div>
    </div>
    <div class="topo-link">
      <div class="topo-lane ok"><span class="topo-line"></span><span class="topo-label"><b>${wgKind ? 'WireGuard · UDP 51820' : 'HTTPS · WebSocket'}</b>opened by the database host, outbound</span></div>
      <div class="topo-wall"><span>firewall / NAT</span></div>
      <div class="topo-lane no"><span class="topo-line"></span><span class="topo-label"><b>3306 inbound</b>blocked, and not needed</span></div>
    </div>
    <div class="topo-node">
      <div class="topo-title">${icon('database')}Database host <span>no inbound port</span></div>
      ${right}
      <div class="topo-step">↓ ${toDb}</div>
      <div class="topo-box"><b>MySQL / MariaDB</b><small>${wgKind ? 'listens on 10.66.0.2 (bind-address)' : 'unchanged, may listen on 127.0.0.1 only'}</small></div>
    </div>
  </figure>`;
}
