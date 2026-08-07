//go:build slatedb

package storage

import (
	"errors"
	"fmt"
	"strings"
)

// errUnreadableRecord is why a placeholder entry has no usable size: the
// authoritative record it projects could not be decoded when it was written.
var errUnreadableRecord = errors.New("authoritative record undecodable")

// scanSkips accumulates the entries a quota scan could not read, so the scan
// reports ONE summary line instead of one per row. Unreadable rows are
// permanent until repaired, so per-row logging would be a standing cost
// proportional to accumulated debris rather than to anything actionable
// (docs/SPEC.md "The log is a PER-PASS SUMMARY, never one line per row").
type scanSkips struct {
	n      int
	sample []string
}

// skipSampleSize bounds the slugs named in the summary line; the COUNT carries
// the magnitude, the sample only gives an operator somewhere to start.
const skipSampleSize = 3

func (s *scanSkips) add(key []byte, cause error) {
	s.n++
	if len(s.sample) < skipSampleSize {
		s.sample = append(s.sample, fmt.Sprintf("%s (%v)", key, cause))
	}
}

// report logs the summary, if any. A clean scan logs nothing, so log volume
// stays the signal: a steady count is known debris, a growing one means
// something is still producing unreadable entries.
func (s *scanSkips) report(r *ShaleRepo, owner, plane string) {
	if s.n == 0 {
		return
	}
	r.repoLog().Printf("shale: %s quota scan for %s skipped %d unreadable entr%s (counted as zero bytes, so the owner is UNDER-charged and keeps working; repair or delete them): %s",
		plane, owner, s.n, plural(s.n), strings.Join(s.sample, "; "))
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
