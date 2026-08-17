//go:build e2e

package e2e

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/chromedp/chromedp"
)

// flamegraphFixture is two folded stacks under one parent, with a 3:1 sample
// split so the child widths have an order a broken layout could get wrong.
// The tree the shell should build: all(40) -> profileMain(40) ->
// encodeFrames(30) + flushBuffers(10).
const flamegraphFixture = `profileMain;encodeFrames 30
profileMain;flushBuffers 10
`

// flameSettled cannot match the unrendered page: shell.html hard-codes
// aria-busy="true" on the mount, and only flame.js flips it, on a render, an
// empty profile, or a fetch error alike. Waiting on it reports a failed load
// as the text the shell wrote instead of as a timeout.
const flameSettled = `#flame[aria-busy="false"]`

// flameFrame is one rendered frame box. Width is the box's fraction of the
// mount, so assertions compare shares rather than pixels.
type flameFrame struct {
	Name  string  `json:"name"`
	Chain bool    `json:"chain"`
	Width float64 `json:"width"`
	Top   float64 `json:"top"`
}

// flameState is the semantic readout of the whole shell: the frame boxes, the
// breadcrumb trail, and the focused share, read in one pass.
type flameState struct {
	MountText string       `json:"mountText"`
	Frames    []flameFrame `json:"frames"`
	Crumbs    []string     `json:"crumbs"`
	Share     string       `json:"share"`
}

const probeFlame = `(() => {
  const mount = document.getElementById("flame");
  const mw = mount.getBoundingClientRect().width;
  const frames = [...mount.querySelectorAll(".fr")].map((el) => ({
    name: el.dataset.name,
    chain: el.classList.contains("chain"),
    width: el.getBoundingClientRect().width / mw,
    top: parseFloat(el.style.top),
  }));
  return {
    mountText: frames.length ? "" : mount.textContent.trim(),
    frames,
    crumbs: [...document.querySelectorAll("#crumbs .crumb")].map((e) => e.textContent.trim()),
    share: (document.querySelector("#crumbs .share") || { textContent: "" }).textContent.trim(),
  };
})()`

// The flamegraph shell lays frames out as width-is-share boxes, zooms into a
// clicked frame, and resets back to the whole profile.
func TestFlamegraphRender(t *testing.T) {
	srv := StartServer(t)
	paste := srv.Upload(t, []byte(flamegraphFixture), UploadOpts{
		Type: "flamegraph",
		Name: "flamegraph proof",
	})

	br := NewBrowser(t)
	flow := NewFlow(t, br, "flamegraph-render")
	br.Open(t, paste.URL)

	// Scoped under the browser cap so a stuck render leaves a live context to
	// screenshot from; waiting on br.Ctx would burn the budget and kill it.
	var got flameState
	renderCtx, cancel := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancel()
	if err := chromedp.Run(renderCtx,
		chromedp.WaitReady(flameSettled, chromedp.ByQuery),
		chromedp.Evaluate(probeFlame, &got),
	); err != nil {
		flow.Shot("stuck")
		t.Fatalf("flame shell never settled: %v\n#flame: %q\npage errors: %v",
			err, flameText(t, br), br.Errors())
	}
	flow.Shot("initial")

	if len(got.Frames) == 0 {
		t.Fatalf("shell settled with no frames: %q\npage errors: %v", got.MountText, br.Errors())
	}
	if names := frameNames(got.Frames); !slices.Equal(names, []string{"all", "profileMain", "encodeFrames", "flushBuffers"}) {
		t.Fatalf("frames = %v, want [all profileMain encodeFrames flushBuffers]", names)
	}
	byName := frameIndex(got.Frames)
	all, main := byName["all"], byName["profileMain"]
	encode, flush := byName["encodeFrames"], byName["flushBuffers"]
	if !all.Chain {
		t.Errorf("synthetic root is not marked as chain context")
	}
	if !near(all.Width, 1) || !near(main.Width, 1) {
		t.Errorf("root row widths = %.3f and %.3f, want both 1: the root does not span the profile",
			all.Width, main.Width)
	}
	// Width is share: 30 vs 10 samples of a 40-sample profile.
	if encode.Width <= flush.Width {
		t.Errorf("encodeFrames (%.3f) not wider than flushBuffers (%.3f) despite 3x the samples",
			encode.Width, flush.Width)
	}
	if !near(encode.Width, 0.75) || !near(flush.Width, 0.25) {
		t.Errorf("child widths = %.3f and %.3f, want 0.75 and 0.25", encode.Width, flush.Width)
	}
	// Depth is the y axis: children share a row below their parent.
	if !(all.Top < main.Top && main.Top < encode.Top && encode.Top == flush.Top) {
		t.Errorf("row tops = all %g, profileMain %g, children %g and %g: rows out of order",
			all.Top, main.Top, encode.Top, flush.Top)
	}
	if !slices.Equal(got.Crumbs, []string{"all"}) || got.Share != "100.0% of 40 samples" {
		t.Errorf("crumbs = %v %q, want [all] and the full profile share", got.Crumbs, got.Share)
	}

	// Zoom: click the narrow child. The wait cannot match the pre-zoom page,
	// where flushBuffers exists but is not chain context, so a click that never
	// reached the handler fails here rather than as a stale probe.
	var zoomed flameState
	zoomCtx, cancelZoom := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelZoom()
	if err := chromedp.Run(zoomCtx,
		chromedp.Click(`#flame .fr[data-name="flushBuffers"]`, chromedp.ByQuery),
		chromedp.WaitReady(`#flame .fr.chain[data-name="flushBuffers"]`, chromedp.ByQuery),
		chromedp.Evaluate(probeFlame, &zoomed),
	); err != nil {
		flow.Shot("stuck-zoom")
		t.Fatalf("zoom into flushBuffers: %v\n#flame: %q\npage errors: %v",
			err, flameText(t, br), br.Errors())
	}
	flow.Shot("zoomed")

	if names := frameNames(zoomed.Frames); !slices.Equal(names, []string{"all", "profileMain", "flushBuffers"}) {
		t.Fatalf("zoomed frames = %v, want the focus chain only", names)
	}
	for _, f := range zoomed.Frames {
		if !f.Chain || !near(f.Width, 1) {
			t.Errorf("zoomed frame %s: chain=%v width=%.3f, want full-width chain context",
				f.Name, f.Chain, f.Width)
		}
	}
	if !slices.Equal(zoomed.Crumbs, []string{"all", "profileMain", "flushBuffers"}) {
		t.Errorf("zoomed crumbs = %v, want the focused stack", zoomed.Crumbs)
	}
	if zoomed.Share != "25.0% of 40 samples" {
		t.Errorf("zoomed share = %q, want %q", zoomed.Share, "25.0% of 40 samples")
	}

	// Reset restores the whole profile. encodeFrames is absent while zoomed, so
	// its reappearance is the reset having rendered.
	var reset flameState
	resetCtx, cancelReset := context.WithTimeout(br.Ctx, renderTimeout)
	defer cancelReset()
	if err := chromedp.Run(resetCtx,
		chromedp.Click(`#reset`, chromedp.ByQuery),
		chromedp.WaitReady(`#flame .fr[data-name="encodeFrames"]`, chromedp.ByQuery),
		chromedp.Evaluate(probeFlame, &reset),
	); err != nil {
		flow.Shot("stuck-reset")
		t.Fatalf("reset zoom: %v\n#flame: %q\npage errors: %v",
			err, flameText(t, br), br.Errors())
	}
	flow.Shot("reset")

	if names := frameNames(reset.Frames); !slices.Equal(names, []string{"all", "profileMain", "encodeFrames", "flushBuffers"}) {
		t.Fatalf("frames after reset = %v, want the whole profile back", names)
	}
	if f := frameIndex(reset.Frames)["flushBuffers"]; f.Chain || !near(f.Width, 0.25) {
		t.Errorf("flushBuffers after reset: chain=%v width=%.3f, want a plain quarter-width frame",
			f.Chain, f.Width)
	}

	br.AssertNoPageErrors(t)
}

// frameNames orders names widest-first per row, which is the layout order the
// renderer commits to, so slices.Equal pins it.
func frameNames(frames []flameFrame) []string {
	names := make([]string, len(frames))
	for i, f := range frames {
		names[i] = f.Name
	}
	return names
}

func frameIndex(frames []flameFrame) map[string]flameFrame {
	m := make(map[string]flameFrame, len(frames))
	for _, f := range frames {
		m[f.Name] = f
	}
	return m
}

// near absorbs the sub-pixel rounding of a percentage width without letting a
// wrong share through: the closest wrong value differs by 1/40.
func near(got, want float64) bool { return math.Abs(got-want) < 0.01 }

// flameText is what the shell left on screen, for a failure message.
func flameText(t *testing.T, br *Browser) string {
	t.Helper()
	var s string
	if err := chromedp.Run(br.Ctx,
		chromedp.Evaluate(`(document.getElementById("flame") || {}).textContent || ""`, &s),
	); err != nil {
		return "unreadable: " + err.Error()
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
