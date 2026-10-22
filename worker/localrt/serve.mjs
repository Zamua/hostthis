// Runs the bundled Worker (worker/dist, from `make bundle`) under
// Miniflare (workerd) on loopback, with the bindings worker/wrangler.jsonc
// declares. Prints "LISTENING <url>" once ready and serves until signalled or,
// when stdin is a pipe or socket, until it closes, so a parent process that
// dies takes the runtime with it.
//
// HOST and PORT pick the listener (default loopback, ephemeral port).
// HOSTTHIS_WORKER_TOKEN, when set, is the bearer token every route but /healthz
// requires. PERSIST_DIR keeps cells and payloads across restarts; without it
// cells live in memory and payloads in a temp dir removed on exit.
//
// HOSTTHIS_LOCALRT_<NAME> sets the Worker variable HOSTTHIS_<NAME> for the
// public surface (docs/SPEC.md "Public and internal hosts"). The prefix keeps
// a runtime started beside hostthisd from taking hostthisd's own
// HOSTTHIS_APEX_DOMAIN. HOSTTHIS_LOCALRT_APEX_DOMAIN turns the surface on; it
// needs the token, and brings what a Cloudflare deploy serves beside the
// Worker: the static assets (dist/assets) and the assets too large for them,
// stored in PAYLOADS.
import { Miniflare } from "miniflare";
import { fstatSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const workerDir = join(here, "..");
const config = JSON.parse(readFileSync(join(workerDir, "wrangler.jsonc"), "utf8")
  .split("\n").filter((line) => !line.trim().startsWith("//")).join("\n"));

// A stale bundle would run old code while the source under test looks current.
function mtime(path) {
  try {
    return statSync(path).mtimeMs;
  } catch {
    return -1;
  }
}
const sources = [join(workerDir, "src"), join(workerDir, "build"), join(workerDir, "vendor"), join(workerDir, "..", "web", "shells")]
  .flatMap((dir) => readdirSync(dir, { recursive: true, withFileTypes: true }))
  .filter((entry) => entry.isFile())
  .map((entry) => mtime(join(entry.parentPath, entry.name)));
const built = mtime(join(workerDir, config.main));
if (built < Math.max(mtime(join(workerDir, "package.json")), ...sources)) {
  process.stderr.write(`worker runtime: ${config.main} is ${built < 0 ? "missing" : "older than its sources"}; run \`make bundle\`\n`);
  process.exit(1);
}

const PUBLIC_VARS = [
  "APEX_DOMAIN",
  "URL_MODE",
  "PUBLIC_SCHEME",
  "CLIENT_IP_SOURCE",
  "EDGE_CACHE_TTL_S",
  "EDGE_CACHE_MAX_BYTES",
  "BLOB_CACHE_MAX_BYTES",
  "SOLID_KEY",
  "SOLID_ORIGIN",
];
const bindings = {};
for (const [name, value] of Object.entries(process.env)) {
  const suffix = name.match(/^HOSTTHIS_LOCALRT_(.+)$/)?.[1];
  if (suffix === undefined) continue;
  // A misspelt name would leave the surface silently configured otherwise.
  if (!PUBLIC_VARS.includes(suffix)) {
    process.stderr.write(`worker runtime: ${name} is not one of ${PUBLIC_VARS.map((v) => `HOSTTHIS_LOCALRT_${v}`).join(", ")}\n`);
    process.exit(1);
  }
  if (value !== "") bindings[`HOSTTHIS_${suffix}`] = value;
}
const token = process.env.HOSTTHIS_WORKER_TOKEN;
if (token) bindings.HOSTTHIS_WORKER_TOKEN = token;
const publicSurface = bindings.HOSTTHIS_APEX_DOMAIN !== undefined;
if (publicSurface && !token) {
  process.stderr.write("worker runtime: HOSTTHIS_LOCALRT_APEX_DOMAIN needs HOSTTHIS_WORKER_TOKEN\n");
  process.exit(1);
}

const persist = process.env.PERSIST_DIR;
const tempDir = persist ? null : mkdtempSync(join(tmpdir(), "hostthis-localrt-"));
if (persist) mkdirSync(persist, { recursive: true });

const mf = new Miniflare({
  modules: true,
  scriptPath: join(here, "entry.mjs"),
  modulesRoot: join(here, ".."),
  // The bundle imports its WASM decoder as a module, as Workers require.
  modulesRules: [
    { type: "ESModule", include: ["**/*.js", "**/*.mjs"] },
    { type: "CompiledWasm", include: ["**/*.wasm"] },
  ],
  compatibilityDate: config.compatibility_date,
  host: process.env.HOST ?? "127.0.0.1",
  // The Worker reads no request.cf, and fetching it would need the network.
  cf: false,
  port: Number(process.env.PORT ?? 0),
  bindings,
  ...(publicSurface && {
    assets: { directory: join(workerDir, "dist", "assets"), binding: "ASSETS", routerConfig: { has_user_worker: true } },
  }),
  r2Buckets: Object.fromEntries(config.r2_buckets.map((b) => [b.binding, b.bucket_name])),
  r2Persist: persist ? join(persist, "r2") : tempDir,
  durableObjectsPersist: persist ? join(persist, "cells") : false,
  durableObjects: Object.fromEntries(config.durable_objects.bindings
    .map((b) => [b.name, { className: b.class_name, useSQLite: true }])),
});

const url = await mf.ready;
if (publicSurface) {
  const payloads = await mf.getR2Bucket("PAYLOADS");
  const repo = join(workerDir, "..");
  for (const { key, source, size } of JSON.parse(readFileSync(join(workerDir, "dist", "static-r2.json"), "utf8"))) {
    if ((await payloads.head(key))?.size !== size) await payloads.put(key, readFileSync(join(repo, source)));
  }
}
process.stdout.write(`LISTENING ${url.origin}\n`);

let stopping = false;
async function stop() {
  if (stopping) return;
  stopping = true;
  await mf.dispose().catch(() => {});
  if (tempDir) rmSync(tempDir, { recursive: true, force: true });
  process.exit(0);
}
process.on("SIGINT", stop);
process.on("SIGTERM", stop);
// Node's child_process hands a child a socket for stdin, Go's os/exec a FIFO.
const stdin = fstatSync(0);
if (stdin.isFIFO() || stdin.isSocket()) {
  process.stdin.on("end", stop);
  process.stdin.resume();
}
