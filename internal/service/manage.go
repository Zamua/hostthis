package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/mime"
)

// PasteAdmin is the persistence interface for everything except "create a new
// paste". internal/storage.PasteRepo satisfies it.
type PasteAdmin interface {
	Get(domain.Slug) (domain.Paste, error)
	ListByOwner(owner string) ([]domain.Paste, error)
	Delete(domain.Slug) error
	SetName(domain.Slug, string) error
	SetPinnedVersion(domain.Slug, domain.Version) error
	Unpin(domain.Slug) error
	AppendVersionWithQuotaCheck(ctx context.Context, slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time) (domain.AppendResult, error)
	ListVersions(domain.Slug) ([]domain.Version, error)
	GetVersion(domain.Slug, int) (domain.Version, error)
	DeleteVersion(domain.Slug, int) error
	CountByOwner(owner string) (int, error)
	SumActiveBytesByOwner(owner string, now time.Time) (int, error)
	OwnerFirstSeen(owner string) (time.Time, error)
}

// ErrNotOwner is returned by an owner-gated operation when the requesting
// identity doesn't match the paste's. The SSH/HTTP surfaces map it to 403.
var ErrNotOwner = errors.New("service: not the paste owner")

// ErrNotFound is returned when a paste / version doesn't exist. An owner-gated
// read of a slug owned by someone else maps here too, so existence never leaks
// to outsiders.
var ErrNotFound = errors.New("service: not found")

// ErrEmptyOwner is returned when an operation requires an identified owner and
// the caller is anonymous.
var ErrEmptyOwner = errors.New("service: anonymous - add an ssh key for this command")

// ErrInvalidName is returned by Rename when the name violates the length /
// unicode rules.
var ErrInvalidName = errors.New("service: name must be 1–60 printable Unicode chars, no newlines")

// Manage is the verb-level service. Each method maps to one ssh verb (or HTTP
// endpoint) and is owner-gated.
type Manage struct {
	// Sniff classifies bytes for DetectKind's text-only rule. A PORT
	// (domain.MIMESniffer).
	Sniff     domain.MIMESniffer
	Repo      PasteAdmin
	Blob      BlobUnit       // Show (ReadAll), Update (Stage+Commit), Delete (UnbindOnDelete)
	KeyGate   *KeyGate       // optional; populates WhoamiInfo.Session when set
	SiteBytes SiteByteSummer // optional; when set, Whoami's used_bytes includes static-site bytes
	Now       func() time.Time
}

// SiteByteSummer returns an identity's active static-site bytes. Whoami adds
// them to the paste-byte sum so used_bytes reflects the SAME paste+site total
// the quota check enforces. Nil on a metadata backend with no site repo, in
// which case Whoami reports paste bytes only.
type SiteByteSummer interface {
	SumActiveBytesByOwner(owner string, now time.Time) (int64, error)
}

func NewManage(repo PasteAdmin, blob BlobUnit) *Manage {
	return &Manage{Repo: repo, Blob: blob, Now: time.Now, Sniff: mime.Detect}
}

// requireOwner returns the paste if owner matches, else the appropriate
// sentinel. Only keyed identities (which carry the "key:" prefix) can manage
// pastes; anonymous and empty owners are rejected.
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

// UpdateResult tells the SSH layer whether the paste was pinned at update
// time, in which case the new version was saved but is not being served.
type UpdateResult struct {
	Paste     domain.Paste
	NewVer    int
	WasPinned bool
	PinnedAt  int // ver_num of the still-served version if WasPinned
}

// Update appends a new version to an existing slug and resets the retention
// expiry. On an UNPINNED paste (the default) the new version also becomes the
// served one; on a PINNED paste the pin holds and the new version is recorded
// but not served.
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
	kind, err := domain.DetectKind(staged.Prefix, typeHint, m.Sniff)
	if err != nil {
		return UpdateResult{}, err
	}
	// KindSite (a gzip-tar archive) is not a paste: accepting it here would
	// skip the deploy pipeline's safe-untar guards.
	if kind == domain.KindSite {
		return UpdateResult{}, domain.ErrUnsupportedKind
	}
	now := m.Now().UTC()
	ctx := context.Background()
	// Commit binds the staged blob with the AppendVersion metadata write. On
	// the standalone path Stage writes the bytes and Commit runs only the
	// metadata write, keeping the blob-first ordering.
	handle, err := m.Blob.Stage(ctx, string(slug), staged.SHA, staged.Body)
	if err != nil {
		// A blob Put rejected by the object store's bucket quota surfaces
		// storage.ErrServiceFull (the durable total-bytes ceiling), which the
		// shared classifier turns into the graceful "at capacity" response.
		if class, terr := classifyCommitErr(err); class != commitOther {
			return UpdateResult{}, terr
		}
		return UpdateResult{}, fmt.Errorf("blob write: %w", err)
	}
	var res domain.AppendResult
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
	// Unbind the paste's blob references. A no-op on the standalone path,
	// where the global sweep reclaims unreferenced content-addressed blobs;
	// naming the delete-side lifecycle keeps the call uniform across backends.
	// The error is swallowed on purpose: the metadata is already gone and the
	// sweep is the backstop.
	_ = m.Blob.UnbindOnDelete(context.Background(), string(slug), []string{p.ContentSHA})
	return nil
}

// Versions returns the slug's full history (newest first), including
// tombstoned rows, which the `versions` verb renders with a `deleted` marker.
func (m *Manage) Versions(slug domain.Slug, owner string) ([]domain.Version, error) {
	if _, err := m.requireOwner(slug, owner); err != nil {
		return nil, err
	}
	return m.Repo.ListVersions(slug)
}

// DeleteVersionResult reports what DeleteVersion did so the SSH layer can
// format messaging like "deleted v2. freed 187.3k.".
type DeleteVersionResult struct {
	VerNum     int
	FreedBytes int
}

// ErrVersionAlreadyDeleted is returned when the target version is already a
// tombstone. The caller chooses soft success or hard error.
var ErrVersionAlreadyDeleted = errors.New("service: version already deleted")

// ErrVersionCurrentlyServed is returned when the target version is the one the
// URL serves. Freeing it requires `pin` to a different version, or `unpin`.
var ErrVersionCurrentlyServed = errors.New("service: version is currently served by the URL; pin a different version first")

// DeleteVersion frees a single version's blob bytes (tombstones the row).
// Refused when:
//   - paste doesn't exist or owner doesn't match → ErrNotFound
//   - target version doesn't exist → ErrNotFound
//   - target is already tombstoned → ErrVersionAlreadyDeleted
//   - target is the version the URL currently serves → ErrVersionCurrentlyServed
//
// The freed byte count is the row's pre-deletion size column.
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
	// No cache purge: the served bytes did not change, only an older
	// version's bytes that no URL surface exposed.
	return DeleteVersionResult{VerNum: verNum, FreedBytes: target.Size}, nil
}

// servedVersion returns the ver_num the URL currently serves for slug.
// Mirrors the read path: pinned wins, else MAX(non-deleted ver_num).
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
	return 0, nil // no live versions: unreachable for an active paste
}

// Pin sets which version_num the public URL serves and makes it sticky, so
// later `update`s do not bump it. Does NOT reset the expiry clock; only Update
// does that.
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

// Unpin clears a sticky pin, reverting the URL to "always serve the latest
// version".
func (m *Manage) Unpin(slug domain.Slug, owner string) error {
	if _, err := m.requireOwner(slug, owner); err != nil {
		return err
	}
	if err := m.Repo.Unpin(slug); err != nil {
		return err
	}
	return nil
}

// WhoamiInfo is the per-owner summary the `whoami` verb renders.
type WhoamiInfo struct {
	Identity   string
	Active     int
	FirstSeen  time.Time
	UsedBytes  int         // compressed bytes summed across active non-deleted versions
	QuotaBytes int         // domain.UserQuotaBytes, surfaced so the SSH formatter need not import domain
	Session    SessionInfo // per-session keygate state; zero value if KeyGate isn't wired
}

// Whoami populates WhoamiInfo for an owner (key fingerprint). An empty subnet,
// or a nil KeyGate, leaves the session fields zero.
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
	// quota check enforces; without this it under-reports and disagrees with
	// the write-check that rejects at the combined cap.
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
		// Best-effort: a keygate error leaves the session fields zero rather
		// than failing whoami.
		if s, err := m.KeyGate.Inspect(owner, subnet); err == nil {
			info.Session = s
		}
	}
	return info, nil
}

// validName holds the spec's rule: 1-60 printable Unicode chars, no newlines.
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
