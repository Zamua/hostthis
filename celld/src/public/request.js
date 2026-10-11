// What a public read asks for, read by the rules Go's net/http applies, so a
// URL resolves the same way here and in hostthisd. Pure, no I/O.

// Go's url.ParseQuery parses a query with more pairs than this as empty.
const MAX_QUERY_PAIRS = 10000;

function hex(code) {
  if (code >= 0x30 && code <= 0x39) return code - 0x30;
  if (code >= 0x41 && code <= 0x46) return code - 0x37;
  if (code >= 0x61 && code <= 0x66) return code - 0x57;
  return -1;
}

// Go's url.QueryUnescape over an ASCII query component, as a byte string;
// null for a malformed escape.
function queryUnescape(text) {
  let out = "";
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === "+") {
      out += " ";
    } else if (c !== "%") {
      out += c;
    } else {
      const hi = hex(text.charCodeAt(i + 1));
      const lo = hex(text.charCodeAt(i + 2));
      if (hi < 0 || lo < 0) {
        return null;
      }
      out += String.fromCharCode(hi * 16 + lo);
      i += 2;
    }
  }
  return out;
}

// The raw and files flags as Go's URL.Query().Has reads them from the query
// (without its "?"): pairs split on "&", a pair holding ";" dropped, keys and
// values percent-decoded with "+" as a space, and a pair whose key or value
// fails to decode dropped. Any value counts.
export function queryFlags(rawQuery) {
  const flags = { raw: false, files: false };
  const pairs = rawQuery.split("&");
  if (rawQuery === "" || pairs.length > MAX_QUERY_PAIRS) {
    return flags;
  }
  for (const pair of pairs) {
    if (pair === "" || pair.includes(";")) {
      continue;
    }
    const eq = pair.indexOf("=");
    const key = queryUnescape(eq < 0 ? pair : pair.slice(0, eq));
    if (key === null || queryUnescape(eq < 0 ? "" : pair.slice(eq + 1)) === null) {
      continue;
    }
    if (key === "raw") {
      flags.raw = true;
    } else if (key === "files") {
      flags.files = true;
    }
  }
  return flags;
}

// Go's path.Base.
export function baseName(path) {
  if (path === "") {
    return ".";
  }
  const trimmed = path.replace(/\/+$/, "");
  if (trimmed === "") {
    return "/";
  }
  return trimmed.slice(trimmed.lastIndexOf("/") + 1);
}
