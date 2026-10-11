// Builds the Worker bundle, dist/index.js, and beside it what a Cloudflare
// deploy serves without running the Worker: dist/assets (the renderer assets
// at _hostthis/<name> and their _headers) and dist/static-r2.json (the assets
// too large for static assets, with the PAYLOADS key each is uploaded to).
import { build } from "esbuild";
import { copyFileSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { listAssets, MAX_ASSET_BYTES, REPO, shellsPlugin } from "./shells.mjs";

const celld = join(dirname(fileURLToPath(import.meta.url)), "..");
const dist = join(celld, "dist");

const CACHE_CONTROL = "public, max-age=31536000, immutable";

await build({
  absWorkingDir: celld,
  entryPoints: ["src/index.js"],
  bundle: true,
  format: "esm",
  platform: "neutral",
  keepNames: true,
  logLevel: "warning",
  outfile: "dist/index.js",
  plugins: [shellsPlugin],
});

const assets = join(dist, "assets");
rmSync(assets, { recursive: true, force: true });
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
