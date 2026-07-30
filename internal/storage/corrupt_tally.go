// Per-pass tally of corrupt rows found by a reconcile scan.
//
// DELIBERATELY NOT behind the `slatedb` build tag. CI runs `go test ./...`
// with no tags, so a tagged pin does not execute there, and the property this
// type exists to hold - log volume per pass is O(1) in the number of corrupt
// rows - is exactly the kind that regresses silently by someone reasonably
// adding a Printf back into the loop. The logic is pure, so there is no reason
// to hide it behind a tag. Same reasoning as retry_acquiring.go.
//
// Why it exists at all: both reconcilers (pastes and sites) previously logged
// ONE LINE PER CORRUPT ROW PER PASS. A corrupt row stays corrupt, and the
// unrepairable class (undecodable AND no slug_owner) can never be fixed by a
// retry, so every pass re-found the same set and re-logged all of it. The cost
// therefore scaled with accumulated debris rather than with anything an
// operator could act on: observed at 19,764 rows emitting ~60 lines/sec and
// consuming ~1.5 CPU cores, which starved the request path badly enough to
// take a read from 0.45s to 19s while every behavioural smoke check stayed
// green. The COUNT is the actionable signal; the individual slugs are
// identical on every pass.

package storage

import "fmt"

// corruptSampleLimit caps how many slugs a tally retains per class. Enough to
// go inspect a real row by hand; small enough that a pathological pass cannot
// grow the summary line without bound.
const corruptSampleLimit = 3

// corruptTally counts the corrupt rows of a single reconcile pass, split by
// what can be done about them, and renders at most one summary line.
//
// The split matters and is not cosmetic:
//
//   - unrepairable: undecodable AND no slug_owner, so no owner can be derived
//     and no enumeration entry can be projected. The row stays invisible to the
//     owner's quota scan until it is repaired out of band. Retrying does
//     nothing, which is precisely why re-logging it every pass was pure cost.
//   - placeholder: undecodable but the owner IS derivable, so a fail-closed
//     placeholder entry gets projected and the owner's next scan hard-fails
//     rather than silently under-counting.
//
// The zero value is ready to use.
type corruptTally struct {
	unrepairable       int
	unrepairableSample []string
	placeholder        int
	placeholderSample  []string
}

// noteUnrepairable records a row whose owner could not be derived.
func (t *corruptTally) noteUnrepairable(slug string) {
	t.unrepairable++
	if len(t.unrepairableSample) < corruptSampleLimit {
		t.unrepairableSample = append(t.unrepairableSample, slug)
	}
}

// notePlaceholder records a row projected as a fail-closed placeholder.
func (t *corruptTally) notePlaceholder(slug string) {
	t.placeholder++
	if len(t.placeholderSample) < corruptSampleLimit {
		t.placeholderSample = append(t.placeholderSample, slug)
	}
}

// summary renders the pass's one line, or ok=false when the pass found nothing
// corrupt. A clean pass must stay SILENT: the log volume is itself the signal,
// so a line on every tick would bury the transition from clean to not.
//
// The line states that unrepairable rows cannot be fixed by retrying, because
// the actionable reading is the DERIVATIVE, not the level: a steady count is
// known debris, while a growing one means something is still producing it.
func (t *corruptTally) summary(scan string) (string, bool) {
	if t.unrepairable == 0 && t.placeholder == 0 {
		return "", false
	}
	return fmt.Sprintf("reconcile %s: corrupt rows this pass: %d unrepairable "+
		"(undecodable AND no slug_owner, e.g. %v), %d projected as fail-closed placeholders (e.g. %v); "+
		"unrepairable rows stay un-enumerated and cannot be fixed by retrying, so a GROWING count "+
		"means something is still producing them",
		scan, t.unrepairable, t.unrepairableSample, t.placeholder, t.placeholderSample), true
}
