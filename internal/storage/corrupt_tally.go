// Untagged on purpose: the reconcilers using this are behind the slatedb tag,
// but CI builds no tags.

package storage

import "fmt"

// corruptSampleLimit caps retained slugs per class, so the summary line stays a
// fixed size regardless of how much debris exists.
const corruptSampleLimit = 3

// corruptTally counts a single reconcile pass's corrupt rows and renders at
// most one summary line. The zero value is ready to use.
//
// Per-record logging is wrong here: a corrupt row is corrupt on every pass, so
// its cost scales with accumulated debris rather than with anything actionable,
// and at scale the log volume alone starves the request path.
//
// The split by class is not cosmetic. An unrepairable row has no derivable
// owner, so no enumeration entry can be projected and no retry can fix it. A
// placeholder row has a known owner, so a fail-closed entry gets projected and
// the owner's next quota scan hard-fails rather than under-counting.
type corruptTally struct {
	unrepairable       int
	unrepairableSample []string
	placeholder        int
	placeholderSample  []string
}

func (t *corruptTally) noteUnrepairable(slug string) {
	t.unrepairable++
	if len(t.unrepairableSample) < corruptSampleLimit {
		t.unrepairableSample = append(t.unrepairableSample, slug)
	}
}

func (t *corruptTally) notePlaceholder(slug string) {
	t.placeholder++
	if len(t.placeholderSample) < corruptSampleLimit {
		t.placeholderSample = append(t.placeholderSample, slug)
	}
}

// summary renders the pass's line, or ok=false when nothing was corrupt.
//
// A clean pass must stay silent: log volume is itself the signal, and a line
// every tick would bury the transition from clean to not.
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
