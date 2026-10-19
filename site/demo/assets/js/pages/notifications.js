import { get, post, put, del } from '../api.js';
import { html, setHTML, icon, on, toast, toastErr, confirmBox, modal, busy, empty, fieldsHTML, readFields, bindShowIf, bindModalForm } from '../ui.js';

export const title = 'Notifications';

export async function render(ctx) {
  const { el } = ctx;
  const [types, initial] = await Promise.all([get('/notify-types'), get('/notifications')]);
  let list = initial;
  const typeDef = (t) => types.find((x) => x.type === t);
  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>Notification channels</h1><div class="sub">Get reports when backups/restores succeed or fail</div></div>
        <button class="btn btn-primary" id="add">${icon('plus')}Add channel</button></div>
      <div class="card"><div class="table-wrap">${list.length ? html`<table class="table"><thead><tr><th>Name</th><th>Type</th><th>Send on</th><th>Status</th><th></th></tr></thead><tbody>
        ${list.map((c) => html`<tr><td><b>${c.name}</b></td><td>${typeDef(c.type)?.label || c.type}</td>
          <td><span class="chips">${c.on_failure ? html`<span class="badge outline">${icon('alert')}Failure</span>` : ''}${c.on_success ? html`<span class="badge outline">${icon('check')}Success</span>` : ''}${c.config?.digest === 'true' ? html`<span class="badge outline">${icon('list')}Weekly digest</span>` : ''}${!c.on_failure && !c.on_success && c.config?.digest !== 'true' ? html`<span class="muted">nothing</span>` : ''}</span></td>
          <td>${c.enabled ? html`<span class="badge good">${icon('check')}Enabled</span>` : html`<span class="badge">${icon('stop')}Disabled</span>`}</td>
          <td class="actions"><button class="btn btn-sm" data-test="${c.id}">${icon('zap')}Send test</button>
            <button class="btn btn-sm btn-ghost btn-icon" data-edit="${c.id}" title="Edit">${icon('edit')}</button>
            <button class="btn btn-sm btn-ghost btn-icon" data-del="${c.id}" title="Delete">${icon('trash')}</button></td></tr>`)}
      </tbody></table>` : empty('No notification channels yet', 'bell')}</div></div>
      <div class="alert info" style="margin-top:16px">${icon('info')}<div>Each job has its own "notify on failure / on success" options. An event is sent to every enabled channel whose conditions match. Alerts about overdue backups and binlog archiving lag go to the channels that send failures, followed by a message when they clear. Email channels can also send a weekly digest (day and hour in <a href="#/settings">Settings</a>).</div></div>`);
  };
  draw();
  const reload = async () => { list = await get('/notifications'); draw(); };

  const showForm = (c) => {
    const td = typeDef(c.type);
    const m = modal({
      title: `${c.id ? 'Edit' : 'Add'} channel: ${td.label}`, size: 'lg',
      body: html`<form id="ch-form" autocomplete="off">
        <div class="field" style="margin-bottom:14px"><label class="req">Name</label><input type="text" name="__name" value="${c.name || td.label}" required></div>
        <p class="muted small" style="margin-top:0">${td.description}</p>
        <div class="form-grid">${fieldsHTML(td.fields, c.config || {})}</div>
        <div class="checks" style="margin-top:14px">
          <label class="check"><input type="checkbox" name="__fail" ${c.on_failure !== false ? 'checked' : ''}><span>Send on failure / partial, and alerts</span></label>
          <label class="check"><input type="checkbox" name="__ok" ${c.on_success ? 'checked' : ''}><span>Send on success</span></label>
          <label class="check"><input type="checkbox" name="__en" ${c.enabled !== false ? 'checked' : ''}><span>Channel enabled</span></label>
        </div></form>`,
      footer: html`<button class="btn" id="test">${icon('zap')}Send test</button>${c.type === 'email' ? html`<button class="btn" id="digest" title="Email the digest of the last 7 days now">${icon('list')}Send digest now</button>` : ''}<span style="flex:1"></span><button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="save">${icon('check')}Save</button>`,
    });
    const form = m.el.querySelector('#ch-form');
    bindShowIf(form, td.fields);
    const payload = () => ({ id: c.id || 0, type: c.type, name: form.__name.value, config: readFields(form, td.fields), on_failure: form.__fail.checked, on_success: form.__ok.checked, enabled: form.__en.checked });
    m.el.querySelector('#test').onclick = (e) => busy(e.currentTarget, async () => {
      try { const r = await post('/notifications/test', payload()); if (r.ok) toast('Test notification sent'); else toast(r.error, 'bad', 9000); } catch (er) { toastErr(er); }
    });
    m.el.querySelector('#digest')?.addEventListener('click', (e) => busy(e.currentTarget, async () => {
      try { const r = await post('/notifications/digest', payload()); if (r.ok) toast('Weekly digest sent'); else toast(r.error, 'bad', 9000); } catch (er) { toastErr(er); }
    }));
    bindModalForm(form, m.el.querySelector('#save'), async () => {
      if (c.id) await put(`/notifications/${c.id}`, payload()); else await post('/notifications', payload());
      m.close();
      toast('Saved');
      await reload();
    });
  };

  on(el, 'click', '#add', () => {
    const pick = modal({ title: 'Choose channel type', body: html`<div class="type-pick">${types.map((t) => html`<button data-type="${t.type}"><b>${t.label}</b><span>${t.description}</span></button>`)}</div>` });
    on(pick.el, 'click', '[data-type]', (e, b) => { pick.close(); showForm({ type: b.dataset.type, config: {} }); });
  });
  on(el, 'click', '[data-edit]', (e, b) => showForm(list.find((x) => x.id === +b.dataset.edit)));
  on(el, 'click', '[data-test]', (e, b) => busy(b, async () => {
    try { const r = await post(`/notifications/${b.dataset.test}/test`); if (r.ok) toast('Test notification sent'); else toast(r.error, 'bad', 9000); } catch (er) { toastErr(er); }
  }));
  on(el, 'click', '[data-del]', async (e, b) => {
    const c = list.find((x) => x.id === +b.dataset.del);
    if (!(await confirmBox({ title: 'Delete channel', message: html`Delete channel <b>${c.name}</b>?`, danger: true, okText: 'Delete' }))) return;
    try { await del(`/notifications/${c.id}`); toast('Deleted'); await reload(); } catch (er) { toastErr(er); }
  });
}
