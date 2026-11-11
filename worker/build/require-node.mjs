// Stops a build or test run on a Node older than .nvmrc names: the Worker
// tests pass or fail by Node version (scripts/require-node.sh is the Makefile's
// twin of this check).
import { readFileSync } from "node:fs";

const want = Number(readFileSync(new URL("../../.nvmrc", import.meta.url), "utf8").trim());
const have = Number(process.versions.node.split(".")[0]);
if (have < want) {
  console.error(`hostthis needs Node ${want} or newer; this is ${process.version}.`);
  process.exit(1);
}
