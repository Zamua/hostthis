package http

import (
	"strings"
	"testing"
)

// md.js fetches the paste's raw bytes from "<path>?raw=1", which a CDN caches
// as a distinct entry, so pasteCacheURLs in internal/cache/urls.go hard-codes
// the same suffix. Changing the query in one place and not the other leaves an
// edited markdown paste serving stale bytes with nothing failing.
func TestMdShell_FetchesRawQuery(t *testing.T) {
	b, err := mdShellFS.ReadFile("assets/mdshell/md.js")
	if err != nil {
		t.Fatalf("read md.js: %v", err)
	}
	if !strings.Contains(string(b), `"?raw=1"`) {
		t.Fatal(`md.js must fetch "?raw=1" - kept in lockstep with the CDN purge URL variants in internal/cache/urls.go; query not found in md.js`)
	}
}
