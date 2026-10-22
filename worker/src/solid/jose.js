// The ES256 JWT and DPoP pieces Solid-OIDC needs, over WebCrypto.

const ES256 = { name: "ECDSA", namedCurve: "P-256" };
const SIGN = { name: "ECDSA", hash: "SHA-256" };
const encoder = new TextEncoder();

export function b64url(bytes) {
  let text = "";
  for (const byte of bytes) text += String.fromCharCode(byte);
  return btoa(text).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function b64urlText(text) {
  return b64url(encoder.encode(text));
}

export function randomToken(bytes = 32) {
  return b64url(crypto.getRandomValues(new Uint8Array(bytes)));
}

export async function sha256(text) {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", encoder.encode(text)));
}

// The public members of an EC JWK, in the order RFC 7638 hashes them.
export function publicJwk(jwk) {
  return { crv: jwk.crv, kty: jwk.kty, x: jwk.x, y: jwk.y };
}

export async function thumbprint(jwk) {
  return b64url(await sha256(JSON.stringify(publicJwk(jwk))));
}

export async function importSigningKey(jwk) {
  return crypto.subtle.importKey("jwk", { ...jwk, key_ops: ["sign"] }, ES256, false, ["sign"]);
}

// A fresh key pair, for a DPoP key that never leaves the isolate.
export async function generateKey() {
  const pair = await crypto.subtle.generateKey(ES256, true, ["sign", "verify"]);
  return { privateKey: pair.privateKey, jwk: publicJwk(await crypto.subtle.exportKey("jwk", pair.publicKey)) };
}

export async function signJwt(header, claims, key) {
  const input = `${b64urlText(JSON.stringify(header))}.${b64urlText(JSON.stringify(claims))}`;
  const signature = await crypto.subtle.sign(SIGN, key, encoder.encode(input));
  return `${input}.${b64url(new Uint8Array(signature))}`;
}

// A JWT's claims, unverified: only for a token this Worker just received from
// the issuer itself.
export function jwtClaims(token) {
  const part = typeof token === "string" ? token.split(".")[1] : undefined;
  if (!part) {
    return null;
  }
  try {
    const text = atob(part.replace(/-/g, "+").replace(/_/g, "/"));
    return JSON.parse(new TextDecoder().decode(Uint8Array.from(text, (c) => c.charCodeAt(0))));
  } catch {
    return null;
  }
}

export function nowSeconds() {
  return Math.floor(Date.now() / 1000);
}

// A DPoP proof for one request (RFC 9449); ath binds it to an access token.
export async function dpopProof(key, method, url, accessToken) {
  const target = new URL(url);
  const claims = { htm: method, htu: `${target.origin}${target.pathname}`, iat: nowSeconds(), jti: randomToken(16) };
  if (accessToken) {
    claims.ath = b64url(await sha256(accessToken));
  }
  return signJwt({ alg: "ES256", typ: "dpop+jwt", jwk: key.jwk }, claims, key.privateKey);
}
