// Install guide (/install/, /vi/install/): copy buttons, and download links that follow the latest release.
(function () {
  "use strict";
  var vi = document.documentElement.lang === "vi";
  var S = vi ? { copy: "Sao chép", copied: "Đã chép", selected: "Đã chọn, nhấn Ctrl+C" }
             : { copy: "Copy", copied: "Copied", selected: "Selected, press Ctrl+C" };

  document.querySelectorAll(".copy").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var el = document.getElementById(btn.getAttribute("data-target"));
      if (!el) return;
      var text = el.textContent;
      function flash(msg) { btn.textContent = msg; setTimeout(function () { btn.textContent = S.copy; }, 1600); }
      function selectIt() {
        var r = document.createRange(); r.selectNodeContents(el);
        var sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(r);
        flash(S.selected);
      }
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(function () { flash(S.copied); }, selectIt);
      } else {
        selectIt();
      }
    });
  });

  // The links point at /#download until the release feed answers; then straight at the package.
  var base = "https://downloads.dumpcat.cc/";
  fetch(base + "latest.json", { cache: "no-cache" }).then(function (r) {
    if (!r.ok) throw new Error(r.status);
    return r.json();
  }).then(function (rel) {
    var v = String(rel && rel.version || "");
    if (!/^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$/.test(v)) return;
    var files = { windows: "dumpcat-" + v + "-windows-amd64.zip", linux: "dumpcat-" + v + "-linux.tar.gz" };
    document.querySelectorAll("a[data-pkg]").forEach(function (a) {
      var f = files[a.getAttribute("data-pkg")];
      if (f) a.href = base + v + "/" + f;
    });
    document.querySelectorAll("[data-version-label]").forEach(function (e) { e.textContent = v; });
  }).catch(function () { /* keep the /#download links */ });
})();
