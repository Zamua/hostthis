// Conditional requests on the read path, by hostthisd's rules with one
// deliberate difference: If-None-Match compares weakly. Pure, no I/O.
import { parseHTTPDate } from "./httpdate.js";

// Go's strings.TrimSpace over a header's bytes.
const trim = (text) => text.replace(/^[\t\n\v\f\r ]+|[\t\n\v\f\r ]+$/g, "");

const opaque = (tag) => (tag.startsWith("W/") ? tag.slice(2) : tag);

// If-None-Match is "*" or lists etag, weakly: an edge that compresses a
// response turns its strong ETag into W/"...", and the revalidation must still
// match (RFC 9110 section 13.1.2).
export function etagMatches(header, etag) {
  if (!header) {
    return false;
  }
  if (trim(header) === "*") {
    return true;
  }
  return header.split(",").some((candidate) => opaque(trim(candidate)) === opaque(etag));
}

// Whether to answer 304: If-None-Match matches when the request has one (RFC
// 9110 section 13.2.2: it then decides alone), else If-Modified-Since is at or
// after updatedAt (ms) truncated to the second. An empty header is none, as
// Go's Header.Get reads it.
export function notModified(headers, etag, updatedAt) {
  const inm = headers.get("If-None-Match");
  if (inm) {
    return etagMatches(inm, etag);
  }
  const since = headers.get("If-Modified-Since");
  if (!since) {
    return false;
  }
  const seconds = parseHTTPDate(since);
  return seconds !== null && Math.floor(updatedAt / 1000) <= seconds;
}
