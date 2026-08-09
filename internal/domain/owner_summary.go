package domain

import "time"

// OwnerSummary is an identity's usage roll-up: what whoami renders. A storage
// backend that keeps a per-owner document can answer it with one read; the
// service falls back to separate count / first-seen / byte-sum calls when the
// repo does not provide it.
type OwnerSummary struct {
	Active     int       // live pastes, directories included
	FirstSeen  time.Time // zero when the owner has never uploaded
	PasteBytes int64     // active compressed paste bytes, the quota basis
	SiteBytes  int64     // active site bytes counted outside the paste sum
}
