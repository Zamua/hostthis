// Plain-text viewer: a line-number gutter and citable line ranges, which is
// the whole reason this kind renders at all rather than downloading.
(function () {
  "use strict";

  var DL = window.HostthisDeepLink;
  var mount = document.getElementById("text");
  var meta = document.getElementById("meta");
  var wrap = document.getElementById("wrap");

  // A bare `download` makes the browser derive a filename from the URL path,
  // which at a paste's subdomain root is "/" - the save dialog then offers a
  // file literally called "/". The slug is the only stable name available.
  (function nameDownload() {
    var dl = document.getElementById("dl");
    var isSlug = function (s) { return /^[a-z0-9]{8}$/.test(s); };
    var host = location.hostname.split(".")[0];
    var segs = location.pathname.split("/").filter(Boolean);
    var last = segs[segs.length - 1];
    dl.setAttribute("download", (isSlug(host) ? host : isSlug(last) ? last : "paste") + ".txt");
  })();

  wrap.addEventListener("change", function () {
    mount.classList.toggle("wrap", wrap.checked);
    try { localStorage.setItem("hostthis.text.wrap", wrap.checked ? "1" : ""); } catch (e) {}
  });
  try { wrap.checked = !!localStorage.getItem("hostthis.text.wrap"); } catch (e) {}
  mount.classList.toggle("wrap", wrap.checked);

  fetch(location.pathname + "?raw=1", { credentials: "same-origin" })
    .then(function (r) {
      if (!r.ok) throw new Error("fetch failed: " + r.status);
      return r.text();
    })
    .then(function (text) {
      // A trailing newline is a terminator, not an empty final line; keeping
      // it would draw a numbered blank row every file ends with.
      var lines = text.replace(/\n$/, "").split("\n");
      var frag = document.createDocumentFragment();
      lines.forEach(function (line, i) {
        var row = document.createElement("div");
        row.className = "row";
        row.dataset.line = String(i + 1);
        row.id = "L" + (i + 1);
        var num = document.createElement("span");
        num.className = "lg-num";
        num.textContent = String(i + 1);
        var code = document.createElement("span");
        code.className = "lg-code";
        // An empty line still needs height, and an empty text node has none.
        code.textContent = line === "" ? " " : line;
        row.appendChild(num);
        row.appendChild(code);
        frag.appendChild(row);
      });
      mount.appendChild(frag);
      mount.setAttribute("aria-busy", "false");
      meta.textContent = lines.length + (lines.length === 1 ? " line" : " lines");

      var g = window.HostthisLineGutter.attach(mount, { hint: true });
      // Resolution runs only after the lines exist. The browser already tried
      // the fragment at parse time, against an empty document.
      DL.onResolve(function (t) { g.resolve(t); });
    })
    .catch(function (err) {
      mount.textContent = "could not load: " + err.message;
      mount.setAttribute("aria-busy", "false");
    });
})();
