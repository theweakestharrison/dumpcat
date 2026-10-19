import { get, post } from '../api.js';
import { html, setHTML, patch, icon, on, fmtBytes, fmtDate, fmtDur, runDuration, statusBadge, verifyBadge, toast, toastErr, confirmBox, modal, copyText, dropdown } from '../ui.js';
import { poll, canOperate, navigate, setTitle } from '../app.js';
import { methodBadge, taskList, attachLog, backBtn, drillBadge, drillRunsTable } from '../components.js';

export const title = 'Backup details';

const META_LABELS = {
  binlog_file: 'Binlog file', binlog_pos: 'Binlog position', gtid: 'GTID', binlog_info: 'Binlog info',
  from_lsn: 'From LSN', to_lsn: 'To LSN', last_lsn: 'Last LSN', backup_type: 'Type (xtrabackup)',
  server_version: 'Server version', server_host: 'Server hostname', flavor: 'Server flavor', tool: 'Tool', tool_version: 'Tool version',
  consistent_at: 'Data consistent at',
};

export async function render(ctx) {
  const { el, params } = ctx;
  const id = +params.id;
  let data = await get(`/runs/${id}`);
  const live = () => data.run.status === 'running' || data.run.status === 'queued' || data.run.verify_status === 'running' || !!data.task;
  const drilling = () => (data.drill_runs || []).some((x) => x.status === 'running');
  setTitle([{ label: 'Backups', href: '#/runs' }, `${data.run.job_name} #${id}`]);

  const draw = () => {
    const { run: r, task, dependents = [], chain } = data;
    const restorable = r.status === 'success' || r.status === 'partial';
    const okLocs = r.locations.filter((l) => l.status === 'ok');
    const metaRows = Object.entries(r.meta || {}).filter(([k, v]) => v !== '' && v !== null && typeof v !== 'object' && k !== 'single_file' && k !== 'mydumper_metadata');
    patch(el.querySelector('#detail'), html`
      <div class="page-head">${backBtn('#/runs')}
        <div style="flex:1;min-width:240px"><h1 class="h1-status">${r.job_name} <span class="muted" style="font-weight:500">#${r.id}</span> ${statusBadge(r.status)}</h1>
          <div class="sub">${[r.dir_name, fmtDate(r.started_at, true)].filter(Boolean).join(' · ')}</div></div>
        <div class="btn-group">
          ${canOperate() && (r.status === 'running' || r.status === 'queued' || r.verify_status === 'running') ? html`<button class="btn btn-danger" id="cancel">${icon('stop')}Cancel</button>` : ''}
          ${canOperate() && restorable ? html`<button class="btn" id="copy" ${task ? 'disabled' : ''} title="Copy this backup to another storage">${icon('copy')}Copy</button>
            <button class="btn" id="verify" ${r.verify_status === 'running' ? 'disabled' : ''}>${icon('shield')}Verify integrity</button>
            <a class="btn btn-primary" href="#/runs/${r.id}/restore">${icon('restore')}Restore</a>` : ''}
          ${canOperate() && r.status !== 'running' && r.status !== 'queued' && r.status !== 'deleted' ? html`<button class="btn btn-danger btn-icon" id="delete" title="Delete backup">${icon('trash')}</button>` : ''}
        </div>
      </div>
      ${r.error && r.status !== 'success' ? html`<div class="alert ${r.status === 'partial' ? 'warn' : 'bad'}" style="margin-bottom:16px">${icon('alert')}<div>${r.error}</div></div>` : ''}
      ${task ? html`<div class="card" style="margin-bottom:16px"><div class="card-head"><h2>Progress</h2></div>${taskList([task])}</div>` : ''}
      <div class="grid grid-2">
        <div class="card"><div class="card-head"><h2>Details</h2></div><div class="card-body"><div class="kv">
          <div>Job</div><div>${r.job_id ? html`<a href="#/jobs/${r.job_id}/edit">${r.job_name}</a>` : r.job_name}</div>
          <div>Server</div><div>${r.server_name}</div>
          <div>Method</div><div>${methodBadge(r.method, r.type)}</div>
          <div>Started / finished</div><div>${fmtDate(r.started_at, true)} → ${fmtDate(r.finished_at, true)}</div>
          <div>Duration</div><div class="num">${fmtDur(runDuration(r))}</div>
          <div>Raw data</div><div class="num">${fmtBytes(r.raw_bytes)}</div>
          <div>Stored size</div><div class="num">${fmtBytes(r.size_bytes)}${r.raw_bytes > 0 && r.size_bytes > 0 ? html` <span class="muted">(compression ratio ${(r.raw_bytes / r.size_bytes).toFixed(1)}×)</span>` : ''}</div>
          <div>Compression / encryption</div><div>${r.compression}${r.encrypted ? html` · <span class="badge info">${icon('lock')}age</span>${!r.has_passphrase ? html` <span class="badge warn">passphrase not stored</span>` : ''}` : ' · not encrypted'}</div>
          <div>Triggered by</div><div>${r.trigger === 'schedule' ? 'Schedule' : r.trigger === 'import' ? 'Imported from storage' : 'Manual'} ${r.triggered_by && r.trigger !== 'schedule' ? '· ' + r.triggered_by : ''}</div>
          <div>Integrity</div><div>${restorable ? verifyBadge(r.verify_status, r.verify_msg) : '—'} ${r.verified_at ? html`<span class="muted small">${fmtDate(r.verified_at)}</span>` : ''}${r.verify_msg && r.verify_status === 'failed' ? html`<div class="small" style="color:var(--critical-ink)">${r.verify_msg}</div>` : ''}</div>
          ${restorable && r.method !== 'physical' ? html`<div>Restore test</div><div>${drillBadge(r.drill_status, r.drill_msg) || html`<span class="muted small">not drilled</span>`} ${r.drilled_at ? html`<span class="muted small">${fmtDate(r.drilled_at)}</span>` : ''}</div>` : ''}
          <div>UUID</div><div class="mono small">${r.uuid}</div>
        </div></div></div>
        <div class="card"><div class="card-head"><h2>Storage locations</h2></div>
          ${r.locations.length ? html`<div class="table-wrap"><table class="table"><thead><tr><th>Storage</th><th>Path</th><th>Status</th><th></th></tr></thead><tbody>
            ${r.locations.map((l) => html`<tr><td><b>${l.storage_name}</b><div class="sub">${l.storage_type}</div></td>
              <td class="mono small break">${l.path}</td>
              <td>${statusBadge(l.status)}${l.error ? html`<div class="sub" style="color:var(--critical-ink)">${l.error}</div>` : ''}</td>
              <td class="actions">${canOperate() && l.status === 'ok' && okLocs.length > 1 ? html`<button class="btn btn-sm btn-ghost" data-del-loc="${l.id}" title="Delete from this storage">${icon('trash')}</button>` : ''}</td></tr>`)}
          </tbody></table></div>` : html`<div class="card-body muted">None yet</div>`}
          ${metaRows.length ? html`<div class="card-body" style="border-top:1px solid var(--border)"><div class="kv">${metaRows.map(([k, v]) => html`<div>${META_LABELS[k] || k}</div><div class="mono small break">${k === 'consistent_at' ? fmtDate(v * 1000, true) : v}</div>`)}</div></div>` : ''}
          ${chain && chain.length > 1 ? html`<div class="card-body" style="border-top:1px solid var(--border)"><div class="section-title">Incremental chain (restore needs the whole chain)</div>
            <div class="chips">${chain.map((c, i) => html`${i ? icon('back', 'muted') : ''}<a class="badge ${c.id === r.id ? 'info' : 'outline'}" href="#/runs/${c.id}">#${c.id} ${c.type === 'full' ? 'FULL' : 'incr'}</a>`)}</div></div>` : ''}
          ${dependents.length ? html`<div class="card-body" style="border-top:1px solid var(--border)"><span class="muted small">${dependents.length} incremental backup(s) depend on this one.</span></div>` : ''}
        </div>
      </div>`);
    const drillRuns = data.drill_runs || [];
    const drills = data.drills || [];
    patch(el.querySelector('#detail-bottom'), html`
      ${restorable && r.method !== 'physical' && (drillRuns.length || canOperate()) ? html`<div class="card"><div class="card-head"><h2>Restore drills</h2>
        ${canOperate() ? (drills.length ? html`<span class="dropdown"><button class="btn btn-sm" id="run-drill" ${drilling() ? 'disabled' : ''}>${icon('flask')}Drill this backup</button></span>`
          : r.job_id ? html`<a class="btn btn-sm" href="#/drills/new?job=${r.job_id}">${icon('plus')}Set up a restore drill</a>` : '') : ''}</div>
        ${drillRuns.length ? drillRunsTable(drillRuns, { backup: false }) : html`<div class="card-body muted small">Not drilled yet: a restore drill restores this backup into a sandbox server under temporary names, compares every table and its rows with the backup and drops the copies.</div>`}</div>` : ''}
      ${r.databases.length ? html`<div class="card"><div class="card-head"><h2>Database (${r.databases.length})</h2></div><div class="table-wrap"><table class="table">
        <thead><tr><th>Database</th><th>Charset / collation</th><th class="right">Raw size</th><th class="right">Tables</th><th class="right hide-sm">Rows</th><th>File</th></tr></thead>
        <tbody>${r.databases.map((d) => html`<tr><td><b>${d.name}</b></td><td class="small">${d.charset} / ${d.collation}</td><td class="right num">${fmtBytes(d.size_bytes)}</td><td class="right num">${d.tables}</td><td class="right num hide-sm">${d.rows ? d.rows.toLocaleString('en-US') : '—'}</td><td class="mono small">${d.file || ''}</td></tr>`)}</tbody></table></div></div>` : ''}
      ${r.files.length ? html`<div class="card"><div class="card-head"><h2>Files (${r.files.length})</h2></div><div class="table-wrap"><table class="table">
        <thead><tr><th>File name</th><th class="right">Size</th><th class="hide-sm">SHA-256</th><th></th></tr></thead>
        <tbody>${r.files.map((f) => html`<tr><td class="mono small">${f.name}<div class="sub">${f.kind}${f.database ? ' · ' + f.database : ''}</div></td>
          <td class="right num">${fmtBytes(f.size)}${f.raw_size ? html`<div class="sub">raw ${fmtBytes(f.raw_size)}</div>` : ''}</td>
          <td class="mono small hide-sm"><span title="${f.sha256}" data-copy="${f.sha256}" style="cursor:pointer">${(f.sha256 || '').slice(0, 16)}…</span></td>
          <td class="actions">${canOperate() && okLocs.length ? html`<a class="btn btn-sm" href="/api/runs/${r.id}/download?file=${encodeURIComponent(f.name)}" title="Download the file as stored">${icon('download')}Download</a>
            ${f.kind === 'sql' || f.kind === 'users' ? html`<a class="btn btn-sm btn-ghost" href="/api/runs/${r.id}/download?file=${encodeURIComponent(f.name)}&decode=1" title="Download as decompressed/decrypted .sql">.sql</a>` : ''}` : ''}</td></tr>`)}
          ${canOperate() && okLocs.length ? html`<tr><td class="mono small">manifest.json<div class="sub">metadata</div></td><td></td><td class="hide-sm"></td><td class="actions"><a class="btn btn-sm btn-ghost" href="/api/runs/${r.id}/download?file=manifest.json">${icon('download')}</a></td></tr>` : ''}
        </tbody></table></div></div>` : ''}`);
  };

  setHTML(el, html`<div id="detail"></div><div id="log-a"></div><div id="detail-bottom"></div><div id="log-b">
    <div class="card" id="log-card"><div class="card-head"><h2>Log</h2><button class="btn btn-sm btn-ghost" id="copy-log">${icon('copy')}Copy</button></div><div class="card-body"><pre class="log" id="log"></pre></div></div></div>`);
  // keep the log next to the progress while running, and near the error when it failed
  const placeLog = () => {
    const st = data.run.status;
    const slot = el.querySelector(live() || st === 'failed' || st === 'cancelled' || st === 'interrupted' ? '#log-a' : '#log-b');
    const card = el.querySelector('#log-card');
    if (card.parentElement !== slot) slot.appendChild(card);
  };
  draw();
  placeLog();
  const logEl = () => el.querySelector('#log');
  const logView = attachLog(ctx, logEl(), `/runs/${id}/log`, live);

  const refresh = async () => {
    const wasLive = live();
    data = await get(`/runs/${id}`);
    const sc = window.scrollY;
    draw();
    window.scrollTo(0, sc);
    placeLog();
    if (wasLive && !live()) {
      logView.reload();
      const st = data.run.status;
      toast(st === 'success' ? `Backup #${id} finished successfully` : `Backup #${id} ended: ${st}`, st === 'success' ? 'good' : st === 'partial' ? 'warn' : 'bad');
    }
  };
  poll(ctx, async () => { if (live() || drilling()) await refresh(); }, 3000);
  on(el, 'click', '#run-drill', (e, b) => {
    e.stopPropagation();
    const start = async (d) => {
      try {
        const x = await post(`/drills/${d.id}/run`, { run_id: id });
        toast(`Drill started — #${x.id}`);
        navigate(`/drill-runs/${x.id}`);
      } catch (er) { toastErr(er); }
    };
    const list = data.drills || [];
    if (list.length === 1) { start(list[0]); return; }
    dropdown(b, list.map((d) => ({ label: `${d.name} → ${d.target_server_name}`, icon: 'flask', onClick: () => start(d) })));
  });

  on(el, 'click', '#cancel', async () => {
    if (!(await confirmBox({ title: 'Cancel task', message: 'Stop the running task? Partial data will be deleted.', danger: true, okText: 'Cancel task' }))) return;
    try { await post(`/runs/${id}/cancel`); toast('Cancellation requested', 'info'); } catch (e) { toastErr(e); }
  });
  on(el, 'click', '#verify', async (e, b) => {
    b.disabled = true;
    try { await post(`/runs/${id}/verify`); toast('Verifying integrity…', 'info'); await refresh(); } catch (er) { toastErr(er); b.disabled = false; }
  });
  on(el, 'click', '#copy', async () => {
    const have = new Set(data.run.locations.filter((l) => l.status === 'ok').map((l) => l.storage_id));
    const sts = (await get('/storages')).filter((s) => !have.has(s.id));
    if (!sts.length) { toast('This backup is already on every storage', 'info'); return; }
    const m = modal({
      title: 'Copy to another storage',
      body: html`<div class="field"><label>Destination storage</label><select id="cp-st">${sts.map((s) => html`<option value="${s.id}">${s.name} (${s.type_label})</option>`)}</select>
        <div class="help">Data is copied as-is (compressed/encrypted) and its size is checked after upload. Cloud sources are first downloaded to the staging directory.</div></div>`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-primary" id="cp-go">${icon('copy')}Copy</button>`,
    });
    m.el.querySelector('#cp-go').onclick = async (ev) => {
      ev.currentTarget.disabled = true;
      try { await post(`/runs/${id}/copy`, { storage_id: +m.el.querySelector('#cp-st').value }); m.close(); toast('Copying…', 'info'); await refresh(); } catch (er) { toastErr(er); m.close(); }
    };
  });
  on(el, 'click', '[data-copy]', (e, s) => copyText(s.dataset.copy));
  on(el, 'click', '#copy-log', () => copyText(logEl().textContent));
  on(el, 'click', '[data-del-loc]', async (e, b) => {
    const loc = data.run.locations.find((l) => l.id === +b.dataset.delLoc);
    if (!(await confirmBox({ title: 'Delete from storage', message: html`Delete this backup from <b>${loc.storage_name}</b>? Other locations are kept.`, danger: true, okText: 'Delete' }))) return;
    try { await post(`/runs/${id}/delete`, { location_id: loc.id }); toast('Deleted'); await refresh(); } catch (er) { toastErr(er); }
  });
  on(el, 'click', '#delete', async () => {
    const deps = data.dependents || [];
    const m = modal({
      title: 'Delete backup',
      body: html`<p>Delete backup <b>${data.run.dir_name}</b> from <b>all</b> storages? This cannot be undone.</p>
        ${deps.length ? html`<div class="alert warn">${icon('alert')}<div>${deps.length} incremental backup(s) depend on it. <label class="check" style="margin-top:6px"><input type="checkbox" id="cascade"><span>Also delete the incremental chain</span></label></div></div>` : ''}`,
      footer: html`<button class="btn" data-close>Cancel</button><button class="btn btn-danger-solid" id="do-del">Delete permanently</button>`,
    });
    m.el.querySelector('#do-del').onclick = async (e) => {
      const btn = e.currentTarget;
      const cascade = !!m.el.querySelector('#cascade')?.checked;
      btn.disabled = true;
      try { await post(`/runs/${id}/delete`, { cascade }); m.close(); toast('Backup deleted'); navigate('/runs'); } catch (er) { toastErr(er); btn.disabled = false; }
    };
  });
}
