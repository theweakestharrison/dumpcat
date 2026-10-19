// The DumpCat UI preview on dumpcat.cc/demo/: a welcome card that says what this is (the interface
// DumpCat gives you on your own server, with sample data), a ribbon that keeps saying it, and the
// download links (which need a real server) replaced by a message.
import { toast } from './ui.js';

let lang = 'en';
try { lang = localStorage.getItem('dumpcat-landing-lang') || (navigator.language || '').slice(0, 2); } catch { /* storage blocked */ }
const vi = lang === 'vi';
const install = vi ? '/vi/install/' : '/install/';
const T = vi ? {
  badge: 'Bản xem trước',
  ribbon: 'Giao diện DumpCat trên server của bạn, với dữ liệu mẫu',
  what: 'Đây là gì?', reset: 'Làm lại', get: 'Cài DumpCat',
  title: 'Đây là giao diện DumpCat sau khi bạn cài',
  lead: 'DumpCat không phải dịch vụ online. Bạn cài nó lên server của mình, và giao diện web này chạy ngay trên server đó: dữ liệu không đi qua ai khác. Bản xem trước này dùng dữ liệu mẫu, chạy hoàn toàn trong trình duyệt và không kết nối tới database nào.',
  steps: ['<b>Cài DumpCat</b> lên server của bạn, Linux hoặc Windows, bằng một lệnh', '<b>Mở</b> <code>http://&lt;ip-server&gt;:6369</code> trên trình duyệt', '<b>Bạn có giao diện này</b>, với database và nơi lưu của chính bạn'],
  you: 'Sau khi cài, bạn có',
  features: [
    ['#/dashboard', 'Dashboard', 'Mọi job, bản backup và cảnh báo trên một màn hình'],
    ['#/jobs', 'Backup jobs', 'Lịch chạy, mysqldump, mydumper hoặc XtraBackup, mã hóa, nhiều nơi lưu'],
    ['#/binlogs', 'Khôi phục theo thời điểm', 'Khôi phục một bản backup, hoặc tới đúng giây trước sự cố'],
    ['#/drills', 'Diễn tập restore', 'Định kỳ chứng minh bản backup restore được thật'],
    ['#/storages', 'Nơi lưu', 'Ổ local, S3, Google Drive, OneDrive, SFTP và hơn 70 nơi khác'],
    ['#/agents', 'Agent và WireGuard', 'Backup cả database nằm sau firewall'],
    ['#/notifications', 'Thông báo và giám sát', 'Email, Slack, Telegram, Discord, báo cáo tuần, Prometheus'],
  ],
  explore: 'Xem giao diện mẫu', installGuide: 'Cách cài DumpCat →',
  download: 'Bản xem trước không có file thật để tải về. Cài DumpCat để tải bản backup của bạn.',
} : {
  badge: 'Preview',
  ribbon: 'The DumpCat interface on your own server, with sample data',
  what: 'What is this?', reset: 'Reset', get: 'Get DumpCat',
  title: 'This is DumpCat after you install it',
  lead: 'DumpCat is not an online service. You install it on your own server, and this web interface runs right there: your data goes through no one else. This preview uses sample data, runs entirely in your browser and connects to no database.',
  steps: ['<b>Install DumpCat</b> on your server, Linux or Windows, with one command', '<b>Open</b> <code>http://&lt;server-ip&gt;:6369</code> in your browser', '<b>You get this interface</b>, with your own databases and storage'],
  you: 'After installing, you get',
  features: [
    ['#/dashboard', 'Dashboard', 'Every job, backup and warning on one screen'],
    ['#/jobs', 'Backup jobs', 'Schedules, mysqldump, mydumper or XtraBackup, encryption, several storages'],
    ['#/binlogs', 'Point-in-time recovery', 'Restore a backup, or to the second before an incident'],
    ['#/drills', 'Restore drills', 'Prove on a schedule that backups really restore'],
    ['#/storages', 'Storage', 'Local disk, S3, Google Drive, OneDrive, SFTP and 70+ more'],
    ['#/agents', 'Agents and WireGuard', 'Back up databases behind a firewall'],
    ['#/notifications', 'Alerts and monitoring', 'Email, Slack, Telegram, Discord, weekly digest, Prometheus'],
  ],
  explore: 'Explore the preview', installGuide: 'How to install DumpCat →',
  download: 'The preview has no real files to download. Install DumpCat to download your backups.',
};

// ---- welcome card ----
const dlg = document.createElement('dialog');
dlg.className = 'demo-intro';
dlg.setAttribute('aria-labelledby', 'demo-intro-title');
dlg.innerHTML = `<div class="di-head"><span class="di-badge">${T.badge}</span><button type="button" class="di-x" aria-label="Close">×</button></div>
  <h2 id="demo-intro-title">${T.title}</h2>
  <p class="di-lead">${T.lead}</p>
  <ol class="di-steps">${T.steps.map((s) => `<li>${s}</li>`).join('')}</ol>
  <p class="di-you">${T.you}</p>
  <ul class="di-features">${T.features.map(([href, name, text]) => `<li><a href="${href}"><b>${name}</b><span>${text}</span></a></li>`).join('')}</ul>
  <div class="di-actions"><button type="button" class="di-go">${T.explore}</button><a href="${install}">${T.installGuide}</a></div>`;
document.body.appendChild(dlg);
const close = () => {
  dlg.close();
  try { sessionStorage.setItem('dumpcat-demo-intro', '1'); } catch { /* storage blocked */ }
};
dlg.querySelector('.di-x').onclick = close;
dlg.querySelector('.di-go').onclick = close;
dlg.querySelectorAll('.di-features a').forEach((a) => a.addEventListener('click', close));
dlg.addEventListener('click', (e) => { if (e.target === dlg) close(); }); // the backdrop
dlg.addEventListener('cancel', () => { try { sessionStorage.setItem('dumpcat-demo-intro', '1'); } catch { /* storage blocked */ } });
// on a phone the card scrolls: start at its title, not at the focused button
const open = () => { if (!dlg.open) dlg.showModal(); dlg.querySelector('.di-go').focus({ preventScroll: true }); dlg.scrollTop = 0; };
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
