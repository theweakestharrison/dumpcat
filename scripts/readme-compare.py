# Writes the "Compared with other tools" table of README.md and README.vi.md from the sourced comparison data
# (site-src/compare/*.json) and DumpCat's own US_GRID in scripts/build-site.py, so the README matches dumpcat.cc/compare.
# Run from the repo root after changing a comparison: python scripts/readme-compare.py
import importlib.util, io, json, sys

spec = importlib.util.spec_from_file_location("buildsite", "scripts/build-site.py")
bs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bs)

TOOLS = ["portabase", "databasus", "sqlbackupandftp", "simplebackups", "sqlbak"]
ROWS = ["open_source", "free", "self_hosted", "windows", "web_ui",
        "parallel", "physical", "incremental",
        "pitr", "restore_ui", "verify",
        "multi_storage", "encryption", "direct", "alerts"]
MARK = {"yes": "✅", "paid": "💲", "partial": "🟡", "no": "❌", "unknown": "❔"}
labels = {k: l for _, rows in bs.GRID for k, l in rows}
tools = [json.load(io.open(f"site-src/compare/{s}.json", encoding="utf-8")) for s in TOOLS]

TEXT = {
    "en": {
        "file": "README.md", "anchor": "## How it works", "title": "## Compared with other tools",
        "lead": ("How DumpCat compares with other MySQL and MariaDB backup tools. Every mark was checked against the "
                 "vendor's own documentation in October 2026; the full comparisons, with notes and sources, are on "
                 "[dumpcat.cc/compare](https://dumpcat.cc/compare/)."),
        "legend": "✅&nbsp;yes · 💲 paid plan · 🟡 partly · ❌ no · ❔ not documented",
        "base": "https://dumpcat.cc/compare/",
        "tail": ("Portabase and Databasus also back up MongoDB, Redis and other engines; DumpCat sticks to MySQL, MariaDB, "
                 "PostgreSQL and SQL Server and goes deeper on each (see [What DumpCat supports](#what-dumpcat-supports))."),
    },
    "vi": {
        "file": "README.vi.md", "anchor": "## Cách hoạt động", "title": "## So sánh với các công cụ khác",
        "lead": ("DumpCat so với các công cụ backup MySQL, MariaDB khác. Mỗi dấu đều được đối chiếu với tài liệu của chính "
                 "hãng vào tháng 10/2026; bản so sánh đầy đủ, kèm ghi chú và nguồn, ở "
                 "[dumpcat.cc/vi/compare](https://dumpcat.cc/vi/compare/)."),
        "legend": "✅&nbsp;có · 💲 gói trả phí · 🟡 một phần · ❌ không · ❔ không có thông tin",
        "base": "https://dumpcat.cc/vi/compare/",
        "tail": ("Portabase và Databasus còn backup cả MongoDB, Redis và các engine khác; DumpCat chỉ tập trung vào MySQL, "
                 "MariaDB, PostgreSQL và SQL Server nhưng làm sâu hơn với từng loại (xem "
                 "[DumpCat hỗ trợ những gì](#dumpcat-hỗ-trợ-những-gì))."),
    },
}


def section(lang):
    t = TEXT[lang]
    head = "| | **DumpCat** | " + " | ".join(f"[{o['name']}]({t['base']}{o['slug']}/)" for o in tools) + " |"
    sep = "|---|:---:|" + ":---:|" * len(tools)
    lines = [t["title"], "", t["lead"], "", t["legend"], "", head, sep]
    for key in ROWS:
        cells = [MARK[bs.US_GRID[key][0]]] + [MARK[o["grid"][key][0]] for o in tools]
        lines.append(f"| {labels[key][lang]} | " + " | ".join(cells) + " |")
    lines += ["", t["tail"], "", ""]
    return "\n".join(lines)


def apply(lang):
    t = TEXT[lang]
    s = io.open(t["file"], encoding="utf-8", newline="").read()
    assert "\r\n" not in s
    new = section(lang)
    start = s.find(t["title"] + "\n")
    if start >= 0:  # already there: replace up to the next section
        end = s.index("\n## ", start + 1) + 1
        s = s[:start] + new + s[end:]
    else:
        assert s.count(t["anchor"] + "\n") == 1, t["anchor"]
        s = s.replace(t["anchor"] + "\n", new + t["anchor"] + "\n")
    io.open(t["file"], "w", encoding="utf-8", newline="").write(s)
    print(t["file"], "ok")


for lang in (sys.argv[1:] or ["en", "vi"]):
    apply(lang)
