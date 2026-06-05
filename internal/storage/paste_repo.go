package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// PasteRepo is the sqlite-backed implementation of paste persistence.
// Other packages depend on the *interface* declared elsewhere (the
// service layer); this concrete type is wired in cmd/hostthisd.
type PasteRepo struct {
	db *sql.DB
}

func NewPasteRepo(db *sql.DB) *PasteRepo { return &PasteRepo{db: db} }

// Insert writes a new paste row. Returns ErrSlugTaken if the slug
// is already in use — the caller retries with a fresh random slug.
func (r *PasteRepo) Insert(p domain.Paste) error {
	_, err := r.db.Exec(`
		INSERT INTO pastes (slug, owner_hash, kind, content_sha, size, name,
		                    created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.Slug.String(), p.OwnerHash, string(p.Kind), p.ContentSHA, p.Size, p.Name,
		formatTime(p.CreatedAt), formatTime(p.UpdatedAt), formatTime(p.ExpiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrSlugTaken
		}
		return fmt.Errorf("insert paste %q: %w", p.Slug, err)
	}
	return nil
}

// Get returns the paste for slug, or ErrNotFound. Expired pastes are
// still returned by this read — the HTTP layer is responsible for
// 404'ing them; the background sweep is responsible for deleting
// them. Keeping read-vs-delete separate lets the sweep run lazily
// without races against in-flight reads.
func (r *PasteRepo) Get(slug domain.Slug) (domain.Paste, error) {
	row := r.db.QueryRow(`
		SELECT slug, owner_hash, kind, content_sha, size, name,
		       created_at, updated_at, expires_at
		FROM pastes WHERE slug = ?
	`, slug.String())
	var p domain.Paste
	var slugStr, kind, created, updated, expires string
	if err := row.Scan(&slugStr, &p.OwnerHash, &kind, &p.ContentSHA, &p.Size, &p.Name,
		&created, &updated, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Paste{}, ErrNotFound
		}
		return domain.Paste{}, fmt.Errorf("get paste %q: %w", slug, err)
	}
	p.Slug = domain.Slug(slugStr)
	p.Kind = domain.ContentKind(kind)
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	p.ExpiresAt = parseTime(expires)
	return p, nil
}

// ErrSlugTaken is returned by Insert when the chosen slug already
// exists in the table. Service-layer retries with a fresh random
// slug.
var ErrSlugTaken = errors.New("storage: slug already taken")

// isUniqueViolation: modernc sqlite returns errors whose message
// contains "UNIQUE constraint failed" when a primary-key or unique
// index collides. Sniffing the string is the lowest-friction way to
// detect it portably; the alternative is type-asserting to the
// driver's *sqlite.Error which couples this layer to the driver.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Sqlite text format we write should always round-trip; a
		// parse error means schema drift. Return zero so the caller
		// fails loudly later rather than silently treating it as Now.
		return time.Time{}
	}
	return t
}
