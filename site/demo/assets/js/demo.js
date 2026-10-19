// The DumpCat demo on dumpcat.cc: a ribbon that says it is a demo, and the
// download links (which need a real server) replaced by a message.
import { toast } from './ui.js';

let lang = 'en';
try { lang = localStorage.getItem('dumpcat-landing-lang') || (navigator.language || '').slice(0, 2); } catch { /* storage blocked */ }
const vi = lang === 'vi';
const T = vi
  ? { demo: 'Bản demo', text: 'Dữ liệu mẫu chạy trong trình duyệt, không lưu gì', reset: 'Làm lại', get: 'Cài DumpCat', download: 'Bản demo không có file thật để tải về — cài DumpCat để tải backup của bạn.' }
  : { demo: 'Demo', text: 'Sample data in your browser — nothing is saved', reset: 'Reset', get: 'Get DumpCat', download: 'The demo has no real files to download — install DumpCat to download your backups.' };

const bar = document.createElement('div');
bar.className = 'demo-ribbon';
bar.setAttribute('role', 'note');
bar.innerHTML = `<span class="demo-dot" aria-hidden="true"></span><b>${T.demo}</b><span class="demo-text">${T.text}</span>`
  + `<button type="button" class="demo-btn">${T.reset}</button><a class="demo-btn primary" href="/#install">${T.get}</a>`;
bar.querySelector('button').onclick = () => location.reload();
document.body.appendChild(bar);

document.addEventListener('click', (e) => {
  const a = e.target.closest && e.target.closest('a[href^="/api/"]');
  if (!a) return;
  e.preventDefault();
  toast(T.download, 'warn', 6000);
}, true);
