import { get, post, setCsrf, setUnauthorizedHandler, setNewBuildHandler } from './api.js';
import { html, setHTML, icon, on, toast, toastErr, busy, formData, closeDropdowns, dropdown, linkLabels } from './ui.js';

export const app = { user: null, version: '', csrf: '' };

const isAdmin = () => app.user && app.user.role === 'admin';
const canOperate = () => app.user && (app.user.role === 'admin' || app.user.role === 'operator');
export { isAdmin, canOperate };

const NAV = [
  { group: 'Overview', items: [{ path: '/dashboard', label: 'Dashboard', icon: 'grid' }] },
  { group: 'Backup', items: [
    { path: '/jobs', label: 'Backup jobs', icon: 'clock' },
    { path: '/runs', label: 'Backups', icon: 'archive' },
    { path: '/restores', label: 'Restores', icon: 'restore' },
    { path: '/drills', label: 'Restore drills', icon: 'flask' },
    { path: '/binlogs', label: 'Binlog & PITR', icon: 'layers' },
  ] },
  { group: 'Configuration', items: [
    { path: '/servers', label: 'Database servers', icon: 'database' },
    { path: '/agents', label: 'Agents', icon: 'link' },
    { path: '/wireguard', label: 'WireGuard', icon: 'shield' },
    { path: '/storages', label: 'Storage', icon: 'cloud' },
    { path: '/notifications', label: 'Notifications', icon: 'bell', admin: true },
  ] },
  { group: 'Administration', items: [
    { path: '/users', label: 'Users', icon: 'users', admin: true },
    { path: '/settings', label: 'Settings', icon: 'sliders', admin: true },
    { path: '/system', label: 'System', icon: 'cpu' },
    { path: '/audit', label: 'Audit log', icon: 'list', admin: true },
  ] },
];

const ROUTES = [
  ['/dashboard', 'dashboard'],
  ['/jobs', 'jobs'],
  ['/jobs/new', 'jobedit'],
  ['/jobs/:id/edit', 'jobedit'],
  ['/runs', 'runs'],
  ['/runs/:id', 'rundetail'],
  ['/runs/:id/restore', 'restore'],
  ['/restores', 'restores'],
  ['/restores/:id', 'restoredetail'],
  ['/drills', 'drills'],
  ['/drills/new', 'drilledit'],
  ['/drills/:id/edit', 'drilledit'],
  ['/drill-runs/:id', 'drillrun'],
  ['/binlogs', 'binlogs'],
  ['/pitr', 'pitr'],
  ['/servers', 'servers'],
  ['/agents', 'agents'],
  ['/wireguard', 'wireguard'],
  ['/storages', 'storages'],
  ['/notifications', 'notifications'],
  ['/users', 'users'],
  ['/settings', 'settings'],
  ['/system', 'system'],
  ['/audit', 'audit'],
  ['/profile', 'profile'],
];

function matchRoute(path) {
  for (const [pattern, mod] of ROUTES) {
    const pp = pattern.split('/');
    const ap = path.split('/');
    if (pp.length !== ap.length) continue;
    const params = {};
    let ok = true;
    for (let i = 0; i < pp.length; i++) {
      if (pp[i].startsWith(':')) params[pp[i].slice(1)] = decodeURIComponent(ap[i]);
      else if (pp[i] !== ap[i]) { ok = false; break; }
    }
    if (ok) return { mod, params };
  }
  return null;
}

export function navigate(path) {
  if (location.hash === '#' + path) route();
  else location.hash = '#' + path;
}

// ---------- unsaved-changes guard ----------
let leaveGuard = null;
/** Registers a function returning a warning text while the page has unsaved changes. */
export function setLeaveGuard(fn) { leaveGuard = fn; }
window.addEventListener('beforeunload', (e) => {
  if (leaveGuard && leaveGuard()) { e.preventDefault(); e.returnValue = ''; }
});
let currentHash = location.hash;
let skipHash = null;
function onHashChange() {
  if (skipHash !== null && location.hash === skipHash) { skipHash = null; return; }
  const msg = leaveGuard && leaveGuard();
  if (msg && !window.confirm(msg)) {
    skipHash = currentHash;
    location.hash = currentHash; // stay on the page
    return;
  }
  route();
}

function parseHash() {
  const h = location.hash.replace(/^#/, '') || '/dashboard';
  const [path, q] = h.split('?');
  return { path: path || '/dashboard', query: Object.fromEntries(new URLSearchParams(q || '')) };
}

let cleanups = [];
let routeSeq = 0;

function runCleanups() {
  for (const fn of cleanups) { try { fn(); } catch { /* ignore */ } }
  cleanups = [];
}

/** Sets the top-bar title; an array renders breadcrumbs: [{label, href}, ..., 'Current']. */
export function setTitle(t) {
  const el = document.querySelector('.topbar .title');
  const parts = Array.isArray(t) ? t : [t];
  const last = parts[parts.length - 1];
  const lastLabel = typeof last === 'string' ? last : last.label;
  if (el) {
    setHTML(el, parts.map((p, i) => {
      const sep = i ? html`<span class="crumb-sep" aria-hidden="true">›</span>` : '';
      if (typeof p === 'string' || !p.href || i === parts.length - 1) return html`${sep}<span aria-current="${i === parts.length - 1 ? 'page' : ''}">${typeof p === 'string' ? p : p.label}</span>`;
      return html`${sep}<a class="crumb" href="${p.href}">${p.label}</a>`;
    }));
  }
  document.title = lastLabel + ' · DumpCat';
}

let setNav = () => {};

// ---------- a new version of DumpCat ----------
// The server reports its UI build with every API answer. A page loaded before
// an update shows a banner and reloads on its next navigation (after the
// unsaved-changes guard), so it never runs old and new modules together.
let newBuild = false;
function reloadPage() {
  const msg = leaveGuard && leaveGuard();
  if (msg && !window.confirm(msg)) return;
  leaveGuard = null;
  location.reload();
}
function showNewBuild() {
  if (newBuild) return;
  newBuild = true;
  const bar = document.createElement('div');
  bar.className = 'update-banner';
  bar.setAttribute('role', 'status');
  setHTML(bar, html`${icon('refresh')}<span><b>DumpCat was updated.</b> Reload to use the new version.</span><button type="button" class="btn btn-sm btn-primary">Reload</button>`);
  bar.querySelector('button').onclick = reloadPage;
  document.body.appendChild(bar);
}
setNewBuildHandler(showNewBuild);
// a page module of another build could not be loaded: reload once
function moduleGone(e) {
  if (!/dynamically imported module|module script failed|error loading dynamically/i.test(String(e && e.message))) return false;
  try {
    const last = +sessionStorage.getItem('dumpcat-reloaded') || 0;
    if (Date.now() - last < 60000) return false;
    sessionStorage.setItem('dumpcat-reloaded', String(Date.now()));
  } catch { /* storage blocked: reload anyway */ }
  location.reload();
  return true;
}

async function route() {
  if (!app.user) return;
  if (newBuild) { leaveGuard = null; location.reload(); return; } // the guard already ran (onHashChange)
  closeDropdowns();
  setNav(false);
  const { path, query } = parseHash();
  const m = matchRoute(path);
  const prev = document.getElementById('content');
  if (!prev) return;
  runCleanups();
  leaveGuard = null;
  currentHash = location.hash;
  // fresh element per page so delegated listeners never leak between pages
  const content = prev.cloneNode(false);
  prev.replaceWith(content);
  const seq = ++routeSeq;
  document.querySelectorAll('.nav a').forEach((a) => {
    const p = a.getAttribute('href').slice(1);
    const active = path === p || path.startsWith(p + '/');
    a.classList.toggle('active', active);
    if (active) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
  if (!m) {
    setTitle('Not found');
    setHTML(content, html`<div class="card card-body">Page not found. <a href="#/dashboard">Back to Dashboard</a></div>`);
    return;
  }
  setHTML(content, html`<div class="loading-page">${icon('loader', 'spin')}</div>`);
  try {
    const mod = await import(`./pages/${m.mod}.js`);
    if (seq !== routeSeq) return;
    const ctx = {
      el: content,
      params: m.params,
      query,
      cleanup: (fn) => cleanups.push(fn),
      alive: () => seq === routeSeq,
    };
    setTitle(mod.title || 'DumpCat');
    await mod.render(ctx);
    window.scrollTo(0, 0);
  } catch (e) {
    if (seq !== routeSeq) return;
    if (moduleGone(e)) return;
    console.error(e);
    setHTML(content, html`<div class="alert bad">${icon('alert')}<div>${e.message || String(e)}</div></div>`);
  }
}

/** Repeating poll bound to the current page lifetime. Pauses while the tab is hidden. */
export function poll(ctx, fn, ms) {
  let timer = null;
  let stopped = false;
  const tick = async () => {
    if (stopped || (ctx && !ctx.alive())) return;
    if (!document.hidden) {
      try { await fn(); } catch { /* keep polling */ }
    }
    if (!stopped) timer = setTimeout(tick, ms);
  };
  timer = setTimeout(tick, ms);
  const stop = () => { stopped = true; clearTimeout(timer); };
  if (ctx) ctx.cleanup(stop);
  return stop;
}

// ---------- theme ----------
function currentTheme() {
  try { return localStorage.getItem('dumpcat-theme') || 'auto'; } catch { return 'auto'; }
}

function applyTheme(t) {
  if (t === 'auto') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme', t);
  try { localStorage.setItem('dumpcat-theme', t); } catch { /* ignore */ }
}

function themeIcon(t) { return t === 'dark' ? 'moon' : t === 'light' ? 'sun' : 'monitor'; }
function themeLabel(t) { return `Theme: ${t === 'auto' ? 'system' : t} (click to change)`; }

// ---------- layout ----------
function renderLayout() {
  const root = document.getElementById('root');
  const navHtml = NAV.map((g) => {
    const items = g.items.filter((i) => !i.admin || isAdmin());
    if (!items.length) return '';
    return html`<div class="nav-group"><div class="nav-title">${g.group}</div>${items.map((i) => html`<a href="#${i.path}">${icon(i.icon)}<span>${i.label}</span></a>`)}</div>`;
  });
  const t = currentTheme();
  setHTML(root, html`<div class="app">
    <aside class="sidebar" id="sidebar">
      <div class="brand"><img class="brand-logo" src="/assets/favicon.svg" alt="" width="30" height="30"><div>DumpCat<small>MySQL · MariaDB backup</small></div></div>
      <nav class="nav">${navHtml}</nav>
      <div class="sidebar-foot">v${app.version} <a href="#/system" class="badge info hidden" id="upd-ind">${icon('download')}<span></span></a></div>
    </aside>
    <div class="backdrop-mobile" id="nav-backdrop"></div>
    <div class="main">
      <header class="topbar">
        <button class="btn btn-ghost btn-icon menu-btn" id="menu-btn" aria-label="Menu" aria-expanded="false" aria-controls="sidebar">${icon('menu')}</button>
        <div class="title"></div>
        <a href="#/dashboard" class="badge info hidden" id="tasks-ind">${icon('loader', 'spin')}<span></span></a>
        <button class="btn btn-ghost btn-icon" id="theme-btn" title="${themeLabel(t)}" aria-label="${themeLabel(t)}">${icon(themeIcon(t))}</button>
        <div class="dropdown"><button class="btn btn-ghost" id="user-btn">${icon('user')}<span class="hide-sm">${app.user.username}</span>${icon('chevron')}</button></div>
      </header>
      <main class="content" id="content"></main>
    </div>
  </div>`);
  const appEl = root.querySelector('.app');
  const menuBtn = root.querySelector('#menu-btn');
  const sidebar = root.querySelector('#sidebar');
  const small = window.matchMedia('(max-width: 860px)');
  // off-canvas sidebar on small screens: inert while closed (no hidden tab stops)
  const syncDrawer = () => {
    const open = appEl.classList.contains('nav-open');
    menuBtn.setAttribute('aria-expanded', String(open));
    sidebar.inert = small.matches && !open;
  };
  setNav = (open) => { appEl.classList.toggle('nav-open', open); syncDrawer(); };
  menuBtn.onclick = () => { setNav(!appEl.classList.contains('nav-open')); if (appEl.classList.contains('nav-open')) sidebar.querySelector('a')?.focus(); };
  root.querySelector('#nav-backdrop').onclick = () => setNav(false);
  small.addEventListener('change', syncDrawer);
  syncDrawer();
  root.querySelector('#theme-btn').onclick = (e) => {
    const order = ['auto', 'light', 'dark'];
    const next = order[(order.indexOf(currentTheme()) + 1) % 3];
    applyTheme(next);
    setHTML(e.currentTarget, icon(themeIcon(next)));
    e.currentTarget.title = themeLabel(next);
    e.currentTarget.setAttribute('aria-label', themeLabel(next));
    toast(next === 'auto' ? 'Theme: system' : next === 'light' ? 'Theme: light' : 'Theme: dark', 'info', 1800);
  };
  root.querySelector('#user-btn').onclick = (e) => {
    e.stopPropagation();
    dropdown(e.currentTarget, [
      { label: 'Profile & security', icon: 'user', onClick: () => navigate('/profile') },
      { label: 'Sign out', icon: 'logout', onClick: logout },
    ]);
  };
  startTaskIndicator();
}

let taskTimer = null;
function startTaskIndicator() {
  clearInterval(taskTimer);
  const upd = async () => {
    if (document.hidden || !app.user) return;
    try {
      const tasks = await get('/activity');
      const ind = document.getElementById('tasks-ind');
      if (!ind) return;
      ind.classList.toggle('hidden', !tasks.length);
      ind.querySelector('span').textContent = `${tasks.length} ${tasks.length === 1 ? 'task' : 'tasks'} running`;
    } catch { /* ignore */ }
  };
  upd();
  taskTimer = setInterval(upd, 5000);
}

async function logout() {
  try { await post('/auth/logout'); } catch { /* ignore */ }
  app.user = null;
  clearInterval(taskTimer);
  renderLogin();
}

// ---------- auth screens ----------
function authShell(body) {
  return html`<div class="auth-wrap"><div class="auth-card">
    <div class="brand"><img class="brand-logo" src="/assets/favicon.svg" alt="" width="30" height="30"><div>DumpCat<small>MySQL · MariaDB backup</small></div></div>
    <div class="card"><div class="card-body">${body}</div></div>
    <p class="muted small" style="text-align:center;margin-top:14px">v${app.version}</p>
  </div></div>`;
}

function renderLogin(msg) {
  const root = document.getElementById('root');
  setHTML(root, authShell(html`
    <h1 style="margin-bottom:4px">Sign in</h1>
    <p class="muted" style="margin:0 0 18px">Database backup &amp; restore management</p>
    ${msg ? html`<div class="alert warn" style="margin-bottom:14px">${icon('alert')}<div>${msg}</div></div>` : ''}
    <form id="login-form" autocomplete="on">
      <div id="step-1">
        <div class="field"><label for="lg-user">Username</label><input type="text" id="lg-user" name="username" autocomplete="username" required></div>
        <div class="field"><label for="lg-pass">Password</label><input type="password" id="lg-pass" name="password" autocomplete="current-password" required></div>
        <div class="field"><label class="check"><input type="checkbox" name="remember"><span>Remember me (30 days)</span></label></div>
      </div>
      <div id="step-2" class="hidden">
        <div class="alert info" style="margin-bottom:14px">${icon('shield')}<div>Enter the 6-digit code from your authenticator app (Google Authenticator, Authy...).</div></div>
        <div class="field"><label for="lg-totp">Two-factor code</label><input type="text" id="lg-totp" name="totp" inputmode="numeric" autocomplete="one-time-code" maxlength="6" pattern="[0-9]*"></div>
      </div>
      <div id="lg-err" class="alert bad hidden" style="margin-top:14px"></div>
      <button class="btn btn-primary" style="width:100%;margin-top:18px;height:38px" type="submit">Sign in</button>
    </form>`));
  const form = root.querySelector('#login-form');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const data = formData(form);
    const errEl = root.querySelector('#lg-err');
    errEl.classList.add('hidden');
    await busy(form.querySelector('button[type=submit]'), async () => {
      try {
        const res = await post('/auth/login', data);
        if (res.totp_required) {
          root.querySelector('#step-2').classList.remove('hidden');
          root.querySelector('#step-1').classList.add('hidden');
          root.querySelector('#lg-totp').focus();
          return;
        }
        await afterLogin(res);
        if (res.require_2fa_setup) toast('Policy requires admins to enable two-factor authentication — set it up in your Profile.', 'warn', 9000);
      } catch (err) {
        setHTML(errEl, html`${icon('alert')}<div>${err.message}</div>`);
        errEl.classList.remove('hidden');
      }
    });
  });
}

function renderSetup() {
  const root = document.getElementById('root');
  const tokenCmd = app.platform === 'windows'
    ? html`<code>&amp; "C:\\Program Files\\DumpCat\\dumpcat.exe" setup-token</code> in PowerShell as Administrator, or open <code>C:\\ProgramData\\DumpCat\\data\\dumpcat.log</code>`
    : html`<code>sudo dumpcat setup-token</code>, or see <code>journalctl -u dumpcat</code>`;
  setHTML(root, authShell(html`
    <h1 style="margin-bottom:4px">Initial setup</h1>
    <p class="muted" style="margin:0 0 16px">No account exists yet. Create the administrator account to start using DumpCat.</p>
    <form id="setup-form">
      <div class="field"><label class="req" for="st-token">Setup token</label><input type="text" id="st-token" name="setup_token" required autocomplete="off" spellcheck="false" placeholder="XXXX-XXXX-XXXX-XXXX-XXXX-XXXX">
        <div class="help">DumpCat printed a one-time token in its log when it started. To show it, run on the server: ${tokenCmd}.</div></div>
      <div class="field"><label class="req">Admin username</label><input type="text" name="username" value="admin" required autocomplete="username"></div>
      <div class="field"><label>Email</label><input type="email" name="email" autocomplete="email"></div>
      <div class="field"><label class="req">Password (min. 8 characters)</label><input type="password" name="password" required minlength="8" autocomplete="new-password"></div>
      <div class="field"><label class="req">Confirm password</label><input type="password" name="password2" required autocomplete="new-password"></div>
      <div id="st-err" class="alert bad hidden" style="margin-top:14px"></div>
      <button class="btn btn-primary" style="width:100%;margin-top:18px;height:38px" type="submit">Create account &amp; sign in</button>
    </form>`));
  const form = root.querySelector('#setup-form');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const d = formData(form);
    const errEl = root.querySelector('#st-err');
    const showErr = (m) => { setHTML(errEl, html`${icon('alert')}<div>${m}</div>`); errEl.classList.remove('hidden'); };
    if (d.password !== d.password2) return showErr('Passwords do not match');
    await busy(form.querySelector('button[type=submit]'), async () => {
      try {
        const res = await post('/auth/setup', { setup_token: d.setup_token.trim(), username: d.username.trim(), email: d.email, password: d.password });
        await afterLogin(res);
        toast('Admin account created. Start by adding a database server and a storage.', 'good', 8000);
        navigate('/servers');
      } catch (err) {
        // someone else finished the setup in the meantime
        if (err.status === 409) renderLogin('The administrator account has already been created. Please sign in.');
        else showErr(err.message);
      }
    });
  });
}

let updTimer = null;

async function afterLogin(res) {
  app.user = res.user;
  app.csrf = res.csrf;
  setCsrf(res.csrf);
  renderLayout();
  refreshUpdateBadge();
  clearInterval(updTimer);
  updTimer = setInterval(refreshUpdateBadge, 15 * 60 * 1000);
  if (!location.hash || location.hash === '#' || location.hash === '#/') location.hash = '#/dashboard';
  else route();
}

/** Shows "vX available" in the sidebar for admins when a newer release is known. */
export async function refreshUpdateBadge() {
  const el = document.getElementById('upd-ind');
  if (!el || !isAdmin()) return;
  try {
    const s = await get('/update');
    el.classList.toggle('hidden', !s.available);
    if (s.available) {
      el.querySelector('span').textContent = `${s.latest.version} available`;
      el.title = `DumpCat ${s.latest.version} is available — open System to install it`;
    }
  } catch { /* not important */ }
}

export function updateUser(u) {
  app.user = { ...app.user, ...u };
}

async function boot() {
  setUnauthorizedHandler(() => {
    if (app.user) {
      app.user = null;
      clearInterval(taskTimer);
      runCleanups();
      renderLogin('Your session has expired. Please sign in again.');
    }
  });
  window.addEventListener('hashchange', onHashChange);
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && document.querySelector('.app.nav-open') && !document.querySelector('.modal-backdrop')) setNav(false);
  });
  // every rendered form control gets an accessible name
  let pending = false;
  new MutationObserver(() => {
    if (pending) return;
    pending = true;
    requestAnimationFrame(() => { pending = false; linkLabels(document.getElementById('content')); });
  }).observe(document.getElementById('root'), { childList: true, subtree: true });
  let st;
  try {
    st = await get('/auth/status');
  } catch (e) {
    setHTML(document.getElementById('root'), html`<div class="auth-wrap"><div class="alert bad">${icon('alert')}<div>${e.message}</div></div></div>`);
    return;
  }
  app.version = st.version;
  app.platform = st.platform;
  if (st.setup_required) return renderSetup();
  if (!st.authenticated) return renderLogin();
  await afterLogin(st);
}

on(document, 'click', 'a[href^="#/"]', () => closeDropdowns());
boot().catch(toastErr);
