//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The harness end to end: an ssh upload yields a URL whose shell renders the
// markdown into the DOM, with nothing logged as an error.
func TestHarness(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte("# harness\n\nrendered in the browser.\n"), UploadOpts{
		Type: "md",
		Name: "harness proof",
	})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "harness")
	br.Open(t, paste.URL)

	var heading string
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		chromedp.WaitVisible("#content h1", chromedp.ByQuery),
		chromedp.Text("#content h1", &heading, chromedp.ByQuery),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("wait for rendered markdown: %v\npage errors: %v", err, br.Errors())
	}
	flow.Shot("rendered")

	if heading != "harness" {
		t.Errorf("rendered h1 = %q, want %q", heading, "harness")
	}
	br.AssertNoPageErrors(t)
}

// A script the page asked for and did not get is reported, so a clean error log
// is evidence rather than a filter that swallows everything.
func TestHarnessReportsMissingSubresource(t *testing.T) {
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
