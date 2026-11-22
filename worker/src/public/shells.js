// The render shells hostthisd serves, from web/shells: the build resolves
// "hostthis:shells" to shells.json and the page templates. Pure, no I/O.
import { r2, table, templates } from "hostthis:shells";

function own(object, key) {
  return Object.hasOwn(object, key) ? object[key] : undefined;
}

const assets = new Map();
for (const shell of Object.values(table.shells)) {
  for (const [name, contentType] of Object.entries(shell.assets)) {
    assets.set(name, { contentType, key: `static/${shell.version}/${name}`, r2: r2.includes(name) });
  }
}

// The generic title every template carries, once.
export const GENERIC_TITLE = "<title>hostthis</title>";

// The page for a client-rendered kind, byte for byte as hostthisd serves it:
// body, version (the ETag) and Content-Security-Policy. null for any other
// kind. A title replaces the generic one and adds the Open Graph tags a link
// unfurler reads, since none runs the page's script; without one the page is
// the template's own.
export function renderShell(kind, title = "") {
  const k = own(table.kinds, kind);
  if (!k) return null;
  const shell = table.shells[k.shell];
  // Substituted in hostthisd's order, by literal replacement.
  let body = templates[k.shell].split("__VER__").join(shell.version).split("__KIND__").join(kind);
  if (title) {
    body = body.replace(GENERIC_TITLE, () => titleTags(escapeHTML(title)));
  }
  return { body, version: shell.version, csp: table.csp[shell.csp] };
}

function titleTags(title) {
  return `<title>${title}</title>
<meta property="og:title" content="${title}">
<meta property="og:type" content="article">
<meta name="twitter:card" content="summary">`;
}

// Text as HTML text and as a quoted attribute value alike.
function escapeHTML(text) {
  return text.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
}

// The Content-Type of a client-rendered kind's ?raw response, or null.
export function rawContentType(kind) {
  return own(table.kinds, kind)?.raw ?? null;
}

// A whitelisted /_hostthis/ asset: its Content-Type, the PAYLOADS key it is
// served from where static assets cannot hold it, and whether they cannot
// (r2). null for any other name.
export function asset(name) {
  return assets.get(name) ?? null;
}
