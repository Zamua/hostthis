// Package service orchestrates use cases. It depends on domain and
// on small interfaces it declares for the infrastructure it needs.
// The concrete adapters live in internal/storage and are wired in
// cmd/hostthisd.
package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// PasteRepo is the persistence interface the upload service needs.
// internal/storage.PasteRepo satisfies it.
type PasteRepo interface {
	Insert(domain.Paste) error
	Get(domain.Slug) (domain.Paste, error)
}

// BlobStore writes and reads content-addressed bytes.
type BlobStore interface {
	Put(sha string, content []byte) error
	Get(sha string) ([]byte, error)
}

// SlugTakenErr signals that the chosen slug already exists. The
// upload service retries internally on Insert; this is exported so
// tests can assert on it if the retry budget is exhausted.
var SlugTakenErr = errors.New("service: slug taken (after retries)")

// Upload is the application service for new paste creation.
type Upload struct {
	Repo  PasteRepo
	Blobs BlobStore
	Now   func() time.Time // overridable for tests
}

// NewUpload wires defaults.
func NewUpload(repo PasteRepo, blobs BlobStore) *Upload {
	return &Upload{Repo: repo, Blobs: blobs, Now: time.Now}
}

// Result captures what Create produced for the caller (SSH / HTTP)
// to format into a response.
type Result struct {
	Paste domain.Paste
}

// CreateAnonymous accepts up to MaxPasteBytes from `body`, detects
// the content kind (HTML or Markdown), persists, and returns the
// resulting Paste. Anonymous = OwnerHash is "". Caller (SSH layer)
// passes its key fingerprint via Owner; the HTTP api layer passes
// from its token.
//
// Type detection uses domain.DetectKind; unsupported types return
// domain.ErrUnsupportedKind so the caller can surface the right
// message verbatim.
func (u *Upload) Create(body []byte, owner string, name string, typeHint string) (Result, error) {
	if len(body) == 0 {
		return Result{}, errors.New("empty upload")
	}
	if len(body) > domain.MaxPasteBytes {
		return Result{}, fmt.Errorf("upload exceeds %d-byte cap", domain.MaxPasteBytes)
	}
	kind, err := domain.DetectKind(body, typeHint)
	if err != nil {
		return Result{}, err
	}
	sha := domain.HashContent(body)
	if err := u.Blobs.Put(sha, body); err != nil {
		return Result{}, fmt.Errorf("blob write: %w", err)
	}
	now := u.Now().UTC()
	p := domain.Paste{
		OwnerHash:     owner,
		Kind:          kind,
		ContentSHA:    sha,
		Size:          len(body),
		Name:          name,
		Published:     true,
		PinnedVersion: 1,
		ShareSecret:   domain.NewShareSecret(),
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(domain.RetentionWindow),
	}
	// Retry on slug collision. SlugAlphabet has 32^8 ≈ 1.1e12 distinct
	// slugs; collisions inside 5 retries are vanishingly unlikely.
	const maxRetries = 5
	for range maxRetries {
		p.Slug = domain.NewRandomSlug()
		err := u.Repo.Insert(p)
		if err == nil {
			return Result{Paste: p}, nil
		}
		// Any sentinel that maps to "slug taken" → retry. Other errors
		// bubble up immediately.
		if isSlugTaken(err) {
			continue
		}
		return Result{}, err
	}
	return Result{}, SlugTakenErr
}

// isSlugTaken returns true if err is any flavor of "slug already
// exists in the repo." We sniff the error message to avoid the
// service layer importing the storage package directly — that would
// invert the dependency direction (service shouldn't know which
// concrete repo it's talking to).
func isSlugTaken(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "slug")
}
