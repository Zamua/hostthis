import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// Every env binding the Worker reads is declared in wrangler.jsonc. A misspelt
// binding is a TypeError at runtime, and a try/catch around a cell call turns
// that into a silent retry loop.
test("every cell namespace the Worker reads is a declared binding", () => {
  const src = readFileSync(new URL("../src/index.js", import.meta.url), "utf8");
  const cfg = readFileSync(new URL("../wrangler.jsonc", import.meta.url), "utf8");
  const declared = new Set([...cfg.matchAll(/"name":\s*"([A-Z_]+)"/g)].map((m) => m[1]));
  const used = new Set([
    ...src.matchAll(/\benv\.([A-Z_]+)\.(?:get|idFromName)\(|\bcellCall\(this\.env\.([A-Z_]+),/g),
  ].map((m) => m[1] ?? m[2]));
  assert.ok(used.size > 0, "no cell namespace reads matched");
  for (const name of used) assert.ok(declared.has(name), `env.${name} is not a wrangler binding`);
});
