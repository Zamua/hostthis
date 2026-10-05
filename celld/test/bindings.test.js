import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// Every env binding the Worker reads is declared in wrangler.jsonc. A misspelt
// binding is a TypeError at runtime, and a try/catch around a cell call turns
// that into a silent retry loop.
test("every binding the Worker reads is declared", () => {
  const src = readFileSync(new URL("../src/index.js", import.meta.url), "utf8");
  const cfg = readFileSync(new URL("../wrangler.jsonc", import.meta.url), "utf8");
  const declared = new Set([...cfg.matchAll(/"(?:name|binding)":\s*"([A-Z_]+)"/g)].map((m) => m[1]));
  const used = new Set([
    ...src.matchAll(/\benv\.([A-Z_]+)\.(?:get|idFromName)\(|\bcellCall\(this\.env\.([A-Z_]+),/g),
  ].map((m) => m[1] ?? m[2]));
  assert.ok(used.size > 0, "no cell namespace reads matched");
  for (const name of used) assert.ok(declared.has(name), `env.${name} is not a wrangler binding`);
});

// celld registers only classes with a binding, so an unbound cell class
// cannot activate even when a migration declares it.
test("every exported cell class has a durable object binding", () => {
  const src = readFileSync(new URL("../src/index.js", import.meta.url), "utf8");
  const cfg = readFileSync(new URL("../wrangler.jsonc", import.meta.url), "utf8");
  const bound = new Set([...cfg.matchAll(/"class_name":\s*"([A-Za-z]+)"/g)].map((m) => m[1]));
  const classes = [...src.matchAll(/^export class ([A-Za-z]+)/gm)].map((m) => m[1]);
  assert.ok(classes.length > 0, "no exported classes matched");
  for (const name of classes) assert.ok(bound.has(name), `class ${name} has no binding`);
});
