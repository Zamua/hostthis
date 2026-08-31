package celld

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Zamua/hostthis/internal/domain"
	"time"
)

// KeyGateRepo is the celld implementation of Sybil admission.
//
// The SUBNET is the rate-limit unit, so it is the cell. The Worker serializes
// each admission through blockConcurrencyWhile, making the check and record one
// exact decision.
//
// SubnetsForIdentity asks how many networks one key is grandfathered on. Admission
// maintains that reverse index in the Identity cell so the query stays a point read.
type KeyGateRepo struct {
	base   string
	client *http.Client
}

func NewKeyGateRepo(base string, c *http.Client) *KeyGateRepo {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &KeyGateRepo{base: base, client: c}
}

func (r *KeyGateRepo) paste() *PasteRepo { return &PasteRepo{base: r.base, client: r.client} }

// AdmitNewKey records a fresh key against its subnet, or reports that it was
// already on file.
//
// The subnet cell decides, because it owns the limit. The identity cell is told
// afterwards, and only on a real admission: noting a subnet for a key that was
// refused would grandfather it on a network it never got onto.
func (r *KeyGateRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int,
	window time.Duration,
) (bool, error) {
	var res struct {
		KnownAlready bool `json:"knownAlready"`
		Admitted     bool `json:"admitted"`
	}
	if _, err := r.paste().call(context.Background(), http.MethodPost, "/subnet/admit", "subnet", subnet,
		map[string]any{
			"identity": identity, "now": now.UTC().UnixMilli(),
			"window": window.Milliseconds(), "limit": limitPerSubnet,
		}, &res); err != nil {
		return false, err
	}
	if !res.Admitted {
		return false, domain.ErrTooManyNewKeys
	}
	if !res.KnownAlready {
		if _, err := r.paste().call(context.Background(), http.MethodPost, "/identity/notesubnet",
			"scope", identity, map[string]any{"subnet": subnet, "now": now.UTC().UnixMilli()}, nil); err != nil {
			return false, err
		}
	}
	return res.KnownAlready, nil
}

// SubnetSnapshot reports how many in-window rows a subnet holds and when the
// oldest was recorded, which is what tells a refused client when a slot frees.
func (r *KeyGateRepo) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error) {
	var res struct {
		FreshCount      int   `json:"freshCount"`
		OldestFirstSeen int64 `json:"oldestFirstSeen"`
	}
	u := fmt.Sprintf("/subnet/snapshot?now=%d&window=%d", now.UTC().UnixMilli(), window.Milliseconds())
	if _, err := r.paste().call(context.Background(), http.MethodGet, u, "subnet", subnet, nil, &res); err != nil {
		return 0, time.Time{}, err
	}
	if res.OldestFirstSeen == 0 {
		return res.FreshCount, time.Time{}, nil
	}
	return res.FreshCount, time.UnixMilli(res.OldestFirstSeen).UTC(), nil
}

// SubnetsForIdentity is a point read of the identity cell's reverse index.
func (r *KeyGateRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	var res struct {
		Count int `json:"count"`
	}
	u := fmt.Sprintf("/identity/subnets?now=%d&window=%d", now.UTC().UnixMilli(), window.Milliseconds())
	if _, err := r.paste().call(context.Background(), http.MethodGet, u, "scope", identity, nil, &res); err != nil {
		return 0, err
	}
	return res.Count, nil
}
