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

  var records = [];
  var active = {};   // level -> excluded when true
  var query = "";
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

  function visible() {
    var q = query.toLowerCase();
    return records.filter(function (r) {
      if (active[r.level]) return false;
      if (!q) return true;
      return r.line.toLowerCase().indexOf(q) >= 0;
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
      b.setAttribute("aria-pressed", "true");
      b.onclick = function () {
        active[lv] = !active[lv];
        b.setAttribute("aria-pressed", active[lv] ? "false" : "true");
        b.classList.toggle("off", !!active[lv]);
        render();
      };
      levelsBar.appendChild(b);
    });
  }

  qBox.addEventListener("input", function () {
    query = qBox.value.trim();
    render();
  });

  fetch(location.pathname + "?raw=1", { credentials: "same-origin" })
    .then(function (r) {
      if (!r.ok) throw new Error("fetch failed: " + r.status);
      return r.text();
    })
    .then(function (text) {
      records = parse(text);
      records.forEach(function (r, i) {
        r.n = i + 1;
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
      DL.onResolve(function (t) { gutter.resolve(t); });
    })
    .catch(function (err) {
      mount.textContent = "could not load: " + err.message;
      mount.setAttribute("aria-busy", "false");
    });
})();
