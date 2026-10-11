// Request paths as hostthisd's Go server sees them (docs/SPEC.md "Public and
// internal hosts"). Pure functions, no I/O.

// path.Clean.
function clean(p) {
  const rooted = p.startsWith("/");
  const out = [];
  for (const part of p.split("/")) {
    if (part === "" || part === ".") {
      continue;
    }
    if (part === "..") {
      if (out.length && out[out.length - 1] !== "..") {
        out.pop();
      } else if (!rooted) {
        out.push("..");
      }
      continue;
    }
    out.push(part);
  }
  const joined = out.join("/");
  return rooted ? `/${joined}` : joined || ".";
}

// ServeMux's cleanPath: rooted, cleaned, a trailing slash kept.
export function cleanPath(p) {
  if (p === "") {
    return "/";
  }
  const rooted = p.startsWith("/") ? p : `/${p}`;
  const cleaned = clean(rooted);
  return rooted.endsWith("/") && cleaned !== "/" ? `${cleaned}/` : cleaned;
}

// The path percent-decoded once, as Go's URL.Path is, as a byte string (one
// char per byte, so bytes that are not UTF-8 survive); null for a malformed
// escape. The runtime hands over an ASCII path.
export function decodePath(pathname) {
  let out = "";
  for (let i = 0; i < pathname.length; i++) {
    if (pathname[i] !== "%") {
      out += pathname[i];
      continue;
    }
    const hex = pathname.slice(i + 1, i + 3);
    if (!/^[0-9a-fA-F]{2}$/.test(hex)) {
      return null;
    }
    out += String.fromCharCode(parseInt(hex, 16));
    i += 2;
  }
  return out;
}

// url.URL.EscapedPath for a byte string path.
export function escapePath(path) {
  let out = "";
  for (let i = 0; i < path.length; i++) {
    const c = path[i];
    if (/[A-Za-z0-9\-_.~$&+,/:;=@]/.test(c)) {
      out += c;
    } else {
      out += `%${path.charCodeAt(i).toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return out;
}

const HTML_ESCAPES = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&#34;", "'": "&#39;" };

// http.Redirect with 307: for GET and HEAD a Content-Type, and for GET the
// body it writes.
function temporaryRedirect(method, location) {
  const headers = { Location: location };
  if (method !== "GET" && method !== "HEAD") {
    return new Response(null, { status: 307, headers });
  }
  headers["Content-Type"] = "text/html; charset=utf-8";
  if (method === "HEAD") {
    return new Response(null, { status: 307, headers });
  }
  const href = location.replace(/[&<>"']/g, (c) => HTML_ESCAPES[c]);
  return new Response(`<a href="${href}">Temporary Redirect</a>.\n\n`, { status: 307, headers });
}

// ServeMux's redirect of a path that is not clean: 307, query kept. Null for a
// clean path.
export function cleanRedirect(method, pathname, search) {
  const cleaned = cleanPath(pathname);
  if (cleaned === pathname) {
    return null;
  }
  return temporaryRedirect(method, escapePath(decodePath(cleaned) ?? cleaned) + search);
}

// The one subtree hostthisd registers, /_hostthis/, which ServeMux answers
// for its root named without the slash with a 307 to /_hostthis/, ahead of
// any cleaning. Null for any other path.
export function subtreeRedirect(method, pathname, search) {
  if (decodePath(cleanPath(pathname)) !== "/_hostthis") {
    return null;
  }
  return temporaryRedirect(method, `/_hostthis/${search}`);
}
