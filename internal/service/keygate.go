package service

import (
	"errors"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// KeyGateRepo is the persistence interface backing the per-IP-subnet
// Sybil rate limit. internal/storage.KeyGateRepo satisfies it.
type KeyGateRepo interface {
	// AdmitNewKey is the atomic check: returns knownAlready=true if
	// (identity, subnet) is already on file (no rate-limit accounting),
	// or admits + records a fresh key when subnet hasn't hit
	// `limitPerSubnet` distinct new fingerprints in the window.
	AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error)

	// DeleteFirstSeenOlderThan removes rows past the rate-limit window, which
	// cannot affect any admission decision.
	DeleteFirstSeenOlderThan(cutoff time.Time) (int, error)

	// SubnetSnapshot returns how many rows from this subnet count toward the
	// per-subnet cap, and the earliest in-window first_seen: adding `window`
	// to it gives the moment the next slot frees up. A subnet with no
	// in-window rows returns (0, time.Time{}).
	SubnetSnapshot(subnet string, now time.Time, window time.Duration) (freshCount int, oldestFirstSeen time.Time, err error)

	// SubnetsForIdentity returns how many distinct subnets this identity has
	// in-window rows for: the count of networks its key is grandfathered on.
	SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error)
}

// KeyGate gates new SSH sessions: a fresh (identity, ip-subnet) pair is
// admitted only if the subnet has not already first-seen more than
// MaxFreshKeysPerSubnet distinct fingerprints within Window. An
// already-recorded pair passes through with no accounting.
//
// The SSH server calls Admit once per session, before any verb dispatch.
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

// ErrSybilRateLimit is the sentinel for a subnet past its fresh-key allowance.
// Prefer errors.As on *SybilRefusal (which still matches errors.Is against
// this) to surface cap usage and when the next slot frees up.
var ErrSybilRateLimit = errors.New("service: too many new keys from this network today")

// SybilRefusal enriches ErrSybilRateLimit with what the SSH layer needs for a
// self-diagnosing message: which subnet, how full the cap is, and when the
// oldest in-window entry ages out.
type SybilRefusal struct {
	Subnet               string
	Cap                  int
	FreshCountInWindow   int
	OldestEntryFirstSeen time.Time
	Window               time.Duration
}

func (e *SybilRefusal) Error() string        { return ErrSybilRateLimit.Error() }
func (e *SybilRefusal) Is(target error) bool { return target == ErrSybilRateLimit }

// NextSlotFreesAt returns when the oldest in-window entry ages out and frees a
// slot. Zero time if there are none.
func (e *SybilRefusal) NextSlotFreesAt() time.Time {
	if e.OldestEntryFirstSeen.IsZero() {
		return time.Time{}
	}
	return e.OldestEntryFirstSeen.Add(e.Window)
}

// Admit gates one session. Both identity and ipSubnet must be non-empty.
// On refusal returns *SybilRefusal (which is also errors.Is(ErrSybilRateLimit)).
func (g *KeyGate) Admit(identity, ipSubnet string) error {
	now := g.Now().UTC()
	known, err := g.Repo.AdmitNewKey(identity, ipSubnet, now, g.MaxFreshKeysPerSubnet, g.Window)
	if err != nil {
		if isStorageRateLimitErr(err) {
			// Best-effort enrichment: a failing snapshot falls back to the
			// bare sentinel rather than turning a refusal into an error.
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

// SessionInfo is the per-session keygate state surfaced by `whoami`. Zero
// values are acceptable.
type SessionInfo struct {
	Subnet           string // canonical /24 or /48 the session came from
	SubnetFreshCount int    // current rows in this subnet's window
	SubnetCap        int    // configured limit per subnet
	IdentitySubnets  int    // distinct subnets this identity has rows for
}

// Inspect returns the rollup KeyGate state for an admitted session. It runs
// after Admit and never gates admission.
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
		Subnet:           ipSubnet,
		SubnetFreshCount: subCount,
		SubnetCap:        g.MaxFreshKeysPerSubnet,
		IdentitySubnets:  idSubnets,
	}, nil
}

// PruneOldRows deletes rows older than the rate-limit window, keeping the table
// bounded. The sweep service calls it every tick.
func (g *KeyGate) PruneOldRows(now time.Time) (int, error) {
	cutoff := now.Add(-g.Window)
	return g.Repo.DeleteFirstSeenOlderThan(cutoff)
}

// isStorageRateLimitErr reports whether err is the keygate rate-limit sentinel
// (domain.ErrTooManyNewKeys, aliased as storage.ErrTooManyNewKeys). Matched by
// identity through any wrapping, never by message text: a string compare
// demotes a wrapped rate-limit error to a generic failure and loses the
// enriched Sybil refusal.
func isStorageRateLimitErr(err error) bool {
	return errors.Is(err, domain.ErrTooManyNewKeys)
}
