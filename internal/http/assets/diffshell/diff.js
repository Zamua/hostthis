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
    mount.setAttribute("aria-busy", "false");
  }

  function pick(next) {
    if (!FORMATS[next] || next === format) return;
    format = next;
    saveFormat(format);
    syncButtons();
    render();
  }

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
