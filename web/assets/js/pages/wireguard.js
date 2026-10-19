import { get, post, put, del } from '../api.js';
import { html, setHTML, icon, on, fmtAgo, fmtBytes, toast, toastErr, confirmBox, modal, empty, formData, copyText, bindModalForm, dropdown, patch } from '../ui.js';
import { isAdmin, poll } from '../app.js';
import { topology } from '../components.js';

export const title = 'WireGuard';

const seen = (t) => t && !String(t).startsWith('0001-');
const isLocal = (ep) => /^(localhost|127\.|\[::1\]|::1)/i.test(ep || '');

const HOW = html`<div class="card"><div class="card-head"><h2>How it works</h2></div><div class="card-body">
  ${topology('wireguard')}
  <ol class="topo-steps">
    <li><span><b>Turn on WireGuard</b> below and open its UDP port (51820) in the firewall of this server.</span></li>
    <li><span><b>Add a peer</b> and install its <span class="mono">dumpcat.conf</span> on the database host with the standard WireGuard tools.</span></li>
    <li><span><b>Let MySQL listen</b> on the peer's address, then add the server with <i>Connect: Through WireGuard</i>.</span></li>
  </ol>
  <div class="help">The database host opens the tunnel and keeps it alive, so it can stay behind NAT and accept no inbound connections. Only DumpCat's address goes through the tunnel; nothing else changes on either machine.</div>
</div></div>`;

function stateView(d) {
  const st = d.state;
  if (st.error) return html`<div class="alert bad">${icon('alert')}<div><b>WireGuard could not start:</b> ${st.error}</div></div>`;
  if (!st.running) {
    return html`<div class="alert info">${icon('info')}<div><b>WireGuard is off.</b> ${d.config.enabled ? 'It is enabled but not running — save the settings again or check the service log.' : 'Turn it on below when a database host accepts no inbound connections and you prefer a VPN to an agent.'}</div></div>`;
  }
  const up = d.peers.filter((p) => p.status.online).length;
  return html`<div class="alert good">${icon('check')}<div><b>WireGuard is running</b> on UDP port ${d.config.listen_port} since ${fmtAgo(st.since)} — ${up} of ${d.peers.length} peer(s) connected. Peers connect to <span class="mono">${d.endpoint}</span>.</div></div>`;
}

function statusCell(d, s) {
  if (!d.state.running) return html`<span class="muted">—</span>`;
  if (s.online) return html`<span class="badge good">${icon('check')}Connected</span><div class="sub">handshake ${fmtAgo(s.last_handshake)}${s.connections ? ` · ${s.connections} connection${s.connections > 1 ? 's' : ''}` : ''}</div>`;
  return html`<span class="badge">${icon('x')}Not connected</span><div class="sub">${seen(s.last_handshake) ? `last handshake ${fmtAgo(s.last_handshake)}` : 'no handshake yet'}</div>`;
}

function peersView(d) {
  if (!d.peers.length) return empty('No peers yet. Add one for each database host that should join the VPN.', 'shield');
  return html`<table class="table"><thead><tr><th>Peer</th><th>Status</th><th>Address</th><th class="hide-md">Endpoint</th><th class="hide-md right">Traffic</th><th>Servers</th><th></th></tr></thead><tbody>
    ${d.peers.map(({ peer: p, status: s, servers }) => html`<tr>
      <td><b>${p.name}</b><div class="sub mono" title="${p.public_key}">${p.public_key.slice(0, 10)}…</div></td>
      <td>${statusCell(d, s)}</td>
      <td class="mono small">${p.address}${p.networks.length ? html`<div class="sub">+ ${p.networks.join(', ')}</div>` : ''}</td>
      <td class="hide-md mono small">${s.endpoint || html`<span class="muted">—</span>`}</td>
      <td class="hide-md right small num">${s.rx_bytes || s.tx_bytes ? html`↓ ${fmtBytes(s.rx_bytes)}<div class="sub">↑ ${fmtBytes(s.tx_bytes)}</div>` : html`<span class="muted">—</span>`}</td>
      <td>${servers.length ? html`<span class="chips">${servers.map((n) => html`<span class="badge outline">${n}</span>`)}</span>` : html`<span class="muted">—</span>`}</td>
      <td class="actions">${isAdmin() ? html`<span class="dropdown"><button class="btn btn-sm btn-ghost btn-icon" data-more="${p.id}" title="More">${icon('dots')}</button></span>` : ''}</td></tr>`)}
  </tbody></table>`;
}

function settingsView(d) {
  const c = d.config;
  if (!isAdmin()) {
    return html`<div class="kv"><div>Status</div><div>${c.enabled ? 'On' : 'Off'}</div><div>UDP port</div><div>${c.listen_port}</div>
      <div>VPN address</div><div class="mono">${c.address}</div><div>Endpoint</div><div class="mono">${d.endpoint}</div>
      ${c.public_key ? html`<div>Public key</div><div class="mono small">${c.public_key}</div>` : ''}</div>`;
  }
  return html`<form id="wg-form" autocomplete="off">
    <label class="check"><input type="checkbox" name="enabled" ${c.enabled ? 'checked' : ''}><span>Turn on WireGuard<span class="help">Runs inside DumpCat: no network interface, route or firewall rule is created on this server. Only DumpCat can reach the peers, and only their database addresses.</span></span></label>
    <div class="form-grid" style="margin-top:14px">
      <div class="field"><label>UDP port</label><input type="number" name="listen_port" value="${c.listen_port}" min="1" max="65535">
        <div class="help">Open it in the firewall of this server, e.g. <span class="mono">ufw allow ${c.listen_port}/udp</span>.</div></div>
      <div class="field"><label>VPN address of DumpCat</label><input type="text" name="address" value="${c.address}" placeholder="10.66.0.1/24">
        <div class="help">With the subnet the peers get addresses from. Pick one that is not used on the database hosts' networks.</div></div>
      <div class="field full"><label>Endpoint for the peers</label><input type="text" name="endpoint" value="${c.endpoint}" placeholder="${d.endpoint}">
        <div class="help">The public host name or IP address (and port, if forwarded differently) the database hosts reach this server at. Empty: <span class="mono">${d.endpoint}</span>.</div></div>
    </div>
    ${c.public_key ? html`<div class="help" style="margin-top:10px">Public key of DumpCat: <span class="mono">${c.public_key}</span></div>` : ''}
    <div style="margin-top:14px"><button class="btn btn-primary" type="submit" id="wg-save">${icon('check')}Save</button></div>
  </form>`;
}

function setupView(res) {
  const p = res.peer;
  const linux = `apt install wireguard-tools   # RHEL / Rocky / Alma: dnf install wireguard-tools
cat > /etc/wireguard/dumpcat.conf <<'EOF'
${res.config.trimEnd()}
EOF
chmod 600 /etc/wireguard/dumpcat.conf
systemctl enable --now wg-quick@dumpcat`;
  const grant = `CREATE USER 'dumpcat'@'${res.server_address}' IDENTIFIED BY 'a_strong_password';
GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, PROCESS, RELOAD,
      REPLICATION CLIENT, REPLICATION SLAVE, SHOW DATABASES ON *.* TO 'dumpcat'@'${res.server_address}';`;
  return html`
    <div class="alert warn" style="margin-bottom:14px">${icon('key')}<div><b>Copy the configuration now — it holds the peer's private key and is shown only once.</b> Lost it? <b>Rotate keys</b> issues a new one.</div></div>
    ${!res.enabled ? html`<div class="alert info" style="margin-bottom:14px">${icon('info')}<div>WireGuard is off: turn it on on this page so the peer can connect.</div></div>` : ''}
    ${isLocal(res.endpoint) ? html`<div class="alert warn" style="margin-bottom:14px">${icon('alert')}<div>The database host cannot reach <span class="mono">${res.endpoint}</span>. Set the <b>Endpoint for the peers</b> (or the Base URL in Settings) and rotate the keys, or fix the <span class="mono">Endpoint</span> line in the file.</div></div>` : ''}
    <div class="section-title">1. On the database host (Linux, as root)</div>
    <pre class="code">${linux}</pre><button type="button" class="btn btn-sm" data-copy="linux" style="margin-top:6px">${icon('copy')}Copy</button>
    <div class="help" style="margin-top:6px">Only DumpCat's address (${res.server_address}) goes through the tunnel; nothing else changes on the host. Windows: install WireGuard from wireguard.com, <b>Import tunnel from file</b> with the file below, then <b>Activate</b>.</div>
    <div class="section-title" style="margin-top:14px">Configuration file (dumpcat.conf)</div>
    <pre class="code">${res.config}</pre>
    <div class="inline" style="gap:8px;margin-top:6px"><button type="button" class="btn btn-sm" data-copy="config">${icon('copy')}Copy</button><button type="button" class="btn btn-sm" data-download>${icon('download')}Download dumpcat.conf</button></div>
    <div class="section-title" style="margin-top:14px">2. Let DumpCat in to MySQL</div>
    <div class="help">MySQL must listen on <span class="mono">${p.address}</span>: <span class="mono">bind-address = 0.0.0.0</span>, or <span class="mono">127.0.0.1,${p.address}</span> on MySQL 8.0.13+ / MariaDB 10.11+. Connections come from <span class="mono">${res.server_address}</span>, so the backup user needs that host, and the host firewall must let the WireGuard interface in (e.g. <span class="mono">ufw allow in on dumpcat to any port 3306</span>).</div>
    <pre class="code" style="margin-top:6px">${grant}</pre><button type="button" class="btn btn-sm" data-copy="grant" style="margin-top:6px">${icon('copy')}Copy</button>
    <div class="section-title" style="margin-top:14px">3. Add the server</div>
    <div class="help"><a href="#/servers">Database servers</a> → Add server → <b>Connect: Through WireGuard: ${p.name}</b>, host <span class="mono">${p.address}</span>, port 3306.</div>`;
}

function showSetup(res) {
  const m = modal({
    title: `WireGuard peer: ${res.peer.name}`, size: 'lg', body: setupView(res),
    footer: html`<button class="btn btn-primary" data-close>${icon('check')}Done</button>`,
  });
  // the private key is shown once: a stray click outside must not close the dialog
  m.el.parentElement.dispatchEvent(new Event('input', { bubbles: true }));
  const texts = {
    config: res.config,
    linux: m.el.querySelectorAll('pre.code')[0].textContent,
    grant: m.el.querySelectorAll('pre.code')[2].textContent,
  };
  on(m.el, 'click', '[data-copy]', (e, b) => copyText(texts[b.dataset.copy]));
  on(m.el, 'click', '[data-download]', () => {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([res.config], { type: 'text/plain' }));
    a.download = 'dumpcat.conf';
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  });
}

export async function render(ctx) {
  const { el } = ctx;
  let d = await get('/wireguard');
  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>WireGuard</h1><div class="sub">Built-in VPN: database hosts that accept no inbound connections join with standard WireGuard, and DumpCat reaches their databases through it</div></div>
        ${isAdmin() ? html`<button class="btn btn-primary" id="add">${icon('plus')}Add peer</button>` : ''}</div>
      <div id="wg-state">${stateView(d)}</div>
      ${d.peers.length ? '' : html`<div style="margin-top:16px">${HOW}</div>`}
      <div class="card" style="margin-top:16px"><div class="card-head"><h2>Settings</h2></div><div class="card-body">${settingsView(d)}</div></div>
      <div class="card"><div class="card-head"><h2>Peers</h2></div><div class="table-wrap" id="wg-peers">${peersView(d)}</div></div>
      ${d.peers.length ? HOW : ''}
      <div class="alert info" style="margin-top:16px">${icon('info')}<div><b>Agent or WireGuard?</b> Both work when the database host accepts no inbound connections. The <a href="#/agents">agent</a> needs nothing but outbound HTTPS and reaches MySQL on 127.0.0.1. WireGuard uses the standard tools already trusted by many teams; the database host must reach this server on UDP and MySQL must listen on the WireGuard address. Physical backups need DumpCat on the database host either way.</div></div>`);
    bindSettings();
  };
  const refresh = () => { patch(el.querySelector('#wg-state'), stateView(d)); patch(el.querySelector('#wg-peers'), peersView(d)); };
  const reload = async () => { d = await get('/wireguard'); draw(); };
  // the settings form keeps what is being typed: only the status and the peers refresh
  poll(ctx, async () => { d = await get('/wireguard'); refresh(); }, 10000);

  function bindSettings() {
    const form = el.querySelector('#wg-form');
    if (!form) return;
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const f = formData(form);
      const btn = el.querySelector('#wg-save');
      btn.disabled = true;
      try {
        const res = await put('/wireguard', { enabled: f.enabled, listen_port: f.listen_port, address: f.address.trim(), endpoint: f.endpoint.trim() });
        if (res.error) toastErr(new Error(`Saved, but WireGuard did not start: ${res.error}`));
        else toast(f.enabled ? 'WireGuard is on' : 'Saved');
        await reload();
      } catch (er) { toastErr(er); } finally { btn.disabled = false; }
    });
  }

  draw();

  const find = (id) => d.peers.find((x) => x.peer.id === +id);
  const peerForm = (p) => {
    const m = modal({
      title: p ? `Edit peer: ${p.name}` : 'Add WireGuard peer',
      body: html`<form id="peer-form" autocomplete="off">
        <div class="field"><label class="req">Name</label><input type="text" name="name" value="${p ? p.name : ''}" required maxlength="100" placeholder="e.g. db-prod-01"></div>
        ${p ? html`<div class="field" style="margin-top:12px"><label>VPN address</label><input type="text" value="${p.address}" disabled></div>`
    : html`<div class="field" style="margin-top:12px"><label>VPN address</label><input type="text" name="address" placeholder="next free address in ${d.config.address}"></div>`}
        <div class="field" style="margin-top:12px"><label>Networks behind this host <span class="muted">(optional)</span></label><textarea name="networks" rows="2" placeholder="192.168.10.0/24">${p ? p.networks.join('\n') : ''}</textarea>
          <div class="help">When this host is a gateway to other database servers (it must forward the traffic). One network per line.</div></div>
      </form>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="save">${icon('check')}${p ? 'Save' : 'Create'}</button>`,
    });
    const form = m.el.querySelector('#peer-form');
    bindModalForm(form, m.el.querySelector('#save'), async () => {
      const f = formData(form);
      const body = { name: f.name, networks: f.networks.split(/[\s,]+/).filter(Boolean) };
      if (p) {
        const res = await put(`/wireguard/peers/${p.id}`, body);
        m.close();
        if (res.error) toastErr(new Error(res.error)); else toast('Peer saved');
      } else {
        const res = await post('/wireguard/peers', { ...body, address: (f.address || '').trim() });
        m.close();
        showSetup(res);
      }
      await reload();
    });
  };

  const actions = {
    edit: (id) => peerForm(find(id).peer),
    rotate: async (id) => {
      const { peer: p } = find(id);
      if (!(await confirmBox({ title: 'Rotate keys', message: html`Issue new keys for <b>${p.name}</b>? Its current configuration stops working at once: install the new file on the database host.`, okText: 'Rotate keys' }))) return;
      try { showSetup(await post(`/wireguard/peers/${p.id}/rotate`)); await reload(); } catch (er) { toastErr(er); }
    },
    del: async (id) => {
      const { peer: p, servers } = find(id);
      if (servers.length) {
        toastErr(new Error(`The peer is used by: ${servers.join(', ')} — switch those servers to another connection first`));
        return;
      }
      if (!(await confirmBox({ title: 'Delete peer', message: html`Delete WireGuard peer <b>${p.name}</b> (${p.address})? It can no longer connect.`, danger: true, okText: 'Delete' }))) return;
      try { await del(`/wireguard/peers/${p.id}`); toast('Deleted'); await reload(); } catch (er) { toastErr(er); }
    },
  };
  on(el, 'click', '#add', () => peerForm(null));
  on(el, 'click', '[data-more]', (e, b) => {
    const id = b.dataset.more;
    dropdown(b, [
      { label: 'Edit', icon: 'edit', onClick: () => actions.edit(id) },
      { label: 'Rotate keys', icon: 'key', onClick: () => actions.rotate(id) },
      { label: 'Delete', icon: 'trash', danger: true, onClick: () => actions.del(id) },
    ]);
  });
}
