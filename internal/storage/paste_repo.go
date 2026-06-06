package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// ErrServiceFull is returned when accepting a write would push total
// active bytes (across all identities) past the operator-configured
// service-wide cap. Caller surfaces a "try again later" message.
var ErrServiceFull = errors.New("storage: service is at capacity")

// ErrOverUserQuota is returned when accepting a write would push an
// identity's active bytes past its per-user cap.
var ErrOverUserQuota = errors.New("storage: would exceed user quota")

// PasteRepo is the sqlite-backed implementation of paste persistence.
type PasteRepo struct {
	db *sql.DB
}

func NewPasteRepo(db *sql.DB) *PasteRepo { return &PasteRepo{db: db} }

// InsertWithQuotaCheck atomically (under BEGIN IMMEDIATE):
//  1. checks service-wide active bytes + p.Size against serviceCap (0 → no cap)
//  2. checks identity active bytes + p.Size against userCap
//  3. inserts the paste row + its v1 version row
//
// Concurrent calls serialize at the transaction boundary, so two
// uploads from the same identity can't both pass the user-cap check
// and both insert. Same for two uploads in different identities
// fighting over the last bytes of the service-wide cap.
//
// Returns:
//   - nil on success
//   - ErrSlugTaken if p.Slug is already in use (caller retries with a fresh slug)
//   - ErrServiceFull if accepting would exceed serviceCap
//   - ErrOverUserQuota if accepting would exceed userCap
func (r *PasteRepo) InsertWithQuotaCheck(p domain.Paste, serviceCap, userCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := formatTime(now)
	body := int64(p.Size)

	// 1. Service-wide check.
	if serviceCap > 0 {
		var total int64
		if err := tx.QueryRow(`
			SELECT COALESCE(SUM(v.size), 0)
			FROM versions v
			JOIN pastes pp ON pp.slug = v.slug
			WHERE pp.expires_at > ?
		`, nowStr).Scan(&total); err != nil {
			return fmt.Errorf("service-wide sum: %w", err)
		}
		if total+body > serviceCap {
			return ErrServiceFull
		}
	}
	// 2. Per-identity check.
	if userCap > 0 {
		var ownerTotal int64
		if err := tx.QueryRow(`
			SELECT COALESCE(SUM(v.size), 0)
			FROM versions v
			JOIN pastes pp ON pp.slug = v.slug
			WHERE pp.identity = ? AND pp.expires_at > ?
		`, p.Identity.String(), nowStr).Scan(&ownerTotal); err != nil {
			return fmt.Errorf("identity sum: %w", err)
		}
		if ownerTotal+body > userCap {
			return ErrOverUserQuota
		}
	}
	// 3. Insert.
	if _, err := tx.Exec(`
		INSERT INTO pastes (slug, identity, kind, content_sha, size, name,
		                    pinned_version,
		                    created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.Slug.String(), p.Identity.String(), string(p.Kind), p.ContentSHA, p.Size, p.Name,
		p.PinnedVersion,
		formatTime(p.CreatedAt), formatTime(p.UpdatedAt), formatTime(p.ExpiresAt)); err != nil {
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

// Insert is the simple variant used by tests + any caller that
// doesn't need quota enforcement. Production paths use
// InsertWithQuotaCheck.
func (r *PasteRepo) Insert(p domain.Paste) error {
	return r.InsertWithQuotaCheck(p, 0, 0, p.CreatedAt)
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

// SetPinnedVersion sets the pin to a specific version_num and rolls
// the pastes row's denormalized head (content_sha + size + kind) to
// that version's bytes. Caller verified ver exists.
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

// Unpin clears the pin (pinned_version=0) and rolls the denormalized
// head fields back to whatever the latest version (MAX ver_num) is.
func (r *PasteRepo) Unpin(slug domain.Slug) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		maxVer    int
		kind, sha string
		size      int
	)
	if err := tx.QueryRow(`
		SELECT ver_num, kind, content_sha, size FROM versions
		WHERE slug = ? ORDER BY ver_num DESC LIMIT 1
	`, slug.String()).Scan(&maxVer, &kind, &sha, &size); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lookup latest version: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE pastes
		SET pinned_version = 0, kind = ?, content_sha = ?, size = ?
		WHERE slug = ?
	`, kind, sha, size, slug.String()); err != nil {
		return fmt.Errorf("unpin: %w", err)
	}
	return tx.Commit()
}

// AppendResult reports what AppendVersionWithQuotaCheck did. The
// caller (SSH layer) uses WasPinned to decide whether to surface a
// "your pin held; new version isn't being served" warning.
type AppendResult struct {
	NewVer    int
	WasPinned bool // paste was pinned to a specific version BEFORE this append
}

// AppendVersionWithQuotaCheck atomically (under BEGIN IMMEDIATE):
//  1. checks service-wide active bytes + size against serviceCap
//  2. checks identity (the existing paste's owner) active bytes + size against userCap
//  3. inserts a new version row
//  4. resets the retention clock (updated_at + expires_at)
//  5. if the paste was UNPINNED (pinned_version=0), updates the
//     denormalized head fields (kind, content_sha, size) so the public
//     URL serves the new bytes. If the paste was PINNED to a specific
//     version, the head fields stay pointing at that version's data —
//     the new version is recorded but not served until the user
//     `unpin`s or `pin`s a different version.
//
// The "size" being charged is the new version's bytes — older versions
// continue to count toward the identity's total until the parent paste
// expires or is deleted.
func (r *PasteRepo) AppendVersionWithQuotaCheck(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, serviceCap, userCap int64, now time.Time) (AppendResult, error) {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return AppendResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := formatTime(now)
	body := int64(size)

	// Look up the paste's identity + pin state.
	var ownerIdentity string
	var existingPin int
	if err := tx.QueryRow(`SELECT identity, pinned_version FROM pastes WHERE slug = ?`, slug.String()).Scan(&ownerIdentity, &existingPin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AppendResult{}, ErrNotFound
		}
		return AppendResult{}, fmt.Errorf("lookup paste identity: %w", err)
	}

	// Service-wide check.
	if serviceCap > 0 {
		var total int64
		if err := tx.QueryRow(`
			SELECT COALESCE(SUM(v.size), 0)
			FROM versions v
			JOIN pastes pp ON pp.slug = v.slug
			WHERE pp.expires_at > ?
		`, nowStr).Scan(&total); err != nil {
			return AppendResult{}, fmt.Errorf("service-wide sum: %w", err)
		}
		if total+body > serviceCap {
			return AppendResult{}, ErrServiceFull
		}
	}
	// Per-identity check.
	if userCap > 0 {
		var ownerTotal int64
		if err := tx.QueryRow(`
			SELECT COALESCE(SUM(v.size), 0)
			FROM versions v
			JOIN pastes pp ON pp.slug = v.slug
			WHERE pp.identity = ? AND pp.expires_at > ?
		`, ownerIdentity, nowStr).Scan(&ownerTotal); err != nil {
			return AppendResult{}, fmt.Errorf("identity sum: %w", err)
		}
		if ownerTotal+body > userCap {
			return AppendResult{}, ErrOverUserQuota
		}
	}

	var maxVer int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(ver_num), 0) FROM versions WHERE slug = ?`, slug.String()).Scan(&maxVer); err != nil {
		return AppendResult{}, fmt.Errorf("max ver: %w", err)
	}
	newVer := maxVer + 1
	expires := now.Add(domain.RetentionWindow)
	if _, err := tx.Exec(`
		INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, slug.String(), newVer, string(kind), contentSHA, size, nowStr); err != nil {
		return AppendResult{}, fmt.Errorf("insert version: %w", err)
	}

	// Head fields (kind, content_sha, size) ARE updated only when the
	// paste was unpinned. A pinned paste keeps serving its pinned
	// version's data; the new version is recorded but not yet served.
	if existingPin == 0 {
		if _, err := tx.Exec(`
			UPDATE pastes
			SET kind = ?, content_sha = ?, size = ?,
			    updated_at = ?, expires_at = ?
			WHERE slug = ?
		`, string(kind), contentSHA, size, nowStr, formatTime(expires), slug.String()); err != nil {
			return AppendResult{}, fmt.Errorf("update paste head (unpinned): %w", err)
		}
	} else {
		// Pinned — only bump the clock; head fields stay pointing at
		// the pinned version's bytes.
		if _, err := tx.Exec(`
			UPDATE pastes
			SET updated_at = ?, expires_at = ?
			WHERE slug = ?
		`, nowStr, formatTime(expires), slug.String()); err != nil {
			return AppendResult{}, fmt.Errorf("update paste head (pinned): %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return AppendResult{}, err
	}
	return AppendResult{NewVer: newVer, WasPinned: existingPin != 0}, nil
}

// AppendVersion is the simple variant used by tests + any caller that
// doesn't need quota enforcement.
func (r *PasteRepo) AppendVersion(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, now time.Time) (int, error) {
	res, err := r.AppendVersionWithQuotaCheck(slug, kind, contentSHA, size, 0, 0, now)
	return res.NewVer, err
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
