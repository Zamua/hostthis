package cache

import (
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// pasteCacheURLs returns every CDN cache key a paste is reachable at, so a
// purge leaves nothing stale. Provider-agnostic: every adapter shares this
// policy and differs only in how it submits the list.
//
// A client-rendered paste serves its shell at the base URL and the bytes at
// "?raw=1", a SEPARATE cache entry, so BOTH must be purged or an edit shows
// stale content until max-age expires. For an HTML paste the extra purge is a
// harmless no-op. The "?raw=1" suffix MUST match what the render shell
// fetches; TestMdShell_FetchesRawQuery pins the two.
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
