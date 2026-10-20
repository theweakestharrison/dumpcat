import { get, post, put } from '../api.js';
import { html, setHTML, icon, toast, toastErr, busy, validate, formError, modal, confirmBox, copyText } from '../ui.js';
import { setLeaveGuard } from '../app.js';

export const title = 'Settings';

const TZ = ['Asia/Ho_Chi_Minh', 'Asia/Bangkok', 'Asia/Singapore', 'Asia/Tokyo', 'UTC', 'Europe/London', 'Europe/Berlin', 'America/New_York', 'America/Los_Angeles'];
const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
const TOOLS = ['mysqldump', 'mariadb-dump', 'mysql', 'mariadb', 'mydumper', 'myloader', 'xtrabackup', 'xbstream', 'mariabackup', 'mbstream', 'pg_dump', 'pg_restore', 'pg_dumpall', 'rclone'];

export async function render(ctx) {
  const { el } = ctx;
  const s = await get('/settings');
  const metricsURL = (s.base_url || location.origin) + '/metrics';
  const tokenState = (prefix) => (prefix ? html`<code>${prefix}…</code>` : html`<span class="muted">none yet</span>`);
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
    <div class="card" id="sec-monitoring"><div class="card-head"><h2>Monitoring</h2></div><div class="card-body"><div class="form-grid">
      <div class="field full"><label class="check"><input type="checkbox" name="freshness_alerts" ${s.freshness_alerts ? 'checked' : ''}><span>Alert when a job has no recent successful backup<span class="help">A job is overdue when its newest successful backup is older than 1.5 × the time between two of its scheduled runs plus the grace period (a daily job: 36 hours + grace), or than the max age set on the job. The alert goes once to the channels that send failures, and a second message follows when a backup succeeds again. The dashboard uses the same rule.</span></span></label></div>
      <div class="field"><label for="stale_grace_minutes">Grace period (minutes)</label><input type="number" id="stale_grace_minutes" name="stale_grace_minutes" value="${s.stale_grace_minutes}" min="0" max="10080"></div>
      <div class="field"><label for="binlog_lag_minutes">Alert when log archiving (binlog / WAL) is behind by (minutes)</label><input type="number" id="binlog_lag_minutes" name="binlog_lag_minutes" value="${s.binlog_lag_minutes}" min="0" max="10080">
        <div class="help">How long the archive may trail the server — the stream is down, or closed binlogs or WAL segments wait for upload. 0 = no alerts.</div></div>
      <div class="field"><label for="digest_weekday">Weekly digest: day</label><select id="digest_weekday" name="digest_weekday">${DAYS.map((d, i) => html`<option value="${i}" ${s.digest_weekday === i ? 'selected' : ''}>${d}</option>`)}</select></div>
      <div class="field"><label for="digest_hour">Weekly digest: hour</label><select id="digest_hour" name="digest_hour">${Array.from({ length: 24 }, (_, h) => html`<option value="${h}" ${s.digest_hour === h ? 'selected' : ''}>${String(h).padStart(2, '0')}:00</option>`)}</select></div>
      <div class="field full"><div class="help" style="margin-top:0">The digest covers the last 7 days: backups per job, sizes and growth, integrity checks, overdue jobs and storage use. Email channels opt in on the <a href="#/notifications">Notifications</a> page. <button type="button" class="btn btn-sm" id="digest-preview">${icon('eye')}Preview</button></div></div>
    </div>
    <div class="section-title" style="margin-top:18px">Prometheus metrics</div>
    <div class="form-grid">
      <div class="field"><label for="metrics_access">Serve <code>/metrics</code></label><select id="metrics_access" name="metrics_access">
        <option value="" ${!s.metrics_access ? 'selected' : ''}>Off</option>
        <option value="token" ${s.metrics_access === 'token' ? 'selected' : ''}>With a bearer token</option>
        <option value="localhost" ${s.metrics_access === 'localhost' ? 'selected' : ''}>Only to this machine</option></select>
        <div class="help">Last success, status, duration and size per job, integrity checks, overdue jobs, binlog lag, storage use, agents and WireGuard peers, version. <span class="mono">${metricsURL}</span></div></div>
      <div class="field" id="metrics-token-field"><label>Bearer token</label>
        <div class="inline"><span id="mt-state">${tokenState(s.metrics_token_prefix)}</span><button type="button" class="btn" id="mt-new">${icon('key')}${s.metrics_token_prefix ? 'Replace token' : 'Create token'}</button></div>
        <div class="help">Stored as a hash and shown once. Prometheus sends it as <code>Authorization: Bearer …</code>.</div></div>
      <div class="field" id="metrics-local-help"><div class="alert info">${icon('info')}<div>Only requests from this machine (127.0.0.1, ::1) that did not pass a reverse proxy are answered — run Prometheus or its agent on the DumpCat host.</div></div></div>
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
  const showMetrics = () => {
    el.querySelector('#metrics-token-field').classList.toggle('hidden', form.metrics_access.value !== 'token');
    el.querySelector('#metrics-local-help').classList.toggle('hidden', form.metrics_access.value !== 'localhost');
  };
  form.metrics_access.addEventListener('change', showMetrics);
  showMetrics();
  el.querySelector('#mt-new').onclick = async (e) => {
    const btn = e.currentTarget;
    if (s.metrics_token_prefix && !(await confirmBox({ title: 'Replace the metrics token', message: 'Prometheus gets no metrics until it uses the new token.', okText: 'Replace', danger: true }))) return;
    await busy(btn, async () => {
      try {
        const r = await post('/settings/metrics-token');
        s.metrics_token_prefix = r.prefix;
        setHTML(el.querySelector('#mt-state'), tokenState(r.prefix));
        const u = new URL(metricsURL);
        const scrape = ['scrape_configs:', '  - job_name: dumpcat', `    scheme: ${u.protocol.replace(':', '')}`, '    metrics_path: /metrics', '    authorization:',
          `      credentials: ${r.token}`, '    static_configs:', `      - targets: ['${u.host}']`, ''].join('\n');
        const m = modal({
          title: 'Metrics token created', size: 'lg',
          body: html`<div class="alert warn" style="margin-bottom:12px">${icon('alert')}<div>Copy it now — the token is shown <b>only once</b>.</div></div>
            <div class="copy-box"><code>${r.token}</code><button type="button" class="btn" id="cpt">${icon('copy')}</button></div>
            <div class="section-title" style="margin-top:16px">prometheus.yml</div><pre class="code" style="white-space:pre-wrap">${scrape}</pre>
            ${form.metrics_access.value !== 'token' ? html`<p class="muted small">Set <b>Serve /metrics</b> to <b>With a bearer token</b> and save the settings to use it.</p>` : ''}`,
          footer: html`<button type="button" class="btn" id="cps">${icon('copy')}Copy prometheus.yml</button><span style="flex:1"></span><button class="btn btn-primary" data-close>Done</button>`,
        });
        m.el.querySelector('#cpt').onclick = () => copyText(r.token);
        m.el.querySelector('#cps').onclick = () => copyText(scrape);
      } catch (er) { toastErr(er); }
    });
    setHTML(btn, html`${icon('key')}${s.metrics_token_prefix ? 'Replace token' : 'Create token'}`);
  };
  el.querySelector('#digest-preview').onclick = (e) => busy(e.currentTarget, async () => {
    try {
      const r = await get('/digest/preview');
      const m = modal({ title: 'Weekly digest — the last 7 days', size: 'lg', body: html`<iframe class="digest-frame" sandbox title="Weekly digest preview"></iframe>`, footer: html`<button class="btn btn-primary" data-close>Close</button>` });
      m.el.querySelector('iframe').srcdoc = r.html;
    } catch (er) { toastErr(er); }
  });
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
        freshness_alerts: form.freshness_alerts.checked, stale_grace_minutes: +form.stale_grace_minutes.value, binlog_lag_minutes: +form.binlog_lag_minutes.value,
        digest_weekday: +form.digest_weekday.value, digest_hour: +form.digest_hour.value, metrics_access: form.metrics_access.value,
      };
      try { await put('/settings', body); setDirty(false); toast('Settings saved'); } catch (er) { formError(form, er.message); }
    });
  });
}
