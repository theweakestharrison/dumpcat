import { get, post, put, del } from '../api.js';
import { html, setHTML, icon, on, fmtAgo, toast, toastErr, confirmBox, modal, empty, formData, copyText, bindModalForm, dropdown, patch } from '../ui.js';
import { isAdmin, poll } from '../app.js';
import { topology } from '../components.js';

export const title = 'Agents';

const DEFAULT_RELEASES = 'https://downloads.dumpcat.cc';

// The install commands for an agent; key is null when it is not known (it is shown once).
function installCommands(info, key) {
  const v = /^\d+\.\d+\.\d+/.test(info.version || '') ? info.version : '';
  const rel = info.release_url || DEFAULT_RELEASES;
  const server = info.server_url;
  const k = key || '<agent key>';
  const linux = [
    ...(v ? [`curl -fLO ${rel}/${v}/dumpcat-${v}-linux.tar.gz`, `tar xzf dumpcat-${v}-linux.tar.gz && cd dumpcat-${v}-linux`] : []),
    `sudo ./install.sh --agent --server ${server} --key ${k}`,
  ];
  const win = [
    ...(v ? [`Invoke-WebRequest ${rel}/${v}/dumpcat-${v}-windows-amd64.zip -OutFile dumpcat.zip`,
      `Expand-Archive dumpcat.zip -DestinationPath . ; cd dumpcat-${v}-windows-amd64`] : []),
    `powershell -ExecutionPolicy Bypass -File .\\install.ps1 -Agent -Server ${server} -Key ${k}`,
  ];
  return {
    'Linux (systemd)': linux.join('\n'),
    'Windows (PowerShell as Administrator)': win.join('\n'),
    'Any OS, in the foreground': `dumpcat agent -server ${server} -key ${k}`,
  };
}

function setupView(info, key) {
  const cmds = installCommands(info, key);
  const local = /^https?:\/\/(localhost|127\.|\[::1\])/i.test(info.server_url);
  return html`
    ${key ? html`<div class="alert warn" style="margin-bottom:14px">${icon('key')}<div><b>Copy the key now — it is shown only once.</b> Anyone with it can connect as this agent; rotate it if it leaks.</div></div>
      <div class="field"><label>Agent key</label><div class="inline" style="gap:8px"><input type="text" class="mono" id="agent-key" value="${key}" readonly style="flex:1"><button type="button" class="btn" data-copy-key>${icon('copy')}Copy</button></div></div>`
    : html`<div class="alert info">${icon('key')}<div>The key was shown when the agent was created and is not stored. Lost it? <b>Rotate key</b> issues a new one.</div></div>`}
    ${local ? html`<div class="alert warn" style="margin-top:14px">${icon('alert')}<div>The database host cannot reach <span class="mono">${info.server_url}</span>. Set the <b>Base URL</b> in Settings to the address it reaches DumpCat at, or replace it in the commands.</div></div>` : ''}
    <div class="help" style="margin:14px 0 6px">Run on the database host. The agent connects out to <span class="mono">${info.server_url}</span> over ${info.server_url.startsWith('https') ? 'HTTPS' : 'HTTP'} — the host needs no open port. By default it only reaches the local MySQL/MariaDB (127.0.0.1:3306 and the usual sockets); add <span class="mono">--allow host:port</span> for others.</div>
    ${Object.entries(cmds).map(([k, v], i) => html`<div class="section-title" style="margin-top:${i ? 14 : 8}px">${k}</div><pre class="code">${v}</pre><button type="button" class="btn btn-sm" data-copy-cmd="${i}" style="margin-top:6px">${icon('copy')}Copy</button>`)}
    ${!info.server_url.startsWith('https') ? html`<div class="alert info" style="margin-top:14px">${icon('lock')}<div>Database traffic goes through this connection: serve DumpCat over HTTPS (a reverse proxy, or <span class="mono">DUMPCAT_TLS_AUTO=1</span> with <span class="mono">--insecure</span> on the agent) when it crosses an untrusted network.</div></div>` : ''}`;
}

function showSetup(info, name, key) {
  const m = modal({
    title: key ? `Install agent: ${name}` : `Install commands: ${name}`, size: 'lg',
    body: setupView(info, key),
    footer: html`<button class="btn btn-primary" data-close>${icon('check')}Done</button>`,
  });
  // the key is shown once: a stray click outside must not close the dialog
  if (key) m.el.parentElement.dispatchEvent(new Event('input', { bubbles: true }));
  const cmds = Object.values(installCommands(info, key));
  on(m.el, 'click', '[data-copy-cmd]', (e, b) => copyText(cmds[+b.dataset.copyCmd]));
  on(m.el, 'click', '[data-copy-key]', () => copyText(key));
}

function statusCell(a, st) {
  if (st.online) {
    return html`<span class="badge good">${icon('check')}Online</span>
      <div class="sub">connected ${fmtAgo(st.since)}${st.streams ? ` · ${st.streams} connection${st.streams > 1 ? 's' : ''}` : ''}</div>`;
  }
  return html`<span class="badge">${icon('x')}Offline</span>
    <div class="sub">${a.last_seen_at ? `last seen ${fmtAgo(a.last_seen_at)}` : 'never connected'}</div>`;
}

const HOW = html`<div class="card"><div class="card-head"><h2>How it works</h2></div><div class="card-body">
  ${topology('agent')}
  <ol class="topo-steps">
    <li><span><b>Add an agent</b> here: DumpCat shows its key and the install command.</span></li>
    <li><span><b>Install it on the database host.</b> It connects out to DumpCat over HTTPS and stays connected.</span></li>
    <li><span><b>Add the server</b> with <i>Connect: Through agent</i> and the address as seen from that host, e.g. 127.0.0.1:3306.</span></li>
  </ol>
  <div class="help">mysqldump, mydumper, restores, binlog archiving and point-in-time recovery work as with a direct connection. Physical backups need DumpCat installed on the database host itself. Prefer a VPN? Use the built-in <a href="#/wireguard">WireGuard</a>.</div>
</div></div>`;

export async function render(ctx) {
  const { el } = ctx;
  let data = await get('/agents');
  const view = () => html`
    <div class="page-head"><div><h1>Agents</h1><div class="sub">Database hosts that accept no inbound connections: the agent connects out to DumpCat and carries the database connections</div></div>
      ${isAdmin() ? html`<button class="btn btn-primary" id="add">${icon('plus')}Add agent</button>` : ''}</div>
    ${data.items.length ? '' : HOW}
    <div class="card"><div class="table-wrap">${data.items.length ? html`<table class="table"><thead><tr><th>Agent</th><th>Status</th><th class="hide-sm">Host</th><th class="hide-md">Connected from</th><th class="hide-md">May connect to</th><th>Servers</th><th></th></tr></thead><tbody>
      ${data.items.map(({ agent: a, status: st, servers }) => html`<tr>
        <td><b>${a.name}</b><div class="sub mono">${a.key_prefix}…</div></td>
        <td>${statusCell(a, st)}</td>
        <td class="hide-sm">${(st.hostname || a.hostname) ? html`${st.hostname || a.hostname}<div class="sub">${st.os || a.os} · DumpCat ${st.version || a.version}${(st.version || a.version) !== data.version ? html` <span class="badge warn" title="The server runs ${data.version}">differs</span>` : ''}</div>` : html`<span class="muted">—</span>`}</td>
        <td class="hide-md mono small">${st.remote_addr || a.last_ip || html`<span class="muted">—</span>`}</td>
        <td class="hide-md mono small">${st.online ? st.allow.join(', ') : html`<span class="muted">—</span>`}</td>
        <td>${servers.length ? html`<span class="chips">${servers.map((s) => html`<span class="badge outline">${s}</span>`)}</span>` : html`<span class="muted">—</span>`}</td>
        <td class="actions">${isAdmin() ? html`<span class="dropdown"><button class="btn btn-sm btn-ghost btn-icon" data-more="${a.id}" title="More">${icon('dots')}</button></span>` : ''}</td></tr>`)}
    </tbody></table>` : empty('No agents yet. Add one for a database host that DumpCat cannot connect to directly.', 'link')}</div></div>
    ${data.items.length ? HOW : ''}`;
  const draw = () => setHTML(el, view());
  draw();
  const reload = async () => { data = await get('/agents'); draw(); };
  // keep the online status current (patch leaves an open menu or a selection alone)
  poll(ctx, async () => {
    data = await get('/agents');
    patch(el, view());
  }, 10000);

  const find = (id) => data.items.find((x) => x.agent.id === +id);

  const nameForm = (a) => {
    const m = modal({
      title: a ? `Rename agent: ${a.name}` : 'Add agent',
      body: html`<form id="agent-form" autocomplete="off">
        <div class="field"><label class="req">Name</label><input type="text" name="name" value="${a ? a.name : ''}" required maxlength="100" placeholder="e.g. db-prod-01"></div>
        ${a ? '' : html`<div class="help" style="margin-top:8px">One agent per database host. The next step shows its key and the install commands.</div>`}
      </form>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="save">${icon('check')}${a ? 'Save' : 'Create'}</button>`,
    });
    const form = m.el.querySelector('#agent-form');
    bindModalForm(form, m.el.querySelector('#save'), async () => {
      const d = formData(form);
      if (a) {
        await put(`/agents/${a.id}`, { name: d.name });
        m.close();
        toast('Agent renamed');
      } else {
        const res = await post('/agents', { name: d.name });
        m.close();
        showSetup(res, res.agent.name, res.key);
      }
      await reload();
    });
  };

  const actions = {
    install: (id) => showSetup(data, find(id).agent.name, null),
    rename: (id) => nameForm(find(id).agent),
    rotate: async (id) => {
      const { agent: a } = find(id);
      if (!(await confirmBox({ title: 'Rotate key', message: html`Issue a new key for <b>${a.name}</b>? The current key stops working at once: the agent disconnects until you put the new key into its configuration.`, okText: 'Rotate key' }))) return;
      try {
        const res = await post(`/agents/${a.id}/rotate`);
        showSetup(res, a.name, res.key);
        await reload();
      } catch (er) { toastErr(er); }
    },
    del: async (id) => {
      const { agent: a, servers } = find(id);
      if (servers.length) {
        toastErr(new Error(`The agent is used by: ${servers.join(', ')} — switch those servers to another connection first`));
        return;
      }
      if (!(await confirmBox({ title: 'Delete agent', message: html`Delete agent <b>${a.name}</b>? Its key stops working at once.`, danger: true, okText: 'Delete' }))) return;
      try { await del(`/agents/${a.id}`); toast('Deleted'); await reload(); } catch (er) { toastErr(er); }
    },
  };

  on(el, 'click', '#add', () => nameForm(null));
  on(el, 'click', '[data-more]', (e, b) => {
    const id = b.dataset.more;
    dropdown(b, [
      { label: 'Install commands', icon: 'download', onClick: () => actions.install(id) },
      { label: 'Rename', icon: 'edit', onClick: () => actions.rename(id) },
      { label: 'Rotate key', icon: 'key', onClick: () => actions.rotate(id) },
      { label: 'Delete', icon: 'trash', danger: true, onClick: () => actions.del(id) },
    ]);
  });
}
