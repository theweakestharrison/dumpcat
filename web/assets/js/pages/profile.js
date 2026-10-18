import { get, post, del } from '../api.js';
import { html, setHTML, icon, on, fmtAgo, fmtDate, toast, toastErr, busy, modal, confirmBox, formData, copyText, empty } from '../ui.js';
import { app, updateUser } from '../app.js';

export const title = 'Profile & security';

export async function render(ctx) {
  const { el } = ctx;
  let me = (await get('/me')).user;
  let tokens = await get('/me/tokens');
  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>Profile &amp; security</h1><div class="sub">${me.username} · ${me.role}</div></div></div>
      <div class="grid grid-2">
        <div class="card"><div class="card-head"><h2>Change password</h2></div><div class="card-body">
          <form id="pw-form" autocomplete="off">
            <div class="field"><label>Current password</label><input type="password" name="current" required autocomplete="current-password"></div>
            <div class="field" style="margin-top:12px"><label>New password (min. 8 characters)</label><input type="password" name="new" required minlength="8" autocomplete="new-password"></div>
            <div class="field" style="margin-top:12px"><label>Confirm new password</label><input type="password" name="new2" required autocomplete="new-password"></div>
            <div class="form-actions" style="margin-top:14px"><button class="btn btn-primary" type="submit">Change password</button></div>
          </form></div></div>
        <div class="card"><div class="card-head"><h2>Two-factor authentication (TOTP)</h2>${me.totp_enabled ? html`<span class="badge good">${icon('shield')}Enabled</span>` : html`<span class="badge warn">${icon('alert')}Not enabled</span>`}</div><div class="card-body">
          ${me.totp_enabled ? html`<p>Signing in requires a 6-digit code from your authenticator app.</p><button class="btn btn-danger" id="totp-off">Disable 2FA</button>`
            : html`<p>Protect your account with Google Authenticator, Microsoft Authenticator, Authy, 1Password…</p><button class="btn btn-primary" id="totp-on">${icon('shield')}Enable 2FA</button>`}
          <hr><button class="btn" id="logout-others">${icon('logout')}Sign out all other sessions</button>
        </div></div>
      </div>
      <div class="card"><div class="card-head"><h2>API tokens</h2><button class="btn btn-sm btn-primary" id="tok-add">${icon('plus')}New token</button></div>
        <div class="card-body small muted" style="padding-bottom:0">For automation / CI: <code>curl -X POST -H "Authorization: Bearer &lt;token&gt;" ${location.origin}/api/jobs/&lt;id&gt;/run</code>. A token has the same permissions as your account.</div>
        <div class="table-wrap" style="margin-top:12px">${tokens.length ? html`<table class="table"><thead><tr><th>Name</th><th>Prefix</th><th>Created</th><th>Last used</th><th>Expires</th><th></th></tr></thead><tbody>
          ${tokens.map((t) => html`<tr><td><b>${t.name}</b></td><td class="mono small">${t.prefix}…</td><td>${fmtDate(t.created_at)}</td><td>${t.last_used_at ? fmtAgo(t.last_used_at) : html`<span class="muted">never</span>`}</td><td>${t.expires_at ? fmtDate(t.expires_at) : 'never'}</td>
            <td class="actions"><button class="btn btn-sm btn-ghost btn-icon" data-tdel="${t.id}" title="Revoke">${icon('trash')}</button></td></tr>`)}</tbody></table>` : empty('No tokens yet', 'key')}</div></div>`);
  };
  draw();
  const reload = async () => { me = (await get('/me')).user; tokens = await get('/me/tokens'); updateUser(me); draw(); };

  on(el, 'submit', '#pw-form', async (e, form) => {
    e.preventDefault();
    const d = formData(form);
    if (d.new !== d.new2) return toast('Passwords do not match', 'bad');
    await busy(form.querySelector('button'), async () => {
      try { await post('/me/password', { current: d.current, new: d.new }); toast('Password changed. Other sessions have been signed out.'); form.reset(); } catch (er) { toastErr(er); }
    });
  });
  on(el, 'click', '#totp-on', async (e, b) => {
    let setup;
    try { setup = await busy(b, () => post('/me/totp/setup')); } catch (er) { return toastErr(er); }
    const m = modal({
      title: 'Enable two-factor authentication',
      body: html`<ol style="padding-left:18px;margin-top:0"><li>Scan the QR code with your authenticator app.</li><li>Enter the 6-digit code it shows to confirm.</li></ol>
        <div style="text-align:center">${setup.qr ? html`<div class="qr"><img src="${setup.qr}" alt="QR code"></div>` : ''}</div>
        <div class="field" style="margin-top:12px"><label>Or enter the key manually</label><div class="copy-box"><code>${setup.secret}</code><button type="button" class="btn" id="cp">${icon('copy')}</button></div></div>
        <div class="field" style="margin-top:12px"><label>Verification code</label><input type="text" id="code" inputmode="numeric" maxlength="6" autocomplete="one-time-code"></div>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="enable">Enable</button>`,
    });
    m.el.querySelector('#cp').onclick = () => copyText(setup.secret);
    m.el.querySelector('#enable').onclick = (ev) => busy(ev.currentTarget, async () => {
      try { await post('/me/totp/enable', { code: m.el.querySelector('#code').value }); m.close(); toast('2FA enabled'); await reload(); } catch (er) { toastErr(er); }
    });
  });
  on(el, 'click', '#totp-off', () => {
    const m = modal({
      title: 'Disable two-factor authentication',
      body: html`<div class="field"><label>Enter your password to confirm</label><input type="password" id="pw" autocomplete="current-password"></div>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-danger-solid" id="off">Disable 2FA</button>`,
    });
    m.el.querySelector('#off').onclick = (ev) => busy(ev.currentTarget, async () => {
      try { await post('/me/totp/disable', { password: m.el.querySelector('#pw').value }); m.close(); toast('2FA disabled', 'warn'); await reload(); } catch (er) { toastErr(er); }
    });
  });
  on(el, 'click', '#logout-others', async (e, b) => {
    try { await busy(b, () => post('/me/logout-others')); toast('Other sessions signed out'); } catch (er) { toastErr(er); }
  });
  on(el, 'click', '#tok-add', () => {
    const m = modal({
      title: 'New API token',
      body: html`<form id="tk"><div class="field"><label class="req">Name</label><input type="text" name="name" placeholder="e.g. jenkins-deploy" required></div>
        <div class="field" style="margin-top:12px"><label>Expires after (days, 0 = never)</label><input type="number" name="expires_days" value="90" min="0"></div></form>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="mk">Create</button>`,
    });
    m.el.querySelector('#mk').onclick = (ev) => busy(ev.currentTarget, async () => {
      try {
        const r = await post('/me/tokens', formData(m.el.querySelector('#tk')));
        m.close();
        const s = modal({
          title: 'Token created',
          body: html`<div class="alert warn" style="margin-bottom:12px">${icon('alert')}<div>Copy it now — the token is shown <b>only once</b>.</div></div><div class="copy-box"><code>${r.token}</code><button class="btn" id="cpt">${icon('copy')}</button></div>`,
          footer: html`<button class="btn btn-primary" data-close>Done</button>`,
        });
        s.el.querySelector('#cpt').onclick = () => copyText(r.token);
        await reload();
      } catch (er) { toastErr(er); }
    });
  });
  on(el, 'click', '[data-tdel]', async (e, b) => {
    if (!(await confirmBox({ title: 'Revoke token', message: 'Applications using this token will lose access.', danger: true, okText: 'Revoke' }))) return;
    try { await del(`/me/tokens/${b.dataset.tdel}`); toast('Revoked'); await reload(); } catch (er) { toastErr(er); }
  });
  void app;
}
