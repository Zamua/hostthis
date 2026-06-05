package service

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Zamua/hostthis/internal/domain"
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
	AppendVersion(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, now time.Time) (int, error)
	ListVersions(domain.Slug) ([]domain.Version, error)
	GetVersion(domain.Slug, int) (domain.Version, error)
	CountByOwner(owner string) (int, error)
	OwnerFirstSeen(owner string) (time.Time, error)
}

// ErrNotOwner is returned by any owner-gated operation when the
// requesting OwnerHash doesn't match the paste's OwnerHash. The
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
	Repo  PasteAdmin
	Blobs BlobStore // for Show + Update; same interface as Upload
	Now   func() time.Time
}

func NewManage(repo PasteAdmin, blobs BlobStore) *Manage {
	return &Manage{Repo: repo, Blobs: blobs, Now: time.Now}
}

// requireOwner returns the paste if owner matches; otherwise the
// appropriate sentinel. Empty owner is treated as "anonymous" and
// rejected with ErrEmptyOwner — no anonymous management.
func (m *Manage) requireOwner(slug domain.Slug, owner string) (domain.Paste, error) {
	if owner == "" {
		return domain.Paste{}, ErrEmptyOwner
	}
	p, err := m.Repo.Get(slug)
	if err != nil {
		return domain.Paste{}, ErrNotFound
	}
	if p.OwnerHash != owner {
		// Don't leak existence; surface as ErrNotFound at the boundary.
		return domain.Paste{}, ErrNotFound
	}
	return p, nil
}

// List returns the owner's active pastes, soonest-to-expire first.
func (m *Manage) List(owner string) ([]domain.Paste, error) {
	if owner == "" {
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

// Update appends a new version to an existing slug, makes it the
// pinned version, and resets the 24h expiry. Owner-gated.
func (m *Manage) Update(slug domain.Slug, owner string, body []byte, typeHint string) (domain.Paste, int, error) {
	if len(body) == 0 {
		return domain.Paste{}, 0, errors.New("empty upload")
	}
	if len(body) > domain.MaxPasteBytes {
		return domain.Paste{}, 0, fmt.Errorf("upload exceeds %d-byte cap", domain.MaxPasteBytes)
	}
	if _, err := m.requireOwner(slug, owner); err != nil {
		return domain.Paste{}, 0, err
	}
	kind, err := domain.DetectKind(body, typeHint)
	if err != nil {
		return domain.Paste{}, 0, err
	}
	sha := domain.HashContent(body)
	if err := m.Blobs.Put(sha, body); err != nil {
		return domain.Paste{}, 0, fmt.Errorf("blob write: %w", err)
	}
	now := m.Now().UTC()
	ver, err := m.Repo.AppendVersion(slug, kind, sha, len(body), now)
	if err != nil {
		return domain.Paste{}, 0, err
	}
	p, err := m.Repo.Get(slug) // re-read so caller sees updated UpdatedAt + ExpiresAt
	if err != nil {
		return domain.Paste{}, 0, err
	}
	return p, ver, nil
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

// Pin sets which version_num the public URL serves. Does NOT reset
// the expiry clock — only Update does that.
func (m *Manage) Pin(slug domain.Slug, owner string, verNum int) (domain.Version, error) {
	if _, err := m.requireOwner(slug, owner); err != nil {
		return domain.Version{}, err
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

// Whoami returns the per-owner summary used by the `whoami` verb.
type WhoamiInfo struct {
	OwnerHash  string
	Active     int
	FirstSeen  time.Time
}

// Whoami populates WhoamiInfo for an owner (key fingerprint).
func (m *Manage) Whoami(owner string) (WhoamiInfo, error) {
	if owner == "" {
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
	return WhoamiInfo{OwnerHash: owner, Active: active, FirstSeen: first}, nil
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
