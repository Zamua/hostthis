//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// fixtureEnd closes every markdown fixture. addHeadingIds in md.js slugs the
// heading text into an id, so waiting on it signals that the WHOLE document
// rendered rather than just its first block.
const fixtureEnd = "\n\n## fixture end\n"

// fixtureEndSel is the id that heading becomes.
const fixtureEndSel = "#fixture-end"

// decoyImageSrc is the src on every broken-image payload: the load has to fail
// for an onerror handler to be worth stripping. Spelled out rather than the
// classic bare "x" so the URL it resolves to can prefix no real slug, which
// would silence that paste's errors along with this 404.
const decoyImageSrc = "no-such-image.png"

// benignHref proves a link survived. Never fetched: only its rendered href is
// read, and it is not among the anchors the test clicks.
const benignHref = "https://example.com/safe"

// settleGrace is how long a page gets after its images resolve. Asserting a
// sentinel is UNSET is only meaningful once the payload has had its chance;
// the control case runs the same wait and does set its sentinels, which is
// what fixes this window as long enough.
const settleGrace = 750 * time.Millisecond

// controlHTML carries the payload markup on an html paste, which hostthis
// serves verbatim by design (docs/SPEC.md "HTML sandboxing"). It exists to
// prove the payloads, the browser, and the sentinel reads all work, so that
// "the sentinel is unset" on the markdown pages is evidence rather than a
// check that could never have fired.
//
// Production bounds an html paste with one origin per paste. Path mode does
// not model that and nothing here asserts it.
const controlHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>sanitizer control</title></head>
<body>
<p id="control-end">control fixture rendered</p>
<img src="` + decoyImageSrc + `" onerror="window.__pwnedImg='img-onerror'">
<script>window.__pwned='inline-script'</script>
</body></html>
`

// sanitizerCases are markdown pastes carrying the payloads a sanitizer
// regression would let through. Markdown is the only kind with a sanitizer to
// regress: an html paste is served as itself, and every other kind renders its
// bytes as text rather than as markup.
var sanitizerCases = []struct {
	label string
	md    string
}{
	{
		label: "script-tag",
		md: "# sanitizer: script tag\n\n" +
			"<script>window.__pwned='inline-script'</script>" + fixtureEnd,
	},
	{
		label: "img-onerror",
		md: "# sanitizer: img onerror\n\n" +
			`<img src="` + decoyImageSrc + `" onerror="window.__pwnedImg='img-onerror'">` + fixtureEnd,
	},
	{
		label: "javascript-url",
		md: "# sanitizer: javascript url\n\n" +
			"[payload](javascript:window.__pwned='markdown-link')\n\n" +
			"[benign](" + benignHref + ")" + fixtureEnd,
	},
	{
		label: "raw-html-block",
		md: "# sanitizer: raw html block\n\n" +
			`<div onclick="window.__pwned='onclick'" onmouseover="window.__pwned='onmouseover'">` + "\n" +
			`  <iframe src="javascript:window.__pwned='iframe'"></iframe>` + "\n" +
			`  <a href="javascript:window.__pwned='anchor'">payload link</a>` + "\n" +
			`  <form action="javascript:window.__pwned='form'"><button>go</button></form>` + "\n" +
			`</div>` + fixtureEnd,
	},
	{
		label: "svg-script",
		md: "# sanitizer: svg script\n\n" +
			`<svg xmlns="http://www.w3.org/2000/svg" width="80" height="80">` + "\n" +
			`  <script>window.__pwned='svg-script'</script>` + "\n" +
			`  <animate onbegin="window.__pwned='svg-animate'" attributeName="x" dur="1s"></animate>` + "\n" +
			`  <circle cx="40" cy="40" r="30" fill="teal"></circle>` + "\n" +
			`</svg>` + fixtureEnd,
	},
}

// TestSanitization pins the one renderer property whose regression is a
// vulnerability rather than a cosmetic bug: anonymous markdown is rendered as
// HTML on hostthis' own origin, so a marked or DOMPurify bump that stops
// stripping has to fail here.
func TestSanitization(t *testing.T) {
	srv := StartServer(t)
	br := NewBrowser(t)
	// The decoy image's 404 is the fixture doing its job. Scoped to that exact
	// URL, so every other missing subresource still fails the page.
	br.Ignore(srv.BaseURL + "/p/" + decoyImageSrc)
	flow := NewFlow(t, br, "sanitization")

	control := srv.Upload(t, []byte(controlHTML), UploadOpts{Type: "html"})
	br.Open(t, control.URL)
	if err := chromedp.Run(br.Ctx, chromedp.WaitVisible("#control-end", chromedp.ByQuery)); err != nil {
		t.Fatalf("control page never rendered: %v", err)
	}
	waitSettled(t, br)
	flow.Shot("control-executes")
	ctl := probeDOM(t, br, "document.body")
	// What this control does and does NOT license. An html paste is streamed
	// with no CSP header, so it proves the sentinel MECHANISM works: an
	// unsanitized payload on this origin really does set these globals. It does
	// NOT license the sentinels on the markdown cases below, which are served
	// under the shell's `script-src 'self'`. There an inline handler is blocked
	// by CSP whether or not DOMPurify ran, so the sentinel cannot fire even with
	// the sanitizer removed outright. The DOM-presence checks in assertSanitized
	// are the load-bearing assertions for those pages; do not drop them as
	// redundant on the strength of this control.
	if ctl.Sentinel != "inline-script" || ctl.ImgSentinel != "img-onerror" {
		t.Fatalf("control did not execute (sentinels %q/%q), so the sentinel mechanism is broken",
			ctl.Sentinel, ctl.ImgSentinel)
	}
	if ctl.Scripts == 0 || len(ctl.EventAttrs) == 0 {
		t.Fatalf("control kept %d <script> and event attributes %v, so the detectors below prove nothing",
			ctl.Scripts, ctl.EventAttrs)
	}
	br.AssertNoPageErrors(t)

	pastes := make([]Paste, len(sanitizerCases))
	for i, c := range sanitizerCases {
		before := len(br.Errors())
		pastes[i] = srv.Upload(t, []byte(c.md), UploadOpts{Type: "md"})
		br.Open(t, pastes[i].URL)
		if err := chromedp.Run(br.Ctx,
			chromedp.WaitVisible(fixtureEndSel, chromedp.ByQuery),
			// A neutered javascript: href leaves an anchor carrying no href at
			// all. Clicking it turns "the attribute is gone" into "the link is
			// inert", which is the property that matters.
			chromedp.Evaluate(`document.querySelectorAll("#content a:not([href])").forEach(a => a.click()), true`, nil),
		); err != nil {
			t.Errorf("%s: page never rendered: %v", c.label, err)
			continue
		}
		waitSettled(t, br)
		flow.Shot(c.label)
		assertSanitized(t, c.label, probeDOM(t, br, `document.getElementById("content")`))
		assertNoNewErrors(t, br, c.label, before)
	}

	// The second defense layer, pinned on its own. Sanitization strips the
	// payload; CSP independently blocks anything that survives. Asserting it
	// here means dropping the header is a failure in its own right rather than
	// a silent change that quietly promotes the sentinels above from backstop
	// to load-bearing without anyone noticing.
	assertShellCSP(t, pastes[0].URL)

	assertRawIsNotMarkup(t, srv, br, flow, pastes)
}

// assertRawIsNotMarkup covers the second door onto the same untrusted bytes.
// ?raw serves the payload unrendered; served as a type the browser parses as
// markup it executes exactly what the shell path strips.
func assertRawIsNotMarkup(t *testing.T, srv *Server, br *Browser, flow *Flow, pastes []Paste) {
	t.Helper()
	for i, p := range pastes {
		label := sanitizerCases[i].label
		// A header read cannot ride the loading page's meta refresh the way a
		// browser assertion does, so the paste has to be ready first.
		srv.WaitReady(t, p)
		resp, err := http.Get(p.URL + "?raw=1")
		if err != nil {
			t.Errorf("%s: raw fetch: %v", label, err)
			continue
		}
		ct := resp.Header.Get("Content-Type")
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code != http.StatusOK {
			t.Errorf("%s: raw returned %d", label, code)
			continue
		}
		if ct == "" {
			t.Errorf("%s: raw carries no Content-Type, leaving the browser to sniff one", label)
			continue
		}
		if markup := markupType(ct); markup != "" {
			t.Errorf("%s: raw served as %q, which a browser parses as markup and executes", label, markup)
		}
	}

	// What the header claims and what the browser concluded are different
	// facts: a type the browser overrides by sniffing is still a live payload.
	raw := pastes[0].URL + "?raw=1"
	br.Open(t, raw)
	waitSettled(t, br)
	flow.Shot("raw-served-as-text")
	got := probeDOM(t, br, "document.body")
	if markup := markupType(got.ContentType); markup != "" {
		t.Errorf("raw: browser parsed %s as %q", raw, markup)
	}
	if got.Sentinel != "undefined" || got.ImgSentinel != "undefined" {
		t.Errorf("raw: payload executed through ?raw (sentinels %q/%q)", got.Sentinel, got.ImgSentinel)
	}
	if got.Scripts != 0 {
		t.Errorf("raw: %d <script> reached the DOM at %s", got.Scripts, raw)
	}
	br.AssertNoPageErrors(t)
}

// assertSanitized checks one rendered markdown page against both ways a payload
// survives: it ran, or it is still sitting there waiting for a user gesture.
func assertSanitized(t *testing.T, label string, p domProbe) {
	t.Helper()
	var bad []string
	// Backstop only on a shell page: CSP blocks inline execution independently
	// of sanitization, so this cannot fire here even if DOMPurify is removed.
	// It earns its place on the raw path, which carries no CSP.
	if p.Sentinel != "undefined" || p.ImgSentinel != "undefined" {
		bad = append(bad, fmt.Sprintf("payload executed (sentinels %q/%q)", p.Sentinel, p.ImgSentinel))
	}
	// Load-bearing from here down: these are what actually go red when the
	// sanitizer stops stripping.
	if p.Scripts != 0 {
		bad = append(bad, fmt.Sprintf("%d <script> survived", p.Scripts))
	}
	if p.Iframes != 0 {
		bad = append(bad, fmt.Sprintf("%d <iframe> survived", p.Iframes))
	}
	if len(p.EventAttrs) != 0 {
		bad = append(bad, "event handler attributes survived: "+strings.Join(p.EventAttrs, ", "))
	}
	if len(p.JSURLAttrs) != 0 {
		bad = append(bad, "javascript: URLs survived: "+strings.Join(p.JSURLAttrs, ", "))
	}
	// One fixture carries a benign link. Its href surviving is what stops the
	// javascript: check above from passing merely because no anchor rendered.
	if href, ok := p.Anchors["benign"]; ok && href != benignHref {
		bad = append(bad, fmt.Sprintf("benign link href = %q, want %q: link rendering is broken, "+
			"so the javascript: check proves nothing", href, benignHref))
	}
	if len(bad) == 0 {
		return
	}
	t.Errorf("%s: %s\nrendered as:\n%s", label, strings.Join(bad, "; "), p.HTML)
}

// assertNoNewErrors attributes page errors to the case that navigated, instead
// of reporting every earlier case's errors against the last one.
func assertNoNewErrors(t *testing.T, br *Browser, label string, before int) {
	t.Helper()
	if errs := br.Errors(); len(errs) > before {
		t.Errorf("%s: page reported %d error(s):\n  %s",
			label, len(errs)-before, strings.Join(errs[before:], "\n  "))
	}
}

// domProbe is what one page reports about the markup that reached its DOM.
type domProbe struct {
	// Sentinel and ImgSentinel read "undefined" unless a payload ran. Two
	// names, so the synchronous path (an inline script) and the asynchronous
	// one (an image error handler) are provable apart.
	Sentinel    string `json:"sentinel"`
	ImgSentinel string `json:"imgSentinel"`
	ContentType string `json:"contentType"`
	Scripts     int    `json:"scripts"`
	Iframes     int    `json:"iframes"`
	// EventAttrs and JSURLAttrs name surviving attributes as "tag@attr".
	EventAttrs []string `json:"eventAttrs"`
	JSURLAttrs []string `json:"jsURLAttrs"`
	// Anchors maps a link's text to its rendered href, "" when it has none.
	Anchors map[string]string `json:"anchors"`
	HTML    string            `json:"html"`
}

// probeJS reads the whole probe in one round trip. Its one verb is a JS
// expression for the region to inspect, so the same probe covers the shell's
// #content and a raw page's body.
//
// The javascript: match tolerates leading whitespace and control characters
// because a URL's scheme is read with those stripped, which is how the payload
// hides from a naive prefix check.
const probeJS = `(() => {
  const root = %s;
  const eventAttrs = [], jsURLAttrs = [], anchors = {};
  for (const el of root.querySelectorAll("*")) {
    for (const a of el.attributes) {
      const where = el.tagName.toLowerCase() + "@" + a.name;
      if (/^on/i.test(a.name)) eventAttrs.push(where);
      if (/^[\s\x00-\x1F]*javascript:/i.test(a.value)) jsURLAttrs.push(where);
    }
  }
  for (const a of root.querySelectorAll("a")) {
    anchors[a.textContent.trim()] = a.getAttribute("href") || "";
  }
  return {
    sentinel: String(window.__pwned),
    imgSentinel: String(window.__pwnedImg),
    contentType: document.contentType,
    scripts: root.querySelectorAll("script").length,
    iframes: root.querySelectorAll("iframe").length,
    eventAttrs, jsURLAttrs, anchors,
    html: root.innerHTML,
  };
})()`

func probeDOM(t *testing.T, br *Browser, root string) domProbe {
	t.Helper()
	var p domProbe
	if err := chromedp.Run(br.Ctx, chromedp.Evaluate(fmt.Sprintf(probeJS, root), &p)); err != nil {
		t.Fatalf("probe %s: %v", root, err)
	}
	return p
}

// waitSettled blocks until every image on the page has resolved, then holds for
// settleGrace. Without it, "the sentinel is unset" could mean only that the
// assertion ran first.
func waitSettled(t *testing.T, br *Browser) {
	t.Helper()
	if err := chromedp.Run(br.Ctx,
		chromedp.Poll(`Array.from(document.images).every(i => i.complete)`, nil,
			chromedp.WithPollingTimeout(15*time.Second)),
		chromedp.Sleep(settleGrace),
	); err != nil {
		t.Fatalf("wait for page to settle: %v", err)
	}
}

// markupTypes are the response types a browser parses as markup and runs script
// from.
var markupTypes = []string{
	"text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml",
}

// markupType returns the matching markup type, or "" when the type is inert.
func markupType(contentType string) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	for _, m := range markupTypes {
		if strings.HasPrefix(ct, m) {
			return m
		}
	}
	return ""
}

// assertShellCSP pins that a rendered shell is served with a script-src policy
// that forbids inline execution. Paired with the sanitizer checks so the two
// layers are verified separately rather than through one end-to-end signal that
// cannot say which of them held.
func assertShellCSP(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("fetch shell for CSP check: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Errorf("shell served with no Content-Security-Policy; the sanitizer is now the only layer")
		return
	}
	// Read the script-src directive specifically. A substring search over the
	// whole policy cannot tell the directives apart, and style-src legitimately
	// carries unsafe-inline; only script-src decides whether an injected handler
	// can run.
	var scriptSrc string
	for _, d := range strings.Split(csp, ";") {
		if f := strings.Fields(d); len(f) > 0 && f[0] == "script-src" {
			scriptSrc = strings.Join(f[1:], " ")
		}
	}
	if scriptSrc == "" {
		t.Errorf("CSP %q names no script-src, so inline execution is unconstrained", csp)
		return
	}
	for _, unsafe := range []string{"'unsafe-inline'", "'unsafe-eval'"} {
		if strings.Contains(scriptSrc, unsafe) {
			t.Errorf("script-src %q allows %s, which defeats the layer it exists to provide", scriptSrc, unsafe)
		}
	}
}
