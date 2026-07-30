// SlateDB-backed static-site persistence: the twin of site_repo.go (the sqlite
// SiteRepo). The site key families live in the SAME SlateDB instance as the
// paste keys, so a site insert + its indexes commit in one transaction.
//
// The site interface method names (Get, Delete, SumActiveBytesByOwner,
// InsertWithQuotaCheck) collide with the paste method names on SlateRepo at
// different signatures, so they cannot both live on SlateRepo. The KV
// operations live on SlateRepo as `...Site` methods (sharing db and lockQuota)
// and SlateSiteRepo re-exposes them under the service.SiteRepo +
// service.SweepSites names by delegating.
//
// Canonical layout in docs/SPEC.md "Static-site storage on the slatedb
// (and shale) backend".
//
// # Key layout
//
//	sites/<slug>                       JSON {Identity, Manifest, DedupedSize, CreatedAt, UpdatedAt, ExpiresAt}
//	identity_sites/<identity>/<slug>   empty value (list/sum sites by identity)
//	expiry_sites/<rfc3339>/<slug>      empty value (sweep prefix scan)
//
// Manifests use the same encodeManifest/decodeManifest as the sqlite backend,
// so the on-wire manifest shape is identical across backends. DedupedSize is
// stored on the row so quota scans never decode a manifest just to sum bytes.

//go:build slatedb

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	slatedb "slatedb.io/slatedb-go/uniffi"

	"github.com/Zamua/hostthis/internal/domain"
)

// SlateSiteRepo is the service.SiteRepo + service.SweepSites adapter over a
// SlateRepo, delegating to its `...Site` methods so the site repo shares one
// SlateDB instance (and quota accounting) with the paste repo.
type SlateSiteRepo struct {
	repo *SlateRepo
}

// NewSlateSiteRepo adapts a SlateRepo to service.SiteRepo + service.SweepSites.
func NewSlateSiteRepo(repo *SlateRepo) *SlateSiteRepo { return &SlateSiteRepo{repo: repo} }

// service.SiteRepo
//
// ctx exists for the interface (the shale backend carries staged blob refs on
// it); the direct slate path has no shale-blob plane and ignores it.
func (s *SlateSiteRepo) InsertWithQuotaCheck(_ context.Context, site domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	return s.repo.InsertSiteWithQuotaCheck(site, dedupedSize, userCap, now)
}
func (s *SlateSiteRepo) ReplaceWithQuotaCheck(_ context.Context, site domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	return s.repo.ReplaceSiteWithQuotaCheck(site, dedupedSize, userCap, now)
}
func (s *SlateSiteRepo) Get(slug domain.Slug) (domain.Site, error) { return s.repo.GetSite(slug) }
func (s *SlateSiteRepo) SumActiveBytesByOwner(owner string, now time.Time) (int64, error) {
	return s.repo.SumActiveSiteBytesByOwner(owner, now)
}
func (s *SlateSiteRepo) ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error) {
	return s.repo.ListSitesByOwner(owner, now)
}

// PreClaimSlug is a NO-OP on the direct slatedb backend: its blobs live in a
// detached content-sha-keyed store, so a deploy's files do not route by slug
// and the slug is minted in the post-untar insert retry loop. Only the
// transactional shale-collocated path pre-claims, so its files stage under the
// manifest's shard.
func (s *SlateSiteRepo) PreClaimSlug(_ context.Context, _ domain.Slug, _ string, _ time.Time) error {
	return nil
}

// service.SweepSites (Delete also serves the owner-facing removal path)
func (s *SlateSiteRepo) Delete(slug domain.Slug) error { return s.repo.DeleteSite(slug) }
func (s *SlateSiteRepo) ExpiredSites(now time.Time) ([]domain.ExpiredSite, error) {
	return s.repo.ExpiredSites(now)
}
func (s *SlateSiteRepo) DeleteExpiredSite(ref domain.ExpiredSite) (bool, error) {
	return s.repo.DeleteExpiredSite(ref)
}
func (s *SlateSiteRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	return s.repo.ReferencedSiteBlobSHAs()
}

// --- JSON row schema -------------------------------------------------------

// siteRow is the persisted shape of a Site. Manifest holds the exact string
// encodeManifest produces, the same compact JSON the sqlite backend stores.
type siteRow struct {
	Identity    string    `json:"identity"`
	Manifest    string    `json:"manifest"`
	DedupedSize int       `json:"deduped_size"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   time.Time `json:"expires_at"`

	// FileBlobs maps a file's content sha to the shale-blob id its bytes were
	// staged under. The manifest references files by sha, so this side-table is
	// how the read path resolves sha -> blobid for GetBlob. Empty on the
	// standalone paths (sqlite / slatedb / disk), where a file is
	// content-addressed by sha alone.
	FileBlobs map[string]string `json:"file_blobs,omitempty"`
}

func (r *SlateRepo) siteRowFromDomain(s domain.Site, dedupedSize int) (siteRow, error) {
	manStr, err := encodeManifest(s.Manifest)
	if err != nil {
		return siteRow{}, err
	}
	return siteRow{
		Identity:    s.Identity.String(),
		Manifest:    manStr,
		DedupedSize: dedupedSize,
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
		ExpiresAt:   s.ExpiresAt,
	}, nil
}

func (row siteRow) toDomain(slug domain.Slug) (domain.Site, error) {
	man, err := decodeManifest(row.Manifest)
	if err != nil {
		return domain.Site{}, err
	}
	return domain.Site{
		Slug:      slug,
		Identity:  domain.Identity(row.Identity),
		Manifest:  man,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		ExpiresAt: row.ExpiresAt,
	}, nil
}

// --- Key builders ----------------------------------------------------------

func keySite(slug domain.Slug) []byte { return []byte("sites/" + slug.String()) }

func keyIdentitySite(identity, slug string) []byte {
	return []byte("identity_sites/" + identity + "/" + slug)
}

func prefixIdentitySites(identity string) []byte {
	return []byte("identity_sites/" + identity + "/")
}

func keyExpirySite(t time.Time, slug domain.Slug) []byte {
	return []byte("expiry_sites/" + t.UTC().Format(expirySiteTimeFormat) + "/" + slug.String())
}

func prefixExpirySites() []byte { return []byte("expiry_sites/") }

// --- Site KV operations (on SlateRepo) -------------------------------------

// InsertSiteWithQuotaCheck checks the deploying identity's quota (counting site
// bytes alongside paste bytes), rejects a slug already taken by a paste OR a
// site, and writes the site row + its two index entries in one transaction.
//
// Charged bytes are dedupedSize (distinct blobs only), matching sqlite. The
// per-identity quota stripe is held across the sum + the write so two
// concurrent same-identity deploys cannot both pass the cap. The durable
// total-bytes ceiling is NOT checked here: it is the object-store bucket
// quota, enforced when a blob Put is rejected (SPEC "Limits -> Durable
// total-bytes ceiling: an object-store quota").
//
// Returns:
//   - nil on success
//   - ErrSlugTaken if the slug already exists (in sites OR pastes)
//   - ErrOverUserQuota if accepting would exceed userCap
func (r *SlateRepo) InsertSiteWithQuotaCheck(s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	defer r.lockQuota(s.Identity.String())()
	body := int64(dedupedSize)

	if userCap > 0 {
		ownerPaste, err := r.sumActiveBytesForOwner(s.Identity.String(), now)
		if err != nil {
			return fmt.Errorf("identity paste sum: %w", err)
		}
		ownerSite, err := r.sumActiveSiteBytesForOwner(s.Identity.String(), now)
		if err != nil {
			return fmt.Errorf("identity site sum: %w", err)
		}
		if err := (domain.Allowance{Cap: userCap, Used: ownerPaste + ownerSite}).Admit(body); err != nil {
			return err
		}
	}

	row, err := r.siteRowFromDomain(s, dedupedSize)
	if err != nil {
		return err
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Reject if a site OR a paste already owns the slug. Both reads participate
	// in SI conflict detection.
	existingSite, err := tx.Get(keySite(s.Slug))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tx site slug check: %w", err)
	}
	if existingSite != nil {
		_ = tx.Rollback()
		return ErrSlugTaken
	}
	existingPaste, err := tx.Get(keyPaste(s.Slug))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tx paste slug check: %w", err)
	}
	if existingPaste != nil {
		_ = tx.Rollback()
		return ErrSlugTaken
	}

	if err := txPutJSON(tx, keySite(s.Slug), row); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Put(keyIdentitySite(s.Identity.String(), s.Slug.String()), []byte{}); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put identity-site index: %w", err)
	}
	if err := tx.Put(keyExpirySite(s.ExpiresAt, s.Slug), []byte{}); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put site expiry index: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit site insert %q: %w", s.Slug, err)
	}
	return nil
}

// ReplaceSiteWithQuotaCheck re-deploys an existing OWNED site in place,
// swapping its row and re-keying its expiry index, enforcing the per-identity
// cap against the REPLACE DELTA.
//
// A missing row OR a foreign-owned row both collapse to ErrNotFound (the SAME
// sentinel a missing slug yields), so existence and ownership never leak.
//
// Quota: the per-identity sum already includes the old (live) row, so the
// post-swap total is (owned - oldDeduped + body). The durable total-bytes
// ceiling is NOT checked here (it is the object-store bucket quota).
//
// Concurrency: the per-identity quota stripe is held across the sum + the
// swap, and the row read + the writes run in one snapshot-isolation tx whose
// read of sites/<slug> participates in SI conflict detection, so a racing
// re-deploy / delete of the same slug conflicts.
//
// Returns:
//   - nil on success
//   - ErrNotFound if the slug isn't a site owned by s.Identity
//   - ErrOverUserQuota if accepting would exceed userCap
func (r *SlateRepo) ReplaceSiteWithQuotaCheck(s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	defer r.lockQuota(s.Identity.String())()
	body := int64(dedupedSize)

	// Ownership + existence gate, outside the tx but under the quota lock.
	var existing siteRow
	if err := r.getJSON(keySite(s.Slug), &existing); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if existing.Identity != s.Identity.String() {
		return ErrNotFound
	}
	// Credit the old bytes back ONLY if the old row is still live: the sums
	// below filter on expiry, so an expired-but-unswept row is not in them and
	// crediting it would under-count and admit an over-quota re-deploy.
	creditOld := int64(0)
	if existing.ExpiresAt.After(now) {
		creditOld = int64(existing.DedupedSize)
	}

	if userCap > 0 {
		ownerPaste, err := r.sumActiveBytesForOwner(s.Identity.String(), now)
		if err != nil {
			return fmt.Errorf("identity paste sum: %w", err)
		}
		ownerSite, err := r.sumActiveSiteBytesForOwner(s.Identity.String(), now)
		if err != nil {
			return fmt.Errorf("identity site sum: %w", err)
		}
		if err := (domain.Allowance{Cap: userCap, Used: ownerPaste + ownerSite}).AdmitReplacing(creditOld, body); err != nil {
			return err
		}
	}

	row, err := r.siteRowFromDomain(s, dedupedSize)
	if err != nil {
		return err
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Re-read inside the tx so presence + ownership participate in SI conflict
	// detection: a concurrent delete / re-deploy conflicts.
	var inTx siteRow
	if err := txGetJSON(tx, keySite(s.Slug), &inTx); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if inTx.Identity != s.Identity.String() {
		_ = tx.Rollback()
		return ErrNotFound
	}
	// created_at is the slug's birth time, immutable across a re-deploy. Pinned
	// from the existing row so a caller cannot move it via the new row.
	row.CreatedAt = inTx.CreatedAt

	// The identity-site index is keyed by (identity, slug), both unchanged, so
	// it needs no rewrite.
	if err := txPutJSON(tx, keySite(s.Slug), row); err != nil {
		_ = tx.Rollback()
		return err
	}
	// Re-key the expiry index so the sweep sees the restarted retention clock.
	if !inTx.ExpiresAt.Equal(s.ExpiresAt) {
		if err := tx.Delete(keyExpirySite(inTx.ExpiresAt, s.Slug)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete old site expiry index: %w", err)
		}
	}
	if err := tx.Put(keyExpirySite(s.ExpiresAt, s.Slug), []byte{}); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put site expiry index: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit site replace %q: %w", s.Slug, err)
	}
	return nil
}

// GetSite returns the site for slug, or ErrNotFound. Expired-but-unswept rows
// are returned too: the HTTP layer 404s them, the sweep deletes them.
func (r *SlateRepo) GetSite(slug domain.Slug) (domain.Site, error) {
	var row siteRow
	if err := r.getJSON(keySite(slug), &row); err != nil {
		return domain.Site{}, err
	}
	return row.toDomain(slug)
}

// SumActiveSiteBytesByOwner returns the identity's active SITE bytes only. The
// service layer adds the paste-side sum where it needs the combined figure.
func (r *SlateRepo) SumActiveSiteBytesByOwner(owner string, now time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	return r.sumActiveSiteBytesForOwner(owner, now)
}

// sumActiveSiteBytesForOwner walks identity_sites/<owner>/ and sums DedupedSize
// of rows whose ExpiresAt > now. The expiry filter is at READ time, so an
// expired-unswept site stops counting the instant it expires
// (conformCaps.ExpiryFreesQuotaAtReadTime = true on slatedb).
func (r *SlateRepo) sumActiveSiteBytesForOwner(owner string, now time.Time) (int64, error) {
	idx, err := r.scanPrefix(prefixIdentitySites(owner))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range idx {
		slug := domain.Slug(extractSlug(item.Key))
		var row siteRow
		if err := r.getJSON(keySite(slug), &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // stale index entry
			}
			return 0, err
		}
		if domain.IsExpired(row.ExpiresAt, now) {
			continue
		}
		total += int64(row.DedupedSize)
	}
	return total, nil
}

// ListSitesByOwner returns the active (non-expired) sites for owner, re-reading
// each authoritative sites/<slug> row. Same scan and read-time expiry filter as
// sumActiveSiteBytesForOwner; a stale index entry whose row is gone is skipped.
func (r *SlateRepo) ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error) {
	if owner == "" {
		return nil, nil
	}
	idx, err := r.scanPrefix(prefixIdentitySites(owner))
	if err != nil {
		return nil, err
	}
	out := make([]domain.Site, 0, len(idx))
	for _, item := range idx {
		slug := domain.Slug(extractSlug(item.Key))
		var row siteRow
		if err := r.getJSON(keySite(slug), &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // stale index entry
			}
			return nil, err
		}
		if domain.IsExpired(row.ExpiresAt, now) {
			continue
		}
		site, err := row.toDomain(slug)
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	return out, nil
}

// DeleteSite removes a site row and its two index entries. Idempotent: a
// missing row is a no-op.
func (r *SlateRepo) DeleteSite(slug domain.Slug) error {
	var row siteRow
	if err := r.getJSON(keySite(slug), &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := tx.Delete(keySite(slug)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete site: %w", err)
	}
	if err := tx.Delete(keyIdentitySite(row.Identity, slug.String())); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete identity-site index: %w", err)
	}
	if err := tx.Delete(keyExpirySite(row.ExpiresAt, slug)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete site expiry index: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit site delete %q: %w", slug, err)
	}
	return nil
}

// ExpiredSites returns one reference per site whose ExpiresAt is at or before
// now (inclusive): the slug plus the entry's full key as the opaque IndexRef,
// so DeleteExpiredSite can remove the EXACT entry the scan surfaced even when
// the site record is already gone. Site expiry keys use the fixed-width
// expirySiteTimeFormat, so byte order is time order exactly and a string
// compare is correct even within a shared whole second. The cutoff is formatted
// with the SAME layout to keep the compare aligned.
func (r *SlateRepo) ExpiredSites(now time.Time) ([]domain.ExpiredSite, error) {
	return scanExpiredRefs(r.scanPrefix, prefixExpirySites(), now, expirySiteTimeFormat, parseExpiredSiteKey)
}

// DeleteExpiredSite processes one expired reference: the full DeleteSite
// cascade when the record still exists, and in every case removal of the exact
// expiry-index entry the scan surfaced (the cascade removes the DERIVED key,
// this the OBSERVED one). Idempotent, and reports whether a record was
// actually deleted. See docs/SPEC.md "Static-site storage" (sweep path).
func (r *SlateRepo) DeleteExpiredSite(ref domain.ExpiredSite) (bool, error) {
	var row siteRow
	return deleteExpiredRef(ref, expirySiteIndexKey,
		func() error { return r.getJSON(keySite(ref.Slug), &row) },
		func() error { return r.DeleteSite(ref.Slug) },
		func(entryKey []byte) error { return r.deleteExpiryEntry(entryKey, "site expiry entry") })
}

// ReferencedSiteBlobSHAs returns every distinct blob SHA referenced by any live
// site's manifest. The sweep unions this with the paste-side set, so a blob
// shared between records survives as long as ANY live record references it.
func (r *SlateRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	sites, err := r.scanPrefix([]byte("sites/"))
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(sites))
	for _, item := range sites {
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		man, err := decodeManifest(row.Manifest)
		if err != nil {
			return nil, err
		}
		for _, sha := range man.SHASet() {
			seen[sha] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for sha := range seen {
		out = append(out, sha)
	}
	return out, nil
}
