// Sample data for the DumpCat demo on dumpcat.cc: a small company with four
// database servers, four storages and two weeks of backup history. Dates are
// relative to the moment the page loads, so the demo always looks current.

export const VERSION = '1.8.0'; // set by scripts/build-demo.py

const MIN = 60e3;
const H = 60 * MIN;
const D = 24 * H;
export const iso = (t) => new Date(t).toISOString();
// backup folder names use the local time of the DumpCat host, like 20261019-020000-full
export const dirStamp = (t) => {
  const d = new Date(t);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
};
const GiB = 1024 ** 3;
const MiB = 1024 ** 2;

// a small deterministic random generator: the same history on every load
function rng(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const hex = (r, n) => Array.from({ length: n }, () => Math.floor(r() * 16).toString(16)).join('');
const uuid = (r) => `${hex(r, 8)}-${hex(r, 4)}-4${hex(r, 3)}-a${hex(r, 3)}-${hex(r, 12)}`;

export function seed(now) {
  const r = rng(20261019);
  const day = (daysAgo, h, m = 0) => {
    const d = new Date(now - daysAgo * D);
    d.setHours(h, m, 0, 0);
    return d.getTime();
  };
  const created = iso(now - 120 * D);

  const users = [
    { id: 1, username: 'demo', email: 'demo@dumpcat.cc', role: 'admin', totp_enabled: true, disabled: false, created_at: created, last_login_at: iso(now) },
    { id: 2, username: 'anna', email: 'anna@example.com', role: 'operator', totp_enabled: true, disabled: false, created_at: iso(now - 90 * D), last_login_at: iso(now - 3 * H) },
    { id: 3, username: 'auditor', email: 'audit@example.com', role: 'viewer', totp_enabled: false, disabled: false, created_at: iso(now - 40 * D), last_login_at: iso(now - 6 * D) },
  ];

  const agents = [
    { id: 1, name: 'crm-host', key_prefix: 'dca_7Qm2xK9p', created_at: iso(now - 30 * D), updated_at: iso(now - 30 * D), last_seen_at: iso(now - 4 * D), last_ip: '203.0.113.24', version: VERSION, hostname: 'crm-01', os: 'linux/amd64',
      live: { online: true, since: iso(now - 4 * D), remote_addr: '203.0.113.24', allow: ['127.0.0.1:3306', '[::1]:3306', 'unix:/run/mysqld/mysqld.sock'], streams: 0,
        caps: { physical: { allowed: true, user: 'mysql', datadir: '/var/lib/mysql', tools: [
          { name: 'mariabackup', found: true, version: 'mariabackup based on MariaDB server 11.4.4-MariaDB Linux (x86_64)' },
          { name: 'xtrabackup', found: false, hint: 'apt install percona-xtrabackup-80' }] }, mssql: {} } } },
    { id: 2, name: 'legacy-erp', key_prefix: 'dca_Lq0vB3sE', created_at: iso(now - 12 * D), updated_at: iso(now - 12 * D), last_seen_at: iso(now - 2 * D - 5 * H), last_ip: '198.51.100.7', version: '1.2.0', hostname: 'erp-old', os: 'windows/amd64', live: { online: false } },
    { id: 3, name: 'sql-host', key_prefix: 'dca_V8tN4cR1', created_at: iso(now - 6 * D), updated_at: iso(now - 6 * D), last_seen_at: iso(now - 6 * D), last_ip: '192.0.2.88', version: VERSION, hostname: 'sql01', os: 'linux/amd64',
      live: { online: true, since: iso(now - 6 * D), remote_addr: '192.0.2.88', allow: ['127.0.0.1:1433'], streams: 0,
        caps: { physical: { allowed: false }, mssql: { dir: '/var/opt/mssql/backup', user: 'mssql' } } } },
  ];

  const wireguard = {
    config: { enabled: true, listen_port: 51820, address: '10.66.0.1/24', endpoint: 'backup.example.com', public_key: 'mL2r8Yp7Q0bJc5n1sE3fW9tK4uV6xZ2aB8dC0eF1gH4=' },
    peers: [
      { id: 1, name: 'dw-01', public_key: 'Qx3wE9rT2yU5iO8pA1sD4fG7hJ0kL3zX6cV9bN2mQ5w=', address: '10.66.0.2', networks: [], last_endpoint: '192.0.2.40:41820', created_at: iso(now - 20 * D), updated_at: iso(now - 20 * D),
        live: { online: true, endpoint: '192.0.2.40:41820', rx: 18.4 * GiB, tx: 412 * MiB } },
      { id: 2, name: 'branch-hq', public_key: 'Zp8nM4bV1cX7zL2kJ5hG9fD3sA6qW0eR8tY1uI4oP7k=', address: '10.66.0.3', networks: ['192.168.50.0/24'], last_endpoint: '198.51.100.61:51244', created_at: iso(now - 9 * D), updated_at: iso(now - 9 * D),
        live: { online: true, endpoint: '198.51.100.61:51244', rx: 2.1 * GiB, tx: 96 * MiB } },
    ],
  };

  const servers = [
    { id: 1, name: 'db-prod-01', host: '10.0.1.20', port: 3306, socket: '', username: 'dumpcat', tls_mode: 'required', ca_cert: '', flavor: 'auto', version: '8.0.39', datadir: '/var/lib/mysql/', service_name: 'mysql', os_user: 'mysql',
      notes: 'Shop and checkout', restore_username: 'dumpcat_restore', agent_id: null, wg_peer_id: null, flavorName: 'mysql', comment: 'MySQL Community Server - GPL', dbs: [['shop', 6.2 * GiB, 148], ['shop_search', 1.4 * GiB, 22], ['payments', 820 * MiB, 31]] },
    { id: 2, name: 'crm-mariadb', host: '127.0.0.1', port: 3306, socket: '', username: 'dumpcat', tls_mode: '', ca_cert: '', flavor: 'auto', version: '11.4.4-MariaDB', datadir: '/var/lib/mysql/', service_name: 'mariadb', os_user: 'mysql',
      notes: 'Behind the office firewall', restore_username: '', agent_id: 1, wg_peer_id: null, flavorName: 'mariadb', comment: 'MariaDB Server', dbs: [['crm', 2.8 * GiB, 96], ['crm_reports', 640 * MiB, 18]] },
    { id: 3, name: 'analytics', host: '10.66.0.2', port: 3306, socket: '', username: 'dumpcat', tls_mode: '', ca_cert: '', flavor: 'percona', version: '8.0.36-28', datadir: '/data/mysql/', service_name: 'mysqld', os_user: 'mysql',
      notes: 'Data warehouse, reached through WireGuard', restore_username: '', agent_id: null, wg_peer_id: 1, flavorName: 'percona', comment: 'Percona Server (GPL), Release 28', dbs: [['warehouse', 38 * GiB, 214], ['events', 12.5 * GiB, 9]] },
    { id: 4, name: 'staging-db', host: '10.0.9.5', port: 3306, socket: '', username: 'dumpcat', tls_mode: '', ca_cert: '', flavor: 'auto', version: '8.4.3', datadir: '/var/lib/mysql/', service_name: 'mysql', os_user: 'mysql',
      notes: 'Restore target for tests', restore_username: 'root_restore', agent_id: null, wg_peer_id: null, flavorName: 'mysql', comment: 'MySQL Community Server - GPL', dbs: [['shop_staging', 5.9 * GiB, 148]] },
    { id: 5, name: 'erp-db', host: '127.0.0.1', port: 3306, socket: '', username: 'dumpcat', tls_mode: '', ca_cert: '', flavor: 'auto', version: '5.7.44', datadir: '/var/lib/mysql/', service_name: 'mysqld', os_user: 'mysql',
      notes: 'Old ERP, reached through the legacy-erp agent', restore_username: '', agent_id: 2, wg_peer_id: null, flavorName: 'mysql', comment: 'MySQL Community Server (GPL)', dbs: [['erp', 1.9 * GiB, 120]] },
  ].map((s) => ({ ...s, created_at: created, updated_at: created, has_password: true, has_restore_password: !!s.restore_username }));

  const storages = [
    { id: 1, name: 'nas-backups', type: 'local', type_label: 'Local directory / NFS', describe: '/mnt/nas/mysql', config: { path: '/mnt/nas/mysql', folder: '' }, last_test_ok: true, last_test_msg: 'Writable, 7.1 TiB free', usage: { total: 8 * 1024 * GiB, used: 1.1 * 1024 * GiB, free: 6.9 * 1024 * GiB } },
    { id: 2, name: 'r2-offsite', type: 's3', type_label: 'Amazon S3 / S3-compatible', describe: 's3://dumpcat-prod/mysql (Cloudflare)', config: { provider: 'Cloudflare', access_key_id: 'b1c9…', secret_access_key: '__SECRET_UNCHANGED__', region: 'auto', endpoint: 'https://example.r2.cloudflarestorage.com', bucket: 'dumpcat-prod', prefix: 'mysql', storage_class: '', sse: '', force_path_style: 'false', no_check_bucket: 'true', folder: '' }, last_test_ok: true, last_test_msg: 'Bucket reachable, write/delete OK' },
    { id: 3, name: 'gdrive-archive', type: 'gdrive', type_label: 'Google Drive', describe: 'Google Drive: DumpCat archive', config: { auth_mode: 'oauth', client_id: '1234567890-abc.apps.googleusercontent.com', client_secret: '__SECRET_UNCHANGED__', token: '__SECRET_UNCHANGED__', scope: 'drive.file', team_drive: '', root_folder_id: '', folder: 'DumpCat archive' }, last_test_ok: true, last_test_msg: 'Folder reachable' },
    { id: 4, name: 'dr-site', type: 'sftp', type_label: 'SFTP (SSH)', describe: 'sftp://backup@dr.example.com:22/srv/backups', config: { host: 'dr.example.com', port: '22', user: 'backup', pass: '', key_pem: '__SECRET_UNCHANGED__', key_file_pass: '', folder: '/srv/backups' }, last_test_ok: false, last_test_msg: 'dial tcp 203.0.113.80:22: i/o timeout' },
  ].map((s) => ({ ...s, created_at: created, updated_at: created, last_test_at: iso(now - (s.id * 7 + 3) * H) }));

  const jobBase = (j) => ({
    uuid: uuid(r), db_mode: 'include', exclude_tables: [], incr_schedule: '', compression: 'zstd', compression_level: 0, encrypt: false, threads: 4,
    retention: { keep_last: 0, keep_days: 0, keep_daily: 7, keep_weekly: 4, keep_monthly: 6 }, enabled: true, notify_success: false, notify_failure: true, timeout_minutes: 0,
    created_at: created, updated_at: iso(now - 15 * D), has_passphrase: false, ...j,
    options: {
      single_file: false, routines: true, triggers: true, events: true, include_users: true, record_binlog: false, gtid_purged: '', chunk_rows: 500000, long_query_guard: 60,
      lock_mode: '', ftwrl_wait_timeout: 60, slave_info: false, safe_slave_backup: false, low_priority: true, stream_upload: false, verify_after: true, verify_mode: 'one',
      bwlimit: '', extra_args: '', pre_command: '', post_command: '', binlog_archive: false, binlog_rotate_minutes: 15, ...(j.options || {}),
    },
  });
  const tgt = (id, retention = null) => ({ storage_id: id, storage_name: storages[id - 1].name, storage_type: storages[id - 1].type, retention });
  const jobs = [
    jobBase({ id: 1, name: 'shop-nightly', server_id: 1, method: 'mysqldump', databases: ['shop', 'shop_search', 'payments'], schedule: '0 2 * * *', encrypt: true, has_passphrase: true,
      options: { record_binlog: true, binlog_archive: true, gtid_purged: 'AUTO' }, targets: [tgt(1), tgt(2, { keep_last: 0, keep_days: 30, keep_daily: 0, keep_weekly: 0, keep_monthly: 0 })] }),
    jobBase({ id: 2, name: 'crm-every-6h', server_id: 2, method: 'mydumper', databases: ['crm', 'crm_reports'], schedule: '15 */6 * * *', threads: 6,
      retention: { keep_last: 20, keep_days: 0, keep_daily: 0, keep_weekly: 0, keep_monthly: 0 }, targets: [tgt(1)] }),
    jobBase({ id: 3, name: 'warehouse-weekly', server_id: 3, method: 'mydumper', databases: ['warehouse', 'events'], schedule: '0 3 * * 0', threads: 8, compression_level: 6,
      exclude_tables: ['events.raw_clicks'], targets: [tgt(2), tgt(3)], notify_success: true }),
    jobBase({ id: 4, name: 'prod-physical', server_id: 1, method: 'physical', db_mode: 'all', databases: [], schedule: '30 0 * * 6', incr_schedule: '30 0 * * 0-5', threads: 4,
      options: { verify_mode: 'all' }, targets: [tgt(1), tgt(4)] }),
    jobBase({ id: 5, name: 'erp-nightly', server_id: 5, method: 'mysqldump', databases: ['erp'], schedule: '30 1 * * *', targets: [tgt(1), tgt(2)] }),
  ].map((j) => ({ ...j, server_name: servers[j.server_id - 1].name }));

  // ---------- backup history ----------
  const runs = [];
  let rid = 0;
  const growth = (daysAgo) => 1 - daysAgo * 0.004;
  function addRun(job, start, opt = {}) {
    const sv = servers[job.server_id - 1];
    const dbs = job.method === 'physical' ? sv.dbs : sv.dbs.filter(([n]) => job.databases.includes(n));
    const daysAgo = (now - start) / D;
    const type = opt.type || 'full';
    const inc = type === 'incremental' ? 0.06 + r() * 0.04 : 1;
    const raw = Math.round(dbs.reduce((a, [, s]) => a + s, 0) * growth(daysAgo) * inc * (0.97 + r() * 0.06));
    const ratio = job.method === 'physical' ? 0.34 : 0.18;
    const size = opt.status === 'failed' ? 0 : Math.round(raw * ratio);
    const dur = opt.dur || Math.round((raw / (job.method === 'physical' ? 160 * MiB : job.method === 'mydumper' ? 120 * MiB : 45 * MiB)) * 1000 * (0.9 + r() * 0.2));
    const status = opt.status || 'success';
    const dir = `${dirStamp(start)}-${type === 'incremental' ? 'incr' : 'full'}`;
    const ext = job.encrypt ? '.zst.age' : '.zst';
    let files = [];
    if (status !== 'failed') {
      if (job.method === 'physical') files = [{ name: `backup.xbstream${ext}`, size, sha256: hex(r, 64), kind: 'xbstream', database: '', raw_size: raw }];
      else if (job.method === 'mydumper') files = dbs.map(([n, s]) => ({ name: `${n}.tar${ext}`, size: Math.round(size * s / raw * growth(daysAgo)) || 1, sha256: hex(r, 64), kind: 'mydumper', database: n, raw_size: Math.round(s * growth(daysAgo)) }));
      else files = dbs.map(([n, s]) => ({ name: `${n}.sql${ext}`, size: Math.round(size * s / raw * growth(daysAgo)) || 1, sha256: hex(r, 64), kind: 'sql', database: n, raw_size: Math.round(s * growth(daysAgo)) }));
      if (job.options.include_users && job.method !== 'physical') files.push({ name: `_users.sql${ext}`, size: 4210, sha256: hex(r, 64), kind: 'users', database: '', raw_size: 18400 });
    }
    const binlog = { file: `mysql-bin.${String(700 + Math.round(120 - daysAgo * 8)).padStart(6, '0')}`, pos: 1000 + Math.floor(r() * 9e7) };
    const run = {
      id: ++rid, uuid: uuid(r), job_id: job.id, job_name: job.name, job_uuid: job.uuid, job_slug: `${job.name}-${job.id}`, server_id: sv.id, server_name: sv.name,
      method: job.method, type, parent_id: opt.parent || null, base_id: opt.base || null, parent_uuid: '', base_uuid: '', status, trigger: opt.trigger || 'schedule', triggered_by: opt.by || '',
      started_at: iso(start), finished_at: iso(start + dur), size_bytes: size, raw_bytes: status === 'failed' ? 0 : raw, files,
      databases: status === 'failed' || job.method === 'physical' ? [] : dbs.map(([n, s, t]) => ({ name: n, charset: 'utf8mb4', collation: sv.flavorName === 'mariadb' ? 'utf8mb4_uca1400_ai_ci' : 'utf8mb4_0900_ai_ci', size_bytes: Math.round(s * growth(daysAgo)), file: `${n}.sql`, tables: t, rows: Math.round(s * growth(daysAgo) / 210), binlog_file: binlog.file, binlog_pos: binlog.pos })),
      meta: { flavor: sv.flavorName, server_version: sv.version, server_host: sv.name, tool: job.method === 'physical' ? 'xtrabackup version 8.0.35-31' : job.method === 'mydumper' ? 'mydumper v0.16.9-1' : 'mysqldump  Ver 8.0.39 for Linux on x86_64', consistent_at: Math.floor(start / 1000) + 2, ...(job.options.record_binlog ? { binlog_file: binlog.file, binlog_pos: binlog.pos, gtid: `3e11fa47-71ca-11e1-9e33-c80aa9429562:1-${48000000 + rid * 7311}` } : {}) },
      error: opt.error || '', compression: job.compression, encrypted: job.encrypt, dir_name: status === 'failed' ? '' : dir,
      verify_status: status === 'success' && job.options.verify_mode !== 'off' ? (opt.verify || 'ok') : '', verified_at: status === 'success' ? iso(start + dur + 40e3) : null,
      verify_msg: status === 'success' ? `${files.length} files OK on ${job.targets[0].storage_name}${job.targets.length > 1 && job.options.verify_mode === 'one' ? ` (1 of ${job.targets.length} copies)` : ''}, ${Math.max(4, Math.round(dur / 9000))}s` : '',
      locations: status === 'failed' ? [] : job.targets.map((t, i) => {
        const st = opt.failTarget === t.storage_id ? 'failed' : 'ok';
        return { id: rid * 10 + i, run_id: rid, storage_id: t.storage_id, storage_name: t.storage_name, storage_type: t.storage_type, path: `${job.name}-${job.id}/${dir}`, status: st, error: st === 'failed' ? opt.locError : '', size_bytes: size, created_at: iso(start + dur), updated_at: iso(start + dur) };
      }),
      has_passphrase: job.has_passphrase, log: opt.log || null, drill_status: '', drilled_at: null, drill_msg: '',
    };
    if (opt.status === 'partial') run.verify_status = '';
    runs.push(run);
    return run;
  }
  for (let d = 13; d >= 0; d--) {
    // nightly mysqldump of the shop (today's has run at 02:00 if it is past that)
    const t = day(d, 2, 0);
    if (t < now) {
      if (d === 5) addRun(jobs[0], t, { status: 'partial', failTarget: 2, locError: 'upload to r2-offsite: 503 Service Unavailable (retried 3 times)', error: 'stored on 1 of 2 storages' });
      else addRun(jobs[0], t);
    }
    // CRM every 6 hours, through the agent
    for (const h of [0, 6, 12, 18]) {
      const s = day(d, h, 15);
      if (s >= now) continue;
      if (d === 2 && h === 12) addRun(jobs[1], s, { status: 'failed', dur: 2400, error: 'could not connect to MySQL: server crm-mariadb: agent crm-host is offline — start it on the database host (see the Agents page)' });
      else addRun(jobs[1], s);
    }
  }
  // ERP nightly through the legacy-erp agent, which went offline two days ago: overdue now
  for (let d = 13; d >= 0; d--) {
    const t = day(d, 1, 30);
    if (t >= now) continue;
    if (now - t < 2 * D - 5 * H) addRun(jobs[4], t, { status: 'failed', dur: 1500, error: 'could not connect to MySQL: server erp-db: agent legacy-erp is offline — start it on the database host (see the Agents page)' });
    else addRun(jobs[4], t);
  }
  // warehouse on Sundays (through WireGuard), physical full on Saturday + daily incrementals
  for (let d = 13; d >= 0; d--) {
    const dt = new Date(now - d * D);
    if (dt.getDay() === 0 && day(d, 3) < now) addRun(jobs[2], day(d, 3));
  }
  let base = null;
  let parent = null;
  for (let d = 13; d >= 0; d--) {
    const dt = new Date(now - d * D);
    const s = day(d, 0, 30);
    if (s >= now) continue;
    if (dt.getDay() === 6 || !base) {
      base = addRun(jobs[3], s, { type: 'full' });
      parent = base;
    } else {
      parent = addRun(jobs[3], s, { type: 'incremental', base: base.id, parent: parent.id });
    }
  }
  runs.sort((a, b) => a.started_at.localeCompare(b.started_at)).forEach((x, i) => { x.id = i + 1; });
  // fix references after renumbering (incremental chains)
  const byUUID = new Map(runs.map((x) => [x.uuid, x]));
  let lastFull = null;
  let prev = null;
  for (const x of runs.filter((y) => y.job_id === 4)) {
    if (x.type === 'full') { lastFull = x; prev = x; x.parent_id = null; x.base_id = null; } else { x.base_id = lastFull.id; x.parent_id = prev.id; x.base_uuid = lastFull.uuid; x.parent_uuid = prev.uuid; prev = x; }
  }
  for (const x of runs) for (const l of x.locations) { l.run_id = x.id; }
  void byUUID;
  // one backup started by hand this morning
  const manual = runs.filter((x) => x.job_id === 1).at(-1);
  if (manual) { manual.trigger = 'manual'; manual.triggered_by = 'anna'; }

  // ---------- binlog archive of shop-nightly (every 15 minutes) ----------
  const binlogs = [];
  let seq = 812;
  for (let t = now - 3 * D; t < now - 15 * MIN; t += 15 * MIN) {
    const raw = Math.round((6 + r() * 34) * MiB);
    binlogs.push({ id: binlogs.length + 1, job_id: 1, job_uuid: jobs[0].uuid, job_name: 'shop-nightly', server_id: 1, server_name: 'db-prod-01', file_name: `mysql-bin.${String(seq++).padStart(6, '0')}`, seq,
      first_ts: iso(t), last_ts: iso(t + 15 * MIN - 1000), size_bytes: raw, stored_bytes: Math.round(raw * 0.19), sha256: hex(r, 64), raw_sha256: hex(r, 64), compression: 'zstd', encrypted: true,
      path: `shop-nightly-1/binlog/${iso(t).slice(0, 10).replace(/-/g, '')}/mysql-bin.${String(seq - 1).padStart(6, '0')}.zst.age`, created_at: iso(t + 15 * MIN + 20e3),
      locations: [1, 2].map((sid) => ({ id: binlogs.length * 2 + sid, binlog_id: binlogs.length + 1, storage_id: sid, storage_name: storages[sid - 1].name, storage_type: storages[sid - 1].type, status: 'ok', error: '', updated_at: iso(t + 15 * MIN + 30e3) })) });
  }

  // ---------- restores ----------
  const shopRuns = runs.filter((x) => x.job_id === 1 && x.status === 'success');
  const pick = shopRuns.at(-3);
  const restores = [
    { id: 1, run: pick, kind: 'logical', target: 4, status: 'success', started: now - 2 * D - 4 * H, dur: 9 * MIN + 12e3, user: 'anna',
      options: { databases: ['shop'], rename: { shop: 'shop_staging' }, drop_existing: true, fast_mode: true, strip_definer: true } },
    { id: 2, run: shopRuns.at(-2), kind: 'logical', target: 1, status: 'success', started: now - 20 * H, dur: 11 * MIN, user: 'demo',
      options: { databases: ['payments'], rename: { payments: 'payments_recovered' }, drop_existing: false, fast_mode: true, pitr: { type: 'time', time: iso(now - 21 * H) } } },
    { id: 3, run: runs.filter((x) => x.job_id === 2 && x.status === 'success').at(-6), kind: 'logical', target: 4, status: 'failed', started: now - 6 * D, dur: 40e3, user: 'anna',
      error: "myloader: Access denied for user 'root_restore'@'10.0.0.12' to database 'crm' (1044)", options: { databases: ['crm'], rename: {}, drop_existing: true } },
  ].map((x) => ({
    id: x.id, run_id: x.run.id, run_uuid: x.run.uuid, job_name: x.run.job_name, kind: x.kind, target_server_id: x.target, target_server_name: servers[x.target - 1].name,
    options: { location_id: 0, target_server_id: x.target, databases: [], rename: {}, drop_existing: false, no_binlog: false, fast_mode: false, restore_users: false, threads: 4, strip_definer: false,
      convert_collations: false, skip_gtid_purged: false, target_dir: '', apply: false, service_name: '', datadir: '', move_back: false, use_memory: '', os_user: '', ...x.options },
    status: x.status, started_at: iso(x.started), finished_at: iso(x.started + x.dur), error: x.error || '', user_id: x.user === 'demo' ? 1 : 2, username: x.user,
  }));

  // ---------- restore drills: weekly for the shop, daily for the CRM ----------
  const drills = [
    { id: 1, job_id: 1, name: 'shop weekly drill', target_server_id: 4, schedule: '0 5 * * 0', databases: [], options: { threads: 2, no_binlog: false, max_rto_minutes: 45 },
      assertions: [
        { name: 'orders of the last two days', database: 'shop', sql: 'SELECT COUNT(*) > 0 FROM orders WHERE created_at > NOW() - INTERVAL 2 DAY' },
        { name: 'ledger balances', database: 'payments', sql: 'SELECT SUM(amount) = 0 FROM ledger_entries' },
      ], notify_success: false },
    { id: 2, job_id: 2, name: 'crm daily drill', target_server_id: 4, schedule: '30 4 * * *', databases: ['crm'], options: { threads: 2, no_binlog: false, max_rto_minutes: 0 },
      assertions: [{ name: 'fresh tickets', database: 'crm', sql: 'SELECT MAX(created_at) > NOW() - INTERVAL 1 DAY FROM tickets' }], notify_success: false },
  ].map((d) => ({ ...d, job_name: jobs[d.job_id - 1].name, target_server_name: servers[d.target_server_id - 1].name, enabled: true, notify_failure: true, created_at: iso(now - 30 * D), updated_at: iso(now - 30 * D) }));
  const drillRuns = [];
  const hdur = (ms) => { const s = Math.round(ms / 1000); return s >= 60 ? `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s` : `${s}s`; };
  function addDrillRun(d, start, opt = {}) {
    const job = jobs[d.job_id - 1];
    const run = runs.filter((x) => x.job_id === job.id && x.status === 'success' && x.finished_at < iso(start)).at(-1);
    if (!run) return;
    const id = drillRuns.length + 1;
    const sv = servers[job.server_id - 1];
    const dbs = run.databases.filter((x) => !d.databases.length || d.databases.includes(x.name));
    const restoreMs = Math.round(dbs.reduce((a, x) => a + x.size_bytes, 0) / (job.method === 'mydumper' ? 95 * MiB : 38 * MiB) * 1000 * (0.9 + r() * 0.2));
    const databases = dbs.map((x) => {
      const tables = (sv.dbs.find(([n]) => n === x.name) || [0, 0, 0])[2];
      return { name: x.name, temp: `${x.name}__drill_${id}`, tables, rows: Math.round(x.size_bytes / 210) };
    });
    const checks = [{ kind: 'restore', name: 'Restore', status: 'ok', actual: hdur(restoreMs), message: `${dbs.length} database(s) restored in ${hdur(restoreMs)}` }];
    if (d.options.max_rto_minutes) checks.push({ kind: 'rto', name: 'Restore time (RTO)', status: 'ok', expected: `≤ ${d.options.max_rto_minutes} min`, actual: checks[0].actual });
    for (const m of databases) {
      checks.push({ kind: 'tables', database: m.name, name: 'Tables', status: 'ok', expected: String(m.tables), actual: String(m.tables) });
      checks.push({ kind: 'rows', database: m.name, name: 'Row counts', status: 'ok', expected: m.rows.toLocaleString('en-US'), actual: m.rows.toLocaleString('en-US'), message: `${m.tables} table(s), every row count matches the backup` });
    }
    for (const a of d.assertions) {
      const bad = opt.failAssert === a.name;
      checks.push({ kind: 'sql', database: a.database, name: a.name, status: bad ? 'failed' : 'ok', expected: 'true', actual: bad ? '0' : '1', message: bad ? 'the first column is 0, not true' : 'the first column is true' });
    }
    const passed = checks.filter((c) => c.status === 'ok').length;
    const failed = checks.filter((c) => c.status === 'failed');
    const checkMs = 20e3 + Math.round(r() * 30e3);
    const x = {
      id, drill_id: d.id, drill_name: d.name, job_id: job.id, job_name: job.name, run_id: run.id, run_dir: run.dir_name, target_server_id: d.target_server_id, target_server_name: d.target_server_name,
      status: failed.length ? 'failed' : 'passed', trigger: opt.trigger || 'schedule', triggered_by: opt.by || 'scheduler', started_at: iso(start), finished_at: iso(start + restoreMs + checkMs + 4e3),
      restore_ms: restoreMs, check_ms: checkMs, databases, checks,
      summary: `${databases.length} database(s) · ${databases.reduce((a, m) => a + m.tables, 0)} tables · ${databases.reduce((a, m) => a + m.rows, 0).toLocaleString('en-US')} rows · restored in ${checks[0].actual} · ${passed}/${checks.length} checks passed`,
      error: failed.map((c) => `${c.name} — ${c.message}`).join('\n'), leftover: [],
    };
    drillRuns.push(x);
    run.drill_status = x.status;
    run.drilled_at = x.finished_at;
    run.drill_msg = x.summary;
  }
  for (let dd = 13; dd >= 0; dd--) {
    const dt = new Date(now - dd * D);
    if (dt.getDay() === 0 && day(dd, 5) < now) addDrillRun(drills[0], day(dd, 5));
    if (day(dd, 4, 30) < now) addDrillRun(drills[1], day(dd, 4, 30), dd === 2 ? { failAssert: 'fresh tickets' } : {});
  }
  drillRuns.sort((a, b) => a.started_at.localeCompare(b.started_at)).forEach((x, i) => {
    x.id = i + 1;
    for (const m of x.databases) m.temp = `${m.name}__drill_${x.id}`;
  });

  const channels = [
    { id: 1, name: 'Ops email', type: 'email', enabled: true, on_success: false, on_failure: true, config: { host: 'smtp.example.com', port: '587', security: 'starttls', username: 'alerts@example.com', password: '__SECRET_UNCHANGED__', from: 'DumpCat <alerts@example.com>', to: 'ops@example.com, dba@example.com', insecure: 'false', digest: 'true' } },
    { id: 2, name: '#db-alerts', type: 'slack', enabled: true, on_success: true, on_failure: true, config: { webhook_url: '__SECRET_UNCHANGED__' } },
    { id: 3, name: 'On-call', type: 'telegram', enabled: false, on_success: false, on_failure: true, config: { bot_token: '__SECRET_UNCHANGED__', chat_id: '-1001234567890', thread_id: '' } },
  ].map((c) => ({ ...c, created_at: created, updated_at: created }));

  const actions = [['login', '', ''], ['job_run', 'job:shop-nightly', 'full'], ['restore_start', 'run:shop-nightly', 'to staging-db'], ['storage_test', 'storage:r2-offsite', 'ok'],
    ['job_update', 'job:crm-every-6h', ''], ['agent_create', 'agent:crm-host', ''], ['wireguard_peer_create', 'wireguard:branch-hq', '10.66.0.3'], ['user_update', 'user:auditor', 'role=viewer'],
    ['settings_update', 'settings', ''], ['server_update', 'server:analytics', ''], ['login', '', ''], ['verify', 'run:warehouse-weekly', '']];
  const audit = [];
  for (let i = 0; i < 42; i++) {
    const [action, target, details] = actions[i % actions.length];
    audit.push({ id: 42 - i, ts: iso(now - i * 5.5 * H - r() * H), user_id: i % 3 === 0 ? 1 : 2, username: i % 3 === 0 ? 'demo' : 'anna', action, target, details, ip: i % 3 === 0 ? '192.0.2.10' : '192.0.2.33' });
  }

  return { users, agents, wireguard, servers, storages, jobs, runs, binlogs, restores, drills, drillRuns, channels, audit, tokens: [
    { id: 1, user_id: 1, name: 'monitoring', prefix: 'mv_Mon1tor9', access: 'read', created_at: iso(now - 60 * D), last_used_at: iso(now - 25 * MIN), expires_at: null },
    { id: 2, user_id: 1, name: 'ci-restore-test', prefix: 'mv_C1rEst0r', access: 'full', created_at: iso(now - 14 * D), last_used_at: iso(now - 2 * D), expires_at: iso(now + 76 * D) },
  ], settings: { timezone: 'Asia/Ho_Chi_Minh', max_concurrent: 2, staging_dir: '/var/lib/dumpcat/staging', history_days: 90, base_url: 'https://backup.example.com', tool_paths: {}, session_hours: 12,
    rclone_flags: '--transfers 8', default_threads: 4, require_2fa_admins: true, update_url: '', update_auto_check: true, update_auto_install: true, update_from_hour: 2, update_to_hour: 5,
    freshness_alerts: true, stale_grace_minutes: 60, binlog_lag_minutes: 30, metrics_access: 'token', digest_weekday: 1, digest_hour: 8, metrics_token_prefix: 'dcm_Pr0m3t' } };
}
