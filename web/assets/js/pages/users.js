import { get, post, put, del } from '../api.js';
import { html, setHTML, icon, on, fmtAgo, toast, toastErr, confirmBox, modal, formData, bindModalForm } from '../ui.js';
import { app } from '../app.js';

export const title = 'Users';

const ROLES = [
  ['admin', 'Admin', 'Full access: users, settings, notifications, replacing the datadir during physical restores'],
  ['operator', 'Operator', 'Manage servers, storage and jobs; run backups and restores'],
  ['viewer', 'Viewer', 'Read-only: dashboard, history, logs'],
];

export async function render(ctx) {
  const { el } = ctx;
  let list = await get('/users');
  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>Users</h1><div class="sub">Sign-in accounts and permissions</div></div><button class="btn btn-primary" id="add">${icon('plus')}Add user</button></div>
      <div class="card"><div class="table-wrap"><table class="table"><thead><tr><th>Username</th><th>Role</th><th>2FA</th><th>Status</th><th class="hide-sm">Last sign-in</th><th></th></tr></thead><tbody>
        ${list.map((u) => html`<tr><td><b>${u.username}</b>${u.id === app.user.id ? html` <span class="badge info">you</span>` : ''}<div class="sub">${u.email}</div></td>
          <td><span class="badge outline">${ROLES.find((r) => r[0] === u.role)?.[1] || u.role}</span></td>
          <td>${u.totp_enabled ? html`<span class="badge good">${icon('shield')}On</span>` : html`<span class="muted">Off</span>`}</td>
          <td>${u.disabled ? html`<span class="badge bad">${icon('lock')}Locked</span>` : html`<span class="badge good">${icon('check')}Active</span>`}</td>
          <td class="hide-sm">${u.last_login_at ? fmtAgo(u.last_login_at) : html`<span class="muted">never</span>`}</td>
          <td class="actions"><button class="btn btn-sm btn-ghost btn-icon" data-edit="${u.id}" title="Edit">${icon('edit')}</button>
            ${u.totp_enabled ? html`<button class="btn btn-sm btn-ghost" data-reset2fa="${u.id}" title="Disable 2FA">${icon('shield')}Reset 2FA</button>` : ''}
            ${u.id !== app.user.id ? html`<button class="btn btn-sm btn-ghost btn-icon" data-del="${u.id}" title="Delete">${icon('trash')}</button>` : ''}</td></tr>`)}
      </tbody></table></div></div>
      <div class="card"><div class="card-head"><h2>Roles</h2></div><div class="card-body"><div class="kv">${ROLES.map(([, n, d]) => html`<div><b>${n}</b></div><div>${d}</div>`)}</div></div></div>`);
  };
  draw();
  const reload = async () => { list = await get('/users'); draw(); };

  const showForm = (u) => {
    const m = modal({
      title: u ? `Edit: ${u.username}` : 'Add user',
      body: html`<form id="u-form" autocomplete="off">
        <div class="field"><label class="req">Username</label><input type="text" name="username" value="${u ? u.username : ''}" ${u ? 'disabled' : 'required'}></div>
        <div class="field" style="margin-top:12px"><label>Email</label><input type="email" name="email" value="${u ? u.email : ''}"></div>
        <div class="field" style="margin-top:12px"><label>Role</label><select name="role">${ROLES.map(([v, n]) => html`<option value="${v}" ${u && u.role === v ? 'selected' : ''} ${!u && v === 'operator' ? 'selected' : ''}>${n}</option>`)}</select></div>
        <div class="field" style="margin-top:12px"><label class="${u ? '' : 'req'}">${u ? 'New password (leave blank to keep)' : 'Password (min. 8 characters)'}</label><input type="password" name="password" autocomplete="new-password" minlength="8" ${u ? '' : 'required'}></div>
        ${u ? html`<label class="check" style="margin-top:12px"><input type="checkbox" name="disabled" ${u.disabled ? 'checked' : ''}><span>Lock account</span></label>` : ''}
      </form>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="save">${icon('check')}Save</button>`,
    });
    const form = m.el.querySelector('#u-form');
    bindModalForm(form, m.el.querySelector('#save'), async () => {
      const d = formData(form);
      if (u) await put(`/users/${u.id}`, { email: d.email, role: d.role, disabled: !!d.disabled, password: d.password });
      else await post('/users', d);
      m.close();
      toast('Saved');
      await reload();
    });
  };
  on(el, 'click', '#add', () => showForm(null));
  on(el, 'click', '[data-edit]', (e, b) => showForm(list.find((x) => x.id === +b.dataset.edit)));
  on(el, 'click', '[data-reset2fa]', async (e, b) => {
    const u = list.find((x) => x.id === +b.dataset.reset2fa);
    if (!(await confirmBox({ title: 'Reset 2FA', message: html`Disable two-factor authentication for <b>${u.username}</b>? (e.g. if they lost their phone)`, okText: 'Disable 2FA' }))) return;
    try { await post(`/users/${u.id}/reset-2fa`); toast('2FA disabled'); await reload(); } catch (er) { toastErr(er); }
  });
  on(el, 'click', '[data-del]', async (e, b) => {
    const u = list.find((x) => x.id === +b.dataset.del);
    if (!(await confirmBox({ title: 'Delete user', message: html`Delete account <b>${u.username}</b>?`, danger: true, okText: 'Delete' }))) return;
    try { await del(`/users/${u.id}`); toast('Deleted'); await reload(); } catch (er) { toastErr(er); }
  });
}
