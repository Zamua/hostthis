// Reads web/shells, the render shells hostthisd embeds, for the Worker build
// and for tests that import the Worker source.
import { readFileSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

export const REPO = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
export const SHELLS_DIR = join(REPO, "web", "shells");

// The module src/public/shells.js imports the table and templates from.
export const SHELLS_MODULE = "hostthis:shells";

// Workers static assets refuse a larger file; it is served from PAYLOADS.
export const MAX_ASSET_BYTES = 25 * 1024 * 1024;

const TEMPLATE = "shell.html";

// shells.json without maintainer notes.
export function loadTable() {
  const raw = JSON.parse(readFileSync(join(SHELLS_DIR, "shells.json"), "utf8"));
  const shells = {};
  for (const [dir, { version, csp, assets }] of Object.entries(raw.shells)) {
    shells[dir] = csp ? { version, csp, assets } : { version, assets };
  }
  return { csp: raw.csp, kinds: raw.kinds, shells };
}

// Source of SHELLS_MODULE: the table, the template of every shell with a
// page, and the assets too large for static assets, which PAYLOADS serves.
export function shellsModule() {
  const table = loadTable();
  const templates = {};
  for (const [dir, shell] of Object.entries(table.shells)) {
    if (shell.csp) templates[dir] = readFileSync(join(SHELLS_DIR, dir, TEMPLATE), "utf8");
  }
  const r2 = listAssets(table).filter((a) => a.size > MAX_ASSET_BYTES).map((a) => a.name);
  return `export const table = ${JSON.stringify(table)};\n`
    + `export const templates = ${JSON.stringify(templates)};\n`
    + `export const r2 = ${JSON.stringify(r2)};\n`;
}

// Resolves SHELLS_MODULE in an esbuild build.
export const shellsPlugin = {
  name: "hostthis-shells",
  setup(b) {
    b.onResolve({ filter: new RegExp(`^${SHELLS_MODULE}$`) }, () => ({ path: SHELLS_MODULE, namespace: "hostthis" }));
    b.onLoad({ filter: /.*/, namespace: "hostthis" }, () => ({ contents: shellsModule(), loader: "js" }));
  },
};

// Every whitelisted asset, sorted by name.
export function listAssets(table = loadTable()) {
  const out = [];
  for (const [dir, shell] of Object.entries(table.shells)) {
    for (const [name, contentType] of Object.entries(shell.assets)) {
      const source = join(SHELLS_DIR, dir, name);
      out.push({ name, version: shell.version, contentType, source, size: statSync(source).size });
    }
  }
  return out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}
