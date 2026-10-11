// Room values and JSON bodies as Go reads them: domain.RoomWireValue,
// domain.RoomValueIsJSON and encoding/json's decoder (docs/SPEC.md "The HTTP
// API"). Pure functions over bytes, no I/O.

// encoding/json's limit, inclusive.
export const MAX_JSON_DEPTH = 10000;

const REPLACEMENT = 0xfffd;
const INVALID = [REPLACEMENT, 1];

function continuation(byte) {
  return byte >= 0x80 && byte <= 0xbf;
}

// Go's utf8.DecodeRune: [code point, size], and (U+FFFD, 1) for any byte that
// does not start a valid sequence, so each byte of a bad sequence costs one
// replacement where a WHATWG decoder replaces a maximal subpart once.
function decodeRune(b, i) {
  const c = b[i];
  if (c < 0x80) {
    return [c, 1];
  }
  const left = b.length - i;
  if (c >= 0xc2 && c <= 0xdf) {
    return left >= 2 && continuation(b[i + 1]) ? [((c & 0x1f) << 6) | (b[i + 1] & 0x3f), 2] : INVALID;
  }
  if (c >= 0xe0 && c <= 0xef) {
    const lo = c === 0xe0 ? 0xa0 : 0x80;
    const hi = c === 0xed ? 0x9f : 0xbf;
    if (left >= 3 && b[i + 1] >= lo && b[i + 1] <= hi && continuation(b[i + 2])) {
      return [((c & 0x0f) << 12) | ((b[i + 1] & 0x3f) << 6) | (b[i + 2] & 0x3f), 3];
    }
    return INVALID;
  }
  if (c >= 0xf0 && c <= 0xf4) {
    const lo = c === 0xf0 ? 0x90 : 0x80;
    const hi = c === 0xf4 ? 0x8f : 0xbf;
    if (left >= 4 && b[i + 1] >= lo && b[i + 1] <= hi && continuation(b[i + 2]) && continuation(b[i + 3])) {
      return [((c & 0x07) << 18) | ((b[i + 1] & 0x3f) << 12) | ((b[i + 2] & 0x3f) << 6) | (b[i + 3] & 0x3f), 4];
    }
    return INVALID;
  }
  return INVALID;
}

function pushCodePoint(units, cp) {
  if (cp > 0xffff) {
    units.push(0xd800 + ((cp - 0x10000) >> 10), 0xdc00 + ((cp - 0x10000) & 0x3ff));
  } else {
    units.push(cp);
  }
}

// Bytes to text the way a Go string becomes JSON: a byte-order mark is kept,
// and an invalid sequence is replaced byte by byte.
export function goText(bytes, start = 0, end = bytes.length) {
  const parts = [];
  let units = [];
  for (let i = start; i < end;) {
    const [cp, size] = decodeRune(bytes, i);
    pushCodePoint(units, cp);
    i += size;
    if (units.length >= 8192) {
      parts.push(String.fromCharCode(...units));
      units = [];
    }
  }
  parts.push(String.fromCharCode(...units));
  return parts.join("");
}

// Go's utf8.Valid.
export function validUTF8(bytes) {
  for (let i = 0; i < bytes.length;) {
    const [, size] = decodeRune(bytes, i);
    if (size === 1 && bytes[i] >= 0x80) {
      return false;
    }
    i += size;
  }
  return true;
}

// A string whose chars are bytes (0-255), such as a decoded URL path, as
// bytes.
export function byteStringBytes(text) {
  const out = new Uint8Array(text.length);
  for (let i = 0; i < text.length; i++) {
    out[i] = text.charCodeAt(i);
  }
  return out;
}

function isSpace(c) {
  return c === 0x20 || c === 0x09 || c === 0x0a || c === 0x0d;
}

function skipSpace(b, i) {
  while (i < b.length && isSpace(b[i])) {
    i++;
  }
  return i;
}

function isDigit(c) {
  return c >= 0x30 && c <= 0x39;
}

function hexValue(c) {
  if (isDigit(c)) return c - 0x30;
  if (c >= 0x41 && c <= 0x46) return c - 0x37;
  if (c >= 0x61 && c <= 0x66) return c - 0x57;
  return -1;
}

function hex4(b, i) {
  if (i + 4 > b.length) {
    return -1;
  }
  let v = 0;
  for (let k = 0; k < 4; k++) {
    const h = hexValue(b[i + k]);
    if (h < 0) {
      return -1;
    }
    v = v * 16 + h;
  }
  return v;
}

// Index past a string starting at the quote at i, or -1. Any byte at or above
// 0x20 may appear raw, invalid UTF-8 included, as Go's scanner allows.
function scanString(b, i) {
  for (i++; i < b.length; i++) {
    const c = b[i];
    if (c === 0x22) {
      return i + 1;
    }
    if (c < 0x20) {
      return -1;
    }
    if (c === 0x5c) {
      const e = b[++i];
      if (e === 0x75) {
        if (hex4(b, i + 1) < 0) {
          return -1;
        }
        i += 4;
      } else if (!"\"\\/bfnrt".includes(String.fromCharCode(e ?? 0))) {
        return -1;
      }
    }
  }
  return -1;
}

function scanDigits(b, i) {
  while (i < b.length && isDigit(b[i])) {
    i++;
  }
  return i;
}

function scanNumber(b, i) {
  if (b[i] === 0x2d) {
    i++;
  }
  if (b[i] === 0x30) {
    i++;
  } else if (b[i] >= 0x31 && b[i] <= 0x39) {
    i = scanDigits(b, i);
  } else {
    return -1;
  }
  if (b[i] === 0x2e) {
    if (!isDigit(b[i + 1])) {
      return -1;
    }
    i = scanDigits(b, i + 1);
  }
  if (b[i] === 0x65 || b[i] === 0x45) {
    i++;
    if (b[i] === 0x2b || b[i] === 0x2d) {
      i++;
    }
    if (!isDigit(b[i])) {
      return -1;
    }
    i = scanDigits(b, i);
  }
  return i;
}

const LITERALS = [["true", true], ["false", false], ["null", null]];
const ESCAPES = { 0x22: 0x22, 0x5c: 0x5c, 0x2f: 0x2f, 0x62: 8, 0x66: 12, 0x6e: 10, 0x72: 13, 0x74: 9 };
const ASCII = new TextDecoder();

function scanLiteral(b, i) {
  for (const [word, value] of LITERALS) {
    let k = 0;
    while (k < word.length && b[i + k] === word.charCodeAt(k)) {
      k++;
    }
    if (k === word.length) {
      return [i + k, value];
    }
  }
  return [-1, undefined];
}

// Go's unquote: escapes decoded, a lone surrogate escape and each byte of an
// invalid sequence become U+FFFD.
function unquote(b, start, end) {
  const units = [];
  for (let i = start + 1; i < end - 1;) {
    const c = b[i];
    if (c !== 0x5c) {
      const [cp, size] = decodeRune(b, i);
      pushCodePoint(units, cp);
      i += size;
      continue;
    }
    const e = b[i + 1];
    if (e !== 0x75) {
      units.push(ESCAPES[e]);
      i += 2;
      continue;
    }
    let cp = hex4(b, i + 2);
    i += 6;
    if (cp >= 0xd800 && cp <= 0xdfff) {
      const low = b[i] === 0x5c && b[i + 1] === 0x75 ? hex4(b, i + 2) : -1;
      if (cp <= 0xdbff && low >= 0xdc00 && low <= 0xdfff) {
        cp = 0x10000 + ((cp - 0xd800) << 10) + (low - 0xdc00);
        i += 6;
      } else {
        cp = REPLACEMENT;
      }
    }
    pushCodePoint(units, cp);
  }
  let out = "";
  for (let k = 0; k < units.length; k += 8192) {
    out += String.fromCharCode(...units.slice(k, k + 8192));
  }
  return out;
}

// Scans the JSON value that starts at start, after whitespace, without
// recursion: Go's grammar and nesting limit. Returns { end, value } with the
// value built when build is set, or null for a syntax error. A parsed value is
// a tree that keeps what JSON.parse loses: { t: "object", entries: [[key,
// node], ...] } in document order, { t: "array", items }, { t: "string", v },
// { t: "number", raw } with the literal's text, { t: "bool", v }, { t: "null" }.
export function scanJSON(b, start = 0, build = false) {
  let i = skipSpace(b, start);
  const stack = [];
  // The member name each open object is reading the value of.
  const names = [];
  let done;
  const attach = (node) => {
    const top = stack[stack.length - 1];
    if (!top) {
      done = node;
    } else if (top.t === "array") {
      top.items.push(node);
    } else {
      top.entries.push([names[stack.length - 1], node]);
    }
  };
  // Reads `"name" :` at i for the innermost object; false on a syntax error.
  const memberKey = () => {
    const keyEnd = b[i] === 0x22 ? scanString(b, i) : -1;
    if (keyEnd < 0) {
      return false;
    }
    if (build) {
      names[stack.length - 1] = unquote(b, i, keyEnd);
    }
    i = skipSpace(b, keyEnd);
    if (b[i] !== 0x3a) {
      return false;
    }
    i = skipSpace(b, i + 1);
    return true;
  };
  for (;;) {
    // A value starts at i.
    const c = b[i];
    if (c === 0x7b || c === 0x5b) {
      if (stack.length + 1 > MAX_JSON_DEPTH) {
        return null;
      }
      const container = c === 0x7b ? { t: "object", entries: [] } : { t: "array", items: [] };
      stack.push(container);
      i = skipSpace(b, i + 1);
      if (b[i] !== (c === 0x7b ? 0x7d : 0x5d)) {
        if (c === 0x7b && !memberKey()) {
          return null;
        }
        continue;
      }
      i++;
      stack.pop();
      if (build) attach(container);
    } else {
      let end;
      let node = null;
      if (c === 0x22) {
        end = scanString(b, i);
        if (end >= 0 && build) node = { t: "string", v: unquote(b, i, end) };
      } else if (c === 0x2d || isDigit(c)) {
        end = scanNumber(b, i);
        if (end >= 0 && build) node = { t: "number", raw: ASCII.decode(b.subarray(i, end)) };
      } else {
        let value;
        [end, value] = scanLiteral(b, i);
        if (end >= 0 && build) node = value === null ? { t: "null" } : { t: "bool", v: value };
      }
      if (end < 0) {
        return null;
      }
      i = end;
      if (build) attach(node);
    }
    // A value ended at i: close containers until one takes another member.
    for (;;) {
      const top = stack[stack.length - 1];
      if (!top) {
        return { end: i, value: done };
      }
      i = skipSpace(b, i);
      if (b[i] === (top.t === "object" ? 0x7d : 0x5d)) {
        i++;
        stack.pop();
        if (build) attach(top);
        continue;
      }
      if (b[i] !== 0x2c) {
        return null;
      }
      i = skipSpace(b, i + 1);
      if (top.t === "object" && !memberKey()) {
        return null;
      }
      break;
    }
  }
}

// Go's json.Valid: one value and nothing but whitespace after it.
export function jsonValid(bytes) {
  const scanned = scanJSON(bytes);
  return scanned !== null && skipSpace(bytes, scanned.end) === bytes.length;
}

// domain.RoomValueIsJSON.
export function roomValueIsJSON(bytes) {
  return bytes.length > 0 && jsonValid(bytes);
}

// domain.RoomWireValue as text: a JSON value passes through verbatim, anything
// else becomes a JSON string of the bytes.
export function roomWireValue(bytes) {
  const text = goText(bytes);
  return roomValueIsJSON(bytes) ? text : JSON.stringify(text);
}

// The first JSON value of a body as json.Decoder.Decode reads it: anything
// after the value is ignored. Null for an empty body or a syntax error.
export function decodeFirstJSON(bytes) {
  return scanJSON(bytes, 0, true)?.value ?? null;
}

export function base64Bytes(text) {
  const binary = atob(text);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

export function bytesBase64(bytes) {
  let binary = "";
  for (let i = 0; i < bytes.length; i += 8192) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 8192));
  }
  return btoa(binary);
}
