import { get, post } from '../api.js';
import { html, setHTML, icon, $, debounce, fmtDate, toDate } from '../ui.js';
import { navigate, canOperate } from '../app.js';
import { backBtn, pitrTargetFields, bindPITRFields, readPITRTarget, pitrPlanView, localInput } from '../components.js';

export const title = 'Point-in-time recovery';

const DAY = 86400000;

// Picks the recovery target first, on a timeline of the job's backups and archived binlogs; the newest
// backup before it is then restored through the normal restore form with the target pre-filled.
export async function render(ctx) {
  const { el, query } = ctx;
  if (!canOperate()) { setHTML(el, html`<div class="alert warn">${icon('lock')}<div>You don't have permission to restore.</div></div>`); return; }
  const jobs = await get('/binlog');
  let jobId = +query.job_id || (jobs[0] && jobs[0].job_id) || 0;
  setHTML(el, html`
    <div class="page-head">${backBtn('#/binlogs')}<div><h1>Restore to a point in time</h1>
      <div class="sub">Pick where the recovery stops. DumpCat restores the newest backup before that point, then replays the archived binlogs up to it.</div></div></div>
    ${jobs.length ? html`<form id="pitr-form" autocomplete="off">
      <div class="card"><div class="card-body">
        <div class="pt-head">
          <div class="field"><label>Backup job</label><select name="job_id">${jobs.map((j) => html`<option value="${j.job_id}" ${j.job_id === jobId ? 'selected' : ''}>${j.job_name} · ${j.server_name}</option>`)}</select></div>
          <div class="pt-facts" id="pt-facts"></div>
        </div>
        <div id="pt-tl"></div>
      </div></div>
      <div class="card" style="margin-top:16px"><div class="card-body">
        <div class="section-title">Stop the recovery</div>
        <div id="pitr-target">${pitrTargetFields({})}</div>
      </div></div>
      <div class="card" style="margin-top:16px"><div class="card-body">
        <div class="section-title">What DumpCat will do</div>
        <div id="pitr-plan"></div>
      </div></div>
      <div class="form-actions" style="margin-top:16px"><a class="btn" href="#/binlogs">Cancel</a><button class="btn btn-primary" type="submit" id="go" disabled>${icon('restore')}Continue to restore options</button></div>
    </form>`
      : html`<div class="card"><div class="empty">${icon('layers')}No job archives binlogs yet: turn on continuous binlog archiving in a job first.</div></div>`}`);
  if (!jobs.length) return;

  const form = $('#pitr-form', el);
  const tbox = $('#pitr-target', el);
  const timeInput = $('[name=pitr_time]', tbox);
  bindPITRFields(tbox);
  let plan = null;
  let target = null;
  let view = null; // { job, runs, from, to } of the timeline

  // ---- timeline: archived binlogs, backups and the recovery point ----
  const pct = (ms) => (view ? Math.max(0, Math.min(100, ((ms - view.from) / (view.to - view.from)) * 100)) : 0);
  const loadJob = async () => {
    const job = jobs.find((j) => j.job_id === jobId) || jobs[0];
    let runs = [];
    try { runs = (await get(`/runs?job_id=${job.job_id}&status=success&limit=300`)).items || []; } catch { /* the timeline shows binlogs only */ }
    if (!ctx.alive()) return;
    const st = job.stats || {};
    const live = job.status && job.status.state === 'streaming';
    const now = Date.now();
    const first = toDate(st.first);
    const last = live ? new Date(now) : toDate(st.last);
    const backups = runs.map((r) => ({ id: r.id, type: r.type, dir: r.dir_name, at: toDate(r.started_at) })).filter((b) => b.at).sort((a, b) => a.at - b.at);
    // the window: from the newest backup taken before the binlogs start (or the first binlog), at most 30 days
    let from = first ? first.getTime() : now - 7 * DAY;
    const before = backups.filter((b) => b.at.getTime() <= from).at(-1);
    if (before) from = before.at.getTime();
    from = Math.max(from, now - 30 * DAY);
    const min = Math.max(from, backups.length ? backups[0].at.getTime() : from, first ? first.getTime() : from);
    view = { job, from, to: now, min, max: last ? last.getTime() : now, backups, live, first, last };
    const inWindow = backups.filter((b) => b.at.getTime() >= from).length;
    setHTML($('#pt-facts', el), html`
      <span class="chip-fact">${icon('layers')}Binlogs ${first ? fmtDate(first) : '—'} → ${live ? html`<b class="live">now, streaming</b>` : (last ? fmtDate(last) : '—')}</span>
      <span class="chip-fact">${icon('archive')}${inWindow} backup${inWindow === 1 ? '' : 's'} in this window</span>`);
    drawTimeline();
  };

  const axis = () => {
    const n = 5;
    const out = [];
    for (let i = 0; i <= n; i++) {
      const d = new Date(view.from + ((view.to - view.from) * i) / n);
      const short = view.to - view.from > 2 * DAY ? `${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:00` : `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
      out.push(html`<span style="left:${(i / n) * 100}%">${short}</span>`);
    }
    return out;
  };

  const drawTimeline = () => {
    if (!view) return;
    const v = view;
    const streamL = pct(v.first ? v.first.getTime() : v.min);
    const streamR = pct(v.max);
    setHTML($('#pt-tl', el), html`<div class="pt-tl">
      <div class="pt-track">
        <div class="pt-off" style="width:${pct(v.min)}%" title="Before the first backup with binlogs after it: not recoverable"></div>
        <div class="pt-stream" style="left:${streamL}%;width:${Math.max(0, streamR - streamL)}%"></div>
        <div class="pt-replay hidden" id="pt-replay"></div>
        ${v.backups.filter((b) => b.at.getTime() >= v.from).map((b) => html`<span class="pt-bk ${b.type === 'incremental' ? 'incr' : 'full'}" data-run="${b.id}" style="left:${pct(b.at.getTime())}%" title="#${b.id} ${b.dir} · ${fmtDate(b.at, true)}"></span>`)}
        <div class="pt-pin hidden" id="pt-pin"><span id="pt-pin-label"></span></div>
        <input type="range" class="pt-range" id="pt-range" min="${v.min}" max="${v.max}" step="1000" aria-label="Recovery point">
      </div>
      <div class="pt-axis">${axis()}</div>
      <ul class="pt-legend">
        <li><i class="lg-full"></i>Full backup</li><li><i class="lg-incr"></i>Incremental</li>
        <li><i class="lg-stream"></i>Archived binlogs</li><li><i class="lg-replay"></i>Replayed on restore</li><li><i class="lg-pin"></i>Recovery point</li>
      </ul>
    </div>`);
    const range = $('#pt-range', el);
    range.addEventListener('input', () => {
      const typeTime = form.querySelector('input[name=pitr_type][value=time]');
      if (!typeTime.checked) { typeTime.checked = true; typeTime.dispatchEvent(new Event('change', { bubbles: true })); }
      timeInput.value = localInput(new Date(+range.value));
      placePin();
      recheck();
    });
    placePin();
    markPlan();
  };

  // the pin follows the target: the chosen time, the end for "latest", hidden for a GTID or a position
  const targetMs = () => {
    const t = readPITRTarget(tbox);
    if (!t || !view) return null;
    if (t.type === 'time') return new Date(t.time).getTime();
    if (t.type === 'latest') return view.max;
    return null;
  };
  const placePin = () => {
    const pin = $('#pt-pin', el);
    if (!pin) return;
    const ms = targetMs();
    pin.classList.toggle('hidden', ms === null);
    if (ms === null) return;
    const p = pct(ms);
    pin.style.left = p + '%';
    pin.classList.toggle('flip', p > 70);
    $('#pt-pin-label', el).textContent = fmtDate(new Date(ms), true);
    const range = $('#pt-range', el);
    if (range && document.activeElement !== range) range.value = String(Math.max(view.min, Math.min(view.max, ms)));
  };
  // the backup that is restored, and the binlogs replayed after it
  const markPlan = () => {
    if (!view) return;
    el.querySelectorAll('.pt-bk.base').forEach((x) => x.classList.remove('base'));
    const rep = $('#pt-replay', el);
    const ms = targetMs();
    if (!plan || !plan.ok || !rep) { if (rep) rep.classList.add('hidden'); return; }
    const tick = el.querySelector(`.pt-bk[data-run="${plan.base.id}"]`);
    if (tick) tick.classList.add('base');
    const a = pct(new Date(plan.base.consistent_at).getTime());
    const b = ms === null ? a : pct(ms);
    rep.classList.toggle('hidden', !(b > a));
    rep.style.left = a + '%';
    rep.style.width = Math.max(0, b - a) + '%';
  };

  const check = async () => {
    const box = $('#pitr-plan', el);
    target = readPITRTarget(tbox);
    $('#go', el).disabled = true;
    placePin();
    if (!target) { plan = null; markPlan(); setHTML(box, html`<div class="muted small">Enter where to stop.</div>`); return; }
    setHTML(box, html`<div class="muted small">${icon('loader', 'spin')} Checking backups and archived binlogs…</div>`);
    try { plan = await post('/pitr/plan', { job_id: jobId, target }); } catch (er) { plan = { ok: false, target: target.type, error: er.message }; }
    if (!ctx.alive()) return;
    setHTML(box, pitrPlanView(plan, 0, target));
    markPlan();
    $('#go', el).disabled = !plan.ok;
    if (plan.ok) setHTML($('#go', el), html`${icon('restore')}Continue with backup #${plan.base.id}`);
  };
  const recheck = debounce(check, 400);
  tbox.addEventListener('input', () => { placePin(); recheck(); });
  tbox.addEventListener('change', () => { placePin(); recheck(); });
  form.job_id.addEventListener('change', async () => { jobId = +form.job_id.value; await loadJob(); check(); });
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    if (plan && plan.ok) navigate(`/runs/${plan.base.id}/restore?pitr=${encodeURIComponent(JSON.stringify(target))}`);
  });
  await loadJob();
  check();
}
