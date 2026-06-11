// Package storage's shale-cluster-backed static-site persistence.
//
// Shale-cluster twin of slate_site_repo.go (the SlateDB-direct site repo)
// and site_repo.go (the sqlite SiteRepo). It reuses the SAME site key
// names + JSON row schema the slatedb backend uses (see slate_site_repo.go
// and docs/SPEC.md "Shale reuses the layout"); co-location across shards is
// by the ShardKeyFn (shaleShardKey in shale_shardkey.go), not by renaming
// keys. The encodeManifest/decodeManifest pair + the siteRow JSON shape are
// shared package-level with the sqlite + slatedb backends, so the on-wire
// manifest is byte-identical across all three.
//
// # Key layout (reuses the slatedb site layout; sharded per family)
//
//	sites/<slug>                       -> {slug} shard  (authoritative JSON row)
//	expiry_sites/<ts>/<slug>           -> {slug} shard  (sweep index, marker value; ts is fixed-width)
//	identity_site_bytes/<id>           -> {id}   shard  (the SITE quota counter)
//	identity_site_reserve/<id>/<slug>  -> {id}   shard  (site reservation marker)
//
// Shale keeps NO identity_sites/<id>/<slug> index (the slatedb backend has
// one, but on shale it would be write-only: the owner sum reads the
// identity_site_bytes counter, the reconciler scans sites/, and there is no
// ListSitesByOwner, so nothing would read it).
//
// # Why a dedicated site-byte counter (not the shared identity_bytes)
//
// The deploy service (service.DeploySite) computes the per-owner budget as
// `UserQuota - paste_bytes - site_bytes`, reading the paste sum and the
// site sum SEPARATELY and adding them. So the two sums MUST be disjoint:
// the paste sum from identity_bytes/<id>, the site sum from a distinct
// identity_site_bytes/<id> counter. Folding sites into identity_bytes would
// double-count (the budget would subtract the same site bytes twice, once
// via paste_bytes and once via site_bytes). The dedicated counter keeps
// SumActiveSiteBytesByOwner site-only (the conformance contract) while the
// per-owner CAP check still sums BOTH counters (paste + site) against the
// cap, exactly like the sqlite identityActiveBytes and the slatedb
// per-identity check.
//
// # Reservation pattern (strict per-owner quota, mirrors paste inserts)
//
// A site deploy spans the {id} shard (the site-byte counter) and the {slug}
// shard (the authoritative sites/<slug> row + the cross-family paste-slug
// collision read), which cannot be one CAS transaction. It is the same
// three-step reservation sequence paste inserts use:
//
//  1. reserve DedupedSize on the {id} identity_site_bytes counter (strict
//     per-owner cap, the increment + the check are one atomic CAS) and
//     stamp a site reservation marker,
//  2. authoritative write on the {slug} shard (the sites/<slug> row + the
//     expiry index, with BOTH the sites/<slug> AND pastes/<slug> collision
//     reads in the CAS read-set),
//  3. confirm on the {id} shard: drop the reservation marker (shale keeps
//     no per-identity site index, so nothing else is written).
//
// The strict-quota and sweep-time-expiry properties carry over from the
// paste counter unchanged (conformCaps.ExpiryFreesQuotaAtReadTime = false,
// StrictQuotaUnderConcurrency = true for shale). A failed authoritative
// write releases the reservation (bounded over-count + the reconciler).

//go:build slatedb

package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

// ShaleSiteRepo is the service.SiteRepo + service.SweepSites adapter over a
// ShaleRepo. It delegates the interface-named methods (Get, Delete,
// InsertWithQuotaCheck, SumActiveBytesByOwner) to the ShaleRepo `...Site`
// methods, so the shale backend's site repo shares the same cluster handle
// (and shard routing) as its paste repo. NewShaleSiteRepo(repo) builds it.
//
// The interface method names collide with the paste method names on
// ShaleRepo with different signatures, so they cannot both live on
// ShaleRepo directly; the KV operations live on ShaleRepo as `...Site`
// methods and this thin adapter exposes them under the service interface
// names. This mirrors SlateSiteRepo exactly.
type ShaleSiteRepo struct {
	repo *ShaleRepo
}

// NewShaleSiteRepo wraps a ShaleRepo so static-site hosting runs on the
// shale backend. The returned adapter satisfies service.SiteRepo and
// service.SweepSites (and so the cmd/hostthisd siteStore union).
func NewShaleSiteRepo(repo *ShaleRepo) *ShaleSiteRepo { return &ShaleSiteRepo{repo: repo} }

// service.SiteRepo
func (s *ShaleSiteRepo) InsertWithQuotaCheck(site domain.Site, dedupedSize int, serviceCap, userCap int64, now time.Time) error {
	return s.repo.InsertSiteWithQuotaCheck(site, dedupedSize, serviceCap, userCap, now)
}
func (s *ShaleSiteRepo) Get(slug domain.Slug) (domain.Site, error) { return s.repo.GetSite(slug) }
func (s *ShaleSiteRepo) SumActiveBytesByOwner(owner string, now time.Time) (int64, error) {
	return s.repo.SumActiveSiteBytesByOwner(owner, now)
}

// service.SweepSites
func (s *ShaleSiteRepo) Delete(slug domain.Slug) error { return s.repo.DeleteSite(slug) }
func (s *ShaleSiteRepo) ExpiredSiteSlugs(now time.Time) ([]string, error) {
	return s.repo.ExpiredSiteSlugs(now)
}
func (s *ShaleSiteRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	return s.repo.ReferencedSiteBlobSHAs()
}

// --- key builders (mirror the slatedb site layout) -------------------------

func shaleKeySite(slug domain.Slug) []byte { return []byte("sites/" + slug.String()) }

func shaleKeyExpirySite(t time.Time, slug domain.Slug) []byte {
	return []byte("expiry_sites/" + t.UTC().Format(expirySiteTimeFormat) + "/" + slug.String())
}

func shaleKeyIdentitySiteBytes(identity string) []byte {
	return []byte("identity_site_bytes/" + identity)
}

func shaleKeyIdentitySiteReserve(identity, slug string) []byte {
	return []byte("identity_site_reserve/" + identity + "/" + slug)
}

// --- JSON row schema (shared with the slatedb backend) ---------------------

// shaleSiteRowFromDomain builds the persisted siteRow (defined in
// slate_site_repo.go: the SAME shape both slatedb-tagged backends store) by
// encoding the manifest with the package-level encodeManifest the sqlite +
// slatedb backends use. DedupedSize is stored so the quota scans never
// decode a manifest just to sum bytes.
func shaleSiteRowFromDomain(s domain.Site, dedupedSize int) (siteRow, error) {
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

// --- site quota counter: reserve / release ---------------------------------

// reserveSiteBytes is step 1 of the site reservation pattern: a single
// {id}-shard CAS that atomically checks the per-owner cap and increments the
// SITE counter (identity_site_bytes) by `body`, plus stamps a site
// reservation marker keyed by slug. The cap check sums BOTH the paste
// counter AND the site counter (so a site deploy is rejected if the owner's
// COMBINED paste+site bytes would exceed userCap, matching the sqlite
// identityActiveBytes which spans both kinds). Only the site counter is
// incremented; the paste counter is read for the cap but never written here.
//
// The increment + the check are one atomic CAS so two concurrent reservers
// serialize (the strict ceiling). userCap == 0 means "no per-owner cap": the
// site counter is still incremented (so SumActiveSiteBytesByOwner stays
// accurate) but the check is skipped.
func (r *ShaleRepo) reserveSiteBytes(identity, slug string, body, userCap int64, now time.Time) error {
	siteCounterKey := shaleKeyIdentitySiteBytes(identity)
	pasteCounterKey := shaleKeyIdentityBytes(identity)
	reserveKey := shaleKeyIdentitySiteReserve(identity, slug)
	markerVal, err := encodeReservationMarker(body, now)
	if err != nil {
		return err
	}
	// Pin on the site counter; the paste counter co-shards on {id} (both
	// keyed by identity), so reading it inside this CAS is a same-shard read.
	return r.cluster.Transact(siteCounterKey, func(tx backend.Transaction) error {
		siteCur, err := txGetCounter(tx, siteCounterKey)
		if err != nil {
			return err
		}
		if userCap > 0 {
			pasteCur, err := txGetCounter(tx, pasteCounterKey)
			if err != nil {
				return err
			}
			if pasteCur+siteCur+body > userCap {
				return ErrOverUserQuota
			}
		}
		if err := tx.Put(siteCounterKey, formatCounter(siteCur+body)); err != nil {
			return err
		}
		return tx.Put(reserveKey, markerVal)
	})
}

// releaseSiteBytes returns `body` bytes to the site counter and drops the
// site reservation marker, in one {id}-shard CAS. Rolls back a failed
// reservation. Idempotent on a missing marker (a confirm already consumed
// it, or a prior release ran), so bytes are never double-credited.
func (r *ShaleRepo) releaseSiteBytes(identity, slug string) error {
	siteCounterKey := shaleKeyIdentitySiteBytes(identity)
	reserveKey := shaleKeyIdentitySiteReserve(identity, slug)
	return r.cluster.Transact(siteCounterKey, func(tx backend.Transaction) error {
		marker, err := tx.Get(reserveKey)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return nil // already consumed/released; do not double-credit
			}
			return err
		}
		m, err := parseReservationMarker(marker)
		if err != nil {
			return err
		}
		cur, err := txGetCounter(tx, siteCounterKey)
		if err != nil {
			return err
		}
		if err := tx.Put(siteCounterKey, formatCounter(cur-m.Bytes)); err != nil {
			return err
		}
		return tx.Delete(reserveKey)
	})
}

// --- Site KV operations (on ShaleRepo) -------------------------------------

// InsertSiteWithQuotaCheck deploys a site via the three-step reservation
// pattern (reserve on {id}, authoritative write on {slug}, confirm on {id}),
// mirroring InsertWithQuotaCheck for pastes:
//
//   - the service-wide cap is a SOFT cross-shard aggregate pre-check (the
//     same posture as the paste path), summing version bytes AND site bytes,
//   - the per-owner cap is STRICT via the reserve step's atomic CAS,
//   - the slug-collision check is BOTH directions inside the authoritative
//     CAS (reject if a site OR a paste already owns the slug).
//
// Returns nil / ErrSlugTaken / ErrServiceFull / ErrOverUserQuota.
func (r *ShaleRepo) InsertSiteWithQuotaCheck(s domain.Site, dedupedSize int, serviceCap, userCap int64, now time.Time) error {
	identity := s.Identity.String()
	slug := s.Slug.String()
	body := int64(dedupedSize)

	// Service-wide cap: best-effort cross-shard pre-check (SOFT). The shared
	// sumServiceWideActiveBytes already counts BOTH version bytes and site
	// bytes, so a site deploy sees the bytes pastes already hold and a paste
	// upload sees the bytes sites hold.
	if serviceCap > 0 {
		total, err := r.sumServiceWideActiveBytes()
		if err != nil {
			return fmt.Errorf("service-wide sum: %w", err)
		}
		if total+body > serviceCap {
			return ErrServiceFull
		}
	}

	// Step 1: reserve (STRICT per-owner quota, combined paste+site).
	if err := r.reserveSiteBytes(identity, slug, body, userCap, now); err != nil {
		return err
	}

	// Step 2: authoritative {slug}-shard write. On any failure, release the
	// reservation so the bytes are returned (over-count bounded to the
	// failure window + the reconciler).
	if err := r.insertSiteAuthoritative(s, dedupedSize); err != nil {
		_ = r.releaseSiteBytes(identity, slug)
		return err
	}

	// Step 3: confirm on the {id} shard: drop the reservation marker. The
	// site counter is NOT touched here (the reserve already accounted the
	// bytes). Shale keeps no per-identity site index, so there is nothing
	// else to write.
	if err := r.confirmSiteInsert(identity, slug); err != nil {
		// The authoritative site exists + the bytes are accounted on the
		// counter; only the reservation marker lingers. The owner sum reads
		// the counter (not any index), so this is invisible to quota; the
		// reconciler drops the leaked marker. Surface the error so the caller
		// knows confirm lagged, but the site is durable + quota-correct.
		return fmt.Errorf("confirm site insert: %w", err)
	}
	return nil
}

// insertSiteAuthoritative writes the {slug}-shard authoritative rows in one
// CAS: the sites/<slug> JSON row + the expiry_sites index. The slug-collision
// check is BOTH directions (sites/<slug> AND pastes/<slug>), and both reads
// participate in the CAS read-set so a racing insert of the same slug (as a
// site OR a paste) conflicts.
func (r *ShaleRepo) insertSiteAuthoritative(s domain.Site, dedupedSize int) error {
	row, err := shaleSiteRowFromDomain(s, dedupedSize)
	if err != nil {
		return err
	}
	siteKey := shaleKeySite(s.Slug)
	return r.cluster.Transact(siteKey, func(tx backend.Transaction) error {
		// Collision check, both directions. A found site OR paste is
		// ErrSlugTaken; the ExpectAbsent read-checks make a concurrent insert
		// of the same slug conflict.
		if _, err := tx.Get(siteKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("site slug check: %w", err)
		}
		if _, err := tx.Get(shaleKeyPaste(s.Slug)); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("paste slug check: %w", err)
		}
		if err := shaleTxPutJSON(tx, siteKey, row); err != nil {
			return err
		}
		return tx.Put(shaleKeyExpirySite(s.ExpiresAt, s.Slug), markerValue)
	})
}

// confirmSiteInsert is step 3: drop the site reservation marker on the {id}
// shard, one CAS. Shale keeps no per-identity site index
// (SumActiveSiteBytesByOwner reads the identity_site_bytes COUNTER, the
// reconciler scans sites/, and there is no ListSitesByOwner), so the marker
// is simply consumed with nothing else written. Idempotent on a missing
// marker (a prior confirm or a reconciler drop already removed it).
func (r *ShaleRepo) confirmSiteInsert(identity, slug string) error {
	reserveKey := shaleKeyIdentitySiteReserve(identity, slug)
	return r.cluster.Transact(reserveKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(reserveKey); err == nil {
			return tx.Delete(reserveKey)
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		return nil
	})
}

// GetSite returns the site for slug, or ErrNotFound. Like the sqlite + slate
// site Get it returns expired-but-unswept rows too (the HTTP layer 404s
// them, the sweep deletes them).
func (r *ShaleRepo) GetSite(slug domain.Slug) (domain.Site, error) {
	var row siteRow
	if err := r.getJSON(shaleKeySite(slug), &row); err != nil {
		return domain.Site{}, err
	}
	return row.toDomain(slug)
}

// SumActiveSiteBytesByOwner returns the identity's active SITE bytes only,
// served from the {id}-shard identity_site_bytes counter (a single read).
// The service layer adds the paste-side sum where it needs the combined
// figure. Like the shale paste counter, it has NO read-time expiry
// awareness: it sheds an expired site's bytes at sweep time (DeleteSite),
// not read time, so `now` is unused and the counter over-counts an
// expired-unswept site transiently but never under-counts
// (conformCaps.ExpiryFreesQuotaAtReadTime = false on shale).
func (r *ShaleRepo) SumActiveSiteBytesByOwner(owner string, now time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	raw, err := r.getRaw(shaleKeyIdentitySiteBytes(owner))
	if err != nil {
		return 0, err
	}
	return parseCounter(raw)
}

// sumServiceWideActiveSiteBytes sums DedupedSize across every site via a
// cross-shard aggregate over sites/. Best-effort, used for the SOFT
// service-wide cap pre-check. Counts expired-unswept sites too (no read-time
// expiry filter), matching the shale paste service-wide sum's sweep-time
// semantics; the over-count is fail-safe.
func (r *ShaleRepo) sumServiceWideActiveSiteBytes() (int64, error) {
	sites, err := r.aggregatePrefix(prefixSites)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range sites {
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		total += int64(row.DedupedSize)
	}
	return total, nil
}

// DeleteSite removes a site authoritative row + its expiry index on the
// {slug} shard, then decrements the site counter by the freed bytes on the
// {id} shard. Idempotent: a missing row is a no-op (matches the sqlite
// DELETE, the slate DeleteSite, and the paste Delete). Shale keeps no
// per-identity site index, so there is no index entry to clean up. The
// sweep calls this for every expired site slug.
func (r *ShaleRepo) DeleteSite(slug domain.Slug) error {
	var row siteRow
	if err := r.getJSON(shaleKeySite(slug), &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	identity := row.Identity
	freed := int64(row.DedupedSize)

	// Authoritative removal on the {slug} shard, one CAS.
	siteKey := shaleKeySite(slug)
	if err := r.cluster.Transact(siteKey, func(tx backend.Transaction) error {
		if err := tx.Delete(siteKey); err != nil {
			return err
		}
		return tx.Delete(shaleKeyExpirySite(row.ExpiresAt, slug))
	}); err != nil {
		return err
	}

	// Counter cleanup on the {id} shard: decrement the site counter by the
	// freed bytes, one CAS.
	counterKey := shaleKeyIdentitySiteBytes(identity)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		return tx.Put(counterKey, formatCounter(cur-freed))
	})
}

// ExpiredSiteSlugs fans out across all {slug} shards over expiry_sites/ and
// returns the slugs whose timestamp is <= now (inclusive boundary). The
// site expiry keys use the fixed-width expirySiteTimeFormat, so the
// timestamp segment's byte order is time order EXACTLY (correct even within
// a shared whole second), unlike the variable-width time.RFC3339Nano the
// paste ExpiredSlugs still uses. The cutoff is formatted with the SAME
// layout so the compare stays aligned. Matches the slate ExpiredSiteSlugs.
func (r *ShaleRepo) ExpiredSiteSlugs(now time.Time) ([]string, error) {
	items, err := r.aggregatePrefix(prefixExpirySitesAll)
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(expirySiteTimeFormat)
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

// reconcileSiteReservations completes past-grace SITE reservation markers,
// the exact parallel of releaseReservationMarkers for pastes (same grace
// rule, same orphan-release / leaked-drop split), applied to
// identity_site_reserve/ markers against the identity_site_bytes counter.
//
//   - YOUNGER than reserveGrace: in-flight deploy; skip (its bytes are
//     correctly in the site counter while the confirm is expected to land).
//   - OLDER, site ABSENT (orphan-release): the deploy was abandoned; release
//     the over-count with a targeted, read-checked {id}-shard CAS that does
//     site_counter -= marker.bytes AND deletes the marker atomically.
//   - OLDER, site PRESENT (leaked-marker drop): a confirm that failed after
//     the authoritative write left the marker behind even though the bytes
//     are already accounted; delete the marker WITHOUT touching the counter.
//
// Cross-shard via aggregate; every write is an idempotent single-{id}-shard
// CAS, so it is safe to run concurrently with live traffic (the same posture
// the paste reconciler holds).
func (r *ShaleRepo) reconcileSiteReservations(now time.Time, reserveGrace time.Duration) error {
	// Live site slugs: a marker's base slug present here means the deploy's
	// authoritative write landed (leaked-marker case), absent means it was
	// abandoned (orphan-release case).
	siteItems, err := r.aggregatePrefix(prefixSites)
	if err != nil {
		return fmt.Errorf("reconcile sites: scan sites: %w", err)
	}
	liveSiteSlugs := make(map[string]struct{}, len(siteItems))
	for _, item := range siteItems {
		liveSiteSlugs[strings.TrimPrefix(string(item.Key), "sites/")] = struct{}{}
	}

	markers, err := r.aggregatePrefix([]byte("identity_site_reserve/"))
	if err != nil {
		return fmt.Errorf("reconcile sites: scan reservations: %w", err)
	}
	for _, item := range markers {
		m, err := parseReservationMarker(item.Value)
		if err != nil {
			return fmt.Errorf("reconcile sites: %w", err)
		}
		if markerInGrace(m, now, reserveGrace) {
			continue // in-flight; leave it for the confirm step to drop
		}
		slug := siteMarkerSlugFromKey(item.Key)
		if _, present := liveSiteSlugs[slug]; present {
			// Leaked confirm-failed marker: bytes already accounted. Drop it.
			_ = r.cluster.Delete(item.Key)
			continue
		}
		// Abandoned reservation: release the over-count atomically.
		if err := r.orphanReleaseSiteMarker(item.Key); err != nil {
			return fmt.Errorf("reconcile sites: orphan-release %s: %w", item.Key, err)
		}
	}
	return nil
}

// orphanReleaseSiteMarker is the targeted, read-checked {id}-shard CAS that
// releases one abandoned SITE reservation: in a single Transact pinned on the
// site counter (which co-shards with the marker, both keyed by {id}) it reads
// the counter + the marker, and if the marker is still present subtracts the
// marker's bytes AND deletes the marker, atomically. Idempotent: a marker
// already consumed reads as absent here and the CAS is a no-op, so bytes are
// never double-credited. Mirrors orphanReleaseMarker for pastes.
func (r *ShaleRepo) orphanReleaseSiteMarker(reserveKey []byte) error {
	owner := siteMarkerOwnerFromKey(reserveKey)
	counterKey := shaleKeyIdentitySiteBytes(owner)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		raw, err := tx.Get(reserveKey)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return nil // already consumed; no-op (never double-credit)
			}
			return err
		}
		m, err := parseReservationMarker(raw)
		if err != nil {
			return err
		}
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		if err := tx.Put(counterKey, formatCounter(cur-m.Bytes)); err != nil {
			return err
		}
		return tx.Delete(reserveKey)
	})
}

// siteMarkerOwnerFromKey extracts <id> from an
// identity_site_reserve/<id>/<slug> key (everything between the prefix and
// the FIRST '/').
func siteMarkerOwnerFromKey(key []byte) string {
	rest := strings.TrimPrefix(string(key), "identity_site_reserve/")
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

// siteMarkerSlugFromKey extracts <slug> from an
// identity_site_reserve/<id>/<slug> key (everything after the FIRST '/'; an
// identity never contains a '/').
func siteMarkerSlugFromKey(key []byte) string {
	rest := strings.TrimPrefix(string(key), "identity_site_reserve/")
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return rest[idx+1:]
	}
	return ""
}

// ReferencedSiteBlobSHAs returns every distinct blob SHA referenced by any
// live site's manifest, aggregated across all {slug} shards. The sweep
// unions this with the paste-side referenced set, so a blob shared between a
// site and a paste (or two sites) survives as long as ANY live record
// references it. A site manifest references a blob unconditionally (no
// per-file tombstone), so a live site with files always contributes a
// non-empty set.
func (r *ShaleRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	sites, err := r.aggregatePrefix(prefixSites)
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
