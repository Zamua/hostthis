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

// ErrServiceFull is returned when the object store rejects a blob Put because
// the bucket is at its configured hard quota (SPEC "Limits -> Durable
// total-bytes ceiling"). It only ever originates from the blob store; the
// metadata repos run no service-wide byte scan. Alias of the domain sentinel.
var ErrServiceFull = domain.ErrServiceFull

// ErrOverUserQuota is returned when a write would push an identity's active
// bytes past its per-user cap. Alias of the domain sentinel.
var ErrOverUserQuota = domain.ErrOverUserQuota

// PasteRepo is the sqlite-backed implementation of paste persistence.
type PasteRepo struct {
	db *sql.DB
	// Retention re-stamps ExpiresAt on update. The composition root overrides
	// the default.
	Retention domain.Retention
}

func NewPasteRepo(db *sql.DB) *PasteRepo {
	return &PasteRepo{db: db, Retention: domain.DefaultRetention()}
}

// InsertWithQuotaCheck checks the identity's active bytes against userCap and
// inserts the paste row plus its v1 version row, both under one BEGIN
// IMMEDIATE. Concurrent calls serialize at the transaction boundary, so two
// uploads from one identity cannot both pass the cap check and both insert.
// The durable total-bytes ceiling is not checked here: it is the object-store
// bucket quota, enforced when the blob Put is rejected.
//
// Returns ErrSlugTaken when p.Slug is in use (the caller re-mints), or
// ErrOverUserQuota when accepting would exceed userCap.
//
// ctx exists to satisfy service.PasteRepo, on which the shale backend carries
// staged blob refs. The sqlite path has no blob plane and ignores it.
func (r *PasteRepo) InsertWithQuotaCheck(_ context.Context, p domain.Paste, userCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := formatTime(now)
	siteNowStr := formatSiteExpiry(now)
	body := int64(p.Size)

	// The check spans BOTH pastes and sites. Tombstoned versions contribute 0.
	if userCap > 0 {
		ownerTotal, err := identityActiveBytes(tx, p.Identity.String(), nowStr, siteNowStr)
		if err != nil {
			return err
		}
		if err := (domain.Allowance{Cap: userCap, Used: ownerTotal}).Admit(body); err != nil {
			return err
		}
	}
	// A slug must be unique across sites too, since a read resolves a slug in
	// either table. SiteRepo checks the pastes table on insert; this mirrors it
	// so a paste cannot take a slug a site owns.
	var siteExists int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM sites WHERE slug = ?`, p.Slug.String()).Scan(&siteExists); err != nil {
		return fmt.Errorf("check site slug collision: %w", err)
	}
	if siteExists > 0 {
		return ErrSlugTaken
	}
	if _, err := tx.Exec(`
		INSERT INTO pastes (slug, identity, status, kind, content_sha, size, name,
		                    pinned_version,
		                    created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.Slug.String(), p.Identity.String(), string(domain.NormalizeStatus(string(p.Status))),
		string(p.Kind), p.ContentSHA, p.Size, p.Name,
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

// Get returns the paste for slug, or ErrNotFound. Expired pastes are still
// returned: the HTTP layer 404s them and the sweep deletes them.
func (r *PasteRepo) Get(slug domain.Slug) (domain.Paste, error) {
	row := r.db.QueryRow(`
		SELECT slug, identity, status, kind, content_sha, size, name,
		       pinned_version,
		       created_at, updated_at, expires_at
		FROM pastes WHERE slug = ?
	`, slug.String())
	return scanPaste(row)
}

// ListByOwner returns an owner's active pastes ordered by expires_at ascending
// (soonest to die first, what `list` shows). An empty identity returns no rows.
func (r *PasteRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	if owner == "" {
		return nil, nil
	}
	// The subquery computes MAX(ver_num) per paste so `list` can show "latest
	// vN" alongside the served version, in one statement instead of N+1.
	rows, err := r.db.Query(`
		SELECT p.slug, p.identity, p.status, p.kind, p.content_sha, p.size, p.name,
		       p.pinned_version,
		       p.created_at, p.updated_at, p.expires_at,
		       COALESCE((SELECT MAX(ver_num) FROM versions v WHERE v.slug = p.slug AND v.deleted = 0), 1) AS latest_version
		FROM pastes p WHERE p.identity = ?
		ORDER BY p.expires_at ASC
	`, owner)
	if err != nil {
		return nil, fmt.Errorf("list by owner: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []domain.Paste
	for rows.Next() {
		p, err := scanPasteWithLatest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete removes a paste and, via FK cascade, all its versions. The caller owns
// the ownership check.
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

// MarkReady flips a paste pending -> ready once its blob has landed. The WHERE
// clause advances only a still-pending row, so a late finalizer cannot
// resurrect a failed paste; a missing or non-pending row is a no-op. See
// docs/SPEC.md "Paste lifecycle status (async blob write)".
func (r *PasteRepo) MarkReady(slug domain.Slug) error {
	_, err := r.db.Exec(
		`UPDATE pastes SET status = 'ready' WHERE slug = ? AND status = 'pending'`,
		slug.String())
	if err != nil {
		return fmt.Errorf("mark ready %q: %w", slug, err)
	}
	return nil
}

// MarkFailed flips a paste pending -> failed. The row stays so a read can serve
// an error page; identityActiveBytes excludes failed pastes, so the status flip
// IS the byte release. The WHERE clause transitions only a still-pending row,
// so a ready paste is never un-counted, and a re-call is a no-op. See
// docs/SPEC.md "Paste lifecycle status (async blob write)".
func (r *PasteRepo) MarkFailed(slug domain.Slug) error {
	_, err := r.db.Exec(
		`UPDATE pastes SET status = 'failed' WHERE slug = ? AND status = 'pending'`,
		slug.String())
	if err != nil {
		return fmt.Errorf("mark failed %q: %w", slug, err)
	}
	return nil
}

// SetPinnedVersion pins to ver and rolls the denormalized head (content_sha,
// size, kind) to that version's bytes. The caller has verified ver exists.
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

// Unpin clears the pin and rolls the denormalized head fields to the latest
// LIVE version, the same rule the read path applies to an unpinned paste.
// Tombstoned versions are skipped: rolling the head onto one would serve bytes
// the owner deleted, or 404 once the GC reclaims them. ErrNotFound when the
// paste has no live version left.
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
		WHERE slug = ? AND deleted = 0 ORDER BY ver_num DESC LIMIT 1
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

// AppendResult aliases domain.AppendResult, which owns the type: the service
// port returning it must not name a storage-owned type.
type AppendResult = domain.AppendResult

// AppendVersionWithQuotaCheck checks the owning identity against userCap,
// inserts a version row, resets the retention clock, and (only when the paste
// is unpinned) rolls the denormalized head fields so the public URL serves the
// new bytes. A pinned paste records the version without serving it until the
// owner unpins or repins. All of it runs under one BEGIN IMMEDIATE.
//
// The charge is the new version's bytes; older versions keep counting until the
// parent paste expires or is deleted. An EXPIRED-unswept paste is the
// exception: identityActiveBytes excludes it, but the UPDATE below revives it,
// so the charge is its FULL post-revival size (existing non-deleted version
// bytes plus the new version) or the revived paste durably exceeds the cap.
//
// ctx exists to satisfy service.PasteAdmin; the sqlite path ignores it.
func (r *PasteRepo) AppendVersionWithQuotaCheck(_ context.Context, slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time) (AppendResult, error) {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return AppendResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := formatTime(now)
	siteNowStr := formatSiteExpiry(now)
	body := int64(size)

	// The pastes.expires_at column is written with formatTime (RFC3339Nano), so
	// it must be compared against nowStr, NOT siteNowStr.
	var ownerIdentity string
	var existingPin int
	var pasteLive bool
	if err := tx.QueryRow(`SELECT identity, pinned_version, expires_at > ? FROM pastes WHERE slug = ?`, nowStr, slug.String()).Scan(&ownerIdentity, &existingPin, &pasteLive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AppendResult{}, ErrNotFound
		}
		return AppendResult{}, fmt.Errorf("lookup paste identity: %w", err)
	}

	// The check spans BOTH pastes and sites. Tombstoned versions contribute 0.
	if userCap > 0 {
		ownerTotal, err := identityActiveBytes(tx, ownerIdentity, nowStr, siteNowStr)
		if err != nil {
			return AppendResult{}, err
		}
		// identityActiveBytes filters expires_at > now, so an expired paste's
		// existing versions are absent from ownerTotal, yet the UPDATE below
		// revives them. Add them to the charge, or the revived paste durably
		// exceeds the cap.
		charge := body
		if !pasteLive {
			var revived int64
			if err := tx.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM versions WHERE slug = ? AND deleted = 0`, slug.String()).Scan(&revived); err != nil {
				return AppendResult{}, fmt.Errorf("revived version sum: %w", err)
			}
			charge += revived
		}
		if err := (domain.Allowance{Cap: userCap, Used: ownerTotal}).Admit(charge); err != nil {
			return AppendResult{}, err
		}
	}

	// MAX(ver_num) includes deleted rows, so a version number is never reused
	// after a tombstone and `versions` shows continuous numbering.
	var maxVer int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(ver_num), 0) FROM versions WHERE slug = ?`, slug.String()).Scan(&maxVer); err != nil {
		return AppendResult{}, fmt.Errorf("max ver: %w", err)
	}
	newVer := maxVer + 1
	expires := r.Retention.ExpiryFor(now)
	if _, err := tx.Exec(`
		INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, slug.String(), newVer, string(kind), contentSHA, size, nowStr); err != nil {
		return AppendResult{}, fmt.Errorf("insert version: %w", err)
	}

	// The head fields move only when the paste is unpinned; a pinned paste
	// keeps serving its pinned version's bytes.
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
		// Pinned: bump only the clock.
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

// ListVersions returns the version history for slug, newest first. Tombstoned
// rows are included so `versions` can render them with a `deleted` marker.
func (r *PasteRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	rows, err := r.db.Query(`
		SELECT slug, ver_num, kind, content_sha, size, created_at, deleted
		FROM versions WHERE slug = ?
		ORDER BY ver_num DESC
	`, slug.String())
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []domain.Version
	for rows.Next() {
		var v domain.Version
		var slugStr, kind, created string
		var deletedInt int
		if err := rows.Scan(&slugStr, &v.VerNum, &kind, &v.ContentSHA, &v.Size, &created, &deletedInt); err != nil {
			return nil, err
		}
		v.Slug = domain.Slug(slugStr)
		v.Kind = domain.ContentKind(kind)
		v.CreatedAt = parseTime(created)
		v.Deleted = deletedInt != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion returns a single version row, or ErrNotFound. Tombstoned rows are
// returned too; a caller needing bytes must check v.Deleted.
func (r *PasteRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	row := r.db.QueryRow(`
		SELECT slug, ver_num, kind, content_sha, size, created_at, deleted
		FROM versions WHERE slug = ? AND ver_num = ?
	`, slug.String(), ver)
	var v domain.Version
	var slugStr, kind, created string
	var deletedInt int
	if err := row.Scan(&slugStr, &v.VerNum, &kind, &v.ContentSHA, &v.Size, &created, &deletedInt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Version{}, ErrNotFound
		}
		return domain.Version{}, err
	}
	v.Slug = domain.Slug(slugStr)
	v.Kind = domain.ContentKind(kind)
	v.CreatedAt = parseTime(created)
	v.Deleted = deletedInt != 0
	return v, nil
}

// DeleteVersion tombstones one version by setting deleted = 1. The row stays so
// ver_num is never reused and the `versions` history survives; quota SUMs and
// latest-served-version pickers filter deleted = 0 so a tombstone contributes
// nothing to active accounting.
//
// Returns ErrNotFound when (slug, ver) does not exist. Owner and served-status
// checks belong to the service layer.
func (r *PasteRepo) DeleteVersion(slug domain.Slug, ver int) error {
	res, err := r.db.Exec(`UPDATE versions SET deleted = 1 WHERE slug = ? AND ver_num = ?`, slug.String(), ver)
	if err != nil {
		return fmt.Errorf("delete version: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountByOwner returns how many active pastes the owner has.
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

// SumActiveSizeByOwner totals the identity's live bytes: every VERSION row
// under a non-expired paste. Summing versions rather than pastes is what
// accounts for update history, since each update leaves a version row on disk
// until the parent paste expires or is deleted (the FK cascade frees both).
func (r *PasteRepo) SumActiveSizeByOwner(owner string, now time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	var n sql.NullInt64
	err := r.db.QueryRow(`
		SELECT COALESCE(SUM(v.size), 0)
		FROM versions v
		JOIN pastes p ON p.slug = v.slug
		WHERE p.identity = ? AND p.expires_at > ? AND v.deleted = 0
		  AND p.status != 'failed'
	`, owner, formatTime(now)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sum active size: %w", err)
	}
	return n.Int64, nil
}

// SumActiveBytesByOwner is SumActiveSizeByOwner narrowed to the int the
// service layer's PasteAdmin interface expects.
func (r *PasteRepo) SumActiveBytesByOwner(owner string, now time.Time) (int, error) {
	n, err := r.SumActiveSizeByOwner(owner, now)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// OwnerFirstSeen returns the earliest CreatedAt across the owner's pastes, a
// proxy for when the key was first seen. Zero time when the owner has none.
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

// scanPaste is shared by Get and ListByOwner: one place to change when the
// column order shifts.
func scanPaste(s scanner) (domain.Paste, error) {
	var p domain.Paste
	var slugStr, identStr, status, kind, created, updated, expires string
	if err := s.Scan(&slugStr, &identStr, &status, &kind, &p.ContentSHA, &p.Size, &p.Name,
		&p.PinnedVersion,
		&created, &updated, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Paste{}, ErrNotFound
		}
		return domain.Paste{}, fmt.Errorf("scan paste: %w", err)
	}
	p.Slug = domain.Slug(slugStr)
	p.Identity = domain.Identity(identStr)
	p.Status = domain.NormalizeStatus(status)
	p.Kind = domain.ContentKind(kind)
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	p.ExpiresAt = parseTime(expires)
	return p, nil
}

// scanPasteWithLatest is scanPaste plus the latest_version column.
func scanPasteWithLatest(s scanner) (domain.Paste, error) {
	var p domain.Paste
	var slugStr, identStr, status, kind, created, updated, expires string
	if err := s.Scan(&slugStr, &identStr, &status, &kind, &p.ContentSHA, &p.Size, &p.Name,
		&p.PinnedVersion,
		&created, &updated, &expires,
		&p.LatestVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Paste{}, ErrNotFound
		}
		return domain.Paste{}, fmt.Errorf("scan paste with latest: %w", err)
	}
	p.Slug = domain.Slug(slugStr)
	p.Identity = domain.Identity(identStr)
	p.Status = domain.NormalizeStatus(status)
	p.Kind = domain.ContentKind(kind)
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	p.ExpiresAt = parseTime(expires)
	return p, nil
}

// scanner is the minimal interface both *sql.Row and *sql.Rows satisfy, so
// scanPaste can take either.
type scanner interface{ Scan(dest ...any) error }

// ErrSlugTaken is returned by Insert when the chosen slug already exists.
// Alias of the domain sentinel.
var ErrSlugTaken = domain.ErrSlugTaken

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
