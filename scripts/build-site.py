#!/usr/bin/env python3
"""Generate the pages of dumpcat.cc.

Writes (English and Vietnamese, linked with hreflang; the landing page also in the LANDING_LANGS below):
  site/index.html, site/<lang>/index.html   (the landing page, from site-src/landing/: index.html in Vietnamese,
                                             en.html in English, <lang>.html for the other languages)
  site/compare/index.html, site/compare/<slug>/index.html   (the comparisons, from site-src/compare/*.json)
  site/vi/compare/index.html, site/vi/compare/<slug>/index.html
  site/install/index.html, site/vi/install/index.html   (the step-by-step install guide, from site-src/install/)
  site/changelog/index.html, site/changelog/<version>/index.html   (What's new, from CHANGELOG.md, English)
  site/404.html, site/sitemap.xml, site/robots.txt

Run from anywhere: python scripts/build-site.py   (standard library only). See docs/website.vi.md.
"""
import datetime
import html
import json
import pathlib
import re
import subprocess
import textwrap

ROOT = pathlib.Path(__file__).resolve().parent.parent
SRC = ROOT / "site-src" / "compare"
GUIDE = ROOT / "site-src" / "install"
LANDING = ROOT / "site-src" / "landing"
OUT = ROOT / "site"
BASE = "https://dumpcat.cc"
REPO = "https://github.com/theweakestharrison/DumpCat"
SUPPORT = "fat@dumpcat.cc"
GH = '<svg class="gh" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 0c4.42 0 8 3.58 8 8a8.013 8.013 0 0 1-5.45 7.59c-.4.08-.55-.17-.55-.38 0-.27.01-1.13.01-2.2 0-.75-.25-1.23-.54-1.48 1.78-.2 3.65-.88 3.65-3.95 0-.88-.31-1.59-.82-2.15.08-.2.36-1.02-.08-2.12 0 0-.67-.22-2.2.82-.64-.18-1.32-.27-2-.27-.68 0-1.36.09-2 .27-1.53-1.03-2.2-.82-2.2-.82-.44 1.1-.16 1.92-.08 2.12-.51.56-.82 1.28-.82 2.15 0 3.06 1.86 3.75 3.64 3.95-.23.2-.44.55-.51 1.07-.46.21-1.61.55-2.33-.66-.15-.24-.6-.83-1.23-.82-.67.01-.27.38.01.53.34.19.73.9.82 1.13.16.45.68 1.31 2.69.94 0 .67.01 1.3.01 1.49 0 .21-.15.45-.55.38A7.995 7.995 0 0 1 0 8c0-4.42 3.58-8 8-8Z"/></svg>'
LANGS = ("en", "vi")
# The landing page has more languages than the comparisons and the install guide: code (its path, /<code>/) →
# (hreflang and html lang, the language's own name, og:locale). Its text: site-src/landing/<code>.html.
LANDING_LANGS = {
    "en": ("en", "English", "en_US"),
    "vi": ("vi", "Tiếng Việt", "vi_VN"),
    "zh": ("zh-Hans", "简体中文", "zh_CN"),
    "ja": ("ja", "日本語", "ja_JP"),
    "ko": ("ko", "한국어", "ko_KR"),
    "es": ("es", "Español", "es_ES"),
    "pt": ("pt-BR", "Português", "pt_BR"),
    "de": ("de", "Deutsch", "de_DE"),
    "fr": ("fr", "Français", "fr_FR"),
}

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
     {"en": "MySQL 5.7, 8.0, 8.4, Percona Server, MariaDB 10.x and 11.x; PostgreSQL 10 and newer; Microsoft SQL Server 2012 and newer (Windows, Linux, containers).",
      "vi": "MySQL 5.7, 8.0, 8.4, Percona Server, MariaDB 10.x và 11.x; PostgreSQL 10 trở lên; Microsoft SQL Server 2012 trở lên (Windows, Linux, container)."}),
    ("methods", {"en": "Backup methods", "vi": "Phương thức backup"},
     {"en": "Logical with `mysqldump` / `mariadb-dump` or parallel `mydumper`; hot physical backups with XtraBackup / MariaBackup, including incrementals. No locks on InnoDB. PostgreSQL: `pg_dump` (tables in parallel) and `pg_basebackup` (the whole cluster). SQL Server: native full and differential backups (`BACKUP DATABASE`, no client tools).",
      "vi": "Logical bằng `mysqldump` / `mariadb-dump` hoặc `mydumper` song song; backup vật lý nóng bằng XtraBackup / MariaBackup, có incremental. Không khóa InnoDB. PostgreSQL: `pg_dump` (các bảng song song) và `pg_basebackup` (cả cluster). SQL Server: backup full và differential gốc (`BACKUP DATABASE`, không cần client)."}),
    ("pitr", {"en": "Point-in-time recovery", "vi": "Khôi phục theo thời điểm"},
     {"en": "Yes. Continuous binlog archiving; restore to a timestamp, a binlog position or just before a GTID. PostgreSQL: continuous WAL archiving; restore to a timestamp, a restore point or a WAL position. SQL Server: scheduled log backups; restore to a timestamp, a marked transaction or an LSN.",
      "vi": "Có. Lưu binlog liên tục; khôi phục đến một thời điểm, một vị trí binlog hoặc ngay trước một GTID. PostgreSQL: lưu WAL liên tục; khôi phục đến một thời điểm, một restore point hoặc một vị trí WAL. SQL Server: log backup theo lịch; khôi phục đến một thời điểm, một marked transaction hoặc một LSN."}),
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

# Side-by-side grid: one verdict per tool and row, in site-src/compare/<slug>.json under "grid":
#   "<row>": ["yes" | "paid" | "partial" | "no" | "unknown", {"en": "short note", "vi": "..."}]   (note optional)
# Groups of rows, each row: key, label per language.
GRID = [
    ({"en": "Cost and platform", "vi": "Chi phí và nền tảng"}, [
        ("open_source", {"en": "Open source", "vi": "Mã nguồn mở"}),
        ("free", {"en": "Every feature free", "vi": "Mọi tính năng miễn phí"}),
        ("self_hosted", {"en": "Runs on your own servers", "vi": "Chạy trên server của bạn"}),
        ("linux", {"en": "Linux", "vi": "Linux"}),
        ("windows", {"en": "Windows", "vi": "Windows"}),
        ("web_ui", {"en": "Web interface", "vi": "Giao diện web"}),
    ]),
    ({"en": "Backup", "vi": "Backup"}, [
        ("mariadb", {"en": "MariaDB", "vi": "MariaDB"}),
        ("logical", {"en": "Logical dumps (mysqldump)", "vi": "Dump logical (mysqldump)"}),
        ("parallel", {"en": "Parallel dumps (mydumper)", "vi": "Dump song song (mydumper)"}),
        ("physical", {"en": "Hot physical backups", "vi": "Backup vật lý nóng"}),
        ("incremental", {"en": "Incremental backups", "vi": "Backup incremental"}),
    ]),
    ({"en": "Recovery", "vi": "Khôi phục"}, [
        ("pitr", {"en": "Point-in-time recovery", "vi": "Khôi phục theo thời điểm"}),
        ("restore_ui", {"en": "Restore from the tool", "vi": "Restore ngay trong công cụ"}),
        ("verify", {"en": "Automatic backup check", "vi": "Tự kiểm tra bản backup"}),
    ]),
    ({"en": "Storage and security", "vi": "Lưu trữ và bảo mật"}, [
        ("cloud", {"en": "Cloud storage (S3 and more)", "vi": "Lưu lên cloud (S3 và khác)"}),
        ("multi_storage", {"en": "Several copies per backup", "vi": "Nhiều bản sao cho một backup"}),
        ("encryption", {"en": "Encryption", "vi": "Mã hóa"}),
        ("direct", {"en": "Data skips the vendor", "vi": "Dữ liệu không qua hãng"}),
    ]),
    ({"en": "Operations", "vi": "Vận hành"}, [
        ("scheduler", {"en": "Built-in scheduler", "vi": "Lập lịch có sẵn"}),
        ("retention", {"en": "Automatic retention", "vi": "Tự dọn bản cũ"}),
        ("alerts", {"en": "Chat and webhook alerts", "vi": "Báo qua chat và webhook"}),
    ]),
]
GRID_KEYS = [k for _, rows in GRID for k, _ in rows]
# rows of the short comparison on the landing page
LANDING_ROWS = ["free", "self_hosted", "linux", "physical", "pitr", "encryption", "direct", "verify"]

US_GRID = {
    "open_source": ["yes", {"en": "Apache-2.0", "vi": "Apache-2.0"}],
    "free": ["yes", {"en": "No paid tier", "vi": "Không có gói trả phí"}],
    "self_hosted": ["yes", {"en": "One binary", "vi": "Một file chạy"}],
    "linux": ["yes", {"en": "amd64, arm64", "vi": "amd64, arm64"}],
    "windows": ["yes", {"en": "No mydumper there", "vi": "Không có mydumper"}],
    "web_ui": ["yes"],
    "mariadb": ["yes", {"en": "10.x and 11.x", "vi": "10.x và 11.x"}],
    "logical": ["yes", {"en": "mysqldump, mariadb-dump", "vi": "mysqldump, mariadb-dump"}],
    "parallel": ["yes", {"en": "mydumper, on Linux", "vi": "mydumper, trên Linux"}],
    "physical": ["yes", {"en": "XtraBackup, MariaBackup", "vi": "XtraBackup, MariaBackup"}],
    "incremental": ["yes", {"en": "Physical and binlogs", "vi": "Vật lý và binlog"}],
    "pitr": ["yes", {"en": "To the second or a GTID", "vi": "Tới từng giây hoặc GTID"}],
    "restore_ui": ["yes", {"en": "Also under a new name", "vi": "Cả sang tên mới"}],
    "verify": ["yes", {"en": "Every run, plus drills", "vi": "Mỗi lần chạy, kèm diễn tập"}],
    "cloud": ["yes", {"en": "S3, Drive, OneDrive, 70+", "vi": "S3, Drive, OneDrive, 70+"}],
    "multi_storage": ["yes"],
    "encryption": ["yes", {"en": "age, before upload", "vi": "age, trước khi upload"}],
    "direct": ["yes", {"en": "Server to your storage", "vi": "Từ server tới storage"}],
    "scheduler": ["yes", {"en": "Cron", "vi": "Cron"}],
    "retention": ["yes", {"en": "Count, age or GFS", "vi": "Số bản, số ngày, GFS"}],
    "alerts": ["yes", {"en": "Slack, Telegram, Discord…", "vi": "Slack, Telegram, Discord…"}],
}

VERDICT = {
    "yes": {"en": "Yes", "vi": "Có"},
    "paid": {"en": "Paid plan", "vi": "Gói trả phí"},
    "partial": {"en": "Partly", "vi": "Một phần"},
    "no": {"en": "No", "vi": "Không"},
    "unknown": {"en": "Not documented", "vi": "Không có thông tin"},
}
MARK = {
    "yes": '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5.5 12.5l4 4L18.5 7.5"/></svg>',
    "no": '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7.5 7.5l9 9M16.5 7.5l-9 9"/></svg>',
    "partial": '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 12h10"/></svg>',
    "paid": '<b aria-hidden="true">$</b>',
    "unknown": '<b aria-hidden="true">?</b>',
}


def mark(cell, lang, note=True):
    v = cell[0]
    label = VERDICT[v][lang]
    n = cell[1][lang] if len(cell) > 1 and cell[1] else ""
    title = f"{label}: {n}" if n else label
    out = f'<span class="mk mk-{v}" title="{esc(title)}">{MARK[v]}<span class="sr">{esc(label)}</span></span>'
    if note and n:
        out += f'<span class="vs-note">{esc(n)}</span>'
    elif note and v != "yes":
        out += f'<span class="vs-note">{esc(label)}</span>'
    return out


def legend(lang):
    return '<ul class="vs-legend">' + "".join(
        f'<li><span class="mk mk-{v}">{MARK[v]}</span>{esc(VERDICT[v][lang])}</li>' for v in VERDICT) + "</ul>"


def vs_table(tool, lang):
    """DumpCat and one tool, side by side, row by row."""
    name = esc(tool["name"])
    body = ""
    for group, rows in GRID:
        body += f'<tr class="vs-group"><th colspan="3" scope="colgroup">{esc(group[lang])}</th></tr>'
        for key, label in rows:
            body += (f'<tr><th scope="row">{esc(label[lang])}</th>'
                     f'<td class="us">{mark(US_GRID[key], lang)}</td><td>{mark(tool["grid"][key], lang)}</td></tr>')
    return (f'<div class="vs"><table><thead><tr><th scope="col"><span class="sr">{"Feature" if lang == "en" else "Tính năng"}</span></th>'
            f'<th scope="col" class="us"><span class="vs-name"><svg aria-hidden="true"><use href="#dc-logo"/></svg>DumpCat</span></th>'
            f'<th scope="col"><span class="vs-name">{wbr(tool["name"])}</span></th></tr></thead><tbody>{body}</tbody></table></div>')


def wbr(name):
    """Let long product names break between words: SQL<wbr>Backup<wbr>And<wbr>FTP."""
    return re.sub(r"(?<=[a-z])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])", "<wbr>", esc(name)).replace("My<wbr>SQL", "MySQL")


def hub_matrix(tools, lang):
    """Every tool side by side: rows are features, columns are tools."""
    head = (f'<th scope="col" class="corner"><span class="sr">{"Feature" if lang == "en" else "Tính năng"}</span></th>'
            f'<th scope="col" class="us"><span>DumpCat</span></th>'
            + "".join(f'<th scope="col"><a href="{path(lang, o["slug"])}">{wbr(o["name"])}</a></th>' for o in tools))
    body = ""
    for group, rows in GRID:
        body += f'<tr class="vs-group"><th colspan="{len(tools) + 2}" scope="colgroup"><span>{esc(group[lang])}</span></th></tr>'
        for key, label in rows:
            body += (f'<tr><th scope="row">{esc(label[lang])}</th><td class="us">{mark(US_GRID[key], lang, note=False)}</td>'
                     + "".join(f'<td>{mark(o["grid"][key], lang, note=False)}</td>' for o in tools) + "</tr>")
    return f'<div class="matrix"><table><thead><tr>{head}</tr></thead><tbody>{body}</tbody></table></div>'


def landing_block(tools, lang):
    """The short side-by-side comparison of the landing page: one tool at a time next to DumpCat (the chips switch it, app.js)."""
    labels = {k: l for _, rows in GRID for k, l in rows}
    first = tools[0]["slug"]
    chips = "".join(
        f'<button type="button" class="lc-chip" data-tool="{o["slug"]}" aria-pressed="{"true" if o["slug"] == first else "false"}">{esc(o["name"])}</button>'
        for o in tools)
    heads = "".join(f'<span class="vs-name" data-tool="{o["slug"]}"{"" if o["slug"] == first else " hidden"}>{wbr(o["name"])}</span>' for o in tools)
    rows = ""
    for key in LANDING_ROWS:
        cells = "".join(f'<span class="lc-cell" data-tool="{o["slug"]}"{"" if o["slug"] == first else " hidden"}>{mark(o["grid"][key], lang)}</span>'
                        for o in tools)
        rows += f'<tr><th scope="row">{esc(labels[key][lang])}</th><td class="us">{mark(US_GRID[key], lang)}</td><td>{cells}</td></tr>'
    links = ""
    for o in tools:
        text = ("Full comparison: DumpCat vs " if lang == "en" else "So sánh đầy đủ DumpCat với ") + o["name"]
        shown = "" if o["slug"] == first else " hidden"
        links += f'<a class="lc-more" data-tool="{o["slug"]}"{shown} href="{path(lang, o["slug"])}">{esc(text)} →</a>'
    all_link = "All comparisons" if lang == "en" else "Tất cả so sánh"
    return (f'<div class="lc">'
            f'<div class="lc-chips" role="group" aria-label="{"Compare with" if lang == "en" else "So sánh với"}">{chips}</div>'
            f'<div class="vs"><table><thead><tr><th scope="col"></th>'
            f'<th scope="col" class="us"><span class="vs-name"><svg aria-hidden="true"><use href="#dc-logo"/></svg>DumpCat</span></th>'
            f'<th scope="col">{heads}</th></tr></thead><tbody>{rows}</tbody></table></div>'
            f'<div class="lc-foot"><div class="lc-links">{links}<a href="{path(lang)}">{all_link} →</a></div>'
            f'{legend(lang)}</div>'
            f'</div>')

T = {
    "en": {
        "home": "Home", "compare": "Compare", "download": "Download", "github": "GitHub",
        "glance": "In detail", "vs": "Side by side", "vs_lead": "Each row says whether the feature exists; hover or tap a mark for the details. The table below has the full answers, with sources.", "choose_them": "Choose {name} if", "choose_us": "Choose DumpCat if",
        "switch": "Switching from {name}", "sources": "Sources",
        "checked": "Facts about {name} were checked on {date} against the sources below. Products change: if something here is out of date, please open an issue on GitHub.",
        "trademark": "{name} is a trademark of its owner. DumpCat is an independent open-source project and is not affiliated with it.",
        "others": "Other comparisons", "cta_download": "Download DumpCat", "cta_github": "Source on GitHub",
        "foot": "Open source under the Apache-2.0 license, with the source on GitHub.", "support": "Support",
        "install": "Install", "whatsnew": "What's new", "features": "Features", "preview": "Preview",
        "guide_title": "Install DumpCat step by step: Linux and Windows",
        "guide_desc": "How to install DumpCat, the free MySQL, MariaDB, PostgreSQL and SQL Server backup manager: one command on a Linux server, download and double-click on Windows, then your first backup. Written for beginners.",
        "guide_h1": "Install DumpCat step by step",
        "guide_lead": "For beginners: no Linux or command-line experience needed. About five minutes from download to your first backup.",
        "guide_linux": "Linux server", "guide_windows": "Windows",
        "hub_title": "DumpCat vs other MySQL backup tools: open-source, self-hosted alternative",
        "hub_desc": "How DumpCat, a free open-source MySQL, MariaDB, PostgreSQL and SQL Server backup manager, compares with SqlBackupAndFTP, SimpleBackups, Ottomatik and other backup tools: price, point-in-time recovery, storage and where your data goes.",
        "hub_h1": "DumpCat compared with other MySQL backup tools",
        "hub_lead": "Fair, sourced comparisons with the tools people use to back up MySQL and MariaDB. Every product here is good at something: each page says when the other tool is the better choice.",
        "hub_matrix": "Every tool side by side", "hub_matrix_lead": "Click a tool for the full comparison, with the details and the sources behind every mark.", "hub_pages": "One page per tool", "hub_summary": "Type and price",
        "hub_tool": "Tool", "hub_kind": "Type", "hub_oss": "Open source", "hub_self": "Runs on your servers",
        "hub_pitr": "Point-in-time recovery", "hub_price": "Price",
        "yes": "Yes", "no": "No", "read": "Read the comparison",
        "us_kind": "Self-hosted backup manager with a web UI", "us_pitr": "Yes, binlog archiving", "us_price": "Free",
    },
    "vi": {
        "home": "Trang chủ", "compare": "So sánh", "download": "Tải về", "github": "GitHub",
        "glance": "Chi tiết từng mục", "vs": "So sánh song song", "vs_lead": "Mỗi dòng cho biết có tính năng đó hay không; rê chuột hoặc chạm vào dấu để xem chi tiết. Bảng bên dưới có câu trả lời đầy đủ, kèm nguồn.", "choose_them": "Chọn {name} nếu", "choose_us": "Chọn DumpCat nếu",
        "switch": "Chuyển từ {name} sang DumpCat", "sources": "Nguồn",
        "checked": "Thông tin về {name} được kiểm tra ngày {date} theo các nguồn bên dưới. Sản phẩm thay đổi theo thời gian: nếu có gì đã cũ, xin mở một issue trên GitHub.",
        "trademark": "{name} là thương hiệu của chủ sở hữu. DumpCat là dự án mã nguồn mở độc lập, không liên kết với họ.",
        "others": "So sánh khác", "cta_download": "Tải DumpCat", "cta_github": "Mã nguồn trên GitHub",
        "foot": "Mã nguồn mở theo giấy phép Apache-2.0, mã nguồn trên GitHub.", "support": "Hỗ trợ",
        "install": "Cài đặt", "whatsnew": "Có gì mới", "features": "Tính năng", "preview": "Xem giao diện",
        "guide_title": "Cài DumpCat từng bước: Linux và Windows",
        "guide_desc": "Cách cài DumpCat, công cụ backup MySQL, MariaDB, PostgreSQL và SQL Server miễn phí: một lệnh trên server Linux, tải về và bấm đúp trên Windows, rồi tạo bản backup đầu tiên. Viết cho người mới.",
        "guide_h1": "Cài DumpCat từng bước",
        "guide_lead": "Dành cho người mới: không cần rành Linux hay dòng lệnh. Khoảng năm phút từ lúc tải về tới bản backup đầu tiên.",
        "guide_linux": "Server Linux", "guide_windows": "Windows",
        "hub_title": "So sánh DumpCat với các công cụ backup MySQL khác: thay thế mã nguồn mở, tự host",
        "hub_desc": "DumpCat, công cụ backup MySQL, MariaDB, PostgreSQL và SQL Server miễn phí, mã nguồn mở, so với SqlBackupAndFTP, SimpleBackups, Ottomatik và các công cụ khác: giá, khôi phục theo thời điểm, nơi lưu và dữ liệu của bạn đi đâu.",
        "hub_h1": "So sánh DumpCat với các công cụ backup MySQL khác",
        "hub_lead": "So sánh công bằng, có dẫn nguồn, với các công cụ mọi người đang dùng để backup MySQL và MariaDB. Công cụ nào cũng có điểm mạnh: mỗi trang đều nói rõ khi nào công cụ kia hợp hơn.",
        "hub_matrix": "Tất cả công cụ, đặt cạnh nhau", "hub_matrix_lead": "Bấm vào tên một công cụ để xem so sánh đầy đủ, với chi tiết và nguồn của từng dấu.", "hub_pages": "Mỗi công cụ một trang", "hub_summary": "Loại công cụ và giá",
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


MONTHS = {
    "es": "enero febrero marzo abril mayo junio julio agosto septiembre octubre noviembre diciembre".split(),
    "pt": "janeiro fevereiro março abril maio junho julho agosto setembro outubro novembro dezembro".split(),
    "de": "Januar Februar März April Mai Juni Juli August September Oktober November Dezember".split(),
    "fr": "janvier février mars avril mai juin juillet août septembre octobre novembre décembre".split(),
}


def fmt_date(iso, lang):
    d = datetime.date.fromisoformat(iso)
    if lang == "vi":
        return f"{d.day} {MONTHS_VI[d.month - 1]}, {d.year}"
    if lang in ("zh", "ja"):
        return f"{d.year}年{d.month}月{d.day}日"
    if lang == "ko":
        return f"{d.year}년 {d.month}월 {d.day}일"
    if lang in ("es", "pt"):
        return f"{d.day} de {MONTHS[lang][d.month - 1]} de {d.year}"
    if lang == "de":
        return f"{d.day}. {MONTHS[lang][d.month - 1]} {d.year}"
    if lang == "fr":
        return f"{'1er' if d.day == 1 else d.day} {MONTHS[lang][d.month - 1]} {d.year}"
    return f"{MONTHS_EN[d.month - 1]} {d.day}, {d.year}"


def path(lang, slug=None):
    p = "/compare/" if lang == "en" else "/vi/compare/"
    return p + (f"{slug}/" if slug else "")


def guide_path(lang):
    return "/install/" if lang == "en" else "/vi/install/"


def home_path(lang):
    return "/" if lang == "en" else f"/{lang}/"


def head(lang, title, desc, slug, crumbs, at=None, alternates=True, canonical=True, robots=""):
    at = at or (lambda l: path(l, slug))
    url = BASE + at(lang)
    meta = (f'<link rel="canonical" href="{url}">\n' if canonical else "") + \
        (f'<meta name="robots" content="{robots}">\n' if robots else "")
    alt = "".join(f'<link rel="alternate" hreflang="{l}" href="{BASE + at(l)}">\n' for l in LANGS) + \
        f'<link rel="alternate" hreflang="x-default" href="{BASE + at("en")}">\n' if alternates else ""
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
{meta}{alt}<link rel="icon" href="/favicon.svg" type="image/svg+xml">
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


def header(lang, slug, crumbs, h1, lead, at=None, cta=None, langs=LANGS, eyebrow=""):
    t = T[lang]
    at = at or (lambda l: path(l, slug))
    cta = cta or (f'<a class="btn btn-light" href="{guide_path(lang)}">{t["cta_download"]}</a>'
                  f'<a class="btn btn-ghost" href="{REPO}">{t["cta_github"]}</a>')
    lang_links = "".join(
        f'<a href="{at(l)}" hreflang="{l}" lang="{l}"{" aria-current=\"true\"" if l == lang else ""}>{l.upper()}</a>'
        for l in langs)
    crumb_html = " <span aria-hidden=\"true\">/</span> ".join(
        f'<a href="{u}">{esc(n)}</a>' if i < len(crumbs) - 1 else f'<span aria-current="page">{esc(n)}</span>'
        for i, (n, u) in enumerate(crumbs))
    return f"""<header class="hero page-hero">
  <div class="wrap">
    <div class="topbar">
      <a class="brand" href="{home_path(lang)}"><svg aria-hidden="true"><use href="#dc-logo"/></svg>DumpCat</a>
      <nav class="nav" aria-label="Site">
        <a href="{home_path(lang)}#methods">{t['features']}</a>
        <a href="{path(lang)}">{t['compare']}</a>
        <a href="/demo/">{t['preview']}</a>
        <a href="{cl_path()}">{t['whatsnew']}</a>
        <a class="nav-gh" href="{REPO}" aria-label="GitHub" title="GitHub">{GH}</a>
      </nav>
      <a class="nav-cta" href="{guide_path(lang)}">{t['install']}</a>
      {f'<div class="lang" role="group" aria-label="Language">{lang_links}</div>' if langs else ''}
    </div>
    <div class="page-head">
      <nav class="crumbs" aria-label="Breadcrumb">{crumb_html}</nav>
      {f'<p class="eyebrow">{esc(eyebrow)}</p>' if eyebrow else ""}<h1>{inline(h1)}</h1>
      <p class="lead">{inline(lead)}</p>
      <div class="cta">{cta}</div>
    </div>
  </div>
</header>
"""


def footer(lang, script=""):
    t = T[lang]
    return f"""<footer class="foot">
  <div class="wrap">
    <a class="brand" href="{home_path(lang)}"><svg aria-hidden="true"><use href="#dc-logo"/></svg>DumpCat</a>
    <p>{esc(t['foot'])} <a href="{REPO}">GitHub</a> · <a href="{path(lang)}">{t['compare']}</a> · <a href="{guide_path(lang)}">{t['install']}</a> · <a href="{cl_path()}">{t['whatsnew']}</a> · {t['support']}: <a href="mailto:{SUPPORT}">{SUPPORT}</a></p>
  </div>
</footer>{script}
</body>
</html>
"""


def tool_page(tool, tools, lang):
    t = T[lang]
    d = tool[lang]
    name = tool["name"]
    crumbs = [(t["home"], home_path(lang)), (t["compare"], path(lang)), (f"DumpCat vs {name}", path(lang, tool["slug"]))]
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
  <section class="section" aria-labelledby="vs-title">
    <div class="wrap">
      <div class="head">
        <h2 id="vs-title">{t['vs']}</h2>
        <p class="intro">{esc(t['vs_lead'])}</p>
      </div>
      <div class="section-body">{vs_table(tool, lang)}{legend(lang)}</div>
    </div>
  </section>

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
    crumbs = [(t["home"], home_path(lang)), (t["compare"], path(lang))]
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
  <section class="section" aria-labelledby="matrix-title">
    <div class="wrap">
      <div class="head">
        <h2 id="matrix-title">{t['hub_matrix']}</h2>
        <p class="intro">{esc(t['hub_matrix_lead'])}</p>
      </div>
      <div class="section-body">{hub_matrix(tools, lang)}{legend(lang)}</div>
    </div>
  </section>

  <section class="section" aria-labelledby="price-title">
    <div class="wrap">
      <h2 id="price-title">{t['hub_summary']}</h2>
      <div class="section-body table-scroll cmp hub">
        <table>
          <thead><tr><th scope="col">{t['hub_tool']}</th><th scope="col">{t['hub_kind']}</th><th scope="col">{t['hub_oss']}</th><th scope="col">{t['hub_self']}</th><th scope="col">{t['hub_pitr']}</th><th scope="col">{t['hub_price']}</th></tr></thead>
          <tbody>{rows}</tbody>
        </table>
      </div>
      <h2 class="cards-title">{t['hub_pages']}</h2>
      <ul class="cards">{cards}</ul>
    </div>
  </section>
</main>
""" + footer(lang)


def guide_page(lang):
    """The step-by-step install guide: site-src/install/<lang>.html inside the common page frame."""
    t = T[lang]
    crumbs = [(t["home"], home_path(lang)), (t["install"], guide_path(lang))]
    body = (GUIDE / f"{lang}.html").read_text(encoding="utf-8")
    cta = (f'<a class="btn btn-light" href="#linux">{t["guide_linux"]}</a>'
           f'<a class="btn btn-light" href="#windows">{t["guide_windows"]}</a>')
    return (head(lang, t["guide_title"], t["guide_desc"], None, crumbs, at=guide_path)
            + header(lang, None, crumbs, t["guide_h1"], t["guide_lead"], at=guide_path, cta=cta)
            + '\n<main class="guide">\n' + body + "</main>\n"
            + footer(lang, '\n<script src="/guide.js" defer></script>'))


# ---------- What's new: one page per version, from CHANGELOG.md (English) ----------
CHANGELOG = ROOT / "CHANGELOG.md"


def md_inline(s):
    """The inline Markdown of CHANGELOG.md: `code`, **bold**, *italic*, [text](url)."""
    out = []
    for i, part in enumerate(s.split("`")):
        if i % 2:
            out.append(f"<code>{esc(part)}</code>")
            continue
        t = esc(part)
        t = re.sub(r"\[([^\]]+)\]\(([^)\s]+)\)", r'<a href="\2">\1</a>', t)
        t = re.sub(r"\*\*(.+?)\*\*", r"<b>\1</b>", t)
        t = re.sub(r"(?<![\w*])\*(?!\s)(.+?)(?<!\s)\*(?![\w*])", r"<i>\1</i>", t)
        out.append(t)
    return "".join(out)


def anchor(title):
    return re.sub(r"[^a-z0-9]+", "-", title.lower()).strip("-")


def releases():
    """CHANGELOG.md sections: version, date, summary, groups of items ("### " titles; untitled when there are none).
    A section without a date is a version not released yet: it is left out, so the site can be rebuilt any time."""
    out = []
    for block in re.split(r"(?m)^## ", CHANGELOG.read_text(encoding="utf-8"))[1:]:
        head, _, body = block.partition("\n")
        m = re.match(r"(\S+)(?: \((\d{4}-\d{2}-\d{2})\))?", head.strip())
        if not m.group(2):
            continue
        rel = {"version": m.group(1), "date": m.group(2), "lead": [], "groups": []}
        cur = None
        for line in body.splitlines():
            if line.startswith("### "):
                cur = {"title": line[4:].strip(), "items": []}
                rel["groups"].append(cur)
            elif line.startswith("- "):
                if cur is None:
                    cur = {"title": "", "items": []}
                    rel["groups"].append(cur)
                cur["items"].append(line[2:].strip())
            elif line.startswith("  ") and cur and cur["items"]:
                cur["items"][-1] += " " + line.strip()
            elif line.strip() and cur is None:
                rel["lead"].append(line.strip())
        rel["lead"] = " ".join(rel["lead"])
        out.append(rel)
    return out


def cl_path(version=None):
    return "/changelog/" + (f"{version}/" if version else "")


def release_page(rel, rels):
    v = rel["version"]
    i = rels.index(rel)
    newer, older = (rels[i - 1] if i else None), (rels[i + 1] if i + 1 < len(rels) else None)
    crumbs = [("Home", "/"), ("What's new", cl_path()), (v, cl_path(v))]
    when = f"Released {fmt_date(rel['date'], 'en')}" if rel["date"] else "Released"
    eyebrow = when + (" · latest version" if i == 0 else "")
    cta = (f'<a class="btn btn-light" href="{guide_path("en")}">Install DumpCat {esc(rels[0]["version"])}</a>'
           f'<a class="btn btn-ghost" href="{cl_path()}">All versions</a>')
    titled = [g for g in rel["groups"] if g["title"]]
    toc = ""
    if len(titled) > 1:
        toc = ('<nav class="cl-toc" aria-label="In this release"><span>In this release</span>'
               + "".join(f'<a href="#{anchor(g["title"])}">{esc(g["title"])}</a>' for g in titled) + "</nav>")
    groups = ""
    for g in rel["groups"]:
        items = "".join(f"<li>{md_inline(x)}</li>" for x in g["items"])
        title = f'<h2 id="{anchor(g["title"])}">{esc(g["title"])}</h2>' if g["title"] else ""
        groups += f'<section class="cl-group">{title}<ul class="cl-items">{items}</ul></section>'
    pager = '<nav class="cl-pager" aria-label="Other versions">'
    pager += (f'<a class="older" href="{cl_path(older["version"])}"><small>Older</small>DumpCat {esc(older["version"])}</a>'
              if older else "<span></span>")
    pager += (f'<a class="newer" href="{cl_path(newer["version"])}"><small>Newer</small>DumpCat {esc(newer["version"])}</a>'
              if newer else "<span></span>")
    pager += "</nav>"
    old_note = ""
    if i:
        old_note = (f'<p class="note">This is an older version: install the latest one from the <a href="{guide_path("en")}">'
                    f'install guide</a>. DumpCat {esc(v)} itself stays available at '
                    f'<code>https://downloads.dumpcat.cc/{esc(v)}/</code>.</p>')
    title = f"What's new in DumpCat {v}"
    desc = rel["lead"] or f"Release notes of DumpCat {v}."
    return (head("en", title, desc, None, crumbs, at=lambda l: cl_path(v), alternates=False)
            + header("en", None, crumbs, title, rel["lead"], at=lambda l: cl_path(v), cta=cta, langs=("en",), eyebrow=eyebrow)
            + f"""
<main class="cl">
  <section class="section">
    <div class="wrap narrow">
      {toc}{groups}{old_note}{pager}
    </div>
  </section>
</main>
""" + footer("en"))


def changelog_index(rels):
    crumbs = [("Home", "/"), ("What's new", cl_path())]
    items = ""
    for i, r in enumerate(rels):
        tags = "".join(f"<span>{esc(g['title'])}</span>" for g in r["groups"] if g["title"])
        date = fmt_date(r["date"], "en") if r["date"] else ""
        latest = '<em>Latest</em>' if i == 0 else ""
        items += (f'<li><a href="{cl_path(r["version"])}"><span class="cl-v">DumpCat {esc(r["version"])}{latest}</span>'
                  f'<span class="cl-date">{date}</span><span class="cl-lead">{md_inline(r["lead"])}</span>'
                  f'{f"<span class=cl-tags>{tags}</span>" if tags else ""}</a></li>')
    title = "What's new in DumpCat"
    lead = "Every release, newest first. The same notes ship as CHANGELOG.md in every package and show in DumpCat when an update is available."
    cta = (f'<a class="btn btn-light" href="{cl_path(rels[0]["version"])}">What\'s new in {esc(rels[0]["version"])}</a>'
           f'<a class="btn btn-ghost" href="{guide_path("en")}">Install DumpCat</a>')
    return (head("en", title + ": release notes", lead, None, crumbs, at=lambda l: cl_path(), alternates=False)
            + header("en", None, crumbs, title, lead, at=lambda l: cl_path(), cta=cta, langs=("en",))
            + f"""
<main class="cl">
  <section class="section">
    <div class="wrap narrow">
      <ol class="cl-list">{items}</ol>
    </div>
  </section>
</main>
""" + footer("en"))


def whatsnew_block(rel, lang):
    """The landing page's What's new card: the newest version's summary and groups, linking to its page (in English)."""
    v = rel["version"]
    t = LANDING_T[lang]
    en = ' lang="en"' if lang != "en" else ""
    date = fmt_date(rel["date"], lang) if rel["date"] else ""
    titled = [g for g in rel["groups"] if g["title"]]
    lis = "".join(f'<li><a href="{cl_path(v)}#{anchor(g["title"])}">{esc(g["title"])}</a></li>' for g in titled)
    return (f'<div class="notes wn" data-wn-version="{esc(v)}">'
            f'<p class="wn-label">{t["wn_label"]}</p>'
            f'<h3><a href="{cl_path(v)}">DumpCat {esc(v)}</a> <span class="wn-date">{date}</span></h3>'
            f'<p class="wn-lead"{en}>{md_inline(rel["lead"])}</p>'
            + (f'<ul class="wn-list"{en}>{lis}</ul>' if lis else "")
            + f'<p class="wn-links"><a href="{cl_path(v)}">{t["wn_all"]} →</a>'
              f'<a href="{cl_path()}">{t["wn_earlier"]}</a></p>'
            + (f'<p class="wn-note">{t["wn_note"]}</p>' if t["wn_note"] else "")
            + '</div>')


# ---------- the landing page: site-src/landing/index.html (Vietnamese), en.html (English), <lang>.html ----------
# What the build writes into the landing page itself: its JSON-LD description, the What's new card's labels (the
# release notes stay in English) and the note above the comparison (its facts exist in English and Vietnamese only).
# Every other text of the page is in site-src/landing/.
LANDING_T = {
    "en": {"ld": "Open-source backup, restore and point-in-time recovery for MySQL, MariaDB, PostgreSQL and SQL Server: one self-hosted binary with a web UI.",
           "wn_label": "What&#x27;s new", "wn_all": "See every change", "wn_earlier": "Earlier versions", "wn_note": ""},
    "vi": {"ld": "Backup, restore và khôi phục theo thời điểm cho MySQL, MariaDB, PostgreSQL và SQL Server, mã nguồn mở: một file chạy tự host, có giao diện web.",
           "wn_label": "Có gì mới", "wn_all": "Xem mọi thay đổi", "wn_earlier": "Các phiên bản trước", "wn_note": "Ghi chú phát hành viết bằng tiếng Anh."},
    "zh": {"ld": "开源的 MySQL、MariaDB、PostgreSQL 和 SQL Server 备份、恢复与时间点恢复工具：一个自托管的二进制文件，自带 Web 界面。",
           "wn_label": "更新内容", "wn_all": "查看全部更新", "wn_earlier": "历史版本", "wn_note": "发布说明为英文。"},
    "ja": {"ld": "MySQL、MariaDB、PostgreSQL、SQL Server のためのオープンソースのバックアップ、リストア、ポイントインタイムリカバリ。Web UI 付きのセルフホスト型シングルバイナリ。",
           "wn_label": "新着情報", "wn_all": "すべての変更を見る", "wn_earlier": "過去のバージョン", "wn_note": "リリースノートは英語です。"},
    "ko": {"ld": "MySQL, MariaDB, PostgreSQL, SQL Server를 위한 오픈 소스 백업, 복원, 특정 시점 복구: 웹 UI가 포함된 셀프 호스팅 단일 바이너리.",
           "wn_label": "새 소식", "wn_all": "모든 변경 사항 보기", "wn_earlier": "이전 버전", "wn_note": "릴리스 노트는 영어로 작성됩니다."},
    "es": {"ld": "Copias de seguridad, restauración y recuperación a un punto en el tiempo de código abierto para MySQL, MariaDB, PostgreSQL y SQL Server: un solo binario autoalojado con interfaz web.",
           "wn_label": "Novedades", "wn_all": "Ver todos los cambios", "wn_earlier": "Versiones anteriores", "wn_note": "Las notas de la versión están en inglés."},
    "pt": {"ld": "Backup, restauração e recuperação point-in-time de código aberto para MySQL, MariaDB, PostgreSQL e SQL Server: um único binário auto-hospedado com interface web.",
           "wn_label": "Novidades", "wn_all": "Ver todas as mudanças", "wn_earlier": "Versões anteriores", "wn_note": "As notas de versão estão em inglês."},
    "de": {"ld": "Open-Source-Backup, -Wiederherstellung und Point-in-Time-Recovery für MySQL, MariaDB, PostgreSQL und SQL Server: eine selbst gehostete Binärdatei mit Weboberfläche.",
           "wn_label": "Neuigkeiten", "wn_all": "Alle Änderungen ansehen", "wn_earlier": "Frühere Versionen", "wn_note": "Die Versionshinweise sind auf Englisch."},
    "fr": {"ld": "Sauvegarde, restauration et récupération à un instant précis open source pour MySQL, MariaDB, PostgreSQL et SQL Server : un seul binaire auto-hébergé avec interface web.",
           "wn_label": "Nouveautés", "wn_all": "Voir tous les changements", "wn_earlier": "Versions précédentes", "wn_note": "Les notes de version sont en anglais."},
}
GLOBE = ('<svg aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round">'
         '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.6 3.8 5.6 3.8 9s-1.3 6.4-3.8 9c-2.5-2.6-3.8-5.6-3.8-9S9.5 5.6 12 3z"/></svg>')


def landing_templates(lang):
    """site-src/landing/<lang>.html: {key: (labels, text)} from its <template data-i18n="key"> elements."""
    f = LANDING / f"{lang}.html"
    if not f.exists():
        raise SystemExit(f"site-src/landing/{lang}.html is missing: every language of LANDING_LANGS needs one (start from en.html)")
    out = {}
    for key, labels, text in re.findall(r'<template data-i18n="([^"]+)"( data-labels)?>(.*?)</template>', f.read_text(encoding="utf-8"), re.S):
        if key in out:
            raise SystemExit(f"site-src/landing/{lang}.html: {key} is there twice")
        out[key] = (bool(labels), textwrap.dedent(text.strip("\n").rstrip()) if "\n" in text.strip() else text.strip())
    return out


def relabel(text, pairs, where):
    """A figure in another language: each "English label => translation" line replaces that text (and an aria-label
    equal to it) in the English figure, so the figures themselves are written once, in en.html."""
    for line in pairs.splitlines():
        if not line.strip():
            continue
        a, sep, b = line.partition(" => ")
        if not sep:
            raise SystemExit(f"{where}: a label line needs \" => \": {line!r}")
        a, b = a.strip(), b.strip()
        new = text.replace(f">{a}<", f">{b}<").replace(f'"{a}"', f'"{b}"')
        if new == text:
            raise SystemExit(f"{where}: {a!r} is not a label of the English figure")
        text = new
    return text


def landing_strings(lang="en"):
    """Every key of the page in lang: en.html, with <lang>.html over it. A key a language has not translated yet stays
    in English (the build lists them); a key en.html does not have is an error."""
    en = {k: t for k, (_, t) in landing_templates("en").items()}
    if lang == "en":
        return en
    out = dict(en)
    own = landing_templates(lang)
    for key, (labels, text) in own.items():
        if key not in en:
            raise SystemExit(f"site-src/landing/{lang}.html: {key} is not a key of en.html")
        out[key] = relabel(en[key], text, f"site-src/landing/{lang}.html: {key}") if labels else text
    missing = sorted(set(en) - set(own))
    if missing:
        print(f"site-src/landing/{lang}.html: {len(missing)} keys still in English: {', '.join(missing)}")
    return out


def lang_menu(lang):
    """The landing page's language menu (summary and list), every language linking to its own page."""
    items = "".join(
        f'<li><a href="{home_path(l)}" hreflang="{h}" lang="{h}"{" aria-current=\"true\"" if l == lang else ""}>{name}<span>{l.upper()}</span></a></li>'
        for l, (h, name, _) in LANDING_LANGS.items())
    return GLOBE + f"<span>{lang.upper()}</span>", f'<ul class="lang-list">{items}</ul>'


def closing_tag(page, name, pos):
    """Where the element <name> opened just before pos ends: the index of its </name>."""
    depth = 1
    for m in re.compile(rf"<(/?){name}\b[^<>]*?(/?)>").finditer(page, pos):
        if not m.group(2):
            depth += -1 if m.group(1) else 1
            if depth == 0:
                return m.start()
    raise SystemExit(f"site-src/landing/index.html: <{name}> is not closed")


def localize(page, strings):
    """The landing template in another language: an element with data-i18n="key" gets strings[key] as its content,
    data-i18n-<attribute>="key" sets that attribute to strings[key]. With strings=None the template's own text stays.
    The data-i18n markers are removed either way."""
    out, pos, used = [], 0, set()
    def get(key):
        if key not in strings:
            raise SystemExit(f"site-src/landing/en.html: no English for {key}")
        used.add(key)
        return strings[key]
    for m in re.finditer(r"<([a-zA-Z][\w-]*)(\s[^<>]*?)?(/?)>", page):
        if m.start() < pos:  # inside an element whose content was replaced
            continue
        name, attrs = m.group(1), m.group(2) or ""
        if "data-i18n" not in attrs:
            continue
        out.append(page[pos:m.start()])
        content = None
        for key_m in re.finditer(r'\sdata-i18n(?:-([\w-]+))?="([^"]+)"', m.group(2)):
            attr, key = key_m.groups()
            if attr is None:
                content = key
            elif strings is not None:
                value = get(key).replace('"', "&quot;")
                attrs, n = re.subn(rf'(\s{attr}=")[^"]*"', lambda a: a.group(1) + value + '"', attrs, count=1)
                if not n:
                    raise SystemExit(f"site-src/landing/index.html: <{name}> has data-i18n-{attr} but no {attr}")
        attrs = re.sub(r'\sdata-i18n(?:-[\w-]+)?="[^"]*"', "", attrs)
        out.append(f"<{name}{attrs}{m.group(3)}>")
        pos = m.end()
        if content is not None and strings is not None:
            text = get(content)
            if "\n" in text:  # one item per line, indented under the element
                indent = re.match(r"[ \t]*", page[page.rfind("\n", 0, m.start()) + 1:]).group()
                text = "".join(f"\n{indent}  {line}" for line in text.splitlines()) + "\n" + indent
            out.append(text)
            pos = closing_tag(page, name, pos)
    out.append(page[pos:])
    if strings is not None and set(strings) - used:
        raise SystemExit(f"site-src/landing/en.html: not used in index.html: {sorted(set(strings) - used)}")
    return "".join(out)


def landing_page(lang, tools, rel):
    page = localize((LANDING / "index.html").read_text(encoding="utf-8"), None if lang == "vi" else landing_strings(lang))
    url = BASE + home_path(lang)
    hl, _, locale = LANDING_LANGS[lang]
    current, menu = lang_menu(lang)
    # the comparison's facts exist in English and Vietnamese: other languages show the English table
    compare = landing_block(tools, lang) if lang in LANGS else landing_block(tools, "en").replace('<div class="lc">', '<div class="lc" lang="en">', 1)
    ld = {"@context": "https://schema.org", "@type": "SoftwareApplication", "name": "DumpCat", "url": url,
          "description": LANDING_T[lang]["ld"], "applicationCategory": "DeveloperApplication", "operatingSystem": "Linux, Windows",
          "license": "https://www.apache.org/licenses/LICENSE-2.0", "isAccessibleForFree": True,
          "offers": {"@type": "Offer", "price": "0", "priceCurrency": "USD"}, "downloadUrl": url + "#download",
          "image": f"{BASE}/og.png", "sameAs": [REPO],
          "author": {"@type": "Person", "name": "theweakestharrison", "url": "https://github.com/theweakestharrison"}}
    parts = [
        (re.compile(r"<!-- The landing page, in Vietnamese;.*?-->", re.S),
         "<!-- Written by scripts/build-site.py from site-src/landing/ (index.html, en.html, <lang>.html): edit those, then run it. -->"),
        ('<html lang="vi">', f'<html lang="{hl}">'),
        ("<!-- urls (scripts/build-site.py): canonical, hreflang, og:url, og:locale -->\n",
         f'<link rel="canonical" href="{url}">\n'
         + "".join(f'<link rel="alternate" hreflang="{h}" href="{BASE + home_path(l)}">\n' for l, (h, _, _) in LANDING_LANGS.items())
         + f'<link rel="alternate" hreflang="x-default" href="{BASE + home_path("en")}">\n'
         + f'<meta property="og:url" content="{url}">\n<meta property="og:locale" content="{locale}">\n'),
        ("<!-- lang-current (scripts/build-site.py) -->", current),
        ("<!-- lang-list (scripts/build-site.py) -->", menu),
        ("<!-- ld+json (scripts/build-site.py) -->",
         '<script type="application/ld+json">\n' + json.dumps(ld, ensure_ascii=False, indent=2) + "\n</script>"),
        ("<!-- compare (scripts/build-site.py) -->", compare),
        ("<!-- whatsnew (scripts/build-site.py) -->", whatsnew_block(rel, lang)),
    ]
    for marker, text in parts:
        if isinstance(marker, str):
            marker = re.compile(re.escape(marker))
        page, n = marker.subn(lambda _: text, page)
        if n != 1:
            raise SystemExit(f"site-src/landing/index.html: {marker.pattern!r} found {n} times")
    return page


def write(rel, text):
    f = OUT / rel
    f.parent.mkdir(parents=True, exist_ok=True)
    f.write_text(text, encoding="utf-8", newline="\n")


TODAY = datetime.date.today().isoformat()


def git_date(*paths):
    """When paths last changed: today if they have uncommitted changes (the build output is committed with
    them), else the date of the last commit that touched them; today when git is not available."""
    try:
        run = lambda *a: subprocess.run(["git", *a, "--", *paths], cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip()
        if run("status", "--porcelain"):
            return TODAY
        return run("log", "-1", "--format=%cs") or TODAY
    except (OSError, subprocess.CalledProcessError):
        return TODAY


def not_found_page():
    """site/404.html: served with status 404 for any missing path (wrangler.jsonc: not_found_handling)."""
    crumbs = [("Home", "/"), ("Not found", "/")]
    cta = (f'<a class="btn btn-light" href="/">DumpCat home</a>'
           f'<a class="btn btn-ghost" href="{guide_path("en")}">Install DumpCat</a>')
    lead = "This page does not exist, or it moved. Không tìm thấy trang này."
    links = "".join(f'<li><a href="{u}">{esc(n)}</a><span>{esc(d)}</span></li>' for n, u, d in [
        ("Install DumpCat", guide_path("en"), "Step by step, on Linux or Windows"),
        ("Compare", path("en"), "DumpCat next to other MySQL backup tools"),
        ("What's new", cl_path(), "Every release"),
        ("Preview the UI", "/demo/", "The interface you get after installing, with sample data"),
    ])
    return (head("en", "Page not found · DumpCat", "This page does not exist.", None, crumbs, at=lambda l: "/404.html",
                 alternates=False, canonical=False, robots="noindex")
            + header("en", None, crumbs, "Page not found", lead, at=lambda l: "/", cta=cta, langs=())
            + f"""
<main>
  <section class="section">
    <div class="wrap narrow">
      <ul class="related">{links}</ul>
    </div>
  </section>
</main>
""" + footer("en"))


def main():
    tools = [json.loads(p.read_text(encoding="utf-8")) for p in sorted(SRC.glob("*.json"))]
    tools.sort(key=lambda o: o.get("order", 100))
    for o in tools:
        for lang in LANGS:
            missing = [k for k, _, _ in DIMENSIONS if k not in o[lang]["values"]]
            if missing:
                raise SystemExit(f"{o['slug']}: {lang} values missing {missing}")
        grid = o.get("grid", {})
        bad = [k for k in GRID_KEYS if k not in grid or grid[k][0] not in VERDICT
               or (grid[k][0] != "yes" and grid[k][0] != "unknown" and not (len(grid[k]) > 1 and grid[k][1].get("en") and grid[k][1].get("vi")))]
        if bad:
            raise SystemExit(f"{o['slug']}: grid rows missing, unknown verdict or a note missing: {bad}")
    for lang in LANGS:
        write(path(lang).lstrip("/") + "index.html", hub_page(tools, lang))
        for o in tools:
            write(path(lang, o["slug"]).lstrip("/") + "index.html", tool_page(o, tools, lang))
        write(guide_path(lang).lstrip("/") + "index.html", guide_page(lang))
    rels = releases()
    write(cl_path().lstrip("/") + "index.html", changelog_index(rels))
    for r in rels:
        write(cl_path(r["version"]).lstrip("/") + "index.html", release_page(r, rels))

    for lang in LANDING_LANGS:
        write(home_path(lang).lstrip("/") + "index.html", landing_page(lang, tools, rels[0]))

    write("404.html", not_found_page())

    # sitemap: every canonical page once, with a lastmod that changes only when the page's content does. The
    # language versions are not repeated here: each page names them in its <head> (hreflang), which search
    # engines read as well, and xhtml:link elements made browsers render the sitemap as loose text
    # instead of showing the XML
    tool_dates = {o["slug"]: max(o["checked"], git_date(f"site-src/compare/{o['slug']}.json")) for o in tools}
    # (a fixed URL, or a path per language; lastmod; the languages, when not LANGS)
    urls = ([(None, home_path, lambda l: git_date(f"site{home_path(l)}index.html", "site/app.js"), tuple(LANDING_LANGS)),
             (None, lambda l: path(l), max(tool_dates.values())),
             (None, guide_path, git_date("site-src/install")),
             ("/demo/", None, git_date("web/index.html", "web/assets", "site-src/demo"))]
            + [(None, lambda l, s=o["slug"]: path(l, s), tool_dates[o["slug"]]) for o in tools]
            + [(cl_path(), None, rels[0]["date"] or git_date("CHANGELOG.md"))]
            + [(cl_path(r["version"]), None, r["date"] or git_date("CHANGELOG.md")) for r in rels])
    items = []
    for fixed, at, mod, *langs in urls:
        if fixed:
            items.append(f"  <url><loc>{BASE}{fixed}</loc><lastmod>{mod}</lastmod></url>")
            continue
        for lang in (langs[0] if langs else LANGS):
            items.append(f"  <url><loc>{BASE + at(lang)}</loc><lastmod>{mod(lang) if callable(mod) else mod}</lastmod></url>")
    write("sitemap.xml", '<?xml version="1.0" encoding="UTF-8"?>\n'
          '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n'
          + "\n".join(items) + "\n</urlset>\n")
    write("robots.txt", "# dumpcat.cc: every page may be crawled; the pages to index are listed in the sitemap.\n"
          "# The one-line installers (/install.sh, /install.ps1) are scripts: they are sent with X-Robots-Tag: noindex.\n"
          f"User-agent: *\nAllow: /\n\nSitemap: {BASE}/sitemap.xml\n")
    print(f"built the landing page x {len(LANDING_LANGS)} languages, {len(tools)} comparisons and the install guide x "
          f"{len(LANGS)} languages, {len(rels)} release pages, sitemap with {len(items)} URLs")


if __name__ == "__main__":
    main()
