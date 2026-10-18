import { get } from '../api.js';
import { html, setHTML, patch, on, icon, fmtBytes, fmtAgo, fmtDate, statusBadge, empty, debounce } from '../ui.js';
import { poll, canOperate } from '../app.js';
import { taskList, methodBadge, locationChips } from '../components.js';

export const title = 'Dashboard';

function roundedTop(x, y, w, h, r) {
  r = Math.min(r, h, w / 2);
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`;
}

function niceMax(v) {
  if (v <= 4) return 4;
  const step = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * step >= v) return m * step;
  return 10 * step;
}

function chart(daily, W = 680) {
  const H = 190, pl = 30, pr = 6, pt = 8, pb = 24;
  const n = daily.length;
  const max = niceMax(Math.max(1, ...daily.map((d) => d.success + d.failed)));
  const slot = (W - pl - pr) / n;
  const bw = Math.min(26, slot * 0.62);
  const y = (v) => pt + (H - pt - pb) * (1 - v / max);
  const ticks = [0, max / 2, max];
  const parts = [];
  for (const t of ticks) {
    parts.push(`<line class="${t === 0 ? 'axis-line' : 'grid-line'}" x1="${pl}" x2="${W - pr}" y1="${y(t)}" y2="${y(t)}"/>`);
    parts.push(`<text class="tick" x="${pl - 6}" y="${y(t) + 4}" text-anchor="end">${Math.round(t)}</text>`);
  }
  daily.forEach((d, i) => {
    const cx = pl + slot * i + slot / 2;
    const x = cx - bw / 2;
    const base = y(0);
    const hs = (H - pt - pb) * (d.success / max);
    const hf = (H - pt - pb) * (d.failed / max);
    const gap = d.success > 0 && d.failed > 0 ? 2 : 0;
    let g = `<g class="col" data-i="${i}">`;
    g += `<rect class="hit" x="${pl + slot * i}" y="${pt}" width="${slot}" height="${H - pt - pb}"/>`;
    if (d.success > 0) {
      g += d.failed > 0
        ? `<rect class="bar bar-good" x="${x}" y="${base - hs}" width="${bw}" height="${hs}"/>`
        : `<path class="bar bar-good" d="${roundedTop(x, base - hs, bw, hs, 4)}"/>`;
    }
    if (d.failed > 0) {
      g += `<path class="bar bar-bad" d="${roundedTop(x, base - hs - gap - hf, bw, hf, 4)}"/>`;
    }
    g += '</g>';
    parts.push(g);
    if (i % (W < 420 ? 3 : 2) === (n - 1) % (W < 420 ? 3 : 2)) {
      const [, m, dd] = d.day.split('-');
      parts.push(`<text class="tick" x="${cx}" y="${H - 6}" text-anchor="middle">${m}-${dd}</text>`);
    }
  });
  return `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Successful and failed backups over the last 14 days">${parts.join('')}</svg>`;
}

function chartTable(daily) {
  return html`<div class="table-wrap"><table class="table"><thead><tr><th>Date</th><th class="right">Success</th><th class="right">Failed / partial</th><th class="right">Size</th></tr></thead>
    <tbody>${daily.slice().reverse().map((d) => html`<tr><td>${d.day}</td><td class="right num">${d.success}</td><td class="right num">${d.failed}</td><td class="right num">${fmtBytes(d.bytes)}</td></tr>`)}</tbody></table></div>`;
}

const ONBOARD_KEY = 'dumpcat-onboarding-dismissed';

function onboardingCard(s, restores) {
  const steps = [
    { done: s.servers > 0, text: 'Add a database server', href: '#/servers', cta: 'Add server' },
    { done: s.storages > 0, text: 'Add a storage (local folder, S3, Google Drive, OneDrive, FTP…)', href: '#/storages', cta: 'Add storage' },
    { done: s.jobs > 0, text: 'Create a backup job with a schedule', href: '#/jobs/new', cta: 'New job' },
    { done: s.last_success_at > 0, text: 'Run the first successful backup', href: '#/jobs', cta: 'Run a job' },
    { done: restores > 0, text: 'Test a restore (into a new database name)', href: '#/runs', cta: 'Pick a backup' },
  ];
  const left = steps.filter((x) => !x.done).length;
  let dismissed = false;
  try { dismissed = localStorage.getItem(ONBOARD_KEY) === '1'; } catch { /* ignore */ }
  if (!left || dismissed) return '';
  const next = steps.find((x) => !x.done);
  return html`<div class="card" style="margin-bottom:16px"><div class="card-head">${icon('rocket')}<h2>Getting started</h2>
      <span class="muted small">${steps.length - left}/${steps.length} done</span>
      <button class="btn btn-sm btn-ghost" id="onboard-hide" title="Hide this checklist">Hide</button></div>
    <div class="card-body"><ol class="steps">${steps.map((x, i) => html`<li class="${x.done ? 'done' : ''}">
      <span class="step-dot" aria-hidden="true">${x.done ? icon('check') : i + 1}</span>
      <span class="step-text">${x.text}${x.done ? html`<span class="sr-only"> (done)</span>` : ''}</span>
      ${!x.done && x === next && (canOperate() || i >= 3) ? html`<a class="btn btn-sm btn-primary" href="${x.href}">${x.cta}</a>` : ''}</li>`)}</ol></div></div>`;
}

export async function render(ctx) {
  const { el } = ctx;
  let showTable = false;
  let lastDaily = null;

  setHTML(el, html`
    <div class="page-head"><div><h1>Overview</h1><div class="sub">Backup health across the whole system</div></div>
      ${canOperate() ? html`<a class="btn btn-primary" href="#/jobs/new">${icon('plus')}New backup job</a>` : ''}</div>
    <div id="d-warn"></div>
    <div id="d-onboard"></div>
    <div class="grid grid-5 stats" id="d-stats"></div>
    <div class="grid grid-2">
      <div class="card"><div class="card-head"><h2>Running</h2><span class="muted small">updates every 2 seconds</span></div><div id="tasks"></div></div>
      <div class="card"><div class="card-head"><h2>Backups, last 14 days</h2>
          <button class="btn btn-sm btn-ghost" id="tbl-toggle" aria-pressed="false">Show as table</button></div>
        <div class="card-body"><div class="legend" style="margin-bottom:8px"><span><i style="background:var(--good)"></i>Success</span><span><i style="background:var(--critical)"></i>Failed / partial</span></div><div id="chart-box"></div></div></div>
    </div>
    <div class="card"><div class="card-head"><h2>Job status</h2><a class="btn btn-sm" href="#/jobs">Manage jobs</a></div><div class="table-wrap" id="d-jobs"></div></div>
    <div class="card"><div class="card-head"><h2>Recent activity</h2><a class="btn btn-sm" href="#/runs">View all</a></div><div class="table-wrap" id="d-recent"></div></div>`);

  const draw = (d, restores) => {
    const s = d.stats;
    const lastOk = s.last_success_at ? new Date(s.last_success_at * 1000) : null;
    patch(el.querySelector('#d-warn'), d.warnings.length ? html`<div style="margin-bottom:16px">${d.warnings.map((w) => html`<div class="alert warn">${icon('alert')}<div>${w}</div></div>`)}</div>` : '');
    patch(el.querySelector('#d-onboard'), onboardingCard(s, restores));
    patch(el.querySelector('#d-stats'), html`
      <div class="card stat"><div class="label">${icon('clock')}Enabled jobs</div><div class="value num">${s.jobs_enabled}<span class="muted" style="font-size:15px"> / ${s.jobs}</span></div><div class="hint">${s.servers} ${s.servers === 1 ? 'server' : 'servers'} · ${s.storages} ${s.storages === 1 ? 'storage' : 'storages'}</div></div>
      <div class="card stat"><div class="label">${icon('archive')}Backups</div><div class="value num">${s.backups}</div><div class="hint">currently retained</div></div>
      <div class="card stat"><div class="label">${icon('hdd')}Stored size</div><div class="value num">${fmtBytes(s.stored_bytes)}</div><div class="hint">total across all storages</div></div>
      <div class="card stat"><div class="label">${icon('check')}Last 24 hours</div><div class="value num">${s.success_24h}<span class="muted" style="font-size:15px"> OK</span></div><div class="hint">${s.failed_24h > 0 ? html`<span style="color:var(--critical-ink)">${icon('alert')} ${s.failed_24h} failed</span>` : 'no failures'}</div></div>
      <div class="card stat"><div class="label">${icon('shield')}Last success</div><div class="value" style="font-size:18px;margin-top:8px">${lastOk ? fmtAgo(lastOk) : '—'}</div><div class="hint">${lastOk ? fmtDate(lastOk) : 'none yet'}</div></div>`);
    patch(el.querySelector('#d-jobs'), d.jobs.length ? html`<table class="table"><thead><tr><th>Job</th><th class="hide-sm">Server</th><th class="hide-sm">Method</th><th>Last run</th><th class="hide-sm">Last success</th><th>Next run</th></tr></thead><tbody>
      ${d.jobs.map((j) => html`<tr class="clickable">
        <td class="nowrap"><a class="row-link" href="#/runs?job_id=${j.id}"><b>${j.name}</b></a>${!j.enabled ? html` <span class="badge">disabled</span>` : ''}</td>
        <td class="hide-sm">${j.server_name}</td>
        <td class="hide-sm">${methodBadge(j.method)}</td>
        <td>${j.running ? statusBadge('running') : j.last_status ? html`${statusBadge(j.last_status)} <span class="sub">${fmtAgo(j.last_at)}</span>` : html`<span class="muted">never run</span>`}</td>
        <td class="hide-sm">${j.last_success_at ? fmtAgo(j.last_success_at) : html`<span class="muted">—</span>`}</td>
        <td>${j.next_at ? html`${fmtDate(j.next_at)}${j.next_type === 'incremental' ? html` <span class="badge info">incr</span>` : ''}` : html`<span class="muted">manual</span>`}</td>
      </tr>`)}</tbody></table>` : empty('No backup jobs yet', 'clock', canOperate() ? { label: 'Create a job', href: '#/jobs/new' } : null));
    patch(el.querySelector('#d-recent'), d.recent.length ? html`<table class="table"><thead><tr><th>Backup</th><th>Job</th><th class="hide-sm">Type</th><th class="right hide-sm">Size</th><th>Status</th><th class="hide-md">Stored at</th></tr></thead><tbody>
      ${d.recent.map((r) => html`<tr class="clickable">
        <td class="nowrap"><a class="row-link" href="#/runs/${r.id}"><span class="muted num">#${r.id}</span> ${fmtDate(r.started_at)}</a></td><td class="nowrap">${r.job_name}</td>
        <td class="hide-sm">${methodBadge(r.method, r.type)}</td><td class="right num hide-sm">${r.size_bytes ? fmtBytes(r.size_bytes) : '—'}</td>
        <td>${statusBadge(r.status)}</td><td class="hide-md">${locationChips(r.locations)}</td></tr>`)}</tbody></table>` : empty('No backups yet'));
    patch(el.querySelector('#tasks'), taskList(d.tasks));
    const key = JSON.stringify(d.daily);
    if (key !== lastDaily) { lastDaily = key; drawChart(d.daily); }
  };

  let daily = [];
  const drawChart = (dd) => {
    if (dd) daily = dd;
    const box = el.querySelector('#chart-box');
    if (!box) return;
    if (showTable) { setHTML(box, chartTable(daily)); return; }
    // draw at the real pixel width so tick labels keep their font size
    box.innerHTML = `<div class="chart">${chart(daily, Math.max(300, box.clientWidth || 600))}<div class="tooltip hidden"></div></div>`;
    const wrap = box.querySelector('.chart');
    const tip = box.querySelector('.tooltip');
    wrap.addEventListener('mousemove', (e) => {
      const g = e.target.closest('g.col');
      if (!g) { tip.classList.add('hidden'); return; }
      const d = daily[+g.dataset.i];
      const r = g.querySelector('.hit').getBoundingClientRect();
      const wr = wrap.getBoundingClientRect();
      setHTML(tip, html`<b>${d.day}</b>
        <div class="row"><span><i class="dot good"></i> Success</span><b class="num">${d.success}</b></div>
        <div class="row"><span><i class="dot bad"></i> Failed / partial</span><b class="num">${d.failed}</b></div>
        <div class="row"><span>Size</span><b class="num">${fmtBytes(d.bytes)}</b></div>`);
      tip.style.left = (r.left - wr.left + r.width / 2) + 'px';
      tip.style.top = '24px';
      tip.classList.remove('hidden');
    });
    wrap.addEventListener('mouseleave', () => tip.classList.add('hidden'));
  };

  el.querySelector('#tbl-toggle').onclick = (e) => {
    showTable = !showTable;
    e.currentTarget.textContent = showTable ? 'Show as chart' : 'Show as table';
    e.currentTarget.setAttribute('aria-pressed', String(showTable));
    drawChart();
  };
  on(el, 'click', '#onboard-hide', () => {
    try { localStorage.setItem(ONBOARD_KEY, '1'); } catch { /* ignore */ }
    patch(el.querySelector('#d-onboard'), '');
  });
  const chartBox = el.querySelector('#chart-box');
  let lastW = 0;
  const ro = new ResizeObserver(debounce(() => {
    const w = chartBox.clientWidth;
    if (Math.abs(w - lastW) > 8) { lastW = w; drawChart(); }
  }, 150));
  ro.observe(chartBox);
  ctx.cleanup(() => ro.disconnect());

  const restoresCount = async (s) => {
    if (s.servers && s.storages && s.jobs && s.last_success_at) {
      try { return (await get('/restores?limit=1')).total; } catch { return 0; }
    }
    return 0;
  };
  const d = await get('/dashboard');
  draw(d, await restoresCount(d.stats));
  lastW = chartBox.clientWidth;
  poll(ctx, async () => {
    patch(el.querySelector('#tasks'), taskList(await get('/activity')));
  }, 2000);
  poll(ctx, async () => {
    const nd = await get('/dashboard');
    draw(nd, await restoresCount(nd.stats));
  }, 20000);
}
