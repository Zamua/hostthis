// Viewer for csv/tsv and json/jsonl pastes.
//
// One shell for both kinds: the sort, the filter, the deep links, and the SQL
// console are identical, and the two differ only in how bytes become rows.
//
// Everything except SQL is computed here in plain JS. DuckDB is ~7 MB gzipped
// and is fetched ONLY when the console is opened, so the default view costs
// nothing extra.
(function () {
  "use strict";
  var DL = window.HostthisDeepLink;
  var content = document.getElementById("content");
  var summary = document.getElementById("summary");
  var filterEl = document.getElementById("filter");
  var sqlBtn = document.getElementById("sqlbtn");

  var views = document.getElementById("views");

  // ONE content region holds either the source table or the query result. They
  // are both tables; stacking them moved the whole layout every time a query
  // ran, which is what made the page feel unsettled.
  var state = { rows: [], cols: [], sort: null, kind: null, raw: "", view: "data", result: null };

  function setView(v) {
    state.view = v;
    Array.prototype.forEach.call(views.querySelectorAll("button"), function (b) {
      b.classList.toggle("on", b.dataset.view === v);
    });
    render();
  }

  // render draws whichever view is active into the shared region.
  function render() {
    if (state.view === "result" && state.result) {
      content.innerHTML = "";
      content.appendChild(state.result.node);
      summary.textContent = state.result.summary;
      return;
    }
    if (state.kind === "json") { renderTree(); return; }
    renderTable();
  }

  function fail(msg) {
    content.innerHTML = "";
    var d = document.createElement("div");
    d.className = "err";
    d.textContent = msg;
    content.appendChild(d);
  }

  // ---- csv ---------------------------------------------------------------

  // RFC 4180 parse, done character-wise rather than by splitting: a split on
  // the delimiter tears any field that legitimately contains one, and a split
  // on newline tears any field containing a line break.
  function parseDelimited(text, delim) {
    var rows = [], row = [], field = "", quoted = false, i = 0;
    while (i < text.length) {
      var c = text[i];
      if (quoted) {
        if (c === '"') {
          if (text[i + 1] === '"') { field += '"'; i += 2; continue; }
          quoted = false; i++; continue;
        }
        field += c; i++; continue;
      }
      if (c === '"' && field === "") { quoted = true; i++; continue; }
      if (c === delim) { row.push(field); field = ""; i++; continue; }
      if (c === "\r") { i++; continue; }
      if (c === "\n") { row.push(field); rows.push(row); row = []; field = ""; i++; continue; }
      field += c; i++;
    }
    if (field !== "" || row.length) { row.push(field); rows.push(row); }
    return rows;
  }

  function pickDelimiter(text) {
    var head = text.slice(0, 4096);
    var tabs = (head.match(/\t/g) || []).length;
    var commas = (head.match(/,/g) || []).length;
    return tabs > commas ? "\t" : ",";
  }

  // ---- typing + stats ----------------------------------------------------

  function isNum(v) {
    return v !== "" && v != null && isFinite(Number(String(v).replace(/,/g, "")));
  }
  function asNum(v) { return Number(String(v).replace(/,/g, "")); }

  // A column is numeric only when EVERY non-blank value is, so one stray label
  // keeps the column textual instead of silently coercing it to NaN.
  function columnType(rows, idx) {
    var seen = 0;
    for (var r = 0; r < rows.length; r++) {
      var v = rows[r][idx];
      if (v === "" || v == null) continue;
      seen++;
      if (!isNum(v)) return "text";
    }
    return seen ? "number" : "empty";
  }

  function statsFor(rows, idx, type) {
    var nulls = 0, distinct = new Set(), min = Infinity, max = -Infinity;
    for (var r = 0; r < rows.length; r++) {
      var v = rows[r][idx];
      if (v === "" || v == null) { nulls++; continue; }
      distinct.add(v);
      if (type === "number") {
        var n = asNum(v);
        if (n < min) min = n;
        if (n > max) max = n;
      }
    }
    return {
      nulls: nulls,
      distinct: distinct.size,
      min: type === "number" && isFinite(min) ? min : null,
      max: type === "number" && isFinite(max) ? max : null,
    };
  }

  // describe renders a column's facts as one short line for its header. Nulls
  // are named only when there are some: "0 null" on every column is noise that
  // makes the one column that HAS nulls harder to spot.
  function describe(st, type) {
    var parts = [];
    if (type === "number" && st.min != null) {
      parts.push(fmtNum(st.min) + " – " + fmtNum(st.max));
    } else {
      parts.push(st.distinct + " distinct");
    }
    if (st.nulls) parts.push(st.nulls + " null");
    return parts.join(" · ");
  }

  function fmtNum(n) {
    if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
    if (Math.abs(n) >= 1e4) return (n / 1e3).toFixed(1).replace(/\.0$/, "") + "k";
    return String(n);
  }

  // ---- table -------------------------------------------------------------

  function renderTable() {
    var q = filterEl.value.trim().toLowerCase();
    var rows = state.rows;
    if (q) {
      rows = rows.filter(function (r) {
        return r.some(function (v) { return String(v).toLowerCase().indexOf(q) !== -1; });
      });
    }
    if (state.sort) {
      var i = state.sort.col, dir = state.sort.dir, numeric = state.cols[i].type === "number";
      rows = rows.slice().sort(function (a, b) {
        var x = a[i], y = b[i];
        if (x === y) return 0;
        if (x === "" || x == null) return 1;   // blanks sink, in both directions
        if (y === "" || y == null) return -1;
        var c = numeric ? asNum(x) - asNum(y) : String(x).localeCompare(String(y));
        return dir === "asc" ? c : -c;
      });
    }

    var wrap = document.createElement("div");
    wrap.className = "tablewrap";
    var t = document.createElement("table");

    var thead = document.createElement("thead");
    var htr = document.createElement("tr");
    var rn = document.createElement("th");
    rn.className = "rn";
    rn.textContent = "#";
    htr.appendChild(rn);
    state.cols.forEach(function (c, i) {
      var th = document.createElement("th");
      var arrow = state.sort && state.sort.col === i ? "▲" : "";
      if (state.sort && state.sort.col === i && state.sort.dir === "desc") arrow = "▼";
      th.innerHTML =
        '<span class="name"></span><span class="arrow"></span>' +
        '<span class="type"></span><span class="facts"></span>';
      th.querySelector(".name").textContent = c.name;
      th.querySelector(".arrow").textContent = arrow;
      th.querySelector(".type").textContent = c.type;
      th.querySelector(".facts").textContent = c.facts;
      if (c.type === "number") {
        var bar = document.createElement("span");
        bar.className = "spark";
        bar.innerHTML = "<i></i>";
        th.appendChild(bar);
      }
      th.onclick = function () {
        var asc = !(state.sort && state.sort.col === i && state.sort.dir === "asc");
        state.sort = { col: i, dir: asc ? "asc" : "desc" };
        renderTable();
      };
      htr.appendChild(th);
    });
    thead.appendChild(htr);
    t.appendChild(thead);

    var tb = document.createElement("tbody");
    rows.forEach(function (r) {
      var tr = document.createElement("tr");
      tr.id = "row-" + r.__n;
      var n = document.createElement("td");
      n.className = "rn";
      n.textContent = r.__n;
      n.title = "link to this row";
      n.onclick = function () {
        DL.setHash("row=" + r.__n);
        DL.reveal(tr);
      };
      tr.appendChild(n);
      state.cols.forEach(function (c, i) {
        var td = document.createElement("td");
        var v = r[i];
        if (v === "" || v == null) { td.className = "null"; td.textContent = "—"; }
        else { if (c.type === "number") td.className = "n"; td.textContent = v; }
        tr.appendChild(td);
      });
      tb.appendChild(tr);
    });
    t.appendChild(tb);
    wrap.appendChild(t);

    content.innerHTML = "";
    content.appendChild(wrap);
    summary.textContent =
      rows.length + (q ? " of " + state.rows.length : "") + " rows · " + state.cols.length + " cols";
  }

  // ---- json tree ---------------------------------------------------------

  function treeNode(key, val, depth) {
    var wrap = document.createElement(typeof val === "object" && val !== null ? "details" : "div");
    if (wrap.tagName === "DETAILS" && depth < 2) wrap.open = true;
    var head = document.createElement(wrap.tagName === "DETAILS" ? "summary" : "span");
    head.className = "row";
    if (key !== null) {
      var k = document.createElement("span");
      k.className = "k";
      k.textContent = JSON.stringify(key) + ": ";
      head.appendChild(k);
    }
    var v = document.createElement("span");
    if (val === null) { v.className = "nul"; v.textContent = "null"; }
    else if (Array.isArray(val)) { v.textContent = "[" + val.length + "]"; }
    else if (typeof val === "object") { v.textContent = "{" + Object.keys(val).length + "}"; }
    else if (typeof val === "number") { v.className = "num"; v.textContent = String(val); }
    else if (typeof val === "boolean") { v.className = "b"; v.textContent = String(val); }
    else { v.className = "s"; v.textContent = JSON.stringify(val); }
    head.appendChild(v);
    wrap.appendChild(head);

    if (typeof val === "object" && val !== null) {
      var keys = Array.isArray(val) ? val.map(function (_, i) { return i; }) : Object.keys(val);
      keys.forEach(function (k2) { wrap.appendChild(treeNode(Array.isArray(val) ? null : k2, val[k2], depth + 1)); });
    }
    return wrap;
  }

  function renderTree() {
    var value = state.json, count = state.jsonCount;
    var q = filterEl.value.trim().toLowerCase();
    var host = document.createElement("div");
    host.className = "tree";
    host.appendChild(treeNode(null, value, 0));
    content.innerHTML = "";
    content.appendChild(host);
    summary.textContent = count;

    if (!q) return;
    // Filtering a tree by hiding nodes hides the path to a match, so matches
    // are highlighted and their ancestors opened instead.
    var hit = 0;
    host.querySelectorAll(".row").forEach(function (row) {
      if (row.textContent.toLowerCase().indexOf(q) === -1) return;
      hit++;
      row.querySelectorAll("span").forEach(function (s) {
        if (s.textContent.toLowerCase().indexOf(q) === -1) return;
        var i = s.textContent.toLowerCase().indexOf(q);
        var t = s.textContent;
        s.innerHTML = "";
        s.append(t.slice(0, i));
        var m = document.createElement("mark");
        m.textContent = t.slice(i, i + q.length);
        s.append(m, t.slice(i + q.length));
      });
      for (var p = row.parentElement; p; p = p.parentElement) {
        if (p.tagName === "DETAILS") p.open = true;
      }
    });
    summary.textContent = count + " · " + hit + " matches";
  }

  // ---- sql (lazy) --------------------------------------------------------

  var db = null;
  async function initDuck(statusEl) {
    if (db) return db;
    statusEl.textContent = "loading engine (~7 MB, once)…";
    var duckdb = await import("/_hostthis/duckdb-browser.mjs");
    var bundle = {
      mainModule: "/_hostthis/duckdb-eh.wasm",
      mainWorker: "/_hostthis/duckdb-browser-eh.worker.js",
    };
    var worker = new Worker(bundle.mainWorker);
    var logger = new duckdb.ConsoleLogger(duckdb.LogLevel.WARNING);
    var inst = new duckdb.AsyncDuckDB(logger, worker);
    await inst.instantiate(bundle.mainModule);
    var conn = await inst.connect();

    // The paste's own bytes are registered as a file so the query reads the
    // real content, not a re-serialisation of the parsed rows.
    await inst.registerFileText("data", state.raw);
    var from = state.kind === "json" ? "read_json_auto('data')" : "read_csv_auto('data')";
    await conn.query("CREATE OR REPLACE VIEW data AS SELECT * FROM " + from);
    db = { inst: inst, conn: conn };
    statusEl.textContent = "";
    return db;
  }

  // The editor is lazy for the same reason the engine is: a reader who never
  // opens the console should not download either. CodeMirror must be ONE
  // bundle - two copies of @codemirror/state make it throw.
  var editor = null;

  async function mountEditor(host, initial, onRun) {
    var cm;
    try {
      cm = await import("/_hostthis/codemirror.min.js");
    } catch (e) {
      // A fallback textarea keeps the console usable rather than dead.
      var ta = document.createElement("textarea");
      ta.className = "fallback";
      ta.rows = 3;
      ta.value = initial;
      host.appendChild(ta);
      return { value: function () { return ta.value; }, focus: function () { ta.focus(); } };
    }

    // Schema-aware completion: the column names are already known, so the
    // editor completes them instead of offering generic SQL keywords only.
    var schema = { data: state.cols.map(function (c) { return c.name; }) };

    var vimCompartment = new cm.Compartment();
    var wantVim = localStorage.getItem("hostthis.vim") === "1";
    var dark = matchMedia("(prefers-color-scheme: dark)").matches;

    var view = new cm.EditorView({
      doc: initial,
      parent: host,
      // Order IS precedence in CodeMirror: earlier extensions win. vim must
      // precede the default keymap, and Mod-Enter must precede basicSetup,
      // whose defaultKeymap already binds it to insertBlankLine.
      extensions: [
        vimCompartment.of(wantVim ? cm.vim() : []),
        cm.keymap.of([
          { key: "Mod-Enter", run: function () { onRun(); return true; }, preventDefault: true },
        ]),
        cm.basicSetup,
        cm.sql({ dialect: cm.PostgreSQL, schema: schema, upperCaseKeywords: true }),
      ].concat(dark ? [cm.oneDark] : []),
    });

    var toggle = document.getElementById("vim");
    toggle.checked = wantVim;
    toggle.onchange = function () {
      localStorage.setItem("hostthis.vim", toggle.checked ? "1" : "0");
      view.dispatch({ effects: vimCompartment.reconfigure(toggle.checked ? cm.vim() : []) });
      view.focus();
    };

    return {
      value: function () { return view.state.doc.toString(); },
      focus: function () { view.focus(); },
      // Running with the completion list still open leaves it floating over
      // the results it just produced.
      dismiss: function () { cm.closeCompletion(view); },
    };
  }

  function buildResult(table) {
    var cols = table.schema.fields.map(function (f) { return f.name; });
    var t = document.createElement("table");
    var thead = document.createElement("thead");
    var htr = document.createElement("tr");
    cols.forEach(function (c) {
      var th = document.createElement("th");
      th.textContent = c;
      htr.appendChild(th);
    });
    thead.appendChild(htr);
    t.appendChild(thead);
    var tb = document.createElement("tbody");
    for (var row of table) {
      var tr = document.createElement("tr");
      cols.forEach(function (c) {
        var td = document.createElement("td");
        var v = row[c];
        if (v === null || v === undefined) { td.className = "null"; td.textContent = "—"; }
        else {
          var str = typeof v === "bigint" ? v.toString() : String(v);
          if (typeof v === "number" || typeof v === "bigint") td.className = "n";
          td.textContent = str;
        }
        tr.appendChild(td);
      });
      tb.appendChild(tr);
    }
    t.appendChild(tb);
    var wrap = document.createElement("div");
    wrap.className = "tablewrap";
    wrap.appendChild(t);
    return wrap;
  }

  function wireSQL() {
    var panel = document.getElementById("sql");
    var host = document.getElementById("editor");
    var run = document.getElementById("run");
    var status = document.getElementById("sqlstatus");

    Array.prototype.forEach.call(views.querySelectorAll("button"), function (b) {
      b.onclick = function () { setView(b.dataset.view); };
    });

    sqlBtn.hidden = false;
    sqlBtn.setAttribute("aria-expanded", "false");

    async function execute() {
      if (!editor) return;
      try {
        run.disabled = true;
        if (editor.dismiss) editor.dismiss();
        var d = await initDuck(status);
        status.textContent = "running…";
        var t0 = performance.now();
        var res = await d.conn.query(editor.value());
        var ms = Math.round(performance.now() - t0);
        status.textContent = res.numRows + " rows · " + ms + " ms";
        state.result = {
          node: buildResult(res),
          summary: res.numRows + " rows · " + ms + " ms",
        };
        views.hidden = false;
        views.querySelector('[data-view="result"]').textContent = "Result · " + res.numRows;
        setView("result");
      } catch (e) {
        status.textContent = "";
        var err = document.createElement("div");
        err.className = "err";
        // A wasm-unsupported browser fails here, and the message says which.
        err.textContent = String((e && e.message) || e);
        state.result = { node: err, summary: "query failed" };
        views.hidden = false;
        views.querySelector('[data-view="result"]').textContent = "Result";
        setView("result");
      } finally {
        run.disabled = false;
      }
    }

    sqlBtn.onclick = async function () {
      var opening = panel.hidden;
      panel.hidden = !opening;
      sqlBtn.setAttribute("aria-expanded", String(opening));
      if (!opening) return;
      if (!editor) {
        editor = await mountEditor(host, "SELECT * FROM data LIMIT 20;", execute);
      }
      editor.focus();
    };
    run.onclick = execute;
  }

  // ---- boot --------------------------------------------------------------

  (async function () {
    try {
      var resp = await fetch(location.pathname + "?raw=1");
      if (!resp.ok) return fail("Failed to load paste.");
      var text = await resp.text();
      state.raw = text;

      var isJSON = document.documentElement.dataset.kind === "json" || looksJSON(text);
      state.kind = isJSON ? "json" : "csv";

      if (isJSON) {
        var value, count;
        try {
          value = JSON.parse(text);
          count = Array.isArray(value) ? value.length + " items" : "object";
        } catch (e) {
          // JSONL: one value per line.
          var lines = text.split("\n").filter(function (l) { return l.trim(); });
          value = lines.map(function (l) { return JSON.parse(l); });
          count = value.length + " records";
        }
        state.json = value;
        state.jsonCount = count;
        filterEl.oninput = render;
        render();
      } else {
        var delim = pickDelimiter(text);
        var grid = parseDelimited(text, delim);
        if (!grid.length) return fail("Empty table.");
        var header = grid[0];
        var body = grid.slice(1).filter(function (r) { return r.some(function (c) { return c !== ""; }); });
        body.forEach(function (r, i) { r.__n = i + 1; });
        state.rows = body;
        state.cols = header.map(function (name, i) {
          var type = columnType(body, i);
          var st = statsFor(body, i, type);
          return {
            name: name || "col" + (i + 1),
            type: type,
            facts: describe(st, type),
            min: st.min,
            max: st.max,
          };
        });
        filterEl.oninput = render;
        render();
      }

      wireSQL();

      DL.onResolve(function (t) {
        if (t.type === "row") {
          var el = document.getElementById("row-" + t.n);
          if (el) DL.reveal(el);
        }
      });
    } catch (e) {
      fail("Failed to render.\n\n" + ((e && e.message) || String(e)));
    }
  })();

  function looksJSON(text) {
    var t = text.trim();
    return t.startsWith("{") || t.startsWith("[");
  }
})();
