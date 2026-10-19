// The demo of DumpCat on dumpcat.cc: the real web UI, with this module in
// place of its API client (scripts/build-demo.py). Everything happens in the
// browser: sample data in memory, backups and restores simulated with
// progress and logs. Nothing is sent anywhere; reloading the page resets it.
import { seed, iso, dirStamp, VERSION } from './demo-data.js';
import TYPES from './demo-types.js';

const MIN = 60e3;
const H = 60 * MIN;
const MiB = 1024 ** 2;

let db = seed(Date.now());
let signedIn = true;

export class ApiError extends Error {
  constructor(message, status, data) {
    super(message);
    this.status = status;
    this.data = data;
  }
}

export function setCsrf() {}
export function setUnauthorizedHandler() {}
export function setNewBuildHandler() {}

export function qs(params) {
  const s = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '' && v !== 0) s.set(k, v);
  }
  const str = s.toString();
  return str ? '?' + str : '';
}

// ---------- helpers ----------
const fail = (msg, status = 400) => { throw new ApiError(msg, status); };
const clone = (v) => (v === undefined ? null : JSON.parse(JSON.stringify(v)));
const find = (list, id, what) => list.find((x) => x.id === +id) || fail(`${what} not found`, 404);
const nextId = (list) => list.reduce((m, x) => Math.max(m, x.id), 0) + 1;
const now = () => Date.now();
const notInDemo = (what) => fail(`${what} is not available in the demo — install DumpCat to try it for real.`);
function fmtBytes(n) {
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 2 : 0)} ${u[i]}`;
}
const stamp = (t) => {
  const d = new Date(t);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
};
function audit(action, target, details = '') {
  db.audit.unshift({ id: nextId(db.audit), ts: iso(now()), user_id: 1, username: 'demo', action, target, details, ip: '192.0.2.10' });
}

// ---------- cron (preview and next runs) ----------
function cronField(f, lo, hi) {
  const set = new Set();
  for (const part of f.split(',')) {
    const [range, stepS] = part.split('/');
    const step = stepS ? +stepS : 1;
    let a = lo;
    let b = hi;
    if (range !== '*') {
      const [x, y] = range.split('-');
      a = +x;
      b = y !== undefined ? +y : (stepS ? hi : +x);
    }
    if (!Number.isInteger(a) || !Number.isInteger(b) || !Number.isInteger(step) || step < 1 || a < lo || b > hi || a > b) throw new Error(`invalid field "${f}"`);
    for (let v = a; v <= b; v += step) set.add(v);
  }
  return set;
}
const MACROS = { '@hourly': '0 * * * *', '@daily': '0 0 * * *', '@midnight': '0 0 * * *', '@weekly': '0 0 * * 0', '@monthly': '0 0 1 * *' };
function cronNext(expr, n, from = now()) {
  const e = MACROS[expr.trim()] || expr.trim();
  const f = e.split(/\s+/);
  if (f.length !== 5) throw new Error('expected 5 fields: minute hour day month weekday');
  const [mi, ho, dom, mo, dw] = [cronField(f[0], 0, 59), cronField(f[1], 0, 23), cronField(f[2], 1, 31), cronField(f[3], 1, 12), cronField(f[4], 0, 7)];
  if (dw.has(7)) dw.add(0);
  const out = [];
  const d = new Date(from);
  d.setSeconds(0, 0);
  d.setMinutes(d.getMinutes() + 1);
  for (let i = 0; i < 60 * 24 * 400 && out.length < n; i++) {
    if (mo.has(d.getMonth() + 1) && dom.has(d.getDate()) && dw.has(d.getDay()) && ho.has(d.getHours()) && mi.has(d.getMinutes())) out.push(iso(d.getTime()));
    d.setMinutes(d.getMinutes() + 1);
  }
  return out;
}

// ---------- views ----------
const tasks = new Map(); // key -> simulated task
const isJobRunning = (id) => [...tasks.values()].some((t) => t.kind === 'backup' && t.job_id === id);
function taskInfo(t) {
  const percent = t.phase === 'upload' ? t.upload_pct : t.estimate_bytes > 0 ? Math.min(99, (t.raw_bytes * 100) / t.estimate_bytes) : 0;
  return { key: t.key, kind: t.kind, id: t.id, job_id: t.job_id || 0, title: t.title, phase: t.phase, started_at: t.started_at, raw_bytes: Math.round(t.raw_bytes),
    out_bytes: Math.round(t.out_bytes), estimate_bytes: t.estimate_bytes, rate: t.rate, upload_pct: t.upload_pct, percent };
}
const taskList = () => [...tasks.values()].map(taskInfo);

function runView(r) {
  const { log, ...rest } = r;
  return rest;
}
function jobNext(j) {
  if (!j.enabled) return null;
  const cand = [];
  try { if (j.schedule) cand.push([cronNext(j.schedule, 1)[0], 'full']); } catch { /* invalid */ }
  try { if (j.incr_schedule) cand.push([cronNext(j.incr_schedule, 1)[0], 'incremental']); } catch { /* invalid */ }
  cand.sort((a, b) => a[0].localeCompare(b[0]));
  return cand.length && cand[0][0] ? { at: cand[0][0], type: cand[0][1] } : null;
}
function lastRun(jobId) {
  const list = db.runs.filter((r) => r.job_id === jobId && r.status !== 'deleted');
  return list.at(-1);
}
function jobView(j) {
  const v = { job: j, running: isJobRunning(j.id) };
  const lr = lastRun(j.id);
  if (lr) v.last_run = { id: lr.id, status: lr.status, type: lr.type, started_at: lr.started_at, finished_at: lr.finished_at, size_bytes: lr.size_bytes, error: lr.error };
  const nx = jobNext(j);
  if (nx) v.next_run = nx;
  const ld = db.drillRuns.filter((x) => x.job_id === j.id).at(-1);
  if (ld) v.last_drill = { id: ld.id, status: ld.status, started_at: ld.started_at, summary: ld.summary, error: ld.error };
  return v;
}
function serverView(s) {
  const { dbs, flavorName, comment, ...rest } = s;
  return rest;
}
function storageView(s, withConfig) {
  const { config, usage, ...rest } = s;
  const v = { ...rest, in_use: db.jobs.filter((j) => j.targets.some((t) => t.storage_id === s.id)).map((j) => j.name) };
  if (withConfig) v.config = config;
  return v;
}
function agentView(a) {
  const { live, ...agent } = a;
  const status = live.online ? { online: true, since: live.since, last_seen: iso(now() - 8e3), remote_addr: live.remote_addr, version: agent.version, hostname: agent.hostname, os: agent.os, allow: live.allow, streams: live.streams }
    : { online: false, since: '0001-01-01T00:00:00Z', last_seen: '0001-01-01T00:00:00Z', remote_addr: '', version: '', hostname: '', os: '', allow: null, streams: 0 };
  return { agent, status, servers: db.servers.filter((s) => s.agent_id === a.id).map((s) => s.name) };
}
function wgView() {
  const w = db.wireguard;
  return {
    config: w.config, endpoint: `${w.config.endpoint}:${w.config.listen_port}`,
    state: { running: w.config.enabled, since: iso(now() - 4 * 24 * H), error: '' },
    peers: w.peers.map((p) => {
      const { live, ...peer } = p;
      const on = w.config.enabled && live && live.online;
      return { peer, servers: db.servers.filter((s) => s.wg_peer_id === p.id).map((s) => s.name),
        status: { online: !!on, last_handshake: on ? iso(now() - (15 + (p.id * 37) % 90) * 1000) : '0001-01-01T00:00:00Z', endpoint: on ? live.endpoint : '', rx_bytes: on ? live.rx : 0, tx_bytes: on ? live.tx : 0, connections: 0 } };
    }),
  };
}
function serverInfo(s) {
  const flavor = s.flavorName || 'mysql';
  const dbs = (s.dbs || []).map(([name, size, tables]) => ({ name, charset: 'utf8mb4', collation: flavor === 'mariadb' ? 'utf8mb4_uca1400_ai_ci' : 'utf8mb4_0900_ai_ci', size_bytes: Math.round(size), tables, non_innodb: 0, system: false }));
  for (const n of ['mysql', 'sys', 'information_schema', 'performance_schema']) dbs.push({ name: n, charset: 'utf8mb4', collation: '', size_bytes: 2.5 * MiB, tables: 30, non_innodb: 0, system: true });
  const [major, minor] = (s.version || '8.0').split('.').map((x) => parseInt(x, 10));
  const tunneled = s.agent_id || s.wg_peer_id;
  return {
    ok: true,
    info: { version: s.version, version_comment: s.comment || '', flavor, major, minor, datadir: s.datadir, log_bin: true, binlog_format: 'ROW', gtid_mode: flavor === 'mariadb' ? 'MariaDB GTID' : 'ON', server_id: String(s.id * 11),
      read_only: false, is_replica: false, current_user: `${s.username}@%`, grants: [], warnings: [], databases: dbs, total_bytes: dbs.reduce((a, d) => a + d.size_bytes, 0), hostname: s.name, uptime: 41 * 24 * 3600 + s.id * 3600 },
    support: [
      { method: 'mysqldump', ok: true, message: flavor === 'mariadb' ? 'mariadb-dump from 11.4.4-MariaDB' : 'mysqldump  Ver 8.0.39 for Linux on x86_64' },
      { method: 'mydumper', ok: true, message: 'mydumper v0.16.9-1, built against MySQL 8.0.39' },
      tunneled ? { method: 'physical', ok: false, message: 'Not through an agent or WireGuard: physical backups copy the datadir, so DumpCat must be installed on the database host itself' }
        : { method: 'physical', ok: true, message: flavor === 'mariadb' ? 'mariadb-backup based on MariaDB server 11.4.4' : 'xtrabackup version 8.0.35-31 based on MySQL server 8.0.35' },
    ],
  };
}

// ---------- logs ----------
function backupLog(r) {
  if (r.log) return r.log.join('\n') + '\n';
  const job = db.jobs.find((j) => j.id === r.job_id);
  const t0 = new Date(r.started_at).getTime();
  const t1 = new Date(r.finished_at || r.started_at).getTime();
  const L = [];
  const at = (f, s) => L.push(`${stamp(t0 + (t1 - t0) * f)} ${s}`);
  at(0, `DumpCat ${VERSION} — backup job "${r.job_name}" (${r.method}, ${r.type})`);
  at(0, `Connecting to ${r.server_name}...`);
  if (r.status === 'failed') {
    at(0.5, `ERROR: ${r.error}`);
    at(1, 'BACKUP FAILED');
    return L.join('\n') + '\n';
  }
  at(0.01, `Server: ${r.meta.server_version} (${r.meta.flavor})`);
  at(0.02, `Tool: ${r.meta.tool}`);
  if (r.meta.binlog_file) at(0.03, `Consistent snapshot at ${r.meta.binlog_file}:${r.meta.binlog_pos}`);
  for (const f of r.files) {
    at(0.1 + 0.6 * Math.random(), `  ✓ ${f.name}: ${fmtBytes(f.raw_size || f.size)} → ${fmtBytes(f.size)} (sha256 ${f.sha256.slice(0, 12)}…)`);
  }
  for (const l of r.locations) {
    if (l.status === 'ok') at(0.85, `Uploaded to ${l.storage_name}: ${l.path}`);
    else at(0.85, `ERROR: ${l.error}`);
  }
  if (job) at(0.9, `Retention: keep ${job.retention.keep_last ? `the last ${job.retention.keep_last}` : `${job.retention.keep_daily} daily, ${job.retention.keep_weekly} weekly, ${job.retention.keep_monthly} monthly`}; nothing to delete`);
  if (r.verify_status === 'ok') {
    at(0.93, '=== INTEGRITY CHECK (verify) ===');
    for (const f of r.files) at(0.97, `  ✓ ${f.name} (${fmtBytes(f.size)}) sha256 OK`);
    at(0.99, 'Verify OK');
  }
  at(1, r.status === 'partial' ? `BACKUP PARTIALLY COMPLETED: ${r.error}` : `BACKUP SUCCEEDED in ${Math.round((t1 - t0) / 1000)}s — ${fmtBytes(r.size_bytes)}`);
  return L.join('\n') + '\n';
}
function restoreLog(x) {
  if (x.log) return x.log.join('\n') + '\n';
  const t0 = new Date(x.started_at).getTime();
  const t1 = new Date(x.finished_at || x.started_at).getTime();
  const L = [];
  const at = (f, s) => L.push(`${stamp(t0 + (t1 - t0) * f)} ${s}`);
  at(0, `DumpCat ${VERSION} — restore of "${x.job_name}" to ${x.target_server_name}`);
  at(0.05, 'Test-decrypting every file before touching the target... OK');
  for (const d of x.options.databases) at(0.2, `Restoring ${d}${x.options.rename[d] ? ` as ${x.options.rename[d]}` : ''}...`);
  if (x.status === 'failed') {
    at(0.9, `ERROR: ${x.error}`);
    at(1, 'RESTORE FAILED');
  } else {
    if (x.options.pitr) at(0.8, `Replaying binlogs up to ${x.options.pitr.time || x.options.pitr.gtid || 'the target'}... OK`);
    at(1, `RESTORE SUCCEEDED in ${Math.round((t1 - t0) / 1000)}s`);
  }
  return L.join('\n') + '\n';
}
function drillLog(x) {
  if (x.log) return x.log.join('\n') + '\n';
  const t0 = new Date(x.started_at).getTime();
  const t1 = new Date(x.finished_at || x.started_at).getTime();
  const L = [];
  const at = (f, s) => L.push(`${stamp(t0 + (t1 - t0) * f)} ${s}`);
  at(0, `DumpCat ${VERSION} — RESTORE DRILL "${x.drill_name}": backup #${x.run_id} "${x.job_name}" ${x.run_dir} → sandbox ${x.target_server_name}`);
  at(0, `Sandbox: ${x.target_server_name} — mysql 8.4.3 on ${x.target_server_name}`);
  for (const m of x.databases) at(0, `  ${m.name} → ${m.temp}`);
  at(0.01, '=== RESTORE ===');
  at(0.02, 'Pre-restore check: every file opened & decoded');
  for (const m of x.databases) at(0.05, `Restore ${m.name} → ${m.temp}`);
  const rto = x.checks.find((c) => c.kind === 'restore');
  at(0.8, `Restored in ${rto ? rto.actual : '—'}`);
  at(0.81, '=== CHECKS ===');
  for (const m of x.databases) at(0.85, `${m.name}: ${m.tables} tables ✓, ${m.rows.toLocaleString('en-US')} rows ✓`);
  for (const c of x.checks.filter((y) => y.kind === 'sql')) at(0.95, `Assertion "${c.name}": ${c.status === 'ok' ? '✓' : '✗'} ${c.message}`);
  for (const m of x.databases) at(0.98, `DROP DATABASE IF EXISTS \`${m.temp}\``);
  at(0.99, 'Temporary databases dropped');
  at(1, x.status === 'passed' ? `DRILL PASSED in ${Math.round((t1 - t0) / 1000)}s — ${x.summary}` : `DRILL FAILED: ${x.error.replace(/\n/g, '; ')}`);
  return L.join('\n') + '\n';
}
const logPage = (text, q) => {
  const off = +q.get('offset') || 0;
  return { data: text.slice(off), next: text.length };
};

// ---------- simulations ----------
setInterval(() => {
  for (const t of [...tasks.values()]) t.step(now());
}, 400);

function startBackup(job, type, trigger = 'manual') {
  if (isJobRunning(job.id)) fail('the job is already running', 409);
  const sv = find(db.servers, job.server_id, 'server');
  const dbs = job.method === 'physical' ? sv.dbs : sv.dbs.filter(([n]) => job.databases.includes(n));
  if (!dbs.length) fail('no databases selected');
  const est = Math.round(dbs.reduce((a, [, s]) => a + s, 0) * (type === 'incremental' ? 0.08 : 1));
  const start = now();
  const id = nextId(db.runs);
  const ext = job.encrypt ? '.zst.age' : '.zst';
  const prevFull = db.runs.filter((r) => r.job_id === job.id && r.type === 'full' && r.status === 'success').at(-1);
  const prev = db.runs.filter((r) => r.job_id === job.id && r.status === 'success').at(-1);
  const run = {
    id, uuid: crypto.randomUUID(), job_id: job.id, job_name: job.name, job_uuid: job.uuid, job_slug: `${job.name}-${job.id}`, server_id: sv.id, server_name: sv.name, method: job.method, type,
    parent_id: type === 'incremental' && prev ? prev.id : null, base_id: type === 'incremental' && prevFull ? prevFull.id : null, parent_uuid: '', base_uuid: '', status: 'running', trigger, triggered_by: trigger === 'manual' ? 'demo' : '',
    started_at: iso(start), finished_at: null, size_bytes: 0, raw_bytes: 0, files: [], databases: [], meta: {}, error: '', compression: job.compression, encrypted: job.encrypt, dir_name: '',
    verify_status: '', verified_at: null, verify_msg: '', locations: [], has_passphrase: job.has_passphrase, log: [],
  };
  db.runs.push(run);
  const log = (s) => run.log.push(`${stamp(now())} ${s}`);
  log(`DumpCat ${VERSION} — backup job "${job.name}" (${job.method}, ${type})`);
  log(`Connecting to ${sv.name}${sv.agent_id ? ' (through agent ' + db.agents.find((a) => a.id === sv.agent_id).name + ')' : sv.wg_peer_id ? ' (through WireGuard)' : ''}...`);
  const ratio = job.method === 'physical' ? 0.34 : 0.18;
  const speed = est / 9000; // the dump takes ~9 s in the demo
  let fileIdx = 0;
  const per = dbs.map(([n, s]) => [n, s * (type === 'incremental' ? 0.08 : 1)]);
  let done = 0;
  const t = {
    key: `backup:${id}`, kind: 'backup', id, job_id: job.id, title: `${job.name} (${type})`, phase: 'prepare', started_at: iso(start), raw_bytes: 0, out_bytes: 0, estimate_bytes: est, rate: 0, upload_pct: 0,
    last: start,
    step(at) {
      const dt = at - this.last;
      this.last = at;
      const el = at - start;
      if (this.phase === 'prepare' && el > 1200) {
        log(`Server: ${sv.version} (${sv.flavorName}), datadir=${sv.datadir}`);
        log(`Tool: ${job.method === 'physical' ? 'xtrabackup version 8.0.35-31' : job.method === 'mydumper' ? 'mydumper v0.16.9-1' : 'mysqldump  Ver 8.0.39 for Linux on x86_64'}`);
        if (job.options.record_binlog) log('Consistent snapshot at mysql-bin.000901:4417762 (--single-transaction, no locks on InnoDB)');
        this.phase = 'dump';
      } else if (this.phase === 'dump') {
        const inc = speed * dt * (0.85 + Math.random() * 0.3);
        this.raw_bytes = Math.min(est, this.raw_bytes + inc);
        this.out_bytes = this.raw_bytes * ratio;
        this.rate = speed * 1000;
        while (fileIdx < per.length && this.raw_bytes >= done + per[fileIdx][1] - 1) {
          const [n, s] = per[fileIdx];
          done += s;
          const name = job.method === 'physical' ? `backup.xbstream${ext}` : job.method === 'mydumper' ? `${n}.tar${ext}` : `${n}.sql${ext}`;
          if (job.method !== 'physical' || fileIdx === per.length - 1) {
            run.files.push({ name, size: Math.round((job.method === 'physical' ? est : s) * ratio), sha256: [...crypto.getRandomValues(new Uint8Array(32))].map((b) => b.toString(16).padStart(2, '0')).join(''), kind: job.method === 'physical' ? 'xbstream' : job.method === 'mydumper' ? 'mydumper' : 'sql', database: job.method === 'physical' ? '' : n, raw_size: Math.round(job.method === 'physical' ? est : s) });
            log(`  ✓ ${name}: ${fmtBytes(job.method === 'physical' ? est : s)} → ${fmtBytes((job.method === 'physical' ? est : s) * ratio)}`);
          }
          fileIdx++;
        }
        if (this.raw_bytes >= est - 1) {
          run.raw_bytes = est;
          run.size_bytes = Math.round(est * ratio);
          this.phase = 'upload';
          this.upload_pct = 0;
          this.rate = 0;
          log(`Uploading to ${job.targets.map((x) => x.storage_name).join(', ')}...`);
        }
      } else if (this.phase === 'upload') {
        this.upload_pct = Math.min(100, this.upload_pct + dt / 35);
        if (this.upload_pct >= 100) {
          const dir = `${dirStamp(start)}-${type === 'incremental' ? 'incr' : 'full'}`;
          run.dir_name = dir;
          run.locations = job.targets.map((x, i) => ({ id: id * 10 + i, run_id: id, storage_id: x.storage_id, storage_name: x.storage_name, storage_type: x.storage_type, path: `${job.name}-${job.id}/${dir}`, status: 'ok', error: '', size_bytes: run.size_bytes, created_at: iso(at), updated_at: iso(at) }));
          for (const l of run.locations) log(`Uploaded to ${l.storage_name}: ${l.path}`);
          run.databases = job.method === 'physical' ? [] : dbs.map(([n, s, tb]) => ({ name: n, charset: 'utf8mb4', collation: 'utf8mb4_0900_ai_ci', size_bytes: Math.round(s), file: `${n}.sql`, tables: tb, binlog_file: 'mysql-bin.000901', binlog_pos: 4417762 }));
          run.meta = { flavor: sv.flavorName, server_version: sv.version, server_host: sv.name, consistent_at: Math.floor(start / 1000) + 1, ...(job.options.record_binlog ? { binlog_file: 'mysql-bin.000901', binlog_pos: 4417762 } : {}) };
          this.phase = 'retention';
          log('Retention: nothing to delete');
          this.retAt = at;
        }
      } else if (this.phase === 'retention' && at - this.retAt > 700) {
        const mode = job.options.verify_mode || (job.options.verify_after ? 'all' : 'off');
        if (mode === 'off') return this.finish(at);
        this.phase = 'verify';
        this.verifyStart = at;
        this.raw_bytes = 0;
        this.estimate_bytes = run.size_bytes * (mode === 'all' ? run.locations.length : 1);
        this.verifyMode = mode;
        log('=== INTEGRITY CHECK (verify) ===');
        log(`Source: ${run.locations[0].storage_name} (${run.locations[0].path})`);
      } else if (this.phase === 'verify') {
        this.raw_bytes = Math.min(this.estimate_bytes, this.raw_bytes + (this.estimate_bytes / 3000) * dt);
        if (this.raw_bytes >= this.estimate_bytes) {
          for (const f of run.files) log(`  ✓ ${f.name} (${fmtBytes(f.size)}) sha256 OK`);
          run.verify_status = 'ok';
          run.verified_at = iso(at);
          const n = this.verifyMode === 'all' ? run.locations.length : 1;
          run.verify_msg = `${run.files.length} files OK on ${n === 1 ? run.locations[0].storage_name : `${n} storages`}${n < run.locations.length ? ` (1 of ${run.locations.length} copies)` : ''}, ${Math.round((at - this.verifyStart) / 1000)}s`;
          log(`Verify OK in ${Math.round((at - this.verifyStart) / 1000)}s`);
          this.finish(at);
        }
      }
    },
    finish(at) {
      run.status = 'success';
      run.finished_at = iso(at);
      log(`BACKUP SUCCEEDED in ${Math.round((at - start) / 1000)}s — ${fmtBytes(run.size_bytes)}`);
      tasks.delete(this.key);
    },
  };
  tasks.set(t.key, t);
  audit('job_run', `job:${job.name}`, type);
  return run;
}

function startVerify(run) {
  if (tasks.has(`verify:${run.id}`)) fail('a verification is already running', 409);
  const ok = run.locations.filter((l) => l.status === 'ok');
  if (!ok.length) fail('no available storage location');
  const start = now();
  run.verify_status = 'running';
  run.log = (run.log || backupLog(run).trimEnd().split('\n'));
  const log = (s) => run.log.push(`${stamp(now())} ${s}`);
  log('=== INTEGRITY CHECK (verify) ===');
  const est = run.size_bytes * ok.length;
  const t = {
    key: `verify:${run.id}`, kind: 'verify', id: run.id, job_id: run.job_id, title: `${run.job_name} — verify`, phase: 'verify', started_at: iso(start), raw_bytes: 0, out_bytes: 0, estimate_bytes: est, rate: est / 4, upload_pct: 0, last: start,
    step(at) {
      this.raw_bytes = Math.min(est, this.raw_bytes + (est / 4000) * (at - this.last));
      this.last = at;
      if (this.raw_bytes >= est) {
        for (const l of ok) { log(`Source: ${l.storage_name} (${l.path})`); for (const f of run.files) log(`  ✓ ${f.name} (${fmtBytes(f.size)}) sha256 OK`); }
        run.verify_status = 'ok';
        run.verified_at = iso(at);
        run.verify_msg = `${run.files.length} files OK on ${ok.length === 1 ? ok[0].storage_name : `${ok.length} storages`}, ${Math.round((at - start) / 1000)}s`;
        log(`Verify OK in ${Math.round((at - start) / 1000)}s`);
        tasks.delete(this.key);
      }
    },
  };
  tasks.set(t.key, t);
}

function startRestore(run, req) {
  const target = find(db.servers, req.target_server_id || run.server_id, 'target server');
  const start = now();
  const id = nextId(db.restores);
  const physical = run.method === 'physical';
  const databases = req.databases && req.databases.length ? req.databases : run.databases.map((d) => d.name);
  const x = {
    id, run_id: run.id, run_uuid: run.uuid, job_name: run.job_name, kind: physical ? 'physical' : 'logical', target_server_id: target.id, target_server_name: target.name,
    options: { location_id: 0, target_server_id: target.id, databases, rename: req.rename || {}, drop_existing: !!req.drop_existing, no_binlog: !!req.no_binlog, fast_mode: !!req.fast_mode, restore_users: !!req.restore_users,
      threads: req.threads || 4, strip_definer: !!req.strip_definer, convert_collations: !!req.convert_collations, skip_gtid_purged: !!req.skip_gtid_purged, target_dir: req.target_dir || '', apply: !!req.apply,
      service_name: '', datadir: '', move_back: false, use_memory: '', os_user: '', ...(req.pitr ? { pitr: req.pitr } : {}) },
    status: 'running', started_at: iso(start), finished_at: null, error: '', user_id: 1, username: 'demo', log: [],
  };
  db.restores.push(x);
  const log = (s) => x.log.push(`${stamp(now())} ${s}`);
  log(`DumpCat ${VERSION} — restore of "${run.job_name}" (${run.dir_name}) to ${target.name}`);
  log(`Databases: ${databases.map((d) => (x.options.rename[d] ? `${d} → ${x.options.rename[d]}` : d)).join(', ')}`);
  const est = run.raw_bytes || run.size_bytes * 5;
  const phases = [['download', 0.25, 'Test-decrypting and downloading every file before touching the target...'], ['restore', 0.55, 'Loading data...']];
  if (req.pitr) phases.push(['binlog_download', 0.1, 'Fetching the archived binlogs...'], ['binlog_apply', 0.1, `Replaying binlogs up to ${req.pitr.time || req.pitr.gtid || (req.pitr.file ? `${req.pitr.file}:${req.pitr.pos}` : 'the latest archived event')}...`]);
  let pi = -1;
  let phaseStart = start;
  const total = 10000;
  const t = {
    key: `restore:${id}`, kind: 'restore', id, job_id: run.job_id, title: `${run.job_name} → ${target.name}`, phase: 'prepare', started_at: iso(start), raw_bytes: 0, out_bytes: 0, estimate_bytes: est, rate: est / 10, upload_pct: 0, last: start,
    step(at) {
      const cur = phases[pi];
      if (pi < 0 || at - phaseStart > total * cur[1]) {
        pi++;
        phaseStart = at;
        if (pi >= phases.length) {
          for (const d of databases) log(`  ✓ ${x.options.rename[d] || d}: restored`);
          x.status = 'success';
          x.finished_at = iso(at);
          log(`RESTORE SUCCEEDED in ${Math.round((at - start) / 1000)}s`);
          tasks.delete(this.key);
          audit('restore_finish', `restore:${id}`, 'success');
          return;
        }
        this.phase = phases[pi][0];
        log(phases[pi][2]);
      }
      this.raw_bytes = Math.min(est, (est * (at - start)) / (total * phases.reduce((a, p) => a + p[1], 0)));
    },
  };
  tasks.set(t.key, t);
  audit('restore_start', `run:${run.job_name}`, `to ${target.name}`);
  return x;
}

const hdur = (ms) => { const s = Math.round(ms / 1000); return s >= 60 ? `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s` : `${s}s`; };
const drillRunning = (id) => [...tasks.values()].some((t) => t.kind === 'drill' && t.drill_id === id);
const drillProblem = (j) => (j.method === 'physical' ? 'restore drills test logical backups (mysqldump, mydumper); physical backups are not supported yet'
  : j.method === 'mysqldump' && j.options.single_file ? 'the job writes one file for all databases, which restores under the original names only — turn off "One file for all databases" to drill its backups' : '');
const drillRunView = (x) => { const { log, ...rest } = x; return rest; };
function drillView(d) {
  const job = db.jobs.find((j) => j.id === d.job_id);
  const v = { drill: d, running: drillRunning(d.id), same_server: !!job && job.server_id === d.target_server_id };
  const x = db.drillRuns.filter((y) => y.drill_id === d.id).at(-1);
  if (x) v.last_run = { id: x.id, status: x.status, run_id: x.run_id, run_dir: x.run_dir, started_at: x.started_at, finished_at: x.finished_at, restore_ms: x.restore_ms, summary: x.summary, error: x.error };
  if (d.enabled && d.schedule) { try { v.next_at = cronNext(d.schedule, 1)[0]; } catch { /* invalid */ } }
  if (job && drillProblem(job)) v.problem = drillProblem(job);
  return v;
}
function applyDrill(d, b) {
  if (!String(b.name || '').trim()) fail('drill name is required');
  const job = db.jobs.find((j) => j.id === +b.job_id) || fail('job does not exist');
  if (drillProblem(job)) fail(drillProblem(job));
  const sv = db.servers.find((x) => x.id === +b.target_server_id) || fail('sandbox server does not exist');
  if (b.schedule && b.schedule.trim()) cronNext(b.schedule, 1);
  const assertions = (b.assertions || []).filter((a) => (a.sql || '').trim() || (a.name || '').trim()).map((a, i) => {
    const q = String(a.sql || '').trim().replace(/;+$/, '');
    if (!/^[\s(]*(select|with)\b/i.test(q) || q.includes(';')) fail(`assertion "${a.name || `Assertion ${i + 1}`}": an assertion must be one SELECT (or WITH … SELECT) query`);
    return { name: String(a.name || '').trim() || `Assertion ${i + 1}`, database: a.database || '', sql: q };
  });
  Object.assign(d, { name: b.name.trim(), job_id: job.id, job_name: job.name, target_server_id: sv.id, target_server_name: sv.name, schedule: (b.schedule || '').trim(), enabled: !!b.enabled,
    databases: b.databases || [], assertions, options: { threads: Math.max(0, Math.min(+((b.options || {}).threads) || 0, 32)), no_binlog: !!(b.options || {}).no_binlog, max_rto_minutes: Math.max(0, +((b.options || {}).max_rto_minutes) || 0) },
    notify_success: !!b.notify_success, notify_failure: !!b.notify_failure, updated_at: iso(now()) });
}
function startDrill(d, runId, trigger = 'manual') {
  if (drillRunning(d.id)) fail('this drill is already running', 409);
  const job = db.jobs.find((j) => j.id === d.job_id) || fail('job not found');
  if (drillProblem(job)) fail(drillProblem(job));
  const run = runId ? find(db.runs, runId, 'backup') : db.runs.filter((r) => r.job_id === job.id && (r.status === 'success' || r.status === 'partial')).at(-1);
  if (!run) fail('the job has no successful backup yet');
  const sv = find(db.servers, job.server_id, 'server');
  const dbs = run.databases.filter((x) => !d.databases.length || d.databases.includes(x.name));
  if (!dbs.length) fail("none of the drill's databases is in this backup");
  const id = nextId(db.drillRuns);
  const start = now();
  const x = {
    id, drill_id: d.id, drill_name: d.name, job_id: job.id, job_name: job.name, run_id: run.id, run_dir: run.dir_name, target_server_id: d.target_server_id, target_server_name: d.target_server_name,
    status: 'running', trigger, triggered_by: trigger === 'manual' ? 'demo' : 'scheduler', started_at: iso(start), finished_at: null, restore_ms: 0, check_ms: 0,
    databases: dbs.map((m) => ({ name: m.name, temp: `${m.name}__drill_${id}`, tables: 0, rows: 0 })), checks: [], summary: '', error: '', leftover: [], log: [],
  };
  db.drillRuns.push(x);
  const log = (s) => x.log.push(`${stamp(now())} ${s}`);
  log(`DumpCat ${VERSION} — RESTORE DRILL "${d.name}": backup #${run.id} "${run.job_name}" ${run.dir_name} (${run.method}) → sandbox ${d.target_server_name}`);
  log('Waiting for an execution slot...');
  if (job.server_id === d.target_server_id) log("The sandbox is the backup's own source server: the copies get temporary names that cannot collide with its databases, and nothing is written to its binary log");
  for (const m of x.databases) log(`  ${m.name} → ${m.temp}`);
  const est = dbs.reduce((a, m) => a + m.size_bytes, 0);
  const phases = [['prepare', 0.08, 'Sandbox: checking that no temporary name exists there...'], ['restore', 0.62, '=== RESTORE ==='], ['check', 0.24, '=== CHECKS ==='], ['cleanup', 0.06, 'Dropping the temporary databases...']];
  const total = 11000;
  let pi = -1;
  let phaseStart = start;
  let restoreStart = start;
  const t = {
    key: `drill:${id}`, kind: 'drill', id, drill_id: d.id, job_id: job.id, title: `${d.name}: ${job.name} ${run.dir_name} → ${d.target_server_name}`, phase: 'queued', started_at: iso(start),
    raw_bytes: 0, out_bytes: 0, estimate_bytes: est, rate: est / 7, upload_pct: 0, last: start,
    step(at) {
      const cur = phases[pi];
      if (pi < 0 || at - phaseStart > total * cur[1]) {
        if (cur && cur[0] === 'restore') {
          x.restore_ms = at - restoreStart;
          log(`Restored in ${hdur(x.restore_ms)}`);
        }
        if (cur && cur[0] === 'check') this.checks(at);
        pi++;
        phaseStart = at;
        if (pi >= phases.length) return this.finish(at);
        this.phase = phases[pi][0];
        if (this.phase === 'restore') restoreStart = at;
        log(phases[pi][2]);
        if (this.phase === 'restore') for (const m of x.databases) log(`Restore ${m.name} → ${m.temp}`);
      }
      if (this.phase === 'restore') this.raw_bytes = Math.min(est, (est * (at - restoreStart)) / (total * 0.62));
    },
    checks(at) {
      const rto = hdur(x.restore_ms);
      x.checks.push({ kind: 'restore', name: 'Restore', status: 'ok', actual: rto, message: `${x.databases.length} database(s) restored in ${rto}` });
      if (d.options.max_rto_minutes) x.checks.push({ kind: 'rto', name: 'Restore time (RTO)', status: 'ok', expected: `≤ ${d.options.max_rto_minutes} min`, actual: rto });
      for (const m of x.databases) {
        const src = dbs.find((y) => y.name === m.name);
        m.tables = src.tables;
        m.rows = src.rows || Math.round(src.size_bytes / 210);
        x.checks.push({ kind: 'tables', database: m.name, name: 'Tables', status: 'ok', expected: String(m.tables), actual: String(m.tables) });
        x.checks.push({ kind: 'rows', database: m.name, name: 'Row counts', status: 'ok', expected: m.rows.toLocaleString('en-US'), actual: m.rows.toLocaleString('en-US'), message: `${m.tables} table(s), every row count matches the backup` });
        log(`${m.name}: ${m.tables} tables ✓, ${m.rows.toLocaleString('en-US')} rows ✓`);
      }
      for (const a of d.assertions) {
        x.checks.push({ kind: 'sql', database: a.database, name: a.name, status: 'ok', expected: 'true', actual: '1', message: 'the first column is true' });
        log(`Assertion "${a.name}": ✓ the first column is true`);
      }
      x.check_ms = at - phaseStart;
    },
    finish(at) {
      for (const m of x.databases) log(`DROP DATABASE IF EXISTS \`${m.temp}\``);
      log('Temporary databases dropped');
      const passed = x.checks.filter((c) => c.status === 'ok').length;
      x.status = 'passed';
      x.finished_at = iso(at);
      x.summary = `${x.databases.length} database(s) · ${x.databases.reduce((a, m) => a + m.tables, 0)} tables · ${x.databases.reduce((a, m) => a + m.rows, 0).toLocaleString('en-US')} rows · restored in ${hdur(x.restore_ms)} · ${passed}/${x.checks.length} checks passed`;
      log(`DRILL PASSED in ${Math.round((at - start) / 1000)}s — ${x.summary}`);
      run.drill_status = 'passed';
      run.drilled_at = x.finished_at;
      run.drill_msg = x.summary;
      tasks.delete(this.key);
      audit('drill_finish', `drill:${d.name}`, 'passed');
    },
  };
  tasks.set(t.key, t);
  audit('drill_run', `drill:${d.name}`, `run:${run.id}`);
  return x;
}

// ---------- routes ----------
const routes = [];
const route = (method, pattern, fn) => routes.push([method, new RegExp('^' + pattern.replace(/:(\w+)/g, '(?<$1>[^/]+)') + '$'), fn]);

route('GET', '/auth/status', () => ({ authenticated: signedIn, csrf: 'demo', platform: 'linux', setup_required: false, user: signedIn ? db.users[0] : null, version: VERSION }));
route('POST', '/auth/login', () => { signedIn = true; return { csrf: 'demo', user: db.users[0] }; });
route('POST', '/auth/logout', () => { signedIn = false; return { ok: true }; });
route('POST', '/auth/setup', () => notInDemo('Initial setup'));
route('GET', '/me', () => ({ csrf: 'demo', user: db.users[0] }));
route('POST', '/me/password', () => ({ ok: true }));
route('POST', '/me/logout-others', () => ({ ok: true, count: 1 }));
route('GET', '/me/tokens', () => db.tokens);
route('POST', '/me/tokens', ({ body }) => {
  const item = { id: nextId(db.tokens), user_id: 1, name: body.name || 'token', prefix: 'mv_Dem0' + Math.random().toString(36).slice(2, 6), created_at: iso(now()), last_used_at: null, expires_at: body.days ? iso(now() + body.days * 24 * H) : null };
  db.tokens.push(item);
  return { item, token: item.prefix + 'xDemoTokenNotUsable0000000000000' };
});
route('DELETE', '/me/tokens/:id', ({ p }) => { db.tokens = db.tokens.filter((x) => x.id !== +p.id); return { ok: true }; });
route('POST', '/me/totp/setup', () => notInDemo('Two-factor setup'));
route('POST', '/me/totp/enable', () => notInDemo('Two-factor setup'));
route('POST', '/me/totp/disable', () => notInDemo('Two-factor setup'));

route('GET', '/dashboard', () => {
  const t24 = now() - 24 * H;
  const recent = db.runs.filter((r) => r.status !== 'deleted');
  const r24 = recent.filter((r) => new Date(r.started_at).getTime() > t24);
  const ok = recent.filter((r) => r.status === 'success' || r.status === 'partial');
  const lastOk = ok.at(-1);
  const daily = [];
  for (let i = 13; i >= 0; i--) {
    const d = new Date(now() - i * 24 * H);
    const key = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
    const day = recent.filter((r) => { const s = new Date(r.started_at); return s.getFullYear() === d.getFullYear() && s.getMonth() === d.getMonth() && s.getDate() === d.getDate(); });
    daily.push({ day: key, success: day.filter((r) => r.status === 'success').length, failed: day.filter((r) => r.status === 'failed' || r.status === 'partial').length, bytes: day.reduce((a, r) => a + r.size_bytes, 0) });
  }
  const warnings = [];
  const st = db.storages.find((s) => s.last_test_ok === false);
  if (st) warnings.push(`Storage "${st.name}" failed its last test: ${st.last_test_msg}`);
  for (const d of db.drills) {
    const x = db.drillRuns.filter((y) => y.drill_id === d.id).at(-1);
    if (x && x.status === 'failed') warnings.push(`Restore drill "${d.name}": the last run FAILED — ${x.error.split('\n')[0]}`);
  }
  return {
    stats: { jobs: db.jobs.length, jobs_enabled: db.jobs.filter((j) => j.enabled).length, servers: db.servers.length, storages: db.storages.length, runs_24h: r24.length,
      success_24h: r24.filter((r) => r.status === 'success').length, failed_24h: r24.filter((r) => r.status === 'failed').length, backups: ok.length,
      stored_bytes: ok.reduce((a, r) => a + r.size_bytes * Math.max(1, r.locations.filter((l) => l.status === 'ok').length), 0), last_success_at: lastOk ? Math.floor(new Date(lastOk.started_at).getTime() / 1000) : 0 },
    recent: recent.slice(-12).reverse().map(runView),
    jobs: db.jobs.map((j) => {
      const lr = lastRun(j.id);
      const ls = db.runs.filter((r) => r.job_id === j.id && r.status === 'success').at(-1);
      const nx = jobNext(j);
      return { id: j.id, name: j.name, method: j.method, enabled: j.enabled, server_name: j.server_name, running: isJobRunning(j.id), last_status: lr ? lr.status : '', last_at: lr && lr.started_at,
        last_size: lr ? lr.size_bytes : 0, last_success_at: ls && ls.started_at, ...(nx ? { next_at: nx.at, next_type: nx.type } : {}) };
    }),
    tasks: taskList(), warnings, daily,
  };
});
route('GET', '/activity', () => taskList());

// jobs
route('GET', '/jobs', () => db.jobs.map(jobView));
route('GET', '/jobs/:id', ({ p }) => jobView(find(db.jobs, p.id, 'job')));
function applyJob(j, b) {
  if (!String(b.name || '').trim()) fail('job name is required');
  const sv = db.servers.find((s) => s.id === +b.server_id) || fail('server does not exist');
  if (b.method === 'physical' && (sv.agent_id || sv.wg_peer_id)) fail(`server ${sv.name} is reached through an agent or WireGuard: physical backups need DumpCat installed on the database host — use mysqldump or mydumper`);
  if (!b.targets || !b.targets.length) fail('select at least one target storage');
  for (const e of [b.schedule, b.incr_schedule]) if (e && e.trim()) cronNext(e, 1);
  const mode = b.options && ['off', 'one', 'all'].includes(b.options.verify_mode) ? b.options.verify_mode : (b.options && b.options.verify_after ? 'all' : (j.id ? 'off' : 'one'));
  Object.assign(j, {
    name: b.name.trim(), server_id: sv.id, server_name: sv.name, method: b.method, db_mode: b.db_mode || 'all', databases: b.databases || [], exclude_tables: b.exclude_tables || [],
    schedule: (b.schedule || '').trim(), incr_schedule: b.method === 'physical' ? (b.incr_schedule || '').trim() : '', compression: b.compression || 'zstd', compression_level: b.compression_level || 0,
    encrypt: !!b.encrypt, has_passphrase: !!b.encrypt, threads: b.threads || 4, options: { ...(b.options || {}), verify_mode: mode, verify_after: mode !== 'off' }, retention: b.retention || {},
    targets: b.targets.map((t) => { const s = find(db.storages, t.storage_id, 'storage'); return { storage_id: s.id, storage_name: s.name, storage_type: s.type, retention: t.retention || null }; }),
    enabled: !!b.enabled, notify_success: !!b.notify_success, notify_failure: !!b.notify_failure, timeout_minutes: b.timeout_minutes || 0, updated_at: iso(now()),
  });
}
route('POST', '/jobs', ({ body }) => {
  if (db.jobs.some((j) => j.name.toLowerCase() === String(body.name || '').trim().toLowerCase())) fail('a job with this name already exists');
  const j = { id: nextId(db.jobs), uuid: crypto.randomUUID(), created_at: iso(now()) };
  applyJob(j, body);
  db.jobs.push(j);
  audit('job_create', `job:${j.name}`, j.method);
  return jobView(j);
});
route('PUT', '/jobs/:id', ({ p, body }) => { const j = find(db.jobs, p.id, 'job'); applyJob(j, body); audit('job_update', `job:${j.name}`); return jobView(j); });
route('DELETE', '/jobs/:id', ({ p, q }) => {
  const j = find(db.jobs, p.id, 'job');
  if (isJobRunning(j.id)) fail('the job is running — cancel it first');
  let deleted = 0;
  if (q.get('delete_backups') === '1') for (const r of db.runs) if (r.job_id === j.id && r.status !== 'deleted') { r.status = 'deleted'; deleted++; }
  db.jobs = db.jobs.filter((x) => x.id !== j.id);
  audit('job_delete', `job:${j.name}`);
  return { ok: true, deleted_backups: deleted, deleted_binlogs: 0, errors: [] };
});
route('POST', '/jobs/:id/run', ({ p, body }) => runView(startBackup(find(db.jobs, p.id, 'job'), body.type === 'incremental' ? 'incremental' : 'full')));
route('POST', '/jobs/:id/toggle', ({ p }) => { const j = find(db.jobs, p.id, 'job'); j.enabled = !j.enabled; return { enabled: j.enabled }; });
route('GET', '/jobs/:id/binlogs', ({ p, q }) => {
  const list = db.binlogs.filter((b) => b.job_id === +p.id).slice().reverse();
  const limit = +q.get('limit') || 50;
  const off = +q.get('offset') || 0;
  return { items: list.slice(off, off + limit), total: list.length };
});
route('GET', '/jobs/:id/binlog/log', ({ q }) => {
  const last = db.binlogs.slice(-6);
  const text = last.map((b) => `${stamp(new Date(b.created_at).getTime())} archived ${b.file_name}: ${fmtBytes(b.size_bytes)} → ${fmtBytes(b.stored_bytes)}, uploaded to nas-backups, r2-offsite`).join('\n') + '\n';
  return logPage(text, q);
});
route('POST', '/jobs/:id/binlog/rotate', () => ({ ok: true }));
route('POST', '/jobs/:id/binlog/restart', () => ({ ok: true }));
route('POST', '/cron/preview', ({ body }) => {
  if (!String(body.expr || '').trim()) return { valid: true, next: [] };
  try { return { valid: true, next: cronNext(body.expr, 5) }; } catch (e) { return { valid: false, error: e.message }; }
});

// runs
route('GET', '/runs', ({ q }) => {
  let list = db.runs.filter((r) => r.status !== 'deleted' || q.get('status') === 'deleted').slice().reverse();
  if (q.get('job_id')) list = list.filter((r) => r.job_id === +q.get('job_id'));
  if (q.get('status')) list = list.filter((r) => r.status === q.get('status'));
  if (q.get('server_id')) list = list.filter((r) => r.server_id === +q.get('server_id'));
  if (q.get('method')) list = list.filter((r) => r.method === q.get('method'));
  const text = (q.get('q') || '').toLowerCase();
  if (text) list = list.filter((r) => (r.job_name + ' ' + r.server_name + ' ' + r.dir_name).toLowerCase().includes(text));
  const limit = +q.get('limit') || 50;
  const off = +q.get('offset') || 0;
  return { items: list.slice(off, off + limit).map(runView), total: list.length };
});
route('GET', '/runs/:id', ({ p }) => {
  const r = find(db.runs, p.id, 'backup');
  const out = { run: runView(r), dependents: db.runs.filter((x) => x.parent_id === r.id && x.status !== 'deleted').map((x) => x.id) };
  const t = tasks.get(`backup:${r.id}`) || tasks.get(`verify:${r.id}`);
  if (t) out.task = taskInfo(t);
  out.drill_runs = db.drillRuns.filter((x) => x.run_id === r.id).slice(-20).reverse().map(drillRunView);
  out.drills = db.drills.filter((d) => d.job_id === r.job_id).map((d) => ({ id: d.id, name: d.name, target_server_name: d.target_server_name, running: drillRunning(d.id) }));
  if (r.type === 'incremental') {
    const chain = [];
    let cur = r;
    while (cur) { chain.unshift({ id: cur.id, type: cur.type, dir_name: cur.dir_name, status: cur.status }); cur = cur.parent_id ? db.runs.find((x) => x.id === cur.parent_id) : null; }
    out.chain = chain;
  }
  return out;
});
route('GET', '/runs/:id/log', ({ p, q }) => logPage(backupLog(find(db.runs, p.id, 'backup')), q));
route('POST', '/runs/:id/verify', ({ p }) => { startVerify(find(db.runs, p.id, 'backup')); return { ok: true }; });
route('POST', '/runs/:id/cancel', ({ p }) => {
  for (const k of [`backup:${p.id}`, `verify:${p.id}`]) {
    if (tasks.has(k)) {
      tasks.delete(k);
      const r = find(db.runs, p.id, 'backup');
      if (k.startsWith('backup')) { r.status = 'canceled'; r.finished_at = iso(now()); r.error = 'Canceled'; r.log.push(`${stamp(now())} Canceled by demo`); } else r.verify_status = '';
    }
  }
  return { ok: true };
});
route('POST', '/runs/:id/delete', ({ p }) => { const r = find(db.runs, p.id, 'backup'); r.status = 'deleted'; audit('run_delete', `run:${r.job_name}`, r.dir_name); return { ok: true }; });
route('DELETE', '/runs/:id', ({ p }) => { const r = find(db.runs, p.id, 'backup'); r.status = 'deleted'; return { ok: true }; });
route('POST', '/runs/:id/copy', ({ p, body }) => {
  const r = find(db.runs, p.id, 'backup');
  const s = find(db.storages, body.storage_id, 'storage');
  if (!r.locations.some((l) => l.storage_id === s.id)) r.locations.push({ id: r.id * 10 + 9, run_id: r.id, storage_id: s.id, storage_name: s.name, storage_type: s.type, path: r.locations[0] ? r.locations[0].path : '', status: 'ok', error: '', size_bytes: r.size_bytes, created_at: iso(now()), updated_at: iso(now()) });
  return { ok: true };
});
route('POST', '/runs/:id/restore-check', ({ p, body }) => {
  const r = find(db.runs, p.id, 'backup');
  const sv = find(db.servers, body.target_server_id || body.server_id, 'target server');
  const same = sv.id === r.server_id;
  const info = serverInfo(sv).info;
  const warnings = [];
  const suggest = { strip_definer: !same, convert_collations: false, skip_gtid_purged: false };
  if (sv.flavorName === 'mariadb' && r.meta.flavor !== 'mariadb') { warnings.push(`Moving from MySQL ${r.meta.server_version} to MariaDB ${sv.version}: tables, views and routines usually load once collations are converted, but engine- or flavor-specific features may need manual fixes.`); suggest.convert_collations = true; }
  if (r.meta.gtid && r.method === 'mysqldump' && r.meta.flavor !== 'mariadb') { warnings.push('The dump sets GTID_PURGED: this fails on a server that already has a GTID history and needs SUPER. Skip it unless you are building a replica of the source.'); suggest.skip_gtid_purged = true; }
  return { ok: true, error: '', version: sv.version, flavor: sv.flavorName, hostname: sv.name, same_server: same, existing: info.databases.filter((d) => !d.system).map((d) => d.name), warnings, suggest };
});
route('POST', '/runs/:id/restore', ({ p, body }) => {
  if (body.confirm !== 'RESTORE' && body.confirm !== undefined) fail('type RESTORE to confirm');
  return startRestore(find(db.runs, p.id, 'backup'), body);
});
route('POST', '/pitr/plan', ({ body }) => {
  const t0 = body.target || { type: 'latest' };
  let r;
  if (body.run_id) r = find(db.runs, body.run_id, 'backup');
  else {
    // the newest successful backup of the job before the target
    const until = t0.type === 'time' && t0.time ? iso(new Date(t0.time).getTime()) : iso(now());
    r = db.runs.filter((x) => x.job_id === +body.job_id && x.status === 'success' && x.started_at <= until).at(-1)
      || fail('no successful backup of this job before the target');
  }
  const job = db.jobs.find((j) => j.id === r.job_id);
  if (!job || !job.options.binlog_archive) return { ok: false, error: 'this job does not archive binlogs: point-in-time recovery is not available' };
  const t = body.target || { type: 'latest' };
  const files = db.binlogs.filter((b) => b.job_id === job.id && b.last_ts >= r.started_at);
  if (!files.length) return { ok: false, error: 'no binlog archived after this backup yet' };
  let until = files.at(-1).last_ts;
  if (t.type === 'time' && t.time) until = iso(new Date(t.time).getTime());
  const used = files.filter((b) => b.first_ts <= until);
  const label = { latest: 'the latest archived binlog', time: `the moment ${until}`, position: `${t.file}:${t.pos}`, gtid: `just before GTID ${t.gtid}` }[t.type] || t.type;
  return { ok: true, target: label, base: { id: r.id, dir_name: r.dir_name, method: r.method, type: r.type, started_at: r.started_at, consistent_at: r.started_at, binlog_file: r.meta.binlog_file, binlog_pos: r.meta.binlog_pos },
    run_ok: true, files: used.length, bytes: used.reduce((a, b) => a + b.stored_bytes, 0), first_file: used[0] && used[0].file_name, last_file: used.at(-1) && used.at(-1).file_name, coverage: files.at(-1).last_ts, warnings: [] };
});

// restores
route('GET', '/restores', ({ q }) => {
  const list = db.restores.slice().reverse();
  const limit = +q.get('limit') || 50;
  const off = +q.get('offset') || 0;
  return { items: list.slice(off, off + limit).map(({ log, ...x }) => x), total: list.length };
});
route('GET', '/restores/:id', ({ p }) => {
  const { log, ...x } = find(db.restores, p.id, 'restore');
  const out = { restore: x };
  const t = tasks.get(`restore:${x.id}`);
  if (t) out.task = taskInfo(t);
  return out;
});
route('GET', '/restores/:id/log', ({ p, q }) => logPage(restoreLog(find(db.restores, p.id, 'restore')), q));
route('POST', '/restores/:id/cancel', ({ p }) => {
  const x = find(db.restores, p.id, 'restore');
  if (tasks.delete(`restore:${x.id}`)) { x.status = 'canceled'; x.finished_at = iso(now()); x.error = 'Canceled'; }
  return { ok: true };
});

// restore drills
route('GET', '/drills', ({ q }) => db.drills.filter((d) => !+q.get('job_id') || d.job_id === +q.get('job_id')).map(drillView));
route('GET', '/drills/:id', ({ p }) => drillView(find(db.drills, p.id, 'drill')));
route('POST', '/drills', ({ body }) => {
  const d = { id: nextId(db.drills), created_at: iso(now()) };
  applyDrill(d, body);
  db.drills.push(d);
  audit('drill_create', `drill:${d.name}`);
  return drillView(d);
});
route('PUT', '/drills/:id', ({ p, body }) => { const d = find(db.drills, p.id, 'drill'); applyDrill(d, body); audit('drill_update', `drill:${d.name}`); return drillView(d); });
route('DELETE', '/drills/:id', ({ p }) => {
  const d = find(db.drills, p.id, 'drill');
  if (drillRunning(d.id)) fail('the drill is running — cancel it first');
  db.drills = db.drills.filter((x) => x.id !== d.id);
  for (const x of db.drillRuns) if (x.drill_id === d.id) x.drill_id = null;
  return { ok: true };
});
route('POST', '/drills/:id/run', ({ p, body }) => drillRunView(startDrill(find(db.drills, p.id, 'drill'), +body.run_id || 0)));
route('GET', '/drill-runs', ({ q }) => {
  let list = db.drillRuns.slice().reverse();
  for (const k of ['drill_id', 'job_id', 'run_id']) if (+q.get(k)) list = list.filter((x) => x[k] === +q.get(k));
  const limit = +q.get('limit') || 50;
  const off = +q.get('offset') || 0;
  return { items: list.slice(off, off + limit).map(drillRunView), total: list.length };
});
route('GET', '/drill-runs/:id', ({ p }) => {
  const x = find(db.drillRuns, p.id, 'drill run');
  const out = { drill_run: drillRunView(x) };
  const t = tasks.get(`drill:${x.id}`);
  if (t) out.task = taskInfo(t);
  return out;
});
route('GET', '/drill-runs/:id/log', ({ p, q }) => logPage(drillLog(find(db.drillRuns, p.id, 'drill run')), q));
route('POST', '/drill-runs/:id/cancel', ({ p }) => {
  const x = find(db.drillRuns, p.id, 'drill run');
  if (tasks.delete(`drill:${x.id}`)) {
    x.status = 'canceled';
    x.error = 'Canceled';
    x.finished_at = iso(now());
    x.log.push(`${stamp(now())} DROP DATABASE IF EXISTS of the temporary copies`);
    x.log.push(`${stamp(now())} DRILL CANCELED`);
  }
  return { ok: true };
});

// servers
route('GET', '/servers', () => db.servers.map(serverView));
route('GET', '/servers/:id', ({ p }) => serverView(find(db.servers, p.id, 'server')));
route('GET', '/servers/:id/info', ({ p }) => {
  const s = find(db.servers, p.id, 'server');
  if (s.agent_id && !db.agents.find((a) => a.id === s.agent_id).live.online) return { ok: false, error: `server ${s.name}: agent is offline — start it on the database host (see the Agents page)` };
  return serverInfo(s);
});
function applyServer(s, b) {
  if (!String(b.name || '').trim()) fail('server name is required');
  if (!b.host && !b.socket) fail('host or socket is required');
  if (b.agent_id && b.wg_peer_id) fail('connect through an agent or through WireGuard, not both');
  Object.assign(s, { name: b.name.trim(), host: b.host || '', port: b.port || 3306, socket: b.socket || '', username: b.username || 'dumpcat', tls_mode: b.tls_mode || '', ca_cert: b.ca_cert || '', flavor: b.flavor || 'auto',
    datadir: b.datadir || s.datadir || '', service_name: b.service_name || '', os_user: b.os_user || '', notes: b.notes || '', restore_username: b.restore_username || '', has_password: true,
    has_restore_password: !!b.restore_username, agent_id: b.agent_id || null, wg_peer_id: b.wg_peer_id || null, updated_at: iso(now()) });
}
route('POST', '/servers/test', ({ body }) => {
  const s = { id: 99, version: '8.0.39', flavorName: body.flavor && body.flavor !== 'auto' ? body.flavor : 'mysql', comment: 'MySQL Community Server - GPL', dbs: [['app', 1.8 * 1024 * MiB, 64], ['app_logs', 420 * MiB, 6]] };
  applyServer(s, body);
  return serverInfo(s);
});
route('POST', '/servers', ({ body }) => {
  const s = { id: nextId(db.servers), version: '8.0.39', flavorName: 'mysql', comment: 'MySQL Community Server - GPL', dbs: [['app', 1.8 * 1024 * MiB, 64], ['app_logs', 420 * MiB, 6]], created_at: iso(now()) };
  applyServer(s, body);
  db.servers.push(s);
  audit('server_create', `server:${s.name}`);
  return serverView(s);
});
route('PUT', '/servers/:id', ({ p, body }) => { const s = find(db.servers, p.id, 'server'); applyServer(s, body); return serverView(s); });
route('DELETE', '/servers/:id', ({ p }) => {
  const s = find(db.servers, p.id, 'server');
  const used = db.jobs.filter((j) => j.server_id === s.id);
  if (used.length) fail(`the server is used by job(s): ${used.map((j) => j.name).join(', ')}`);
  const sandbox = db.drills.filter((d) => d.target_server_id === s.id);
  if (sandbox.length) fail(`server is the sandbox of ${sandbox.length} restore drill(s) — change or delete them first`);
  db.servers = db.servers.filter((x) => x.id !== s.id);
  return { ok: true };
});

// storages
route('GET', '/storage-types', () => ({ types: TYPES.storage_types, oauth_callback: 'https://backup.example.com/api/oauth/callback' }));
route('GET', '/storages', () => db.storages.map((s) => storageView(s, false)));
route('GET', '/storages/:id', ({ p }) => storageView(find(db.storages, p.id, 'storage'), true));
const describe = (type, c) => ({ local: c.path, s3: `s3://${c.bucket}/${c.prefix || ''}`, sftp: `sftp://${c.user}@${c.host}:${c.port || 22}/${c.folder || ''}`, ftp: `ftp://${c.user}@${c.host}`, webdav: c.url }[type] || `${type}: ${c.folder || ''}`);
function applyStorage(s, b) {
  if (!String(b.name || '').trim()) fail('storage name is required');
  const td = TYPES.storage_types.find((t) => t.type === b.type) || fail('unknown storage type');
  const cfg = { ...(s.config || {}) };
  for (const [k, v] of Object.entries(b.config || {})) if (v !== '__SECRET_UNCHANGED__') cfg[k] = v;
  for (const f of td.fields) if (f.required && !cfg[f.key]) fail(`${f.label} is required`);
  Object.assign(s, { name: b.name.trim(), type: b.type, type_label: td.label, config: cfg, describe: describe(b.type, cfg), updated_at: iso(now()) });
}
route('POST', '/storages/test', () => ({ ok: true, message: 'Connected: write, read and delete of a test file OK (demo)' }));
route('POST', '/storages', ({ body }) => {
  const s = { id: nextId(db.storages), created_at: iso(now()), last_test_at: null, last_test_ok: null, last_test_msg: '' };
  applyStorage(s, body);
  db.storages.push(s);
  audit('storage_create', `storage:${s.name}`, s.type);
  return storageView(s, true);
});
route('PUT', '/storages/:id', ({ p, body }) => { const s = find(db.storages, p.id, 'storage'); applyStorage(s, body); return storageView(s, true); });
route('DELETE', '/storages/:id', ({ p }) => {
  const s = find(db.storages, p.id, 'storage');
  const v = storageView(s, false);
  if (v.in_use.length) fail(`the storage is used by job(s): ${v.in_use.join(', ')}`);
  db.storages = db.storages.filter((x) => x.id !== s.id);
  return { ok: true };
});
route('POST', '/storages/:id/test', ({ p }) => {
  const s = find(db.storages, p.id, 'storage');
  s.last_test_at = iso(now());
  if (s.name === 'dr-site') { s.last_test_ok = false; s.last_test_msg = 'dial tcp 203.0.113.80:22: i/o timeout'; return { ok: false, error: s.last_test_msg }; }
  s.last_test_ok = true;
  s.last_test_msg = s.last_test_msg && s.last_test_ok ? s.last_test_msg : 'Write, read and delete of a test file OK';
  return { ok: true, message: `${s.name}: ${s.last_test_msg}` };
});
route('GET', '/storages/:id/usage', ({ p }) => {
  const s = find(db.storages, p.id, 'storage');
  return s.usage ? { ok: true, usage: s.usage } : { ok: true, usage: { total: 0, used: db.runs.filter((r) => r.locations.some((l) => l.storage_id === s.id)).reduce((a, r) => a + r.size_bytes, 0), free: 0 } };
});
route('GET', '/storages/:id/browse', ({ p, q }) => {
  const s = find(db.storages, p.id, 'storage');
  const path = (q.get('path') || '').replace(/^\/+|\/+$/g, '');
  const runs = db.runs.filter((r) => r.status !== 'deleted' && r.locations.some((l) => l.storage_id === s.id && l.status === 'ok'));
  const entries = [];
  if (!path) {
    for (const j of new Set(runs.map((r) => `${r.job_name}-${r.job_id}`))) entries.push({ path: j, name: j, size: 0, is_dir: true, mod_time: iso(now() - H) });
  } else if (!path.includes('/')) {
    for (const r of runs.filter((x) => `${x.job_name}-${x.job_id}` === path)) entries.push({ path: `${path}/${r.dir_name}`, name: r.dir_name, size: 0, is_dir: true, mod_time: r.finished_at });
  } else {
    const r = runs.find((x) => path.endsWith(x.dir_name));
    if (r) {
      entries.push({ path: `${path}/manifest.json`, name: 'manifest.json', size: 2400 + r.files.length * 300, is_dir: false, mod_time: r.finished_at });
      for (const f of r.files) entries.push({ path: `${path}/${f.name}`, name: f.name, size: f.size, is_dir: false, mod_time: r.finished_at });
    }
  }
  return { entries, path };
});
route('POST', '/storages/:id/import', () => ({ found: 0, imported: 0, linked: 0, skipped: 0, errors: [] }));
route('POST', '/oauth/start', () => notInDemo('Signing in to Google or Microsoft'));
route('POST', '/oauth/exchange', () => notInDemo('Signing in to Google or Microsoft'));
route('GET', '/oauth/result', () => notInDemo('Signing in to Google or Microsoft'));

// agents
const agentSetup = (v) => ({ ...v, server_url: 'https://backup.example.com', release_url: 'https://downloads.dumpcat.cc', version: VERSION });
route('GET', '/agents', () => agentSetup({ items: db.agents.map(agentView) }));
route('POST', '/agents', ({ body }) => {
  if (!String(body.name || '').trim()) fail('agent name is required (max 100 characters)');
  if (db.agents.some((a) => a.name.toLowerCase() === body.name.trim().toLowerCase())) fail('an agent with this name already exists');
  const key = 'dca_demo' + Math.random().toString(36).slice(2, 10) + 'NotARealKeyItOnlyWorksInTheDemo';
  const a = { id: nextId(db.agents), name: body.name.trim(), key_prefix: key.slice(0, 12), created_at: iso(now()), updated_at: iso(now()), last_seen_at: null, last_ip: '', version: '', hostname: '', os: '', live: { online: false } };
  db.agents.push(a);
  audit('agent_create', `agent:${a.name}`);
  return agentSetup({ ...agentView(a), key });
});
route('PUT', '/agents/:id', ({ p, body }) => { const a = find(db.agents, p.id, 'agent'); a.name = String(body.name || a.name).trim(); return agentView(a); });
route('POST', '/agents/:id/rotate', ({ p }) => {
  const a = find(db.agents, p.id, 'agent');
  const key = 'dca_demo' + Math.random().toString(36).slice(2, 10) + 'NotARealKeyItOnlyWorksInTheDemo';
  a.key_prefix = key.slice(0, 12);
  a.live = { online: false };
  return agentSetup({ ...agentView(a), key });
});
route('DELETE', '/agents/:id', ({ p }) => {
  const a = find(db.agents, p.id, 'agent');
  const used = db.servers.filter((s) => s.agent_id === a.id);
  if (used.length) fail(`the agent is used by server(s): ${used.map((s) => s.name).join(', ')} — switch them to another connection first`);
  db.agents = db.agents.filter((x) => x.id !== a.id);
  return { ok: true };
});

// WireGuard
route('GET', '/wireguard', () => wgView());
route('PUT', '/wireguard', ({ body }) => {
  Object.assign(db.wireguard.config, { enabled: !!body.enabled, listen_port: body.listen_port || 51820, address: body.address || '10.66.0.1/24', endpoint: body.endpoint || '' });
  return { config: db.wireguard.config, state: wgView().state };
});
function peerConfig(p) {
  const w = db.wireguard.config;
  return `# DumpCat WireGuard peer "${p.name}" — install as /etc/wireguard/dumpcat.conf\n[Interface]\nPrivateKey = (demo — a real install shows the peer's private key here)\nAddress = ${p.address}/32\n\n[Peer]\n# DumpCat\nPublicKey = ${w.public_key}\nPresharedKey = (demo)\nEndpoint = ${w.endpoint || 'backup.example.com'}:${w.listen_port}\nAllowedIPs = ${w.address.split('/')[0]}/32\nPersistentKeepalive = 25\n`;
}
const peerSetup = (p) => ({ peer: p, config: peerConfig(p), endpoint: `${db.wireguard.config.endpoint}:${db.wireguard.config.listen_port}`, server_address: db.wireguard.config.address.split('/')[0], enabled: db.wireguard.config.enabled });
route('POST', '/wireguard/peers', ({ body }) => {
  if (!String(body.name || '').trim()) fail('peer name is required (max 100 characters)');
  const used = new Set(db.wireguard.peers.map((x) => x.address));
  let n = 2;
  while (used.has(`10.66.0.${n}`)) n++;
  const p = { id: nextId(db.wireguard.peers), name: body.name.trim(), public_key: btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32)))), address: body.address || `10.66.0.${n}`, networks: body.networks || [], last_endpoint: '', created_at: iso(now()), updated_at: iso(now()), live: { online: false } };
  db.wireguard.peers.push(p);
  const { live, ...peer } = p;
  return peerSetup(peer);
});
route('PUT', '/wireguard/peers/:id', ({ p, body }) => {
  const x = find(db.wireguard.peers, p.id, 'peer');
  x.name = String(body.name || x.name).trim();
  x.networks = body.networks || [];
  const { live, ...peer } = x;
  return { peer, status: {}, servers: [] };
});
route('POST', '/wireguard/peers/:id/rotate', ({ p }) => { const x = find(db.wireguard.peers, p.id, 'peer'); x.live = { online: false }; const { live, ...peer } = x; return peerSetup(peer); });
route('DELETE', '/wireguard/peers/:id', ({ p }) => {
  const x = find(db.wireguard.peers, p.id, 'peer');
  const used = db.servers.filter((s) => s.wg_peer_id === x.id);
  if (used.length) fail(`the peer is used by server(s): ${used.map((s) => s.name).join(', ')} — switch them to another connection first`);
  db.wireguard.peers = db.wireguard.peers.filter((y) => y.id !== x.id);
  return { ok: true };
});

// binlog archiving
route('GET', '/binlog', () => db.jobs.filter((j) => j.options.binlog_archive).map((j) => {
  const files = db.binlogs.filter((b) => b.job_id === j.id);
  const last = files.at(-1);
  return {
    archive: true, enabled: j.enabled, job_id: j.id, job_name: j.name, method: j.method, rotate_minutes: j.options.binlog_rotate_minutes, server_name: j.server_name,
    stats: { files: files.length, raw_bytes: files.reduce((a, b) => a + b.size_bytes, 0), stored_bytes: files.reduce((a, b) => a + b.stored_bytes, 0), first: files[0] && files[0].first_ts, last: last && last.last_ts },
    status: { job_id: j.id, job_name: j.name, server_name: j.server_name, state: 'streaming', error: '', error_at: null, next_try: null, since: iso(now() - 4 * 24 * H), restarts: 0,
      current_file: `mysql-bin.${String((last ? last.seq : 900) + 1).padStart(6, '0')}`, received_bytes: Math.round(3.2 * MiB + (now() % 600000) * 20), received_at: iso(now() - 2000),
      server_file: `mysql-bin.${String((last ? last.seq : 900) + 1).padStart(6, '0')}`, server_pos: Math.round(3.2 * MiB + (now() % 600000) * 20), checked_at: iso(now() - 3000),
      lag_bytes: 0, lag_seconds: 0, pending_files: 0, pending_bytes: 0, upload_error: '', last_archived: last && last.file_name, archived_at: last && last.created_at, archived_until: last && last.last_ts, rotated_at: last && last.last_ts },
  };
}));

// notifications
route('GET', '/notify-types', () => TYPES.notify_types);
route('GET', '/notifications', () => db.channels);
function applyChannel(c, b) {
  if (!String(b.name || '').trim()) fail('channel name is required');
  const cfg = { ...(c.config || {}) };
  for (const [k, v] of Object.entries(b.config || {})) if (v !== '__SECRET_UNCHANGED__') cfg[k] = v;
  Object.assign(c, { name: b.name.trim(), type: b.type || c.type, enabled: !!b.enabled, on_success: !!b.on_success, on_failure: !!b.on_failure, config: cfg, updated_at: iso(now()) });
}
route('POST', '/notifications', ({ body }) => { const c = { id: nextId(db.channels), created_at: iso(now()) }; applyChannel(c, body); db.channels.push(c); return c; });
route('PUT', '/notifications/:id', ({ p, body }) => { const c = find(db.channels, p.id, 'channel'); applyChannel(c, body); return c; });
route('DELETE', '/notifications/:id', ({ p }) => { db.channels = db.channels.filter((x) => x.id !== +p.id); return { ok: true }; });
route('POST', '/notifications/test', () => ({ ok: false, error: 'The demo sends nothing: install DumpCat to get real notifications.' }));
route('POST', '/notifications/:id/test', () => ({ ok: false, error: 'The demo sends nothing: install DumpCat to get real notifications.' }));

// users, settings, system, updates, audit
route('GET', '/users', () => db.users);
route('POST', '/users', ({ body }) => {
  if (!String(body.username || '').trim()) fail('username is required');
  if (db.users.some((u) => u.username === body.username.trim())) fail('this username already exists');
  const u = { id: nextId(db.users), username: body.username.trim(), email: body.email || '', role: body.role || 'operator', totp_enabled: false, disabled: false, created_at: iso(now()), last_login_at: null };
  db.users.push(u);
  return u;
});
route('PUT', '/users/:id', ({ p, body }) => { const u = find(db.users, p.id, 'user'); Object.assign(u, { email: body.email ?? u.email, role: body.role || u.role, disabled: !!body.disabled }); return u; });
route('DELETE', '/users/:id', ({ p }) => { if (+p.id === 1) fail('you cannot delete yourself'); db.users = db.users.filter((u) => u.id !== +p.id); return { ok: true }; });
route('POST', '/users/:id/reset-2fa', ({ p }) => { find(db.users, p.id, 'user').totp_enabled = false; return { ok: true }; });
route('GET', '/settings', () => db.settings);
route('PUT', '/settings', ({ body }) => { Object.assign(db.settings, body); audit('settings_update', 'settings'); return db.settings; });
const TOOL = (name, purpose, version, found = true, mariadb = false) => ({ name, path: found ? `/usr/bin/${name}` : '', version: found ? version : '', found, mariadb, purpose, hint: found ? '' : `apt install ${name}` });
route('GET', '/system', () => ({
  base_url: db.settings.base_url, cpus: 8, data_dir: '/var/lib/dumpcat', data_disk: { free: 412 * 1024 ** 3, total: 980 * 1024 ** 3 }, go: 'go1.26.2', hostname: 'backup-01', listen: ':6369', os: 'linux/amd64',
  staging_dir: '/var/lib/dumpcat/staging', staging_disk: { free: 412 * 1024 ** 3, total: 980 * 1024 ** 3 }, tls: true, uid: 0, version: VERSION,
  tools: [
    TOOL('mysqldump', 'Logical backup (MySQL)', 'mysqldump  Ver 8.0.39 for Linux on x86_64 (MySQL Community Server - GPL)'),
    TOOL('mariadb-dump', 'Logical backup (MariaDB)', 'mariadb-dump from 11.4.4-MariaDB, client 10.19 for debian-linux-gnu (x86_64)', true, true),
    TOOL('mysql', 'Logical restore (MySQL client)', 'mysql  Ver 8.0.39 for Linux on x86_64'),
    TOOL('mariadb', 'Logical restore (MariaDB client)', 'mariadb from 11.4.4-MariaDB, client 15.2', true, true),
    TOOL('mydumper', 'Parallel logical backup (large databases)', 'mydumper v0.16.9-1, built against MySQL 8.0.39'),
    TOOL('myloader', 'Parallel restore for mydumper', 'myloader v0.16.9-1, built against MySQL 8.0.39'),
    TOOL('xtrabackup', 'Hot physical backup (MySQL/Percona)', 'xtrabackup version 8.0.35-31 based on MySQL server 8.0.35 Linux (x86_64)'),
    TOOL('xbstream', 'Extracts xtrabackup streams', 'xbstream  Ver 8.0.35-31 for Linux (x86_64)'),
    TOOL('mariabackup', 'Hot physical backup (MariaDB)', 'mariabackup based on MariaDB server 11.4.4-MariaDB Linux (x86_64)', true, true),
    TOOL('mbstream', 'Extracts mariabackup streams', 'mbstream Ver 1.0', true, true),
    TOOL('mysqlbinlog', 'Continuous binlog archiving & PITR (MySQL)', 'mysqlbinlog  Ver 8.0.39 for Linux on x86_64'),
    TOOL('mariadb-binlog', 'Continuous binlog archiving & PITR (MariaDB)', 'mariadb-binlog Ver 3.5 for debian-linux-gnu at x86_64', true, true),
    TOOL('rclone', 'Uploads to S3/Google Drive/OneDrive/FTP/SFTP...', 'rclone v1.68.2'),
  ],
}));
route('POST', '/system/tools/refresh', () => ({ ok: true }));
const updateView = () => ({ current: VERSION, platform: 'linux/amd64', server: 'https://downloads.dumpcat.cc', server_source: 'build', latest: null, available: false, checked_at: iso(now() - 3 * H),
  state: { phase: 'idle', started_at: '0001-01-01T00:00:00Z' }, last: null, rollback: { available: true, version: '1.3.0', reason: '' }, signing: { required: true, keys: ['gmAZk8uUPf8Ep4eR+TaUu0cTYWuaznIuMOlesAcB4BE='] } });
route('GET', '/update', () => updateView());
route('POST', '/update/check', () => ({ ...updateView(), checked_at: iso(now()) }));
route('POST', '/update/install', () => notInDemo('Installing updates'));
route('POST', '/update/cancel', () => ({ ok: true }));
route('POST', '/update/rollback', () => notInDemo('Rolling back'));
route('GET', '/audit', ({ q }) => {
  let list = db.audit;
  const text = (q.get('q') || '').toLowerCase();
  if (text) list = list.filter((a) => (a.username + ' ' + a.action + ' ' + a.target + ' ' + a.details).toLowerCase().includes(text));
  const limit = +q.get('limit') || 100;
  const off = +q.get('offset') || 0;
  return { items: list.slice(off, off + limit), total: list.length };
});

// ---------- the client ----------
export async function api(method, path, body) {
  await new Promise((res) => setTimeout(res, 60 + Math.random() * 140)); // feels like a network
  const [p, query] = path.split('?');
  const q = new URLSearchParams(query || '');
  for (const [m, re, fn] of routes) {
    if (m !== method) continue;
    const match = re.exec(p);
    if (!match) continue;
    try {
      return clone(fn({ p: match.groups || {}, q, body: body || {} }));
    } catch (e) {
      if (e instanceof ApiError) throw e;
      throw new ApiError(e.message || String(e), 400);
    }
  }
  throw new ApiError(`${method} ${p} is not part of the demo`, 404);
}

export const get = (p) => api('GET', p);
export const post = (p, b = {}) => api('POST', p, b);
export const put = (p, b) => api('PUT', p, b);
export const del = (p) => api('DELETE', p);
