// UI helpers: safe HTML templating, icons, formatters, modals, toasts, forms.

class Raw {
  constructor(s) { this.s = s; }
  toString() { return this.s; }
}
export const raw = (s) => new Raw(String(s ?? ''));

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
export const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ESC[c]);

function val(v) {
  if (v === null || v === undefined || v === false) return '';
  if (v instanceof Raw) return v.s;
  if (Array.isArray(v)) return v.map(val).join('');
  return esc(v);
}

/** Tagged template: interpolated values are escaped unless wrapped in raw()/html``. */
export function html(strings, ...vals) {
  let out = strings[0];
  for (let i = 0; i < vals.length; i++) out += val(vals[i]) + strings[i + 1];
  return new Raw(out);
}

export function setHTML(el, content) {
  el.innerHTML = val(content);
  return el;
}

/** Delegated event listener. */
export function on(root, type, selector, fn) {
  const h = (e) => {
    const t = e.target.closest(selector);
    if (t && root.contains(t)) fn(e, t);
  };
  root.addEventListener(type, h);
  return () => root.removeEventListener(type, h);
}

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

// ---------- icons ----------
const P = {
  grid: '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  archive: '<rect x="3" y="4" width="18" height="5" rx="1"/><path d="M5 9v10a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9M10 13h4"/>',
  restore: '<path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/>',
  server: '<rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/>',
  database: '<ellipse cx="12" cy="5.5" rx="8" ry="3"/><path d="M4 5.5v13c0 1.7 3.6 3 8 3s8-1.3 8-3v-13"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
  cloud: '<path d="M7 18a5 5 0 0 1-.6-9.96A6 6 0 0 1 18 9a4.5 4.5 0 0 1-.5 9H7z"/>',
  bell: '<path d="M6 8a6 6 0 1 1 12 0c0 7 3 8 3 8H3s3-1 3-8"/><path d="M10.3 20a2 2 0 0 0 3.4 0"/>',
  users: '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20a6.5 6.5 0 0 1 13 0"/><path d="M16 4.6a3.5 3.5 0 0 1 0 6.8M18 14a6.5 6.5 0 0 1 3.5 6"/>',
  sliders: '<path d="M4 6h10M18 6h2M4 12h4M12 12h8M4 18h12"/><circle cx="16" cy="6" r="2"/><circle cx="10" cy="12" r="2"/><circle cx="18" cy="18" r="2"/>',
  cpu: '<rect x="5" y="5" width="14" height="14" rx="2"/><rect x="9" y="9" width="6" height="6"/><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/>',
  list: '<path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/>',
  logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="M16 17l5-5-5-5M21 12H9"/>',
  play: '<path d="M7 4.5v15l12-7.5z" fill="currentColor" stroke="none"/>',
  edit: '<path d="M4 20h4L19 9l-4-4L4 16v4z"/><path d="M13.5 6.5l4 4"/>',
  trash: '<path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3"/>',
  copy: '<rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/>',
  check: '<path d="M5 12.5l4.5 4.5L19 7"/>',
  x: '<path d="M6 6l12 12M18 6L6 18"/>',
  alert: '<path d="M12 3.5l9 16H3z"/><path d="M12 10v4M12 17h.01"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v6M12 7.5h.01"/>',
  refresh: '<path d="M20 11a8 8 0 0 0-14.9-3M4 4v4h4"/><path d="M4 13a8 8 0 0 0 14.9 3M20 20v-4h-4"/>',
  download: '<path d="M12 4v11M7 10l5 5 5-5M5 20h14"/>',
  upload: '<path d="M12 20V9M7 14l5-5 5 5M5 4h14"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  shield: '<path d="M12 3l8 3v6c0 4.5-3.4 8.4-8 9-4.6-.6-8-4.5-8-9V6z"/><path d="M9 12l2 2 4-4"/>',
  key: '<circle cx="8" cy="15" r="4"/><path d="M11 12l9-9M16 7l3 3M14 9l2 2"/>',
  folder: '<path d="M3 6a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
  file: '<path d="M14 3H6a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8z"/><path d="M14 3v5h5"/>',
  stop: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
  moon: '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  monitor: '<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>',
  menu: '<path d="M4 6h16M4 12h16M4 18h16"/>',
  user: '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
  loader: '<path d="M12 3a9 9 0 1 0 9 9"/>',
  eye: '<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
  chevron: '<path d="M6 9l6 6 6-6"/>',
  dots: '<circle cx="5" cy="12" r="1.2" fill="currentColor"/><circle cx="12" cy="12" r="1.2" fill="currentColor"/><circle cx="19" cy="12" r="1.2" fill="currentColor"/>',
  rocket: '<path d="M5 15c-1.5 1.5-2 5-2 5s3.5-.5 5-2"/><path d="M9 11a14 14 0 0 1 11-8 14 14 0 0 1-8 11l-3 1-2-2z"/><circle cx="15" cy="9" r="1.5"/>',
  back: '<path d="M15 18l-6-6 6-6"/>',
  link: '<path d="M10 14a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1"/><path d="M14 10a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1"/>',
  lock: '<rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/>',
  zap: '<path d="M13 2L4 14h7l-1 8 9-12h-7z"/>',
  layers: '<path d="M12 3l9 5-9 5-9-5z"/><path d="M3 13l9 5 9-5"/>',
  hdd: '<rect x="3" y="13" width="18" height="7" rx="2"/><path d="M5 13l3-8h8l3 8M7 16.5h.01"/>',
  flask: '<path d="M9 3h6M10 3v6.5L4.6 18.4A1.7 1.7 0 0 0 6.1 21h11.8a1.7 1.7 0 0 0 1.5-2.6L14 9.5V3"/><path d="M7.5 15h9"/>',
};

export function icon(name, cls = '') {
  const p = P[name] || P.info;
  return raw(`<svg class="${cls}" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${p}</svg>`);
}

// ---------- formatting ----------
const pad = (n) => String(n).padStart(2, '0');

export function toDate(s) {
  if (!s) return null;
  const d = s instanceof Date ? s : new Date(s);
  if (isNaN(d) || d.getFullYear() < 1971) return null;
  return d;
}

export function fmtDate(s, withSec = false) {
  const d = toDate(s);
  if (!d) return '—';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}${withSec ? ':' + pad(d.getSeconds()) : ''}`;
}

export function fmtAgo(s) {
  const d = toDate(s);
  if (!d) return '—';
  const diff = (Date.now() - d.getTime()) / 1000;
  const a = Math.abs(diff);
  if (a < 45) return diff >= 0 ? 'just now' : 'in a few seconds';
  let t;
  if (a < 3600) t = Math.round(a / 60) + ' min';
  else if (a < 86400) t = Math.round(a / 3600) + ' h';
  else if (a < 86400 * 45) { const n = Math.round(a / 86400); t = n + (n === 1 ? ' day' : ' days'); }
  else { const n = Math.round(a / 86400 / 30); t = n + (n === 1 ? ' month' : ' months'); }
  return diff >= 0 ? t + ' ago' : 'in ' + t;
}

export function fmtBytes(n, digits = 1) {
  if (n === null || n === undefined || n < 0 || isNaN(n)) return '—';
  if (n < 1024) return n + ' B';
  const u = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n >= 100 ? 0 : digits) + ' ' + u[i];
}

export function fmtDur(sec) {
  if (sec === null || sec === undefined || sec < 0 || isNaN(sec)) return '—';
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  if (h) return `${h}h ${pad(m)}m`;
  if (m) return `${m}m ${pad(s)}s`;
  return `${s}s`;
}

export function runDuration(r) {
  const a = toDate(r.started_at), b = toDate(r.finished_at);
  if (!a) return null;
  return ((b ? b.getTime() : Date.now()) - a.getTime()) / 1000;
}

export const METHOD_LABEL = { mysqldump: 'mysqldump', mydumper: 'mydumper', physical: 'Physical (hot)' };
export const TYPE_LABEL = { full: 'Full', incremental: 'Incremental' };

const STATUS = {
  success: ['good', 'check', 'Success'],
  ok: ['good', 'check', 'OK'],
  partial: ['warn', 'alert', 'Partial'],
  failed: ['bad', 'x', 'Failed'],
  running: ['info', 'loader', 'Running'],
  uploading: ['info', 'loader', 'Uploading'],
  queued: ['', 'clock', 'Queued'],
  canceled: ['', 'stop', 'Canceled'],
  deleted: ['', 'trash', 'Deleted'],
  passed: ['good', 'check', 'Passed'],
  warning: ['warn', 'alert', 'Warning'],
  skipped: ['', 'info', 'Skipped'],
};

export function statusBadge(st, text) {
  const [cls, ic, label] = STATUS[st] || ['', 'info', st || '—'];
  return html`<span class="badge ${cls}">${icon(ic, ic === 'loader' ? 'spin' : '')}${text || label}</span>`;
}

export function verifyBadge(st, msg) {
  if (!st) return html`<span class="muted small">not verified</span>`;
  if (st === 'ok') return html`<span class="badge good" title="${msg || ''}">${icon('shield')}Verified</span>`;
  if (st === 'running') return html`<span class="badge info">${icon('loader', 'spin')}Verifying</span>`;
  return html`<span class="badge bad" title="${msg || ''}">${icon('x')}Integrity error</span>`;
}

export function boolIcon(v) {
  return v ? html`<span class="badge good">${icon('check')}Yes</span>` : html`<span class="badge">${icon('x')}No</span>`;
}

// ---------- toasts ----------
/** Success/info toasts auto-hide (paused on hover); error toasts stay until dismissed. */
export function toast(msg, type = 'good', ms = 4500) {
  const box = document.getElementById('toasts');
  while (box.children.length >= 3) box.firstElementChild.remove();
  const el = document.createElement('div');
  el.className = 'toast ' + type;
  el.setAttribute('role', type === 'bad' ? 'alert' : 'status');
  const ic = type === 'good' ? 'check' : type === 'bad' ? 'alert' : 'info';
  setHTML(el, html`${icon(ic)}<div>${msg}</div><button class="toast-x" aria-label="Dismiss">${icon('x')}</button>`);
  el.querySelector('.toast-x').onclick = () => el.remove();
  box.appendChild(el);
  if (type === 'bad') return;
  let timer = setTimeout(() => el.remove(), ms);
  el.addEventListener('mouseenter', () => clearTimeout(timer));
  el.addEventListener('mouseleave', () => { timer = setTimeout(() => el.remove(), 2000); });
}

export function toastErr(e) {
  toast(e && e.message ? e.message : String(e), 'bad');
}

// ---------- modals ----------
const modalStack = [];

function syncInert() {
  const root = document.getElementById('root');
  if (root) root.inert = modalStack.length > 0;
  modalStack.forEach((m, i) => { m.bd.inert = i !== modalStack.length - 1; });
  document.body.classList.toggle('modal-open', modalStack.length > 0);
}

document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || !modalStack.length) return;
  if (document.querySelector('.dropdown-menu')) return; // the menu closes first
  e.preventDefault();
  modalStack[modalStack.length - 1].close();
});

let modalSeq = 0;
/**
 * Accessible modal: stacked (Escape closes the top one only), background made
 * inert (focus stays inside), focus returns to the opener, and a backdrop
 * click does not discard a form the user already edited.
 */
export function modal({ title, body, footer, size = '', onClose }) {
  const opener = document.activeElement;
  const bd = document.createElement('div');
  bd.className = 'modal-backdrop';
  const tid = 'mdl-title-' + (++modalSeq);
  setHTML(bd, html`<div class="modal ${size}" role="dialog" aria-modal="true" aria-labelledby="${tid}">
    <div class="modal-head"><h2 id="${tid}">${title}</h2><button class="btn btn-ghost btn-icon" data-close aria-label="Close">${icon('x')}</button></div>
    <div class="modal-body">${body}</div>
    ${footer ? html`<div class="modal-foot">${footer}</div>` : ''}
  </div>`);
  let dirty = false;
  let closed = false;
  const entry = { bd };
  const close = () => {
    if (closed) return;
    closed = true;
    bd.remove();
    const i = modalStack.indexOf(entry);
    if (i >= 0) modalStack.splice(i, 1);
    syncInert();
    if (opener && opener.isConnected && typeof opener.focus === 'function') opener.focus();
    onClose && onClose();
  };
  entry.close = close;
  bd.addEventListener('input', () => { dirty = true; });
  bd.addEventListener('mousedown', (e) => { if (e.target === bd && !dirty) close(); });
  on(bd, 'click', '[data-close]', close);
  document.body.appendChild(bd);
  modalStack.push(entry);
  syncInert();
  linkLabels(bd);
  // first field; else the primary button; for destructive dialogs the safe "Cancel"
  const first = bd.querySelector('.modal-body input:not([type=hidden]):not([disabled]), .modal-body select:not([disabled]), .modal-body textarea:not([disabled])')
    || bd.querySelector('.modal-foot .btn-primary') || bd.querySelector('.modal-foot [data-close]') || bd.querySelector('[data-close]');
  setTimeout(() => first && first.focus(), 30);
  return { el: bd.querySelector('.modal'), body: bd.querySelector('.modal-body'), close };
}

// ---------- forms: labels, validation, inline errors ----------
let fieldSeq = 0;
/** Gives every unlabeled form control an accessible name (label[for] or aria-label). */
export function linkLabels(root) {
  if (!root || !root.querySelectorAll) return;
  for (const label of root.querySelectorAll('.field > label:not([for]):not(.check)')) {
    const ctl = label.parentElement.querySelector('input:not([type=hidden]), select, textarea');
    if (!ctl) continue;
    if (!ctl.id) ctl.id = 'fld-' + (++fieldSeq);
    label.htmlFor = ctl.id;
  }
  for (const ctl of root.querySelectorAll('input:not([type=hidden]), select, textarea')) {
    if (ctl.labels && ctl.labels.length) continue;
    if (ctl.getAttribute('aria-label') || ctl.getAttribute('aria-labelledby')) continue;
    const name = ctl.placeholder || (ctl.tagName === 'SELECT' && ctl.options[0] ? ctl.options[0].textContent : '') || ctl.name;
    if (name) ctl.setAttribute('aria-label', name.trim());
  }
}

/** Shows an error at the top of a form (and scrolls it into view); null clears it. */
export function formError(form, msg) {
  let box = form.querySelector(':scope > .form-error');
  if (!msg) { if (box) box.remove(); return; }
  if (!box) {
    box = document.createElement('div');
    box.className = 'alert bad form-error';
    box.setAttribute('role', 'alert');
    form.prepend(box);
  }
  setHTML(box, html`${icon('alert')}<div>${msg}</div>`);
  box.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

/** Runs native validation (required, min, pattern...) and reports the first problem. */
export function validate(form) {
  // controls in hidden sections never block saving
  const off = [...form.querySelectorAll('[required], [pattern], [min], [max]')].filter((c) => !c.disabled && c.closest('.hidden'));
  off.forEach((c) => { c.disabled = true; });
  const ok = form.checkValidity();
  if (!ok) {
    for (const c of form.querySelectorAll(':invalid')) { const d = c.closest('details'); if (d) d.open = true; }
    form.reportValidity();
  }
  off.forEach((c) => { c.disabled = false; });
  return ok;
}

/**
 * Wires a form shown in a modal: Enter in a text field and the save button both
 * run native validation first, then save(); a thrown error is shown inside the form.
 */
export function bindModalForm(form, btn, save) {
  const run = () => {
    if (btn.disabled) return;
    formError(form, null);
    if (!validate(form)) return;
    busy(btn, async () => {
      try { await save(); } catch (e) { formError(form, e.message); }
    });
  };
  btn.onclick = run;
  form.addEventListener('submit', (e) => { e.preventDefault(); run(); });
  form.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' || e.isComposing || e.shiftKey) return;
    if (e.target.matches('input:not([type=checkbox]):not([type=radio]):not([type=button]):not([type=submit])')) { e.preventDefault(); run(); }
  });
}

// ---------- polled re-renders ----------
const lastHTML = new WeakMap();
/**
 * Re-renders el only when the markup changed and the user is not interacting
 * with it (focus inside, a text selection, or an open menu). Focus is kept on
 * an element with the same id / data-key.
 */
export function patch(el, content, { force = false } = {}) {
  const markup = val(content);
  if (!force && lastHTML.get(el) === markup) return false;
  if (!force) {
    const active = document.activeElement;
    const sel = window.getSelection && window.getSelection();
    if (document.querySelector('.dropdown-menu')) return false;
    if (active && el.contains(active) && active.matches('input, select, textarea')) return false;
    if (sel && !sel.isCollapsed && sel.rangeCount && el.contains(sel.getRangeAt(0).commonAncestorContainer)) return false;
  }
  const active = document.activeElement;
  const key = active && el.contains(active) ? (active.id || active.dataset.key) : null;
  el.innerHTML = markup;
  lastHTML.set(el, markup);
  if (key) {
    const again = el.querySelector(`#${CSS.escape(key)}`) || el.querySelector(`[data-key="${CSS.escape(key)}"]`);
    if (again) again.focus();
  }
  return true;
}

export function confirmBox({ title = 'Confirm', message, okText = 'OK', danger = false, typeText = '' }) {
  return new Promise((resolve) => {
    let done = false;
    const m = modal({
      title,
      body: html`<div>${message}</div>${typeText ? html`<div class="field" style="margin-top:14px"><label>Type <code>${typeText}</code> to confirm</label><input type="text" id="cf-type" autocomplete="off"></div>` : ''}`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn ${danger ? 'btn-danger-solid' : 'btn-primary'}" id="cf-ok" ${typeText ? 'disabled' : ''}>${okText}</button>`,
      onClose: () => { if (!done) resolve(false); },
    });
    const okb = m.el.querySelector('#cf-ok');
    if (typeText) {
      m.el.querySelector('#cf-type').addEventListener('input', (e) => { okb.disabled = e.target.value.trim() !== typeText; });
    }
    okb.addEventListener('click', () => { done = true; m.close(); resolve(true); });
  });
}

// ---------- misc ----------
export function debounce(fn, ms = 300) {
  let t;
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('Copied');
  } catch {
    const ta = document.createElement('textarea');
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); toast('Copied'); } catch { toast('Could not copy', 'bad'); }
    ta.remove();
  }
}

/** Disable a button while an async action runs. */
export async function busy(btn, fn) {
  if (!btn) return fn();
  const old = btn.innerHTML;
  btn.disabled = true;
  btn.innerHTML = `${icon('loader', 'spin')}<span>${btn.textContent.trim()}</span>`;
  try { return await fn(); } finally { btn.disabled = false; btn.innerHTML = old; }
}

/** Empty state, optionally with a call-to-action link: action = { label, href, icon }. */
export function empty(text, ic = 'archive', action = null) {
  return html`<div class="empty">${icon(ic)}<div>${text}</div>${action ? html`<a class="btn btn-primary" href="${action.href}">${icon(action.icon || 'plus')}${action.label}</a>` : ''}</div>`;
}

export function pager(total, limit, offset) {
  if (total <= limit) return '';
  const page = Math.floor(offset / limit) + 1;
  const pages = Math.ceil(total / limit);
  return html`<div class="pagination"><span>Page ${page}/${pages} · ${total} items</span>
    <button class="btn btn-sm" data-page="${offset - limit}" ${offset <= 0 ? 'disabled' : ''}>${icon('back')}Previous</button>
    <button class="btn btn-sm" data-page="${offset + limit}" ${offset + limit >= total ? 'disabled' : ''}>Next</button></div>`;
}

// ---------- cron ----------
const DOW = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
const isNum = (s) => /^\d+$/.test(s);

export function describeCron(expr) {
  expr = (expr || '').trim();
  if (!expr) return 'Manual only';
  const desc = { '@hourly': 'Every hour', '@daily': 'Daily at 00:00', '@midnight': 'Daily at 00:00', '@weekly': 'Weekly (Sun 00:00)', '@monthly': 'Day 1 of every month at 00:00', '@yearly': 'Yearly (Jan 1)', '@annually': 'Yearly (Jan 1)' };
  if (desc[expr]) return desc[expr];
  if (expr.startsWith('@every ')) return 'Every ' + expr.slice(7);
  const p = expr.split(/\s+/);
  if (p.length !== 5) return expr;
  const [mi, h, dom, mon, dow] = p;
  const at = (hh) => `${pad(hh)}:${pad(mi)}`;
  if (mi.startsWith('*/') && h === '*' && dom === '*' && dow === '*') return `Every ${mi.slice(2)} minutes`;
  if (isNum(mi) && h === '*' && dom === '*' && dow === '*') return `Every hour (minute ${mi})`;
  if (isNum(mi) && h.startsWith('*/') && dom === '*' && dow === '*') return `Every ${h.slice(2)} hours (minute ${mi})`;
  if (!isNum(mi) || mon !== '*') return expr;
  const hours = h.split(',');
  if (!hours.every(isNum)) return expr;
  const times = hours.map(at).join(', ');
  if (dom === '*' && dow === '*') return `Daily at ${times}`;
  if (dom === '*' && /^[0-7,\-]+$/.test(dow)) {
    const days = dow.split(',').map((d) => d.includes('-') ? d.split('-').map((x) => DOW[+x]).join('–') : DOW[+d]).join(', ');
    return `${days} weekly at ${times}`;
  }
  if (isNum(dom) && dow === '*') return `Day ${dom} of every month at ${times}`;
  return expr;
}

// ---------- dynamic fields (storage / notification types) ----------
export const SECRET_MASK = '__SECRET_UNCHANGED__';

export function fieldsHTML(fields, values = {}) {
  return fields.map((f) => {
    const v = values[f.key] ?? f.default ?? '';
    const saved = f.secret && v === SECRET_MASK;
    const id = 'f_' + f.key;
    const help = f.help ? html`<div class="help">${f.help}</div>` : '';
    const wide = f.type === 'textarea' ? 'full' : '';
    const showIf = f.show_if ? JSON.stringify(f.show_if) : '';
    const attrs = html`data-field="${f.key}" data-showif="${showIf}"`;
    const ph = saved ? '•••••• saved — leave blank to keep' : (f.placeholder || '');
    const req = f.required && !saved ? 'required' : '';
    let input;
    switch (f.type) {
      case 'select':
        input = html`<select id="${id}" name="${f.key}" ${req}>${(f.options || []).map((o) => html`<option value="${o.value}" ${String(v) === o.value ? 'selected' : ''}>${o.label}</option>`)}</select>`;
        break;
      case 'textarea':
        input = html`<textarea id="${id}" name="${f.key}" rows="5" placeholder="${ph}" data-saved="${saved ? 1 : ''}" spellcheck="false" ${req}>${saved ? '' : v}</textarea>`;
        break;
      case 'bool':
        return html`<div class="field" ${attrs}><label class="check"><input type="checkbox" id="${id}" name="${f.key}" ${v === 'true' || v === true ? 'checked' : ''}><span>${f.label}${help}</span></label></div>`;
      case 'number':
        input = html`<input type="number" id="${id}" name="${f.key}" value="${v}" placeholder="${ph}" ${req}>`;
        break;
      case 'password':
        input = html`<input type="password" id="${id}" name="${f.key}" value="${saved ? '' : v}" placeholder="${ph}" data-saved="${saved ? 1 : ''}" autocomplete="new-password" ${req}>`;
        break;
      default:
        input = html`<input type="text" id="${id}" name="${f.key}" value="${v}" placeholder="${ph}" autocomplete="off" spellcheck="false" ${req}>`;
    }
    return html`<div class="field ${wide}" ${attrs}><label for="${id}" class="${f.required ? 'req' : ''}">${f.label}</label>${input}${help}</div>`;
  });
}

export function readFields(root, fields) {
  const out = {};
  for (const f of fields) {
    const el = root.querySelector(`[name="${f.key}"]`);
    if (!el) continue;
    if (f.type === 'bool') { out[f.key] = el.checked ? 'true' : 'false'; continue; }
    let v = el.value;
    if (f.secret && el.dataset.saved === '1' && v === '') v = SECRET_MASK;
    out[f.key] = v;
  }
  return out;
}

export function bindShowIf(root, fields) {
  const apply = () => {
    for (const box of root.querySelectorAll('[data-showif]')) {
      const cond = box.dataset.showif;
      if (!cond) continue;
      let ok = true;
      for (const [k, vals] of Object.entries(JSON.parse(cond))) {
        const el = root.querySelector(`[name="${k}"]`);
        const v = el ? el.value : '';
        if (!vals.includes(v)) ok = false;
      }
      box.classList.toggle('hidden', !ok);
    }
  };
  root.addEventListener('change', apply);
  apply();
  return apply;
}

export function formData(form) {
  const o = {};
  for (const el of form.elements) {
    if (!el.name) continue;
    if (el.type === 'checkbox') o[el.name] = el.checked;
    else if (el.type === 'number') o[el.name] = el.value === '' ? 0 : Number(el.value);
    else o[el.name] = el.value;
  }
  return o;
}

let menuOwner = null;

/**
 * Action menu attached to <body> with fixed positioning (never clipped by a
 * scrolling table), flips upward near the bottom, keyboard navigable.
 */
export function dropdown(btn, items) {
  const wasOpenForBtn = menuOwner === btn;
  closeDropdowns();
  if (wasOpenForBtn) return; // second click toggles closed
  const menu = document.createElement('div');
  menu.className = 'dropdown-menu';
  menu.setAttribute('role', 'menu');
  const parts = [];
  items.forEach((it, i) => {
    if (it.danger && i > 0 && !items[i - 1].danger) parts.push(html`<div class="sep" role="separator"></div>`);
    parts.push(html`<button role="menuitem" data-i="${i}" class="${it.danger ? 'btn-danger' : ''}">${it.icon ? icon(it.icon) : ''}${it.label}</button>`);
  });
  setHTML(menu, parts);
  document.body.appendChild(menu);
  const r = btn.getBoundingClientRect();
  const mw = menu.offsetWidth, mh = menu.offsetHeight;
  const left = Math.max(8, Math.min(r.right - mw, window.innerWidth - mw - 8));
  const top = r.bottom + 4 + mh > window.innerHeight - 8 ? Math.max(8, r.top - mh - 4) : r.bottom + 4;
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';
  menuOwner = btn;
  btn.setAttribute('aria-haspopup', 'menu');
  btn.setAttribute('aria-expanded', 'true');
  const buttons = () => Array.from(menu.querySelectorAll('button[role=menuitem]'));
  menu.addEventListener('click', (e) => {
    const b = e.target.closest('button[data-i]');
    if (!b) return;
    closeDropdowns(true);
    items[+b.dataset.i].onClick();
  });
  menu.addEventListener('keydown', (e) => {
    const list = buttons();
    const i = list.indexOf(document.activeElement);
    if (e.key === 'ArrowDown') { e.preventDefault(); list[(i + 1) % list.length].focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); list[(i - 1 + list.length) % list.length].focus(); }
    else if (e.key === 'Home') { e.preventDefault(); list[0].focus(); }
    else if (e.key === 'End') { e.preventDefault(); list[list.length - 1].focus(); }
    else if (e.key === 'Escape' || e.key === 'Tab') { e.preventDefault(); closeDropdowns(true); }
  });
  buttons()[0]?.focus();
  setTimeout(() => {
    document.addEventListener('click', onDocClick);
    window.addEventListener('resize', onViewportChange);
    window.addEventListener('scroll', onViewportChange, true);
  }, 0);
}

function onDocClick(e) {
  if (!e.target.closest('.dropdown-menu')) closeDropdowns();
}

function onViewportChange(e) {
  if (e && e.target && e.target.closest && e.target.closest('.dropdown-menu')) return;
  closeDropdowns();
}

export function closeDropdowns(returnFocus = false) {
  document.querySelectorAll('.dropdown-menu').forEach((m) => m.remove());
  document.removeEventListener('click', onDocClick);
  window.removeEventListener('resize', onViewportChange);
  window.removeEventListener('scroll', onViewportChange, true);
  if (menuOwner) {
    menuOwner.setAttribute('aria-expanded', 'false');
    if (returnFocus && menuOwner.isConnected) menuOwner.focus();
  }
  menuOwner = null;
}
