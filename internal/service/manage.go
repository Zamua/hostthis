package service

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// PasteAdmin is the persistence interface for everything except
// "create a new paste" — list, get versions, mutate flags, delete.
// internal/storage.PasteRepo satisfies it.
type PasteAdmin interface {
	Get(domain.Slug) (domain.Paste, error)
	ListByOwner(owner string) ([]domain.Paste, error)
	Delete(domain.Slug) error
	SetName(domain.Slug, string) error
	SetPinnedVersion(domain.Slug, domain.Version) error
	Unpin(domain.Slug) error
	AppendVersionWithQuotaCheck(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, serviceCap, userCap int64, now time.Time) (storage.AppendResult, error)
	ListVersions(domain.Slug) ([]domain.Version, error)
	GetVersion(domain.Slug, int) (domain.Version, error)
	CountByOwner(owner string) (int, error)
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
var ErrEmptyOwner = errors.New("service: anonymous — add an ssh key for this command")

// ErrInvalidName is returned by Rename when the name violates the
// length / unicode rules.
var ErrInvalidName = errors.New("service: name must be 1–60 printable Unicode chars, no newlines")

// Manage is the verb-level service. Each method maps to one ssh verb
// (or HTTP endpoint) and is owner-gated.
type Manage struct {
	Repo            PasteAdmin
	Blobs           BlobStore // for Show + Update; same interface as Upload
	ServiceCapBytes int64     // 0 = no service-wide cap
	Now             func() time.Time
}

func NewManage(repo PasteAdmin, blobs BlobStore) *Manage {
	return &Manage{Repo: repo, Blobs: blobs, Now: time.Now}
}

// requireOwner returns the paste if owner matches; otherwise the
// appropriate sentinel. Anonymous identities (no key offered) and
// empty owners are rejected — only keyed identities (which carry
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
	body, err := m.Blobs.Get(p.ContentSHA)
	if err != nil {
		return domain.Paste{}, nil, fmt.Errorf("blob: %w", err)
	}
	return p, body, nil
}

// UpdateResult is returned from Update so the SSH layer can surface
// the right messaging — in particular, whether the paste was pinned
// before the update (in which case the new version was saved but
// isn't being served).
type UpdateResult struct {
	Paste     domain.Paste
	NewVer    int
	WasPinned bool
	PinnedAt  int // ver_num of the still-served version if WasPinned
}

// Update appends a new version to an existing slug and resets the
// 24h expiry. If the paste was UNPINNED (default), the new version
// also becomes the served version. If it was PINNED to a specific
// version, the pin holds and the new version is recorded but not
// served — the SSH layer prints a note pointing at `unpin` or
// `pin <new ver>`.
func (m *Manage) Update(slug domain.Slug, owner string, body []byte, typeHint string) (UpdateResult, error) {
	if len(body) == 0 {
		return UpdateResult{}, errors.New("empty upload")
	}
	if len(body) > domain.MaxPasteBytes {
		return UpdateResult{}, fmt.Errorf("upload exceeds %d-byte cap", domain.MaxPasteBytes)
	}
	existing, err := m.requireOwner(slug, owner)
	if err != nil {
		return UpdateResult{}, err
	}
	kind, err := domain.DetectKind(body, typeHint)
	if err != nil {
		return UpdateResult{}, err
	}
	now := m.Now().UTC()
	sha := domain.HashContent(body)
	if err := m.Blobs.Put(sha, body); err != nil {
		return UpdateResult{}, fmt.Errorf("blob write: %w", err)
	}
	res, err := m.Repo.AppendVersionWithQuotaCheck(slug, kind, sha, len(body), m.ServiceCapBytes, int64(domain.UserQuotaBytes), now)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrServiceFull):
			return UpdateResult{}, ErrServiceFull
		case errors.Is(err, storage.ErrOverUserQuota):
			return UpdateResult{}, ErrOverQuota
		default:
			return UpdateResult{}, err
		}
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
	if _, err := m.requireOwner(slug, owner); err != nil {
		return err
	}
	return m.Repo.Delete(slug)
}

// Versions returns the slug's full history (newest first).
func (m *Manage) Versions(slug domain.Slug, owner string) ([]domain.Version, error) {
	if _, err := m.requireOwner(slug, owner); err != nil {
		return nil, err
	}
	return m.Repo.ListVersions(slug)
}

// Pin sets which version_num the public URL serves and makes it
// sticky — subsequent `update`s won't bump the pin. Does NOT reset
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
	return m.Repo.Unpin(slug)
}

// Whoami returns the per-owner summary used by the `whoami` verb.
type WhoamiInfo struct {
	Identity  string
	Active    int
	FirstSeen time.Time
}

// Whoami populates WhoamiInfo for an owner (key fingerprint).
func (m *Manage) Whoami(owner string) (WhoamiInfo, error) {
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
	return WhoamiInfo{Identity: owner, Active: active, FirstSeen: first}, nil
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
