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
type PasteRepo struct {
	db *sql.DB
}

func NewPasteRepo(db *sql.DB) *PasteRepo { return &PasteRepo{db: db} }

// Insert writes a new paste row plus its v1 version row in one
// transaction. Returns ErrSlugTaken if the slug is already in use —
// the caller retries with a fresh random slug.
func (r *PasteRepo) Insert(p domain.Paste) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op when commit succeeds
	_, err = tx.Exec(`
		INSERT INTO pastes (slug, identity, kind, content_sha, size, name,
		                    pinned_version,
		                    created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.Slug.String(), p.Identity.String(), string(p.Kind), p.ContentSHA, p.Size, p.Name,
		p.PinnedVersion,
		formatTime(p.CreatedAt), formatTime(p.UpdatedAt), formatTime(p.ExpiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrSlugTaken
		}
		return fmt.Errorf("insert paste %q: %w", p.Slug, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at)
		VALUES (?, 1, ?, ?, ?, ?)
	`, p.Slug.String(), string(p.Kind), p.ContentSHA, p.Size, formatTime(p.CreatedAt)); err != nil {
		return fmt.Errorf("insert v1 for %q: %w", p.Slug, err)
	}
	return tx.Commit()
}

// Get returns the paste for slug, or ErrNotFound. Expired pastes are
// still returned by this read — the HTTP layer is responsible for
// 404'ing them; the background sweep is responsible for deleting
// them.
func (r *PasteRepo) Get(slug domain.Slug) (domain.Paste, error) {
	row := r.db.QueryRow(`
		SELECT slug, identity, kind, content_sha, size, name,
		       pinned_version,
		       created_at, updated_at, expires_at
		FROM pastes WHERE slug = ?
	`, slug.String())
	return scanPaste(row)
}

// ListByOwner returns all of an owner's active pastes, ordered by
// expires_at ascending (soonest to die first — matches what `list`
// should show). Empty identity returns no rows.
func (r *PasteRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	if owner == "" {
		return nil, nil
	}
	rows, err := r.db.Query(`
		SELECT slug, identity, kind, content_sha, size, name,
		       pinned_version,
		       created_at, updated_at, expires_at
		FROM pastes WHERE identity = ?
		ORDER BY expires_at ASC
	`, owner)
	if err != nil {
		return nil, fmt.Errorf("list by owner: %w", err)
	}
	defer rows.Close()
	var out []domain.Paste
	for rows.Next() {
		p, err := scanPaste(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete removes a paste and (via FK cascade) all its versions.
// Caller is responsible for owner check.
func (r *PasteRepo) Delete(slug domain.Slug) error {
	_, err := r.db.Exec(`DELETE FROM pastes WHERE slug = ?`, slug.String())
	if err != nil {
		return fmt.Errorf("delete %q: %w", slug, err)
	}
	return nil
}

// SetName updates the human label. Passing "" clears it.
func (r *PasteRepo) SetName(slug domain.Slug, name string) error {
	_, err := r.db.Exec(`UPDATE pastes SET name = ? WHERE slug = ?`, name, slug.String())
	if err != nil {
		return fmt.Errorf("set name: %w", err)
	}
	return nil
}

// SetPinnedVersion changes which version_num the public URL serves,
// and rolls the pastes row's content_sha + size + kind to match.
// Caller verified ver exists.
func (r *PasteRepo) SetPinnedVersion(slug domain.Slug, ver domain.Version) error {
	_, err := r.db.Exec(`
		UPDATE pastes
		SET pinned_version = ?, content_sha = ?, size = ?, kind = ?
		WHERE slug = ?
	`, ver.VerNum, ver.ContentSHA, ver.Size, string(ver.Kind), slug.String())
	if err != nil {
		return fmt.Errorf("set pinned version: %w", err)
	}
	return nil
}

// AppendVersion writes a new version row for an existing paste, sets
// it as the pinned version, and bumps updated_at + expires_at to
// reset the 24h retention clock. Returns the new ver_num.
func (r *PasteRepo) AppendVersion(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, now time.Time) (int, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	var maxVer int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(ver_num), 0) FROM versions WHERE slug = ?`, slug.String()).Scan(&maxVer); err != nil {
		return 0, fmt.Errorf("max ver: %w", err)
	}
	newVer := maxVer + 1
	expires := now.Add(domain.RetentionWindow)
	if _, err := tx.Exec(`
		INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, slug.String(), newVer, string(kind), contentSHA, size, formatTime(now)); err != nil {
		return 0, fmt.Errorf("insert version: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE pastes
		SET kind = ?, content_sha = ?, size = ?,
		    pinned_version = ?, updated_at = ?, expires_at = ?
		WHERE slug = ?
	`, string(kind), contentSHA, size, newVer, formatTime(now), formatTime(expires), slug.String()); err != nil {
		return 0, fmt.Errorf("update paste head: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newVer, nil
}

// ListVersions returns the version history for slug, newest first.
func (r *PasteRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	rows, err := r.db.Query(`
		SELECT slug, ver_num, kind, content_sha, size, created_at
		FROM versions WHERE slug = ?
		ORDER BY ver_num DESC
	`, slug.String())
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()
	var out []domain.Version
	for rows.Next() {
		var v domain.Version
		var slugStr, kind, created string
		if err := rows.Scan(&slugStr, &v.VerNum, &kind, &v.ContentSHA, &v.Size, &created); err != nil {
			return nil, err
		}
		v.Slug = domain.Slug(slugStr)
		v.Kind = domain.ContentKind(kind)
		v.CreatedAt = parseTime(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion returns a single version row, or ErrNotFound.
func (r *PasteRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	row := r.db.QueryRow(`
		SELECT slug, ver_num, kind, content_sha, size, created_at
		FROM versions WHERE slug = ? AND ver_num = ?
	`, slug.String(), ver)
	var v domain.Version
	var slugStr, kind, created string
	if err := row.Scan(&slugStr, &v.VerNum, &kind, &v.ContentSHA, &v.Size, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Version{}, ErrNotFound
		}
		return domain.Version{}, err
	}
	v.Slug = domain.Slug(slugStr)
	v.Kind = domain.ContentKind(kind)
	v.CreatedAt = parseTime(created)
	return v, nil
}

// CountByOwner returns how many active pastes the owner has — used
// by whoami output.
func (r *PasteRepo) CountByOwner(owner string) (int, error) {
	if owner == "" {
		return 0, nil
	}
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM pastes WHERE identity = ?`, owner).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count by owner: %w", err)
	}
	return n, nil
}

// SumActiveSizeByOwner returns the total bytes the identity currently
// has alive — the sum of every VERSION row attached to any of its
// non-expired pastes. We sum versions, not pastes, so that update
// history is properly accounted: each call to `update` adds a new
// version row that persists on disk until the parent paste expires
// or is deleted.
//
// Delete frees this completely: the FK cascade drops the parent
// paste row AND all its version rows, so the next call to this
// function returns the lower total.
func (r *PasteRepo) SumActiveSizeByOwner(owner string, now time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	var n sql.NullInt64
	err := r.db.QueryRow(`
		SELECT COALESCE(SUM(v.size), 0)
		FROM versions v
		JOIN pastes p ON p.slug = v.slug
		WHERE p.identity = ? AND p.expires_at > ?
	`, owner, formatTime(now)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sum active size: %w", err)
	}
	return n.Int64, nil
}

// OwnerFirstSeen returns the earliest CreatedAt across the owner's
// pastes — a proxy for "when did we first see this key". Zero
// time.Time when the owner has no pastes.
func (r *PasteRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	if owner == "" {
		return time.Time{}, nil
	}
	var s sql.NullString
	err := r.db.QueryRow(`SELECT MIN(created_at) FROM pastes WHERE identity = ?`, owner).Scan(&s)
	if err != nil {
		return time.Time{}, fmt.Errorf("owner first seen: %w", err)
	}
	if !s.Valid {
		return time.Time{}, nil
	}
	return parseTime(s.String), nil
}

// scanPaste is shared by Get + ListByOwner — same column order, one
// place to update if columns shift.
func scanPaste(s scanner) (domain.Paste, error) {
	var p domain.Paste
	var slugStr, identStr, kind, created, updated, expires string
	if err := s.Scan(&slugStr, &identStr, &kind, &p.ContentSHA, &p.Size, &p.Name,
		&p.PinnedVersion,
		&created, &updated, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Paste{}, ErrNotFound
		}
		return domain.Paste{}, fmt.Errorf("scan paste: %w", err)
	}
	p.Slug = domain.Slug(slugStr)
	p.Identity = domain.Identity(identStr)
	p.Kind = domain.ContentKind(kind)
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	p.ExpiresAt = parseTime(expires)
	return p, nil
}

// scanner is the minimal interface *sql.Row and *sql.Rows both satisfy
// so scanPaste can take either.
type scanner interface{ Scan(dest ...any) error }

// ErrSlugTaken is returned by Insert when the chosen slug already exists.
var ErrSlugTaken = errors.New("storage: slug already taken")

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
		return time.Time{}
	}
	return t
}
