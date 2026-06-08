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

	// SubnetSnapshot returns (freshCount, oldestFirstSeen) for the
	// (identity, subnet)-rows from this subnet within the window.
	// freshCount is how many rows count toward the per-subnet cap;
	// oldestFirstSeen is the earliest in-window first_seen — adding
	// `window` to it gives the moment when the next slot frees up.
	// Empty subnet (no rows in window) returns (0, time.Time{}).
	// Used by Admit's refusal path + by Whoami for budget display.
	SubnetSnapshot(subnet string, now time.Time, window time.Duration) (freshCount int, oldestFirstSeen time.Time, err error)

	// SubnetsForIdentity returns the number of distinct subnets this
	// identity has rows for within the window. Used by Whoami so the
	// user can see how many networks their key is grandfathered on.
	SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error)
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

// ErrSybilRateLimit is the sentinel for "the subnet has exceeded its
// fresh-key allowance." Callers should prefer `errors.As` on
// *SybilRefusal (which still matches via errors.Is(err, ErrSybilRateLimit))
// to surface a richer message including current cap usage + when the
// next slot frees up.
var ErrSybilRateLimit = errors.New("service: too many new keys from this network today")

// SybilRefusal enriches ErrSybilRateLimit with the context the SSH
// layer needs to print a self-diagnosing message: which subnet, how
// full the cap is, and when the oldest in-window entry ages out.
type SybilRefusal struct {
	Subnet              string
	Cap                 int
	FreshCountInWindow  int
	OldestEntryFirstSeen time.Time
	Window              time.Duration
}

func (e *SybilRefusal) Error() string { return ErrSybilRateLimit.Error() }
func (e *SybilRefusal) Is(target error) bool { return target == ErrSybilRateLimit }
// NextSlotFreesAt returns the moment the oldest in-window entry ages
// out and frees a slot. Zero time if no in-window entries.
func (e *SybilRefusal) NextSlotFreesAt() time.Time {
	if e.OldestEntryFirstSeen.IsZero() { return time.Time{} }
	return e.OldestEntryFirstSeen.Add(e.Window)
}

// Admit gates one session. Both identity and ipSubnet must be non-empty.
// On refusal returns *SybilRefusal (which is also errors.Is(ErrSybilRateLimit)).
func (g *KeyGate) Admit(identity, ipSubnet string) error {
	now := g.Now().UTC()
	known, err := g.Repo.AdmitNewKey(identity, ipSubnet, now, g.MaxFreshKeysPerSubnet, g.Window)
	if err != nil {
		if isStorageRateLimitErr(err) {
			// Enrich with subnet snapshot. Best-effort: if the
			// snapshot itself errors, fall back to the bare sentinel
			// so the caller still gets a sensible response.
			count, oldest, sErr := g.Repo.SubnetSnapshot(ipSubnet, now, g.Window)
			if sErr != nil {
				return ErrSybilRateLimit
			}
			return &SybilRefusal{
				Subnet: ipSubnet, Cap: g.MaxFreshKeysPerSubnet,
				FreshCountInWindow: count, OldestEntryFirstSeen: oldest,
				Window: g.Window,
			}
		}
		return err
	}
	_ = known
	return nil
}

// SessionInfo is the per-session keygate state surfaced by `whoami`.
// Filled by KeyGate.Inspect(identity, subnet); zero values are OK.
type SessionInfo struct {
	Subnet            string        // canonical /24 or /48 the session came from
	SubnetFreshCount  int           // current rows in this subnet's window
	SubnetCap         int           // configured limit per subnet
	IdentitySubnets   int           // distinct subnets this identity has rows for
}

// Inspect returns the rollup KeyGate state for an admitted session.
// Called by Whoami AFTER admit, so we don't gate on success.
func (g *KeyGate) Inspect(identity, ipSubnet string) (SessionInfo, error) {
	if g == nil {
		return SessionInfo{}, nil
	}
	now := g.Now().UTC()
	subCount, _, err := g.Repo.SubnetSnapshot(ipSubnet, now, g.Window)
	if err != nil {
		return SessionInfo{}, err
	}
	idSubnets, err := g.Repo.SubnetsForIdentity(identity, now, g.Window)
	if err != nil {
		return SessionInfo{}, err
	}
	return SessionInfo{
		Subnet: ipSubnet,
		SubnetFreshCount: subCount,
		SubnetCap: g.MaxFreshKeysPerSubnet,
		IdentitySubnets: idSubnets,
	}, nil
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
