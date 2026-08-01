// Client-side unified-diff renderer bootstrap. Fetches the raw diff
// bytes for this paste (?raw=1 forces the raw branch on the server),
// renders them with diff2html, and syntax-highlights the code with
// highlight.js. No server-side diffing: the server only streamed us the
// fixed shell + the raw bytes. Mirrors mdshell/md.js.
(function () {
  var STORE_KEY = "hostthis:diff:format";
  var FORMATS = { "line-by-line": 1, "side-by-side": 1 };

  // The persisted layout choice (line-by-line default). localStorage can
  // throw (private mode / disabled), so guard every access.
  function loadFormat() {
    try {
      var v = window.localStorage.getItem(STORE_KEY);
      if (v && FORMATS[v]) return v;
    } catch (e) {}
    return "line-by-line";
  }
  function saveFormat(v) {
    try {
      window.localStorage.setItem(STORE_KEY, v);
    } catch (e) {}
  }

  var DL = window.HostthisDeepLink;
  var mount = document.getElementById("diff");
  var btnLine = document.getElementById("btn-line");
  var btnSide = document.getElementById("btn-side");
  var diffText = null;
  var format = loadFormat();

  function syncButtons() {
    var lineActive = format === "line-by-line";
    btnLine.setAttribute("aria-pressed", String(lineActive));
    btnSide.setAttribute("aria-pressed", String(!lineActive));
    btnLine.classList.toggle("active", lineActive);
    btnSide.classList.toggle("active", !lineActive);
  }

  function render() {
    if (diffText === null) return;
    var ui = new Diff2HtmlUI(
      mount,
      diffText,
      {
        drawFileList: true,
        matching: "lines",
        outputFormat: format,
        highlight: true,
        // 'auto' tags the output with .d2h-auto-color-scheme so
        // diff2html's prefers-color-scheme:dark rules apply.
        colorScheme: "auto",
      },
      window.hljs
    );
    ui.draw();
    ui.highlightCode();
    anchorLines();
    reapplyHighlight(false);
    mount.setAttribute("aria-busy", "false");
  }

  // Line anchors are numbered over the LINE-BY-LINE rendering: side-by-side
  // splits one logical line across two rows, so the same ordinal would point
  // somewhere else and a shared link would land on the wrong line. Opening a
  // #L link therefore switches to line-by-line rather than guessing.
  function anchorLines() {
    if (format !== "line-by-line") return;
    var n = 0;
    mount.querySelectorAll("tr").forEach(function (tr) {
      var num = tr.querySelector("td.d2h-code-linenumber");
      var code = tr.querySelector(".d2h-code-line");
      // A hunk header is a row but not a line of content, so numbering it
      // would make every anchor after it drift by one per hunk.
      if (!num || !code || num.classList.contains("d2h-info")) return;
      n++;
      var at = n;
      tr.id = "L" + at;
      num.classList.add("linkable");
      num.title = "link to this line \u2014 shift-click for a range";
      num.addEventListener("click", function (e) {
        e.preventDefault();
        selectLine(at, e.shiftKey);
      });
    });
    lineCount = n;
  }

  // anchorStart remembers the last single-line click so a shift-click extends
  // from it, the selection model people already know from file listings.
  var anchorStart = null;
  var lineCount = 0;

  function selectLine(n, extend) {
    var a = extend && anchorStart ? anchorStart : n;
    var b = n;
    if (!extend) anchorStart = n;
    var lo = Math.min(a, b), hi = Math.max(a, b);
    DL.setHash(lo === hi ? "L" + lo : "L" + lo + "-L" + hi);
    highlight(lo, hi, true);
  }

  function highlight(lo, hi, scroll) {
    mount.querySelectorAll("tr.dl-line").forEach(function (tr) {
      tr.classList.remove("dl-line");
    });
    var first = null;
    for (var i = lo; i <= hi; i++) {
      var tr = document.getElementById("L" + i);
      if (!tr) continue;
      tr.classList.add("dl-line");
      if (!first) first = tr;
    }
    if (scroll && first) first.scrollIntoView({ block: "center", behavior: "smooth" });
  }

  // Re-applies the addressed range after a render, because a render rebuilds
  // the rows and drops the highlight with them.
  //
  // It does NOT change the format. Forcing line-by-line here would fire on a
  // format switch too, so a reader with a line link in the URL would press
  // side-by-side and be yanked straight back - a button that looks broken.
  function reapplyHighlight(scroll) {
    var t = DL.parse();
    if (!t || t.type !== "line" || format !== "line-by-line") return;
    anchorStart = t.from;
    highlight(t.from, t.to, scroll);
  }

  // Resolving a fragment is the one moment the format may be overridden: the
  // ordinals are only meaningful in line-by-line, so a link that arrives while
  // side-by-side is showing has to switch to be honoured at all.
  function resolveHash() {
    var t = DL.parse();
    if (!t || t.type !== "line") return;
    if (format !== "line-by-line") {
      pick("line-by-line"); // re-renders, and reapplyHighlight runs with it
      return;
    }
    reapplyHighlight(true);
  }

  function pick(next) {
    if (!FORMATS[next] || next === format) return;
    format = next;
    saveFormat(format);
    syncButtons();
    render();
  }

  // A fragment arriving later (someone pastes a link into the same tab) still
  // resolves, and re-resolves on every hashchange.
  DL.onResolve(function () { resolveHash(); });

  btnLine.addEventListener("click", function () { pick("line-by-line"); });
  btnSide.addEventListener("click", function () { pick("side-by-side"); });
  syncButtons();

  fetch(location.pathname + "?raw=1")
    .then(function (resp) {
      if (!resp.ok) throw new Error("fetch failed");
      return resp.text();
    })
    .then(function (text) {
      diffText = text;
      render();
    })
    .catch(function () {
      mount.setAttribute("aria-busy", "false");
      mount.textContent = "Failed to load paste.";
    });
})();
