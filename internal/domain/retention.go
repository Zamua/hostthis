package domain

import (
	"fmt"
	"time"
)

// DefaultRetentionWindow is the out-of-the-box content TTL: a paste or site
// expires this long after its last update unless the operator overrides it
// (HOSTTHIS_RETENTION). 30 days matches the long-standing default.
const DefaultRetentionWindow = 30 * 24 * time.Hour

// NeverExpires is the ExpiresAt sentinel for content under a no-expiry policy.
// It is far enough in the future that the sweep's `expires_at < now` check
// never matches and the fixed-width (RFC3339Nano) expiry index never reaches
// it on a cutoff scan, so such content is simply never swept. It is also an
// unambiguous marker the read/display layers test against to render "never".
var NeverExpires = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// ExpiredPaste references one expired paste surfaced by the sweep's expiry
// scan: the slug to delete plus, on backends that keep a standalone expiry
// index, an opaque reference to the exact index entry that surfaced it. The
// sweep round-trips the reference into the repo's DeleteExpired so the index
// entry is removed even when the paste record is already gone; without that,
// an orphaned entry would resurface on every scan forever. See docs/SPEC.md
// "The storage contract" (Expiry).
type ExpiredPaste struct {
	Slug Slug
	// IndexRef is opaque to everything but the backend that produced it
	// (the index-backed backends encode the entry's full key). Empty on
	// backends whose expiry scan reads the paste records themselves and
	// so have no standalone entry to clean.
	IndexRef string
}

// Retention is the installation's content-TTL policy, set once at the
// composition root from config and injected into the services / repos that
// stamp ExpiresAt. A positive Window expires content that long after its last
// update; a Window <= 0 means content never expires.
//
// It is a pure value object: no I/O, safe to copy, the single source of truth
// for "when does content expire" and "how do we describe that to a human".
type Retention struct {
	Window time.Duration // <= 0 means "never expire"
}

// DefaultRetention is the out-of-the-box policy (DefaultRetentionWindow).
func DefaultRetention() Retention { return Retention{Window: DefaultRetentionWindow} }

// Enabled reports whether content expires at all under this policy.
func (r Retention) Enabled() bool { return r.Window > 0 }

// ExpiryFor returns the ExpiresAt to stamp on content last updated at now.
// Under a no-expiry policy it returns the NeverExpires sentinel.
func (r Retention) ExpiryFor(now time.Time) time.Time {
	if !r.Enabled() {
		return NeverExpires
	}
	return now.Add(r.Window)
}

// Describe renders the policy for user-facing copy: "30 days", "12 hours",
// "90 minutes", or "never" when disabled. Used so every "expires in ..."
// string tracks the configured window instead of a hard-coded literal.
func (r Retention) Describe() string {
	if !r.Enabled() {
		return "never"
	}
	return humanizeDuration(r.Window)
}

// humanizeDuration renders a duration in the largest whole unit that divides
// it cleanly (days, then hours, then minutes), falling back to Go's own format.
func humanizeDuration(d time.Duration) string {
	day := 24 * time.Hour
	switch {
	case d%day == 0:
		return plural(int(d/day), "day")
	case d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return plural(int(d/time.Minute), "minute")
	default:
		return d.String()
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
