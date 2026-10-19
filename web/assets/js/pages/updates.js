// "Updates" card of the System page (admins).
import { get, post } from '../api.js';
import { html, patch, icon, on, fmtBytes, fmtDate, toast, toastErr, busy, confirmBox } from '../ui.js';
import { poll, refreshUpdateBadge } from '../app.js';

const PHASES = {
  checking: 'Checking the release server',
  downloading: 'Downloading',
  verifying: 'Verifying the download',
  waiting: 'Waiting for running tasks to finish',
  backing_up: 'Backing up the DumpCat database',
  restarting: 'Restarting',
};
const ACTIVE = new Set(Object.keys(PHASES));
const CANCELLABLE = new Set(['checking', 'downloading', 'verifying', 'waiting']);
const SOURCES = { settings: 'Settings', env: 'DUMPCAT_UPDATE_URL', build: 'built into this version' };

function progressView(s) {
  const st = s.state;
  const what = st.kind === 'rollback' ? `Rolling back to DumpCat ${st.target}` : `Updating to DumpCat ${st.target}`;
  let bar = html`<div class="progress indeterminate" style="margin-top:10px"><div></div></div>`;
  let detail = st.message || '';
  if (st.phase === 'downloading' && st.total > 0) {
    const pct = Math.min(100, (st.done / st.total) * 100);
    bar = html`<div class="progress" style="margin-top:10px"><div style="width:${pct.toFixed(1)}%"></div></div>`;
    detail = `${fmtBytes(st.done)} of ${fmtBytes(st.total)}`;
  }
  return html`<div class="alert info" role="status">${icon('loader', 'spin')}<div style="flex:1">
      <b>${what}</b> — ${PHASES[st.phase] || st.phase}${detail ? html`<div class="small muted">${detail}</div>` : ''}
      ${st.phase === 'waiting' && st.waiting?.length ? html`<ul class="small">${st.waiting.map((w) => html`<li>${w}</li>`)}</ul>` : ''}
      ${st.phase === 'restarting' ? html`<div class="small">The page reloads automatically when DumpCat is back.</div>` : ''}
      ${bar}</div>
      ${CANCELLABLE.has(st.phase) ? html`<button class="btn btn-sm" id="upd-cancel">Cancel</button>` : ''}</div>`;
}

function lastView(r) {
  if (!r) return '';
  const when = fmtDate(r.finished_at);
  if (r.status === 'success') {
    return html`<div class="alert good">${icon('check')}<div>Updated from ${r.from_version} to <b>${r.to_version}</b> on ${when}${r.by ? ` by ${r.by}` : ''}.</div></div>`;
  }
  if (r.status === 'rolled_back') {
    return r.kind === 'rollback'
      ? html`<div class="alert warn">${icon('restore')}<div>Rolled back from ${r.from_version} to <b>${r.to_version}</b> on ${when}${r.by ? ` by ${r.by}` : ''}.</div></div>`
      : html`<div class="alert warn">${icon('alert')}<div><b>The update to ${r.to_version} was rolled back automatically</b> on ${when}.<div class="small">${r.message}</div></div></div>`;
  }
  return html`<div class="alert bad">${icon('alert')}<div><b>The ${r.kind} to ${r.to_version} failed</b> on ${when}.<div class="small">${r.message}</div></div></div>`;
}

function signingView(s) {
  const sg = s.signing || {};
  if (!sg.required) {
    return html`<span class="badge warn">not checked</span>
      <div class="small muted">No signing key configured — integrity relies on HTTPS + SHA-256 only.</div>`;
  }
  const n = sg.keys.length;
  const signed = s.latest?.signed_by;
  return html`<span class="badge good">required</span> <span class="small">${n} trusted key${n === 1 ? '' : 's'}</span>
    ${sg.keys.map((k) => html`<div class="mono small break">${k}${k === signed ? html` <span class="muted">(signed the latest release)</span>` : ''}</div>`)}`;
}

function cardBody(s) {
  const st = s.state;
  const busyNow = ACTIVE.has(st.phase);
  const latest = s.latest;
  let summary;
  if (!s.server) {
    summary = html`<div class="alert warn">${icon('info')}<div>No release server is configured. Set it under <a href="#/settings">Settings → Updates</a>.</div></div>`;
  } else if (s.check_error) {
    summary = html`<div class="alert bad">${icon('alert')}<div><b>Update check failed:</b> ${s.check_error}</div></div>`;
  } else if (s.available) {
    summary = html`<div class="alert info">${icon('download')}<div><b>DumpCat ${latest.version} is available</b>${latest.released_at ? html` <span class="muted">(released ${latest.released_at})</span>` : ''}.
      ${latest.notes ? html`<pre class="release-notes">${latest.notes}</pre>` : ''}
      ${/^https?:\/\//i.test(latest.notes_url || '') ? html`<a href="${latest.notes_url}" target="_blank" rel="noopener">Release notes</a>` : ''}</div></div>`;
  } else if (latest) {
    summary = html`<div class="alert good">${icon('check')}<div>You are running the latest version.</div></div>`;
  } else {
    summary = html`<div class="muted small">Not checked yet.</div>`;
  }
  return html`
    <div class="kv" style="margin-bottom:14px">
      <div>Installed</div><div><b>${s.current}</b> <span class="muted small">${s.platform}</span></div>
      <div>Latest release</div><div>${latest ? latest.version : html`<span class="muted">—</span>`}${s.checked_at ? html` <span class="muted small">checked ${fmtDate(s.checked_at)}</span>` : ''}</div>
      <div>Release server</div><div>${s.server ? html`<span class="mono small break">${s.server}</span> <span class="muted small">(${SOURCES[s.server_source] || s.server_source})</span>` : html`<span class="muted">not configured</span>`}</div>
      <div>Release signature</div><div>${signingView(s)}</div>
    </div>
    ${busyNow ? progressView(s) : summary}
    ${st.phase === 'failed' ? html`<div class="alert bad" style="margin-top:8px">${icon('alert')}<div><b>${st.kind === 'rollback' ? 'Rollback' : 'Update'} not installed:</b> ${st.error}</div></div>` : ''}
    ${!busyNow && s.last ? html`<div style="margin-top:8px">${lastView(s.last)}</div>` : ''}
    <div class="btn-group" style="margin-top:14px">
      <button class="btn" id="upd-check" ${busyNow || !s.server ? 'disabled' : ''}>${icon('refresh')}Check now</button>
      ${s.available ? html`<button class="btn btn-primary" id="upd-install" ${busyNow ? 'disabled' : ''}>${icon('download')}Install ${latest.version}</button>` : ''}
      ${s.rollback.available ? html`<button class="btn btn-danger" id="upd-rollback" ${busyNow ? 'disabled' : ''}>${icon('restore')}Roll back to ${s.rollback.version}</button>` : ''}
    </div>
    <p class="help" style="margin-bottom:0">Installing waits for running backups and restores, backs up the DumpCat database, verifies the download
      (${s.signing?.required ? 'release signature and SHA-256' : 'SHA-256'}) and restarts. If the new version does not start, the previous version and its database are restored automatically.
      From a shell: <code>dumpcat update</code>.</p>`;
}

/** Mounts the card into box; returns nothing. */
export async function mountUpdates(ctx, box) {
  let s = await get('/update');
  let restartFrom = null;
  const draw = () => patch(box, html`<div class="card" id="updates"><div class="card-head"><h2>Updates</h2></div><div class="card-body">${cardBody(s)}</div></div>`);
  draw();

  const waitForRestart = async () => {
    // poll the public status until the server answers with another version (or the old one again after a rollback)
    const started = Date.now();
    while (ctx.alive() && Date.now() - started < 4 * 60 * 1000) {
      await new Promise((r) => setTimeout(r, 2000));
      try {
        // a new process answers with another version, or (after an automatic rollback) with no restart in progress
        const u = await get('/update');
        if (u.current !== restartFrom || u.state.phase !== 'restarting') {
          location.reload();
          return;
        }
      } catch { /* still down */ }
    }
    toast('DumpCat did not come back within 4 minutes — check the server log.', 'bad');
  };

  const startVersion = s.current;
  poll(ctx, async () => {
    if (!ACTIVE.has(s.state.phase) || restartFrom !== null) return;
    let next;
    try {
      next = await get('/update');
    } catch {
      // the server went away between two polls: it is restarting
      restartFrom = startVersion;
      waitForRestart();
      return;
    }
    if (next.current !== startVersion) {
      location.reload(); // restarted so fast that "restarting" was never seen
      return;
    }
    s = next;
    draw();
    if (s.state.phase === 'restarting') {
      restartFrom = s.current;
      waitForRestart();
    } else if (!ACTIVE.has(s.state.phase)) {
      refreshUpdateBadge();
    }
  }, 1500);

  on(box, 'click', '#upd-check', (e, b) => busy(b, async () => {
    try { s = await post('/update/check'); draw(); refreshUpdateBadge(); } catch (er) { toastErr(er); }
  }));
  on(box, 'click', '#upd-install', async () => {
    const v = s.latest.version;
    const ok = await confirmBox({
      title: `Install DumpCat ${v}`,
      message: html`<p>DumpCat will download ${v}, verify it, wait for running backups and restores to finish, back up its database and restart.</p>
        <p class="muted small" style="margin-bottom:0">The web UI is unavailable for a few seconds during the restart. Scheduled backups that are due meanwhile run right after it.</p>`,
      okText: `Install ${v}`,
    });
    if (!ok) return;
    try { s = await post('/update/install'); draw(); } catch (er) { toastErr(er); }
  });
  on(box, 'click', '#upd-cancel', async (e, b) => {
    b.disabled = true;
    try { await post('/update/cancel'); s = await get('/update'); draw(); } catch (er) { toastErr(er); b.disabled = false; }
  });
  on(box, 'click', '#upd-rollback', async () => {
    const v = s.rollback.version;
    const ok = await confirmBox({
      title: `Roll back to DumpCat ${v}`,
      message: html`<p>DumpCat ${s.current} is replaced by ${v} and <b>its database is restored to the state before the update</b>.</p>
        <p>Backup history, jobs and settings changed since the update are lost (backup files on your storages are kept — use <b>Import catalog</b> to list newer backups again).</p>`,
      okText: 'Roll back', danger: true, typeText: 'ROLLBACK',
    });
    if (!ok) return;
    try { s = await post('/update/rollback'); draw(); } catch (er) { toastErr(er); }
  });
}
