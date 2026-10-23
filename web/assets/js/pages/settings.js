import { get, post, put, del } from '../api.js';
import { html, setHTML, icon, toast, toastErr, busy, validate, formError, modal, confirmBox, copyText, SECRET_MASK } from '../ui.js';
import { setLeaveGuard, app, setBrand } from '../app.js';

export const title = 'Settings';

const TZ = ['Asia/Ho_Chi_Minh', 'Asia/Bangkok', 'Asia/Singapore', 'Asia/Tokyo', 'UTC', 'Europe/London', 'Europe/Berlin', 'America/New_York', 'America/Los_Angeles'];
const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
const TOOLS = ['mysqldump', 'mariadb-dump', 'mysql', 'mariadb', 'mydumper', 'myloader', 'xtrabackup', 'xbstream', 'mariabackup', 'mbstream', 'pg_dump', 'pg_restore', 'pg_dumpall', 'rclone'];

export async function render(ctx) {
  const { el } = ctx;
  const [s, sso] = await Promise.all([get('/settings'), get('/settings/sso')]);
  let ssoSaved = sso.client_secret === SECRET_MASK;
  const roleOpts = (v) => [['viewer', 'Viewer'], ['operator', 'Operator'], ['admin', 'Admin']].map(([r, n]) => html`<option value="${r}" ${v === r ? 'selected' : ''}>${n}</option>`);
  const metricsURL = (s.base_url || location.origin) + '/metrics';
  const tokenState = (prefix) => (prefix ? html`<code>${prefix}…</code>` : html`<span class="muted">none yet</span>`);
  setHTML(el, html`
    <div class="page-head"><div><h1>System settings</h1></div></div>
    <form id="set-form" autocomplete="off">
    <div class="card" id="sec-branding"><div class="card-head"><h2>Branding</h2></div><div class="card-body"><div class="form-grid">
      <div class="field"><label for="instance_name">Instance name</label><input type="text" id="instance_name" name="instance_name" value="${s.instance_name || ''}" maxlength="60" placeholder="DumpCat">
        <div class="help">Shown in the sidebar, the browser tab, the sign-in page and notifications; email subjects start with it. Leave blank for DumpCat.</div></div>
      <div class="field"><label>Logo</label>
        <div class="inline"><img class="logo-preview" id="logo-preview" src="${app.brand.logo || '/assets/favicon.svg'}" alt="Current logo" width="40" height="40">
          <button type="button" class="btn" id="logo-upload">${icon('upload')}Upload logo</button>
          <button type="button" class="btn btn-ghost" id="brand-reset">${icon('refresh')}Reset to default</button>
          <input type="file" id="logo-file" class="hidden" accept="image/png,image/svg+xml,image/webp,.png,.svg,.webp" aria-label="Logo file"></div>
        <div class="help">PNG, SVG or WebP, at most 256 KB; a square image looks best. SVGs with scripts or links to other files are refused. Emails show a PNG logo (with SVG or WebP they keep the cat). <b>Reset to default</b> brings back the name DumpCat and the cat.</div></div>
    </div></div></div>
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
      <div class="field full"><label class="check"><input type="checkbox" name="update_auto_check" ${s.update_auto_check || s.update_auto_install ? 'checked' : ''}><span>Check for a new version once a day<span class="help">Shows a notice to admins when a new version is out.</span></span></label></div>
      <div class="field full"><label class="check"><input type="checkbox" name="update_auto_install" ${s.update_auto_install ? 'checked' : ''}><span>Install new versions automatically<span class="help">Over the air from the release server, between the hours below (time zone above), and only releases signed with a trusted key. DumpCat waits for running backups and restores, backs up its own database, verifies the download and restarts; if the new version does not start, the previous one and its database come back by themselves. Your notification channels hear about every update.</span></span></label></div>
      <div class="field" id="update-hours-from"><label for="update_from_hour">Install between</label><select id="update_from_hour" name="update_from_hour">${Array.from({ length: 24 }, (_, h) => html`<option value="${h}" ${s.update_from_hour === h ? 'selected' : ''}>${String(h).padStart(2, '0')}:00</option>`)}</select></div>
      <div class="field" id="update-hours-to"><label for="update_to_hour">and</label><select id="update_to_hour" name="update_to_hour">${Array.from({ length: 24 }, (_, h) => html`<option value="${h}" ${s.update_to_hour === h ? 'selected' : ''}>${String(h).padStart(2, '0')}:00</option>`)}</select>
        <div class="help">An update that waits for running tasks gives up at the end and tries again the next day. The same hour twice: any time.</div></div>
    </div></div></div>
    <div class="card"><div class="card-head"><h2>Security</h2></div><div class="card-body"><div class="form-grid">
      <div class="field"><label>Idle session timeout (hours)</label><input type="number" name="session_hours" value="${s.session_hours}" min="1" max="720"></div>
      <div class="field"><label class="check" style="margin-top:26px"><input type="checkbox" name="require_2fa_admins" ${s.require_2fa_admins ? 'checked' : ''}><span>Remind admins to enable two-factor authentication at sign-in</span></label></div>
      <div class="field full"><label class="check"><input type="checkbox" name="allow_indexing" ${s.allow_indexing ? 'checked' : ''}><span>Let search engines index this instance<span class="help">Off (recommended): <code>/robots.txt</code> disallows everything, every answer carries <code>X-Robots-Tag: noindex, nofollow</code> and the page has a robots meta tag, so the sign-in page does not show up in search results. Turn on only for a public demo.</span></span></label></div>
    </div></div></div>
    <div class="card" id="sec-sso"><div class="card-head"><h2>Authentication: single sign-on (OpenID Connect)</h2></div><div class="card-body">
      <div class="help" style="margin:0 0 12px">Sign in with Google, Microsoft Entra ID, Keycloak, Authentik or any OpenID Connect provider. Users are found by their email address (verified at the provider) and linked to their provider account at the first sign-in. Password sign-in stays available as the fallback, and accounts with two-factor authentication still enter their code after SSO.</div>
      <div class="form-grid">
      <div class="field full"><label class="check"><input type="checkbox" name="sso_enabled" ${sso.enabled ? 'checked' : ''}><span>Allow single sign-on<span class="help">Shows the SSO button on the sign-in page. Saving checks that the provider answers.</span></span></label></div>
      <div class="field full"><label for="sso_issuer">Issuer URL</label><input type="url" id="sso_issuer" name="sso_issuer" value="${sso.issuer || ''}" placeholder="https://accounts.google.com" spellcheck="false">
        <div class="help">Google: <code>https://accounts.google.com</code> · Microsoft Entra ID: <code>https://login.microsoftonline.com/&lt;tenant-id&gt;/v2.0</code> · Keycloak: <code>https://&lt;host&gt;/realms/&lt;realm&gt;</code> · Authentik: <code>https://&lt;host&gt;/application/o/&lt;slug&gt;/</code>. DumpCat reads <code>/.well-known/openid-configuration</code> from it.</div></div>
      <div class="field"><label for="sso_client_id">Client ID</label><input type="text" id="sso_client_id" name="sso_client_id" value="${sso.client_id || ''}" autocomplete="off" spellcheck="false"></div>
      <div class="field"><label for="sso_client_secret">Client secret</label><input type="password" id="sso_client_secret" name="sso_client_secret" value="" placeholder="${ssoSaved ? '•••••• saved — leave blank to keep' : ''}" autocomplete="new-password">
        <div class="help">Stored encrypted with the master key, like other secrets.</div></div>
      <div class="field full"><label>Redirect URL</label><div class="copy-box"><code id="sso-redirect">${sso.redirect_url}</code><button type="button" class="btn" id="sso-copy" title="Copy">${icon('copy')}</button></div>
        <div class="help">Register it at the provider as the redirect (callback) URL of a web application. It follows the <b>Base URL</b> above: set that when DumpCat is behind a reverse proxy.</div></div>
      <div class="field"><label for="sso_scopes">Scopes</label><input type="text" id="sso_scopes" name="sso_scopes" value="${sso.scopes || 'openid email profile'}" spellcheck="false"></div>
      <div class="field"><label for="sso_button_label">Button label</label><input type="text" id="sso_button_label" name="sso_button_label" value="${sso.button_label || ''}" maxlength="60" placeholder="Sign in with SSO"></div>
      <div class="field"><label for="sso_email_claim">Email claim</label><input type="text" id="sso_email_claim" name="sso_email_claim" value="${sso.email_claim || ''}" placeholder="email" spellcheck="false">
        <div class="help">Where the email address is. Entra ID without the optional <code>email</code> claim: <code>preferred_username</code>.</div></div>
      <div class="field"><label for="sso_allowed_domains">Allowed email domains</label><input type="text" id="sso_allowed_domains" name="sso_allowed_domains" value="${sso.allowed_domains || ''}" placeholder="any — e.g. example.com, example.org" spellcheck="false">
        <div class="help">Only these addresses may sign in with SSO.</div></div>
      <div class="field"><label class="check" style="margin-top:26px"><input type="checkbox" name="sso_auto_create" ${sso.auto_create ? 'checked' : ''}><span>Create accounts for new users<span class="help">Without it, an admin adds the user in Users first (a password is optional then).</span></span></label></div>
      <div class="field"><label for="sso_default_role">Role of new accounts</label><select id="sso_default_role" name="sso_default_role">${roleOpts(sso.default_role || 'viewer')}</select></div>
      <div class="field"><label for="sso_role_claim">Role from a claim (optional)</label><input type="text" id="sso_role_claim" name="sso_role_claim" value="${sso.role_claim || ''}" placeholder="groups, roles, realm_access.roles" spellcheck="false">
        <div class="help">When set, every SSO sign-in takes the role from this claim: the highest that matches below. Users in none of these groups cannot sign in with SSO.</div></div>
      <div class="field"><label for="sso_admin_values">Admin values</label><input type="text" id="sso_admin_values" name="sso_admin_values" value="${sso.admin_values || ''}" placeholder="e.g. dumpcat-admins" spellcheck="false"></div>
      <div class="field"><label for="sso_operator_values">Operator values</label><input type="text" id="sso_operator_values" name="sso_operator_values" value="${sso.operator_values || ''}" placeholder="comma-separated" spellcheck="false"></div>
      <div class="field"><label for="sso_viewer_values">Viewer values</label><input type="text" id="sso_viewer_values" name="sso_viewer_values" value="${sso.viewer_values || ''}" placeholder="comma-separated" spellcheck="false"></div>
      <div class="field"><label for="sso_password_admins_only">Password sign-in</label><select id="sso_password_admins_only" name="sso_password_admins_only">
        <option value="all" ${!sso.password_admins_only ? 'selected' : ''}>Everyone may still use a password</option>
        <option value="admins" ${sso.password_admins_only ? 'selected' : ''}>Admins only: everyone else uses SSO</option></select>
        <div class="help">Admins can always sign in with their password: the way in when the provider is down.</div></div>
      <div class="field full"><div class="inline"><button type="button" class="btn" id="sso-test">${icon('play')}Test SSO</button><span class="help" style="margin:0">Opens the provider in a new tab with the settings above (saved or not) and shows what it sent and what a sign-in would do. Nobody is signed in.</span></div></div>
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
  // installing automatically needs the daily check; the hours only matter then
  const showUpdates = () => {
    const auto = form.update_auto_install.checked;
    if (auto) form.update_auto_check.checked = true;
    form.update_auto_check.disabled = auto;
    el.querySelector('#update-hours-from').classList.toggle('hidden', !auto);
    el.querySelector('#update-hours-to').classList.toggle('hidden', !auto);
  };
  form.update_auto_install.addEventListener('change', showUpdates);
  showUpdates();
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
  const ssoBody = () => ({
    enabled: form.sso_enabled.checked, issuer: form.sso_issuer.value.trim(), client_id: form.sso_client_id.value.trim(),
    client_secret: form.sso_client_secret.value === '' && ssoSaved ? SECRET_MASK : form.sso_client_secret.value,
    scopes: form.sso_scopes.value.trim(), button_label: form.sso_button_label.value.trim(), email_claim: form.sso_email_claim.value.trim(),
    allowed_domains: form.sso_allowed_domains.value.trim(), auto_create: form.sso_auto_create.checked, default_role: form.sso_default_role.value,
    role_claim: form.sso_role_claim.value.trim(), admin_values: form.sso_admin_values.value.trim(), operator_values: form.sso_operator_values.value.trim(),
    viewer_values: form.sso_viewer_values.value.trim(), password_admins_only: form.sso_password_admins_only.value === 'admins',
  });
  el.querySelector('#sso-copy').onclick = () => copyText(el.querySelector('#sso-redirect').textContent);
  el.querySelector('#sso-test').onclick = async (e) => {
    // the tab opens during the click (popup blockers), then goes to the provider
    const win = window.open('', '_blank');
    if (win) win.opener = null;
    await busy(e.currentTarget, async () => {
      try {
        const r = await post('/settings/sso/test', ssoBody());
        if (!win) {
          // pop-ups blocked: a link the admin clicks opens it
          modal({ title: 'Test SSO', body: html`<p style="margin-top:0">The browser blocked the new tab. Open the provider's sign-in page: after signing in, it shows what the provider sent and what a sign-in would do.</p>
            <a class="btn btn-primary" href="${r.url}" target="_blank" rel="noopener" data-close>${icon('play')}Open the test sign-in</a>`, footer: html`<button class="btn" data-close>Close</button>` });
          return;
        }
        win.location.href = r.url;
        toast('Sign in at the provider in the new tab: it shows the result', 'info', 8000);
      } catch (er) {
        if (win) win.close();
        toastErr(er);
      }
    });
  };
  const preview = el.querySelector('#logo-preview');
  const fileInput = el.querySelector('#logo-file');
  // the logo is saved at once: picking a file does not leave the form with unsaved changes
  ['input', 'change'].forEach((t) => fileInput.addEventListener(t, (e) => e.stopPropagation()));
  el.querySelector('#logo-upload').onclick = () => fileInput.click();
  fileInput.addEventListener('change', async () => {
    const f = fileInput.files[0];
    fileInput.value = '';
    if (!f) return;
    if (f.size > 256 * 1024) { toast('The logo must be at most 256 KB', 'bad'); return; }
    const data = await new Promise((res, rej) => { const rd = new FileReader(); rd.onload = () => res(rd.result); rd.onerror = () => rej(rd.error); rd.readAsDataURL(f); });
    await busy(el.querySelector('#logo-upload'), async () => {
      try {
        const b = await put('/branding/logo', { data });
        setBrand(b);
        preview.src = b.logo;
        toast('Logo updated');
      } catch (er) { toastErr(er); }
    });
  });
  el.querySelector('#brand-reset').onclick = async (e) => {
    const btn = e.currentTarget;
    if (!(await confirmBox({ title: 'Reset branding', message: 'Go back to the name DumpCat and the cat logo?', okText: 'Reset' }))) return;
    await busy(btn, async () => {
      try {
        const cur = await get('/settings');
        await put('/settings', { ...cur, instance_name: '' });
        const b = await del('/branding/logo');
        form.instance_name.value = '';
        s.instance_name = '';
        setBrand(b);
        preview.src = '/assets/favicon.svg';
        toast('Branding reset to default');
      } catch (er) { toastErr(er); }
    });
  };
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
        instance_name: form.instance_name.value.trim(),
        timezone: form.timezone.value.trim(), base_url: form.base_url.value.trim(), max_concurrent: +form.max_concurrent.value,
        default_threads: +form.default_threads.value, staging_dir: form.staging_dir.value.trim(), history_days: +form.history_days.value,
        rclone_flags: form.rclone_flags.value.trim(), session_hours: +form.session_hours.value, require_2fa_admins: form.require_2fa_admins.checked, tool_paths: tp,
        allow_indexing: form.allow_indexing.checked,
        update_url: form.update_url.value.trim(), update_auto_check: form.update_auto_check.checked,
        update_auto_install: form.update_auto_install.checked, update_from_hour: +form.update_from_hour.value, update_to_hour: +form.update_to_hour.value,
        freshness_alerts: form.freshness_alerts.checked, stale_grace_minutes: +form.stale_grace_minutes.value, binlog_lag_minutes: +form.binlog_lag_minutes.value,
        digest_weekday: +form.digest_weekday.value, digest_hour: +form.digest_hour.value, metrics_access: form.metrics_access.value,
      };
      try {
        try {
          const sv = await put('/settings/sso', ssoBody());
          ssoSaved = sv.client_secret === SECRET_MASK;
          app.sso = sv.enabled ? { enabled: true, label: sv.button_label || 'Sign in with SSO' } : null;
          form.sso_client_secret.value = '';
          form.sso_client_secret.placeholder = ssoSaved ? '•••••• saved — leave blank to keep' : '';
        } catch (er) { formError(form, 'Single sign-on: ' + er.message); return; }
        const saved = await put('/settings', body);
        setDirty(false);
        setBrand({ name: saved.instance_name || 'DumpCat' });
        toast('Settings saved');
      } catch (er) { formError(form, er.message); }
    });
  });
}
