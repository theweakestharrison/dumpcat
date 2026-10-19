(function () {
  "use strict";

  var STR = {
    vi: { aria: "Thời điểm khôi phục", consistent: "mysqldump · nhất quán lúc", files: function (n) { return n + " file, kiểm tra từng file trước khi đụng vào dữ liệu"; }, ok: "Trước sự cố: bảng orders còn nguyên", bad: "Sau sự cố: bảng orders đã bị xóa", copy: "Sao chép", copied: "Đã chép", selected: "Đã chọn, nhấn Ctrl+C",
      relLoading: "Đang tải thông tin bản phát hành…",
      relIntro: "Phiên bản {v}, phát hành ngày {d}. Mỗi gói có file chạy, script cài đặt và hướng dẫn đầy đủ; SHA-256 của mọi file nằm trong {sums}.",
      relFailed: "Chưa tải được thông tin bản phát hành. Phiên bản mới nhất và các file được liệt kê trong {feed}.", dateLocale: "vi-VN" },
    en: { aria: "Recovery point", consistent: "mysqldump · consistent at", files: function (n) { return n + (n === 1 ? " file" : " files") + ", each checked before any data is touched"; }, ok: "Before the incident: orders is intact", bad: "After the incident: orders is already gone", copy: "Copy", copied: "Copied", selected: "Selected, press Ctrl+C",
      relLoading: "Loading the latest release…",
      relIntro: "Version {v}, released {d}. Each package holds the binaries, the installer and the full manual; SHA-256 checksums of every file are in {sums}.",
      relFailed: "The release details could not be loaded. The latest version and its files are listed in {feed}.", dateLocale: "en" }
  };

  // ---- PITR timeline model (seconds since 00:00 on 18/10/2026) ----
  var AXIS_END = 18 * 3600, NOW = 16 * 3600 + 20 * 60;
  var BACKUP_AT = 2 * 3600, CONSISTENT = BACKUP_AT + 3, INCIDENT = 14 * 3600 + 37 * 60 + 12;
  var FILE_SECONDS = 900, FILE_TODAY = 123, FILE_YESTERDAY = FILE_TODAY - 86400 / FILE_SECONDS;
  // The page's language: / is English, /vi/ is Vietnamese (both written by scripts/build-site.py).
  var lang = document.documentElement.lang === "vi" ? "vi" : "en";
  var t = INCIDENT - 1;

  var range = document.getElementById("tl-range");
  var replay = document.getElementById("tl-replay");
  var roTo = document.getElementById("ro-to");
  var roBase = document.getElementById("ro-base");
  var roReplay = document.getElementById("ro-replay");
  var roStatus = document.getElementById("ro-status");

  function pad(n, w) { n = String(n); while (n.length < w) n = "0" + n; return n; }
  function hms(s) { return pad(Math.floor(s / 3600), 2) + ":" + pad(Math.floor(s / 60) % 60, 2) + ":" + pad(s % 60, 2); }
  function pct(s) { return (s / AXIS_END * 100).toFixed(4) + "%"; }
  function binlog(i) { return "mysql-bin." + pad(i, 6); }

  function render() {
    var s = STR[lang];
    var today = t >= CONSISTENT;
    var baseAt = today ? CONSISTENT : CONSISTENT - 86400;
    var first = today ? FILE_TODAY : FILE_YESTERDAY;
    var last = first + Math.floor((t - baseAt) / FILE_SECONDS);
    var start = today ? BACKUP_AT : 0;

    replay.style.left = pct(start);
    replay.style.width = pct(Math.max(0, t - start));
    roTo.textContent = "18/10/2026 " + hms(t);
    roBase.innerHTML = (today ? "20261018-020000-full" : "20261017-020000-full") + "<small>" + s.consistent + " " + (today ? "" : "17/10 ") + hms(CONSISTENT) + "</small>";
    roReplay.innerHTML = binlog(first) + " → " + binlog(last) + "<small>" + s.files(last - first + 1) + "</small>";
    var ok = t < INCIDENT;
    roStatus.className = "status " + (ok ? "ok" : "bad");
    roStatus.textContent = ok ? s.ok : s.bad;
    range.setAttribute("aria-label", s.aria);
    range.setAttribute("aria-valuetext", hms(t));
  }

  function setT(v) {
    t = Math.max(0, Math.min(NOW, Math.round(v)));
    range.value = String(t);
    render();
  }

  range.addEventListener("input", function () { setT(Number(range.value)); });
  range.addEventListener("keydown", function (e) {
    var step = { ArrowLeft: -60, ArrowDown: -60, ArrowRight: 60, ArrowUp: 60, PageDown: -900, PageUp: 900 }[e.key];
    if (step) { e.preventDefault(); setT(t + (e.shiftKey ? step * 15 : step)); }
  });
  document.getElementById("tl-snap").addEventListener("click", function () { setT(INCIDENT - 1); range.focus(); });

  // ---- language: EN / VI link to the other page; keep the section the visitor is reading ----
  document.querySelectorAll(".lang a").forEach(function (a) {
    a.addEventListener("click", function () { a.hash = location.hash; });
  });

  // ---- latest release: read from the update feed, so a new upload shows here without redeploying the site ----
  var relMeta = document.querySelector('meta[name="dumpcat-release-base"]');
  var relBase = new URL(relMeta ? relMeta.content : "https://downloads.dumpcat.cc/", location.href);
  if (!/\/$/.test(relBase.pathname)) relBase.pathname += "/";
  var relFeed = new URL("latest.json", relBase);
  var release = null, releaseFailed = false;
  var PLATFORMS = { "linux/amd64": "Linux x86-64", "linux/arm64": "Linux ARM64", "windows/amd64": "Windows x64" };

  // Links only to the release server itself: a tampered feed must not turn this page into a link to elsewhere.
  function relURL(ref) {
    var u = new URL(ref, relFeed);
    if (u.origin !== relBase.origin || u.pathname.indexOf(relBase.pathname) !== 0) throw new Error("file outside the release server: " + ref);
    return u.href;
  }
  function node(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function link(href, text) { var a = node("a", "", text); a.href = href; return a; }
  // "{name}" placeholders in a sentence become the given nodes or strings.
  function fill(el, tpl, parts) {
    el.textContent = "";
    tpl.split(/\{(\w+)\}/).forEach(function (piece, i) {
      if (i % 2 === 0) { if (piece) el.appendChild(document.createTextNode(piece)); return; }
      var p = parts[piece];
      el.appendChild(typeof p === "string" ? document.createTextNode(p) : p);
    });
  }
  function renderRelease() {
    var s = STR[lang];
    var intro = document.getElementById("rel-intro");
    if (releaseFailed) { fill(intro, s.relFailed, { feed: link(relFeed.href, "latest.json") }); return; }
    if (!release) { intro.textContent = s.relLoading; return; }
    var v = release.version;
    document.querySelectorAll("[data-version]").forEach(function (e) { e.textContent = v; });
    document.querySelectorAll("[data-base]").forEach(function (e) { e.textContent = relBase.href; });
    var when = new Date(release.released_at);
    var date = isNaN(when) ? "" : new Intl.DateTimeFormat(s.dateLocale, { dateStyle: "long" }).format(when);
    fill(intro, date ? s.relIntro : s.relIntro.replace(/, [^{]*\{d\}/, ""), {
      v: node("b", "", v), d: date, sums: link(relURL(v + "/SHA256SUMS"), "SHA256SUMS")
    });
  }

  function showRelease(rel) {
    var v = String(rel && rel.version || "");
    if (!/^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$/.test(v)) throw new Error("bad version in latest.json");

    var linux = "dumpcat-" + v + "-linux.tar.gz", windows = "dumpcat-" + v + "-windows-amd64.zip";
    document.getElementById("pkg-linux").href = relURL(v + "/" + linux);
    document.getElementById("pkg-linux-file").textContent = linux;
    document.getElementById("pkg-windows").href = relURL(v + "/" + windows);
    document.getElementById("pkg-windows-file").textContent = windows;
    document.querySelectorAll("a[data-pkg]").forEach(function (a) {
      a.href = relURL(v + "/" + (a.getAttribute("data-pkg") === "windows" ? windows : linux));
    });

    // The What's new card is built from CHANGELOG.md with the site; a release uploaded without
    // deploying the site has no page yet, so the card of an older version is not shown.
    var wn = document.querySelector("[data-wn-version]");
    if (wn) wn.hidden = wn.getAttribute("data-wn-version") !== v;

    var bins = document.getElementById("rel-bins");
    bins.textContent = "";
    Object.keys(rel.files || {}).sort().forEach(function (key) {
      var f = rel.files[key];
      if (!f || typeof f.url !== "string") return;
      var href = relURL(f.url);
      var li = node("li");
      li.appendChild(node("b", "", PLATFORMS[key] || key));
      li.appendChild(link(href, href.slice(href.lastIndexOf("/") + 1)));
      li.appendChild(node("span", "size", typeof f.size === "number" && f.size > 0 ? (f.size / 1e6).toFixed(1) + " MB" : ""));
      if (/^[0-9a-f]{64}$/i.test(f.sha256 || "")) li.appendChild(node("span", "sha", "SHA-256 " + f.sha256.toLowerCase()));
      bins.appendChild(li);
    });

    release = { version: v, released_at: rel.released_at };
    document.getElementById("release").hidden = false;
    renderRelease();
  }

  fetch(relFeed.href, { credentials: "omit" })
    .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status + " for " + relFeed.href); return r.json(); })
    .then(showRelease)
    .catch(function (err) {
      releaseFailed = true;
      renderRelease();
      if (window.console) console.warn("dumpcat.cc: " + err);
    });

  // ---- install tabs ----
  var tabs = Array.prototype.slice.call(document.querySelectorAll('[role="tab"]'));
  function selectTab(tab) {
    tabs.forEach(function (x) {
      var on = x === tab;
      x.setAttribute("aria-selected", String(on));
      x.tabIndex = on ? 0 : -1;
      document.getElementById(x.getAttribute("aria-controls")).hidden = !on;
    });
  }
  tabs.forEach(function (tab, i) {
    tab.addEventListener("click", function () { selectTab(tab); });
    tab.addEventListener("keydown", function (e) {
      if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
      var next = tabs[(i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
      selectTab(next); next.focus();
    });
  });

  // ---- comparison: one tool at a time next to DumpCat ----
  document.querySelectorAll(".lc").forEach(function (box) {
    box.querySelectorAll(".lc-chip").forEach(function (chip) {
      chip.addEventListener("click", function () {
        var tool = chip.getAttribute("data-tool");
        box.querySelectorAll(".lc-chip").forEach(function (c) { c.setAttribute("aria-pressed", String(c === chip)); });
        box.querySelectorAll("[data-tool]:not(.lc-chip)").forEach(function (e) { e.hidden = e.getAttribute("data-tool") !== tool; });
      });
    });
  });

  // ---- copy buttons ----
  document.querySelectorAll(".copy").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var code = document.getElementById(btn.getAttribute("data-target"));
      var text = code.textContent;
      function flash(msg) { btn.textContent = msg; setTimeout(function () { btn.textContent = STR[lang].copy; }, 1600); }
      function selectIt() {
        var r = document.createRange(); r.selectNodeContents(code);
        var sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(r);
        flash(STR[lang].selected);
      }
      try {
        navigator.clipboard.writeText(text).then(function () { flash(STR[lang].copied); }, selectIt);
      } catch (e) { selectIt(); }
    });
  });

  render();
  renderRelease();
})();
