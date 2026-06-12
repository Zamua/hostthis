package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// SiteRepo is the sqlite-backed implementation of static-site
// persistence. It lives alongside PasteRepo and shares the same db.
// A site row carries an Identity (owner) on every record and is queried
// by slug, so the same owner-gating and not-found-on-cross-owner
// behavior pastes get applies to sites.
type SiteRepo struct {
	db *sql.DB
}

func NewSiteRepo(db *sql.DB) *SiteRepo { return &SiteRepo{db: db} }

// expirySiteTimeFormat is the timestamp layout the site EXPIRY clock uses
// across every backend: the sqlite sites.expires_at column, and the
// slatedb/shale expiry_sites/<ts>/<slug> index keys. It is RFC3339 with a
// FIXED-WIDTH, zero-padded 9-digit nanosecond fraction, so a lexicographic
// compare (the sqlite TEXT-column compare AND the KV key prefix scan) is
// byte order == time order EXACTLY, including within a shared whole second.
//
// It is deliberately NOT time.RFC3339Nano, which drops trailing fractional
// zeros: under that variable-width format "...00.5Z" sorts BEFORE "...00Z"
// (because '.' < 'Z'), so a record could be swept up to ~1s before its real
// ExpiresAt. Sites have no prod data on any backend yet, so the fixed-width
// format is free here. The pre-existing PASTE expiry path (sqlite
// pastes.expires_at via formatTime, and the slatedb/shale expiry/<rfc3339>/
// index) still uses time.RFC3339Nano and carries the same latent sub-second
// skew; it holds live prod data on slatedb, so re-keying it is a migration
// (a format flip alone orphans old keys -> those pastes never expire) and is
// a documented follow-up, not done here.
const expirySiteTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// formatSiteExpiry / parseSiteExpiry are the fixed-width site-expiry
// (de)serializers. Used for the sqlite sites.expires_at column and every
// query that compares against it, so the stored value and the comparison
// operand share one byte-order == time-order layout.
func formatSiteExpiry(t time.Time) string { return t.UTC().Format(expirySiteTimeFormat) }
func parseSiteExpiry(s string) time.Time {
	t, err := time.Parse(expirySiteTimeFormat, s)
	if err != nil {
		// Tolerate a legacy RFC3339Nano value (e.g. a row written before the
		// fixed-width switch) so a read never silently zeroes a timestamp.
		return parseTime(s)
	}
	return t
}

// manifestJSON is the on-disk shape of a Manifest. Kept private so the
// JSON representation is an internal storage concern, not a domain one:
// the domain Manifest is the value object; this is just how we persist
// it. Field names are short to keep the metadata footprint small.
type manifestJSON struct {
	Files map[string]entryJSON `json:"files"`
}

type entryJSON struct {
	SHA  string `json:"sha"`
	Size int    `json:"size"`
	CT   string `json:"ct"`
}

func encodeManifest(m domain.Manifest) (string, error) {
	mj := manifestJSON{Files: make(map[string]entryJSON, len(m.Files))}
	for p, e := range m.Files {
		mj.Files[p] = entryJSON{SHA: e.SHA, Size: e.Size, CT: e.ContentType}
	}
	b, err := json.Marshal(mj)
	if err != nil {
		return "", fmt.Errorf("encode manifest: %w", err)
	}
	return string(b), nil
}

func decodeManifest(s string) (domain.Manifest, error) {
	var mj manifestJSON
	if err := json.Unmarshal([]byte(s), &mj); err != nil {
		return domain.Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	m := domain.NewManifest()
	for p, e := range mj.Files {
		m.Add(p, domain.ManifestEntry{SHA: e.SHA, Size: e.Size, ContentType: e.CT})
	}
	return m, nil
}

// InsertWithQuotaCheck atomically (under the serializable tx) checks
// the deploying identity's quota against the SAME per-identity cap a
// paste upload uses, summing BOTH the identity's active paste versions
// AND its active sites, then inserts the site row.
//
// The "deduped size" charged is s.Manifest.DedupedSize() - distinct
// blobs only, matching the "dedupe for free" storage property and the
// quota the decompression-bomb guard enforced mid-untar. The serializable
// boundary mirrors PasteRepo so concurrent uploads from the same identity
// can't both pass the cap check and both insert.
//
// Returns:
//   - nil on success
//   - ErrSlugTaken if the slug already exists (in sites OR pastes)
//   - ErrServiceFull if accepting would exceed serviceCap
//   - ErrOverUserQuota if accepting would exceed userCap
func (r *SiteRepo) InsertWithQuotaCheck(s domain.Site, dedupedSize int, serviceCap, userCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := formatTime(now)
	siteNowStr := formatSiteExpiry(now)
	body := int64(dedupedSize)

	// 1. Service-wide check across BOTH pastes and sites.
	if serviceCap > 0 {
		total, err := serviceWideActiveBytes(tx, nowStr, siteNowStr)
		if err != nil {
			return err
		}
		if total+body > serviceCap {
			return ErrServiceFull
		}
	}
	// 2. Per-identity check across BOTH pastes and sites.
	if userCap > 0 {
		owned, err := identityActiveBytes(tx, s.Identity.String(), nowStr, siteNowStr)
		if err != nil {
			return err
		}
		if owned+body > userCap {
			return ErrOverUserQuota
		}
	}

	manStr, err := encodeManifest(s.Manifest)
	if err != nil {
		return err
	}

	// A slug must be unique across pastes too (a read resolves a slug
	// in either table). Reject if a paste already owns it.
	var pasteExists int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM pastes WHERE slug = ?`, s.Slug.String()).Scan(&pasteExists); err != nil {
		return fmt.Errorf("check paste slug collision: %w", err)
	}
	if pasteExists > 0 {
		return ErrSlugTaken
	}

	if _, err := tx.Exec(`
		INSERT INTO sites (slug, identity, manifest, deduped_size,
		                   created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, s.Slug.String(), s.Identity.String(), manStr, dedupedSize,
		formatTime(s.CreatedAt), formatTime(s.UpdatedAt), formatSiteExpiry(s.ExpiresAt)); err != nil {
		if isUniqueViolation(err) {
			return ErrSlugTaken
		}
		return fmt.Errorf("insert site %q: %w", s.Slug, err)
	}
	return tx.Commit()
}

// ReplaceWithQuotaCheck re-deploys an existing owned site in place,
// atomically (under the serializable tx) swapping its manifest, deduped
// size, updated_at, and expires_at, while enforcing the per-identity and
// service caps against the REPLACE DELTA.
//
// The slug must already exist as a site AND be owned by s.Identity. If it
// is missing, OR owned by a different identity, this returns ErrNotFound -
// the SAME sentinel a missing row yields, so the caller can't distinguish
// "not yours" from "doesn't exist" (no existence leak).
//
// The quota check credits the OLD row's deduped bytes and charges the new
// dedupedSize: the identity-active and service-wide sums computed inside
// the tx already include the old row, so the new totals subtract the old
// deduped bytes and add the new (total - oldDeduped + body). A same-size
// re-deploy nets zero; a smaller one frees the difference.
//
// Returns:
//   - nil on success
//   - ErrNotFound if the slug isn't a site owned by s.Identity
//   - ErrServiceFull if accepting would exceed serviceCap
//   - ErrOverUserQuota if accepting would exceed userCap
func (r *SiteRepo) ReplaceWithQuotaCheck(s domain.Site, dedupedSize int, serviceCap, userCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := formatTime(now)
	siteNowStr := formatSiteExpiry(now)
	body := int64(dedupedSize)

	// Ownership + existence gate, inside the tx so a concurrent delete /
	// re-deploy can't race the swap. A missing row and a foreign-owned row
	// both collapse to ErrNotFound.
	var ownerStr string
	var oldDeduped int64
	var oldLive bool
	err = tx.QueryRow(`SELECT identity, deduped_size, expires_at > ? FROM sites WHERE slug = ?`, siteNowStr, s.Slug.String()).
		Scan(&ownerStr, &oldDeduped, &oldLive)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load site for replace: %w", err)
	}
	if ownerStr != s.Identity.String() {
		return ErrNotFound
	}

	// Credit the old bytes back ONLY if the old row is still live: the active
	// sums below filter on expiry, so an expired-but-unswept old row is NOT
	// in them. Crediting it would under-count and admit an over-quota
	// re-deploy (resurrecting an expired site must charge the full new size).
	creditOld := int64(0)
	if oldLive {
		creditOld = oldDeduped
	}

	// Quota: the active sums below include the OLD (live) row, so subtract
	// its credited deduped bytes and add the new to evaluate post-swap totals.
	if serviceCap > 0 {
		total, err := serviceWideActiveBytes(tx, nowStr, siteNowStr)
		if err != nil {
			return err
		}
		if total-creditOld+body > serviceCap {
			return ErrServiceFull
		}
	}
	if userCap > 0 {
		owned, err := identityActiveBytes(tx, s.Identity.String(), nowStr, siteNowStr)
		if err != nil {
			return err
		}
		if owned-creditOld+body > userCap {
			return ErrOverUserQuota
		}
	}

	manStr, err := encodeManifest(s.Manifest)
	if err != nil {
		return err
	}

	// Swap in place: created_at is left untouched (the slug's birth time is
	// stable across re-deploys); updated_at + expires_at restart the clock.
	res, err := tx.Exec(`
		UPDATE sites
		SET manifest = ?, deduped_size = ?, updated_at = ?, expires_at = ?
		WHERE slug = ? AND identity = ?
	`, manStr, dedupedSize, formatTime(s.UpdatedAt), formatSiteExpiry(s.ExpiresAt),
		s.Slug.String(), s.Identity.String())
	if err != nil {
		return fmt.Errorf("replace site %q: %w", s.Slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("replace site rows affected: %w", err)
	}
	if n == 0 {
		// Row vanished between the gate and the UPDATE (shouldn't happen
		// under the serializable tx, but fail closed as not-found).
		return ErrNotFound
	}
	return tx.Commit()
}

// Insert is the simple variant used by tests + callers that don't need
// quota enforcement.
func (r *SiteRepo) Insert(s domain.Site) error {
	return r.InsertWithQuotaCheck(s, s.Manifest.DedupedSize(), 0, 0, s.CreatedAt)
}

// Get returns the site for slug, or ErrNotFound. Like PasteRepo.Get it
// returns expired rows too - the HTTP layer 404s them, the sweep deletes
// them.
func (r *SiteRepo) Get(slug domain.Slug) (domain.Site, error) {
	row := r.db.QueryRow(`
		SELECT slug, identity, manifest, created_at, updated_at, expires_at
		FROM sites WHERE slug = ?
	`, slug.String())
	var slugStr, identStr, manStr, created, updated, expires string
	if err := row.Scan(&slugStr, &identStr, &manStr, &created, &updated, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Site{}, ErrNotFound
		}
		return domain.Site{}, fmt.Errorf("scan site: %w", err)
	}
	man, err := decodeManifest(manStr)
	if err != nil {
		return domain.Site{}, err
	}
	return domain.Site{
		Slug:      domain.Slug(slugStr),
		Identity:  domain.Identity(identStr),
		Manifest:  man,
		CreatedAt: parseTime(created),
		UpdatedAt: parseTime(updated),
		ExpiresAt: parseSiteExpiry(expires),
	}, nil
}

// Delete removes a site row. Caller is responsible for the owner check.
func (r *SiteRepo) Delete(slug domain.Slug) error {
	if _, err := r.db.Exec(`DELETE FROM sites WHERE slug = ?`, slug.String()); err != nil {
		return fmt.Errorf("delete site %q: %w", slug, err)
	}
	return nil
}

// ExpiredSiteSlugs returns the slugs of sites whose expires_at is at or
// before now. The sweep deletes them; the HTTP read path 404s expired-
// but-not-yet-deleted rows inline.
func (r *SiteRepo) ExpiredSiteSlugs(now time.Time) ([]string, error) {
	rows, err := r.db.Query(`SELECT slug FROM sites WHERE expires_at <= ?`, formatSiteExpiry(now))
	if err != nil {
		return nil, fmt.Errorf("expired site slugs: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReferencedSiteBlobSHAs returns the set of blob SHAs referenced by any
// site's manifest. The sweep unions this with the paste-side referenced
// set so a blob shared between a site and a paste (or between two sites)
// is kept alive as long as ANY live record references it.
func (r *SiteRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	rows, err := r.db.Query(`SELECT manifest FROM sites`)
	if err != nil {
		return nil, fmt.Errorf("site manifests for gc: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]struct{}, 256)
	for rows.Next() {
		var manStr string
		if err := rows.Scan(&manStr); err != nil {
			return nil, err
		}
		man, err := decodeManifest(manStr)
		if err != nil {
			return nil, err
		}
		for _, sha := range man.SHASet() {
			seen[sha] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for sha := range seen {
		out = append(out, sha)
	}
	return out, nil
}

// serviceWideActiveBytes sums active bytes across pastes (live,
// non-deleted versions), sites (deduped_size of non-expired sites), AND
// rooms (value bytes of non-expired rooms). Shared by the paste, site, and
// room quota checks so the service-wide cap counts every kind of stored
// content - a write of any kind sees the bytes the other kinds already
// hold. Runs inside the caller's tx.
// serviceWideActiveBytes takes the now-string in BOTH formats because the
// paste expires_at column is stored with formatTime (RFC3339Nano) while the
// site AND room expires_at columns are stored with formatSiteExpiry
// (fixed-width); each subquery compares its column against the
// matching-format operand, so a cross-format lexical comparison never
// happens. (The room expiry column adopted the fixed-width format so the
// sweep's sub-second ordering is correct, matching the site column.)
func serviceWideActiveBytes(tx *sql.Tx, nowStr, siteNowStr string) (int64, error) {
	var pasteTotal int64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(v.size), 0)
		FROM versions v
		JOIN pastes pp ON pp.slug = v.slug
		WHERE pp.expires_at > ? AND v.deleted = 0
	`, nowStr).Scan(&pasteTotal); err != nil {
		return 0, fmt.Errorf("service-wide paste sum: %w", err)
	}
	var siteTotal int64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(deduped_size), 0) FROM sites WHERE expires_at > ?
	`, siteNowStr).Scan(&siteTotal); err != nil {
		return 0, fmt.Errorf("service-wide site sum: %w", err)
	}
	// The room expires_at is fixed-width (formatSiteExpiry), the same as the
	// site column, so it takes siteNowStr.
	roomTotal, err := activeRoomBytesTx(tx, siteNowStr)
	if err != nil {
		return 0, err
	}
	return pasteTotal + siteTotal + roomTotal, nil
}

// identityActiveBytes sums active bytes owned by one identity across
// BOTH pastes and sites. Runs inside the caller's tx. Takes two now-strings
// for the same reason serviceWideActiveBytes does (paste vs site column
// timestamp formats differ).
func identityActiveBytes(tx *sql.Tx, identity, nowStr, siteNowStr string) (int64, error) {
	var pasteTotal int64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(v.size), 0)
		FROM versions v
		JOIN pastes pp ON pp.slug = v.slug
		WHERE pp.identity = ? AND pp.expires_at > ? AND v.deleted = 0
	`, identity, nowStr).Scan(&pasteTotal); err != nil {
		return 0, fmt.Errorf("identity paste sum: %w", err)
	}
	var siteTotal int64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(deduped_size), 0) FROM sites WHERE identity = ? AND expires_at > ?
	`, identity, siteNowStr).Scan(&siteTotal); err != nil {
		return 0, fmt.Errorf("identity site sum: %w", err)
	}
	return pasteTotal + siteTotal, nil
}

// SumActiveBytesByOwner returns the identity's active SITE bytes only.
// The service layer adds this to the paste-side sum where it needs a
// combined figure (e.g. computing the remaining quota budget a deploy
// may use). Kept site-only here so the two repos stay independent.
func (r *SiteRepo) SumActiveBytesByOwner(owner string, now time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	var n sql.NullInt64
	if err := r.db.QueryRow(`
		SELECT COALESCE(SUM(deduped_size), 0) FROM sites WHERE identity = ? AND expires_at > ?
	`, owner, formatSiteExpiry(now)).Scan(&n); err != nil {
		return 0, fmt.Errorf("sum active site size: %w", err)
	}
	return n.Int64, nil
}
