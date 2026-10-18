import { get, put } from '../api.js';
import { html, setHTML, icon, toast, busy, validate, formError } from '../ui.js';
import { setLeaveGuard } from '../app.js';

export const title = 'Settings';

const TZ = ['Asia/Ho_Chi_Minh', 'Asia/Bangkok', 'Asia/Singapore', 'Asia/Tokyo', 'UTC', 'Europe/London', 'Europe/Berlin', 'America/New_York', 'America/Los_Angeles'];
const TOOLS = ['mysqldump', 'mariadb-dump', 'mysql', 'mariadb', 'mydumper', 'myloader', 'xtrabackup', 'xbstream', 'mariabackup', 'mbstream', 'rclone'];

export async function render(ctx) {
  const { el } = ctx;
  const s = await get('/settings');
  setHTML(el, html`
    <div class="page-head"><div><h1>System settings</h1></div></div>
    <form id="set-form" autocomplete="off">
    <div class="card"><div class="card-head"><h2>General</h2></div><div class="card-body"><div class="form-grid">
      <div class="field"><label>Time zone (schedules &amp; backup directory names)</label><input type="text" name="timezone" value="${s.timezone}" list="tz-list" placeholder="default: the server's time zone">
        <datalist id="tz-list">${TZ.map((t) => html`<option value="${t}">`)}</datalist></div>
      <div class="field"><label>Base URL (the address DumpCat is reached at)</label><input type="text" name="base_url" value="${s.base_url}" placeholder="https://backup.example.com">
        <div class="help">Used for links in notifications and for the OAuth callback.</div></div>
      <div class="field"><label>Max concurrent backup jobs</label><input type="number" name="max_concurrent" value="${s.max_concurrent}" min="1" max="32"></div>
      <div class="field"><label>Default threads for new jobs</label><input type="number" name="default_threads" value="${s.default_threads}" min="1" max="64"></div>
      <div class="field full"><label>Staging directory</label><input type="text" name="staging_dir" value="${s.staging_dir}" placeholder="default: <data dir>/staging">
        <div class="help">Where backups are written temporarily before upload (when a job has no local storage) and where mydumper restores are extracted. DumpCat uses a <code>dumpcat-staging</code> subdirectory inside it. Put it on a disk with enough space, ideally not the one holding the datadir.</div></div>
      <div class="field"><label>Keep failed/deleted history (days)</label><input type="number" name="history_days" value="${s.history_days}" min="0"><div class="help">0 = keep forever.</div></div>
      <div class="field"><label>Extra rclone flags</label><input type="text" name="rclone_flags" value="${s.rclone_flags}" placeholder="e.g. --transfers 8 --s3-upload-concurrency 8"></div>
    </div></div></div>
    <div class="card"><div class="card-head"><h2>Updates</h2></div><div class="card-body"><div class="form-grid">
      <div class="field full"><label for="update_url">Release server</label><input type="url" id="update_url" name="update_url" value="${s.update_url || ''}" placeholder="https://downloads.dumpcat.cc">
        <div class="help">DumpCat reads <code>latest.json</code> from this address (HTTPS). Leave blank to use <code>DUMPCAT_UPDATE_URL</code> or the address built into this version. Install updates on the <a href="#/system">System</a> page.</div></div>
      <div class="field full"><label class="check"><input type="checkbox" name="update_auto_check" ${s.update_auto_check ? 'checked' : ''}><span>Check for a new version once a day<span class="help">Only checks and shows a notice to admins — nothing is installed automatically.</span></span></label></div>
    </div></div></div>
    <div class="card"><div class="card-head"><h2>Security</h2></div><div class="card-body"><div class="form-grid">
      <div class="field"><label>Idle session timeout (hours)</label><input type="number" name="session_hours" value="${s.session_hours}" min="1" max="720"></div>
      <div class="field"><label class="check" style="margin-top:26px"><input type="checkbox" name="require_2fa_admins" ${s.require_2fa_admins ? 'checked' : ''}><span>Remind admins to enable two-factor authentication at sign-in</span></label></div>
    </div></div></div>
    <div class="card"><div class="card-head"><h2>Tool paths (optional)</h2></div><div class="card-body">
      <div class="help" style="margin-bottom:12px">Leave blank to search PATH. Set these when several versions are installed (e.g. xtrabackup 8.0 and 8.4).</div>
      <div class="form-grid">${TOOLS.map((t) => html`<div class="field"><label class="mono">${t}</label><input type="text" data-tool="${t}" value="${(s.tool_paths || {})[t] || ''}" placeholder="/usr/bin/${t}"></div>`)}</div>
    </div></div>
    <div class="sticky-actions"><span class="dirty-note" id="dirty-note" aria-live="polite"></span><button class="btn btn-primary" type="submit">${icon('check')}Save settings</button></div>
    </form>`);
  const form = el.querySelector('#set-form');
  let dirty = false;
  const setDirty = (v) => { dirty = v; el.querySelector('#dirty-note').textContent = v ? 'Unsaved changes' : ''; };
  form.addEventListener('input', () => setDirty(true));
  form.addEventListener('change', () => setDirty(true));
  setLeaveGuard(() => (dirty ? 'Settings have unsaved changes. Leave without saving?' : null));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    formError(form, null);
    if (!validate(form)) return;
    busy(form.querySelector('button[type=submit]'), async () => {
      const tp = {};
      form.querySelectorAll('[data-tool]').forEach((i) => { if (i.value.trim()) tp[i.dataset.tool] = i.value.trim(); });
      const body = {
        timezone: form.timezone.value.trim(), base_url: form.base_url.value.trim(), max_concurrent: +form.max_concurrent.value,
        default_threads: +form.default_threads.value, staging_dir: form.staging_dir.value.trim(), history_days: +form.history_days.value,
        rclone_flags: form.rclone_flags.value.trim(), session_hours: +form.session_hours.value, require_2fa_admins: form.require_2fa_admins.checked, tool_paths: tp,
        update_url: form.update_url.value.trim(), update_auto_check: form.update_auto_check.checked,
      };
      try { await put('/settings', body); setDirty(false); toast('Settings saved'); } catch (er) { formError(form, er.message); }
    });
  });
}
