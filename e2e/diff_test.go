//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// diffFixture is a two-file git diff so the file-name assertion cannot pass on
// a single accidental match. The counts are asymmetric (4 added, 2 removed,
// split 3+1 and 2+0 across the files) so a counter reading the wrong class or
// the wrong file reads as a mismatch, not a coincidence.
const diffFixture = `diff --git a/greet.go b/greet.go
index 1111111..2222222 100644
--- a/greet.go
+++ b/greet.go
@@ -1,7 +1,8 @@
 package main
 
-func greet(name string) string {
-	return "hi " + name
+// greet returns a friendly greeting.
+func greet(name string) string {
+	return "hello, " + name + "!"
 }
 
 func main() {
diff --git a/greet_test.go b/greet_test.go
index 3333333..4444444 100644
--- a/greet_test.go
+++ b/greet_test.go
@@ -3,6 +3,7 @@ package main
 import "testing"
 
 func TestGreet(t *testing.T) {
+	t.Parallel()
 	if greet("x") == "" {
 		t.Fatal("empty greeting")
 	}
`

// fileNamesJS reads the per-file block headers. Scoped to .d2h-file-wrapper so
// the file LIST at the top, which repeats the names, is not double-counted.
const fileNamesJS = `Array.from(document.querySelectorAll("#diff .d2h-file-wrapper .d2h-file-name"), e => e.textContent.trim())`

// The content cell and its line-number cell both carry the change class, so
// line counts exclude the number cell to count each line once.
const (
	insCountJS = `document.querySelectorAll("#diff td.d2h-ins:not(.d2h-code-linenumber)").length`
	delCountJS = `document.querySelectorAll("#diff td.d2h-del:not(.d2h-code-linenumber)").length`
)

// hljsCountJS counts highlight.js spans inside code lines. Any is proof the
// highlighter ran over the diffed source; the exact token classes are hljs's.
const hljsCountJS = `document.querySelectorAll('#diff .d2h-code-line-ctn [class*="hljs-"]').length`

// sideBySideCountJS counts the panes only a side-by-side render emits: two per
// file. Line-by-line output has none, so the count IS the layout mode.
const sideBySideCountJS = `document.querySelectorAll("#diff .d2h-file-side-diff").length`

const sidePressedJS = `document.getElementById("btn-side").getAttribute("aria-pressed") === "true"`

// The diff viewer renders one block per file with diff2html, highlights the
// code, and rebuilds the layout when the side-by-side toggle is pressed. A
// bare git-diff body reaches the same shell with no --type hint.
func TestDiffRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte(diffFixture), UploadOpts{Type: "diff", Name: "diff proof"})

	// Sniffing check: the hunk header alone must route an untyped upload to the
	// diff shell, and diff.js is served by no other shell.
	sniffed := srv.Upload(t, []byte(diffFixture), UploadOpts{})
	srv.WaitReady(t, sniffed)
	if body := fetchBody(t, sniffed.URL); !strings.Contains(body, "/_hostthis/diff.js") {
		t.Errorf("untyped git-diff upload was not served the diff shell; page:\n%.400s", body)
	}

	br := NewBrowser(t)
	flow := NewFlow(t, br, "diff-render")
	br.Open(t, paste.URL)

	// aria-busy starts true in the static shell and flips to false only when
	// the renderer finished or the fetch failed, so waiting on it reports a
	// failed load through the assertions below instead of as a timeout. Scoped
	// under the browser cap so a stuck render leaves a live context to
	// screenshot from; waiting on br.Ctx would burn the budget and kill it.
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		chromedp.WaitVisible(`#diff[aria-busy="false"]`, chromedp.ByQuery),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("diff shell never settled: %v\n#diff: %q\npage errors: %v",
			err, diffShellText(t, br), br.Errors())
	}
	flow.Shot("line-by-line")

	var (
		fileNames                              []string
		insCount, delCount, hljsCount, sideCnt int
	)
	if err := chromedp.Run(br.Ctx,
		chromedp.Evaluate(fileNamesJS, &fileNames),
		chromedp.Evaluate(insCountJS, &insCount),
		chromedp.Evaluate(delCountJS, &delCount),
		chromedp.Evaluate(hljsCountJS, &hljsCount),
		chromedp.Evaluate(sideBySideCountJS, &sideCnt),
	); err != nil {
		t.Fatalf("probe rendered page: %v", err)
	}

	if want := []string{"greet.go", "greet_test.go"}; !slices.Equal(fileNames, want) {
		t.Fatalf("file blocks = %q, want %q\n#diff: %q", fileNames, want, diffShellText(t, br))
	}
	if insCount != 4 || delCount != 2 {
		t.Errorf("rendered %d added and %d removed line(s), want 4 and 2", insCount, delCount)
	}
	if hljsCount == 0 {
		t.Errorf("no highlight.js spans in the code lines: the highlighter did not run")
	}
	// A fresh browser profile has no persisted format, so the first render must
	// be line-by-line, which makes the toggle below an observable change.
	if sideCnt != 0 {
		t.Fatalf("initial render already has %d side-by-side pane(s), want 0", sideCnt)
	}

	// The toggle re-renders through the same pipeline; waiting on the panes it
	// alone produces means a click that never reached the handler fails as a
	// timeout here rather than as a count mismatch.
	var sideAfter int
	var sidePressed bool
	toggleCtx, cancelToggle := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelToggle()
	if err := chromedp.Run(toggleCtx,
		chromedp.Click("#btn-side", chromedp.ByQuery),
		chromedp.Poll(sideBySideCountJS+" > 0", nil),
		chromedp.Evaluate(sideBySideCountJS, &sideAfter),
		chromedp.Evaluate(sidePressedJS, &sidePressed),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("side-by-side toggle never re-rendered: %v\npage errors: %v", err, br.Errors())
	}
	flow.Shot("side-by-side")

	if sideAfter != 4 {
		t.Errorf("side-by-side render has %d pane(s), want 4 (two per file)", sideAfter)
	}
	if !sidePressed {
		t.Errorf("side-by-side button is not aria-pressed after the switch")
	}

	br.AssertNoPageErrors(t)
}

// diffShellText is what the shell left in #diff, for a failure message.
func diffShellText(t *testing.T, br *Browser) string {
	t.Helper()
	var s string
	if err := chromedp.Run(br.Ctx,
		chromedp.Evaluate(`(document.getElementById("diff") || {}).textContent || ""`, &s),
	); err != nil {
		return "unreadable: " + err.Error()
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// fetchBody reads a paste page over plain http, for assertions on which shell
// the server chose rather than on what a browser rendered.
func fetchBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b)
}
