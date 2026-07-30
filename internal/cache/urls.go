package cache

import (
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// pasteCacheURLs returns every CDN cache key a paste is reachable at, so a
// purge leaves nothing stale. PROVIDER-AGNOSTIC: which URLs a paste occupies
// is a property of hostthis's URL scheme, so every adapter shares this policy
// and differs only in how it submits the list to its purge API.
//
// A markdown paste serves its content-independent render shell at the base URL
// and the actual bytes at "?raw=1", a SEPARATE cache entry the shell fetches
// client-side, so BOTH must be purged or an edit shows stale content until
// max-age expires. An HTML paste only uses the base URL; purging the extra
// "?raw=1" is a harmless no-op at the edge.
//
// The "?raw=1" suffix MUST match what the render shell fetches
// (internal/http/assets/mdshell/md.js). TestMdShell_FetchesRawQuery pins the
// two in lockstep so a change to md.js's query cannot silently break
// markdown-edit invalidation.
func pasteCacheURLs(scheme, apex, mode string, slug domain.Slug) []string {
	if scheme == "" {
		scheme = "https"
	}
	var base string
	switch strings.ToLower(mode) {
	case "path":
		// No trailing slash, matching the URL hostthis emits.
		base = scheme + "://" + apex + "/p/" + slug.String()
	default:
		// Subdomain mode: the browser requests "/", so the cached key carries
		// the trailing slash.
		base = scheme + "://" + slug.String() + "." + apex + "/"
	}
	return []string{base, base + "?raw=1"}
}
