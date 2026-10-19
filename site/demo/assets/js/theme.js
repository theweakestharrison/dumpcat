// Applies the saved theme before first paint (avoids a flash of the wrong theme).
(function () {
  try {
    var t = localStorage.getItem('dumpcat-theme');
    if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
  } catch (e) { /* storage unavailable */ }
})();
