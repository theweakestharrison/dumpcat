import { get, post } from '../api.js';
import { html, setHTML, icon, on, $, debounce } from '../ui.js';
import { navigate, canOperate } from '../app.js';
import { backBtn, pitrTargetFields, bindPITRFields, readPITRTarget, pitrPlanView } from '../components.js';

export const title = 'Point-in-time recovery';

// Picks the recovery target first; the newest backup before it is then
// restored through the normal restore form with the target pre-filled.
export async function render(ctx) {
  const { el, query } = ctx;
  if (!canOperate()) { setHTML(el, html`<div class="alert warn">${icon('lock')}<div>You don't have permission to restore.</div></div>`); return; }
  const jobs = await get('/binlog');
  let jobId = +query.job_id || (jobs[0] && jobs[0].job_id) || 0;
  setHTML(el, html`
    <div class="page-head">${backBtn('#/binlogs')}<div><h1>Restore to a point in time</h1>
      <div class="sub">Choose where the recovery should stop: DumpCat picks the newest backup before that point and replays the archived binlogs after it.</div></div></div>
    ${jobs.length ? html`<form id="pitr-form" autocomplete="off"><div class="card"><div class="card-body">
      <div class="form-grid"><div class="field"><label class="req">Backup job</label><select name="job_id">${jobs.map((j) => html`<option value="${j.job_id}" ${j.job_id === jobId ? 'selected' : ''}>${j.job_name} (${j.server_name})</option>`)}</select></div></div>
      <div class="section-title" style="margin-top:14px">Recover up to</div>
      <div id="pitr-target">${pitrTargetFields({})}</div>
      <div id="pitr-plan" style="margin-top:14px"></div>
    </div></div>
    <div class="form-actions" style="margin-top:16px"><a class="btn" href="#/binlogs">Cancel</a><button class="btn btn-primary" type="submit" id="go" disabled>${icon('restore')}Continue to restore options</button></div></form>`
      : html`<div class="card">${html`<div class="empty">${icon('layers')}No job archives binlogs yet — turn on continuous binlog archiving in a job first.</div>`}</div>`}`);
  if (!jobs.length) return;
  const form = $('#pitr-form', el);
  const tbox = $('#pitr-target', el);
  bindPITRFields(tbox);
  let plan = null;
  let target = null;
  const check = async () => {
    const box = $('#pitr-plan', el);
    jobId = +form.job_id.value;
    target = readPITRTarget(tbox);
    $('#go', el).disabled = true;
    if (!target) { setHTML(box, html`<div class="muted small">Enter where to stop.</div>`); return; }
    setHTML(box, html`<div class="muted small">${icon('loader', 'spin')} Checking backups and archived binlogs…</div>`);
    try { plan = await post('/pitr/plan', { job_id: jobId, target }); } catch (er) { plan = { ok: false, target: target.type, error: er.message }; }
    if (!ctx.alive()) return;
    setHTML(box, pitrPlanView(plan, 0));
    $('#go', el).disabled = !plan.ok;
    if (plan.ok) setHTML($('#go', el), html`${icon('restore')}Continue with backup #${plan.base.id}`);
  };
  const recheck = debounce(check, 400);
  form.addEventListener('input', recheck);
  form.addEventListener('change', recheck);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    if (plan && plan.ok) navigate(`/runs/${plan.base.id}/restore?pitr=${encodeURIComponent(JSON.stringify(target))}`);
  });
  void on;
  check();
}
