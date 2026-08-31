package celld

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// PasteRepo is the celld implementation of the paste persistence port.
//
// A create spans two cells and celld has no transaction across them. The row is
// addressed by slug; the owner's index and quota are addressed by identity.
//
// The sequence is therefore:
//
//  1. Identity cell: atomically admit quota, reserve bytes, and record intent.
//  2. Paste cell: write the row.
//  3. Identity cell: confirm the entry and discharge the intent.
//
// The ORDER is deliberate. Reserving first and failing at step 2 leaves an
// entry charged with no row, which the owner sees as a paste that is briefly
// pending. The intent's exact row fingerprint lets recovery either confirm that
// row or fence the generation in the Paste cell before releasing its charge.
// Writing the row first would instead leave a visible, uncharged orphan.
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
	Generation    string `json:"generation"`
	Status        string `json:"status"`
	Kind          string `json:"kind"`
	ContentSHA    string `json:"contentSha"`
	Size          int    `json:"size"`
	Name          string `json:"name"`
	PinnedVersion int    `json:"pinnedVersion"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`

	// The SERVED version's file set. A single-document paste carries a
	// one-entry manifest, so the site surface needs no second shape.
	Manifest domain.Manifest `json:"manifest"`
}

func rowOf(p domain.Paste) pasteRow {
	return pasteRow{
		Slug: p.Slug.String(), Identity: p.Identity.String(),
		Generation: p.Generation,
		Status:     string(p.Status), Kind: string(p.Kind),
		ContentSHA: p.ContentSHA, Size: p.Size, Name: p.Name,
		PinnedVersion: p.PinnedVersion,
		CreatedAt:     p.CreatedAt.UTC().UnixMilli(),
		UpdatedAt:     p.UpdatedAt.UTC().UnixMilli(),
		Manifest:      p.Manifest,
	}
}

func createFingerprint(row pasteRow) (string, error) {
	body, err := json.Marshal(row)
	if err != nil {
		return "", fmt.Errorf("celld: fingerprint paste row: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func newOpaqueID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("celld: generate %s id: %w", prefix, err)
	}
	return prefix + ":" + hex.EncodeToString(raw[:]), nil
}

func (r pasteRow) domain() domain.Paste {
	return domain.Paste{
		Slug: domain.Slug(r.Slug), Identity: domain.Identity(r.Identity),
		Generation: r.Generation,
		Status:     domain.PasteStatus(r.Status), Kind: domain.ContentKind(r.Kind),
		ContentSHA: r.ContentSHA, Size: r.Size, Name: r.Name,
		PinnedVersion: r.PinnedVersion,
		CreatedAt:     time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:     time.UnixMilli(r.UpdatedAt).UTC(),
		Manifest:      r.Manifest,
	}
}

// isCellAnswer reports whether a status is part of the adapter's vocabulary
// rather than a failure. Kept as a list so adding a new one is a deliberate
// edit, not a widened comparison.
func isCellAnswer(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusInsufficientStorage:
		return true
	}
	return false
}

func (r *PasteRepo) call(ctx context.Context, method, path, key, val string, body, out any) (int, error) {
	// The routing param is MERGED rather than appended: a path may already carry
	// query params of its own, and a second bare "?" makes the whole string one
	// unparseable query, which presents as the cell rejecting a param that is
	// plainly in the URL.
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u := fmt.Sprintf("%s%s%s%s=%s", r.base, path, sep, key, url.QueryEscape(val))
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
	// Some statuses are ANSWERS, not faults: the caller maps them to domain
	// sentinels (not-found, slug taken, over quota, at capacity). Wrapping one
	// as an error hides the sentinel behind a transport failure, and the caller's
	// switch on it becomes unreachable - which is what happened to 404/409 until
	// the owner-index suite caught it, and to 413/507 until the room suite did.
	//
	// A status not on this list means the cell did something the adapter does not
	// model, which IS a fault.
	if resp.StatusCode >= 400 && !isCellAnswer(resp.StatusCode) {
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
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return resp.StatusCode, fmt.Errorf("celld: decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// InsertWithQuotaCheck runs the three-step create described on the type.
func (r *PasteRepo) InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
	owner := p.Identity.String()
	generation := p.Generation
	if generation == "" {
		generated, err := newOpaqueID("generation")
		if err != nil {
			return err
		}
		generation = generated
	}
	intentID := "create:" + p.Slug.String() + ":" + generation
	wireRow := rowOf(p)
	wireRow.Generation = generation
	fingerprint, err := createFingerprint(wireRow)
	if err != nil {
		return err
	}

	// 1. Reserve. Over-quota is refused HERE, before any row exists, so a
	//    rejected upload leaves nothing behind.
	status, err := r.call(ctx, http.MethodPost, "/identity/reserve", "scope", owner, map[string]any{
		"slug": p.Slug.String(), "generation": generation,
		"size": p.Size, "userCap": userCap,
		"now": now.UTC().UnixMilli(), "status": string(p.Status),
		"updatedAt": p.UpdatedAt.UTC().UnixMilli(),
		"kind":      string(p.Kind), "name": p.Name, "contentSha": p.ContentSHA,
		"intent": map[string]any{
			"id": intentID, "kind": "create_paste", "subject": p.Slug.String(),
			"fingerprint": fingerprint,
			"startedAt":   now.UTC().UnixMilli(),
		},
	}, nil)
	if status == http.StatusInsufficientStorage {
		return domain.ErrOverUserQuota
	}
	if status == http.StatusConflict {
		return domain.ErrSlugTaken
	}
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("celld: reserve: unexpected status %d", status)
	}

	// 2. A lost response is ambiguous: retry the exact generation once so the
	//    cell's same-generation replay can prove whether the row landed.
	putBody := map[string]any{
		"row": wireRow, "generation": generation, "fingerprint": fingerprint,
	}
	for range 2 {
		status, err = r.call(ctx, http.MethodPost, "/paste/put", "slug", p.Slug.String(), putBody, nil)
		if err == nil {
			break
		}
	}
	if err != nil {
		// The durable intent owns resolution. Releasing here could erase a row
		// whose successful response was the only thing lost.
		return err
	}
	if status == http.StatusConflict {
		_, _ = r.call(ctx, http.MethodPost, "/identity/release", "scope", owner,
			map[string]any{
				"slug": p.Slug.String(), "generation": generation, "intentId": intentID,
			}, nil)
		return domain.ErrSlugTaken
	}
	if status >= 300 {
		return fmt.Errorf("celld: paste put: unexpected status %d", status)
	}

	// 3. Confirm. The intent is discharged only once the row is durable.
	status, err = r.call(ctx, http.MethodPost, "/identity/confirm", "scope", owner,
		map[string]any{
			"slug": p.Slug.String(), "generation": generation,
			"status": string(p.Status), "intentId": intentID,
		}, nil)
	if err != nil {
		// The resolver observes the matching row and completes the confirm.
		return nil
	}
	if status >= 300 {
		return fmt.Errorf("celld: confirm: unexpected status %d", status)
	}
	return nil
}

func (r *PasteRepo) getRow(slug domain.Slug) (pasteRow, error) {
	var row pasteRow
	status, err := r.call(context.Background(), http.MethodGet, "/paste/get", "slug", slug.String(), nil, &row)
	if err != nil {
		return pasteRow{}, err
	}
	if status == http.StatusNotFound {
		return pasteRow{}, domain.ErrNotFound
	}
	if status >= 300 {
		return pasteRow{}, fmt.Errorf("celld: paste get: unexpected status %d", status)
	}
	return row, nil
}

func (r *PasteRepo) Get(slug domain.Slug) (domain.Paste, error) {
	row, err := r.getRow(slug)
	if err != nil {
		return domain.Paste{}, err
	}
	return row.domain(), nil
}

// MarkReady advances a still-pending paste. Absent or already-settled is a
// no-op rather than an error: a late finalizer racing the reconciler is normal.
func (r *PasteRepo) MarkReady(paste domain.Paste) error {
	status, err := r.call(context.Background(), http.MethodPost, "/paste/status", "slug", paste.Slug.String(),
		map[string]any{
			"status": string(domain.PasteStatusReady), "generation": paste.Generation,
		}, nil)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("celld: ready transition: unexpected status %d", status)
	}
	return nil
}

// MarkFailed persists the failed row before driving its absolute allocation to
// zero. The Paste cell owns retries, so a lost response cannot strand quota.
func (r *PasteRepo) MarkFailed(paste domain.Paste) error {
	if paste.Generation == "" {
		return fmt.Errorf("celld: failed paste has no accounting generation")
	}
	status, err := r.callArtifactMutation(context.Background(), "/paste/status", paste.Slug,
		map[string]any{
			"status":     string(domain.PasteStatusFailed),
			"generation": paste.Generation,
			"opId":       "fail:" + paste.Generation,
		}, nil)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("celld: fail accounting: unexpected status %d", status)
	}
	return nil
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
	Slug        string `json:"slug"`
	Size        int    `json:"size"`
	ChargedSize int    `json:"chargedSize"`
	ServedSize  int    `json:"servedSize"`
	Status      string `json:"status"`
	At          int64  `json:"at"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	ContentSHA  string `json:"contentSha"`

	// UpdatedAt orders the listing and LatestVersion is displayed in it. Both
	// are denormalised into the identity cell so a listing stays a POINT READ:
	// fetching them from each paste cell would make one listing N round trips.
	UpdatedAt     int64 `json:"updatedAt"`
	LatestVersion int   `json:"latestVersion"`
	PinnedVersion int   `json:"pinnedVersion"`
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
		updated := at
		if e.UpdatedAt != 0 {
			updated = time.UnixMilli(e.UpdatedAt).UTC()
		}
		latest := e.LatestVersion
		if latest == 0 {
			latest = 1 // every live paste has at least v1
		}
		out = append(out, domain.Paste{
			Slug: domain.Slug(e.Slug), Identity: domain.Identity(owner),
			Status: domain.PasteStatus(e.Status), Kind: domain.ContentKind(e.Kind),
			ContentSHA: e.ContentSHA, Size: e.ServedSize, StoredBytes: e.ChargedSize,
			Name:      e.Name,
			CreatedAt: at, UpdatedAt: updated, LatestVersion: latest,
			PinnedVersion: e.PinnedVersion,
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
	return nil
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
	row, err := r.getRow(slug)
	if err != nil {
		return err
	}
	opID, err := newOpaqueID("remove")
	if err != nil {
		return err
	}
	var res struct {
		Removed bool `json:"removed"`
	}
	status, err := r.callArtifactMutation(context.Background(), "/paste/remove", slug,
		map[string]any{
			"opId": opID, "generation": row.Generation,
			"identity":  wantIdentity.String(),
			"createdAt": wantCreatedAt.UTC().UnixMilli(),
		}, &res)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		return fmt.Errorf("celld: remove accounting conflict")
	}
	if !res.Removed {
		return domain.ErrNotFound
	}
	return nil
}

func (r *PasteRepo) callArtifactMutation(ctx context.Context, path string, slug domain.Slug,
	body, out any,
) (int, error) {
	var status int
	var err error
	for range 2 {
		status, err = r.call(ctx, http.MethodPost, path, "slug", slug.String(), body, out)
		if err == nil {
			return status, nil
		}
	}
	return status, err
}

func (r *PasteRepo) appendArtifact(ctx context.Context, slug domain.Slug, generation string,
	kind domain.ContentKind, contentSHA string, size int, manifest domain.Manifest,
	userCap int64, now time.Time,
) (domain.AppendResult, error) {
	if generation == "" {
		return domain.AppendResult{}, domain.ErrNotFound
	}
	opID, err := newOpaqueID("append")
	if err != nil {
		return domain.AppendResult{}, err
	}
	var res struct {
		Appended  bool `json:"appended"`
		Ver       int  `json:"ver"`
		WasPinned bool `json:"wasPinned"`
	}
	status, err := r.callArtifactMutation(ctx, "/paste/append", slug, map[string]any{
		"opId": opID, "generation": generation, "userCap": userCap,
		"kind": string(kind), "contentSha": contentSHA, "size": size,
		"manifest": manifest, "now": now.UTC().UnixMilli(),
	}, &res)
	if status == http.StatusInsufficientStorage {
		return domain.AppendResult{}, domain.ErrOverUserQuota
	}
	if err != nil {
		return domain.AppendResult{}, err
	}
	if status == http.StatusConflict {
		return domain.AppendResult{}, fmt.Errorf("celld: append accounting conflict")
	}
	if !res.Appended {
		return domain.AppendResult{}, domain.ErrNotFound
	}
	return domain.AppendResult{NewVer: res.Ver, WasPinned: res.WasPinned}, nil
}

// AppendVersionWithQuotaCheck atomically reserves charge and publishes one version.
func (r *PasteRepo) AppendVersionWithQuotaCheck(ctx context.Context, slug domain.Slug, generation string,
	kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time,
) (domain.AppendResult, error) {
	return r.appendArtifact(ctx, slug, generation, kind, contentSHA, size, domain.Manifest{}, userCap, now)
}

// ListVersions is a single-cell read: the appended versions live beside the row.
func (r *PasteRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	var wire []struct {
		Ver        int             `json:"ver"`
		Kind       string          `json:"kind"`
		ContentSHA string          `json:"contentSha"`
		Size       int             `json:"size"`
		CreatedAt  int64           `json:"createdAt"`
		Deleted    bool            `json:"deleted"`
		Manifest   domain.Manifest `json:"manifest"`
	}
	status, err := r.call(context.Background(), http.MethodGet, "/paste/versions", "slug", slug.String(), nil, &wire)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, domain.ErrNotFound
	}
	out := make([]domain.Version, 0, len(wire))
	for _, w := range wire {
		out = append(out, domain.Version{
			Slug: slug, VerNum: w.Ver, Kind: domain.ContentKind(w.Kind),
			ContentSHA: w.ContentSHA, Size: w.Size, Manifest: w.Manifest,
			CreatedAt: time.UnixMilli(w.CreatedAt).UTC(), Deleted: w.Deleted,
		})
	}
	return out, nil
}

// DeleteVersion removes a retained version and re-charges the owner.
//
// TWO cells, like append, but NOT its mirror image. Append can be refused, so
// it checks quota first and a rejection leaves nothing behind. This cannot be
// refused, so there is nothing to check and the only question is order:
//
// the version goes FIRST, then the new total.
//
// A crash between them leaves the owner charged for bytes that are gone, which
// over-charges - conservative, visible to the owner, and repairable, because an
// absolute total is reconstructible from the paste cell. The other order frees
// the charge while the bytes remain, which under-charges silently and is the
// direction nothing watches.
func (r *PasteRepo) DeleteVersion(slug domain.Slug, generation string, ver int) error {
	if generation == "" {
		return domain.ErrNotFound
	}
	opID, err := newOpaqueID("delete-version")
	if err != nil {
		return err
	}
	var res struct {
		Deleted bool   `json:"deleted"`
		Error   string `json:"error"`
	}
	status, err := r.callArtifactMutation(context.Background(), "/paste/delversion", slug,
		map[string]any{"opId": opID, "generation": generation, "ver": ver}, &res)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		if res.Error == "version-served" {
			return domain.ErrVersionCurrentlyServed
		}
		return fmt.Errorf("celld: delete-version accounting conflict")
	}
	if !res.Deleted {
		return domain.ErrNotFound
	}
	return nil
}

// GetVersion reads one retained version. Single-cell.
func (r *PasteRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	all, err := r.ListVersions(slug)
	if err != nil {
		return domain.Version{}, err
	}
	for _, v := range all {
		if v.VerNum == ver {
			return v, nil
		}
	}
	return domain.Version{}, domain.ErrNotFound
}

// IsVersionServed reports whether a version is the one the public URL resolves
// to: the pin when set, otherwise the latest. Single-cell.
func (r *PasteRepo) IsVersionServed(slug domain.Slug, ver int) (bool, error) {
	got, err := r.Get(slug)
	if err != nil {
		return false, err
	}
	if got.PinnedVersion != 0 {
		return got.PinnedVersion == ver, nil
	}
	all, err := r.ListVersions(slug)
	if err != nil {
		return false, err
	}
	latest := 1 // v1 lives in the row; the list holds only appended versions
	for _, v := range all {
		if v.VerNum > latest {
			latest = v.VerNum
		}
	}
	return ver == latest, nil
}

// SetPinnedVersion and Unpin change what the public URL SERVES, not what is
// retained, so neither touches the charge and both stay inside the paste cell.
// Checked against the identity summary rather than assumed: it carries name,
// status, size and kind, and a pin moves none of them.
func (r *PasteRepo) SetPinnedVersion(slug domain.Slug, generation string, ver domain.Version) error {
	return r.setPin(slug, generation, ver.VerNum)
}

func (r *PasteRepo) Unpin(slug domain.Slug, generation string) error {
	return r.setPin(slug, generation, 0)
}

// setPin also updates the owner index, which renders the listing from its own
// denormalised entry: without this the pin is honoured when serving but
// invisible in `list`, so an owner cannot see which version their URL is stuck
// to. Found by migrating a pinned paste and reading the listing afterwards.
func (r *PasteRepo) setPin(slug domain.Slug, generation string, ver int) error {
	if generation == "" {
		return domain.ErrNotFound
	}
	opID, err := newOpaqueID("pin")
	if err != nil {
		return err
	}
	var res struct {
		Pinned bool `json:"pinned"`
	}
	status, err := r.callArtifactMutation(context.Background(), "/paste/pin", slug,
		map[string]any{"opId": opID, "generation": generation, "ver": ver}, &res)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		return fmt.Errorf("celld: pin accounting conflict")
	}
	if !res.Pinned {
		return domain.ErrNotFound
	}
	return nil
}

// OwnerSummary is the `whoami` view. One point read of the identity cell, which
// already holds every entry it charges for, plus the first-seen stamp.
//
// SiteBytes is zero: sites are not yet on celld, so reporting anything else
// would be inventing a number. That is a gap in the port's coverage rather than
// a property of the backend, and it is named here so a reader does not mistake
// an unimplemented surface for an empty one.
func (r *PasteRepo) OwnerSummary(owner string, now time.Time) (domain.OwnerSummary, error) {
	entries, err := r.ownerEntries(owner)
	if err != nil {
		return domain.OwnerSummary{}, err
	}
	var active int
	var bytes int64
	for _, e := range entries {
		if domain.PasteStatus(e.Status) == domain.PasteStatusFailed {
			continue
		}
		active++
		bytes += int64(e.ChargedSize)
	}
	first, err := r.OwnerFirstSeen(owner)
	if err != nil {
		return domain.OwnerSummary{}, err
	}
	return domain.OwnerSummary{Active: active, FirstSeen: first, PasteBytes: bytes}, nil
}

// --- the site surface -------------------------------------------------------
//
// A directory IS a paste whose version carries N manifest entries, so these
// three methods are all storage.Sites needs to run on cells: no site class, no
// second quota, no second enumeration index. The slug namespace stays global
// because the paste cell IS the slug.

// AppendManifestVersion appends a retained file-set version.
func (r *PasteRepo) AppendManifestVersion(ctx context.Context, slug domain.Slug, generation string,
	m domain.Manifest, root domain.ManifestEntry, size int, userCap int64, now time.Time,
) (domain.AppendResult, error) {
	return r.appendArtifact(ctx, slug, generation, domain.KindSite, root.SHA, size, m, userCap, now)
}

// urlQuery escapes a value for a query string. Named rather than inlined so
// every call site escapes, which is what keeps a key containing & or = from
// silently becoming two parameters.
func urlQuery(v string) string { return url.QueryEscape(v) }
