import { get, qs } from '../api.js';
import { html, setHTML, on, fmtDate, empty, pager, debounce } from '../ui.js';

export const title = 'Audit log';

export async function render(ctx) {
  const { el } = ctx;
  const f = { limit: 100, offset: 0, q: '' };
  setHTML(el, html`<div class="page-head"><div><h1>Audit log</h1><div class="sub">Who did what, when, and from where</div></div></div>
    <div class="card"><div class="filters"><input type="search" id="q" placeholder="Search by user, action, target..."></div><div id="list"></div></div>`);
  const box = el.querySelector('#list');
  const load = async () => {
    const r = await get('/audit' + qs(f));
    setHTML(box, r.items.length ? html`<div class="table-wrap"><table class="table"><thead><tr><th>Time</th><th>User</th><th>Action</th><th>Target</th><th class="hide-sm">Details</th><th class="hide-sm">IP</th></tr></thead><tbody>
      ${r.items.map((a) => html`<tr><td class="nowrap">${fmtDate(a.ts, true)}</td><td>${a.username || '—'}</td>
        <td><span class="badge ${a.action.includes('fail') ? 'bad' : a.action.includes('delete') ? 'warn' : 'outline'}">${a.action}</span></td>
        <td class="mono small">${a.target}</td><td class="small hide-sm">${a.details}</td><td class="mono small hide-sm">${a.ip}</td></tr>`)}
      </tbody></table></div>${pager(r.total, f.limit, f.offset)}` : empty('No entries', 'list'));
  };
  await load();
  el.querySelector('#q').addEventListener('input', debounce((e) => { f.q = e.target.value.trim(); f.offset = 0; load(); }, 350));
  on(el, 'click', '[data-page]', (e, b) => { f.offset = Math.max(0, +b.dataset.page); load(); });
}
