// The render shells hostthisd serves, from web/shells: the build resolves
// "hostthis:shells" to shells.json and the page templates. Pure, no I/O.
import { table, templates } from "hostthis:shells";

function own(object, key) {
  return Object.hasOwn(object, key) ? object[key] : undefined;
}

const assets = new Map();
for (const shell of Object.values(table.shells)) {
  for (const [name, contentType] of Object.entries(shell.assets)) {
    assets.set(name, { contentType, key: `static/${shell.version}/${name}` });
  }
}

// The page for a client-rendered kind, byte for byte as hostthisd serves it:
// body, version (the ETag) and Content-Security-Policy. null for any other
// kind.
export function renderShell(kind) {
  const k = own(table.kinds, kind);
  if (!k) return null;
  const shell = table.shells[k.shell];
  // Substituted in hostthisd's order, by literal replacement.
  const body = templates[k.shell].split("__VER__").join(shell.version).split("__KIND__").join(kind);
  return { body, version: shell.version, csp: table.csp[shell.csp] };
}

// The Content-Type of a client-rendered kind's ?raw response, or null.
export function rawContentType(kind) {
  return own(table.kinds, kind)?.raw ?? null;
}

// A whitelisted /_hostthis/ asset: its Content-Type and the PAYLOADS key it is
// served from where static assets cannot hold it. null for any other name.
export function asset(name) {
  return assets.get(name) ?? null;
}
