#!/usr/bin/env python3
"""Generate the comparison pages of dumpcat.cc from site-src/compare/*.json.

Writes (English and Vietnamese, linked with hreflang):
  site/compare/index.html, site/compare/<slug>/index.html
  site/vi/compare/index.html, site/vi/compare/<slug>/index.html
  site/install/index.html, site/vi/install/index.html   (the step-by-step install guide, from site-src/install/)
  site/sitemap.xml, site/robots.txt

Run from anywhere: python scripts/build-site.py   (standard library only). See docs/website.vi.md.
"""
import datetime
import html
import json
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parent.parent
SRC = ROOT / "site-src" / "compare"
GUIDE = ROOT / "site-src" / "install"
OUT = ROOT / "site"
BASE = "https://dumpcat.cc"
REPO = "https://github.com/theweakestharrison/DumpCat"
SUPPORT = "fat@dumpcat.cc"
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


def landing_block(tools):
    """The short side-by-side comparison of the landing page, in both languages (the page shows one by html[lang])."""
    def both(d, cls=""):
        c = f' class="{cls}"' if cls else ""
        return f'<span lang="vi"{c}>{esc(d["vi"])}</span><span lang="en"{c}>{esc(d["en"])}</span>'
    def cell(c):
        return (f'<span lang="vi">{mark(c, "vi")}</span><span lang="en">{mark(c, "en")}</span>')
    labels = {k: l for _, rows in GRID for k, l in rows}
    first = tools[0]["slug"]
    chips = "".join(
        f'<button type="button" class="lc-chip" data-tool="{o["slug"]}" aria-pressed="{"true" if o["slug"] == first else "false"}">{esc(o["name"])}</button>'
        for o in tools)
    heads = "".join(f'<span class="vs-name" data-tool="{o["slug"]}"{"" if o["slug"] == first else " hidden"}>{wbr(o["name"])}</span>' for o in tools)
    rows = ""
    for key in LANDING_ROWS:
        cells = "".join(f'<span class="lc-cell" data-tool="{o["slug"]}"{"" if o["slug"] == first else " hidden"}>{cell(o["grid"][key])}</span>'
                        for o in tools)
        rows += f'<tr><th scope="row">{both(labels[key])}</th><td class="us">{cell(US_GRID[key])}</td><td>{cells}</td></tr>'
    links = ""
    for o in tools:
        text = {"en": "Full comparison: DumpCat vs " + o["name"], "vi": "So sánh đầy đủ DumpCat với " + o["name"]}
        shown = "" if o["slug"] == first else " hidden"
        links += (f'<a class="lc-more" data-tool="{o["slug"]}"{shown} href="/compare/{o["slug"]}/"'
                  f' data-vi-href="/vi/compare/{o["slug"]}/">{both(text)} →</a>')
    all_link = both({"en": "All comparisons", "vi": "Tất cả so sánh"})
    legends = legend("vi").replace("<ul ", '<ul lang="vi" ', 1) + legend("en").replace("<ul ", '<ul lang="en" ', 1)
    return (f'<div class="lc">'
            f'<div class="lc-chips" role="group" aria-label="Compare with">{chips}</div>'
            f'<div class="vs"><table><thead><tr><th scope="col"></th>'
            f'<th scope="col" class="us"><span class="vs-name"><svg aria-hidden="true"><use href="#dc-logo"/></svg>DumpCat</span></th>'
            f'<th scope="col">{heads}</th></tr></thead><tbody>{rows}</tbody></table></div>'
            f'<div class="lc-foot"><div class="lc-links">{links}<a href="/compare/" data-vi-href="/vi/compare/">{all_link} →</a></div>'
            f'{legends}</div>'
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
        "install": "Install",
        "guide_title": "Install DumpCat step by step: Linux and Windows",
        "guide_desc": "How to install DumpCat, the free MySQL and MariaDB backup manager: one command on a Linux server, download and double-click on Windows, then your first backup. Written for beginners.",
        "guide_h1": "Install DumpCat step by step",
        "guide_lead": "For beginners: no Linux or command-line experience needed. About five minutes from download to your first backup.",
        "guide_linux": "Linux server", "guide_windows": "Windows",
        "hub_title": "DumpCat vs other MySQL backup tools: open-source, self-hosted alternative",
        "hub_desc": "How DumpCat, a free open-source MySQL and MariaDB backup manager, compares with SqlBackupAndFTP, SimpleBackups, Ottomatik and other backup tools: price, point-in-time recovery, storage and where your data goes.",
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
        "install": "Cài đặt",
        "guide_title": "Cài DumpCat từng bước: Linux và Windows",
        "guide_desc": "Cách cài DumpCat, công cụ backup MySQL và MariaDB miễn phí: một lệnh trên server Linux, tải về và bấm đúp trên Windows, rồi tạo bản backup đầu tiên. Viết cho người mới.",
        "guide_h1": "Cài DumpCat từng bước",
        "guide_lead": "Dành cho người mới: không cần rành Linux hay dòng lệnh. Khoảng năm phút từ lúc tải về tới bản backup đầu tiên.",
        "guide_linux": "Server Linux", "guide_windows": "Windows",
        "hub_title": "So sánh DumpCat với các công cụ backup MySQL khác: thay thế mã nguồn mở, tự host",
        "hub_desc": "DumpCat, công cụ backup MySQL và MariaDB miễn phí, mã nguồn mở, so với SqlBackupAndFTP, SimpleBackups, Ottomatik và các công cụ khác: giá, khôi phục theo thời điểm, nơi lưu và dữ liệu của bạn đi đâu.",
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


def fmt_date(iso, lang):
    d = datetime.date.fromisoformat(iso)
    if lang == "vi":
        return f"{d.day} {MONTHS_VI[d.month - 1]}, {d.year}"
    return f"{MONTHS_EN[d.month - 1]} {d.day}, {d.year}"


def path(lang, slug=None):
    p = "/compare/" if lang == "en" else "/vi/compare/"
    return p + (f"{slug}/" if slug else "")


def guide_path(lang):
    return "/install/" if lang == "en" else "/vi/install/"


def head(lang, title, desc, slug, crumbs, at=None):
    at = at or (lambda l: path(l, slug))
    url = BASE + at(lang)
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
<link rel="alternate" hreflang="en" href="{BASE + at('en')}">
<link rel="alternate" hreflang="vi" href="{BASE + at('vi')}">
<link rel="alternate" hreflang="x-default" href="{BASE + at('en')}">
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


def header(lang, slug, crumbs, h1, lead, at=None, cta=None):
    t = T[lang]
    at = at or (lambda l: path(l, slug))
    cta = cta or (f'<a class="btn btn-light" href="{guide_path(lang)}">{t["cta_download"]}</a>'
                  f'<a class="btn btn-ghost" href="{REPO}">{t["cta_github"]}</a>')
    lang_links = "".join(
        f'<a href="{at(l)}" hreflang="{l}" lang="{l}"{" aria-current=\"true\"" if l == lang else ""}>{l.upper()}</a>'
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
        <a href="{guide_path(lang)}">{t['install']}</a>
        <a href="/#download">{t['download']}</a>
        <a href="{REPO}">{t['github']}</a>
      </nav>
      <div class="lang" role="group" aria-label="Language">{lang_links}</div>
    </div>
    <div class="page-head">
      <nav class="crumbs" aria-label="Breadcrumb">{crumb_html}</nav>
      <h1>{inline(h1)}</h1>
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
    <a class="brand" href="/"><svg aria-hidden="true"><use href="#dc-logo"/></svg>DumpCat</a>
    <p>{esc(t['foot'])} <a href="{REPO}">GitHub</a> · <a href="{path(lang)}">{t['compare']}</a> · <a href="{guide_path(lang)}">{t['install']}</a> · {t['support']}: <a href="mailto:{SUPPORT}">{SUPPORT}</a></p>
  </div>
</footer>{script}
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
    crumbs = [(t["home"], "/"), (t["install"], guide_path(lang))]
    body = (GUIDE / f"{lang}.html").read_text(encoding="utf-8")
    cta = (f'<a class="btn btn-light" href="#linux">{t["guide_linux"]}</a>'
           f'<a class="btn btn-light" href="#windows">{t["guide_windows"]}</a>')
    return (head(lang, t["guide_title"], t["guide_desc"], None, crumbs, at=guide_path)
            + header(lang, None, crumbs, t["guide_h1"], t["guide_lead"], at=guide_path, cta=cta)
            + '\n<main class="guide">\n' + body + "</main>\n"
            + footer(lang, '\n<script src="/guide.js" defer></script>'))


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

    # the landing page's short comparison sits between two markers in site/index.html
    idx = OUT / "index.html"
    page = idx.read_text(encoding="utf-8")
    a, z = "<!-- compare:begin (scripts/build-site.py) -->", "<!-- compare:end -->"
    i, j = page.index(a) + len(a), page.index(z)
    idx.write_text(page[:i] + "\n" + landing_block(tools) + "\n          " + page[j:], encoding="utf-8", newline="\n")

    newest = max([o["checked"] for o in tools] + [datetime.date.today().isoformat()])
    guide_mod = datetime.date.fromtimestamp(max(p.stat().st_mtime for p in GUIDE.glob("*.html"))).isoformat()
    urls = ([("/", None, newest), (None, lambda l: path(l), newest), (None, guide_path, guide_mod)]
            + [(None, lambda l, s=o["slug"]: path(l, s), o["checked"]) for o in tools])
    items = []
    for fixed, at, mod in urls:
        if fixed:
            items.append(f"  <url><loc>{BASE}{fixed}</loc><lastmod>{mod}</lastmod></url>")
            continue
        alts = "".join(f'<xhtml:link rel="alternate" hreflang="{l}" href="{BASE + at(l)}"/>' for l in LANGS)
        for lang in LANGS:
            items.append(f"  <url><loc>{BASE + at(lang)}</loc><lastmod>{mod}</lastmod>{alts}</url>")
    write("sitemap.xml", '<?xml version="1.0" encoding="UTF-8"?>\n'
          '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">\n'
          + "\n".join(items) + "\n</urlset>\n")
    write("robots.txt", f"User-agent: *\nAllow: /\n\nSitemap: {BASE}/sitemap.xml\n")
    print(f"built {len(tools)} comparisons and the install guide x {len(LANGS)} languages, sitemap with {len(items)} URLs")


if __name__ == "__main__":
    main()
