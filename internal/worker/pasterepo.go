package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// PasteRepo is the Worker implementation of the paste persistence port.
//
// A create spans two cells and no transaction spans cells. The row is
// addressed by slug; the owner's index and quota are addressed by identity.
//
// The sequence is therefore:
//
//  1. Identity cell: atomically admit quota, reserve bytes, and record intent.
//  2. Paste cell: write the row.
//  3. Identity cell: confirm the entry and discharge the intent.
//
// Reserving first and failing at step 2 leaves an entry charged with no row,
// which the owner sees as a briefly pending paste; the intent's row
// fingerprint lets recovery confirm that row or fence the generation before
// releasing its charge. Writing the row first would instead leave a visible,
// uncharged orphan.
type PasteRepo struct {
	*cell

	// Logger receives one line per stored manifest skipped as undecodable. Nil
	// drops them.
	Logger *log.Logger
}

func NewPasteRepo(base string, c *http.Client) *PasteRepo { return &PasteRepo{cell: newCell(base, c)} }

// pasteRow is the wire and stored shape. Private so the domain type can change
// without a stored-format migration.
type pasteRow struct {
	Slug          string `json:"slug"`
	Identity      string `json:"identity"`
	Generation    string `json:"generation"`
	Status        string `json:"status"`
	Kind          string `json:"kind"`
	UploadID      string `json:"uploadId,omitempty"`
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
		UploadID: p.UploadID, Size: p.Size, Name: p.Name,
		PinnedVersion: p.PinnedVersion,
		CreatedAt:     p.CreatedAt.UTC().UnixMilli(),
		UpdatedAt:     p.UpdatedAt.UTC().UnixMilli(),
		Manifest:      p.Manifest,
	}
}

func createFingerprint(row pasteRow) (string, error) {
	body, err := json.Marshal(row)
	if err != nil {
		return "", fmt.Errorf("worker: fingerprint paste row: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func newOpaqueID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("worker: generate %s id: %w", prefix, err)
	}
	return prefix + ":" + hex.EncodeToString(raw[:]), nil
}

func (r pasteRow) domain() domain.Paste {
	return domain.Paste{
		Slug: domain.Slug(r.Slug), Identity: domain.Identity(r.Identity),
		Generation: r.Generation,
		Status:     domain.PasteStatus(r.Status), Kind: domain.ContentKind(r.Kind),
		UploadID: r.UploadID, Size: r.Size, Name: r.Name,
		PinnedVersion: r.PinnedVersion,
		CreatedAt:     time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:     time.UnixMilli(r.UpdatedAt).UTC(),
		Manifest:      r.Manifest,
	}
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
	var refused refusal
	status, err := r.call(ctx, opIdentityReserve, owner, reserveRequest{
		Slug: p.Slug.String(), Generation: generation,
		Size: p.Size, UserCap: userCap,
		Now: now.UTC().UnixMilli(), Status: p.Status,
		UpdatedAt: p.UpdatedAt.UTC().UnixMilli(),
		Kind:      p.Kind, Name: p.Name,
		Intent: createIntent{
			ID: intentID, Kind: intentCreatePaste, Subject: p.Slug.String(),
			Fingerprint: fingerprint,
			StartedAt:   now.UTC().UnixMilli(),
		},
	}, &refused)
	switch {
	case err != nil:
		return err
	case status == http.StatusInsufficientStorage:
		return domain.ErrOverUserQuota
	case status == http.StatusConflict && refused.Error == codeSlugTaken:
		return domain.ErrSlugTaken
	case status >= 300:
		return fmt.Errorf("worker: reserve: status %d %s", status, refused.Error)
	}

	// 2. A lost response is ambiguous: retry the exact generation once so the
	//    cell's same-generation replay can prove whether the row landed. A
	//    transport failure is NOT released: the durable intent owns resolution,
	//    and releasing could erase a row whose success response was lost.
	status, err = r.callArtifactMutation(ctx, opPastePut, p.Slug, pastePutRequest{
		Row: wireRow, Generation: generation, Fingerprint: fingerprint,
	}, &refused)
	if err != nil {
		return err
	}
	if status == http.StatusConflict && (refused.Error == codeSlugTaken || refused.Error == codeCreateAborted) {
		// This generation's row can never land, so its charge is released.
		_, _ = r.call(ctx, opIdentityRelease, owner,
			releaseRequest{
				Slug: p.Slug.String(), Generation: generation, IntentID: intentID,
			}, nil)
		return domain.ErrSlugTaken
	}
	if status >= 300 {
		// Any other conflict may name this generation's own row; the intent
		// resolves it.
		return fmt.Errorf("worker: paste put: status %d %s", status, refused.Error)
	}

	// 3. Confirm. The intent is discharged only once the row is durable. A
	//    lost response is fine: the resolver observes the matching row and
	//    completes the confirm.
	status, err = r.call(ctx, opIdentityConfirm, owner,
		confirmRequest{
			Slug: p.Slug.String(), Generation: generation,
			Status: p.Status, IntentID: intentID,
		}, nil)
	if err != nil {
		return nil
	}
	return answer("confirm", status, nil, nil)
}

func (r *PasteRepo) getRow(slug domain.Slug) (pasteRow, error) {
	// The outer Manifest shadows pasteRow's, so the manifest decodes apart from
	// the rest of the row, which stays strict.
	var wire struct {
		pasteRow
		Manifest json.RawMessage `json:"manifest"`
	}
	status, err := r.call(context.Background(), opPasteGet, slug.String(), nil, &wire)
	// A quarantine answer's body names the owner, which lands in the row.
	if err == nil && status == http.StatusUnavailableForLegalReasons {
		return pasteRow{}, &domain.QuarantinedError{Identity: domain.Identity(wire.Identity)}
	}
	if err := answer("paste get", status, err, notFound); err != nil {
		return pasteRow{}, err
	}
	row := wire.pasteRow
	row.Manifest = r.storedManifest(wire.Manifest, slug, 0)
	return row, nil
}

// storedManifest decodes a stored manifest. One of the wrong shape reads as no
// manifest, so reads, version listings and deletes of a damaged record keep
// working (CLAUDE.md engineering principle 3). ver is 0 for the row's manifest.
func (r *PasteRepo) storedManifest(raw json.RawMessage, slug domain.Slug, ver int) domain.Manifest {
	var m domain.Manifest
	if len(raw) == 0 {
		return m
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		if r.Logger != nil {
			where := slug.String()
			if ver != 0 {
				where = fmt.Sprintf("%s v%d", slug, ver)
			}
			r.Logger.Printf("worker: paste %s: undecodable manifest read as none: %v", where, err)
		}
		// A failed decode may have filled some entries; none of them is trusted.
		return domain.Manifest{}
	}
	return m
}

func (r *PasteRepo) Get(slug domain.Slug) (domain.Paste, error) {
	row, err := r.getRow(slug)
	if err != nil {
		return domain.Paste{}, err
	}
	return row.domain(), nil
}

// MarkReady advances a still-pending paste. An already-settled paste is a
// no-op: a late finalizer racing the reconciler is normal. A paste that is
// gone, absent or re-minted under another generation, is domain.ErrNotFound.
func (r *PasteRepo) MarkReady(paste domain.Paste) error {
	var res struct {
		Reason string `json:"reason"`
		Error  string `json:"error"`
	}
	status, err := r.call(context.Background(), opPasteStatus,
		paste.Slug.String(), pasteStatusRequest{
			Status: domain.PasteStatusReady, Generation: paste.Generation,
		}, &res)
	switch {
	case err != nil:
		return err
	case status < 300 && res.Reason == reasonAbsent, replaced(status, res.Error):
		return domain.ErrNotFound
	}
	return answer("ready transition", status, nil, nil)
}

// MarkFailed persists the failed row before driving its absolute allocation to
// zero. The Paste cell owns retries, so a lost response cannot strand quota.
func (r *PasteRepo) MarkFailed(paste domain.Paste) error {
	if paste.Generation == "" {
		return fmt.Errorf("worker: failed paste has no accounting generation")
	}
	status, err := r.callArtifactMutation(context.Background(), opPasteStatus, paste.Slug,
		pasteStatusRequest{
			Status:     domain.PasteStatusFailed,
			Generation: paste.Generation,
			OpID:       "fail:" + paste.Generation,
		}, nil)
	return answer("fail accounting", status, err, map[int]error{http.StatusLocked: domain.ErrBusy})
}

// SumActiveBytesByOwner reads the identity cell's maintained aggregate. A point
// read, not a scan: the cell already holds every entry it charges for.
func (r *PasteRepo) SumActiveBytesByOwner(owner string, _ time.Time) (int, error) {
	var res struct {
		Bytes int `json:"bytes"`
	}
	if err := r.ask(context.Background(), "identity bytes", opIdentityBytes, owner, nil, &res, nil); err != nil {
		return 0, err
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

	// UpdatedAt and LatestVersion are denormalised into the identity cell so a
	// listing stays a POINT READ rather than N round trips.
	UpdatedAt     int64 `json:"updatedAt"`
	LatestVersion int   `json:"latestVersion"`
	PinnedVersion int   `json:"pinnedVersion"`
}

func (r *PasteRepo) ownerEntries(owner string) ([]ownerEntry, error) {
	var out []ownerEntry
	if err := r.ask(context.Background(), "identity list", opIdentityList, owner, nil, &out, nil); err != nil {
		return nil, err
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
			Size: e.ServedSize, StoredBytes: e.ChargedSize,
			Name:      e.Name,
			CreatedAt: at, UpdatedAt: updated, LatestVersion: latest,
			PinnedVersion: e.PinnedVersion,
		})
	}
	return out, nil
}

// OwnerFirstSeen is stamped by the identity cell on its first reservation, so
// it survives every paste being deleted.
func (r *PasteRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	var res struct {
		FirstSeen int64 `json:"firstSeen"`
	}
	if err := r.ask(context.Background(), "identity firstSeen", opIdentityFirstSeen,
		owner, nil, &res, nil); err != nil {
		return time.Time{}, err
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
	if _, err := r.Get(slug); err == nil || errors.Is(err, domain.ErrQuarantined) {
		return false, nil // the paste exists; the entry is not stale
	} else if !errors.Is(err, domain.ErrNotFound) {
		return false, err
	}
	var res struct {
		Dropped bool `json:"dropped"`
	}
	if _, err := r.call(context.Background(), opIdentityDrop, owner,
		dropRequest{Slug: slug.String()}, &res); err != nil {
		return false, err
	}
	return res.Dropped, nil
}

// SetName renames a paste. TWO cells, because the identity summary carries the
// name so that a listing is one point read. The row is authoritative and is
// written FIRST, so a failure between them leaves the listing stale rather
// than authoritative over a row that never changed.
func (r *PasteRepo) SetName(slug domain.Slug, name string, wantIdentity domain.Identity, wantCreatedAt time.Time) error {
	var res struct {
		Changed bool `json:"changed"`
	}
	if _, err := r.call(context.Background(), opPasteRename, slug.String(),
		renameRequest{
			Name: name, Identity: wantIdentity.String(),
			CreatedAt: wantCreatedAt.UTC().UnixMilli(),
		}, &res); err != nil {
		return err
	}
	if !res.Changed {
		// Absent, foreign and re-minted slugs all read as not this owner's paste.
		return domain.ErrNotFound
	}
	return nil
}

// Delete removes a paste and stops charging its owner.
//
// TWO cells, not three: the index entry and the quota reservation are the
// SAME identity-cell record (a per-slug entry carrying the size, summed for
// the charged total), so dropping the entry releases the quota in one write.
//
// ORDER: the row first, then the entry. A crash between them leaves a stale
// entry, which DropStaleOwnerEntry repairs. The other order would free quota
// while the paste still exists, which no repair path watches for.
//
// The cell answers with the uploads of the incarnation it removed, kept in its
// receipt, so a retried request names the same bytes rather than a re-mint's.
func (r *PasteRepo) Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) ([]string, error) {
	row, err := r.getRow(slug)
	if err != nil {
		return nil, err
	}
	opID, err := newOpaqueID("remove")
	if err != nil {
		return nil, err
	}
	var res struct {
		Removed bool     `json:"removed"`
		Uploads []string `json:"uploads"`
		Error   string   `json:"error"`
	}
	status, err := r.callArtifactMutation(context.Background(), opPasteRemove, slug,
		removeRequest{
			OpID: opID, Generation: row.Generation,
			Identity:  wantIdentity.String(),
			CreatedAt: wantCreatedAt.UTC().UnixMilli(),
		}, &res)
	if err != nil {
		return nil, err
	}
	if status == http.StatusLocked {
		return nil, domain.ErrBusy
	}
	if replaced(status, res.Error) {
		return nil, domain.ErrNotFound
	}
	if status == http.StatusConflict {
		return nil, fmt.Errorf("worker: remove accounting conflict")
	}
	if !res.Removed {
		return nil, domain.ErrNotFound
	}
	uploads := make([]string, 0, len(res.Uploads))
	for _, id := range res.Uploads {
		if id != "" {
			uploads = append(uploads, id)
		}
	}
	return uploads, nil
}

func (r *PasteRepo) callArtifactMutation(ctx context.Context, rt route, slug domain.Slug,
	body, out any,
) (int, error) {
	var status int
	var err error
	for range 2 {
		status, err = r.call(ctx, rt, slug.String(), body, out)
		if err == nil {
			return status, nil
		}
	}
	return status, err
}

func (r *PasteRepo) appendArtifact(ctx context.Context, slug domain.Slug, generation string,
	kind domain.ContentKind, uploadID string, manifest domain.Manifest, size int,
	userCap int64, now time.Time, base int,
) (domain.AppendResult, error) {
	opID, err := newOpaqueID("append")
	if err != nil {
		return domain.AppendResult{}, err
	}
	var res struct {
		Appended  *bool  `json:"appended"`
		Reason    string `json:"reason"`
		Error     string `json:"error"`
		Ver       int    `json:"ver"`
		WasPinned bool   `json:"wasPinned"`
	}
	body := appendRequest{
		OpID: opID, Generation: generation, UserCap: userCap,
		Kind: kind, UploadID: uploadID, Size: size,
		Manifest: manifest, Now: now.UTC().UnixMilli(),
	}
	if base > 0 {
		body.Base = &base
	}
	status, err := r.callArtifactMutation(ctx, opPasteAppend, slug, body, &res)
	// Only an identified refusal is definitive: the service deletes the upload's
	// bytes on one, and any other answer may still publish the version.
	switch {
	case err != nil:
		return domain.AppendResult{}, err
	case status == http.StatusInsufficientStorage:
		return domain.AppendResult{}, domain.ErrOverUserQuota
	case status == http.StatusRequestEntityTooLarge:
		return domain.AppendResult{}, fmt.Errorf("%w: version too large to store", domain.ErrTooManyFiles)
	case status == http.StatusLocked:
		return domain.AppendResult{}, domain.ErrBusy
	case replaced(status, res.Error):
		return domain.AppendResult{}, domain.ErrNotFound
	case status == http.StatusConflict && res.Error == codeBaseMoved:
		return domain.AppendResult{}, domain.ErrBaseMoved
	case status == http.StatusConflict && res.Error == codeBadReuse:
		return domain.AppendResult{}, domain.ErrBadReuse
	case status >= 300 || res.Appended == nil:
		return domain.AppendResult{}, fmt.Errorf("worker: append: unrecognised answer: status %d %s", status, res.Error)
	case *res.Appended:
		return domain.AppendResult{NewVer: res.Ver, WasPinned: res.WasPinned}, nil
	case res.Reason == reasonAbsent:
		return domain.AppendResult{}, domain.ErrNotFound
	default:
		return domain.AppendResult{}, fmt.Errorf("worker: append: unrecognised refusal %q", res.Reason)
	}
}

// AppendVersionWithQuotaCheck atomically reserves charge and publishes one version.
func (r *PasteRepo) AppendVersionWithQuotaCheck(ctx context.Context, slug domain.Slug, generation string,
	kind domain.ContentKind, uploadID string, manifest domain.Manifest, size int, userCap int64, now time.Time,
) (domain.AppendResult, error) {
	return r.appendArtifact(ctx, slug, generation, kind, uploadID, manifest, size, userCap, now, 0)
}

// ListVersions is a single-cell read: the appended versions live beside the row.
func (r *PasteRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	var wire []struct {
		Ver       int    `json:"ver"`
		Kind      string `json:"kind"`
		UploadID  string `json:"uploadId"`
		Size      int    `json:"size"`
		CreatedAt int64  `json:"createdAt"`
		Deleted   bool   `json:"deleted"`
	}
	status, err := r.call(context.Background(), opPasteVersions, slug.String(), nil, &wire)
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
			UploadID: w.UploadID, Size: w.Size,
			CreatedAt: time.UnixMilli(w.CreatedAt).UTC(), Deleted: w.Deleted,
		})
	}
	return out, nil
}

// DeleteVersion removes a retained version and re-charges the owner.
//
// TWO cells: the version goes FIRST, then the new total. A crash between them
// leaves the owner charged for bytes that are gone, which over-charges:
// conservative, visible, and repairable from the paste cell. The other order
// frees the charge while the bytes remain, which under-charges silently and is
// the direction nothing watches.
//
// The answer names what the delete freed, decided in its commit; a delete of
// a version already deleted frees nothing.
func (r *PasteRepo) DeleteVersion(slug domain.Slug, generation string, ver int) (domain.VersionRelease, error) {
	opID, err := newOpaqueID("delete-version")
	if err != nil {
		return domain.VersionRelease{}, err
	}
	var res struct {
		Deleted  bool      `json:"deleted"`
		Upload   string    `json:"upload"`
		Prefixes *[]string `json:"prefixes"`
		Keys     []string  `json:"keys"`
		Freed    int       `json:"freed"`
		Error    string    `json:"error"`
	}
	status, err := r.callArtifactMutation(context.Background(), opPasteDelVersion, slug,
		versionRequest{OpID: opID, Generation: generation, Ver: ver}, &res)
	if err != nil {
		return domain.VersionRelease{}, err
	}
	if status == http.StatusLocked {
		return domain.VersionRelease{}, domain.ErrBusy
	}
	if replaced(status, res.Error) {
		return domain.VersionRelease{}, domain.ErrNotFound
	}
	if status == http.StatusConflict {
		if res.Error == codeVersionServed {
			return domain.VersionRelease{}, domain.ErrVersionCurrentlyServed
		}
		return domain.VersionRelease{}, fmt.Errorf("worker: delete-version accounting conflict")
	}
	if !res.Deleted {
		return domain.VersionRelease{}, domain.ErrNotFound
	}
	rel := domain.VersionRelease{Upload: res.Upload, Keys: res.Keys, Freed: res.Freed}
	if res.Prefixes != nil {
		rel.Prefixes, rel.Listed = *res.Prefixes, true
	}
	return rel, nil
}

// HeadManifest reads the latest live version and its manifest: the base a
// redeploy is computed against.
func (r *PasteRepo) HeadManifest(slug domain.Slug) (int, domain.Manifest, error) {
	var res struct {
		Found    bool            `json:"found"`
		Ver      int             `json:"ver"`
		Manifest json.RawMessage `json:"manifest"`
	}
	status, err := r.call(context.Background(), opPasteHead, slug.String(), nil, &res)
	if err != nil {
		return 0, domain.Manifest{}, err
	}
	if status == http.StatusNotFound || !res.Found {
		return 0, domain.Manifest{}, domain.ErrNotFound
	}
	return res.Ver, r.storedManifest(res.Manifest, slug, res.Ver), nil
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
// invisible in `list`.
func (r *PasteRepo) setPin(slug domain.Slug, generation string, ver int) error {
	opID, err := newOpaqueID("pin")
	if err != nil {
		return err
	}
	var res struct {
		Pinned bool   `json:"pinned"`
		Error  string `json:"error"`
	}
	status, err := r.callArtifactMutation(context.Background(), opPastePin, slug,
		versionRequest{OpID: opID, Generation: generation, Ver: ver}, &res)
	if err != nil {
		return err
	}
	if status == http.StatusLocked {
		return domain.ErrBusy
	}
	if replaced(status, res.Error) {
		return domain.ErrNotFound
	}
	if status == http.StatusConflict {
		return fmt.Errorf("worker: pin accounting conflict")
	}
	if !res.Pinned {
		return domain.ErrNotFound
	}
	return nil
}

// OwnerSummary is the `whoami` view. One point read of the identity cell, which
// already holds every entry it charges for, plus the first-seen stamp.
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
	var charge struct {
		RoomBytes int64 `json:"roomBytes"`
	}
	if err := r.ask(context.Background(), "identity bytes", opIdentityBytes, owner, nil, &charge, nil); err != nil {
		return domain.OwnerSummary{}, err
	}
	return domain.OwnerSummary{Active: active, FirstSeen: first, PasteBytes: bytes, RoomBytes: charge.RoomBytes}, nil
}

// --- the site surface -------------------------------------------------------
//
// A directory IS a paste whose version carries N manifest entries, so these
// three methods are all storage.Sites needs to run on cells: no site class, no
// second quota, no second enumeration index. The slug namespace stays global
// because the paste cell IS the slug.

// AppendManifestVersion appends a retained file-set version under the directory
// shape this deploy decided. A base above zero names the version it was
// computed against and may share that version's objects.
func (r *PasteRepo) AppendManifestVersion(ctx context.Context, slug domain.Slug, generation string,
	kind domain.ContentKind, uploadID string, m domain.Manifest, size int, userCap int64, now time.Time, base int,
) (domain.AppendResult, error) {
	return r.appendArtifact(ctx, slug, generation, kind, uploadID, m, size, userCap, now, base)
}

// urlQuery escapes a value for a query string. Named rather than inlined so
// every call site escapes, which is what keeps a key containing & or = from
// silently becoming two parameters.
func urlQuery(v string) string { return url.QueryEscape(v) }

// replaced reports the cell's refusal of a call addressed to an incarnation
// the slug no longer holds: the paste the caller named is gone.
func replaced(status int, code string) bool {
	return status == http.StatusConflict && code == codeGenerationMismatch
}
