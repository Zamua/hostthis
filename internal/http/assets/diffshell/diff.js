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

  // Anchors carry the line number the reader can SEE in the gutter, scoped by
  // file. An earlier version numbered content rows sequentially, which is
  // unambiguous but means #L75 highlights the row whose gutter reads 180 - the
  // link contradicts the page.
  //
  // The number is the new-file line for context and added lines, and the
  // old-file line for deletions, which is the only number those rows show.
  // F<n> scopes it because the same line number occurs in every file.
  function anchorLines() {
    if (format !== "line-by-line") return;
    var fileIdx = 0;
    mount.querySelectorAll(".d2h-file-wrapper").forEach(function (file) {
      fileIdx++;
      var fi = fileIdx;
      file.querySelectorAll("tr").forEach(function (tr) {
        var cells = tr.querySelectorAll("td.d2h-code-linenumber");
        var code = tr.querySelector(".d2h-code-line");
        // A hunk header is a row but not a line of content.
        if (!cells.length || !code || cells[0].classList.contains("d2h-info")) return;
        var nums = [];
        cells.forEach(function (c) {
          c.querySelectorAll("*").forEach(function (n) {
            var v = parseInt(n.innerText.trim(), 10);
            if (!isNaN(v)) nums.push(v);
          });
          if (!c.children.length) {
            var v = parseInt(c.innerText.trim(), 10);
            if (!isNaN(v)) nums.push(v);
          }
        });
        if (!nums.length) return;
        // Prefer the LAST number: line-by-line prints old then new, so the new
        // one is the number a reader tracking the result cares about.
        var line = nums[nums.length - 1];
        tr.id = "F" + fi + "L" + line;
        cells.forEach(function (c) {
          c.classList.add("linkable");
          c.title = "link to this line \u2014 shift-click for a range";
          c.addEventListener("click", function (e) {
            e.preventDefault();
            selectLine(fi, line, e.shiftKey);
          });
        });
      });
    });
  }

  // The resolver runs before the fetch completes, so on a cold load the first
  // highlight comes from render(), not from resolveHash(). Arrival is tracked
  // as a pending flag and consumed by whichever highlight lands first;
  // otherwise the pulse is applied to an empty document and then cleared.
  var pendingArrival = true;

  // anchorStart remembers the last single-line click so a shift-click extends
  // from it, the selection model file listings already use. It carries the file
  // too: a range spanning two files has no meaning here.
  var anchorStart = null;

  function selectLine(file, line, extend) {
    var start = extend && anchorStart && anchorStart.file === file ? anchorStart.line : line;
    if (!extend || !anchorStart || anchorStart.file !== file) {
      anchorStart = { file: file, line: line };
    }
    var lo = Math.min(start, line), hi = Math.max(start, line);
    DL.setHash("F" + file + "L" + lo + (lo === hi ? "" : "-L" + hi));
    highlight(file, lo, hi, true, false);
  }

  function highlight(file, lo, hi, scroll, arrived) {
    mount.querySelectorAll("tr.dl-line, tr.dl-first, tr.dl-last, tr.dl-arrive")
      .forEach(function (tr) {
        tr.classList.remove("dl-line", "dl-first", "dl-last", "dl-arrive");
      });
    var rows = [];
    for (var i = lo; i <= hi; i++) {
      var tr = document.getElementById("F" + file + "L" + i);
      if (!tr) continue; // a line absent from the diff simply has no row
      tr.classList.add("dl-line");
      rows.push(tr);
    }
    if (!rows.length) return;
    // The bounds are marked on the rows that actually exist, not on lo and hi:
    // a range whose first line is absent from the diff would otherwise draw its
    // top rule nowhere.
    rows[0].classList.add("dl-first");
    rows[rows.length - 1].classList.add("dl-last");
    if (arrived && pendingArrival) {
      pendingArrival = false;
      rows.forEach(function (tr) { tr.classList.add("dl-arrive"); });
    }
    if (scroll) rows[0].scrollIntoView({ block: "center", behavior: "smooth" });
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
    anchorStart = { file: t.file, line: t.from };
    // A re-render must not re-pulse, but the FIRST paint after a cold load is
    // an arrival, so the flag decides rather than the call site.
    highlight(t.file, t.from, t.to, scroll, true);
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
    pendingArrival = true; // a hashchange is a new arrival
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
