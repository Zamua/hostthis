// Runs the celld Worker under Miniflare (workerd) on loopback, with the
// bindings celld/wrangler.jsonc declares. Prints "LISTENING <url>" once ready
// and serves until signalled or, when stdin is a pipe or socket, until it
// closes, so a parent process that dies takes the runtime with it.
//
// HOST and PORT pick the listener (default loopback, ephemeral port; the
// routes are cluster-internal and carry no auth). PERSIST_DIR keeps cells and
// payloads across restarts; without it cells live in memory and payloads in a
// temp dir removed on exit.
import { Miniflare } from "miniflare";
import { fstatSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const config = JSON.parse(readFileSync(join(here, "..", "wrangler.jsonc"), "utf8")
  .split("\n").filter((line) => !line.trim().startsWith("//")).join("\n"));

const persist = process.env.PERSIST_DIR;
const tempDir = persist ? null : mkdtempSync(join(tmpdir(), "hostthis-localrt-"));
if (persist) mkdirSync(persist, { recursive: true });

const mf = new Miniflare({
  modules: true,
  scriptPath: join(here, "entry.mjs"),
  modulesRoot: join(here, ".."),
  modulesRules: [{ type: "ESModule", include: ["**/*.js", "**/*.mjs"] }],
  compatibilityDate: config.compatibility_date,
  host: process.env.HOST ?? "127.0.0.1",
  // The Worker reads no request.cf, and fetching it would need the network.
  cf: false,
  port: Number(process.env.PORT ?? 0),
  r2Buckets: Object.fromEntries(config.r2_buckets.map((b) => [b.binding, b.bucket_name])),
  r2Persist: persist ? join(persist, "r2") : tempDir,
  durableObjectsPersist: persist ? join(persist, "cells") : false,
  durableObjects: Object.fromEntries(config.durable_objects.bindings
    .map((b) => [b.name, { className: b.class_name, useSQLite: true }])),
});

const url = await mf.ready;
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
