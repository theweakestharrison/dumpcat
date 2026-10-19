// Shared view components.
import { html, icon, fmtBytes, fmtDur, fmtDate, toDate, statusBadge, esc, raw } from './ui.js';

export const PHASE = {
  queued: 'Waiting for a slot', prepare: 'Preparing', dump: 'Dumping data', upload: 'Uploading',
  retention: 'Pruning old backups', verify: 'Verifying', download: 'Downloading & decompressing', restore: 'Restoring',
  prepare_physical: 'Prepare', apply: 'Applying to datadir',
  binlog_download: 'Fetching binlogs', binlog_apply: 'Replaying binlogs',
  check: 'Checking the restored copy', cleanup: 'Dropping the temporary databases',
};

export function taskLink(t) {
  if (t.kind === 'restore') return `#/restores/${t.id}`;
  if (t.kind === 'drill') return `#/drill-runs/${t.id}`;
  return `#/runs/${t.id}`;
}

export function taskList(tasks) {
  if (!tasks || !tasks.length) return html`<div class="empty">${icon('check')}No tasks running</div>`;
  return tasks.map((t) => {
    const pct = Math.max(0, Math.min(100, t.percent || 0));
    const indet = !(pct > 0);
    const kind = { backup: 'Backup', restore: 'Restore', verify: 'Verify', copy: 'Copy', drill: 'Drill' }[t.kind] || t.kind;
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

/** Result of the last restore drill of a backup (lists of backups). */
export function drillBadge(st, msg) {
  if (st === 'passed') return html`<span class="badge good" title="${msg || 'A restore drill restored this backup and its checks passed'}">${icon('flask')}Restore-tested</span>`;
  if (st === 'failed') return html`<span class="badge bad" title="${msg || ''}">${icon('flask')}Drill failed</span>`;
  return '';
}

export function methodBadge(m, type) {
  const label = m === 'physical' ? 'Physical' : m;
  return html`<span class="badge outline">${label}</span>${type === 'incremental' ? html` <span class="badge info">incr</span>` : ''}`;
}

// What an online agent reported about physical backups on its host:
// { ok, short, detail } (null while it is offline).
export function agentPhysical(st) {
  if (!st || !st.online) return null;
  const p = st.caps && st.caps.physical;
  if (!p) return { ok: false, short: 'Update the agent', detail: `DumpCat ${st.version} on the agent cannot run physical backups` };
  if (!p.allowed) return { ok: false, off: true, short: 'Off', detail: 'Start the agent with --allow-physical to run mariabackup/xtrabackup on this host' };
  const tools = (p.tools || []).filter((t) => t.found).map((t) => `${t.name} ${(/\d+\.\d+\.\d+/.exec(t.version) || [''])[0]}`.trim());
  if (!tools.length) return { ok: false, short: 'No backup tool', detail: `Install mariabackup (MariaDB) or Percona XtraBackup (MySQL) on ${st.hostname || 'the database host'}` };
  if (p.datadir_error) return { ok: false, short: 'Datadir not readable', detail: p.datadir_error };
  return { ok: true, short: tools.join(', '), detail: [p.user && `runs as ${p.user}`, p.datadir && `datadir ${p.datadir} readable`].filter(Boolean).join(' · ') };
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
 * How DumpCat reaches a database host that accepts no inbound connections
 * (kind 'agent' or 'wireguard'): an illustration of the two machines, the
 * firewall between them and the tunnel the database host opens. A wide and a
 * tall drawing; CSS shows the one that fits the card.
 */
let topoSeq = 0;
const TOPO = {
  agent: {
    name: 'Agent',
    endpoint: ['Agent endpoint', 'HTTPS · web UI port', 'link'],
    client: ['dumpcat agent', 'Allow list 127.0.0.1:3306', 'link'],
    local: '127.0.0.1:3306',
    db: 'Unchanged · 127.0.0.1 only',
    pipe: 'HTTPS · opened by the database host',
    pipeShort: ['HTTPS', 'opened by the database host'],
    reach: 'Reachable over HTTPS',
  },
  wireguard: {
    name: 'WireGuard',
    endpoint: ['WireGuard, built in', '10.66.0.1 · UDP 51820', 'shield'],
    client: ['wg-quick@dumpcat', '10.66.0.2 · keepalive 25 s', 'shield'],
    local: '10.66.0.2:3306',
    db: 'Listens on 10.66.0.2',
    pipe: 'WireGuard UDP · opened by the database host',
    pipeShort: ['WireGuard UDP', 'opened by the database host'],
    reach: 'UDP 51820 open',
  },
};

// an icon of the UI set, placed in an SVG drawing
const svgIcon = (name, x, y, size) => String(icon(name, 'tp-ico')).replace('width="16" height="16"', `x="${x}" y="${y}" width="${size}" height="${size}"`);

function topoChip(x, y, w, title, sub, ic, accent) {
  return `<rect class="tp-chip${accent ? ' accent' : ''}" x="${x}" y="${y}" width="${w}" height="54" rx="12"/>
    <rect class="tp-ico-bg" x="${x + 10}" y="${y + 12}" width="30" height="30" rx="8"/>${svgIcon(ic, x + 16, y + 18, 18)}
    <text class="tp-chip-t" x="${x + 50}" y="${y + 23}">${esc(title)}</text><text class="tp-chip-s" x="${x + 50}" y="${y + 40}">${esc(sub)}</text>`;
}

function topoDb(x, y, w, sub) {
  return `<rect class="tp-chip" x="${x}" y="${y}" width="${w}" height="54" rx="12"/>
    <g class="tp-cyl"><path d="M${x + 12} ${y + 15}v22c0 3.3 5.8 6 13 6s13-2.7 13-6v-22"/><ellipse cx="${x + 25}" cy="${y + 15}" rx="13" ry="5"/><path d="M${x + 12} ${y + 26}c0 3.3 5.8 6 13 6s13-2.7 13-6"/></g>
    <text class="tp-chip-t" x="${x + 50}" y="${y + 23}">MySQL / MariaDB</text><text class="tp-chip-s" x="${x + 50}" y="${y + 40}">${esc(sub)}</text>`;
}

const brick = (id) => `<pattern id="${id}" width="24" height="14" patternUnits="userSpaceOnUse">
  <rect class="tp-brick-bg" width="24" height="14"/><path class="tp-brick-line" d="M0 .7H24M0 7.7H24M6 .7V7.7M18 7.7V14"/></pattern>`;

// packets moving through the tunnel: green = the database host's side, blue = DumpCat's
function topoDots(from, to) {
  // until its animation begins a dot waits, hidden, at the start of its way
  const at = (p) => { const [x, y] = p.split(' '); return `cx="${x}" cy="${y}"`; };
  let out = '';
  for (let i = 0; i < 3; i++) {
    const b1 = `${(i * 0.9).toFixed(1)}s`;
    const b2 = `${(i * 0.9 + 0.45).toFixed(2)}s`;
    out += `<circle class="tp-dot out" r="4" ${at(from)} opacity="0"><set attributeName="opacity" to="1" begin="${b1}"/><animateMotion dur="2.7s" begin="${b1}" repeatCount="indefinite" path="M0 0L${to.split(' ')[0] - from.split(' ')[0]} ${to.split(' ')[1] - from.split(' ')[1]}"/></circle>`;
    out += `<circle class="tp-dot in" r="3" ${at(to)} opacity="0"><set attributeName="opacity" to="1" begin="${b2}"/><animateMotion dur="2.7s" begin="${b2}" repeatCount="indefinite" path="M0 0L${from.split(' ')[0] - to.split(' ')[0]} ${from.split(' ')[1] - to.split(' ')[1]}"/></circle>`;
  }
  return `<g class="tp-dots">${out}</g>`;
}

function topoWide(t, id) {
  return `<svg class="tp tp-h" viewBox="0 0 760 300" role="img" aria-label="${esc(t.name)}: the database host opens the connection to DumpCat through its firewall">
  <defs>${brick(id)}</defs>
  <rect class="tp-panel" x="12" y="24" width="228" height="252" rx="18"/>
  <image href="/demo/assets/favicon.svg" x="28" y="40" width="34" height="34"/>
  <text class="tp-title" x="74" y="55">DumpCat</text><text class="tp-sub" x="74" y="71">Backup server</text>
  ${topoChip(28, 92, 196, 'Backup & restore', 'mysqldump · mydumper', 'archive', false)}
  <path class="tp-conn" d="M126 146V168"/><text class="tp-mono" x="134" y="161">127.0.0.1:port</text>
  ${topoChip(28, 168, 196, t.endpoint[0], t.endpoint[1], t.endpoint[2], true)}
  <circle class="tp-ok-dot" cx="33" cy="252" r="3.5"/><text class="tp-foot" x="43" y="256">${esc(t.reach)}</text>

  <rect class="tp-panel" x="520" y="24" width="228" height="252" rx="18"/>
  <rect class="tp-ico-bg big" x="536" y="40" width="34" height="34" rx="9"/>${svgIcon('server', 544, 48, 18)}
  <text class="tp-title" x="582" y="55">Database host</text><text class="tp-sub" x="582" y="71">No inbound port</text>
  ${topoDb(536, 92, 196, t.db)}
  <path class="tp-conn" d="M634 146V168"/><text class="tp-mono" x="642" y="161">${esc(t.local)}</text>
  ${topoChip(536, 168, 196, t.client[0], t.client[1], t.client[2], true)}
  ${svgIcon('lock', 536, 245, 14)}<text class="tp-foot" x="556" y="256">Nothing opened on this host</text>

  <text class="tp-wall-t" x="380" y="30" text-anchor="middle">Firewall / NAT</text>
  <rect class="tp-wall" x="362" y="38" width="36" height="226" rx="6" fill="url(#${id})"/>
  <rect class="tp-pipe" x="222" y="185" width="316" height="20" rx="10"/>
  <rect class="tp-rim" x="357" y="180" width="7" height="30" rx="3.5"/><rect class="tp-rim" x="396" y="180" width="7" height="30" rx="3.5"/>
  ${topoDots('530 195', '230 195')}
  <rect class="tp-pill" x="252" y="148" width="256" height="24" rx="12"/>
  <text class="tp-pill-t" x="380" y="164" text-anchor="middle">← ${esc(t.pipe)}</text>

  <path class="tp-block" d="M240 244H346"/>
  <circle class="tp-x-bg" cx="356" cy="244" r="10"/><path class="tp-x" d="M352 240l8 8M360 240l-8 8"/>
  <text class="tp-block-t" x="293" y="236" text-anchor="middle">3306 inbound</text>
  <text class="tp-block-s" x="293" y="262" text-anchor="middle">blocked, not needed</text>
</svg>`;
}

function topoTall(t, id) {
  return `<svg class="tp tp-v" viewBox="0 0 340 640" role="img" aria-label="${esc(t.name)}: the database host opens the connection to DumpCat through its firewall">
  <defs>${brick(id)}</defs>
  <rect class="tp-panel" x="10" y="10" width="320" height="240" rx="18"/>
  <image href="/demo/assets/favicon.svg" x="26" y="26" width="34" height="34"/>
  <text class="tp-title" x="72" y="41">DumpCat</text><text class="tp-sub" x="72" y="57">Backup server</text>
  ${topoChip(26, 78, 288, 'Backup & restore', 'mysqldump · mydumper', 'archive', false)}
  <path class="tp-conn" d="M120 132V154"/><text class="tp-mono" x="128" y="147">127.0.0.1:port</text>
  ${topoChip(26, 154, 288, t.endpoint[0], t.endpoint[1], t.endpoint[2], true)}
  <circle class="tp-ok-dot" cx="31" cy="232" r="3.5"/><text class="tp-foot" x="41" y="236">${esc(t.reach)}</text>

  <text class="tp-wall-t" x="318" y="350" text-anchor="end">Firewall / NAT</text>
  <rect class="tp-wall" x="20" y="298" width="300" height="32" rx="6" fill="url(#${id})"/>
  <rect class="tp-pipe" x="160" y="204" width="20" height="248" rx="10"/>
  <rect class="tp-rim" x="155" y="293" width="30" height="7" rx="3.5"/><rect class="tp-rim" x="155" y="328" width="30" height="7" rx="3.5"/>
  ${topoDots('170 446', '170 212')}
  <rect class="tp-pill" x="72" y="256" width="196" height="34" rx="12"/>
  <text class="tp-pill-t" x="170" y="270" text-anchor="middle">↑ ${esc(t.pipeShort[0])}</text>
  <text class="tp-pill-s" x="170" y="284" text-anchor="middle">${esc(t.pipeShort[1])}</text>
  <path class="tp-block" d="M44 250V288"/>
  <circle class="tp-x-bg" cx="44" cy="298" r="10"/><path class="tp-x" d="M40 294l8 8M48 294l-8 8"/>
  <text class="tp-block-t" x="24" y="352">3306 inbound</text><text class="tp-block-s" x="24" y="367">blocked, not needed</text>

  <rect class="tp-panel" x="10" y="380" width="320" height="250" rx="18"/>
  <rect class="tp-ico-bg big" x="26" y="396" width="34" height="34" rx="9"/>${svgIcon('server', 34, 404, 18)}
  <text class="tp-title" x="72" y="411">Database host</text><text class="tp-sub" x="72" y="427">No inbound port</text>
  ${topoChip(26, 448, 288, t.client[0], t.client[1], t.client[2], true)}
  <path class="tp-conn" d="M120 502V524"/><text class="tp-mono" x="128" y="517">${esc(t.local)}</text>
  ${topoDb(26, 524, 288, t.db)}
  ${svgIcon('lock', 26, 599, 14)}<text class="tp-foot" x="46" y="610">Nothing opened on this host</text>
</svg>`;
}

export function topology(kind) {
  const t = TOPO[kind] || TOPO.agent;
  const n = ++topoSeq;
  return raw(`<figure class="topo-canvas">${topoWide(t, `tp-brick-h${n}`)}${topoTall(t, `tp-brick-v${n}`)}
    <figcaption class="tp-legend"><span><i class="out"></i>The database host opens the tunnel</span><span><i class="in"></i>DumpCat's database traffic flows back inside it</span></figcaption></figure>`);
}

// ---------- restore drills ----------

/** Restore time (RTO) of a drill run. */
export function fmtRTO(x) {
  return x.restore_ms > 0 ? fmtDur(x.restore_ms / 1000) : '—';
}

/** Table of drill runs; cols picks the optional columns: drill, backup, sandbox. */
export function drillRunsTable(items, cols = {}) {
  const c = { drill: true, backup: true, sandbox: true, ...cols };
  return html`<div class="table-wrap"><table class="table"><thead><tr><th>Result</th>${c.drill ? html`<th>Drill</th>` : ''}${c.backup ? html`<th class="hide-sm">Backup</th>` : ''}${c.sandbox ? html`<th class="hide-md">Sandbox</th>` : ''}
    <th class="right hide-sm">Restore time</th><th>Status</th></tr></thead>
    <tbody>${items.map((x) => html`<tr class="clickable">
      <td class="nowrap"><a class="row-link" href="#/drill-runs/${x.id}"><span class="muted num">#${x.id}</span> ${fmtDate(x.started_at)}</a><div class="sub">${x.trigger === 'schedule' ? 'scheduled' : x.triggered_by || x.trigger}</div></td>
      ${c.drill ? html`<td><b>${x.drill_name}</b><div class="sub">${x.job_name}</div></td>` : ''}
      ${c.backup ? html`<td class="hide-sm nowrap">${x.run_id ? html`<a href="#/runs/${x.run_id}">#${x.run_id}</a> <span class="muted small">${x.run_dir}</span>` : html`<span class="muted">${x.run_dir || '—'}</span>`}</td>` : ''}
      ${c.sandbox ? html`<td class="hide-md">${x.target_server_name}</td>` : ''}
      <td class="right num hide-sm">${fmtRTO(x)}</td>
      <td>${statusBadge(x.status)}${x.error || x.summary ? html`<span class="sub cell-ellipsis" title="${x.error || x.summary}">${(x.error || x.summary).split('\n')[0]}</span>` : ''}</td></tr>`)}</tbody></table></div>`;
}
