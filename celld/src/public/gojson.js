// JSON text byte-identical to what Go's json.Encoder writes. Pure, no I/O.

// Characters Go escapes beyond JSON's required set: <, > and & for HTML, and
// U+2028 and U+2029, which end a line in JavaScript source.
const SHORT = { "\"": "\\\"", "\\": "\\\\", "\b": "\\b", "\f": "\\f", "\n": "\\n", "\r": "\\r", "\t": "\\t" };

function escapeChar(ch) {
  if (Object.hasOwn(SHORT, ch)) {
    return SHORT[ch];
  }
  const code = ch.charCodeAt(0);
  if (code < 0x20 || ch === "<" || ch === ">" || ch === "&" || code === 0x2028 || code === 0x2029) {
    return `\\u${code.toString(16).padStart(4, "0")}`;
  }
  // A lone surrogate is not UTF-8; Go writes U+FFFD for invalid bytes.
  return "\\ufffd";
}

// Go's encoding of a string, with HTML escaping on.
export function goString(text) {
  return `"${text.replace(/["\\\u0000-\u001f<>&\u2028\u2029]|[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/g, escapeChar)}"`;
}

// json.NewEncoder(w).Encode(list) for a []string: compact, then a newline.
export function goStringList(list) {
  return `[${list.map(goString).join(",")}]\n`;
}
