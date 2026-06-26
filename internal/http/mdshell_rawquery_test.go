package http

import (
	"strings"
	"testing"
)

// The markdown render shell (md.js) fetches the paste's raw bytes from a
// SEPARATE URL - "<path>?raw=1" - which a CDN caches as a distinct entry.
// The CDN purge in internal/cache/urls.go (pasteCacheURLs) hard-codes that
// same "?raw=1" suffix so an edited markdown paste invalidates BOTH cache
// keys. If md.js ever changes the query it fetches, that purge would
// silently stop matching and edits would serve stale content. This test
// pins the two in lockstep: if you change md.js's fetch query, you must
// update internal/cache/urls.go (and this test) to match.
func TestMdShell_FetchesRawQuery(t *testing.T) {
	b, err := mdShellFS.ReadFile("assets/mdshell/md.js")
	if err != nil {
		t.Fatalf("read md.js: %v", err)
	}
	if !strings.Contains(string(b), `"?raw=1"`) {
		t.Fatal(`md.js must fetch "?raw=1" - kept in lockstep with the CDN purge URL variants in internal/cache/urls.go; query not found in md.js`)
	}
}
