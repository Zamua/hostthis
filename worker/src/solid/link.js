// The apex /_solid/ surface: hostthis's identity documents and the pod link
// flow (docs/SPEC.md "Pod storage").
import {
  clientDocument, issuerConfiguration, jwks, solidEnabled, solidOrigin, solidUrls, webidProfile,
} from "./identity.js";
import { b64url, dpopProof, jwtClaims, randomToken, sha256 } from "./jose.js";
import { linkCellName } from "./pod.js";

const LINK_PATH = /^\/_solid\/link\/([a-f0-9]{16})$/;
const SOLID = "http://www.w3.org/ns/solid/terms#";
const PIM = "http://www.w3.org/ns/pim/space#";
const ACP_ACR = "http://www.w3.org/ns/solid/acp#AccessControlResource";
const ES256 = { name: "ECDSA", namedCurve: "P-256" };

function cell(env, name, op, body) {
  const url = `https://cell/identity/${op}?scope=${encodeURIComponent(name)}`;
  const stub = env.IDENTITY.get(env.IDENTITY.idFromName(name));
  return body === undefined
    ? stub.fetch(url)
    : stub.fetch(url, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) });
}

async function loadLink(env, id) {
  const res = await cell(env, linkCellName(id), "linkget");
  return res.ok ? res.json() : null;
}

// The fingerprint `whoami` shows, without the identity namespace.
function keyOf(owner) {
  return String(owner).replace(/^key:/, "");
}

function escapeHtml(text) {
  return String(text).replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
}

function page(status, title, body) {
  const html = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>${escapeHtml(title)}</title>
<style>body{font:16px/1.5 ui-monospace,Menlo,monospace;max-width:40rem;margin:2rem auto;padding:0 1rem;background:#fff;color:#111}
@media (prefers-color-scheme:dark){body{background:#111;color:#eee}}button{font:inherit;padding:.5rem 1rem}code{word-break:break-all}</style>
<h1>${escapeHtml(title)}</h1>${body}`;
  return new Response(html, {
    status,
    headers: {
      "Content-Type": "text/html; charset=utf-8",
      "Cache-Control": "no-store",
      "Referrer-Policy": "no-referrer",
      // No form-action: Continue posts here and is redirected to the owner's
      // provider, and browsers apply form-action to that redirect too.
      "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

function dead() {
  return page(410, "Link expired", "<p>This link expired or was already used. Run <code>ssh hostthis.dev pod link &lt;webid&gt;</code> again.</p>");
}

function failed(message) {
  return page(502, "Link failed", `<p>${escapeHtml(message)}</p><p>Nothing was linked. Run <code>pod link</code> again to retry.</p>`);
}

async function hashHex(text) {
  return [...(await sha256(text))].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// The link, when its secret is right and it is still pending and unexpired.
async function liveLink(env, id, secret) {
  const link = await loadLink(env, id);
  if (!link || link.state !== "pending" || Date.now() >= link.expires || typeof secret !== "string") {
    return null;
  }
  return (await hashHex(secret)) === link.secretHash ? link : null;
}

function resolveIri(value, base) {
  try {
    return new URL(value, base).href;
  } catch {
    return null;
  }
}

// The WebID's issuer and storage, from its profile as JSON-LD or Turtle.
async function readProfile(webid) {
  const res = await fetch(webid, { headers: { Accept: "application/ld+json, text/turtle;q=0.9" } });
  if (!res.ok) {
    throw new Error(`your WebID profile answered ${res.status}`);
  }
  const type = res.headers.get("Content-Type") ?? "";
  const text = await res.text();
  const found = { issuer: null, storage: null };
  if (type.includes("json")) {
    const doc = JSON.parse(text);
    const nodes = Array.isArray(doc) ? doc : (doc["@graph"] ?? [doc]);
    const first = (node, iri) => {
      const ref = [].concat(node[iri] ?? [])[0];
      return typeof ref === "string" ? ref : ref?.["@id"];
    };
    for (const node of nodes) {
      if (resolveIri(node["@id"] ?? "", webid) !== webid) continue;
      found.issuer ??= first(node, `${SOLID}oidcIssuer`);
      found.storage ??= first(node, `${PIM}storage`);
    }
  } else {
    found.issuer = /(?:solid:oidcIssuer|<http:\/\/www\.w3\.org\/ns\/solid\/terms#oidcIssuer>)\s+<([^>]*)>/.exec(text)?.[1];
    found.storage = /(?:pim:storage|space:storage|<http:\/\/www\.w3\.org\/ns\/pim\/space#storage>)\s+<([^>]*)>/.exec(text)?.[1];
  }
  const issuer = found.issuer && resolveIri(found.issuer, webid);
  const storage = found.storage && resolveIri(found.storage, webid);
  if (!issuer) throw new Error("your WebID profile names no solid:oidcIssuer");
  return { issuer, storage: storage ? (storage.endsWith("/") ? storage : `${storage}/`) : null };
}

async function issuerConfig(issuer) {
  const res = await fetch(`${issuer.replace(/\/$/, "")}/.well-known/openid-configuration`, { headers: { Accept: "application/json" } });
  if (!res.ok) {
    throw new Error(`your identity provider answered ${res.status}`);
  }
  return res.json();
}

// The client this link signs in as. A provider that offers dynamic
// registration gets a registered client: some (the Nextcloud Solid app among
// them) lose the authorization result after their approval page for a Client
// ID Document client. Otherwise, or if registration fails, the document.
async function signInClient(env, config) {
  const urls = solidUrls(env);
  if (typeof config.registration_endpoint === "string") {
    try {
      const res = await fetch(config.registration_endpoint, {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({
          client_name: "hostthis",
          client_uri: solidOrigin(env),
          redirect_uris: [urls.callback],
          grant_types: ["authorization_code"],
          response_types: ["code"],
          scope: "openid webid",
          application_type: "web",
          token_endpoint_auth_method: "client_secret_basic",
        }),
      });
      const reg = res.ok ? await res.json() : null;
      if (typeof reg?.client_id === "string" && reg.client_id) {
        return { id: reg.client_id, secret: typeof reg.client_secret === "string" ? reg.client_secret : undefined };
      }
    } catch {
      // Fall back to the Client ID Document.
    }
  }
  return { id: urls.client };
}

async function confirmPage(env, id, url) {
  const secret = url.searchParams.get("k");
  const link = await liveLink(env, id, secret);
  if (!link) {
    return dead();
  }
  return page(200, "Link your pod to hostthis", `
<p>Key: <code>${escapeHtml(keyOf(link.owner))}</code></p>
<p>WebID: <code>${escapeHtml(link.webid)}</code></p>
<p>Continue only if you ran <code>pod link</code> yourself. You will sign in to your pod provider; hostthis
then shares one folder of your pod, <code>hostthis/</code>, with its own identity, and forgets your session.</p>
<form method="post" action="/_solid/link/${id}"><input type="hidden" name="k" value="${escapeHtml(secret)}"><button>Continue</button></form>`);
}

async function beginLogin(env, id, request) {
  const form = await request.formData().catch(() => null);
  const link = await liveLink(env, id, form?.get("k"));
  if (!link) {
    return dead();
  }
  let profile;
  let config;
  try {
    profile = await readProfile(link.webid);
    config = await issuerConfig(profile.issuer);
  } catch (error) {
    return failed(error.message);
  }
  const urls = solidUrls(env);
  const verifier = randomToken(32);
  const state = `${id}.${randomToken(16)}`;
  const pair = await crypto.subtle.generateKey(ES256, true, ["sign", "verify"]);
  const dpop = await crypto.subtle.exportKey("jwk", pair.privateKey);
  const client = await signInClient(env, config);
  const begun = await cell(env, linkCellName(id), "linkbegin", {
    storage: profile.storage,
    login: { state, verifier, dpop, tokenEndpoint: config.token_endpoint, client },
  });
  if (!begun.ok) {
    return dead();
  }
  const auth = new URL(config.authorization_endpoint);
  auth.search = new URLSearchParams({
    client_id: client.id,
    redirect_uri: urls.callback,
    response_type: "code",
    scope: "openid webid",
    code_challenge: b64url(await sha256(verifier)),
    code_challenge_method: "S256",
    state,
    prompt: "consent",
  }).toString();
  return new Response(null, { status: 303, headers: { Location: auth.href, "Cache-Control": "no-store" } });
}

async function sessionKey(jwk) {
  const privateKey = await crypto.subtle.importKey("jwk", jwk, ES256, false, ["sign"]);
  return { privateKey, jwk: { crv: jwk.crv, kty: jwk.kty, x: jwk.x, y: jwk.y } };
}

function headLinks(res) {
  const links = [];
  for (const part of (res.headers.get("Link") ?? "").split(/,(?=\s*<)/)) {
    const match = /<([^>]*)>\s*;(.*)/.exec(part.trim());
    if (match) {
      const rel = /rel="?([^";]+)"?/.exec(match[2])?.[1] ?? "";
      links.push({ href: match[1], rels: rel.split(/\s+/) });
    }
  }
  return links;
}

function acpRule(container, owner, agent) {
  return `@prefix acp: <http://www.w3.org/ns/solid/acp#>.
@prefix acl: <http://www.w3.org/ns/auth/acl#>.
<#owner> a acp:AccessControl; acp:apply <#ownerPolicy>.
<#ownerPolicy> a acp:Policy; acp:allow acl:Read, acl:Write, acl:Append, acl:Control; acp:allOf <#ownerMatcher>.
<#ownerMatcher> a acp:Matcher; acp:agent <${owner}>.
<#hostthis> a acp:AccessControl; acp:apply <#hostthisPolicy>.
<#hostthisPolicy> a acp:Policy; acp:allow acl:Read, acl:Write, acl:Append; acp:allOf <#hostthisMatcher>.
<#hostthisMatcher> a acp:Matcher; acp:agent <${agent}>.
<> acp:resource <${container}>; acp:accessControl <#owner>, <#hostthis>; acp:memberAccessControl <#owner>, <#hostthis>.
`;
}

function wacRule(container, owner, agent) {
  return `@prefix acl: <http://www.w3.org/ns/auth/acl#>.
<#owner> a acl:Authorization; acl:agent <${owner}>; acl:accessTo <${container}>; acl:default <${container}>;
  acl:mode acl:Read, acl:Write, acl:Control.
<#hostthis> a acl:Authorization; acl:agent <${agent}>; acl:accessTo <${container}>; acl:default <${container}>;
  acl:mode acl:Read, acl:Write.
`;
}

function ownerFetch(session, accessToken) {
  return async (method, url, init = {}) => fetch(url, {
    ...init,
    method,
    headers: { ...(init.headers ?? {}), Authorization: `DPoP ${accessToken}`, DPoP: await dpopProof(session, method, url, accessToken) },
  });
}

// A profile without pim:storage: the nearest container above the WebID that
// answers as a storage (Solid Protocol, "Storage").
async function discoverStorage(call, webid) {
  const url = new URL(webid);
  url.hash = "";
  url.search = "";
  let path = url.pathname.slice(0, url.pathname.lastIndexOf("/") + 1);
  for (;;) {
    const res = await call("HEAD", `${url.origin}${path}`);
    if (headLinks(res).some((l) => l.href === `${PIM}Storage` && l.rels.includes("type"))) {
      return `${url.origin}${path}`;
    }
    if (path === "/") {
      throw new Error("no storage found above your WebID");
    }
    path = path.slice(0, path.slice(0, -1).lastIndexOf("/") + 1);
  }
}

// Creates <storage>hostthis/ and shares it with hostthis, as the owner.
async function shareContainer(env, link, call) {
  const container = `${link.storage}hostthis/`;
  let res = await call("HEAD", container);
  if (res.status === 404) {
    res = await call("PUT", container, { headers: { "Content-Type": "text/turtle" }, body: "" });
    if (!res.ok) {
      throw new Error(`your pod answered ${res.status} creating ${container}`);
    }
    res = await call("HEAD", container);
  }
  if (!res.ok) {
    throw new Error(`your pod answered ${res.status} for ${container}`);
  }
  const acl = headLinks(res).find((l) => l.rels.includes("acl"));
  if (!acl) {
    throw new Error("your pod names no access rule resource for the folder");
  }
  const aclUrl = new URL(acl.href, container).href;
  const aclHead = await call("HEAD", aclUrl);
  const acp = headLinks(aclHead).some((l) => l.href === ACP_ACR && l.rels.includes("type")) || aclUrl.endsWith(".acr");
  const rule = (acp ? acpRule : wacRule)(container, link.webid, solidUrls(env).webid);
  res = await call("PUT", aclUrl, { headers: { "Content-Type": "text/turtle" }, body: rule });
  if (!res.ok) {
    throw new Error(`your pod answered ${res.status} writing the share rule`);
  }
}

async function callback(env, url) {
  const state = url.searchParams.get("state") ?? "";
  const id = /^([a-f0-9]{16})\./.exec(state)?.[1];
  const link = id && await loadLink(env, id);
  const login = link?.logins?.find((l) => l.state === state);
  if (!link || link.state !== "pending" || Date.now() >= link.expires || !login) {
    return dead();
  }
  const code = url.searchParams.get("code");
  if (!code) {
    return failed(`your provider answered ${url.searchParams.get("error") ?? "without a code"}`);
  }
  const urls = solidUrls(env);
  const session = await sessionKey(login.dpop);
  let token;
  try {
    const res = await fetch(login.tokenEndpoint, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        DPoP: await dpopProof(session, "POST", login.tokenEndpoint),
        ...(login.client?.secret
          ? { Authorization: `Basic ${btoa(`${encodeURIComponent(login.client.id)}:${encodeURIComponent(login.client.secret)}`)}` }
          : {}),
      },
      body: new URLSearchParams({
        grant_type: "authorization_code",
        code,
        redirect_uri: urls.callback,
        client_id: login.client?.id ?? urls.client,
        code_verifier: login.verifier,
      }),
    });
    token = await res.json();
    if (!res.ok || typeof token.access_token !== "string") {
      return failed(`your provider refused the sign-in (${token.error ?? res.status})`);
    }
  } catch (error) {
    return failed(`your provider could not be reached: ${error.message}`);
  }
  if (jwtClaims(token.access_token)?.webid !== link.webid) {
    return failed(`you signed in as a different WebID than ${link.webid}`);
  }
  const call = ownerFetch(session, token.access_token);
  try {
    link.storage ??= await discoverStorage(call, link.webid);
    await shareContainer(env, link, call);
  } catch (error) {
    return failed(error.message);
  }
  const linked = Date.now();
  const done = await cell(env, linkCellName(id), "linkcomplete", { linked, storage: link.storage });
  if (!done.ok) {
    return dead();
  }
  const set = await cell(env, link.owner, "podset", { id, webid: link.webid, storage: link.storage, linked });
  if (!set.ok) {
    return failed("the link could not be saved");
  }
  return page(200, "Pod linked", `<p>New uploads from <code>${escapeHtml(keyOf(link.owner))}</code> are stored in
<code>${escapeHtml(link.storage)}hostthis/</code>.</p><p>You can close this page.</p>`);
}

function json(body) {
  return new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json", "Cache-Control": "public, max-age=300" } });
}

// The /_solid/ answer for a request on the apex host, or null for any other
// path.
export async function serveSolid(request, env, path, url) {
  if (!path.startsWith("/_solid/")) {
    return null;
  }
  if (!solidEnabled(env)) {
    return null;
  }
  const method = request.method;
  if (method === "GET" || method === "HEAD") {
    switch (path) {
      case "/_solid/id":
        return new Response(webidProfile(env), { headers: { "Content-Type": "text/turtle", "Cache-Control": "public, max-age=300" } });
      case "/_solid/.well-known/openid-configuration":
        return json(issuerConfiguration(env));
      case "/_solid/jwks":
        return json(jwks(env));
      case "/_solid/client":
        return new Response(JSON.stringify(clientDocument(env)), { headers: { "Content-Type": "application/ld+json", "Cache-Control": "public, max-age=300" } });
      case "/_solid/callback":
        return callback(env, url);
    }
  }
  const linkId = LINK_PATH.exec(path)?.[1];
  if (linkId && method === "GET") {
    return confirmPage(env, linkId, url);
  }
  if (linkId && method === "POST") {
    return beginLogin(env, linkId, request);
  }
  return null;
}
