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
  //
  // Those two numbering schemes collide: a line deleted at old-17 and another
  // added at new-17 both read "17" in the same file. A duplicate id is not just
  // invalid, it is unreachable - getElementById only ever returns the first, so
  // one of the rows can neither be linked nor lit. Deletions therefore carry a
  // "d", leaving the bare number to mean the new file, which is what a reader
  // shares. The suffix stays out of the fragment grammar; it is resolved here.
  var fileRows = {};

  function rowFor(file, n) {
    return document.getElementById("F" + file + "L" + n) ||
      document.getElementById("F" + file + "L" + n + "d");
  }

  function anchorLines() {
    if (format !== "line-by-line") return;
    fileRows = {};
    var fileIdx = 0;
    mount.querySelectorAll(".d2h-file-wrapper").forEach(function (file) {
      fileIdx++;
      var fi = fileIdx;
      var ordered = fileRows[fi] = [];
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
        var del = !!tr.querySelector("td.d2h-del");
        tr.id = "F" + fi + "L" + line + (del ? "d" : "");
        ordered.push(tr);
        cells.forEach(function (c) {
          c.classList.add("linkable");
          c.title = "link to this line; shift-click for a range";
          c.addEventListener("click", function (e) {
            e.preventDefault();
            // On touch a second tap extends, because there is no shift key.
            // On a mouse it replaces, which is what a plain click means
            // everywhere else and what shift-click exists to override.
            var extend = e.shiftKey;
            if (!extend && lastTouch && anchorStart && anchorStart.file === fi &&
                anchorStart.tr !== tr) {
              extend = true;
            }
            selectLine(fi, line, tr, extend);
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

  // Touch has no shift key, so a range would be unreachable on a phone. The
  // pointer type is read from the event that STARTED the click rather than
  // from a media query, so a hybrid laptop behaves per-input rather than being
  // classified once at load.
  var lastTouch = false;
  document.addEventListener("pointerdown", function (e) {
    lastTouch = e.pointerType === "touch" || e.pointerType === "pen";
  }, true);

  // anchorStart remembers the last single-line click so a shift-click extends
  // from it, the selection model file listings already use. It carries the file
  // too: a range spanning two files has no meaning here.
  var anchorStart = null;

  // clearSelection drops the highlight AND the fragment, so a URL copied after
  // deselecting does not still carry a range.
  function clearSelection() {
    clearMarks();
    anchorStart = null;
    DL.clearHash();
  }

  // Shown at most once per load: a reader who has already made a range knows.
  var hinted = false;

  function hintOnce() {
    if (hinted) return;
    hinted = true;
    var el = document.createElement("div");
    el.id = "dl-hint";
    el.textContent = "tap another line to extend";
    document.body.appendChild(el);
    requestAnimationFrame(function () { el.classList.add("on"); });
    setTimeout(function () {
      el.classList.remove("on");
      setTimeout(function () { el.remove(); }, 400);
    }, 2600);
  }

  function clearMarks() {
    mount.querySelectorAll("tr.dl-line, tr.dl-arrive").forEach(function (tr) {
      tr.classList.remove("dl-line", "dl-arrive");
    });
  }

  function selectLine(file, line, tr, extend) {
    // Tapping a line that is already IN the selection clears it. On a mouse
    // that only matters for a single line (Escape handles the rest), but on
    // touch there is no Escape, so it is the only way out of a range.
    if (tr.classList.contains("dl-line")) {
      var lit = mount.querySelectorAll("tr.dl-line");
      if (lit.length === 1 || lastTouch) {
        clearSelection();
        return;
      }
    }
    var from = extend && anchorStart && anchorStart.file === file ? anchorStart : null;
    if (!from) {
      anchorStart = { file: file, line: line, tr: tr };
      // A second tap extending the range is not a gesture anyone guesses, and
      // the tooltip that says so on a mouse never appears without hover.
      if (lastTouch) hintOnce();
    }
    paint(file, from ? from.tr : tr, tr, false, false);
  }

  // Endpoints are ROWS, not line numbers. Consecutive gutter numbers are not
  // consecutive rows across a deletion, so walking the numbers would skip the
  // deleted rows a reader dragged over and light a range with holes in it.
  function paint(file, a, b, scroll, arrived) {
    clearMarks();
    var all = fileRows[file] || [];
    var i = all.indexOf(a), j = all.indexOf(b);
    if (i < 0 || j < 0) return;
    if (i > j) { var t = i; i = j; j = t; }
    var rows = all.slice(i, j + 1);
    rows.forEach(function (tr) { tr.classList.add("dl-line"); });
    var lo = lineOf(rows[0]), hi = lineOf(rows[rows.length - 1]);
    DL.setHash("F" + file + "L" + lo + (lo === hi ? "" : "-L" + hi));
    // Arrival drives BOTH the pulse and the scroll, and both hang off the
    // pending flag rather than the caller. On a cold load the resolver runs
    // before the ?raw fetch returns, so the first highlight comes from the
    // render path with scroll=false - passing the intent from the resolver
    // alone left a deep link opening at the top of the document.
    if (arrived && pendingArrival) {
      pendingArrival = false;
      rows.forEach(function (tr) { tr.classList.add("dl-arrive"); });
      rows[0].scrollIntoView({ block: "center", behavior: "smooth" });
    } else if (scroll) {
      rows[0].scrollIntoView({ block: "center", behavior: "smooth" });
    }
  }

  function lineOf(tr) {
    return parseInt(tr.id.replace(/^F\d+L/, ""), 10);
  }

  // Resolves a fragment range onto rows. A line absent from the diff has no
  // row; the range is clamped to the rows that exist rather than dropped, so a
  // link written by hand around a hunk still lands somewhere sensible.
  function highlight(file, lo, hi, scroll, arrived) {
    var all = fileRows[file] || [];
    var a = null, b = null;
    for (var i = 0; i < all.length; i++) {
      var n = lineOf(all[i]);
      if (n >= lo && n <= hi) {
        if (!a) a = all[i];
        b = all[i];
      }
    }
    if (!a) return;
    anchorStart = { file: file, line: lineOf(a), tr: a };
    paint(file, a, b, scroll, arrived);
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

  // Escape clears from anywhere, which is what a reader reaches for and the
  // only route out of a multi-line range short of re-clicking its first line.
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && mount.querySelector("tr.dl-line")) {
      clearSelection();
    }
  });

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
