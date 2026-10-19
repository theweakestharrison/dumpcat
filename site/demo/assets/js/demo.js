// The DumpCat UI preview on dumpcat.cc/demo/: a short card that says what this is (DumpCat is not an
// online service; this is sample data showing its features), closed only with its button; a ribbon that
// keeps saying it; and the download links (which need a real server) replaced by a message.
import { toast } from './ui.js';

let lang = 'en';
try { lang = localStorage.getItem('dumpcat-landing-lang') || (navigator.language || '').slice(0, 2); } catch { /* storage blocked */ }
const vi = lang === 'vi';
const install = vi ? '/vi/install/' : '/install/';
const T = vi ? {
  badge: 'Bản xem trước',
  ribbon: 'Dữ liệu mẫu, không phải dịch vụ online',
  what: 'Đây là gì?', reset: 'Làm lại', get: 'Cài DumpCat',
  title: 'DumpCat không phải dịch vụ online',
  points: [
    ['lock', 'Bạn cài DumpCat lên server của mình. <b>Dữ liệu không đi qua đâu cả.</b>'],
    ['eye', 'Đây là <b>dữ liệu mẫu</b>, để bạn xem DumpCat có những tính năng gì.'],
  ],
  features: ['Dashboard', 'Backup jobs', 'Khôi phục theo thời điểm', 'Diễn tập restore', 'S3, Google Drive, SFTP…', 'Agent và WireGuard', 'Cảnh báo và giám sát'],
  explore: 'Xem các tính năng',
  download: 'Bản xem trước không có file thật để tải về. Cài DumpCat để tải bản backup của bạn.',
} : {
  badge: 'Preview',
  ribbon: 'Sample data, not an online service',
  what: 'What is this?', reset: 'Reset', get: 'Get DumpCat',
  title: 'DumpCat is not an online service',
  points: [
    ['lock', 'You install DumpCat on your own server. <b>Your data goes nowhere else.</b>'],
    ['eye', 'This is <b>sample data</b>, to show you what DumpCat can do.'],
  ],
  features: ['Dashboard', 'Backup jobs', 'Point-in-time recovery', 'Restore drills', 'S3, Google Drive, SFTP…', 'Agents and WireGuard', 'Alerts and monitoring'],
  explore: 'Explore the features',
  download: 'The preview has no real files to download. Install DumpCat to download your backups.',
};
const ICON = {
  lock: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/></svg>',
  eye: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/></svg>',
};

// ---- the card: only its button closes it ----
const dlg = document.createElement('dialog');
dlg.className = 'demo-intro';
dlg.setAttribute('aria-labelledby', 'demo-intro-title');
dlg.innerHTML = `<span class="di-badge">${T.badge}</span>
  <h2 id="demo-intro-title">${T.title}</h2>
  <ul class="di-points">${T.points.map(([i, s]) => `<li>${ICON[i]}<span>${s}</span></li>`).join('')}</ul>
  <ul class="di-tags">${T.features.map((f) => `<li>${f}</li>`).join('')}</ul>
  <button type="button" class="di-go">${T.explore}</button>`;
document.body.appendChild(dlg);
let leaving = false;
const done = () => {
  leaving = true;
  dlg.close();
  try { sessionStorage.setItem('dumpcat-demo-intro', '1'); } catch { /* storage blocked */ }
};
dlg.querySelector('.di-go').onclick = done;
dlg.addEventListener('cancel', (e) => e.preventDefault()); // Esc
dlg.addEventListener('close', () => { if (!leaving) dlg.showModal(); }); // a browser that closes it anyway
const open = () => { leaving = false; if (!dlg.open) dlg.showModal(); dlg.querySelector('.di-go').focus({ preventScroll: true }); dlg.scrollTop = 0; };
let seen = false;
try { seen = sessionStorage.getItem('dumpcat-demo-intro') === '1'; } catch { /* storage blocked */ }
if (!seen) open();

// ---- ribbon ----
const bar = document.createElement('div');
bar.className = 'demo-ribbon';
bar.setAttribute('role', 'note');
bar.innerHTML = `<span class="demo-dot" aria-hidden="true"></span><b>${T.badge}</b><span class="demo-text">${T.ribbon}</span>`
  + `<button type="button" class="demo-btn" data-act="what">${T.what}</button>`
  + `<button type="button" class="demo-btn" data-act="reset">${T.reset}</button><a class="demo-btn primary" href="${install}">${T.get}</a>`;
bar.querySelector('[data-act="what"]').onclick = open;
bar.querySelector('[data-act="reset"]').onclick = () => location.reload();
document.body.appendChild(bar);

document.addEventListener('click', (e) => {
  const a = e.target.closest && e.target.closest('a[href^="/api/"]');
  if (!a) return;
  e.preventDefault();
  toast(T.download, 'warn', 6000);
}, true);
