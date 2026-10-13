// Builds the Worker bundle, dist/index.js, with each WASM module it imports
// beside it (dist/<name>.wasm), and what a Cloudflare deploy serves without
// running the Worker: dist/assets (the renderer assets at _hostthis/<name> and
// their _headers) and dist/static-r2.json (the assets too large for static
// assets, with the PAYLOADS key each is uploaded to).
import { build } from "esbuild";
import { copyFileSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { basename, dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { buildIdPlugin } from "./buildid.mjs";
import { listAssets, MAX_ASSET_BYTES, REPO, shellsPlugin } from "./shells.mjs";

const workerDir = join(dirname(fileURLToPath(import.meta.url)), "..");
const dist = join(workerDir, "dist");

const CACHE_CONTROL = "public, max-age=31536000, immutable";

// A runtime compiles WASM only from a module it was handed, never from bytes,
// so each .wasm import stays an import of a sibling file.
const wasm = new Map();
const wasmPlugin = {
  name: "hostthis-wasm",
  setup(b) {
    b.onResolve({ filter: /\.wasm$/ }, (args) => {
      const source = resolve(args.resolveDir, args.path);
      const name = basename(source);
      if (wasm.has(name) && wasm.get(name) !== source) throw new Error(`two WASM modules named ${name}`);
      wasm.set(name, source);
      return { path: `./${name}`, external: true };
    });
  },
};

rmSync(dist, { recursive: true, force: true });
await build({
  absWorkingDir: workerDir,
  entryPoints: ["src/index.js"],
  bundle: true,
  format: "esm",
  platform: "neutral",
  keepNames: true,
  logLevel: "warning",
  outfile: "dist/index.js",
  plugins: [shellsPlugin, buildIdPlugin, wasmPlugin],
});
for (const [name, source] of wasm) copyFileSync(source, join(dist, name));

const assets = join(dist, "assets");
mkdirSync(join(assets, "_hostthis"), { recursive: true });
const rules = [];
const r2 = [];
for (const a of listAssets()) {
  // A name is a path pattern in _headers, where * and : have meaning.
  if (!/^[A-Za-z0-9._-]+$/.test(a.name)) throw new Error(`asset name ${JSON.stringify(a.name)} is not path-safe`);
  if (a.size > MAX_ASSET_BYTES) {
    r2.push({
      name: a.name,
      key: `static/${a.version}/${a.name}`,
      source: relative(REPO, a.source),
      contentType: a.contentType,
      size: a.size,
    });
    continue;
  }
  copyFileSync(a.source, join(assets, "_hostthis", a.name));
  // Each rule carries every header, so none depends on how rules merge. A file
  // with no Content-Type rule is served without one.
  rules.push(`/_hostthis/${a.name}\n  Content-Type: ${a.contentType}\n  Cache-Control: ${CACHE_CONTROL}\n`);
}
writeFileSync(join(assets, "_headers"), rules.join("\n"));
writeFileSync(join(dist, "static-r2.json"), `${JSON.stringify(r2, null, 2)}\n`);
