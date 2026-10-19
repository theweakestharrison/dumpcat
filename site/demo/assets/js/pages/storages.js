import { get, post, put, del, qs } from '../api.js';
import { html, setHTML, icon, on, fmtBytes, fmtAgo, fmtDate, toast, toastErr, confirmBox, modal, busy, empty, fieldsHTML, readFields, bindShowIf, dropdown, bindModalForm } from '../ui.js';
import { canOperate, isAdmin } from '../app.js';
import { storageIcon } from '../components.js';

export const title = 'Storage';

const OAUTH_NAME = { gdrive: 'Google', onedrive: 'Microsoft', dropbox: 'Dropbox' };
// the storage type picker, by kind of storage
const TYPE_GROUPS = [
  ['On this server', ['local']],
  ['Object storage', ['s3', 'b2', 'azureblob']],
  ['Cloud drives', ['gdrive', 'onedrive', 'dropbox']],
  ['File servers', ['sftp', 'ftp', 'webdav']],
  ['Other', ['rclone']],
];
const RCLONE_NAME = { gdrive: 'drive', onedrive: 'onedrive', dropbox: 'dropbox' };

export async function render(ctx) {
  const { el } = ctx;
  const [{ types, oauth_callback: callbackURL }, initial] = await Promise.all([get('/storage-types'), get('/storages')]);
  let list = initial;
  const typeDef = (t) => types.find((x) => x.type === t);

  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>Storage</h1><div class="sub">Local/NFS directories, S3 &amp; S3-compatible, Google Drive, OneDrive, FTP/FTPS, SFTP, WebDAV, B2, Azure, Dropbox and any rclone backend</div></div>
        ${canOperate() ? html`<button class="btn btn-primary" id="add">${icon('plus')}Add storage</button>` : ''}</div>
      <div class="card"><div class="table-wrap">${list.length ? html`<table class="table"><thead><tr><th>Name / location</th><th>Type</th><th>Last test</th><th class="hide-md">Used by jobs</th><th></th></tr></thead><tbody>
        ${list.map((s) => html`<tr>
          <td style="max-width:300px"><b>${s.name}</b><div class="sub mono" style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${s.describe}">${s.describe || html`<span style="color:var(--critical-ink)">${s.config_error}</span>`}</div></td>
          <td><span class="inline" style="gap:6px">${icon(storageIcon(s.type))}${s.type_label}</span></td>
          <td>${s.last_test_at ? html`${s.last_test_ok ? html`<span class="badge good">${icon('check')}OK</span>` : html`<span class="badge bad" title="${s.last_test_msg}">${icon('x')}Error</span>`} <span class="sub">${fmtAgo(s.last_test_at)}</span>
            <div class="sub" style="max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${s.last_test_msg}">${s.last_test_msg}</div>` : html`<span class="muted">not tested</span>`}</td>
          <td class="hide-md">${s.in_use.length ? html`<span class="chips">${s.in_use.map((j) => html`<span class="badge outline">${j}</span>`)}</span>` : html`<span class="muted">—</span>`}</td>
          <td class="actions">${canOperate() ? html`
            <button class="btn btn-sm" data-test="${s.id}">${icon('zap')}Test</button>
            <button class="btn btn-sm btn-ghost btn-icon" data-edit="${s.id}" title="Edit">${icon('edit')}</button>
            <span class="dropdown"><button class="btn btn-sm btn-ghost btn-icon" data-more="${s.id}" title="More">${icon('chevron')}</button></span>` : ''}</td></tr>`)}
      </tbody></table>` : empty('No storage yet. Add at least one location to store backups.', 'cloud')}</div></div>
      <div class="alert info" style="margin-top:16px">${icon('info')}<div><b>Disaster recovery:</b> every backup is stored with a <code>manifest.json</code>. On a fresh DumpCat install, just add the storage again and click ${icon('upload')} <b>Import catalog</b> to see all backups again and restore them right away.</div></div>`);
  };
  draw();
  const reload = async () => { list = await get('/storages'); draw(); };

  // ---------- form ----------
  const openForm = (existing) => {
    if (!existing) {
      const allowed = types.filter((t) => isAdmin() || (t.type !== 'local' && t.type !== 'rclone'));
      const grouped = new Set(TYPE_GROUPS.flatMap(([, ids]) => ids));
      const groups = TYPE_GROUPS.map(([name, ids]) => [name, ids.map((id) => allowed.find((t) => t.type === id)).filter(Boolean)]);
      groups[groups.length - 1][1].push(...allowed.filter((t) => !grouped.has(t.type)));
      const pick = modal({
        title: 'Choose storage type', size: 'lg',
        body: html`${!isAdmin() ? html`<div class="alert info" style="margin-bottom:14px">${icon('info')}<div>Only admins can configure local directories and custom rclone remotes.</div></div>` : ''}
          <div class="type-pick">${groups.filter(([, list]) => list.length).map(([name, list]) => html`<section><div class="type-group-title">${name}</div>
            <div class="type-grid">${list.map((t) => html`<button type="button" class="type-card" data-type="${t.type}">
              <span class="type-icon">${icon(storageIcon(t.type))}</span><span class="type-text"><b>${t.label}</b><span>${t.description}</span></span></button>`)}</div></section>`)}</div>`,
      });
      on(pick.el, 'click', '[data-type]', (e, b) => { pick.close(); showForm({ type: b.dataset.type, name: '', config: {} }); });
      pick.el.querySelector('[data-type]')?.focus();
      return;
    }
    showForm(existing);
  };

  const showForm = (st) => {
    const td = typeDef(st.type);
    const isNew = !st.id;
    // OAuth types: client credentials, then the authorize button, then the manual token field (collapsed)
    const ti = td.oauth ? td.fields.findIndex((f) => f.key === 'token') : -1;
    const tokenF = ti >= 0 ? td.fields[ti] : null;
    const before = tokenF ? td.fields.slice(0, ti) : td.fields;
    const after = tokenF ? td.fields.slice(ti + 1) : [];
    const tokenShowIf = tokenF && tokenF.show_if ? JSON.stringify(tokenF.show_if) : '';
    const tokenSaved = tokenF && st.config && st.config.token;
    const m = modal({
      title: `${isNew ? 'Add' : 'Edit'}: ${td.label}`, size: 'lg',
      body: html`<form id="st-form" autocomplete="off">
        <div class="field" style="margin-bottom:14px"><label class="req">Name</label><input type="text" name="__name" value="${st.name}" placeholder="e.g. s3-offsite, gdrive-company" required></div>
        <p class="muted small" style="margin-top:0">${td.description}</p>
        <div class="form-grid">${fieldsHTML(before, st.config || {})}</div>
        ${tokenF ? html`<div data-showif="${tokenShowIf}">
          <div class="alert info" style="margin-top:14px">${icon('key')}<div>
            <button type="button" class="btn btn-sm btn-primary" id="oauth">${icon('link')}Authorize with ${OAUTH_NAME[st.type]}</button>
            <span class="help" style="display:block;margin-top:6px">Signs in with the Client ID/Secret above and stores the token. No browser here? Run <code>rclone authorize "${RCLONE_NAME[st.type]}"</code> on another machine and paste the JSON below.</span></div></div>
          <details class="adv" style="margin-top:10px"><summary>${tokenSaved ? 'Token saved — replace it manually' : 'Paste a token manually'}</summary>
            <div class="form-grid">${fieldsHTML([{ ...tokenF, show_if: null }], st.config || {})}</div></details></div>` : ''}
        ${after.length ? html`<div class="form-grid" style="margin-top:14px">${fieldsHTML(after, st.config || {})}</div>` : ''}
        <div id="st-test" style="margin-top:14px"></div></form>`,
      footer: html`${isNew ? html`<button class="btn btn-ghost" id="change-type">${icon('back')}Change type</button>` : ''}<button class="btn" id="test">${icon('zap')}Test</button><span style="flex:1"></span><button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="save">${icon('check')}Save</button>`,
    });
    const form = m.el.querySelector('#st-form');
    m.el.querySelector('#change-type')?.addEventListener('click', () => { m.close(); openForm(null); });
    bindShowIf(form, td.fields);
    const payload = () => ({ id: st.id || 0, name: form.__name.value.trim(), type: st.type, config: readFields(form, td.fields) });
    m.el.querySelector('#test').onclick = (e) => busy(e.currentTarget, async () => {
      const box = m.el.querySelector('#st-test');
      try {
        const r = await post('/storages/test', payload());
        setHTML(box, r.ok ? html`<div class="alert good">${icon('check')}<div>${r.message}</div></div>` : html`<div class="alert bad">${icon('alert')}<div>${r.error}</div></div>`);
      } catch (er) { setHTML(box, html`<div class="alert bad">${icon('alert')}<div>${er.message}</div></div>`); }
    });
    bindModalForm(form, m.el.querySelector('#save'), async () => {
      const p = payload();
      if (st.id) await put(`/storages/${st.id}`, p); else await post('/storages', p);
      m.close();
      toast('Storage saved — click Test to check it');
      await reload();
    });
    const ob = m.el.querySelector('#oauth');
    if (ob) ob.onclick = () => oauthFlow(st, form, td);
  };

  const oauthFlow = (st, form, td) => {
    const cfg = readFields(form, td.fields);
    const om = modal({
      title: `Authorize ${OAUTH_NAME[st.type]}`, size: 'lg',
      body: html`
        <div class="field"><label>How to receive the result</label>
          <label class="check"><input type="radio" name="omode" value="manual" checked><span><b>Paste the redirect URL</b> (always works)<span class="help">After you consent, the browser is redirected to <code>http://127.0.0.1:53682/…</code> or <code>http://localhost:53682/…</code> and shows a connection error — <b>that is expected</b>: copy the full URL from the address bar and paste it here. This redirect URI must be registered in the OAuth client (Google: "Desktop app" type; Microsoft: "Mobile and desktop" platform with <code>http://localhost</code>).</span></span></label>
          <label class="check" style="margin-top:8px"><input type="radio" name="omode" value="callback" ${callbackURL ? '' : 'disabled'}><span><b>Automatic callback</b>${callbackURL ? html`<span class="help">Register this redirect URI: <code>${callbackURL}</code></span>` : html`<span class="help">Requires a Base URL (HTTPS) in Settings.</span>`}</span></label>
        </div>
        <div id="o-step" style="margin-top:14px"></div>`,
      footer: html`<button class="btn" data-close>Close</button><button class="btn btn-primary" id="o-start">${icon('link')}Open sign-in page</button>`,
    });
    let state = '';
    let pollTimer = null;
    const finish = (res) => {
      clearInterval(pollTimer);
      const set = (k, v) => { const i = form.querySelector(`[name="${k}"]`); if (i && v) { i.value = v; i.dataset.saved = ''; } };
      set('token', res.token);
      set('drive_id', res.drive_id);
      if (res.drive_type) { const s = form.querySelector('[name="drive_type"]'); if (s) s.value = res.drive_type; }
      om.close();
      toast(`Got ${OAUTH_NAME[st.type]} token${res.account ? ' — ' + res.account : ''}. Click Save to finish.`, 'good', 8000);
    };
    om.el.querySelector('#o-start').onclick = (e) => busy(e.currentTarget, async () => {
      const mode = om.el.querySelector('input[name=omode]:checked').value;
      try {
        const r = await post('/oauth/start', { type: st.type, config: cfg, storage_id: st.id || 0, mode });
        state = r.state;
        window.open(r.auth_url, '_blank', 'noopener');
        const step = om.el.querySelector('#o-step');
        if (mode === 'manual') {
          setHTML(step, html`<div class="alert info">${icon('info')}<div>The sign-in page opened in a new tab (if not: <a href="${r.auth_url}" target="_blank" rel="noopener">click here</a>). Redirect URI: <code>${r.redirect_uri}</code></div></div>
            <div class="field" style="margin-top:12px"><label>Paste the URL after the redirect (or the code)</label><textarea id="o-url" rows="3" placeholder="http://127.0.0.1:53682/?state=...&code=..."></textarea></div>
            <button class="btn btn-primary" id="o-ex" style="margin-top:10px">${icon('check')}Finish</button>`);
          step.querySelector('#o-ex').onclick = (ev) => busy(ev.currentTarget, async () => {
            try { finish(await post('/oauth/exchange', { state, input: step.querySelector('#o-url').value })); } catch (er) { toastErr(er); }
          });
        } else {
          setHTML(step, html`<div class="alert info">${icon('loader', 'spin')}<div>Waiting for authorization in the new tab…</div></div>`);
          pollTimer = setInterval(async () => {
            try {
              const res = await get('/oauth/result' + qs({ state }));
              if (res.done) { clearInterval(pollTimer); if (res.result) finish(res.result); else toast(res.error, 'bad'); }
            } catch { /* ignore */ }
          }, 2000);
        }
      } catch (er) { toastErr(er); }
    });
  };

  // ---------- actions ----------
  const find = (id) => list.find((x) => x.id === +id);
  const actions = {
    async del(id) {
      const s = find(id);
      if (!(await confirmBox({ title: 'Delete storage', message: html`Remove <b>${s.name}</b> from DumpCat? Backup files on the storage are <b>not</b> deleted, but DumpCat will stop tracking them.`, danger: true, okText: 'Delete' }))) return;
      try { await del(`/storages/${s.id}`); toast('Deleted'); await reload(); } catch (er) { toastErr(er); }
    },
    async usage(id) {
      toast('Measuring usage…', 'info', 2500);
      try {
        const r = await get(`/storages/${id}/usage`);
        if (!r.ok) return toast(r.error, 'warn');
        const u = r.usage;
        toast(`Usage: ${fmtBytes(u.used)} used${u.total > 0 ? ` / ${fmtBytes(u.total)} (${fmtBytes(u.free)} free)` : ''}`, 'info', 8000);
      } catch (er) { toastErr(er); }
    },
    async import(id) {
      const s = find(id);
      if (!(await confirmBox({ title: 'Import backup catalog', message: html`Scan <b>${s.name}</b> for <code>manifest.json</code> files and add any backups that aren't listed yet. Data on the storage is not changed.`, okText: 'Scan' }))) return;
      toast('Scanning storage…', 'info', 4000);
      try {
        const r = await post(`/storages/${s.id}/import`);
        toast(`Found ${r.found} manifests: ${r.imported} imported, ${r.linked} locations linked, ${r.skipped} skipped`, 'good', 9000);
        if (r.errors && r.errors.length) toast(r.errors.slice(0, 3).join('; '), 'bad', 10000);
      } catch (er) { toastErr(er); }
    },
    browse(id) {
      const s = find(id);
      const m = modal({ title: `Browse: ${s.name}`, size: 'xl', body: html`<div id="br"></div>`, footer: html`<button class="btn" data-close>Close</button>` });
      const box = m.el.querySelector('#br');
      const go = async (path) => {
        setHTML(box, html`<div class="loading-page">${icon('loader', 'spin')}</div>`);
        try {
          const r = await get(`/storages/${s.id}/browse` + qs({ path }));
          const parts = r.path ? r.path.split('/') : [];
          const crumbs = [html`<a href="#" data-path="">${s.describe}</a>`];
          parts.forEach((p, i) => crumbs.push(html` / <a href="#" data-path="${parts.slice(0, i + 1).join('/')}">${p}</a>`));
          const items = r.entries.sort((a, c) => (c.is_dir - a.is_dir) || a.name.localeCompare(c.name));
          setHTML(box, html`<div class="mono small" style="margin-bottom:10px">${crumbs}</div>
            <div class="table-wrap" style="max-height:60vh;overflow:auto"><table class="table"><thead><tr><th>Name</th><th class="right">Size</th><th>Modified</th></tr></thead><tbody>
            ${parts.length ? html`<tr><td colspan="3"><a href="#" data-path="${parts.slice(0, -1).join('/')}">${icon('back')} ..</a></td></tr>` : ''}
            ${items.map((x) => html`<tr><td>${x.is_dir ? html`<a href="#" data-path="${x.path}">${icon('folder')} ${x.name}</a>` : html`<span>${icon('file')} ${x.name}</span>`}</td>
              <td class="right num">${x.is_dir ? '' : fmtBytes(x.size)}</td><td class="small">${fmtDate(x.mod_time)}</td></tr>`)}
            ${!items.length ? html`<tr><td colspan="3" class="muted">Empty directory</td></tr>` : ''}</tbody></table></div>`);
        } catch (er) { setHTML(box, html`<div class="alert bad">${icon('alert')}<div>${er.message}</div></div>`); }
      };
      on(box, 'click', '[data-path]', (ev, a) => { ev.preventDefault(); go(a.dataset.path); });
      go('');
    },
  };

  on(el, 'click', '#add', () => openForm(null));
  on(el, 'click', '[data-edit]', async (e, b) => openForm(await get(`/storages/${b.dataset.edit}`)));
  on(el, 'click', '[data-test]', (e, b) => busy(b, async () => {
    try {
      const r = await post(`/storages/${b.dataset.test}/test`);
      if (r.ok) toast(r.message); else toast(r.error, 'bad', 10000);
      await reload();
    } catch (er) { toastErr(er); }
  }));
  on(el, 'click', '[data-more]', (e, b) => {
    e.stopPropagation();
    const id = b.dataset.more;
    dropdown(b, [
      { label: 'Browse files', icon: 'folder', onClick: () => actions.browse(id) },
      { label: 'Show usage', icon: 'hdd', onClick: () => actions.usage(id) },
      { label: 'Import backup catalog', icon: 'upload', onClick: () => actions.import(id) },
      { label: 'Delete', icon: 'trash', danger: true, onClick: () => actions.del(id) },
    ]);
  });
}
