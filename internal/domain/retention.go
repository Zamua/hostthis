package domain

import (
	"fmt"
	"time"
)

// DefaultRetentionWindow is the out-of-the-box content TTL: a paste or site
// expires this long after its last update unless the operator overrides it
// (HOSTTHIS_RETENTION).
const DefaultRetentionWindow = 30 * 24 * time.Hour

// NeverExpires is the ExpiresAt sentinel for content under a no-expiry policy.
// Far enough in the future that the sweep's `expires_at < now` check never
// matches and the expiry index never reaches it on a cutoff scan, so such
// content is never swept. Also the marker the read/display layers test against
// to render "never".
var NeverExpires = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// ExpiredPaste references one expired paste surfaced by the sweep's expiry
// scan. The sweep round-trips the reference into the repo's DeleteExpired so
// the index entry is removed even when the paste record is already gone;
// without that, an orphaned entry resurfaces on every scan forever. See
// docs/SPEC.md "The storage contract" (Expiry).
type ExpiredPaste struct {
	Slug Slug
	// IndexRef is opaque to everything but the backend that produced it
	// (index-backed backends encode the entry's full key). Empty on backends
	// whose expiry scan reads the paste records themselves, which therefore
	// have no standalone entry to clean.
	IndexRef string
}

// ExpiredSite is the static-site twin of ExpiredPaste, round-tripped into
// DeleteExpiredSite for the same reason. IndexRef is empty on backends without
// a standalone site-expiry index.
type ExpiredSite struct {
	Slug     Slug
	IndexRef string
}

// ExpiredRoom is the room twin of ExpiredPaste, keyed by the (app-slug,
// room-id) pair and round-tripped into DeleteExpiredRoom for the same reason.
// IndexRef is empty on backends whose scan reads the room records themselves.
type ExpiredRoom struct {
	AppSlug  Slug
	ID       RoomID
	IndexRef string
}

// Retention is the installation's content-TTL policy, set once at the
// composition root and injected into the services / repos that stamp
// ExpiresAt. A positive Window expires content that long after its last
// update; a Window <= 0 means content never expires.
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
// "90 minutes", or "never" when disabled.
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

// IsExpired reports whether content with this ExpiresAt has expired at now.
//
// The BOUNDARY is the point: expiry is INCLUSIVE, so content whose ExpiresAt
// equals now is expired. `ExpiresAt.Before(now)` reads just as naturally and
// disagrees at exactly that instant, serving content one tick past its
// lifetime.
//
// A free function rather than a method because the field lives on several row
// types across the adapters, not only on Paste.
//
// Safe under the no-expiry policy only because NeverExpires is a far-future
// sentinel rather than the zero time: a zero value would report "expired" and
// silently sweep content that must never be swept.
func IsExpired(expiresAt, now time.Time) bool {
	return !expiresAt.After(now)
}
