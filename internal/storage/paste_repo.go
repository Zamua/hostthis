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

// ErrServiceFull is returned when the durable total-bytes ceiling is hit:
// the object store rejects a blob Put because the bucket is at its
// configured hard quota (see SPEC "Limits -> Durable total-bytes ceiling:
// an object-store quota"). The blob store surfaces it; the service layer
// translates it into a graceful "try again later" response. The metadata
// repos no longer run a service-wide byte scan, so this never originates
// from a quota pre-check on the write path. Alias of the domain-owned
// sentinel (see internal/domain/errors.go).
var ErrServiceFull = domain.ErrServiceFull

// ErrOverUserQuota is returned when accepting a write would push an
// identity's active bytes past its per-user cap. Alias of the
// domain-owned sentinel (see internal/domain/errors.go).
var ErrOverUserQuota = domain.ErrOverUserQuota

// PasteRepo is the sqlite-backed implementation of paste persistence.
type PasteRepo struct {
	db *sql.DB
	// Retention is the content-TTL policy used to (re)stamp ExpiresAt on
	// update. Defaults to the 30-day policy; the composition root overrides it.
	Retention domain.Retention
}

func NewPasteRepo(db *sql.DB) *PasteRepo {
	return &PasteRepo{db: db, Retention: domain.DefaultRetention()}
}

// InsertWithQuotaCheck atomically (under BEGIN IMMEDIATE):
//  1. checks identity active bytes + p.Size against userCap
//  2. inserts the paste row + its v1 version row
//
// Concurrent calls serialize at the transaction boundary, so two
// uploads from the same identity can't both pass the user-cap check
// and both insert. The durable total-bytes ceiling is NOT checked here:
// it is the object-store bucket quota, enforced when the blob Put is
// rejected (see SPEC "Limits -> Durable total-bytes ceiling: an
// object-store quota").
//
// Returns:
//   - nil on success
//   - ErrSlugTaken if p.Slug is already in use (caller retries with a fresh slug)
//   - ErrOverUserQuota if accepting would exceed userCap
//
// ctx is accepted to satisfy the service.PasteRepo interface (the shale
// backend carries staged blob refs on it); the sqlite path has no shale-blob
// plane, so it ignores ctx and uses its own serializable-tx context.
func (r *PasteRepo) InsertWithQuotaCheck(_ context.Context, p domain.Paste, userCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := formatTime(now)
	siteNowStr := formatSiteExpiry(now)
	body := int64(p.Size)

	// Per-identity check across BOTH pastes and sites. Tombstoned
	// versions contribute 0.
	if userCap > 0 {
		ownerTotal, err := identityActiveBytes(tx, p.Identity.String(), nowStr, siteNowStr)
		if err != nil {
			return err
		}
		if ownerTotal+body > userCap {
			return ErrOverUserQuota
		}
	}
	// A slug must be unique across sites too (a read resolves a slug in
	// either table). The SiteRepo already checks the pastes table on insert;
	// mirror it here so a paste cannot take a slug a site owns.
	var siteExists int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM sites WHERE slug = ?`, p.Slug.String()).Scan(&siteExists); err != nil {
		return fmt.Errorf("check site slug collision: %w", err)
	}
	if siteExists > 0 {
		return ErrSlugTaken
	}
	// 3. Insert.
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

// Insert is the simple variant used by tests + any caller that
// doesn't need quota enforcement. Production paths use
// InsertWithQuotaCheck.
func (r *PasteRepo) Insert(p domain.Paste) error {
	return r.InsertWithQuotaCheck(context.Background(), p, 0, p.CreatedAt)
}

// Get returns the paste for slug, or ErrNotFound. Expired pastes are
// still returned by this read - the HTTP layer is responsible for
// 404'ing them; the background sweep is responsible for deleting
// them.
func (r *PasteRepo) Get(slug domain.Slug) (domain.Paste, error) {
	row := r.db.QueryRow(`
		SELECT slug, identity, status, kind, content_sha, size, name,
		       pinned_version,
		       created_at, updated_at, expires_at
		FROM pastes WHERE slug = ?
	`, slug.String())
	return scanPaste(row)
}

// ListByOwner returns all of an owner's active pastes, ordered by
// expires_at ascending (soonest to die first - matches what `list`
// should show). Empty identity returns no rows.
func (r *PasteRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	if owner == "" {
		return nil, nil
	}
	// Subquery computes MAX(ver_num) per paste so `list` can show "latest
	// vN" alongside the served version. The N+1 query alternative would
	// have been simpler but the active-pastes-per-owner count is small
	// enough that one join in one statement is the right shape.
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
	defer rows.Close()
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

// MarkReady flips a paste's status pending -> ready (the finalizer's
// success transition once the blob landed). Guarded by the WHERE clause:
// only a still-pending row is advanced, so a late finalizer cannot
// resurrect a failed paste. A missing or non-pending row is a no-op. See
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

// MarkFailed flips a paste's status pending -> failed (the finalizer's
// failure transition AND the reconciler's age-out). The row stays so a
// read can serve an error page; the quota SUM excludes failed pastes
// (see identityActiveBytes), so flipping the status IS the byte release -
// no separate index to drop on sqlite. Guarded by the WHERE clause: only
// a still-pending row transitions, so a ready paste is never un-counted.
// Idempotent: a re-call finds the row already failed and changes nothing.
// See docs/SPEC.md "Paste lifecycle status (async blob write)".
func (r *PasteRepo) MarkFailed(slug domain.Slug) error {
	_, err := r.db.Exec(
		`UPDATE pastes SET status = 'failed' WHERE slug = ? AND status = 'pending'`,
		slug.String())
	if err != nil {
		return fmt.Errorf("mark failed %q: %w", slug, err)
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
//  1. checks identity (the existing paste's owner) active bytes + size against userCap
//  2. inserts a new version row
//  3. resets the retention clock (updated_at + expires_at)
//  4. if the paste was UNPINNED (pinned_version=0), updates the
//     denormalized head fields (kind, content_sha, size) so the public
//     URL serves the new bytes. If the paste was PINNED to a specific
//     version, the head fields stay pointing at that version's data -
//     the new version is recorded but not served until the user
//     `unpin`s or `pin`s a different version.
//
// The "size" being charged is the new version's bytes - older versions
// continue to count toward the identity's total until the parent paste
// expires or is deleted. EXCEPTION: if the target paste is EXPIRED-unswept it
// is excluded from identityActiveBytes, but the UPDATE below resets expires_at
// (revives it), so the check charges the paste's full post-revival size - its
// existing non-deleted version bytes plus the new version - not the new version
// alone (docs/SPEC.md "Reviving an expired-but-unswept record charges its FULL
// post-revival size").
// ctx is accepted to satisfy the service.PasteAdmin interface; the sqlite
// path ignores it (no shale-blob plane) and uses its own serializable-tx
// context.
func (r *PasteRepo) AppendVersionWithQuotaCheck(_ context.Context, slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time) (AppendResult, error) {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return AppendResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := formatTime(now)
	siteNowStr := formatSiteExpiry(now)
	body := int64(size)

	// Look up the paste's identity + pin state + liveness. The paste
	// expires_at column is stored with formatTime (RFC3339Nano), so it is
	// compared against nowStr (the same format), NOT siteNowStr.
	var ownerIdentity string
	var existingPin int
	var pasteLive bool
	if err := tx.QueryRow(`SELECT identity, pinned_version, expires_at > ? FROM pastes WHERE slug = ?`, nowStr, slug.String()).Scan(&ownerIdentity, &existingPin, &pasteLive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AppendResult{}, ErrNotFound
		}
		return AppendResult{}, fmt.Errorf("lookup paste identity: %w", err)
	}

	// Per-identity check across BOTH pastes and sites. Tombstoned
	// versions contribute 0.
	if userCap > 0 {
		ownerTotal, err := identityActiveBytes(tx, ownerIdentity, nowStr, siteNowStr)
		if err != nil {
			return AppendResult{}, err
		}
		// Reviving an EXPIRED-unswept paste must charge its full post-revival
		// size: identityActiveBytes filters expires_at > now, so an expired
		// paste's existing versions are NOT in ownerTotal - but the UPDATE below
		// resets expires_at (revives it), bringing them back. Charge the paste's
		// existing non-deleted version bytes PLUS the new version, not the new
		// version alone, or the revived paste durably exceeds the cap (docs/SPEC.md
		// "Reviving an expired-but-unswept record charges its FULL post-revival
		// size"). Matches the slatedb + shale append revival charge.
		charge := body
		if !pasteLive {
			var revived int64
			if err := tx.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM versions WHERE slug = ? AND deleted = 0`, slug.String()).Scan(&revived); err != nil {
				return AppendResult{}, fmt.Errorf("revived version sum: %w", err)
			}
			charge += revived
		}
		if ownerTotal+charge > userCap {
			return AppendResult{}, ErrOverUserQuota
		}
	}

	// MAX(ver_num) includes deleted rows - version numbers are NOT
	// reused even after a tombstone. Predictable for users who
	// `versions` and see continuous numbering.
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
		// Pinned - only bump the clock; head fields stay pointing at
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
	res, err := r.AppendVersionWithQuotaCheck(context.Background(), slug, kind, contentSHA, size, 0, now)
	return res.NewVer, err
}

// ListVersions returns the version history for slug, newest first.
// Includes tombstoned (deleted=1) rows so the `versions` verb can
// render them with a `deleted` marker.
func (r *PasteRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	rows, err := r.db.Query(`
		SELECT slug, ver_num, kind, content_sha, size, created_at, deleted
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

// GetVersion returns a single version row, or ErrNotFound. Returns
// tombstoned rows too; callers needing "real, with bytes" should
// check v.Deleted.
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

// DeleteVersion marks a single version's blob bytes as freed by
// flipping the deleted flag to 1. The row stays so:
//   - ver_num isn't reused (a future `update` still gets MAX(ver_num)+1)
//   - audit trail + `versions` history survives
//
// Quota SUMs, latest-served-version pickers, and AppendVersion's
// MAX(ver_num) all filter `deleted = 0` so tombstones don't influence
// active accounting.
//
// Returns ErrNotFound if (slug, ver) doesn't exist. Does NOT check
// owner or served-status - service layer enforces those.
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

// CountByOwner returns how many active pastes the owner has - used
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
// has alive - the sum of every VERSION row attached to any of its
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
		WHERE p.identity = ? AND p.expires_at > ? AND v.deleted = 0
		  AND p.status != 'failed'
	`, owner, formatTime(now)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sum active size: %w", err)
	}
	return n.Int64, nil
}

// SumActiveBytesByOwner is the int-returning shape the service layer's
// PasteAdmin interface expects (whoami formatting uses int). Same
// query as SumActiveSizeByOwner but with the type narrowed.
func (r *PasteRepo) SumActiveBytesByOwner(owner string, now time.Time) (int, error) {
	n, err := r.SumActiveSizeByOwner(owner, now)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// OwnerFirstSeen returns the earliest CreatedAt across the owner's
// pastes - a proxy for "when did we first see this key". Zero
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

// scanPaste is shared by Get + ListByOwner - same column order, one
// place to update if columns shift.
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

// scanPasteWithLatest is scanPaste plus the latest_version column. Used
// by ListByOwner; one extra COALESCE'd subquery in the SELECT.
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

// scanner is the minimal interface *sql.Row and *sql.Rows both satisfy
// so scanPaste can take either.
type scanner interface{ Scan(dest ...any) error }

// ErrSlugTaken is returned by Insert when the chosen slug already exists.
// Alias of the domain-owned sentinel (see internal/domain/errors.go).
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
