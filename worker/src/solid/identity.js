// hostthis as its own Solid agent (docs/SPEC.md "Pod storage"): its WebID,
// its issuer documents, and the access tokens it signs for itself.
import { dpopProof, generateKey, importSigningKey, nowSeconds, publicJwk, randomToken, signJwt, thumbprint } from "./jose.js";

const TOKEN_TTL = 120;
// A token is reused until this close to expiry, so a pod never sees one expire
// mid-request.
const TOKEN_MARGIN = 30;

export function solidEnabled(env) {
  return Boolean(env.HOSTTHIS_SOLID_KEY) && Boolean(env.HOSTTHIS_APEX_DOMAIN);
}

// The public origin the WebID and issuer live at.
export function solidOrigin(env) {
  if (env.HOSTTHIS_SOLID_ORIGIN) {
    return env.HOSTTHIS_SOLID_ORIGIN.replace(/\/+$/, "");
  }
  return `${env.HOSTTHIS_PUBLIC_SCHEME || "https"}://${env.HOSTTHIS_APEX_DOMAIN}`;
}

export function solidUrls(env) {
  const origin = solidOrigin(env);
  const issuer = `${origin}/_solid`;
  return {
    issuer,
    webid: `${issuer}/id#me`,
    profile: `${issuer}/id`,
    jwks: `${issuer}/jwks`,
    client: `${issuer}/client`,
    callback: `${issuer}/callback`,
  };
}

function signingJwk(env) {
  return JSON.parse(env.HOSTTHIS_SOLID_KEY);
}

function kidOf(jwk) {
  return jwk.kid || "hostthis";
}

export function webidProfile(env) {
  const urls = solidUrls(env);
  return `@prefix solid: <http://www.w3.org/ns/solid/terms#>.
@prefix foaf: <http://xmlns.com/foaf/0.1/>.

<#me> a foaf:Agent;
  foaf:name "hostthis";
  solid:oidcIssuer <${urls.issuer}>.
`;
}

export function issuerConfiguration(env) {
  const urls = solidUrls(env);
  return { issuer: urls.issuer, jwks_uri: urls.jwks };
}

export function jwks(env) {
  const jwk = signingJwk(env);
  return { keys: [{ ...publicJwk(jwk), kid: kidOf(jwk), alg: "ES256", use: "sig" }] };
}

export function clientDocument(env) {
  const urls = solidUrls(env);
  return {
    "@context": ["https://www.w3.org/ns/solid/oidc-context.jsonld"],
    client_id: urls.client,
    client_name: "hostthis",
    client_uri: solidOrigin(env),
    redirect_uris: [urls.callback],
    grant_types: ["authorization_code"],
    response_types: ["code"],
    scope: "openid webid",
    token_endpoint_auth_method: "none",
  };
}

// Per isolate and pod origin: a DPoP key and the token bound to it. A token is
// valid at any pod that shares with hostthis, so one sent to a pod only ever
// pairs with proofs made for that pod, and a pod that ignored DPoP binding
// would hold it for at most TOKEN_TTL.
const sessions = new Map();

async function freshSession(env) {
  const jwk = signingJwk(env);
  const dpop = await generateKey();
  const urls = solidUrls(env);
  const iat = nowSeconds();
  const token = await signJwt(
    { alg: "ES256", kid: kidOf(jwk), typ: "at+jwt" },
    {
      iss: urls.issuer,
      aud: "solid",
      sub: urls.webid,
      webid: urls.webid,
      client_id: urls.client,
      azp: urls.client,
      cnf: { jkt: await thumbprint(dpop.jwk) },
      iat,
      exp: iat + TOKEN_TTL,
      jti: randomToken(16),
    },
    await importSigningKey(jwk),
  );
  return { token, dpop, exp: iat + TOKEN_TTL, key: env.HOSTTHIS_SOLID_KEY };
}

// The headers one pod request carries as hostthis's WebID.
export async function agentHeaders(env, method, url) {
  const origin = new URL(url).origin;
  let session = sessions.get(origin);
  if (!session || session.key !== env.HOSTTHIS_SOLID_KEY || session.exp - TOKEN_MARGIN <= nowSeconds()) {
    session = await freshSession(env);
    sessions.set(origin, session);
  }
  return {
    Authorization: `DPoP ${session.token}`,
    DPoP: await dpopProof(session.dpop, method, url, session.token),
  };
}
