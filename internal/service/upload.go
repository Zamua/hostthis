// Package service orchestrates use cases. It depends on domain and
// on small interfaces it declares for the infrastructure it needs.
// The concrete adapters live in internal/storage and are wired in
// cmd/hostthisd.
package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// PasteRepo is the persistence interface the upload service needs.
// internal/storage.PasteRepo satisfies it.
type PasteRepo interface {
	InsertWithQuotaCheck(p domain.Paste, serviceCap, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Paste, error)
}

// BlobStore writes and reads content-addressed bytes. Put streams r
// to the backing store; size is the expected byte length (required by
// S3-shaped backends to set Content-Length, accepted by the disk impl
// for interface uniformity). Get returns the full bytes in memory —
// fine at our 1 MiB-per-paste cap.
type BlobStore interface {
	Put(sha string, r io.Reader, size int64) error
	Get(sha string) ([]byte, error)
}

// SlugTakenErr signals that the chosen slug already exists. The
// upload service retries internally on Insert; this is exported so
// tests can assert on it if the retry budget is exhausted.
var SlugTakenErr = errors.New("service: slug taken (after retries)")

// Upload is the application service for new paste creation.
type Upload struct {
	Repo            PasteRepo
	Blobs           BlobStore
	ServiceCapBytes int64 // 0 = no service-wide cap
	Now             func() time.Time
}

// NewUpload wires defaults. ServiceCap defaults to 0 (no cap); main
// flips it to the operator value.
func NewUpload(repo PasteRepo, blobs BlobStore) *Upload {
	return &Upload{Repo: repo, Blobs: blobs, Now: time.Now}
}

// Result captures what Create produced for the caller (SSH / HTTP)
// to format into a response.
type Result struct {
	Paste domain.Paste
}

// ErrOverQuota is returned when accepting the upload would push the
// identity's total active bytes above UserQuotaBytes.
var ErrOverQuota = errors.New("service: would exceed your 1 MiB total quota; delete a paste or wait for one to expire")

// ErrServiceFull is returned when the service-wide disk cap is hit.
var ErrServiceFull = errors.New("service: service is at capacity, try again after the next expiry")

// Create persists a new paste owned by the given identity.
// The identity is a "key:<fp>" string built from the uploader's ssh
// public key fingerprint. The identity gates quota — sum of the
// identity's active pastes plus this body cannot exceed UserQuotaBytes.
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
	now := u.Now().UTC()
	sha := domain.HashContent(body)
	if err := u.Blobs.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		return Result{}, fmt.Errorf("blob write: %w", err)
	}
	p := domain.Paste{
		Identity:      domain.Identity(owner),
		Kind:          kind,
		ContentSHA:    sha,
		Size:          len(body),
		Name:          name,
		PinnedVersion: 0, // unpinned by default — public URL follows the latest version
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(domain.RetentionWindow),
	}
	// Retry on slug collision. SlugAlphabet has 32^8 ≈ 1.1e12 distinct
	// slugs; collisions inside 5 retries are vanishingly unlikely.
	// The quota checks live inside InsertWithQuotaCheck so concurrent
	// uploads can't both pass and both insert.
	const maxRetries = 5
	for range maxRetries {
		p.Slug = domain.NewRandomSlug()
		err := u.Repo.InsertWithQuotaCheck(p, u.ServiceCapBytes, int64(domain.UserQuotaBytes), now)
		switch {
		case err == nil:
			return Result{Paste: p}, nil
		case errors.Is(err, storage.ErrServiceFull):
			return Result{}, ErrServiceFull
		case errors.Is(err, storage.ErrOverUserQuota):
			return Result{}, ErrOverQuota
		case isSlugTaken(err):
			continue
		default:
			return Result{}, err
		}
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
