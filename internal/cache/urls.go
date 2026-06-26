package cache

import (
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// pasteCacheURLs returns every CDN cache key a paste is reachable at, so a
// purge leaves nothing stale. This is PROVIDER-AGNOSTIC: which URLs a paste
// occupies is a property of hostthis's URL scheme, not of any CDN, so every
// adapter (cloudflare, a future fastly, ...) shares this policy and differs
// only in how it submits the list to its purge API.
//
// A markdown paste serves its fixed, content-independent render shell at the
// base URL and the actual bytes at "?raw=1" (a SEPARATE cache entry the shell
// fetches client-side), so BOTH must be purged or an edit shows stale content
// until max-age expires. An HTML paste only uses the base URL; purging the
// extra "?raw=1" for it is a harmless no-op at the edge (nothing caches that
// key).
//
// The "?raw=1" suffix MUST match what the render shell actually fetches -
// internal/http/assets/mdshell/md.js does `fetch(location.pathname+"?raw=1")`.
// TestMdShell_FetchesRawQuery (internal/http) pins the two in lockstep so a
// change to md.js's query can't silently break markdown-edit invalidation.
func pasteCacheURLs(scheme, apex, mode string, slug domain.Slug) []string {
	if scheme == "" {
		scheme = "https"
	}
	var base string
	switch strings.ToLower(mode) {
	case "path":
		// Dev path mode: https://<apex>/p/<slug> (no trailing slash, matching
		// the URL hostthis emits). The shell fetches <path>?raw=1.
		base = scheme + "://" + apex + "/p/" + slug.String()
	default:
		// Subdomain mode (prod): the browser requests "/", so the cached key
		// carries the trailing slash; the shell fetches "/?raw=1".
		base = scheme + "://" + slug.String() + "." + apex + "/"
	}
	return []string{base, base + "?raw=1"}
}
