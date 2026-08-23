package celld

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// PasteRepo is the celld implementation of the paste persistence port.
//
// A create spans TWO cells and celld has no transaction across them, which is
// the same condition that forced the durable intent log on shale: the row is
// addressed by slug because a reader arrives holding one, and the owner's index
// and quota are addressed by identity because the index reader arrives holding
// that. No single cell can serve both.
//
// The sequence is therefore:
//
//  1. identity cell, ONE event: check quota, reserve it, record the intent.
//     A cell is single-threaded, so no concurrent upload by the same owner can
//     interleave between the check and the reservation - which is what makes
//     the per-identity cap exact rather than best-effort.
//  2. paste cell: write the row.
//  3. identity cell: confirm the entry and discharge the intent.
//
// The ORDER is deliberate. Reserving first and failing at step 2 leaves an
// entry charged with no row, which the owner sees as a paste that is briefly
// pending and then reconciled away. Writing the row first would instead leave a
// row visible by slug and absent from its owner's listing, which is an orphan
// nothing is accounted for. Charging first is the recoverable failure, and it
// is recoverable precisely because the intent sits in the cell that owns both
// the quota and the index, so resolution needs no coordination.
type PasteRepo struct {
	base   string
	client *http.Client
}

func NewPasteRepo(base string, c *http.Client) *PasteRepo {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &PasteRepo{base: base, client: c}
}

// pasteRow is the wire and stored shape. Private so the domain type can change
// without a stored-format migration.
type pasteRow struct {
	Slug          string `json:"slug"`
	Identity      string `json:"identity"`
	Status        string `json:"status"`
	Kind          string `json:"kind"`
	ContentSHA    string `json:"contentSha"`
	Size          int    `json:"size"`
	Name          string `json:"name"`
	PinnedVersion int    `json:"pinnedVersion"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

func rowOf(p domain.Paste) pasteRow {
	return pasteRow{
		Slug: p.Slug.String(), Identity: p.Identity.String(),
		Status: string(p.Status), Kind: string(p.Kind),
		ContentSHA: p.ContentSHA, Size: p.Size, Name: p.Name,
		PinnedVersion: p.PinnedVersion,
		CreatedAt:     p.CreatedAt.UTC().UnixMilli(),
		UpdatedAt:     p.UpdatedAt.UTC().UnixMilli(),
	}
}

func (r pasteRow) domain() domain.Paste {
	return domain.Paste{
		Slug: domain.Slug(r.Slug), Identity: domain.Identity(r.Identity),
		Status: domain.PasteStatus(r.Status), Kind: domain.ContentKind(r.Kind),
		ContentSHA: r.ContentSHA, Size: r.Size, Name: r.Name,
		PinnedVersion: r.PinnedVersion,
		CreatedAt:     time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:     time.UnixMilli(r.UpdatedAt).UTC(),
	}
}

func (r *PasteRepo) call(ctx context.Context, method, path, key, val string, body, out any) (int, error) {
	u := fmt.Sprintf("%s%s?%s=%s", r.base, path, key, url.QueryEscape(val))
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("celld: encode %s: %w", path, err)
		}
		payload = b
	}
	rdr := bytes.NewReader(payload)
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return 0, fmt.Errorf("celld: build %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("celld: %s: %w", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("celld: decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// InsertWithQuotaCheck runs the three-step create described on the type.
func (r *PasteRepo) InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
	owner := p.Identity.String()
	intentID := "create:" + p.Slug.String()

	// 1. Reserve. Over-quota is refused HERE, before any row exists, so a
	//    rejected upload leaves nothing behind.
	status, err := r.call(ctx, http.MethodPost, "/identity/reserve", "scope", owner, map[string]any{
		"slug": p.Slug.String(), "size": p.Size, "userCap": userCap,
		"now": now.UTC().UnixMilli(),
		"intent": map[string]any{
			"id": intentID, "kind": "create_paste", "subject": p.Slug.String(),
			"startedAt": now.UTC().UnixMilli(),
		},
	}, nil)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		return domain.ErrOverUserQuota
	}
	if status >= 300 {
		return fmt.Errorf("celld: reserve: unexpected status %d", status)
	}

	// 2. The row. A failure here is compensated, not left dangling.
	if status, err = r.call(ctx, http.MethodPost, "/paste/put", "slug", p.Slug.String(),
		map[string]any{"row": rowOf(p)}, nil); err != nil || status >= 300 {
		_, _ = r.call(ctx, http.MethodPost, "/identity/release", "scope", owner,
			map[string]any{"slug": p.Slug.String(), "intentId": intentID}, nil)
		if err != nil {
			return err
		}
		return fmt.Errorf("celld: paste put: unexpected status %d", status)
	}

	// 3. Confirm. The intent is discharged only once the row is durable.
	if _, err = r.call(ctx, http.MethodPost, "/identity/confirm", "scope", owner,
		map[string]any{"slug": p.Slug.String(), "status": string(p.Status), "intentId": intentID}, nil); err != nil {
		// The row and the reservation both exist, so the paste is correct; only
		// the intent lingers, and resolution clears it. Not an error the caller
		// can act on.
		return nil
	}
	return nil
}

func (r *PasteRepo) Get(slug domain.Slug) (domain.Paste, error) {
	var row pasteRow
	status, err := r.call(context.Background(), http.MethodGet, "/paste/get", "slug", slug.String(), nil, &row)
	if err != nil {
		return domain.Paste{}, err
	}
	if status == http.StatusNotFound {
		return domain.Paste{}, domain.ErrNotFound
	}
	if status >= 300 {
		return domain.Paste{}, fmt.Errorf("celld: paste get: unexpected status %d", status)
	}
	return row.domain(), nil
}

// MarkReady advances a still-pending paste. Absent or already-settled is a
// no-op rather than an error: a late finalizer racing the reconciler is normal.
func (r *PasteRepo) MarkReady(slug domain.Slug) error {
	_, err := r.call(context.Background(), http.MethodPost, "/paste/status", "slug", slug.String(),
		map[string]any{"status": string(domain.PasteStatusReady)}, nil)
	return err
}

// MarkFailed settles the row AND releases the reservation, so a failed paste
// stops charging its owner. Two cells again, and the row is settled first: a
// released reservation with a still-pending row would under-count while the
// paste is still visible.
func (r *PasteRepo) MarkFailed(slug domain.Slug) error {
	var res struct {
		Changed bool `json:"changed"`
	}
	if _, err := r.call(context.Background(), http.MethodPost, "/paste/status", "slug", slug.String(),
		map[string]any{"status": string(domain.PasteStatusFailed)}, &res); err != nil {
		return err
	}
	if !res.Changed {
		// Not pending, so nothing was un-counted and nothing should be released.
		return nil
	}
	got, err := r.Get(slug)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	_, err = r.call(context.Background(), http.MethodPost, "/identity/release", "scope",
		got.Identity.String(), map[string]any{"slug": slug.String()}, nil)
	return err
}

// SumActiveBytesByOwner reads the identity cell's maintained aggregate. A point
// read, not a scan: the cell already holds every entry it charges for.
func (r *PasteRepo) SumActiveBytesByOwner(owner string, _ time.Time) (int, error) {
	var res struct {
		Bytes int `json:"bytes"`
	}
	status, err := r.call(context.Background(), http.MethodGet, "/identity/bytes", "scope", owner, nil, &res)
	if err != nil {
		return 0, err
	}
	if status >= 300 {
		return 0, fmt.Errorf("celld: identity bytes: unexpected status %d", status)
	}
	return res.Bytes, nil
}
