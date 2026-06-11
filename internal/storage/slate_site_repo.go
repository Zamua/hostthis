// Package storage's SlateDB-backed static-site persistence.
//
// SlateDB-backed twin of site_repo.go (the sqlite SiteRepo). Adds the
// site key families to the SAME SlateDB instance the paste keys live in,
// so a site insert + its indexes commit in one transaction and the
// service-wide quota sum (sumServiceWideActiveBytes in slate_repo.go)
// can include site bytes alongside paste bytes.
//
// The site interface method names (Get, Delete, SumActiveBytesByOwner,
// InsertWithQuotaCheck) collide with the paste method names on SlateRepo
// with different signatures, so they cannot both live on SlateRepo. The
// KV operations live on SlateRepo as `...Site` methods (sharing db,
// lockQuota, and the service-wide sum), and a thin SlateSiteRepo adapter
// exposes them under the service.SiteRepo + service.SweepSites interface
// names by delegating. NewSlateSiteRepo(repo) builds the adapter.
//
// Canonical layout in docs/SPEC.md "Static-site storage on the slatedb
// (and shale) backend".
//
// # Key layout
//
// Mirrors the paste families (sites/<slug> ~ pastes/<slug>, etc.):
//
//	sites/<slug>                       JSON {Identity, Manifest, DedupedSize, CreatedAt, UpdatedAt, ExpiresAt}
//	identity_sites/<identity>/<slug>   empty value (list/sum sites by identity)
//	expiry_sites/<rfc3339>/<slug>      empty value (sweep prefix scan)
//
// The Manifest is encoded with the SAME encodeManifest/decodeManifest the
// sqlite backend uses (package-level in site_repo.go), so the on-wire
// manifest shape is identical across backends. DedupedSize is stored on
// the row so the quota scans never decode a manifest just to sum bytes.

//go:build slatedb

package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	slatedb "slatedb.io/slatedb-go/uniffi"

	"github.com/Zamua/hostthis/internal/domain"
)

// SlateSiteRepo is the service.SiteRepo + service.SweepSites adapter over
// a SlateRepo. It delegates the interface-named methods to the SlateRepo
// `...Site` methods, so the slatedb backend's site repo shares the same
// SlateDB instance (and quota accounting) as its paste repo.
type SlateSiteRepo struct {
	repo *SlateRepo
}

// NewSlateSiteRepo wraps a SlateRepo so static-site hosting runs on the
// slatedb backend. The returned adapter satisfies service.SiteRepo and
// service.SweepSites.
func NewSlateSiteRepo(repo *SlateRepo) *SlateSiteRepo { return &SlateSiteRepo{repo: repo} }

// service.SiteRepo
func (s *SlateSiteRepo) InsertWithQuotaCheck(site domain.Site, dedupedSize int, serviceCap, userCap int64, now time.Time) error {
	return s.repo.InsertSiteWithQuotaCheck(site, dedupedSize, serviceCap, userCap, now)
}
func (s *SlateSiteRepo) Get(slug domain.Slug) (domain.Site, error) { return s.repo.GetSite(slug) }
func (s *SlateSiteRepo) SumActiveBytesByOwner(owner string, now time.Time) (int64, error) {
	return s.repo.SumActiveSiteBytesByOwner(owner, now)
}

// service.SweepSites
func (s *SlateSiteRepo) Delete(slug domain.Slug) error { return s.repo.DeleteSite(slug) }
func (s *SlateSiteRepo) ExpiredSiteSlugs(now time.Time) ([]string, error) {
	return s.repo.ExpiredSiteSlugs(now)
}
func (s *SlateSiteRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	return s.repo.ReferencedSiteBlobSHAs()
}

// --- JSON row schema -------------------------------------------------------

// siteRow is the persisted shape of a Site. Manifest holds the exact
// string encodeManifest produces (the same compact JSON the sqlite backend
// stores), so the slatedb and sqlite backends serialize the manifest
// identically (path -> sha + size + content-type).
type siteRow struct {
	Identity    string    `json:"identity"`
	Manifest    string    `json:"manifest"`
	DedupedSize int       `json:"deduped_size"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   time.Time `json:"expires_at"`
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
	return []byte("expiry_sites/" + t.UTC().Format(time.RFC3339Nano) + "/" + slug.String())
}

func prefixExpirySites() []byte { return []byte("expiry_sites/") }

// --- Site KV operations (on SlateRepo) -------------------------------------

// InsertSiteWithQuotaCheck atomically checks the deploying identity's quota
// (per-identity AND service-wide, BOTH counting site bytes alongside paste
// bytes), rejects a slug already taken by a paste OR a site, and writes the
// site row + its two index entries in one transaction.
//
// The deduped size charged is dedupedSize (distinct blobs only), the same
// figure the sqlite backend charges. The per-identity quota stripe is held
// across the sum + the write so two concurrent same-identity deploys cannot
// both pass the cap (mirrors SlateRepo.InsertWithQuotaCheck for pastes).
//
// Returns:
//   - nil on success
//   - ErrSlugTaken if the slug already exists (in sites OR pastes)
//   - ErrServiceFull if accepting would exceed serviceCap
//   - ErrOverUserQuota if accepting would exceed userCap
func (r *SlateRepo) InsertSiteWithQuotaCheck(s domain.Site, dedupedSize int, serviceCap, userCap int64, now time.Time) error {
	defer r.lockQuota(s.Identity.String())()
	body := int64(dedupedSize)

	if serviceCap > 0 {
		total, err := r.sumServiceWideActiveBytes(now)
		if err != nil {
			return fmt.Errorf("service-wide sum: %w", err)
		}
		if total+body > serviceCap {
			return ErrServiceFull
		}
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
		if ownerPaste+ownerSite+body > userCap {
			return ErrOverUserQuota
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

	// Slug-collision check, BOTH directions: reject if a site OR a paste
	// already owns the slug. Both reads participate in SI conflict
	// detection.
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

// GetSite returns the site for slug, or ErrNotFound. Like the sqlite
// SiteRepo it returns expired-but-unswept rows too (the HTTP layer 404s
// them, the sweep deletes them).
func (r *SlateRepo) GetSite(slug domain.Slug) (domain.Site, error) {
	var row siteRow
	if err := r.getJSON(keySite(slug), &row); err != nil {
		return domain.Site{}, err
	}
	return row.toDomain(slug)
}

// SumActiveSiteBytesByOwner returns the identity's active SITE bytes only
// (DedupedSize of non-expired sites). The service layer adds the
// paste-side sum where it needs the combined figure, matching
// SiteRepo.SumActiveBytesByOwner on sqlite.
func (r *SlateRepo) SumActiveSiteBytesByOwner(owner string, now time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	return r.sumActiveSiteBytesForOwner(owner, now)
}

// sumActiveSiteBytesForOwner walks identity_sites/<owner>/ and sums
// DedupedSize of the rows whose ExpiresAt > now. Read-time expiry filter,
// so an expired-unswept site stops counting the instant it expires
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
		if !row.ExpiresAt.After(now) {
			continue
		}
		total += int64(row.DedupedSize)
	}
	return total, nil
}

// sumServiceWideActiveSiteBytes sums DedupedSize across every non-expired
// site. Called by sumServiceWideActiveBytes (slate_repo.go) so the
// service-wide cap counts site bytes alongside paste bytes.
func (r *SlateRepo) sumServiceWideActiveSiteBytes(now time.Time) (int64, error) {
	sites, err := r.scanPrefix([]byte("sites/"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range sites {
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if !row.ExpiresAt.After(now) {
			continue
		}
		total += int64(row.DedupedSize)
	}
	return total, nil
}

// DeleteSite removes a site row and its two index entries. Idempotent: a
// missing row is a no-op (matches the sqlite DELETE and the paste Delete).
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

// ExpiredSiteSlugs returns the slugs of sites whose ExpiresAt is at or
// before now. Inclusive boundary, RFC3339Nano lex order = time order
// (the same shape as the paste ExpiredSlugs).
func (r *SlateRepo) ExpiredSiteSlugs(now time.Time) ([]string, error) {
	items, err := r.scanPrefix(prefixExpirySites())
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(time.RFC3339Nano)
	var out []string
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), "expiry_sites/")
		idx := strings.LastIndex(rest, "/")
		if idx < 0 {
			continue
		}
		ts := rest[:idx]
		slug := rest[idx+1:]
		if ts <= cutoff {
			out = append(out, slug)
		}
	}
	return out, nil
}

// ReferencedSiteBlobSHAs returns every distinct blob SHA referenced by any
// live site's manifest. The sweep unions this with the paste-side
// referenced set, so a blob shared between a site and a paste (or two
// sites) survives as long as ANY live record references it. A site
// manifest references a blob unconditionally (no per-file tombstone), so a
// live site with files always contributes a non-empty set.
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
