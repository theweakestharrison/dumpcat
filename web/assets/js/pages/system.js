import { get, post } from '../api.js';
import { html, setHTML, icon, on, fmtBytes, busy, toastErr, copyText } from '../ui.js';
import { isAdmin } from '../app.js';
import { mountUpdates } from './updates.js';

export const title = 'System';

const INSTALL = {
  'Ubuntu / Debian': `# basic tools + rclone
sudo apt update && sudo apt install -y mariadb-client  # or: mysql-client
curl https://rclone.org/install.sh | sudo bash
# mydumper (large databases, parallel backup/restore)
sudo apt install -y mydumper
# MariaDB: physical backups
sudo apt install -y mariadb-backup
# MySQL 8.0/8.4: Percona XtraBackup
curl -O https://repo.percona.com/apt/percona-release_latest.generic_all.deb
sudo apt install -y ./percona-release_latest.generic_all.deb
sudo percona-release enable pxb-80   # MySQL 8.4: pxb-84-lts
sudo apt update && sudo apt install -y percona-xtrabackup-80   # or percona-xtrabackup-84`,
  'RHEL / Rocky / Alma': `sudo dnf install -y mariadb  # or: mysql
curl https://rclone.org/install.sh | sudo bash
# mydumper: download the .rpm from https://github.com/mydumper/mydumper/releases
# MariaDB: physical backups
sudo dnf install -y MariaDB-backup   # (MariaDB repo) or mariadb-backup
# MySQL: Percona XtraBackup
sudo dnf install -y https://repo.percona.com/yum/percona-release-latest.noarch.rpm
sudo percona-release enable pxb-80
sudo dnf install -y percona-xtrabackup-80`,
};

function diskBar(d) {
  if (!d) return html`<span class="muted">—</span>`;
  const used = d.total - d.free;
  const pct = d.total ? (used / d.total) * 100 : 0;
  return html`<div style="max-width:420px"><div class="progress"><div style="width:${pct.toFixed(1)}%;background:${pct > 90 ? 'var(--critical)' : pct > 75 ? 'var(--warning)' : 'var(--accent)'}"></div></div>
    <div class="small muted" style="margin-top:4px"><b>${fmtBytes(d.free)}</b> free / ${fmtBytes(d.total)} (${pct.toFixed(0)}% used)</div></div>`;
}

export async function render(ctx) {
  const { el } = ctx;
  let s = await get('/system');
  const draw = () => {
    setHTML(el, html`
      <div class="page-head"><div><h1>System</h1><div class="sub">DumpCat host information and backup tools</div></div>
        ${isAdmin() ? html`<button class="btn" id="refresh">${icon('refresh')}Re-detect tools</button>` : ''}</div>
      <div class="grid grid-2">
        <div class="card"><div class="card-head"><h2>DumpCat</h2></div><div class="card-body"><div class="kv">
          <div>Version</div><div>${s.version} (${s.go})</div>
          <div>Operating system</div><div>${s.os} · ${s.cpus} CPU · ${s.hostname}</div>
          <div>Listening on</div><div class="mono small">${s.listen}${s.tls ? ' (HTTPS)' : ''}</div>
          <div>Base URL</div><div>${s.base_url || html`<span class="muted">not set</span>`}</div>
          <div>Running as UID</div><div>${s.uid}${s.uid === 0 ? ' (root)' : ''}</div>
          <div>Data directory</div><div class="mono small">${s.data_dir}</div>
        </div></div></div>
        <div class="card"><div class="card-head"><h2>Disk space</h2></div><div class="card-body">
          <div class="section-title">Data directory</div>${diskBar(s.data_disk)}
          <div class="section-title" style="margin-top:14px">Staging directory <span class="mono small muted">${s.staging_dir}</span></div>${diskBar(s.staging_disk)}
        </div></div>
      </div>
      ${isAdmin() ? html`<div id="updates-box"></div>` : ''}
      <div class="card"><div class="card-head"><h2>Tools</h2></div><div class="table-wrap"><table class="table"><thead><tr><th>Tool</th><th>Purpose</th><th>Status</th><th>Version / path</th></tr></thead><tbody>
        ${s.tools.map((t) => html`<tr><td class="mono"><b>${t.name}</b></td><td class="small">${t.purpose}</td>
          <td>${t.found ? html`<span class="badge good">${icon('check')}Found</span>` : html`<span class="badge warn">${icon('alert')}Not installed</span>`}</td>
          <td class="small">${t.found ? html`${t.version}<div class="sub mono">${t.path}</div>` : html`<span class="muted">${t.hint}</span>`}</td></tr>`)}
      </tbody></table></div></div>
      <div class="card"><div class="card-head"><h2>Installing the tools</h2></div><div class="card-body">
        ${Object.entries(INSTALL).map(([k, v]) => html`<div class="section-title" style="margin-top:8px">${k}</div><pre class="code">${v}</pre><button class="btn btn-sm" data-copy="${k}" style="margin-top:6px">${icon('copy')}Copy</button>`)}
        <div class="alert info" style="margin-top:14px">${icon('info')}<div><b>Version notes:</b> mariabackup must match the MariaDB server version; use XtraBackup 8.0 for MySQL 8.0, 8.4 for MySQL 8.4 and 2.4 for MySQL 5.7. Physical backups require DumpCat to run on the database server itself with read access to the datadir (usually as root).</div></div>
      </div></div>`);
  };
  draw();
  if (isAdmin()) mountUpdates(ctx, el.querySelector('#updates-box')).catch(toastErr);
  on(el, 'click', '#refresh', (e, b) => busy(b, async () => {
    try {
      await post('/system/tools/refresh');
      s = await get('/system');
      draw();
      mountUpdates(ctx, el.querySelector('#updates-box')).catch(toastErr);
    } catch (er) { toastErr(er); }
  }));
  on(el, 'click', '[data-copy]', (e, b) => copyText(INSTALL[b.dataset.copy]));
}
