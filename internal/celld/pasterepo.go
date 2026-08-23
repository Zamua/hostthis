package celld

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// 404 and 409 are ANSWERS, not faults: callers map them to domain sentinels
	// (not-found, over-quota). Wrapping them as errors would hide the sentinel
	// behind a transport failure - which it did, until the owner-index suite
	// caught it.
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound &&
		resp.StatusCode != http.StatusConflict {
		// Carry the cell's own explanation up. The identity cell refuses an
		// impossible charge total and says WHICH value it refused; discarding
		// that would trade a legible failure for a bare status code, and the
		// number is the whole diagnostic.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if len(detail) > 0 {
			return resp.StatusCode, fmt.Errorf("celld: %s: status %d: %s", path, resp.StatusCode, detail)
		}
		return resp.StatusCode, nil
	}
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
		"now": now.UTC().UnixMilli(), "status": string(p.Status),
		"kind": string(p.Kind), "name": p.Name, "contentSha": p.ContentSHA,
		"intent": map[string]any{
			"id": intentID, "kind": "create_paste", "subject": p.Slug.String(),
			"startedAt": now.UTC().UnixMilli(),
		},
	}, nil)
	if status == http.StatusConflict {
		return domain.ErrOverUserQuota // an expected refusal, not a fault
	}
	if err != nil {
		return err
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

// --- owner-facing reads -----------------------------------------------------
//
// All four answer from the identity cell's maintained summary rather than by
// visiting each paste cell. Fetching N paste cells to build one listing would
// be N cross-cell reads on a request path, which is the scan the architecture
// forbids wearing a different hat (CLAUDE.md engineering principle 2).

type ownerEntry struct {
	Slug       string `json:"slug"`
	Size       int    `json:"size"`
	Status     string `json:"status"`
	At         int64  `json:"at"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	ContentSHA string `json:"contentSha"`
}

func (r *PasteRepo) ownerEntries(owner string) ([]ownerEntry, error) {
	var out []ownerEntry
	status, err := r.call(context.Background(), http.MethodGet, "/identity/list", "scope", owner, nil, &out)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("celld: identity list: unexpected status %d", status)
	}
	return out, nil
}

// ListByOwner returns the owner's pastes oldest first. A FAILED paste is
// excluded: it charges nothing and cannot be served, so showing it in a listing
// would offer the owner something they cannot act on.
func (r *PasteRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	entries, err := r.ownerEntries(owner)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Paste, 0, len(entries))
	for _, e := range entries {
		if domain.PasteStatus(e.Status) == domain.PasteStatusFailed {
			continue
		}
		at := time.UnixMilli(e.At).UTC()
		out = append(out, domain.Paste{
			Slug: domain.Slug(e.Slug), Identity: domain.Identity(owner),
			Status: domain.PasteStatus(e.Status), Kind: domain.ContentKind(e.Kind),
			ContentSHA: e.ContentSHA, Size: e.Size, Name: e.Name,
			CreatedAt: at, UpdatedAt: at,
		})
	}
	return out, nil
}

func (r *PasteRepo) CountByOwner(owner string) (int, error) {
	entries, err := r.ownerEntries(owner)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if domain.PasteStatus(e.Status) != domain.PasteStatusFailed {
			n++
		}
	}
	return n, nil
}

// OwnerFirstSeen is stamped by the identity cell on its first reservation, so
// it survives every paste being deleted.
func (r *PasteRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	var res struct {
		FirstSeen int64 `json:"firstSeen"`
	}
	status, err := r.call(context.Background(), http.MethodGet, "/identity/firstSeen", "scope", owner, nil, &res)
	if err != nil {
		return time.Time{}, err
	}
	if status >= 300 {
		return time.Time{}, fmt.Errorf("celld: identity firstSeen: unexpected status %d", status)
	}
	if res.FirstSeen == 0 {
		return time.Time{}, nil
	}
	return time.UnixMilli(res.FirstSeen).UTC(), nil
}

// DropStaleOwnerEntry removes an index entry whose paste is gone. The ABSENCE
// is established here, against the paste cell, before the index is touched:
// dropping first would delete a live paste's index entry if the read were
// merely slow.
func (r *PasteRepo) DropStaleOwnerEntry(slug domain.Slug, owner string) (bool, error) {
	if owner == "" {
		return false, nil
	}
	if _, err := r.Get(slug); err == nil {
		return false, nil // the paste exists; the entry is not stale
	} else if !errors.Is(err, domain.ErrNotFound) {
		return false, err
	}
	var res struct {
		Dropped bool `json:"dropped"`
	}
	if _, err := r.call(context.Background(), http.MethodPost, "/identity/drop", "scope", owner,
		map[string]any{"slug": slug.String()}, &res); err != nil {
		return false, err
	}
	return res.Dropped, nil
}

// SetName renames a paste. TWO cells, because the identity summary carries the
// name so that a listing is one point read: the row is authoritative and the
// summary must follow it, or the owner's listing serves a name that is no
// longer true. The row is written FIRST, so a failure between them leaves the
// listing stale rather than leaving it authoritative over a row that never
// changed.
//
// Denormalising a mutable field is what makes this two-cell rather than
// mechanical. That is the cost of the listing being a point read, taken
// deliberately.
func (r *PasteRepo) SetName(slug domain.Slug, name string, wantIdentity domain.Identity, wantCreatedAt time.Time) error {
	var res struct {
		Changed bool   `json:"changed"`
		Reason  string `json:"reason"`
	}
	if _, err := r.call(context.Background(), http.MethodPost, "/paste/rename", "slug", slug.String(),
		map[string]any{
			"name": name, "identity": wantIdentity.String(),
			"createdAt": wantCreatedAt.UTC().UnixMilli(),
		}, &res); err != nil {
		return err
	}
	if !res.Changed {
		if res.Reason == "absent" {
			return domain.ErrNotFound
		}
		return domain.ErrNotFound // a foreign or re-minted slug is not this owner's paste
	}
	_, err := r.call(context.Background(), http.MethodPost, "/identity/touch", "scope",
		wantIdentity.String(), map[string]any{"slug": slug.String(), "name": name}, nil)
	return err
}

// Delete removes a paste and stops charging its owner.
//
// TWO cells, not three. The index entry and the quota reservation are the SAME
// record - the identity cell stores a per-slug entry carrying the size, and the
// charged total is the sum over those entries - so dropping the entry releases
// the quota in one write. They cannot diverge, because there is nothing to keep
// in step.
//
// ORDER: the row first, then the entry.
//
// A crash between them leaves the row gone with its index entry still present,
// which is precisely what DropStaleOwnerEntry repairs and what the owner-index
// conformance pins. The other order would free the owner's quota while the
// paste still exists, letting them exceed their cap - the money-losing
// direction, and the one no repair path is watching for. So the step that is
// irreversible in the wrong direction goes last, where the fewest crashes can
// reach it.
func (r *PasteRepo) Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) error {
	var res struct {
		Removed bool   `json:"removed"`
		Reason  string `json:"reason"`
	}
	if _, err := r.call(context.Background(), http.MethodPost, "/paste/remove", "slug", slug.String(),
		map[string]any{
			"identity":  wantIdentity.String(),
			"createdAt": wantCreatedAt.UTC().UnixMilli(),
		}, &res); err != nil {
		return err
	}
	if !res.Removed {
		// Absent and foreign are the same answer to the caller: this is not
		// your paste to delete, and saying which would leak existence.
		return domain.ErrNotFound
	}
	// Releases the quota by un-counting the slug, which is idempotent by
	// construction: a resolver replaying this cannot under-charge.
	_, err := r.call(context.Background(), http.MethodPost, "/identity/release", "scope",
		wantIdentity.String(), map[string]any{"slug": slug.String()}, nil)
	return err
}
