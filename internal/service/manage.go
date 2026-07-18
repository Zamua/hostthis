package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// PasteAdmin is the persistence interface for everything except
// "create a new paste" - list, get versions, mutate flags, delete.
// internal/storage.PasteRepo satisfies it.
type PasteAdmin interface {
	Get(domain.Slug) (domain.Paste, error)
	ListByOwner(owner string) ([]domain.Paste, error)
	Delete(domain.Slug) error
	SetName(domain.Slug, string) error
	SetPinnedVersion(domain.Slug, domain.Version) error
	Unpin(domain.Slug) error
	AppendVersionWithQuotaCheck(ctx context.Context, slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time) (storage.AppendResult, error)
	ListVersions(domain.Slug) ([]domain.Version, error)
	GetVersion(domain.Slug, int) (domain.Version, error)
	DeleteVersion(domain.Slug, int) error
	CountByOwner(owner string) (int, error)
	SumActiveBytesByOwner(owner string, now time.Time) (int, error)
	OwnerFirstSeen(owner string) (time.Time, error)
}

// ErrNotOwner is returned by any owner-gated operation when the
// requesting identity doesn't match the paste's identity. The
// SSH/HTTP surfaces map this to 403.
var ErrNotOwner = errors.New("service: not the paste owner")

// ErrNotFound is returned when a paste / version doesn't exist.
// Owner-gated reads of a slug that exists but belongs to someone
// else also map to ErrNotFound (don't leak existence to outsiders).
var ErrNotFound = errors.New("service: not found")

// ErrEmptyOwner is returned when an operation requires an identified
// owner (list, delete, rename, …) and the caller is anonymous.
var ErrEmptyOwner = errors.New("service: anonymous - add an ssh key for this command")

// ErrInvalidName is returned by Rename when the name violates the
// length / unicode rules.
var ErrInvalidName = errors.New("service: name must be 1–60 printable Unicode chars, no newlines")

// Manage is the verb-level service. Each method maps to one ssh verb
// (or HTTP endpoint) and is owner-gated.
type Manage struct {
	Repo      PasteAdmin
	Blob      BlobUnit       // for Show (ReadAll) + Update (Stage+Commit) + Delete (UnbindOnDelete)
	KeyGate   *KeyGate       // optional; populates WhoamiInfo.Session when set
	SiteBytes SiteByteSummer // optional; when set, Whoami's used_bytes includes static-site bytes
	Now       func() time.Time
}

// SiteByteSummer returns an identity's active static-site bytes. Whoami adds
// this to the paste-byte sum so used_bytes reflects the SAME paste+site total
// the deploy/upload quota check enforces (the per-identity cap sums both
// counters). Optional: a metadata backend with no site repo leaves it nil and
// Whoami reports paste bytes only. The site repo satisfies it directly.
type SiteByteSummer interface {
	SumActiveBytesByOwner(owner string, now time.Time) (int64, error)
}

func NewManage(repo PasteAdmin, blob BlobUnit) *Manage {
	return &Manage{Repo: repo, Blob: blob, Now: time.Now}
}

// requireOwner returns the paste if owner matches; otherwise the
// appropriate sentinel. Anonymous identities (no key offered) and
// empty owners are rejected - only keyed identities (which carry
// the "key:" prefix) can manage their pastes.
func (m *Manage) requireOwner(slug domain.Slug, owner string) (domain.Paste, error) {
	if !domain.Identity(owner).IsKeyed() {
		return domain.Paste{}, ErrEmptyOwner
	}
	p, err := m.Repo.Get(slug)
	if err != nil {
		return domain.Paste{}, ErrNotFound
	}
	if p.Identity.String() != owner {
		// Don't leak existence; surface as ErrNotFound at the boundary.
		return domain.Paste{}, ErrNotFound
	}
	return p, nil
}

// List returns the owner's active pastes, soonest-to-expire first.
func (m *Manage) List(owner string) ([]domain.Paste, error) {
	if !domain.Identity(owner).IsKeyed() {
		return nil, ErrEmptyOwner
	}
	return m.Repo.ListByOwner(owner)
}

// Show returns the bytes + paste metadata for owner-controlled read.
func (m *Manage) Show(slug domain.Slug, owner string) (domain.Paste, []byte, error) {
	p, err := m.requireOwner(slug, owner)
	if err != nil {
		return domain.Paste{}, nil, err
	}
	body, err := m.Blob.ReadAll(context.Background(), string(slug), p.ContentSHA)
	if err != nil {
		return domain.Paste{}, nil, fmt.Errorf("blob: %w", err)
	}
	return p, body, nil
}

// UpdateResult is returned from Update so the SSH layer can surface
// the right messaging - in particular, whether the paste was pinned
// before the update (in which case the new version was saved but
// isn't being served).
type UpdateResult struct {
	Paste     domain.Paste
	NewVer    int
	WasPinned bool
	PinnedAt  int // ver_num of the still-served version if WasPinned
}

// Update appends a new version to an existing slug and resets the
// retention expiry. If the paste was UNPINNED (default), the new version
// also becomes the served version. If it was PINNED to a specific
// version, the pin holds and the new version is recorded but not
// served - the SSH layer prints a note pointing at `unpin` or
// `pin <new ver>`.
func (m *Manage) Update(slug domain.Slug, owner string, body io.Reader, typeHint string) (UpdateResult, error) {
	staged, err := streamUpload(body)
	switch {
	case errors.Is(err, errRawCapExceeded):
		return UpdateResult{}, ErrRawTooLarge
	case errors.Is(err, errCompressedCapExceeded):
		return UpdateResult{}, ErrCompressedTooLarge
	case err != nil:
		return UpdateResult{}, fmt.Errorf("staging: %w", err)
	}
	if staged.RawSize == 0 {
		return UpdateResult{}, errors.New("empty upload")
	}
	existing, err := m.requireOwner(slug, owner)
	if err != nil {
		return UpdateResult{}, err
	}
	kind, err := domain.DetectKind(staged.Prefix, typeHint)
	if err != nil {
		return UpdateResult{}, err
	}
	// KindSite (a gzip-tar archive) is not a paste; updating a paste with
	// archive content must not skip the deploy pipeline's safe-untar guards.
	if kind == domain.KindSite {
		return UpdateResult{}, domain.ErrUnsupportedKind
	}
	now := m.Now().UTC()
	ctx := context.Background()
	// Stage the version's bytes, then Commit binds the staged blob with the
	// AppendVersion metadata write. On the standalone path Stage writes the
	// bytes and Commit just runs the metadata write - the same blob-first
	// ordering this path always used.
	handle, err := m.Blob.Stage(ctx, string(slug), staged.SHA, staged.Body)
	if err != nil {
		// A blob Put rejected by the object store's bucket quota surfaces
		// storage.ErrServiceFull (the durable total-bytes ceiling); the
		// shared classifier translates it into the graceful "service is at
		// capacity" response.
		if class, terr := classifyCommitErr(err); class != commitOther {
			return UpdateResult{}, terr
		}
		return UpdateResult{}, fmt.Errorf("blob write: %w", err)
	}
	var res storage.AppendResult
	err = m.Blob.Commit(ctx, []BlobHandle{handle}, func(ctx context.Context) error {
		var aerr error
		res, aerr = m.Repo.AppendVersionWithQuotaCheck(ctx, slug, kind, staged.SHA, staged.CompressedSize, int64(domain.UserQuotaBytes), now)
		return aerr
	})
	if err != nil {
		_, terr := classifyCommitErr(err)
		return UpdateResult{}, terr
	}
	p, err := m.Repo.Get(slug) // re-read so caller sees updated UpdatedAt + ExpiresAt
	if err != nil {
		return UpdateResult{}, err
	}
	return UpdateResult{
		Paste:     p,
		NewVer:    res.NewVer,
		WasPinned: res.WasPinned,
		PinnedAt:  existing.PinnedVersion,
	}, nil
}

// Rename sets the human label. Empty string clears it.
func (m *Manage) Rename(slug domain.Slug, owner, name string) error {
	if _, err := m.requireOwner(slug, owner); err != nil {
		return err
	}
	if name != "" {
		if !validName(name) {
			return ErrInvalidName
		}
	}
	return m.Repo.SetName(slug, name)
}

// Delete removes a paste and its versions (FK cascade).
func (m *Manage) Delete(slug domain.Slug, owner string) error {
	p, err := m.requireOwner(slug, owner)
	if err != nil {
		return err
	}
	if err := m.Repo.Delete(slug); err != nil {
		return err
	}
	// Unbind the paste's blob references. On the standalone path this is a
	// no-op (the global sweep reclaims unreferenced content-addressed blobs);
	// it names the delete-side lifecycle so the call is uniform across
	// backends. A failure here is logged-by-impl, not fatal - the metadata is
	// already gone and the sweep is the backstop - so the error is swallowed
	// like the cache purge below.
	_ = m.Blob.UnbindOnDelete(context.Background(), string(slug), []string{p.ContentSHA})
	return nil
}

// Versions returns the slug's full history (newest first). Includes
// tombstoned (deleted) rows - the `versions` verb renders them with
// a `deleted` marker.
func (m *Manage) Versions(slug domain.Slug, owner string) ([]domain.Version, error) {
	if _, err := m.requireOwner(slug, owner); err != nil {
		return nil, err
	}
	return m.Repo.ListVersions(slug)
}

// DeleteVersionResult reports what DeleteVersion did so the SSH
// layer can format messaging like "deleted v2. freed 187.3k.".
type DeleteVersionResult struct {
	VerNum     int
	FreedBytes int
}

// ErrVersionAlreadyDeleted is returned when the target version is
// already a tombstone. Caller chooses whether to treat this as a
// soft success or hard error.
var ErrVersionAlreadyDeleted = errors.New("service: version already deleted")

// ErrVersionCurrentlyServed is returned when the target version is
// the one the URL currently serves. Caller should hint at `pin`
// (to a different version) or `unpin` as the way to free it.
var ErrVersionCurrentlyServed = errors.New("service: version is currently served by the URL; pin a different version first")

// DeleteVersion frees a single version's blob bytes (tombstones the
// row). Refused when:
//   - paste doesn't exist or owner doesn't match → ErrNotFound
//   - target version doesn't exist → ErrNotFound
//   - target is already tombstoned → ErrVersionAlreadyDeleted
//   - target is the version the URL currently serves → ErrVersionCurrentlyServed
//
// On success, returns the version number and the freed byte count
// (the row's pre-deletion size column).
func (m *Manage) DeleteVersion(slug domain.Slug, owner string, verNum int) (DeleteVersionResult, error) {
	p, err := m.requireOwner(slug, owner)
	if err != nil {
		return DeleteVersionResult{}, err
	}
	if verNum < 1 {
		return DeleteVersionResult{}, fmt.Errorf("version must be >= 1")
	}
	target, err := m.Repo.GetVersion(slug, verNum)
	if err != nil {
		return DeleteVersionResult{}, ErrNotFound
	}
	if target.Deleted {
		return DeleteVersionResult{VerNum: verNum, FreedBytes: 0}, ErrVersionAlreadyDeleted
	}

	servedVer, err := m.servedVersion(slug, p.PinnedVersion)
	if err != nil {
		return DeleteVersionResult{}, err
	}
	if servedVer == verNum {
		return DeleteVersionResult{}, ErrVersionCurrentlyServed
	}

	if err := m.Repo.DeleteVersion(slug, verNum); err != nil {
		return DeleteVersionResult{}, err
	}
	// No cache purge - the served bytes didn't change; we just freed
	// an older version's bytes that no URL surface was exposing.
	return DeleteVersionResult{VerNum: verNum, FreedBytes: target.Size}, nil
}

// servedVersion returns the ver_num of the version the URL is
// currently serving for slug. Mirrors the read path: pinned wins,
// else MAX(non-deleted ver_num).
func (m *Manage) servedVersion(slug domain.Slug, pinnedVersion int) (int, error) {
	if pinnedVersion > 0 {
		return pinnedVersion, nil
	}
	versions, err := m.Repo.ListVersions(slug)
	if err != nil {
		return 0, err
	}
	for _, v := range versions {
		if !v.Deleted {
			return v.VerNum, nil
		}
	}
	return 0, nil // no live versions - shouldn't happen for an active paste
}

// Pin sets which version_num the public URL serves and makes it
// sticky - subsequent `update`s won't bump the pin. Does NOT reset
// the expiry clock; only Update does that.
func (m *Manage) Pin(slug domain.Slug, owner string, verNum int) (domain.Version, error) {
	if _, err := m.requireOwner(slug, owner); err != nil {
		return domain.Version{}, err
	}
	if verNum < 1 {
		return domain.Version{}, fmt.Errorf("version must be >= 1; use `unpin` to clear")
	}
	ver, err := m.Repo.GetVersion(slug, verNum)
	if err != nil {
		return domain.Version{}, ErrNotFound
	}
	if err := m.Repo.SetPinnedVersion(slug, ver); err != nil {
		return domain.Version{}, err
	}
	return ver, nil
}

// Unpin clears a sticky pin and reverts the URL to "always serve the
// latest version." On future updates the new version is published
// immediately.
func (m *Manage) Unpin(slug domain.Slug, owner string) error {
	if _, err := m.requireOwner(slug, owner); err != nil {
		return err
	}
	if err := m.Repo.Unpin(slug); err != nil {
		return err
	}
	return nil
}

// Whoami returns the per-owner summary used by the `whoami` verb.
type WhoamiInfo struct {
	Identity   string
	Active     int
	FirstSeen  time.Time
	UsedBytes  int         // compressed bytes summed across active non-deleted versions
	QuotaBytes int         // domain.UserQuotaBytes - surfaced so SSH formatter doesn't import domain
	Session    SessionInfo // per-session keygate state; zero value if KeyGate isn't wired
}

// Whoami populates WhoamiInfo for an owner (key fingerprint). subnet
// may be empty when the caller doesn't have one (tests etc.); session
// fields are then zero. KeyGate may be nil - same effect.
func (m *Manage) Whoami(owner, subnet string) (WhoamiInfo, error) {
	if !domain.Identity(owner).IsKeyed() {
		return WhoamiInfo{}, ErrEmptyOwner
	}
	active, err := m.Repo.CountByOwner(owner)
	if err != nil {
		return WhoamiInfo{}, err
	}
	first, err := m.Repo.OwnerFirstSeen(owner)
	if err != nil {
		return WhoamiInfo{}, err
	}
	used, err := m.Repo.SumActiveBytesByOwner(owner, m.Now().UTC())
	if err != nil {
		return WhoamiInfo{}, err
	}
	// Include static-site bytes so used_bytes matches the paste+site total the
	// quota check enforces; without this, used_bytes under-reports (pastes
	// only) and disagrees with the write-check that rejects at the combined
	// cap. Nil when the backend has no site repo (used_bytes = pastes only).
	if m.SiteBytes != nil {
		siteUsed, err := m.SiteBytes.SumActiveBytesByOwner(owner, m.Now().UTC())
		if err != nil {
			return WhoamiInfo{}, err
		}
		used += int(siteUsed)
	}
	info := WhoamiInfo{
		Identity:   owner,
		Active:     active,
		FirstSeen:  first,
		UsedBytes:  used,
		QuotaBytes: domain.UserQuotaBytes,
	}
	if m.KeyGate != nil && subnet != "" {
		// Best-effort: a keygate error doesn't fail whoami; session
		// fields just stay zero. The user still sees their pastes.
		if s, err := m.KeyGate.Inspect(owner, subnet); err == nil {
			info.Session = s
		}
	}
	return info, nil
}

// validName: per spec, 1–60 printable Unicode chars, no newlines.
func validName(s string) bool {
	if utf8.RuneCountInString(s) == 0 || utf8.RuneCountInString(s) > 60 {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\r' {
			return false
		}
	}
	return true
}
