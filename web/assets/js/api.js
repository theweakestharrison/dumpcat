// Thin JSON API client with CSRF handling.
let csrf = '';
let onUnauthorized = () => {};
let onNewBuild = () => {};
// the UI build this page was loaded with; the server reports its own with every answer
const PAGE_BUILD = document.querySelector('meta[name="dumpcat-build"]')?.content || '';

export function setCsrf(t) { csrf = t || ''; }
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }
/** Called once the server runs another version of the UI than this page (DumpCat was updated). */
export function setNewBuildHandler(fn) { onNewBuild = fn; }

export class ApiError extends Error {
  constructor(message, status, data) {
    super(message);
    this.status = status;
    this.data = data;
  }
}

export async function api(method, path, body) {
  const opts = { method, headers: { Accept: 'application/json' }, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  if (method !== 'GET') opts.headers['X-CSRF-Token'] = csrf;
  let res;
  try {
    res = await fetch('/api' + path, opts);
  } catch (e) {
    throw new ApiError('Cannot connect to the DumpCat server', 0);
  }
  const build = res.headers.get('X-DumpCat-Build');
  if (PAGE_BUILD && build && build !== PAGE_BUILD) onNewBuild(build);
  let data = null;
  const text = await res.text();
  if (text) {
    try { data = JSON.parse(text); } catch { data = { error: text }; }
  }
  if (!res.ok) {
    const msg = (data && data.error) || res.statusText || ('HTTP ' + res.status);
    if (res.status === 401 && !path.startsWith('/auth/')) onUnauthorized();
    throw new ApiError(msg, res.status, data);
  }
  return data;
}

export const get = (p) => api('GET', p);
export const post = (p, b = {}) => api('POST', p, b);
export const put = (p, b) => api('PUT', p, b);
export const del = (p) => api('DELETE', p);

/** GETs a text answer, e.g. the Markdown API reference (/docs.md). */
export async function getText(path) {
  let res;
  try {
    res = await fetch('/api' + path, { headers: { Accept: 'text/markdown, text/plain' }, credentials: 'same-origin' });
  } catch (e) {
    throw new ApiError('Cannot connect to the DumpCat server', 0);
  }
  const text = await res.text();
  if (!res.ok) {
    let msg = res.statusText || ('HTTP ' + res.status);
    try { msg = JSON.parse(text).error || msg; } catch { /* not JSON */ }
    if (res.status === 401) onUnauthorized();
    throw new ApiError(msg, res.status);
  }
  return text;
}

/** Sends one MCP request to /api/mcp with an API token, as an AI assistant would (the API & MCP page tests a token). */
export async function mcpRequest(token, method, params = {}) {
  let res;
  try {
    res = await fetch('/api/mcp', {
      method: 'POST', credentials: 'omit',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream', Authorization: 'Bearer ' + token },
      body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
    });
  } catch (e) {
    throw new ApiError('Cannot connect to the DumpCat server', 0);
  }
  let data = null;
  try { data = await res.json(); } catch { /* empty */ }
  if (!res.ok) throw new ApiError((data && data.error) || res.statusText || ('HTTP ' + res.status), res.status, data);
  if (data && data.error) throw new ApiError(data.error.message, 400, data);
  return data && data.result;
}

export function qs(params) {
  const s = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '' && v !== 0) s.set(k, v);
  }
  const str = s.toString();
  return str ? '?' + str : '';
}
