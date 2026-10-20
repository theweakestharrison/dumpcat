// API & MCP: API tokens, connecting AI assistants (MCP) and scripts, and the Markdown API reference.
import { get, post, del, getText, mcpRequest } from '../api.js';
import { html, raw, esc, setHTML, icon, on, fmtAgo, fmtDate, toast, toastErr, busy, modal, confirmBox, formData, copyText, empty } from '../ui.js';
import { app } from '../app.js';

export const title = 'API & MCP';

const TOK = '@@TOKEN@@'; // where a snippet takes the token (survives JSON.stringify)
const EXAMPLE_TOKEN = 'mv_xxxxxxxx';
const DESKTOP_CMD = { windows: 'C:\\Program Files\\DumpCat\\dumpcat.exe', linux: '/usr/local/bin/dumpcat', macos: '/usr/local/bin/dumpcat' };

const json = (v) => JSON.stringify(v, null, 2);
// Claude Desktop runs on the viewer's computer, not on the DumpCat server
const clientOS = () => (/Win/.test(navigator.platform) ? 'windows' : /Mac/.test(navigator.platform) ? 'macos' : 'linux');

/** The setup snippets, with TOK where the token goes. */
function snippets(info, os) {
  const header = 'Bearer ' + TOK;
  return {
    claude: [
      { label: 'Add DumpCat to Claude Code (one command)', lang: 'bash', text: `claude mcp add --transport http dumpcat ${info.endpoint} --header "Authorization: ${header}"` },
      { label: 'Or share it with a project: .mcp.json takes the token from your environment (DUMPCAT_TOKEN)', lang: 'json', text: json({ mcpServers: { dumpcat: { type: 'http', url: info.endpoint, headers: { Authorization: 'Bearer ${DUMPCAT_TOKEN}' } } } }) },
    ],
    desktop: [
      { label: 'Claude Desktop → Settings → Developer → Edit Config, then restart Claude Desktop', lang: 'json', text: json({ mcpServers: { dumpcat: { command: DESKTOP_CMD[os], args: ['mcp'], env: { DUMPCAT_MCP_URL: info.origin, DUMPCAT_MCP_TOKEN: TOK } } } }) },
      { label: 'Check the settings from a terminal on that computer', lang: os === 'windows' ? 'powershell' : 'bash',
        text: os === 'windows' ? `$env:DUMPCAT_MCP_URL="${info.origin}"; $env:DUMPCAT_MCP_TOKEN="${TOK}"; & "${DESKTOP_CMD.windows}" mcp -check`
          : `DUMPCAT_MCP_URL=${info.origin} DUMPCAT_MCP_TOKEN=${TOK} dumpcat mcp -check` },
    ],
    other: [
      { label: 'Clients configured with an mcpServers file (remote server, streamable HTTP)', lang: 'json', text: json({ mcpServers: { dumpcat: { url: info.endpoint, headers: { Authorization: header } } } }) },
      { label: 'Clients that start a local command: run dumpcat mcp with these two settings', lang: 'env', text: `DUMPCAT_MCP_URL=${info.origin}\nDUMPCAT_MCP_TOKEN=${TOK}` },
    ],
    rest: [
      { label: 'Call the REST API from scripts and CI', lang: 'bash', text: [
        `TOKEN=${TOK}`,
        '# what needs attention',
        `curl -H "Authorization: Bearer $TOKEN" ${info.origin}/api/dashboard`,
        '# the 5 newest backups',
        `curl -H "Authorization: Bearer $TOKEN" "${info.origin}/api/runs?limit=5"`,
        '# run job 1 now (full-access token)',
        `curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"type":"full"}' ${info.origin}/api/jobs/1/run`,
      ].join('\n') },
      { label: 'Hand the API to an AI agent: it reads the Markdown reference, then calls the API', lang: 'prompt', text: `Read the DumpCat API reference at ${info.docs} (send the header "Authorization: Bearer ${TOK}"), then use that API and token to work with my DumpCat at ${info.origin}.` },
    ],
  };
}

const TABS = [
  ['claude', 'Claude Code'],
  ['desktop', 'Claude Desktop'],
  ['other', 'Other MCP clients'],
  ['rest', 'REST API & agents'],
];

// A token's access: read looks, full also starts things, manage also lets an assistant change the configuration over MCP.
const ACCESS = { read: ['info', 'Read-only'], full: ['warn', 'Full access'], manage: ['bad', 'Manage'] };
const accessBadge = (a) => { const [cls, label] = ACCESS[a] || ACCESS.read; return html`<span class="badge ${cls}">${label}</span>`; };
const toolNeeds = (t) => (t.manage ? html`<span class="badge bad">Manage token</span>` : t.write ? html`<span class="badge warn">full-access token</span>` : html`<span class="badge info">read-only token</span>`);

const fill = (text, token) => text.split(TOK).join(token || EXAMPLE_TOKEN);
const showToken = (text, token) => raw(esc(text).split(esc(TOK)).join(`<mark class="api-tok">${esc(token || EXAMPLE_TOKEN)}</mark>`));

export async function render(ctx) {
  const { el } = ctx;
  let [me, tokens, info, docs] = await Promise.all([get('/me').then((r) => r.user), get('/me/tokens'), get('/mcp/tools'), getText('/docs.md').catch(() => '')]);
  const state = { tab: 'claude', os: clientOS(), token: '', test: null };
  const routes = (docs.match(/^#### `/gm) || []).length;
  const myTools = info.tools.filter((t) => t.available);

  const tokenTable = () => (tokens.length ? html`<div class="table-wrap"><table class="table"><thead><tr><th>Name</th><th>Access</th><th>Prefix</th><th>Created</th><th>Last used</th><th>Expires</th><th></th></tr></thead><tbody>
    ${tokens.map((t) => html`<tr><td><b>${t.name}</b></td><td>${accessBadge(t.access)}</td><td class="mono small">${t.prefix}…</td><td>${fmtDate(t.created_at)}</td><td>${t.last_used_at ? fmtAgo(t.last_used_at) : html`<span class="muted">never</span>`}</td><td>${t.expires_at ? fmtDate(t.expires_at) : 'never'}</td>
      <td class="actions"><button class="btn btn-sm btn-ghost btn-icon" data-tdel="${t.id}" title="Revoke" aria-label="Revoke ${t.name}">${icon('trash')}</button></td></tr>`)}</tbody></table></div>`
    : empty('No tokens yet. Create one for each assistant or script, so you can revoke it on its own.', 'key'));

  const snippetView = () => snippets(info, state.os)[state.tab].map((s, i) => html`<div class="api-snippet">
      <div class="api-snippet-head"><span>${s.label}</span><button type="button" class="btn btn-sm" data-copy-snippet="${i}">${icon('copy')}Copy</button></div>
      <pre class="code" data-lang="${s.lang}">${showToken(s.text, state.token)}</pre></div>`);

  const testView = () => {
    const t = state.test;
    if (!t) return '';
    if (t.error) return html`<div class="alert bad">${icon('x')}<div><b>Could not connect.</b> ${t.error}</div></div>`;
    return html`<div class="alert good">${icon('check')}<div><b>Connected.</b> DumpCat ${t.version} answers MCP ${t.protocol}${t.who ? html` · ${t.who}` : ''}. The assistant gets ${t.tools.length} tools:
      <div class="chips" style="margin-top:6px">${t.tools.map((n) => html`<span class="badge outline mono">${n}</span>`)}</div></div></div>`;
  };

  const toolGroup = (list) => html`<ul class="api-tools">${list.map((t) => html`<li class="${t.available ? '' : 'off'}" title="${t.description}"><code>${t.name}</code><span>${t.title}</span>${t.available ? '' : html`<small>needs ${t.role}</small>`}</li>`)}</ul>`;
  const toolsView = () => html`<div class="grid grid-3 api-tool-groups">
      <div><div class="section-title">${icon('eye')} Look and investigate <span class="badge info">read-only token</span></div>${toolGroup(info.tools.filter((t) => !t.write))}</div>
      <div><div class="section-title">${icon('play')} Act <span class="badge warn">full-access token</span></div>${toolGroup(info.tools.filter((t) => t.write && !t.manage))}
        <p class="small muted">Starting something also needs the operator or admin role.</p></div>
      <div><div class="section-title">${icon('edit')} Set up and change <span class="badge bad">Manage token</span></div>${toolGroup(info.tools.filter((t) => t.manage))}
        <p class="small muted">Jobs, restore drills, servers and storages. Passwords, keys and passphrases are never sent over MCP: the assistant gives you a link to the page where you enter them.</p></div></div>
    <details class="api-preview"><summary>Every tool in detail</summary>${toolsTable()}</details>`;
  const toolsTable = () => html`<div class="table-wrap" style="margin-top:10px"><table class="table"><thead><tr><th>Tool</th><th>What it does</th><th>Needs</th><th>You</th></tr></thead><tbody>
    ${info.tools.map((t) => html`<tr><td><div class="mono"><b>${t.name}</b></div><div class="sub">${t.title}</div></td><td class="small">${t.description.split('. ')[0].replace(/\.$/, '')}.</td>
      <td><div class="chips"><span class="badge outline">${t.role}</span>${toolNeeds(t)}</div></td>
      <td>${t.available ? html`<span class="badge good">${icon('check')}Yes</span>` : html`<span class="muted small">needs ${t.role}</span>`}</td></tr>`)}</tbody></table></div>`;

  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>API &amp; MCP</h1><div class="sub">Connect AI assistants and scripts to DumpCat with an API token</div></div></div>

      <div class="grid grid-3 api-ways">
        <div class="card api-way"><span class="api-way-ico">${icon('sparkle')}</span><div><h3>AI assistants (MCP)</h3>
          <p>Claude Code, Claude Desktop and other MCP clients check what failed or is overdue, read logs and explain failures. With full access they also start backups, verifications and drills; with Manage access they set up and change jobs, drills, servers and storages.</p>
          <span class="chip-fact">${info.tools.length} tools · ${myTools.length} for your role</span></div></div>
        <div class="card api-way"><span class="api-way-ico">${icon('code')}</span><div><h3>REST API</h3>
          <p>Automate DumpCat from scripts, CI and monitoring: JSON over HTTPS with a bearer token, the same requests the web UI makes.</p>
          <span class="chip-fact">${routes ? `${routes} documented routes` : 'Bearer token'}</span></div></div>
        <div class="card api-way"><span class="api-way-ico">${icon('file')}</span><div><h3>API reference for AI</h3>
          <p>Every route in one Markdown file, written by this server: paste it into an AI chat or let an agent fetch it with a token.</p>
          <span class="chip-fact mono">/api/docs.md</span></div></div>
      </div>

      <div class="card"><div class="card-head"><span class="api-num">1</span><h2>Create a token</h2><button class="btn btn-sm btn-primary" data-tok-add>${icon('plus')}New token</button></div>
        <div class="card-body small muted api-access">
          <div><span class="badge info">Read-only</span> sees what your account sees and changes nothing. Enough for monitoring and for an assistant that investigates.</div>
          <div><span class="badge warn">Full access</span> can do everything your role (${me.role}) can: CI that runs jobs, an assistant that starts backups, verifications and drills.</div>
          <div><span class="badge bad">Manage</span> is full access, and over MCP an assistant can also set up and change jobs, restore drills, servers and storages. Give it only to an assistant you let change DumpCat.</div>
        </div>
        ${tokenTable()}</div>

      <div class="card"><div class="card-head"><span class="api-num">2</span><h2>Connect your assistant or script</h2></div><div class="card-body">
        <div class="field api-tok-field"><label for="api-tok">Token for the snippets below</label>
          <div class="copy-box"><input type="password" id="api-tok" class="mono" placeholder="Paste a token, or create one in step 1" autocomplete="off" spellcheck="false" value="${state.token}">
            <button type="button" class="btn btn-icon" data-tok-show title="Show the token" aria-label="Show the token">${icon('eye')}</button></div>
          <div class="help">Kept in this page only: it fills the snippets so you can copy them ready to use. A token you create here is filled in for you.</div></div>
        <div class="tabs api-tabs" role="tablist">${TABS.map(([k, label]) => html`<button type="button" role="tab" aria-selected="${state.tab === k}" class="${state.tab === k ? 'active' : ''}" data-tab="${k}">${label}</button>`)}</div>
        ${state.tab === 'desktop' ? html`<div class="seg" role="radiogroup" aria-label="Operating system">${[['windows', 'Windows'], ['linux', 'Linux'], ['macos', 'macOS']].map(([k, l]) => html`<label><input type="radio" name="api-os" value="${k}" ${state.os === k ? 'checked' : ''}><span>${l}</span></label>`)}</div>
          <p class="small muted" style="margin-top:0">Claude Desktop starts <code>dumpcat mcp</code>, which forwards to this server: it needs the <code>dumpcat</code> program on the computer where Claude Desktop runs (the Windows package, <code>dumpcat-linux-amd64</code> on Linux; on macOS build it from source). Change <code>command</code> if you put it elsewhere.</p>` : ''}
        ${state.tab === 'other' ? html`<div class="kv api-kv"><div>Transport</div><div>Streamable HTTP (POST, JSON answers)</div><div>URL</div><div><code>${info.endpoint}</code> <button type="button" class="btn btn-sm btn-ghost" data-copy-text="${info.endpoint}">${icon('copy')}</button></div><div>Header</div><div><code>Authorization: Bearer ${state.token ? '••••••••' : EXAMPLE_TOKEN}</code></div></div>` : ''}
        <div id="api-snippets">${snippetView()}</div>
        <div class="api-test"><button type="button" class="btn" data-test ${state.token ? '' : 'disabled'}>${icon('zap')}Test MCP connection</button>
          <span class="small muted">${state.token ? 'Connects to the MCP endpoint with this token, as an assistant would.' : 'Paste or create a token to test it.'}</span></div>
        <div id="api-test-result" style="margin-top:10px">${testView()}</div>
      </div></div>

      <div class="card"><div class="card-head"><span class="api-num">3</span><h2>What an assistant can do</h2></div>
        <div class="card-body"><p class="small muted" style="margin-top:0">Each tool runs the matching REST request with the token: it is allowed, refused and written to the audit log exactly as that request would be. MCP clients ask you before calling a tool that starts something.</p>
        ${toolsView()}
        <div class="alert info" style="margin-top:14px">${icon('lock')}<div>Restores, deletions and settings are never available over MCP, and no setting turns them on. Passwords, keys, tokens and passphrases never go through it, in either direction: you enter them on this site.</div></div></div></div>

      <div class="card"><div class="card-head"><h2>API reference (Markdown)</h2>
        <div class="btn-group"><button class="btn btn-sm" data-docs-copy ${docs ? '' : 'disabled'}>${icon('copy')}Copy Markdown</button><button class="btn btn-sm" data-docs-dl ${docs ? '' : 'disabled'}>${icon('download')}Download</button></div></div>
        <div class="card-body">
          <p style="margin-top:0">Authentication, conventions, curl examples, the MCP tools and every route with the role and token it needs, in a format AI models read well. An agent can fetch it itself:</p>
          <div class="copy-box"><code>curl -H "Authorization: Bearer $TOKEN" ${info.docs}</code><button type="button" class="btn" data-copy-text='curl -H "Authorization: Bearer $TOKEN" ${info.docs}' title="Copy">${icon('copy')}</button></div>
          ${docs ? html`<details class="api-preview"><summary>Preview</summary><pre class="code">${docs}</pre></details>` : html`<p class="muted small">The reference could not be loaded.</p>`}
        </div></div>`);
  };
  draw();

  const reload = async () => { tokens = await get('/me/tokens'); draw(); };
  const setToken = (t) => {
    state.token = t.trim();
    state.test = null;
    draw();
  };

  on(el, 'input', '#api-tok', (e, input) => {
    state.token = input.value.trim();
    state.test = null;
    setHTML(el.querySelector('#api-snippets'), snippetView());
    setHTML(el.querySelector('#api-test-result'), '');
    const b = el.querySelector('[data-test]');
    b.disabled = !state.token;
    b.nextElementSibling.textContent = state.token ? 'Connects to the MCP endpoint with this token, as an assistant would.' : 'Paste or create a token to test it.';
  });
  on(el, 'click', '[data-tok-show]', () => {
    const i = el.querySelector('#api-tok');
    i.type = i.type === 'password' ? 'text' : 'password';
  });
  on(el, 'click', '[data-tab]', (e, b) => { state.tab = b.dataset.tab; draw(); });
  on(el, 'change', 'input[name="api-os"]', (e, r) => { state.os = r.value; draw(); });
  on(el, 'click', '[data-copy-snippet]', (e, b) => copyText(fill(snippets(info, state.os)[state.tab][+b.dataset.copySnippet].text, state.token)));
  on(el, 'click', '[data-copy-text]', (e, b) => copyText(b.dataset.copyText));
  on(el, 'click', '[data-docs-copy]', () => copyText(docs));
  on(el, 'click', '[data-docs-dl]', () => {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([docs], { type: 'text/markdown' }));
    a.download = 'dumpcat-api.md';
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  });
  on(el, 'click', '[data-test]', (e, b) => busy(b, async () => {
    try {
      const init = await mcpRequest(state.token, 'initialize', { protocolVersion: info.protocol_versions[0], capabilities: {}, clientInfo: { name: 'dumpcat-web', version: app.version } });
      const list = await mcpRequest(state.token, 'tools/list');
      const who = /Signed in as (\S+) \(role (\w+)\) (with a read-only token|as a viewer|with full access|with Manage access)/.exec(init.instructions || '');
      const access = { 'with a read-only token': 'read-only token', 'as a viewer': 'viewer: looks only', 'with full access': 'full access', 'with Manage access': 'Manage access' };
      state.test = { version: init.serverInfo.version, protocol: init.protocolVersion, tools: list.tools.map((t) => t.name),
        who: who ? `signed in as ${who[1]} (${who[2]}), ${access[who[3]]}` : '' };
    } catch (er) {
      state.test = { error: er.status === 401 ? 'The token is not valid: it may be mistyped, revoked or expired.' : er.message };
    }
    setHTML(el.querySelector('#api-test-result'), testView());
  }));

  on(el, 'click', '[data-tok-add]', () => {
    const m = modal({
      title: 'New API token',
      body: html`<form id="tk"><div class="field"><label class="req">Name</label><input type="text" name="name" placeholder="e.g. claude-code, jenkins-deploy" required></div>
        <div class="field" style="margin-top:12px"><label>Access</label><select name="access"><option value="read" selected>Read-only: see status, backups and logs, change nothing</option><option value="full">Full: everything your role (${me.role}) can do</option><option value="manage">Manage: full, and an assistant can set up and change jobs, drills, servers and storages</option></select>
          <div class="help">Read-only suits monitoring and AI assistants. Running jobs from CI, or letting an assistant start backups, verifications and drills, needs full access. An assistant that sets backups up for you needs Manage; it never sees or sends passwords.</div></div>
        <div class="field" style="margin-top:12px"><label>Expires after (days, 0 = never)</label><input type="number" name="expires_days" value="90" min="0"></div>
        ${me.has_password === false
    ? html`<div class="alert info" style="margin-top:12px">${icon('lock')}<div>Your account signs in with single sign-on: a token can be created within 10 minutes of signing in.</div></div>`
    : html`<div class="field" style="margin-top:12px"><label class="req">Your current password</label><input type="password" name="password" autocomplete="current-password" required></div>`}</form>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="mk">Create</button>`,
    });
    m.el.querySelector('#mk').onclick = (ev) => busy(ev.currentTarget, async () => {
      try {
        const r = await post('/me/tokens', formData(m.el.querySelector('#tk')));
        m.close();
        const s = modal({
          title: 'Token created',
          body: html`<div class="alert warn" style="margin-bottom:12px">${icon('alert')}<div>Copy it now: the token is shown <b>only once</b>. It is filled into the setup snippets on this page.</div></div><div class="copy-box"><code>${r.token}</code><button class="btn" id="cpt">${icon('copy')}</button></div>`,
          footer: html`<button class="btn btn-primary" data-close>Continue to setup</button>`,
          onClose: () => el.querySelector('#api-tok')?.scrollIntoView({ behavior: 'smooth', block: 'center' }),
        });
        s.el.querySelector('#cpt').onclick = () => copyText(r.token);
        tokens = await get('/me/tokens');
        setToken(r.token);
      } catch (er) { toastErr(er); }
    });
  });
  on(el, 'click', '[data-tdel]', async (e, b) => {
    if (!(await confirmBox({ title: 'Revoke token', message: 'Assistants and scripts using this token will lose access.', danger: true, okText: 'Revoke' }))) return;
    try { await del(`/me/tokens/${b.dataset.tdel}`); toast('Revoked'); await reload(); } catch (er) { toastErr(er); }
  });
}
