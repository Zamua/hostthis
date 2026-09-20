// Search index for the knowledge base shell.
//
// Paths are searchable immediately: they come from the file list, so path
// matching always covers the WHOLE base. Headings and body text need the
// documents themselves, which are fetched in the background, one at a time in
// the file list's path-sort order and bounded (docs/SPEC.md "Knowledge bases").
// Sequential rather than concurrent so the bound falls on the same file every
// time rather than wherever a race left it.
//
// A caller that renders results must report state().partial: a search box that
// silently returns less than the base holds is worse than one that says so.
(function () {
  "use strict";

  var MAX_FILES = 500;
  var MAX_BYTES = 8 * 1024 * 1024;
  var MAX_HITS = 50;
  var HEADINGS_PER_FILE = 2;

  function byteLength(text) {
    if (typeof TextEncoder === "function") return new TextEncoder().encode(text).length;
    return text.length;
  }

  // headingsOf reads ATX headings from the markdown source, skipping fenced
  // blocks so a "# comment" inside a code fence is not offered as a section.
  // Ids repeat the renderer's dedupe so a hit addresses the anchor the document
  // actually got.
  function headingsOf(md, slug) {
    var out = [];
    var used = Object.create(null);
    var fence = null;
    md.split(/\r?\n/).forEach(function (line) {
      var f = line.match(/^\s{0,3}(`{3,}|~{3,})/);
      if (f) {
        if (!fence) fence = f[1].charAt(0);
        else if (f[1].charAt(0) === fence) fence = null;
        return;
      }
      if (fence) return;
      var m = line.match(/^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/);
      if (!m) return;
      // The renderer slugs the RENDERED text, so inline markup has to come off
      // first or the id would carry the asterisks.
      var text = m[2].replace(/[*_`]/g, "").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1").trim();
      if (!text) return;
      var base = slug(text) || "section";
      var id = base;
      var n = 1;
      while (used[id]) id = base + "-" + n++;
      used[id] = true;
      out.push({ level: m[1].length, text: text, id: id });
    });
    return out;
  }

  // snippet frames the match with enough either side to read, as three plain
  // strings: the caller builds text nodes from them, never markup.
  function snippet(text, lower, at, len) {
    var from = Math.max(0, at - 40);
    var to = Math.min(text.length, at + len + 90);
    var squash = function (s) { return s.replace(/\s+/g, " "); };
    return {
      before: (from > 0 ? "…" : "") + squash(text.slice(from, at)),
      match: squash(text.slice(at, at + len)),
      after: squash(text.slice(at + len, to)) + (to < text.length ? "…" : ""),
    };
  }

  function create(opts) {
    var files = opts.files;
    var docs = files.filter(opts.isDoc);
    var maxFiles = opts.maxFiles || MAX_FILES;
    var maxBytes = opts.maxBytes || MAX_BYTES;
    var slug = opts.slug;

    var entries = {};   // path -> { text, lower, headings }
    var seeds = {};     // path -> text already fetched for rendering
    var cursor = 0;
    var count = 0;
    var bytes = 0;
    var failures = 0;
    var done = docs.length === 0;
    var started = false;

    function absorb(path, text) {
      bytes += byteLength(text);
      count++;
      entries[path] = {
        text: text,
        lower: text.toLowerCase(),
        headings: headingsOf(text, slug),
      };
    }

    function progress() {
      if (opts.onProgress) opts.onProgress(state());
    }

    function step() {
      if (cursor >= docs.length || count >= maxFiles || bytes >= maxBytes) {
        done = true;
        progress();
        return;
      }
      var path = docs[cursor++];
      if (entries[path]) {
        next();
        return;
      }
      if (seeds[path] != null) {
        absorb(path, seeds[path]);
        next();
        return;
      }
      fetch(opts.rawURL(path), { credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.text() : null; })
        .then(function (text) {
          if (text == null) failures++;
          else absorb(path, text);
        })
        .catch(function () { failures++; })
        .then(next);
    }

    // One file per turn, yielding between them, so indexing never competes
    // with the document the reader is actually looking at.
    function next() {
      if (count % 10 === 0) progress();
      setTimeout(step, 0);
    }

    function state() {
      return {
        done: done,
        indexed: count,
        total: docs.length,
        failures: failures,
        // Partial covers both reasons the index can fall short of the base:
        // the bound stopped it, or a document could not be read.
        partial: done && (count < docs.length || failures > 0),
        maxFiles: maxFiles,
        maxBytes: maxBytes,
      };
    }

    function query(q) {
      var needle = String(q || "").trim().toLowerCase();
      var res = { query: needle, hits: [], truncated: false };
      if (needle.length < 2) return res;
      var full = false;
      var push = function (hit) {
        if (res.hits.length >= MAX_HITS) {
          full = true;
          res.truncated = true;
          return;
        }
        res.hits.push(hit);
      };

      var i;
      for (i = 0; i < files.length && !full; i++) {
        if (files[i].toLowerCase().indexOf(needle) >= 0) {
          push({ path: files[i], kind: "path", doc: opts.isDoc(files[i]) });
        }
      }
      for (i = 0; i < docs.length && !full; i++) {
        var e = entries[docs[i]];
        if (!e) continue;
        var shown = 0;
        for (var j = 0; j < e.headings.length && shown < HEADINGS_PER_FILE && !full; j++) {
          if (e.headings[j].text.toLowerCase().indexOf(needle) < 0) continue;
          shown++;
          push({ path: docs[i], kind: "heading", doc: true, heading: e.headings[j] });
        }
      }
      for (i = 0; i < docs.length && !full; i++) {
        var d = entries[docs[i]];
        if (!d) continue;
        var at = d.lower.indexOf(needle);
        if (at < 0) continue;
        push({ path: docs[i], kind: "text", doc: true, snippet: snippet(d.text, d.lower, at, needle.length) });
      }
      return res;
    }

    return {
      start: function () {
        if (started) return;
        started = true;
        progress();
        setTimeout(step, 0);
      },
      // seed hands the indexer a document the shell already fetched to render,
      // so reading a base does not download it twice. It is stored rather than
      // indexed on the spot: the bound must fall in the file list's order, not
      // in the reader's.
      seed: function (path, text) { seeds[path] = text; },
      query: query,
      state: state,
    };
  }

  window.HostthisKBSearch = { create: create };
})();
