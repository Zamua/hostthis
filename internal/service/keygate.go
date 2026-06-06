package service

import (
	"errors"
	"time"
)

// KeyGateRepo is the persistence interface backing the per-IP-subnet
// Sybil rate limit. internal/storage.KeyGateRepo satisfies it.
type KeyGateRepo interface {
	// AdmitNewKey is the atomic check: returns knownAlready=true if
	// (identity, subnet) is already on file (no rate-limit accounting),
	// or admits + records a fresh key when subnet hasn't hit
	// `limitPerSubnet` distinct new fingerprints in the window.
	AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error)

	// DeleteFirstSeenOlderThan removes rows past the rate-limit
	// window — they no longer affect any future admission decision.
	// Called by the periodic sweep to keep the table from growing.
	DeleteFirstSeenOlderThan(cutoff time.Time) (int, error)
}

// KeyGate gates new SSH sessions: a fresh (identity, ip-subnet) pair
// is admitted only if the subnet hasn't already first-seen more than
// `MaxFreshKeysPerSubnet` distinct fingerprints in the last `Window`.
// Returning users (any (identity, subnet) pair we've seen before) pass
// through with no accounting.
//
// Defaults: 20 fresh keys per subnet per 24h. The SSH server calls
// Admit once per session before any verb dispatch.
type KeyGate struct {
	Repo                  KeyGateRepo
	MaxFreshKeysPerSubnet int
	Window                time.Duration
	Now                   func() time.Time
}

// NewKeyGate wires the documented defaults: 20 fresh / 24h.
func NewKeyGate(repo KeyGateRepo) *KeyGate {
	return &KeyGate{
		Repo:                  repo,
		MaxFreshKeysPerSubnet: 20,
		Window:                24 * time.Hour,
		Now:                   time.Now,
	}
}

// ErrSybilRateLimit is returned when the subnet has exceeded its fresh-
// key allowance. The SSH server surfaces a friendly "too many new keys
// from this network today; try tomorrow or reuse an existing key"
// message and exits the session.
var ErrSybilRateLimit = errors.New("service: too many new keys from this network today")

// Admit gates one session. Both identity and ipSubnet must be non-empty.
func (g *KeyGate) Admit(identity, ipSubnet string) error {
	now := g.Now().UTC()
	known, err := g.Repo.AdmitNewKey(identity, ipSubnet, now, g.MaxFreshKeysPerSubnet, g.Window)
	if err != nil {
		// Map storage's "too many new keys" → our user-facing sentinel.
		// Other errors propagate.
		if isStorageRateLimitErr(err) {
			return ErrSybilRateLimit
		}
		return err
	}
	_ = known // not used by callers right now, but useful for telemetry
	return nil
}

// PruneOldRows deletes rows older than the rate-limit window. The
// sweep service calls this on every tick so the table stays bounded.
// Returns the number of rows deleted.
func (g *KeyGate) PruneOldRows(now time.Time) (int, error) {
	cutoff := now.Add(-g.Window)
	return g.Repo.DeleteFirstSeenOlderThan(cutoff)
}

// isStorageRateLimitErr matches storage.ErrTooManyNewKeys without
// importing storage into the service layer's stable signature surface.
func isStorageRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "storage: too many new keys from this network"
}
