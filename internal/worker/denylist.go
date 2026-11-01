package worker

import (
	"context"
	"net/http"

	"github.com/Zamua/hostthis/internal/domain"
)

// DenylistRepo reads the operator denylist from its one cell (docs/SPEC.md
// "Operator denylist"). Entries are written by the operator through the
// Worker's internal API, never by hostthisd.
type DenylistRepo struct {
	*cell
}

func NewDenylistRepo(base string, c *http.Client) *DenylistRepo {
	return &DenylistRepo{cell: newCell(base, c)}
}

func (r *DenylistRepo) Denylist(ctx context.Context) ([]domain.DenyEntry, error) {
	var res struct {
		Entries []struct {
			Entry  string `json:"entry"`
			Reason string `json:"reason"`
		} `json:"entries"`
	}
	if _, err := r.call(ctx, opDenylistList, "", nil, &res); err != nil {
		return nil, err
	}
	out := make([]domain.DenyEntry, 0, len(res.Entries))
	for _, e := range res.Entries {
		out = append(out, domain.DenyEntry{Entry: e.Entry, Reason: e.Reason})
	}
	return out, nil
}
