// The build id the Worker keys its edge cache under: a hash of everything that
// decides a read response (the shells module, celld/src and celld/vendor), so
// a deploy that changes any of it starts a fresh key space.
import { createHash } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative, sep } from "node:path";

import { REPO, shellsModule } from "./shells.mjs";

// The module src/public/surface.js imports the id from.
export const BUILD_MODULE = "hostthis:build";

const ROOTS = [join(REPO, "celld", "src"), join(REPO, "celld", "vendor")];

// Every file under dir, as { path, bytes } with path relative to the repo
// and slash-separated.
function filesUnder(dir) {
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((e) => e.isFile())
    .map((e) => {
      const full = join(e.parentPath, e.name);
      return { path: relative(REPO, full).split(sep).join("/"), bytes: readFileSync(full) };
    });
}

// The id of the shells module's source and files, whatever order they come in.
export function hashInputs(shells, files) {
  const hash = createHash("sha256");
  hash.update(`shells\0${shells.length}\0`).update(shells);
  for (const { path, bytes } of [...files].sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0))) {
    hash.update(`\0${path}\0${bytes.length}\0`).update(bytes);
  }
  return hash.digest("hex").slice(0, 16);
}

export function buildId() {
  return hashInputs(shellsModule(), ROOTS.flatMap(filesUnder));
}

// Source of BUILD_MODULE.
export function buildModule() {
  return `export const BUILD = ${JSON.stringify(buildId())};\n`;
}

// Resolves BUILD_MODULE in an esbuild build.
export const buildIdPlugin = {
  name: "hostthis-build-id",
  setup(b) {
    b.onResolve({ filter: new RegExp(`^${BUILD_MODULE}$`) }, () => ({ path: BUILD_MODULE, namespace: "hostthis-build" }));
    b.onLoad({ filter: /.*/, namespace: "hostthis-build" }, () => ({ contents: buildModule(), loader: "js" }));
  },
};
