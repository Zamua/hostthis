//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Viewport for every tab. Wide enough that a renderer's desktop layout is what
// gets photographed.
const (
	viewportWidth  = 1280
	viewportHeight = 900
)

// browserTimeout bounds one test's whole tab. It has to cover a cold fetch of
// a renderer's own libraries, not just a paint.
const browserTimeout = 90 * time.Second

// renderTimeout bounds one wait for a shell to settle. Shorter than the tab
// budget so a shell that never settles reports that, rather than spending the
// rest of the test's time on it.
const renderTimeout = 30 * time.Second

// browserLaunchAttempts bounds the launch retry in launchBrowser.
const browserLaunchAttempts = 2

// Browser is a headless tab plus every error its page reported. The error log
// is the cheap half of the blank-page check: a shell whose bundle 404s or
// throws renders nothing and says so only here.
type Browser struct {
	// Ctx is the chromedp context, already bounded by browserTimeout.
	Ctx context.Context

	mu       sync.Mutex
	problems []string
	ignored  []string
}

// Ignore drops recorded problems whose text contains substr, for a URL a
// fixture asks for ON PURPOSE. A sanitization payload keeps the classic
// broken-image src, whose 404 is the fixture working rather than the page
// failing. Filtering happens on read, so registration order does not matter.
// Keep the substring as specific as the URL itself: a loose one silences the
// blank-page evidence this type exists to collect.
func (b *Browser) Ignore(substr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ignored = append(b.ignored, substr)
}

var (
	browserOnce sync.Once
	// rootCtx is the tab whose first Run launched Chrome. Cancelling it ends the
	// process, so it lives until package teardown and every test tab hangs off
	// it.
	rootCtx     context.Context
	cancelRoot  context.CancelFunc
	cancelAlloc context.CancelFunc
	browserErr  error
)

// NewBrowser returns a fresh tab in the one Chrome the package shares, bound
// to the test's lifetime. The tab gets its own browser context, so storage a
// shell persists (the diff layout, for one) never leaks between tests.
func NewBrowser(t *testing.T) *Browser {
	t.Helper()
	browserOnce.Do(func() { browserErr = launchBrowser(t) })
	if browserErr != nil {
		t.Fatalf("start chrome after %d attempts: %v (set E2E_CHROME_PATH if it is installed somewhere unusual)",
			browserLaunchAttempts, browserErr)
	}

	// The tab is created by hand rather than through WithNewBrowserContext: a
	// context that owns no window yet refuses a plain createTarget in headless
	// Chrome ("no browser is open"), and only newWindow gets it one.
	var bcID cdp.BrowserContextID
	var tid target.ID
	browserExec := cdp.WithExecutor(rootCtx, chromedp.FromContext(rootCtx).Browser)
	if err := chromedp.Run(rootCtx, chromedp.ActionFunc(func(context.Context) error {
		var err error
		if bcID, err = target.CreateBrowserContext().Do(browserExec); err != nil {
			return err
		}
		tid, err = target.CreateTarget("about:blank").
			WithBrowserContextID(bcID).WithNewWindow(true).Do(browserExec)
		return err
	})); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	t.Cleanup(func() {
		_ = target.DisposeBrowserContext(bcID).Do(browserExec)
	})

	tabCtx, cancelTab := chromedp.NewContext(rootCtx, chromedp.WithTargetID(tid))
	ctx, cancelTimeout := context.WithTimeout(tabCtx, browserTimeout)
	t.Cleanup(cancelTimeout)
	t.Cleanup(cancelTab)

	b := &Browser{Ctx: ctx}
	chromedp.ListenTarget(ctx, b.record)
	// Log.enable both opens the tab and surfaces a script tag whose src 404s:
	// a failed subresource throws no JS exception and reaches the Log domain
	// only.
	if err := chromedp.Run(ctx, cdplog.Enable()); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	return b
}

// stopSharedBrowser closes Chrome. TestMain calls it after every test has run.
func stopSharedBrowser() {
	if cancelRoot != nil {
		cancelRoot()
		cancelAlloc()
	}
}

// launchBrowser starts headless Chrome through the root tab. E2E_CHROME_PATH
// pins the binary; without it chromedp searches the macOS bundle paths and the
// Linux names on PATH.
//
// The launch is retried once against a FRESH allocator. A CI runner under load
// can miss the DevTools websocket handshake, and that failure has nothing to
// do with the renderer a test is here to check. A second attempt cannot mask a
// real defect, because a browser that cannot start fails both times; the retry
// only removes a startup race from the verdict. Retrying in place would not
// work: a dead allocator stays dead, so each attempt builds its own.
func launchBrowser(t *testing.T) error {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.WindowSize(viewportWidth, viewportHeight),
		// The tab only ever loads a fixture server this process started, so the
		// sandbox guards nothing, while the user namespaces it needs are
		// restricted on common CI images.
		chromedp.NoSandbox,
	)
	if path := os.Getenv("E2E_CHROME_PATH"); path != "" {
		opts = append(opts, chromedp.ExecPath(path))
	}

	var lastErr error
	for attempt := 1; attempt <= browserLaunchAttempts; attempt++ {
		allocCtx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
		ctx, cancelR := chromedp.NewContext(allocCtx)
		// The root tab is never navigated, so a launch failure is the only
		// thing this Run can report.
		if lastErr = chromedp.Run(ctx); lastErr == nil {
			rootCtx, cancelRoot, cancelAlloc = ctx, cancelR, cancelA
			return nil
		}
		// Tear the failed attempt down immediately rather than deferring, so a
		// retry never races a half-dead browser.
		cancelR()
		cancelA()
		t.Logf("chrome launch attempt %d/%d failed: %v", attempt, browserLaunchAttempts, lastErr)
	}
	return lastErr
}

// Open navigates and returns once the document has loaded. A renderer fills its
// target element afterwards, so a test still waits on its own DOM signal.
func (b *Browser) Open(t *testing.T, url string) {
	t.Helper()
	if err := chromedp.Run(b.Ctx, chromedp.Navigate(url)); err != nil {
		t.Fatalf("navigate %s: %v", url, err)
	}
}

// Errors returns every page error recorded so far, minus the ones Ignore
// silenced.
func (b *Browser) Errors() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := make([]string, 0, len(b.problems))
	for _, p := range b.problems {
		if !containsAny(p, b.ignored) {
			kept = append(kept, p)
		}
	}
	return kept
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// AssertNoPageErrors fails the test if the page threw, logged an error, or
// failed to load a subresource.
func (b *Browser) AssertNoPageErrors(t *testing.T) {
	t.Helper()
	if errs := b.Errors(); len(errs) > 0 {
		t.Errorf("page reported %d error(s):\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
}

func (b *Browser) record(ev any) {
	switch e := ev.(type) {
	case *runtime.EventExceptionThrown:
		b.add("uncaught exception: " + e.ExceptionDetails.Error())
	case *runtime.EventConsoleAPICalled:
		if e.Type != runtime.APITypeError {
			return
		}
		b.add("console.error: " + consoleText(e.Args))
	case *cdplog.EventEntryAdded:
		if e.Entry.Level != cdplog.LevelError || isFaviconMiss(e.Entry) {
			return
		}
		b.add(fmt.Sprintf("%s error: %s %s", e.Entry.Source, e.Entry.Text, e.Entry.URL))
	}
}

// isFaviconMiss identifies the tab-icon fetch Chrome issues on its own for
// every navigation. No shell references a favicon, so ignoring this exact path
// cannot mask a subresource the page actually asked for.
func isFaviconMiss(e *cdplog.Entry) bool {
	if e.Source != cdplog.SourceNetwork {
		return false
	}
	u, err := url.Parse(e.URL)
	return err == nil && u.Path == "/favicon.ico"
}

func (b *Browser) add(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.problems = append(b.problems, strings.TrimSpace(msg))
}

// consoleText flattens console arguments onto one line. Description carries
// objects and errors; a primitive arrives as raw JSON only.
func consoleText(args []*runtime.RemoteObject) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case a.Description != "":
			parts = append(parts, a.Description)
		case len(a.Value) > 0:
			parts = append(parts, string(a.Value))
		}
	}
	return strings.Join(parts, " ")
}
