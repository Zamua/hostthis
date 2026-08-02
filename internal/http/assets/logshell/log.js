// Structured-log viewer for NDJSON.
//
// No log format standardised its field names, so records are normalised on the
// way in against the union of what the common loggers emit. Everything the
// viewer does downstream works on that normal form, not on any one logger's
// shape.
(function () {
  "use strict";

  var DL = window.HostthisDeepLink;
  var mount = document.getElementById("log");
  var levelsBar = document.getElementById("levels");
  var qBox = document.getElementById("q");
  var meta = document.getElementById("meta");

  var TIME_KEYS = ["@timestamp", "timestamp", "ts", "time", "Time", "eventTime"];
  var LEVEL_KEYS = ["level", "severity", "lvl", "levelname", "log.level", "Level"];
  var MSG_KEYS = ["message", "msg", "body", "Body", "event", "log"];

  // syslog/bunyan/pino emit numeric levels; every other logger emits a word.
  var NUM_LEVELS = [
    [60, "fatal"], [50, "error"], [40, "warn"], [30, "info"], [20, "debug"], [10, "trace"],
  ];

  var ORDER = ["fatal", "error", "warn", "info", "debug", "trace", "other"];

  var Q = window.HostthisLogQuery;
  var records = [];
  var query = "";
  var brush = null;  // [fromMs, toMs] selected on the histogram
  var gutter = null;

  function pick(obj, keys) {
    for (var i = 0; i < keys.length; i++) {
      if (obj[keys[i]] !== undefined) return obj[keys[i]];
    }
    return undefined;
  }

  function normLevel(v) {
    if (v === undefined || v === null) return "other";
    if (typeof v === "number") {
      for (var i = 0; i < NUM_LEVELS.length; i++) {
        if (v >= NUM_LEVELS[i][0]) return NUM_LEVELS[i][1];
      }
      return "trace";
    }
    var s = String(v).toLowerCase();
    if (s === "warning") return "warn";
    if (s === "err") return "error";
    if (s === "critical" || s === "crit" || s === "panic") return "fatal";
    return ORDER.indexOf(s) >= 0 ? s : "other";
  }

  // Loki nanosecond strings, epoch seconds, epoch millis and RFC3339 all show
  // up. Anything unrecognised is passed through verbatim rather than guessed
  // at, because a wrong timestamp is worse than an ugly one.
  function normTime(v) {
    if (v === undefined || v === null) return "";
    if (typeof v === "number") {
      var ms = v > 1e14 ? v / 1e6 : v > 1e11 ? v : v * 1000;
      return new Date(ms).toISOString();
    }
    var s = String(v);
    if (/^\d{16,}$/.test(s)) return new Date(Number(s) / 1e6).toISOString();
    return s;
  }

  function isBulkAction(o) {
    var k = Object.keys(o);
    return k.length === 1 && ["index", "create", "update", "delete"].indexOf(k[0]) >= 0;
  }

  // Three container shapes are unwrapped because they are what the tools
  // actually export. Without this a Loki dump renders one row per STREAM and
  // an OpenSearch response renders a single row for the whole response.
  function expand(obj, out) {
    if (obj.stream && Array.isArray(obj.values)) {
      obj.values.forEach(function (v) {
        out.push({ time: normTime(v[0]), level: "other", msg: String(v[1]),
                   fields: obj.stream });
      });
      return;
    }
    if (obj.hits && obj.hits.hits && Array.isArray(obj.hits.hits)) {
      obj.hits.hits.forEach(function (h) { if (h._source) push(h._source, out); });
      return;
    }
    push(obj, out);
  }

  function push(o, out) {
    var lvl = pick(o, LEVEL_KEYS);
    var msg = pick(o, MSG_KEYS);
    out.push({
      time: normTime(pick(o, TIME_KEYS)),
      level: normLevel(lvl),
      msg: msg === undefined ? "" : (typeof msg === "string" ? msg : JSON.stringify(msg)),
      fields: o,
    });
  }

  function parse(text) {
    var out = [];
    text.split("\n").forEach(function (line) {
      line = line.trim();
      if (!line || line[0] !== "{") return;
      var o;
      try { o = JSON.parse(line); } catch (e) { return; }
      if (isBulkAction(o)) return;
      expand(o, out);
    });
    return out;
  }

  // One filtering mechanism, not two: the level chips write into the query,
  // so the URL always describes the whole view.
  function visible() {
    var terms = Q.parse(query);
    return records.filter(function (r) {
      if (brush && (r.at < brush[0] || r.at > brush[1])) return false;
      return Q.match(r, terms);
    });
  }

  function render() {
    var rows = visible();
    mount.textContent = "";
    var frag = document.createDocumentFragment();
    rows.forEach(function (r) {
      var el = document.createElement("div");
      el.className = "rec lv-" + r.level;
      // The line number is the record's ORIGINAL position, so a link keeps
      // meaning after the reader filters. Numbering the filtered view would
      // make #L12 point at a different record for every filter.
      el.dataset.line = String(r.n);
      el.id = "L" + r.n;

      var num = document.createElement("span");
      num.className = "lg-num";
      num.textContent = String(r.n);

      var t = document.createElement("span");
      t.className = "t";
      t.textContent = r.time;

      var lv = document.createElement("span");
      lv.className = "lv";
      lv.textContent = r.level === "other" ? "" : r.level.toUpperCase();

      var m = document.createElement("span");
      m.className = "msg";
      m.textContent = r.msg || r.line;

      el.appendChild(num);
      el.appendChild(t);
      el.appendChild(lv);
      el.appendChild(m);

      // Every field is available, not just the three the columns show; a
      // record whose detail is in its fields is the normal case.
      if (r.extra) {
        var more = document.createElement("span");
        more.className = "extra";
        more.textContent = r.extra;
        el.appendChild(more);
      }
      frag.appendChild(el);
    });
    mount.appendChild(frag);
    mount.setAttribute("aria-busy", "false");
    meta.textContent = rows.length === records.length
      ? records.length + " records"
      : rows.length + " of " + records.length;
    drawHist();
    if (gutter) gutter.resolve(DL.parse(), false);
  }

  function drawLevels() {
    var counts = {};
    records.forEach(function (r) { counts[r.level] = (counts[r.level] || 0) + 1; });
    levelsBar.textContent = "";
    ORDER.forEach(function (lv) {
      if (!counts[lv]) return;
      var b = document.createElement("button");
      b.type = "button";
      b.className = "chip lv-" + lv;
      b.textContent = (lv === "other" ? "other" : lv.toUpperCase()) + " " + counts[lv];
      b.onclick = function () {
        var terms = Q.parse(query);
        var cur = null;
        terms.forEach(function (t) { if (t.op === "=" && t.field === "level") cur = t; });
        var picked = cur ? cur.value.split("|") : [];
        var at = picked.indexOf(lv);
        if (at >= 0) picked.splice(at, 1); else picked.push(lv);
        setQuery(picked.length ? Q.withTerm(query, "level", "=", picked.join("|"))
                               : Q.withoutField(query, "level"));
      };
      levelsBar.appendChild(b);
    });
  }

  function setQuery(q) {
    query = q;
    qBox.value = q;
    render();
    writeHash();
  }

  // The whole view is the fragment: a filtered, time-boxed log is a link.
  // A paste has nowhere to save a search and needs nowhere, since the link
  // carries the data and the view together.
  function writeHash() {
    var parts = [];
    if (query) parts.push("q=" + encodeURIComponent(query));
    if (brush) parts.push("t=" + Math.round(brush[0]) + "-" + Math.round(brush[1]));
    var sel = gutter && gutter.current();
    if (sel && sel.length) {
      parts.push("L" + sel[0] + (sel.length > 1 ? "-L" + sel[sel.length - 1] : ""));
    }
    if (parts.length) DL.setHash(parts.join("&"));
    else DL.clearHash();
  }

  var typing = null;
  qBox.addEventListener("input", function () {
    query = qBox.value;
    render();
    // The hash is debounced but the render is not: filtering must feel
    // immediate, while rewriting the URL on every keystroke is wasted work.
    clearTimeout(typing);
    typing = setTimeout(writeHash, 300);
  });

  // ---- volume histogram --------------------------------------------------

  var histEl = document.getElementById("hist");

  function drawHist() {
    var withTime = records.filter(function (r) { return r.at; });
    if (withTime.length < 2) { histEl.hidden = true; return; }
    histEl.hidden = false;
    var lo = Infinity, hi = -Infinity;
    withTime.forEach(function (r) { lo = Math.min(lo, r.at); hi = Math.max(hi, r.at); });
    if (!(hi > lo)) { histEl.hidden = true; return; }

    var BUCKETS = 60;
    var span = (hi - lo) / BUCKETS;
    var shown = {}, all = new Array(BUCKETS).fill(0), lit = new Array(BUCKETS).fill(0);
    visible().forEach(function (r) { shown[r.n] = true; });
    withTime.forEach(function (r) {
      var i = Math.min(BUCKETS - 1, Math.floor((r.at - lo) / span));
      all[i]++;
      if (shown[r.n]) lit[i]++;
    });
    var peak = Math.max.apply(null, all) || 1;

    histEl.textContent = "";
    for (var i = 0; i < BUCKETS; i++) {
      var col = document.createElement("div");
      col.className = "hb";
      col.dataset.from = String(Math.round(lo + i * span));
      col.dataset.to = String(Math.round(lo + (i + 1) * span));
      // Two stacked bars: total in grey behind, matching in colour in front,
      // so narrowing a query shows what it EXCLUDED rather than rescaling to
      // hide it. A histogram that rescales cannot show a query's effect.
      var back = document.createElement("span");
      back.className = "hb-all";
      back.style.height = (all[i] / peak * 100) + "%";
      var front = document.createElement("span");
      front.className = "hb-lit";
      front.style.height = (lit[i] / peak * 100) + "%";
      col.appendChild(back);
      col.appendChild(front);
      col.title = new Date(Number(col.dataset.from)).toISOString() + "  " + all[i] + " records";
      histEl.appendChild(col);
    }
    histEl.classList.toggle("brushed", !!brush);
  }

  // Drag across the histogram to select a time window.
  (function wireBrush() {
    var from = null;
    function bucketAt(e) {
      var el = document.elementFromPoint(e.clientX, e.clientY);
      return el && el.closest ? el.closest(".hb") : null;
    }
    histEl.addEventListener("pointerdown", function (e) {
      var b = bucketAt(e);
      if (!b) return;
      from = b;
      histEl.setPointerCapture(e.pointerId);
    });
    histEl.addEventListener("pointerup", function (e) {
      if (!from) return;
      var to = bucketAt(e) || from;
      var a = Math.min(Number(from.dataset.from), Number(to.dataset.from));
      var z = Math.max(Number(from.dataset.to), Number(to.dataset.to));
      from = null;
      // A click with no drag on an already-brushed chart clears it, which is
      // the only way back out without editing the URL.
      brush = (brush && a === brush[0] && z === brush[1]) ? null : [a, z];
      render();
      writeHash();
    });
  })();

  fetch(location.pathname + "?raw=1", { credentials: "same-origin" })
    .then(function (r) {
      if (!r.ok) throw new Error("fetch failed: " + r.status);
      return r.text();
    })
    .then(function (text) {
      records = parse(text);
      records.forEach(function (r, i) {
        r.n = i + 1;
        r.at = Date.parse(r.time) || 0;
        // The query engine resolves `level=` and `message=` against these
        // regardless of which key the logger actually used.
        r.norm = { level: r.level, message: r.msg, msg: r.msg, time: r.time };
        // Precomputed once: the filter runs over every record on each
        // keystroke, and re-serialising there made typing lag on a big file.
        var rest = {};
        Object.keys(r.fields).forEach(function (k) {
          if (TIME_KEYS.indexOf(k) < 0 && LEVEL_KEYS.indexOf(k) < 0 && MSG_KEYS.indexOf(k) < 0) {
            rest[k] = r.fields[k];
          }
        });
        var keys = Object.keys(rest);
        r.extra = keys.length
          ? keys.map(function (k) {
              var v = rest[k];
              return k + "=" + (typeof v === "string" ? v : JSON.stringify(v));
            }).join("  ")
          : "";
        r.line = [r.time, r.level, r.msg, r.extra].join(" ");
      });
      if (!records.length) {
        mount.textContent = "no records";
        mount.setAttribute("aria-busy", "false");
        return;
      }
      drawLevels();
      render();
      gutter = window.HostthisLineGutter.attach(mount, { hint: true });
      DL.onResolve(function (t) {
        if (!t) return;
        if (t.type === "logview") {
          brush = t.from ? [t.from, t.to] : null;
          query = t.q;
          qBox.value = t.q;
          // Query and window are applied BEFORE the line selection, because
          // render() rebuilds the rows and a selection painted first would be
          // discarded with them.
          render();
          if (t.line) gutter.resolve(t.line);
          return;
        }
        if (t.type === "line") gutter.resolve(t);
        else if (t.type === "query") { query = t.q; qBox.value = t.q; render(); }
      });
    })
    .catch(function (err) {
      mount.textContent = "could not load: " + err.message;
      mount.setAttribute("aria-busy", "false");
    });
})();
