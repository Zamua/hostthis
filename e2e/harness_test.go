//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// A script the page asked for and did not get is reported, so a clean error log
// is evidence rather than a filter that swallows everything.
func TestHarnessReportsMissingSubresource(t *testing.T) {
	t.Parallel()
	srv := StartServer(t)
	paste := srv.Upload(t, []byte("# probe\n"), UploadOpts{Type: "md"})

	br := NewBrowser(t)
	br.Open(t, paste.URL)
	if err := chromedp.Run(br.Ctx,
		chromedp.WaitVisible("#content h1", chromedp.ByQuery),
		chromedp.Evaluate(`document.head.appendChild(
			Object.assign(document.createElement("script"), {src: "/_hostthis/no-such-asset.js"}))`, nil),
	); err != nil {
		t.Fatalf("inject missing script: %v", err)
	}

	// The load failure arrives asynchronously over CDP, so it is not readable
	// on the line after the injection.
	var got []string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if got = br.Errors(); len(got) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatal("a 404 script was reported as no error at all")
	}
	if !strings.Contains(strings.Join(got, "\n"), "no-such-asset.js") {
		t.Errorf("errors do not name the missing asset: %v", got)
	}
}
