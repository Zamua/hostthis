// Query language for the log viewer.
//
// Deliberately smaller than LogQL or DQL: those describe a SELECTION across a
// corpus the client has never seen. Here the whole file is already in memory,
// so a query only has to narrow rows, and the grammar can stay small enough to
// type from memory without a reference open.
//
//   level=error            field equals
//   level!=info            field does not equal
//   level=error|warn       either (per-field OR)
//   status>=500            numeric comparison
//   path=~^/p/             regex
//   path!~healthz          negated regex
//   "connection reset"     phrase, matched against the whole record
//   reset                  bare word, same as a phrase
//
// Terms combine with AND, because a reader adding a term is narrowing an
// investigation, never widening it.
(function () {
  "use strict";

  // Tokens are split on whitespace EXCEPT inside quotes, so a phrase with a
  // space survives and a value containing "=" is not re-split.
  function tokenize(s) {
    var out = [], cur = "", q = null;
    for (var i = 0; i < s.length; i++) {
      var c = s[i];
      if (q) {
        if (c === q) { q = null; continue; }
        cur += c;
        continue;
      }
      if (c === '"' || c === "'") { q = c; continue; }
      if (/\s/.test(c)) {
        if (cur) out.push(cur);
        cur = "";
        continue;
      }
      cur += c;
    }
    if (cur) out.push(cur);
    return out;
  }

  var OPS = ["!~", "=~", "!=", ">=", "<=", "=", ">", "<"];

  function parseTerm(tok) {
    for (var i = 0; i < OPS.length; i++) {
      var op = OPS[i];
      var at = tok.indexOf(op);
      // A term must have a field NAME before the operator; a leading "=" is
      // part of a phrase, not a matcher.
      if (at > 0) {
        return { field: tok.slice(0, at), op: op, value: tok.slice(at + op.length) };
      }
    }
    return { op: "text", value: tok };
  }

  function parse(s) {
    return tokenize(s || "").map(parseTerm).filter(function (t) {
      return t.value !== "" || t.op === "text";
    });
  }

  function numeric(v) {
    if (typeof v === "number") return v;
    if (typeof v === "string" && v.trim() !== "" && !isNaN(Number(v))) return Number(v);
    return null;
  }

  // Field lookup is case-insensitive and matches a trailing path segment, so
  // `level=` finds ECS's `log.level` without the reader knowing which logger
  // wrote the file.
  function fieldValue(rec, name) {
    var want = name.toLowerCase();
    var fields = rec.fields || {};
    if (rec.norm && rec.norm[want] !== undefined) return rec.norm[want];
    var keys = Object.keys(fields);
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i].toLowerCase();
      if (k === want || k.endsWith("." + want)) return fields[keys[i]];
    }
    return undefined;
  }

  function asText(v) {
    if (v === undefined || v === null) return "";
    return typeof v === "string" ? v : JSON.stringify(v);
  }

  function matchTerm(rec, t) {
    if (t.op === "text") {
      return rec.line.toLowerCase().indexOf(t.value.toLowerCase()) >= 0;
    }
    var raw = fieldValue(rec, t.field);
    var text = asText(raw).toLowerCase();
    var val = t.value.toLowerCase();

    switch (t.op) {
      case "=":
      case "!=": {
        // "a|b" is per-field OR. The alternative would be a precedence rule
        // between AND and OR, which is exactly the complexity this grammar
        // exists to avoid.
        var hit = val.split("|").some(function (v) { return text === v; });
        return t.op === "=" ? hit : !hit;
      }
      case "=~":
      case "!~": {
        var re;
        try { re = new RegExp(t.value, "i"); } catch (e) { return false; }
        var m = re.test(asText(raw));
        return t.op === "=~" ? m : !m;
      }
      default: {
        var n = numeric(raw), want = numeric(t.value);
        if (n === null || want === null) return false;
        if (t.op === ">") return n > want;
        if (t.op === ">=") return n >= want;
        if (t.op === "<") return n < want;
        if (t.op === "<=") return n <= want;
        return false;
      }
    }
  }

  function match(rec, terms) {
    for (var i = 0; i < terms.length; i++) {
      if (!matchTerm(rec, terms[i])) return false;
    }
    return true;
  }

  // Editing an existing term rather than appending keeps the bar from filling
  // with contradictory matchers as a reader clicks around the field sidebar.
  function withTerm(q, field, op, value) {
    var terms = parse(q).filter(function (t) {
      return !(t.op !== "text" && t.field.toLowerCase() === field.toLowerCase());
    });
    terms.push({ field: field, op: op, value: value });
    return terms.map(fmt).join(" ");
  }

  function withoutField(q, field) {
    return parse(q).filter(function (t) {
      return t.op === "text" || t.field.toLowerCase() !== field.toLowerCase();
    }).map(fmt).join(" ");
  }

  function fmt(t) {
    if (t.op === "text") return /\s/.test(t.value) ? '"' + t.value + '"' : t.value;
    var v = /\s/.test(t.value) ? '"' + t.value + '"' : t.value;
    return t.field + t.op + v;
  }

  window.HostthisLogQuery = {
    parse: parse,
    match: match,
    fieldValue: fieldValue,
    withTerm: withTerm,
    withoutField: withoutField,
    fmt: fmt,
  };
})();
