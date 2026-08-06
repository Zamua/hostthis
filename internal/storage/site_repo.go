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

// SiteRepo is the sqlite-backed implementation of static-site persistence,
// sharing PasteRepo's db. Every site row carries an Identity and is queried by
// slug, so sites get the same owner-gating and not-found-on-cross-owner
// behavior as pastes.
type SiteRepo struct {
	db *sql.DB
}

func NewSiteRepo(db *sql.DB) *SiteRepo { return &SiteRepo{db: db} }

// sortableTimeFormat is RFC3339 with a FIXED-WIDTH, zero-padded 9-digit
// nanosecond fraction, so a lexicographic compare is byte order == time order
// exactly, including within a shared whole second. Timestamps embedded in KV
// index keys use it, since a prefix scan orders by bytes.
//
// Deliberately NOT time.RFC3339Nano, which drops trailing fractional zeros:
// under that variable-width format "...00.5Z" sorts BEFORE "...00Z" (because
// '.' < 'Z'), so a range scan would cut up to ~1s on the wrong side.
const sortableTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

func formatSortableTime(t time.Time) string { return t.UTC().Format(sortableTimeFormat) }

// manifestJSON is the on-disk shape of a Manifest. Private so the JSON
// representation stays a storage concern rather than a domain one; the domain
// Manifest is the value object. Field names are short to keep the metadata
// footprint small.
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

// InsertWithQuotaCheck checks the deploying identity against the SAME
// per-identity cap a paste upload uses, summing BOTH its active paste versions
// AND its active sites, then inserts the site row. The serializable tx is what
// stops two concurrent uploads from the same identity both passing the cap
// check and both inserting.
//
// The charged size is the caller's dedupedSize, not a figure derived from the
// manifest here: the deploy path passes Manifest.CompressedDedupedSize(), the
// distinct blobs' STORED bytes, so the charge is denominated in the same unit
// as every other quota in the system. Manifest.DedupedSize() is the
// uncompressed display figure and would over-charge.
//
// Returns:
//   - nil on success
//   - ErrSlugTaken if the slug already exists (in sites OR pastes)
//   - ErrOverUserQuota if accepting would exceed userCap
//
// The durable total-bytes ceiling is NOT checked here: it is the object-store
// bucket quota, enforced when a blob Put is rejected (SPEC "Limits -> Durable
// total-bytes ceiling"). ctx satisfies the service.SiteRepo interface (the
// shale backend carries staged blob refs on it); the sqlite path has no
// shale-blob plane and uses its own serializable-tx context.
func (r *SiteRepo) InsertWithQuotaCheck(_ context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	body := int64(dedupedSize)

	// Per-identity check across BOTH pastes and sites.
	if userCap > 0 {
		owned, err := identityActiveBytes(tx, s.Identity.String())
		if err != nil {
			return err
		}
		if err := (domain.Allowance{Cap: userCap, Used: owned}).Admit(body); err != nil {
			return err
		}
	}

	manStr, err := encodeManifest(s.Manifest)
	if err != nil {
		return err
	}

	// A slug must be unique across pastes too: a read resolves a slug in
	// either table.
	var pasteExists int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM pastes WHERE slug = ?`, s.Slug.String()).Scan(&pasteExists); err != nil {
		return fmt.Errorf("check paste slug collision: %w", err)
	}
	if pasteExists > 0 {
		return ErrSlugTaken
	}

	if _, err := tx.Exec(`
		INSERT INTO sites (slug, identity, manifest, deduped_size,
		                   created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, s.Slug.String(), s.Identity.String(), manStr, dedupedSize,
		formatTime(s.CreatedAt), formatTime(s.UpdatedAt)); err != nil {
		if isUniqueViolation(err) {
			return ErrSlugTaken
		}
		return fmt.Errorf("insert site %q: %w", s.Slug, err)
	}
	return tx.Commit()
}

// ReplaceWithQuotaCheck re-deploys an owned site in place, swapping its
// manifest, deduped size and updated_at under the serializable tx while
// enforcing the per-identity cap against the REPLACE DELTA.
//
// A slug that is missing OR owned by a different identity returns ErrNotFound,
// the SAME sentinel either way, so "not yours" is indistinguishable from
// "doesn't exist" (no existence leak).
//
// The identity-active sum computed inside the tx already includes the old row,
// so the post-swap total is owned - oldDeduped + body: a same-size re-deploy
// nets zero, a smaller one frees the difference. The durable total-bytes
// ceiling is NOT checked here (it is the object-store bucket quota).
//
// Returns:
//   - nil on success
//   - ErrNotFound if the slug isn't a site owned by s.Identity
//   - ErrOverUserQuota if accepting would exceed userCap
//
// ctx satisfies the service.SiteRepo interface; the sqlite path ignores it and
// uses its own serializable-tx context.
func (r *SiteRepo) ReplaceWithQuotaCheck(_ context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	body := int64(dedupedSize)

	// Ownership + existence gate, inside the tx so a concurrent delete or
	// re-deploy cannot race the swap.
	var ownerStr string
	var oldDeduped int64
	err = tx.QueryRow(`SELECT identity, deduped_size FROM sites WHERE slug = ?`, s.Slug.String()).
		Scan(&ownerStr, &oldDeduped)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load site for replace: %w", err)
	}
	if ownerStr != s.Identity.String() {
		return ErrNotFound
	}

	if userCap > 0 {
		owned, err := identityActiveBytes(tx, s.Identity.String())
		if err != nil {
			return err
		}
		if err := (domain.Allowance{Cap: userCap, Used: owned}).AdmitReplacing(oldDeduped, body); err != nil {
			return err
		}
	}

	manStr, err := encodeManifest(s.Manifest)
	if err != nil {
		return err
	}

	// created_at is left untouched: the slug's birth time is stable across
	// re-deploys.
	res, err := tx.Exec(`
		UPDATE sites
		SET manifest = ?, deduped_size = ?, updated_at = ?
		WHERE slug = ? AND identity = ?
	`, manStr, dedupedSize, formatTime(s.UpdatedAt),
		s.Slug.String(), s.Identity.String())
	if err != nil {
		return fmt.Errorf("replace site %q: %w", s.Slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("replace site rows affected: %w", err)
	}
	if n == 0 {
		// Row vanished between the gate and the UPDATE: unreachable under the
		// serializable tx, but fail closed as not-found.
		return ErrNotFound
	}
	return tx.Commit()
}

// Get returns the site for slug, or ErrNotFound.
func (r *SiteRepo) Get(slug domain.Slug) (domain.Site, error) {
	row := r.db.QueryRow(`
		SELECT slug, identity, manifest, deduped_size, created_at, updated_at
		FROM sites WHERE slug = ?
	`, slug.String())
	var slugStr, identStr, manStr, created, updated string
	var dedupedSize int
	if err := row.Scan(&slugStr, &identStr, &manStr, &dedupedSize, &created, &updated); err != nil {
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
		Slug:        domain.Slug(slugStr),
		Identity:    domain.Identity(identStr),
		Manifest:    man,
		StoredBytes: dedupedSize,
		CreatedAt:   parseTime(created),
		UpdatedAt:   parseTime(updated),
	}, nil
}

// Delete removes a site row. Caller is responsible for the owner check.
func (r *SiteRepo) Delete(slug domain.Slug) error {
	if _, err := r.db.Exec(`DELETE FROM sites WHERE slug = ?`, slug.String()); err != nil {
		return fmt.Errorf("delete site %q: %w", slug, err)
	}
	return nil
}

// ReferencedSiteBlobSHAs returns the set of blob SHAs referenced by any site's
// manifest. The sweep unions this with the paste-side set so a blob shared
// between a site and a paste, or between two sites, stays alive as long as ANY
// live record references it.
func (r *SiteRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	rows, err := r.db.Query(`SELECT manifest FROM sites`)
	if err != nil {
		return nil, fmt.Errorf("site manifests for gc: %w", err)
	}
	defer rows.Close() //nolint:errcheck
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

// identityActiveBytes sums active bytes owned by one identity across BOTH
// pastes and sites, inside the caller's tx.
func identityActiveBytes(tx *sql.Tx, identity string) (int64, error) {
	var pasteTotal int64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(v.size), 0)
		FROM versions v
		JOIN pastes pp ON pp.slug = v.slug
		WHERE pp.identity = ? AND v.deleted = 0
		  AND pp.status != 'failed'
	`, identity).Scan(&pasteTotal); err != nil {
		return 0, fmt.Errorf("identity paste sum: %w", err)
	}
	var siteTotal int64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(deduped_size), 0) FROM sites WHERE identity = ?
	`, identity).Scan(&siteTotal); err != nil {
		return 0, fmt.Errorf("identity site sum: %w", err)
	}
	return pasteTotal + siteTotal, nil
}

// SumActiveBytesByOwner returns the identity's active SITE bytes only. The
// service layer adds this to the paste-side sum where it needs the combined
// figure; site-only here keeps the two repos independent.
func (r *SiteRepo) SumActiveBytesByOwner(owner string, _ time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	var n sql.NullInt64
	if err := r.db.QueryRow(`
		SELECT COALESCE(SUM(deduped_size), 0) FROM sites WHERE identity = ?
	`, owner).Scan(&n); err != nil {
		return 0, fmt.Errorf("sum active site size: %w", err)
	}
	return n.Int64, nil
}

// ListSitesByOwner returns the identity's sites so the SSH `list` verb can show
// them alongside text pastes.
func (r *SiteRepo) ListSitesByOwner(owner string, _ time.Time) ([]domain.Site, error) {
	if owner == "" {
		return nil, nil
	}
	rows, err := r.db.Query(`
		SELECT slug, identity, manifest, deduped_size, created_at, updated_at
		FROM sites WHERE identity = ?
	`, owner)
	if err != nil {
		return nil, fmt.Errorf("list sites by owner: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Site
	for rows.Next() {
		var slugStr, identStr, manStr, created, updated string
		var dedupedSize int
		if err := rows.Scan(&slugStr, &identStr, &manStr, &dedupedSize, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan site row: %w", err)
		}
		man, err := decodeManifest(manStr)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.Site{
			Slug:        domain.Slug(slugStr),
			Identity:    domain.Identity(identStr),
			Manifest:    man,
			StoredBytes: dedupedSize,
			CreatedAt:   parseTime(created),
			UpdatedAt:   parseTime(updated),
		})
	}
	return out, rows.Err()
}

// PreClaimSlug is a NO-OP on the sqlite backend: its blobs are
// content-sha-keyed in a detached store, so a deploy's files do not route by
// slug and the slug is minted in the post-untar insert retry loop, where
// InsertWithQuotaCheck's collision check is the authority. Only the
// transactional shale path pre-claims, so its files stage under the right
// shard. The arguments satisfy the service.SiteRepo seam.
func (r *SiteRepo) PreClaimSlug(_ context.Context, _ domain.Slug, _ string, _ time.Time) error {
	return nil
}
