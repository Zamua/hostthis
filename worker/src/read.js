// The read rules: what a request to a paste's origin serves, decided from the
// stored row alone. Pure, with no I/O and no runtime API, so the Paste cell runs
// them and only the small answer leaves it. testdata/read-vectors.json holds
// them to the Go read path (domain.Manifest, internal/http serveSlug).
//
// An answer is one of:
//   { found: false }                       no row
//   { status: "pending" | "failed" }       a document still writing, or never written
//   { action, kind, updatedAt, uploadId, entry?, shell?, files? }
// action is html | shell | raw | file | kbshell | kbraw | files | notfound.
// entry ({ key, contentType, decodedSize }, plus offset and length for a pack
// member) is the stored entry whose bytes to stream; decodedSize is its recorded decoded length, 0 when it has none (its
// Size is for display and may be wrong, so it never leaves the cell). Which
// Content-Type serves it is the caller's choice per action. shell names
// the kind whose template renders; a kind without one is the caller's 500.

const ROOT = "/";
const HTML = "html";
const SITE = "site";
const KNOWLEDGE_BASE = "knowledgebase";

// Kinds a fixed shell renders from their ?raw bytes.
const SHELL_KINDS = new Set(["markdown", "diff", "mermaid", "pdf", "csv", "json", "flamegraph", "text", "log"]);

// Known extensions a manifest miss on is a missing file (404) rather than a
// client-side route. The rest of the known set (.html, .htm, .md, .markdown)
// routes, as does every unknown extension.
export const ASSET_EXTENSIONS = new Set([
  ".css", ".js", ".mjs", ".cjs", ".json", ".map", ".xml", ".txt", ".csv", ".pdf", ".wasm", ".webmanifest",
  ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg", ".ico", ".bmp",
  ".woff", ".woff2", ".ttf", ".otf", ".eot",
  ".mp4", ".webm", ".mov", ".m4v", ".ogv", ".mp3", ".wav", ".ogg", ".flac", ".m4a", ".aac",
  ".gz", ".br", ".zip", ".bin", ".dat",
]);

export function resolveRead(row, { path, raw = false, files = false }) {
  if (!row) {
    return { found: false };
  }
  const kind = typeof row.kind === "string" ? row.kind : "";
  const answer = (action, extra) => ({
    action,
    kind,
    updatedAt: typeof row.updatedAt === "number" ? row.updatedAt : 0,
    uploadId: typeof row.uploadId === "string" ? row.uploadId : "",
    ...extra,
  });
  const manifest = decodeManifest(row.manifest);

  // The shape is the declared kind, never the entry count: a one-file
  // directory still hands back its file.
  if (kind === KNOWLEDGE_BASE) {
    return knowledgeBase(manifest, path, raw, files, answer);
  }
  if (kind === SITE) {
    return servedFile(lookupWithSPAFallback(manifest, path), answer);
  }
  // A document answers only at its own URL, and status gates it only there.
  if (path !== ROOT) {
    return answer("notfound");
  }
  if (row.status === "pending" || row.status === "failed") {
    return { status: row.status };
  }
  const root = lookup(manifest, ROOT);
  if (!root?.key) {
    return answer("notfound");
  }
  if (kind === HTML) {
    return answer("html", { entry: root });
  }
  if (raw && SHELL_KINDS.has(kind)) {
    return answer("raw", { entry: root });
  }
  return answer("shell", { shell: kind });
}

function servedFile(entry, answer) {
  return entry?.key ? answer("file", { entry }) : answer("notfound");
}

// The shell answers the root, every markdown path, and route-shaped misses;
// ?raw on a markdown file is its bytes, and every other file serves as a
// site's does. A directory path is the shell's to list, so the lookup is exact.
function knowledgeBase(manifest, path, raw, files, answer) {
  if (files) {
    return answer("files", { files: [...manifest.keys()].sort(byUTF8) });
  }
  const entry = exactFile(manifest, path);
  const doc = path === ROOT || isMarkdownPath(path);
  if (entry && doc && raw) {
    return entry.key ? answer("kbraw", { entry }) : answer("notfound");
  }
  if (entry && !doc) {
    return servedFile(entry, answer);
  }
  if (!entry && looksLikeAsset(path)) {
    return answer("notfound");
  }
  return answer("kbshell", { shell: KNOWLEDGE_BASE });
}

// --- manifest lookups (domain.Manifest) --------------------------------------

function trimSlash(path) {
  return path.startsWith("/") ? path.slice(1) : path;
}

// The directory-index rule: the root resolves to the Root key a document uses,
// else index.html; "dir/" and a bare "dir" resolve to "dir/index.html".
function lookup(manifest, path) {
  const clean = trimSlash(path);
  if (clean === "" || clean.endsWith("/")) {
    if (clean === "" && manifest.has(ROOT)) {
      return manifest.get(ROOT);
    }
    return manifest.get(`${clean}index.html`);
  }
  return manifest.get(clean) ?? manifest.get(`${clean}/index.html`);
}

function exactFile(manifest, path) {
  if (path === "" || path === ROOT) {
    return manifest.get(ROOT);
  }
  const clean = trimSlash(path);
  return clean.endsWith("/") ? undefined : manifest.get(clean);
}

// A route-shaped miss serves the root index.html; an asset-shaped one stays a
// miss.
function lookupWithSPAFallback(manifest, path) {
  return lookup(manifest, path) ?? (looksLikeAsset(path) ? undefined : manifest.get("index.html"));
}

function looksLikeAsset(path) {
  return ASSET_EXTENSIONS.has(lowerExt(extension(baseName(path))));
}

function isMarkdownPath(path) {
  const ext = lowerExt(extension(path));
  return ext === ".md" || ext === ".markdown";
}

// Go's path.Base.
function baseName(path) {
  if (path === "") {
    return ".";
  }
  const trimmed = path.replace(/\/+$/, "");
  const last = trimmed.slice(trimmed.lastIndexOf("/") + 1);
  return last === "" ? "/" : last;
}

// Go's path.Ext.
function extension(path) {
  for (let i = path.length - 1; i >= 0 && path[i] !== "/"; i--) {
    if (path[i] === ".") {
      return path.slice(i);
    }
  }
  return "";
}

// Go's strings.ToLower maps U+0130 to "i", where toLowerCase gives "i" plus a
// combining dot; it is the one non-ASCII letter whose difference can reach an
// ASCII extension.
function lowerExt(ext) {
  return ext.replaceAll("\u0130", "i").toLowerCase();
}

// UTF-8 byte order, as Go sorts strings. UTF-16 order differs where a surrogate
// pair (U+10000 and up) meets a unit at U+E000 or above.
function byUTF8(a, b) {
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i++) {
    const x = a.charCodeAt(i);
    const y = b.charCodeAt(i);
    if (x !== y) {
      return utf8Rank(x) - utf8Rank(y);
    }
  }
  return a.length - b.length;
}

function utf8Rank(unit) {
  if (unit >= 0xd800 && unit <= 0xdfff) {
    return unit + 0x2000;
  }
  return unit >= 0xe000 ? unit - 0x800 : unit;
}

// --- decoding the stored manifest --------------------------------------------
//
// hostthisd reads the manifest with encoding/json into domain.Manifest, so it
// decodes here by the same rules: field names match case-insensitively (with
// Go's folds of U+017F to S and U+212A to K), a later duplicate overwrites, a
// null field leaves the value as it was, a repeated Files merges into the map,
// unknown fields are ignored, and a value of the wrong type fails the whole
// manifest, which then reads as none (CLAUDE.md principle 3). The result maps
// each path to { key, contentType, decodedSize }.

class Undecodable extends Error {}

const ENTRY_FIELDS = {
  KEY: ["key", "string"],
  OFFSET: ["offset", "int"],
  LENGTH: ["length", "int"],
  SIZE: [null, "int"],
  DECODEDSIZE: ["decodedSize", "int"],
  COMPRESSEDSIZE: [null, "int"],
  CONTENTTYPE: ["contentType", "string"],
  KIND: [null, "string"],
};

function decodeManifest(value) {
  try {
    return decodeFiles(value);
  } catch (error) {
    if (error instanceof Undecodable) {
      return new Map();
    }
    throw error;
  }
}

// An entry's validator, the same as Go's ManifestEntry.Validator: its key,
// plus its offset when several files share one pack.
export function entryValidator(entry) {
  return entry.length > 0 ? `${entry.key}@${entry.offset}` : entry.key;
}

function decodeFiles(value) {
  let files = new Map();
  if (value === undefined || value === null) {
    return files;
  }
  for (const [name, field] of Object.entries(asObject(value))) {
    if (foldName(name) !== "FILES" || field === undefined) {
      continue;
    }
    if (field === null) {
      files = new Map();
      continue;
    }
    for (const [path, entry] of Object.entries(asObject(field))) {
      files.set(path, decodeEntry(entry));
    }
  }
  return files;
}

function decodeEntry(value) {
  const entry = { key: "", offset: 0, length: 0, contentType: "", decodedSize: 0 };
  for (const [name, field] of Object.entries(value === undefined || value === null ? {} : asObject(value))) {
    const known = ENTRY_FIELDS[foldName(name)];
    if (!known || field === undefined || field === null) {
      continue;
    }
    const [as, type] = known;
    const decoded = type === "string" ? asString(field) : asInt(field);
    if (as) {
      entry[as] = decoded;
    }
  }
  // A span exists only for a pack member; a whole object carries none. Go
  // omits a zero Offset, so the first member arrives with none.
  if (!(entry.length > 0)) {
    delete entry.offset;
    delete entry.length;
  }
  return entry;
}

function asObject(value) {
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new Undecodable();
  }
  return value;
}

function asString(value) {
  if (typeof value !== "string") {
    throw new Undecodable();
  }
  return value;
}

// A Go int is 64 bits, and a number it would see in exponent form (1e21 and
// up) or with a fraction fails it.
function asInt(value) {
  if (typeof value !== "number" || !Number.isInteger(value) || value < -(2 ** 63) || value >= 2 ** 63) {
    throw new Undecodable();
  }
  return value;
}

// encoding/json's foldName, for names that can match an ASCII field.
function foldName(name) {
  let out = "";
  for (const ch of name) {
    out += ch === "\u017f" ? "S" : ch === "\u212a" ? "K" : ch < "\x80" ? ch.toUpperCase() : ch;
  }
  return out;
}
