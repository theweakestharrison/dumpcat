#!/usr/bin/env python3
"""Generate the comparison pages of dumpcat.cc from site-src/compare/*.json.

Writes (English and Vietnamese, linked with hreflang):
  site/compare/index.html, site/compare/<slug>/index.html
  site/vi/compare/index.html, site/vi/compare/<slug>/index.html
  site/sitemap.xml, site/robots.txt

Run from anywhere: python scripts/build-site.py   (standard library only). See docs/website.vi.md.
"""
import datetime
import html
import json
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent
SRC = ROOT / "site-src" / "compare"
OUT = ROOT / "site"
BASE = "https://dumpcat.cc"
REPO = "https://github.com/theweakestharrison/DumpCat"
LANGS = ("en", "vi")

# Rows of the comparison table: key, label per language, and DumpCat's own value.
DIMENSIONS = [
    ("price", {"en": "Price and license", "vi": "Giá và giấy phép"},
     {"en": "Free and open source (Apache-2.0). No account, no license key.",
      "vi": "Miễn phí, mã nguồn mở (Apache-2.0). Không cần tài khoản hay license key."}),
    ("runs", {"en": "How it runs", "vi": "Cách chạy"},
     {"en": "Self-hosted: one binary with a web UI, running as a systemd or Windows service next to your database.",
      "vi": "Tự host: một file chạy kèm giao diện web, chạy như service systemd hoặc Windows service cạnh database."}),
    ("platforms", {"en": "Platforms", "vi": "Hệ điều hành"},
     {"en": "Linux (amd64, arm64) and Windows (64-bit).", "vi": "Linux (amd64, arm64) và Windows (64-bit)."}),
    ("databases", {"en": "Databases", "vi": "Database"},
     {"en": "MySQL 5.7, 8.0, 8.4, Percona Server, MariaDB 10.x and 11.x.",
      "vi": "MySQL 5.7, 8.0, 8.4, Percona Server, MariaDB 10.x và 11.x."}),
    ("methods", {"en": "Backup methods", "vi": "Phương thức backup"},
     {"en": "Logical with `mysqldump` / `mariadb-dump` or parallel `mydumper`; hot physical backups with XtraBackup / MariaBackup, including incrementals. No locks on InnoDB.",
      "vi": "Logical bằng `mysqldump` / `mariadb-dump` hoặc `mydumper` song song; backup vật lý nóng bằng XtraBackup / MariaBackup, có incremental. Không khóa InnoDB."}),
    ("pitr", {"en": "Point-in-time recovery", "vi": "Khôi phục theo thời điểm"},
     {"en": "Yes. Continuous binlog archiving; restore to a timestamp, a binlog position or just before a GTID.",
      "vi": "Có. Lưu binlog liên tục; khôi phục đến một thời điểm, một vị trí binlog hoặc ngay trước một GTID."}),
    ("storage", {"en": "Storage", "vi": "Nơi lưu trữ"},
     {"en": "Local disk / NFS, S3 and compatibles, Google Drive, OneDrive, SFTP, FTP / FTPS, WebDAV, Backblaze B2, Azure Blob, Dropbox and every rclone backend; several per job.",
      "vi": "Ổ local / NFS, S3 và tương thích, Google Drive, OneDrive, SFTP, FTP / FTPS, WebDAV, Backblaze B2, Azure Blob, Dropbox và mọi backend của rclone; nhiều nơi cho một job."}),
    ("encryption", {"en": "Encryption", "vi": "Mã hóa"},
     {"en": "age encryption on your server before anything is uploaded; zstd compression; SHA-256 for every file.",
      "vi": "Mã hóa age ngay trên server trước khi upload; nén zstd; SHA-256 cho từng file."}),
    ("schedule", {"en": "Scheduling and retention", "vi": "Lịch và retention"},
     {"en": "Cron-style schedules; retention per storage by count, age or GFS (daily / weekly / monthly).",
      "vi": "Lịch kiểu cron; retention riêng cho từng nơi lưu theo số bản, số ngày hoặc GFS (ngày / tuần / tháng)."}),
    ("restore", {"en": "Restore", "vi": "Khôi phục"},
     {"en": "From the web UI: restore under a new database name, test-decrypt every file before dropping anything, integrity checks after each run.",
      "vi": "Ngay trên giao diện web: restore sang tên database mới, thử giải mã mọi file trước khi xóa gì, kiểm tra toàn vẹn sau mỗi lần chạy."}),
    ("datapath", {"en": "Where your data goes", "vi": "Dữ liệu đi đâu"},
     {"en": "Straight from your server to the storage you choose. Nothing passes through a third party.",
      "vi": "Đi thẳng từ server của bạn tới nơi lưu bạn chọn, không qua bên thứ ba nào."}),
    ("alerts", {"en": "Notifications", "vi": "Thông báo"},
     {"en": "Email, Telegram, Slack, Discord and HMAC-signed webhooks.",
      "vi": "Email, Telegram, Slack, Discord và webhook ký HMAC."}),
]

T = {
    "en": {
        "home": "Home", "compare": "Compare", "download": "Download", "github": "GitHub",
        "glance": "At a glance", "choose_them": "Choose {name} if", "choose_us": "Choose DumpCat if",
        "switch": "Switching from {name}", "sources": "Sources",
        "checked": "Facts about {name} were checked on {date} against the sources below. Products change: if something here is out of date, please open an issue on GitHub.",
        "trademark": "{name} is a trademark of its owner. DumpCat is an independent open-source project and is not affiliated with it.",
        "others": "Other comparisons", "cta_download": "Download DumpCat", "cta_github": "Source on GitHub",
        "foot": "Open source under the Apache-2.0 license, with the source on GitHub.",
        "hub_title": "DumpCat vs other MySQL backup tools: open-source, self-hosted alternative",
        "hub_desc": "How DumpCat, a free open-source MySQL and MariaDB backup manager, compares with SqlBackupAndFTP, SimpleBackups, Ottomatik and other backup tools: price, point-in-time recovery, storage and where your data goes.",
        "hub_h1": "DumpCat compared with other MySQL backup tools",
        "hub_lead": "Fair, sourced comparisons with the tools people use to back up MySQL and MariaDB. Every product here is good at something: each page says when the other tool is the better choice.",
        "hub_tool": "Tool", "hub_kind": "Type", "hub_oss": "Open source", "hub_self": "Runs on your servers",
        "hub_pitr": "Point-in-time recovery", "hub_price": "Price",
        "yes": "Yes", "no": "No", "read": "Read the comparison",
        "us_kind": "Self-hosted backup manager with a web UI", "us_pitr": "Yes, binlog archiving", "us_price": "Free",
    },
    "vi": {
        "home": "Trang chủ", "compare": "So sánh", "download": "Tải về", "github": "GitHub",
        "glance": "So sánh nhanh", "choose_them": "Chọn {name} nếu", "choose_us": "Chọn DumpCat nếu",
        "switch": "Chuyển từ {name} sang DumpCat", "sources": "Nguồn",
        "checked": "Thông tin về {name} được kiểm tra ngày {date} theo các nguồn bên dưới. Sản phẩm thay đổi theo thời gian: nếu có gì đã cũ, xin mở một issue trên GitHub.",
        "trademark": "{name} là thương hiệu của chủ sở hữu. DumpCat là dự án mã nguồn mở độc lập, không liên kết với họ.",
        "others": "So sánh khác", "cta_download": "Tải DumpCat", "cta_github": "Mã nguồn trên GitHub",
        "foot": "Mã nguồn mở theo giấy phép Apache-2.0, mã nguồn trên GitHub.",
        "hub_title": "So sánh DumpCat với các công cụ backup MySQL khác: thay thế mã nguồn mở, tự host",
        "hub_desc": "DumpCat, công cụ backup MySQL và MariaDB miễn phí, mã nguồn mở, so với SqlBackupAndFTP, SimpleBackups, Ottomatik và các công cụ khác: giá, khôi phục theo thời điểm, nơi lưu và dữ liệu của bạn đi đâu.",
        "hub_h1": "So sánh DumpCat với các công cụ backup MySQL khác",
        "hub_lead": "So sánh công bằng, có dẫn nguồn, với các công cụ mọi người đang dùng để backup MySQL và MariaDB. Công cụ nào cũng có điểm mạnh: mỗi trang đều nói rõ khi nào công cụ kia hợp hơn.",
        "hub_tool": "Công cụ", "hub_kind": "Loại", "hub_oss": "Mã nguồn mở", "hub_self": "Chạy trên server của bạn",
        "hub_pitr": "Khôi phục theo thời điểm", "hub_price": "Giá",
        "yes": "Có", "no": "Không", "read": "Xem so sánh",
        "us_kind": "Trình quản lý backup tự host, có giao diện web", "us_pitr": "Có, lưu binlog liên tục", "us_price": "Miễn phí",
    },
}

LOGO = ('<svg width="0" height="0" style="position:absolute" aria-hidden="true"><defs><linearGradient id="dc-bg" x1="0" y1="0" x2="1" y2="1">'
        '<stop offset="0" stop-color="#3a86e8"/><stop offset="1" stop-color="#1c56b0"/></linearGradient><symbol id="dc-logo" viewBox="0 0 64 64">'
        '<rect width="64" height="64" rx="15" fill="url(#dc-bg)"/><path d="M47.5 45.5c7.5 0 10.5-5.5 7.8-11.5" fill="none" stroke="#fff" stroke-width="4.2" stroke-linecap="round"/>'
        '<path d="M15.6 25 18.4 7.6 30.5 17.5z" fill="#fff"/><path d="M48.4 25 45.6 7.6 33.5 17.5z" fill="#fff"/><path d="M19.4 18.6 20.4 12.4 25.6 16.4z" fill="#f9a8c9"/>'
        '<path d="M44.6 18.6 43.6 12.4 38.4 16.4z" fill="#f9a8c9"/><path d="M15 21v24c0 3.9 7.6 7 17 7s17-3.1 17-7V21z" fill="#fff"/><ellipse cx="32" cy="21" rx="17" ry="7" fill="#d6e6fb"/>'
        '<path d="M15 38c0 3.9 7.6 7 17 7s17-3.1 17-7" fill="none" stroke="#1c56b0" stroke-opacity=".28" stroke-width="2.4"/><circle cx="25.4" cy="31" r="2.5" fill="#1b2433"/>'
        '<circle cx="38.6" cy="31" r="2.5" fill="#1b2433"/><path d="M30.1 34.2h3.8L32 36.4z" fill="#f27aa8"/></symbol></defs></svg>')

MONTHS_VI = "tháng 1,tháng 2,tháng 3,tháng 4,tháng 5,tháng 6,tháng 7,tháng 8,tháng 9,tháng 10,tháng 11,tháng 12".split(",")
MONTHS_EN = "January February March April May June July August September October November December".split()


def esc(s):
    return html.escape(s, quote=True)


def inline(s):
    """Escape text and turn `code` spans into <code>."""
    parts = esc(s).split("`")
    return "".join(f"<code>{p}</code>" if i % 2 else p for i, p in enumerate(parts))


def fmt_date(iso, lang):
    d = datetime.date.fromisoformat(iso)
    if lang == "vi":
        return f"{d.day} {MONTHS_VI[d.month - 1]}, {d.year}"
    return f"{MONTHS_EN[d.month - 1]} {d.day}, {d.year}"


def path(lang, slug=None):
    p = "/compare/" if lang == "en" else "/vi/compare/"
    return p + (f"{slug}/" if slug else "")


def head(lang, title, desc, slug, crumbs):
    url = BASE + path(lang, slug)
    ld = {"@context": "https://schema.org", "@type": "BreadcrumbList",
          "itemListElement": [{"@type": "ListItem", "position": i + 1, "name": n, "item": BASE + u}
                              for i, (n, u) in enumerate(crumbs)]}
    return f"""<!doctype html>
<html lang="{lang}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>{esc(title)}</title>
<meta name="description" content="{esc(desc)}">
<link rel="canonical" href="{url}">
<link rel="alternate" hreflang="en" href="{BASE + path('en', slug)}">
<link rel="alternate" hreflang="vi" href="{BASE + path('vi', slug)}">
<link rel="alternate" hreflang="x-default" href="{BASE + path('en', slug)}">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<meta property="og:type" content="article">
<meta property="og:url" content="{url}">
<meta property="og:site_name" content="DumpCat">
<meta property="og:title" content="{esc(title)}">
<meta property="og:description" content="{esc(desc)}">
<meta property="og:locale" content="{'en_US' if lang == 'en' else 'vi_VN'}">
<meta property="og:image" content="{BASE}/og.png">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:image" content="{BASE}/og.png">
<meta name="theme-color" content="#1f5fbf">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Be+Vietnam+Pro:wght@400;500;600;700&family=Bricolage+Grotesque:opsz,wdth,wght@12..96,75..100,400..800&family=JetBrains+Mono:wght@400;600&display=swap">
<link rel="stylesheet" href="/style.css">
<script type="application/ld+json">{json.dumps(ld, ensure_ascii=False)}</script>
</head>
<body>
{LOGO}
"""


def header(lang, slug, crumbs, h1, lead):
    t = T[lang]
    other = "vi" if lang == "en" else "en"
    lang_links = "".join(
        f'<a href="{path(l, slug)}" hreflang="{l}" lang="{l}"{" aria-current=\"true\"" if l == lang else ""}>{l.upper()}</a>'
        for l in LANGS)
    crumb_html = " <span aria-hidden=\"true\">/</span> ".join(
        f'<a href="{u}">{esc(n)}</a>' if i < len(crumbs) - 1 else f'<span aria-current="page">{esc(n)}</span>'
        for i, (n, u) in enumerate(crumbs))
    return f"""<header class="hero page-hero">
  <div class="wrap">
    <div class="topbar">
      <a class="brand" href="/"><svg aria-hidden="true"><use href="#dc-logo"/></svg>DumpCat</a>
      <nav class="nav" aria-label="Site">
        <a href="{path(lang)}">{t['compare']}</a>
        <a href="/#download">{t['download']}</a>
        <a href="{REPO}">{t['github']}</a>
      </nav>
      <div class="lang" role="group" aria-label="Language">{lang_links}</div>
    </div>
    <div class="page-head">
      <nav class="crumbs" aria-label="Breadcrumb">{crumb_html}</nav>
      <h1>{inline(h1)}</h1>
      <p class="lead">{inline(lead)}</p>
      <div class="cta">
        <a class="btn btn-light" href="/#download">{t['cta_download']}</a>
        <a class="btn btn-ghost" href="{REPO}">{t['cta_github']}</a>
      </div>
    </div>
  </div>
</header>
"""


def footer(lang):
    t = T[lang]
    return f"""<footer class="foot">
  <div class="wrap">
    <a class="brand" href="/"><svg aria-hidden="true"><use href="#dc-logo"/></svg>DumpCat</a>
    <p>{esc(t['foot'])} <a href="{REPO}">GitHub</a> · <a href="{path(lang)}">{t['compare']}</a></p>
  </div>
</footer>
</body>
</html>
"""


def tool_page(tool, tools, lang):
    t = T[lang]
    d = tool[lang]
    name = tool["name"]
    crumbs = [(t["home"], "/"), (t["compare"], path(lang)), (f"DumpCat vs {name}", path(lang, tool["slug"]))]
    rows = "".join(
        f'<tr><th scope="row">{esc(label[lang])}</th><td class="us" data-m="DumpCat">{inline(ours[lang])}</td>'
        f'<td data-m="{esc(name)}">{inline(d["values"][key])}</td></tr>'
        for key, label, ours in DIMENSIONS)
    li = lambda items: "".join(f"<li>{inline(x)}</li>" for x in items)
    switch = ""
    if d.get("switch"):
        switch = f"""
  <section class="section" aria-labelledby="switch-title">
    <div class="wrap narrow">
      <h2 id="switch-title">{esc(t['switch'].format(name=name))}</h2>
      <ol class="steps-list">{li(d['switch'])}</ol>
    </div>
  </section>"""
    sources = "".join(f'<li><a href="{esc(s["url"])}" rel="nofollow noopener">{esc(s["label"])}</a></li>' for s in tool["sources"])
    others = "".join(
        f'<li><a href="{path(lang, o["slug"])}">DumpCat vs {esc(o["name"])}</a><span>{inline(o["summary"][lang])}</span></li>'
        for o in tools if o["slug"] != tool["slug"])
    return head(lang, d["title"], d["description"], tool["slug"], crumbs) + header(lang, tool["slug"], crumbs, f"DumpCat vs {name}", d["lead"]) + f"""
<main>
  <section class="section" aria-labelledby="glance-title">
    <div class="wrap">
      <h2 id="glance-title">{t['glance']}</h2>
      <div class="section-body table-scroll cmp">
        <table>
          <thead><tr><th scope="col"></th><th scope="col" class="us">DumpCat</th><th scope="col">{esc(name)}</th></tr></thead>
          <tbody>{rows}</tbody>
        </table>
      </div>
    </div>
  </section>

  <section class="section" aria-label="{esc(t['choose_them'].format(name=name))} / {esc(t['choose_us'])}">
    <div class="wrap choose">
      <div class="choose-card">
        <h2>{esc(t['choose_them'].format(name=name))}</h2>
        <ul>{li(d['choose_them'])}</ul>
      </div>
      <div class="choose-card us">
        <h2>{esc(t['choose_us'])}</h2>
        <ul>{li(d['choose_us'])}</ul>
      </div>
    </div>
  </section>
{switch}
  <section class="section" aria-labelledby="sources-title">
    <div class="wrap narrow">
      <h2 id="sources-title">{t['sources']}</h2>
      <p class="note">{esc(t['checked'].format(name=name, date=fmt_date(tool['checked'], lang)))}</p>
      <ol class="sources">{sources}</ol>
      <p class="note">{esc(t['trademark'].format(name=name))}</p>
    </div>
  </section>

  <section class="section" aria-labelledby="others-title">
    <div class="wrap narrow">
      <h2 id="others-title">{t['others']}</h2>
      <ul class="related">{others}</ul>
    </div>
  </section>
</main>
""" + footer(lang)


def hub_page(tools, lang):
    t = T[lang]
    crumbs = [(t["home"], "/"), (t["compare"], path(lang))]
    yn = lambda b: t["yes"] if b else t["no"]
    rows = (f'<tr class="fit"><th scope="row">DumpCat</th><td data-m="{esc(t["hub_kind"])}">{esc(t["us_kind"])}</td>'
            f'<td class="yes" data-m="{esc(t["hub_oss"])}">{t["yes"]}</td><td class="yes" data-m="{esc(t["hub_self"])}">{t["yes"]}</td>'
            f'<td class="yes" data-m="{esc(t["hub_pitr"])}">{esc(t["us_pitr"])}</td><td data-m="{esc(t["hub_price"])}">{esc(t["us_price"])}</td></tr>')
    for o in tools:
        h = o["hub"]
        rows += (f'<tr><th scope="row"><a href="{path(lang, o["slug"])}">{esc(o["name"])}</a></th>'
                 f'<td data-m="{esc(t["hub_kind"])}">{inline(o["kind"][lang])}</td>'
                 f'<td class="{"yes" if h["open_source"] else "no"}" data-m="{esc(t["hub_oss"])}">{yn(h["open_source"])}</td>'
                 f'<td data-m="{esc(t["hub_self"])}">{inline(h["self_hosted"][lang])}</td>'
                 f'<td data-m="{esc(t["hub_pitr"])}">{inline(h["pitr"][lang])}</td>'
                 f'<td data-m="{esc(t["hub_price"])}">{inline(h["price"][lang])}</td></tr>')
    cards = "".join(
        f'<li><a href="{path(lang, o["slug"])}"><b>DumpCat vs {esc(o["name"])}</b><span>{inline(o["summary"][lang])}</span>'
        f'<em>{t["read"]} →</em></a></li>' for o in tools)
    return head(lang, t["hub_title"], t["hub_desc"], None, crumbs) + header(lang, None, crumbs, t["hub_h1"], t["hub_lead"]) + f"""
<main>
  <section class="section">
    <div class="wrap">
      <div class="table-scroll cmp hub">
        <table>
          <thead><tr><th scope="col">{t['hub_tool']}</th><th scope="col">{t['hub_kind']}</th><th scope="col">{t['hub_oss']}</th><th scope="col">{t['hub_self']}</th><th scope="col">{t['hub_pitr']}</th><th scope="col">{t['hub_price']}</th></tr></thead>
          <tbody>{rows}</tbody>
        </table>
      </div>
      <ul class="cards">{cards}</ul>
    </div>
  </section>
</main>
""" + footer(lang)


def write(rel, text):
    f = OUT / rel
    f.parent.mkdir(parents=True, exist_ok=True)
    f.write_text(text, encoding="utf-8", newline="\n")


def main():
    tools = [json.loads(p.read_text(encoding="utf-8")) for p in sorted(SRC.glob("*.json"))]
    tools.sort(key=lambda o: o.get("order", 100))
    for o in tools:
        for lang in LANGS:
            missing = [k for k, _, _ in DIMENSIONS if k not in o[lang]["values"]]
            if missing:
                raise SystemExit(f"{o['slug']}: {lang} values missing {missing}")
    for lang in LANGS:
        write(path(lang).lstrip("/") + "index.html", hub_page(tools, lang))
        for o in tools:
            write(path(lang, o["slug"]).lstrip("/") + "index.html", tool_page(o, tools, lang))

    newest = max([o["checked"] for o in tools] + [datetime.date.today().isoformat()])
    urls = [("/", None, newest)] + [(None, None, newest)] + [(None, o["slug"], o["checked"]) for o in tools]
    items = []
    for fixed, slug, mod in urls:
        if fixed:
            items.append(f"  <url><loc>{BASE}{fixed}</loc><lastmod>{mod}</lastmod></url>")
            continue
        alts = "".join(f'<xhtml:link rel="alternate" hreflang="{l}" href="{BASE + path(l, slug)}"/>' for l in LANGS)
        for lang in LANGS:
            items.append(f"  <url><loc>{BASE + path(lang, slug)}</loc><lastmod>{mod}</lastmod>{alts}</url>")
    write("sitemap.xml", '<?xml version="1.0" encoding="UTF-8"?>\n'
          '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">\n'
          + "\n".join(items) + "\n</urlset>\n")
    write("robots.txt", f"User-agent: *\nAllow: /\n\nSitemap: {BASE}/sitemap.xml\n")
    print(f"built {len(tools)} comparisons x {len(LANGS)} languages, sitemap with {len(items)} URLs")


if __name__ == "__main__":
    main()
