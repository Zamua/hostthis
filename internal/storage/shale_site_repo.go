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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/cluster"

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
func (s *ShaleSiteRepo) InsertWithQuotaCheck(ctx context.Context, site domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	return s.repo.InsertSiteWithQuotaCheck(ctx, site, dedupedSize, userCap, now)
}
func (s *ShaleSiteRepo) ReplaceWithQuotaCheck(ctx context.Context, site domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	return s.repo.ReplaceSiteWithQuotaCheck(ctx, site, dedupedSize, userCap, now)
}
func (s *ShaleSiteRepo) Get(slug domain.Slug) (domain.Site, error) { return s.repo.GetSite(slug) }
func (s *ShaleSiteRepo) SumActiveBytesByOwner(owner string, now time.Time) (int64, error) {
	return s.repo.SumActiveSiteBytesByOwner(owner, now)
}
func (s *ShaleSiteRepo) ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error) {
	return s.repo.ListSitesByOwner(owner, now)
}
func (s *ShaleSiteRepo) PreClaimSlug(ctx context.Context, slug domain.Slug, owner string, now time.Time) error {
	return s.repo.PreClaimSiteSlug(ctx, slug, owner, now)
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

// shaleKeyIdentitySiteRelease is the delete-side mirror of the reserve marker
// key. It co-shards on <id> with the site counter (identity_site_bytes) so the
// delete's decrement and the marker's deletion are ONE atomic single-shard CAS
// (the marker-consume). The value reuses the reservationMarker encoding
// {bytes, created_at}; the prefix is DISTINCT from identity_site_reserve/ so
// the reserve reconciler and the release reconciler never cross-contaminate.
func shaleKeyIdentitySiteRelease(identity, slug string) []byte {
	return []byte("identity_site_release/" + identity + "/" + slug)
}

// shaleKeyIdentitySite / shalePrefixIdentitySites are the per-owner site
// ENUMERATION index (mirror shaleKeyIdentityPaste / shalePrefixIdentityPastes).
// The entry carries a one-byte marker value (shale's Put rejects an empty
// value): ListSitesByOwner re-reads the authoritative sites/<slug> row for
// the returned fields, so the index only needs to enumerate the owner's
// slugs. It co-shards on <id> with the site byte counter + reserve marker.
func shaleKeyIdentitySite(identity, slug string) []byte {
	return []byte("identity_sites/" + identity + "/" + slug)
}

func shalePrefixIdentitySites(identity string) []byte {
	return []byte("identity_sites/" + identity + "/")
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

// reserveSiteReplaceBytes is the replace-path twin of reserveSiteBytes: a
// single {id}-shard CAS that atomically checks the per-owner cap and adjusts
// the SITE counter by the REPLACE DELTA (newBody - oldBody), plus stamps a
// site reservation marker recording that delta. Because the old row's bytes
// are ALREADY in the site counter, applying the delta moves the counter to
// the post-replace total in one step.
//
// The cap check sums BOTH the paste counter AND the post-delta site counter
// (pasteCur + siteCur - oldBody + newBody), so a re-deploy is rejected only
// if the owner's COMBINED post-swap bytes would exceed userCap. A same-size
// re-deploy nets zero against the cap; a smaller one frees the difference.
// The check + the adjust are one atomic CAS so two concurrent reservers
// serialize (the strict ceiling carries over from the insert path).
//
// The marker records the delta (which may be negative), so a rollback
// (releaseSiteBytes) and the reconciler's orphan-release both subtract the
// SAME delta to undo the adjustment exactly. userCap == 0 means "no per-owner
// cap": the counter is still adjusted (so SumActiveSiteBytesByOwner stays
// accurate) but the check is skipped.
func (r *ShaleRepo) reserveSiteReplaceBytes(identity, slug string, oldBody, newBody, userCap int64, now time.Time) error {
	siteCounterKey := shaleKeyIdentitySiteBytes(identity)
	pasteCounterKey := shaleKeyIdentityBytes(identity)
	reserveKey := shaleKeyIdentitySiteReserve(identity, slug)
	delta := newBody - oldBody
	markerVal, err := encodeReservationMarker(delta, now)
	if err != nil {
		return err
	}
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
			if pasteCur+siteCur+delta > userCap {
				return ErrOverUserQuota
			}
		}
		if err := tx.Put(siteCounterKey, formatCounter(siteCur+delta)); err != nil {
			return err
		}
		return tx.Put(reserveKey, markerVal)
	})
}

// --- Site KV operations (on ShaleRepo) -------------------------------------

// InsertSiteWithQuotaCheck deploys a site via the three-step reservation
// pattern (reserve on {id}, authoritative write on {slug}, confirm on {id}),
// mirroring InsertWithQuotaCheck for pastes:
//
//   - the per-owner cap is STRICT via the reserve step's atomic CAS,
//   - the slug-collision check is BOTH directions inside the authoritative
//     CAS (reject if a site OR a paste already owns the slug).
//
// The durable total-bytes ceiling is NOT checked here: it is the
// object-store bucket quota, enforced when a blob Put is rejected (see SPEC
// "Limits -> Durable total-bytes ceiling: an object-store quota").
//
// Returns nil / ErrSlugTaken / ErrOverUserQuota.
func (r *ShaleRepo) InsertSiteWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	identity := s.Identity.String()
	slug := s.Slug.String()
	body := int64(dedupedSize)

	// The deploy's staged file refs (if any) ride this call's context, isolated
	// from any concurrent same-slug write. Read once and pass them down so the
	// authoritative {slug} transaction binds exactly this call's blobs.
	binds := pendingBindsFromContext(ctx)

	// Step 1: reserve (STRICT per-owner quota, combined paste+site).
	if err := r.reserveSiteBytes(identity, slug, body, userCap, now); err != nil {
		return err
	}

	// Step 2: authoritative {slug}-shard write. On any failure, release the
	// reservation so the bytes are returned (over-count bounded to the
	// failure window + the reconciler).
	if err := r.insertSiteAuthoritative(s, dedupedSize, binds); err != nil {
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

// ReplaceSiteWithQuotaCheck re-deploys an existing OWNED site in place via a
// three-step sequence that mirrors InsertSiteWithQuotaCheck, charging the
// REPLACE DELTA rather than the full new size:
//
//  1. reserve the delta (newBody - oldBody) on the {id} site counter
//     (STRICT per-owner cap, the adjust + the combined paste+site check are
//     one atomic CAS) and stamp a site reservation marker recording the
//     delta,
//  2. authoritative swap on the {slug} shard (re-read sites/<slug> in the
//     CAS read-set for ownership + the old expiry key, overwrite the row,
//     re-key the expiry index),
//  3. confirm on the {id} shard: drop the reservation marker.
//
// Ownership/existence: the slug must already be a site owned by s.Identity.
// A missing row OR a foreign-owned row both collapse to ErrNotFound (the SAME
// sentinel a missing slug yields), so existence/ownership never leaks. The
// gate is enforced TWICE: an up-front Get to size the delta (read oldBody +
// reject a foreign/missing row before touching the counter), and again INSIDE
// the authoritative CAS so a concurrent delete/re-deploy conflicts.
//
// The durable total-bytes ceiling is NOT checked here: it is the
// object-store bucket quota, enforced when a blob Put is rejected (see SPEC
// "Limits -> Durable total-bytes ceiling: an object-store quota").
//
// Reconciler interplay: a replace's authoritative row is ALWAYS present (it
// is a pre-existing site), so a confirm that fails leaves a marker the
// reconciler classifies as a leaked-marker drop (delete the marker, do NOT
// touch the counter) - correct, because the delta was applied during reserve
// and the swap succeeded. If instead the authoritative write FAILS, step 2's
// caller releases the reservation (subtracting the delta back); only a crash
// between the failed write and that release leaves the counter off by the
// delta (a bounded over/under-count the reconciler cannot distinguish from a
// landed replace, the same window the insert path accepts).
//
// Returns nil / ErrNotFound / ErrOverUserQuota.
func (r *ShaleRepo) ReplaceSiteWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	identity := s.Identity.String()
	slug := s.Slug.String()
	newBody := int64(dedupedSize)

	// The redeploy's staged file refs (if any) ride this call's context,
	// isolated from any concurrent same-slug write. Read once and pass down.
	binds := pendingBindsFromContext(ctx)

	// Up-front ownership + existence gate. Sizes the delta (oldBody) and
	// rejects a missing/foreign row BEFORE the counter is touched. A missing
	// row and a foreign-owned row both collapse to ErrNotFound.
	var existing siteRow
	if err := r.getJSON(shaleKeySite(s.Slug), &existing); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if existing.Identity != identity {
		return ErrNotFound
	}
	oldBody := int64(existing.DedupedSize)

	// Step 1: reserve the delta (STRICT per-owner quota, combined paste+site).
	if err := r.reserveSiteReplaceBytes(identity, slug, oldBody, newBody, userCap, now); err != nil {
		return err
	}

	// Step 2: authoritative {slug}-shard swap. On any failure, release the
	// reservation so the delta is undone (over/under-count bounded to the
	// failure window + the reconciler).
	if err := r.replaceSiteAuthoritative(s, dedupedSize, binds); err != nil {
		_ = r.releaseSiteBytes(identity, slug)
		return err
	}

	// Step 3: confirm on the {id} shard: drop the reservation marker. The
	// counter is NOT touched here (the reserve already applied the delta).
	if err := r.confirmSiteInsert(identity, slug); err != nil {
		// The authoritative row is swapped + the delta is accounted on the
		// counter; only the reservation marker lingers. The owner sum reads the
		// counter (not the marker), so this is invisible to quota; the
		// reconciler drops the leaked marker. Surface the error so the caller
		// knows confirm lagged, but the replace is durable + quota-correct.
		return fmt.Errorf("confirm site replace: %w", err)
	}
	return nil
}

// replaceSiteAuthoritative swaps the {slug}-shard authoritative rows in one
// CAS: re-read sites/<slug> (ownership re-check INSIDE the read-set so a
// racing delete/re-deploy conflicts), overwrite it with the new row, and
// re-key the expiry index (delete the old (oldExpiresAt, slug) marker, write
// the new one) so the sweep sees the restarted retention clock. A missing or
// foreign-owned row inside the CAS collapses to ErrNotFound.
func (r *ShaleRepo) replaceSiteAuthoritative(s domain.Site, dedupedSize int, refs []cluster.BlobRef) error {
	row, err := shaleSiteRowFromDomain(s, dedupedSize)
	if err != nil {
		return err
	}
	siteKey := shaleKeySite(s.Slug)
	// On the transactional shale-blob path the redeploy's staged files (passed
	// in via refs, carried on the call's context by Commit) bind in this swap
	// transaction, and EVERY blob the OLD row referenced is unbound in the same
	// transaction. So a file carried across the redeploy is re-staged under a
	// fresh blob id and bound, while its OLD blob (and every truly-dropped
	// file's blob) is unbound; the bytes go unreferenced and SweepOrphans
	// reclaims them. This re-uploads unchanged bytes (no within-record byte
	// dedup in this phase) but never leaks. The new FileBlobs side-table is
	// authoritative for the read path.
	row.FileBlobs = fileBlobsFromRefs(refs)
	swapBody := func(tx shaleKVTx, bindAll func() error, unbind func(blobID string) error) error {
		var cur siteRow
		if err := shaleTxGetJSON(tx, siteKey, &cur); err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrNotFound // vanished between the gate and the swap
			}
			return err
		}
		if cur.Identity != s.Identity.String() {
			return ErrNotFound // re-deployed to a different owner; treat as gone
		}
		// created_at is the slug's birth time: STRUCTURALLY immutable across a
		// re-deploy, matching the sqlite UPDATE which never touches the column.
		// Pin it from the current row so a caller cannot move it via the new row
		// (the service layer forwards existing.CreatedAt, but the storage
		// contract must not trust the caller).
		row.CreatedAt = cur.CreatedAt
		if err := shaleTxPutJSON(tx, siteKey, row); err != nil {
			return err
		}
		// Re-key the expiry index off the CURRENT row's ExpiresAt (read in this
		// CAS), so a concurrent re-deploy that changed it can't orphan a stale
		// marker. Skip the delete when the timestamp is unchanged.
		if !cur.ExpiresAt.Equal(s.ExpiresAt) {
			if err := tx.Delete(shaleKeyExpirySite(cur.ExpiresAt, s.Slug)); err != nil {
				return err
			}
		}
		if err := tx.Put(shaleKeyExpirySite(s.ExpiresAt, s.Slug), markerValue); err != nil {
			return err
		}
		// Unbind every blob the OLD row referenced (its FileBlobs side-table);
		// the new manifest's files are freshly staged + bound below, so a file
		// carried across re-deploys is re-staged under a new id and the old id is
		// dropped. This frees the dropped files' bytes (and the unchanged files'
		// old bytes); SweepOrphans reclaims them after the grace.
		for _, oldBlobID := range cur.FileBlobs {
			if err := unbind(oldBlobID); err != nil {
				return err
			}
		}
		return bindAll()
	}
	if r.kv != nil {
		return r.kv.Transact(siteKey, func(tx *cluster.BlobTx) error {
			return swapBody(tx,
				func() error {
					for _, ref := range refs {
						if err := tx.BindBlob(ref); err != nil {
							return err
						}
					}
					return nil
				},
				func(blobID string) error {
					return tx.UnbindBlob(r.blobRefFor(siteKey, blobID))
				},
			)
		})
	}
	return r.cluster.Transact(siteKey, func(tx backend.Transaction) error {
		return swapBody(tx, func() error { return nil }, func(string) error { return nil })
	})
}

// PreClaimSiteSlug stakes a metadata-only claim on slug BEFORE a transactional
// (shale-collocated blob) site deploy consumes its untar stream, so every file
// stages under slug's {slug} shard and the pointers co-bind with the manifest
// at commit. It is the cheap existence stake the site-deploy service needs: a
// single {slug}-shard CAS that writes slug_owner/<slug> = owner IFF the slug is
// free in BOTH directions (no pastes/<slug>, no sites/<slug>) AND not already
// pre-claimed (no slug_owner/<slug>). All three reads join the CAS read-set, so
// two concurrent claims of the same slug serialize and one loses.
//
// It returns ErrSlugTaken on any collision (the caller re-mints + re-claims a
// fresh slug before reading the body), nil on a successful claim.
//
// The slug_owner/<slug> family is the SAME key a paste insert writes (it co-
// shards on {slug}); a site claim parks a marker there without conflicting with
// the site authoritative insert, which checks only sites/<slug> + pastes/<slug>
// for its collision (NOT slug_owner), so the deploy's own InsertSiteWithQuota
// Check does not reject the claim it just made. A crash after a successful claim
// but before the deploy commits leaves a slug_owner/<slug> marker with no
// authoritative site row: a harmless metadata leak. There is NO dedicated
// slug_owner sweep - the marker self-heals because a later paste insert that
// mints that slug overwrites slug_owner/<slug> unconditionally (insertAuthoritative
// Puts it and checks only pastes/<slug> + sites/<slug> for its collision). Until
// such a paste reuses it, the only effect is that one slug staying un-pre-claimable
// for a future SITE deploy, in a 32^8 space (the owner sum reads the counter and
// the site reconciler scans sites/; neither touches slug_owner).
//
// owner + now are accepted for symmetry with the seam and to value the marker;
// the claim itself does not charge quota (it is not a byte reservation).
func (r *ShaleRepo) PreClaimSiteSlug(_ context.Context, slug domain.Slug, owner string, _ time.Time) error {
	pasteKey := shaleKeyPaste(slug)
	siteKey := shaleKeySite(slug)
	ownerKey := shaleKeySlugOwner(slug)
	return r.cluster.Transact(siteKey, func(tx backend.Transaction) error {
		// Free in both directions? A present paste OR site row means the slug is
		// taken. Each Get joins the CAS read-set so a racing insert conflicts.
		if _, err := tx.Get(pasteKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("preclaim paste check: %w", err)
		}
		if _, err := tx.Get(siteKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("preclaim site check: %w", err)
		}
		// Already pre-claimed by another in-flight deploy? slug_owner present
		// means a paste owns it OR a concurrent site deploy claimed it first;
		// either way the slug is taken.
		if _, err := tx.Get(ownerKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("preclaim slug_owner check: %w", err)
		}
		return tx.Put(ownerKey, []byte(owner))
	})
}

// insertSiteAuthoritative writes the {slug}-shard authoritative rows in one
// CAS: the sites/<slug> JSON row + the expiry_sites index. The slug-collision
// check is BOTH directions (sites/<slug> AND pastes/<slug>), and both reads
// participate in the CAS read-set so a racing insert of the same slug (as a
// site OR a paste) conflicts.
func (r *ShaleRepo) insertSiteAuthoritative(s domain.Site, dedupedSize int, refs []cluster.BlobRef) error {
	row, err := shaleSiteRowFromDomain(s, dedupedSize)
	if err != nil {
		return err
	}
	siteKey := shaleKeySite(s.Slug)
	// On the transactional shale-blob path the deploy's staged files (passed in
	// via refs, carried on the call's context by ShaleBlobUnit.Commit) are ALL
	// bound in this one {slug} transaction with the manifest - no reservation
	// needed (the files are reader-invisible until the bind co-commits with the
	// manifest row). The sha -> blob-id side-table lands on the row so the read
	// path resolves a manifest sha to the blob id GetBlob needs.
	row.FileBlobs = fileBlobsFromRefs(refs)
	return r.runAuthoritative(siteKey, refs, func(tx shaleKVTx, bind func() error) error {
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
		if err := tx.Put(shaleKeyExpirySite(s.ExpiresAt, s.Slug), markerValue); err != nil {
			return err
		}
		// Bind every staged file's pointer, co-committed with the manifest.
		return bind()
	})
}

// confirmSiteInsert is step 3: on the {id} shard, drop the site reservation
// marker AND write the identity_sites/<id>/<slug> enumeration index entry,
// one CAS (the reserve marker, the index entry, and the byte counter all
// co-shard on {id}, so this is the exact analog of the paste confirmInsert).
// The index entry is a one-byte marker (ListSitesByOwner re-reads the
// authoritative sites/<slug> row for its fields). Called by BOTH insert and
// replace, so the index write also refreshes an in-place re-deploy (the
// marker is idempotent - a Put over an existing marker is a no-op).
// Best-effort + reconciler-healed: a lost confirm leaves a missing index
// entry the reconciler rebuilds, never a failed deploy. Idempotent on a
// missing reserve marker (a prior confirm or a reconciler drop removed it).
func (r *ShaleRepo) confirmSiteInsert(identity, slug string) error {
	reserveKey := shaleKeyIdentitySiteReserve(identity, slug)
	indexKey := shaleKeyIdentitySite(identity, slug)
	return r.cluster.Transact(reserveKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(reserveKey); err == nil {
			if err := tx.Delete(reserveKey); err != nil {
				return err
			}
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		return tx.Put(indexKey, markerValue)
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

// ListSitesByOwner enumerates the owner's identity_sites/<id>/ index on the
// {id} shard and re-reads each authoritative sites/<slug> row (mirroring the
// paste ListByOwner). A stale index entry whose authoritative row is gone is
// skipped and best-effort deleted (repair-on-read). Read-time expiry
// filtering (expires_at > now) drops expired-unswept sites so the list
// matches what the owner can still serve. The caller (verbList) merges the
// result with ListByOwner and sorts the union by expiry.
func (r *ShaleRepo) ListSitesByOwner(owner string, now time.Time) ([]domain.Site, error) {
	if owner == "" {
		return nil, nil
	}
	idx, err := r.scanPrefix(shalePrefixIdentitySites(owner))
	if err != nil {
		return nil, err
	}
	out := make([]domain.Site, 0, len(idx))
	var staleKeys [][]byte
	for _, item := range idx {
		slug := domain.Slug(extractSlug(item.Key))
		var row siteRow
		if err := r.getJSON(shaleKeySite(slug), &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				staleKeys = append(staleKeys, append([]byte(nil), item.Key...))
				continue
			}
			return nil, err
		}
		if !row.ExpiresAt.After(now) {
			continue // expired-unswept: stops counting/listing at read time
		}
		site, err := row.toDomain(slug)
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	for _, k := range staleKeys {
		_ = r.cluster.Delete(k)
	}
	return out, nil
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

// errSiteSizeChanged aborts the DeleteSite tombstone when the row's
// DedupedSize no longer matches the release marker's recorded bytes (a
// concurrent in-place ReplaceSiteWithQuotaCheck mutated it). DeleteSite catches
// it and restarts from step 1 with a refreshed marker, so the marker's bytes
// always equal exactly what the committed tombstone frees - never more, never
// less. That equality is what keeps the decrement from ever over- OR
// under-counting when the marker is later consumed (by the hot path or the
// reconciler).
var errSiteSizeChanged = errors.New("shale: site DedupedSize changed under delete; restart with a refreshed release marker")

// deleteSiteMaxAttempts bounds the DeleteSite restart loop so a pathological
// stream of same-slug re-deploys cannot spin it forever. A concurrent
// size-changing replace racing a delete is already rare; 64 attempts is far
// beyond any real interleaving and turns a livelock into a surfaced error.
const deleteSiteMaxAttempts = 64

// DeleteSite removes a site the crash-durable way, symmetric with the site
// reserve path: it writes a RELEASE MARKER on the {id} shard BEFORE the
// authoritative tombstone, so a crash between the tombstone and the counter
// decrement leaves a marker the reconciler completes (closing the markerless
// residual the old unconditional decrement left behind). The ordered protocol
// (docs/SPEC.md "Crash-durable delete decrement via a symmetric release
// marker"):
//
//  1. Read sites/<slug> -> owner + freed (its DedupedSize) + expiry + blobs.
//     Absent -> no-op return (idempotent; the sweep re-calls this for
//     already-gone slugs, matching the sqlite/slate DeleteSite + paste Delete).
//  2. Write the release marker on the {id} shard:
//     Put(identity_site_release/<id>/<slug>, {bytes: freed, created_at: now}).
//     A re-run overwrites it with the same value.
//  3. Tombstone on the {slug} shard, one CAS: re-read the row IN the read-set
//     and confirm its DedupedSize still equals the marker's bytes; a concurrent
//     in-place re-deploy that changed the size means the marker is stale, so
//     abort (errSiteSizeChanged) and restart from step 1 with a refreshed
//     marker. Otherwise delete sites/<slug>, delete expiry_sites/<ts>/<slug>,
//     and unbind the file blobs - the authoritative removal, atomic on the
//     transactional shale-blob path so the bytes go unreferenced exactly when
//     the manifest vanishes (SweepOrphans reclaims them after the grace).
//  4. Decrement + consume on the {id} shard, one CAS: drop the
//     identity_sites/<id>/<slug> enumeration entry (idempotent), then consume
//     the release marker - counter -= marker.bytes AND delete the marker iff it
//     is still present. This atomic marker-consume REPLACES the old
//     unconditional counter -= freed, so the hot path and the reconciler can
//     never both apply the same delete (at-most-once decrement).
//
// A crash before step 3 leaves the row live with a marker: the reconciler drops
// that marker WITHOUT decrementing (the bytes are still counted). A crash
// between step 3 and step 4 leaves the row gone with a marker: the reconciler
// decrements exactly marker.bytes and deletes the marker. Either way the
// never-under-count invariant holds.
func (r *ShaleRepo) DeleteSite(slug domain.Slug) error {
	siteKey := shaleKeySite(slug)
	for attempt := 0; attempt < deleteSiteMaxAttempts; attempt++ {
		// Step 1: read the authoritative row.
		var row siteRow
		if err := r.getJSON(siteKey, &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		identity := row.Identity
		freed := int64(row.DedupedSize)

		// Mint a fresh per-attempt nonce that stamps THIS delete instance's
		// release marker. The release-marker slot is a single per-(id,slug) key
		// reused across delete instances, so a concurrent re-deploy + re-delete
		// of the same slug can overwrite it between this step 2 and this step 4;
		// step 4 consumes ONLY a marker still carrying this nonce, so a delete
		// never decrements bytes recorded by a different delete's marker.
		nonce, err := newReleaseNonce()
		if err != nil {
			return err
		}

		// Step 2: write the release marker BEFORE the tombstone. created_at is
		// this delete's wall clock (the grace-window stamp); the marker is
		// consumed microseconds later in step 4 on the happy path, so the stamp
		// only matters if the process crashes before step 4.
		if err := r.writeSiteReleaseMarker(identity, slug.String(), freed, time.Now().UTC(), nonce); err != nil {
			return err
		}

		// Step 3: authoritative tombstone on the {slug} shard, size-guarded.
		delSiteBody := func(tx shaleKVTx, unbind func(blobID string) error) error {
			var cur siteRow
			if err := shaleTxGetJSON(tx, siteKey, &cur); err != nil {
				if errors.Is(err, ErrNotFound) {
					// Vanished (a concurrent delete already tombstoned it):
					// nothing to remove. Fall through to step 4's consume, which
					// is idempotent (the marker is deleted exactly once across
					// both racers).
					return nil
				}
				return err
			}
			if int64(cur.DedupedSize) != freed {
				return errSiteSizeChanged // stale marker; restart from step 1
			}
			if err := tx.Delete(siteKey); err != nil {
				return err
			}
			// Use the re-read row's ExpiresAt + FileBlobs (not the step-1 read,
			// which a same-size re-deploy could have moved) so the expiry index
			// and blob unbinds match the row this CAS actually removes.
			if err := tx.Delete(shaleKeyExpirySite(cur.ExpiresAt, slug)); err != nil {
				return err
			}
			for _, blobID := range cur.FileBlobs {
				if err := unbind(blobID); err != nil {
					return err
				}
			}
			return nil
		}
		var delErr error
		if r.kv != nil {
			delErr = r.kv.Transact(siteKey, func(tx *cluster.BlobTx) error {
				return delSiteBody(tx, func(blobID string) error {
					return tx.UnbindBlob(r.blobRefFor(siteKey, blobID))
				})
			})
		} else {
			delErr = r.cluster.Transact(siteKey, func(tx backend.Transaction) error {
				return delSiteBody(tx, func(string) error { return nil })
			})
		}
		if errors.Is(delErr, errSiteSizeChanged) {
			continue // refresh freed + marker, retry
		}
		if delErr != nil {
			return delErr
		}

		// Step 4: decrement + consume on the {id} shard.
		return r.consumeSiteReleaseMarker(identity, slug.String(), nonce)
	}
	return fmt.Errorf("delete site %s: exceeded %d restart attempts (a concurrent re-deploy kept changing the size)", slug, deleteSiteMaxAttempts)
}

// writeSiteReleaseMarker is step 2 of the crash-durable delete: a single
// {id}-shard CAS that stamps the release marker recording the bytes this delete
// will free AND the per-delete nonce that identifies this delete instance.
// Idempotent on re-run within one delete (overwrites the same
// {bytes, created_at, nonce}); a DIFFERENT delete instance writes a DIFFERENT
// nonce, which is exactly how the consume tells them apart.
func (r *ShaleRepo) writeSiteReleaseMarker(identity, slug string, freed int64, now time.Time, nonce string) error {
	releaseKey := shaleKeyIdentitySiteRelease(identity, slug)
	markerVal, err := encodeReleaseMarker(freed, now, nonce)
	if err != nil {
		return err
	}
	return r.cluster.Transact(releaseKey, func(tx backend.Transaction) error {
		return tx.Put(releaseKey, markerVal)
	})
}

// consumeSiteReleaseMarker is step 4 of the crash-durable delete: one
// {id}-shard CAS that drops the enumeration index entry AND consumes the
// release marker THIS delete wrote in step 2 (identified by nonce). The consume
// is the at-most-once gate: it decrements the counter by marker.bytes and
// deletes the marker ONLY if the marker is still present AND its nonce matches
// the nonce this delete stamped. An absent marker means the decrement already
// happened (a prior retry consumed it, or the reconciler did); a marker with a
// DIFFERENT nonce means a concurrent re-deploy + re-delete of the same slug
// overwrote the single per-(id,slug) slot after this delete's step 2 - that
// marker belongs to the OTHER delete (its bytes may still be LIVE), so this
// delete must NOT decrement it (that would under-count). Either way the counter
// is left untouched. All three keys (counter, index, release marker) co-shard
// on {id}, so this is one atomic single-shard transaction. Mirrors
// orphanReleaseSiteMarker's read-check on the marker, extended with the
// idempotent index drop and the nonce ownership check.
func (r *ShaleRepo) consumeSiteReleaseMarker(identity, slug, nonce string) error {
	counterKey := shaleKeyIdentitySiteBytes(identity)
	indexKey := shaleKeyIdentitySite(identity, slug)
	releaseKey := shaleKeyIdentitySiteRelease(identity, slug)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		// Drop the enumeration index entry (idempotent: a missing entry -
		// already reconciler-dropped, or a pre-index site - is fine).
		if _, err := tx.Get(indexKey); err == nil {
			if err := tx.Delete(indexKey); err != nil {
				return err
			}
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		// Consume the release marker: decrement + delete iff still present AND
		// still carrying THIS delete's nonce.
		raw, err := tx.Get(releaseKey)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return nil // already consumed; never double-decrement
			}
			return err
		}
		m, err := parseReservationMarker(raw)
		if err != nil {
			return err
		}
		if m.Nonce != nonce {
			// A concurrent re-delete of the same slug overwrote the slot after
			// this delete's step 2. That marker is the other delete's to
			// consume (its bytes may still be live); leave it untouched.
			return nil
		}
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		if err := tx.Put(counterKey, formatCounter(cur-m.Bytes)); err != nil {
			return err
		}
		return tx.Delete(releaseKey)
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
	// sitesByOwner drives the identity_sites enumeration-index backfill: every
	// authoritative site reprojected into its owner's index (add missing,
	// including sites deployed before the index existed). Mirrors the paste
	// reconciler's pastesByOwner.
	sitesByOwner := make(map[string]map[string]struct{})
	for _, item := range siteItems {
		slug := strings.TrimPrefix(string(item.Key), "sites/")
		liveSiteSlugs[slug] = struct{}{}
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			// Idempotent reconcile: the row physically exists (slug already
			// marked live above), we just cannot decode it - skip its index
			// projection; the next tick retries. Matches the paste reconciler's
			// undecodable-row policy.
			r.repoLog().Printf("reconcile sites: skip undecodable site %s: %v", item.Key, err)
			continue
		}
		if sitesByOwner[row.Identity] == nil {
			sitesByOwner[row.Identity] = make(map[string]struct{})
		}
		sitesByOwner[row.Identity][slug] = struct{}{}
	}

	markers, err := r.aggregatePrefix([]byte("identity_site_reserve/"))
	if err != nil {
		return fmt.Errorf("reconcile sites: scan reservations: %w", err)
	}
	for _, item := range markers {
		m, err := parseReservationMarker(item.Value)
		if err != nil {
			// Idempotent reconcile: skip + log the undecodable site marker and
			// continue. One poisoned site-reservation marker must not stall the
			// whole site-reconcile pass; the next tick retries it. See
			// docs/SPEC.md "Decode tolerance is per-scan-semantics", Policy 1.
			r.repoLog().Printf("reconcile sites: skip undecodable reservation marker %s: %v", item.Key, err)
			continue
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

	// Backfill/heal the identity_sites enumeration index from the authoritative
	// sites/ scan (the same "reproject rows, drop orphans" heal the paste
	// reconciler runs for identity_pastes). This is what makes sites deployed
	// before the index existed appear in ListSitesByOwner.
	return r.reconcileSiteIndexes(sitesByOwner)
}

// reconcileSiteIndexes rebuilds the per-owner identity_sites index to match
// the authoritative sites present, one {id}-shard CAS per entry (idempotent
// marker Put). Mirrors reconcileIndexes for pastes. Owners whose sites are
// ALL gone are not scanned here (nothing in sitesByOwner); any leftover index
// entry for them is dropped lazily by ListSitesByOwner's repair-on-read.
func (r *ShaleRepo) reconcileSiteIndexes(sitesByOwner map[string]map[string]struct{}) error {
	for owner, want := range sitesByOwner {
		have, err := r.scanPrefix(shalePrefixIdentitySites(owner))
		if err != nil {
			return fmt.Errorf("reconcile sites: scan index %s: %w", owner, err)
		}
		for _, item := range have {
			slug := extractSlug(item.Key)
			if _, ok := want[slug]; !ok {
				// Stale: the authoritative site is gone; drop the index entry.
				_ = r.cluster.Delete(item.Key)
			}
		}
		for slug := range want {
			key := shaleKeyIdentitySite(owner, slug)
			if err := r.cluster.Transact(key, func(tx backend.Transaction) error {
				return tx.Put(key, markerValue)
			}); err != nil {
				return err
			}
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
	if before, _, ok := strings.Cut(rest, "/"); ok {
		return before
	}
	return rest
}

// siteMarkerSlugFromKey extracts <slug> from an
// identity_site_reserve/<id>/<slug> key (everything after the FIRST '/'; an
// identity never contains a '/').
func siteMarkerSlugFromKey(key []byte) string {
	rest := strings.TrimPrefix(string(key), "identity_site_reserve/")
	if _, after, ok := strings.Cut(rest, "/"); ok {
		return after
	}
	return ""
}

// reconcileSiteReleaseMarkers completes past-grace SITE release markers - the
// crash-durable-delete backstop, and the exact INVERSE of
// reconcileSiteReservations. A reserve's increment is undone when the site did
// NOT materialize (site absent -> decrement); a release's decrement is applied
// when the site DE-materialized (site absent -> decrement). For each release
// marker:
//
//   - YOUNGER than reserveGrace: an in-flight delete between its marker (step 2)
//     and its consume (step 4). Leave it strictly alone.
//   - OLDER, site ABSENT (tombstone committed, decrement lost): complete it with
//     the same targeted, read-checked {id}-shard CAS as orphanReleaseSiteMarker
//     (counter -= marker.bytes AND delete the marker, atomically). Idempotent -
//     a marker already consumed (by the hot path or a prior pass) reads as
//     absent and the CAS is a no-op, so the bytes are never double-freed. This
//     is what makes a DOUBLE reconcile safe.
//   - OLDER, site PRESENT (the delete crashed before its tombstone, or the slug
//     was re-deployed): the bytes are LIVE and correctly counted, so the
//     completion must NOT decrement (that would under-count the live site).
//     Drop the marker with a read-checked CAS that deletes it only if it
//     re-reads as still past grace, so a fresh delete's young marker is never
//     removed out from under an in-flight retry. This branch cannot under-count
//     by construction, which is why it wins over the alternative.
//
// Cross-shard via aggregate; every write is an idempotent single-{id}-shard CAS,
// so it is safe to run concurrently with live traffic (the same posture the
// reserve reconciler holds).
func (r *ShaleRepo) reconcileSiteReleaseMarkers(now time.Time, reserveGrace time.Duration) error {
	// Live site slugs: a release marker whose base slug is present means the
	// bytes are live (never decrement); absent means the tombstone committed
	// (complete the decrement).
	siteItems, err := r.aggregatePrefix(prefixSites)
	if err != nil {
		return fmt.Errorf("reconcile site releases: scan sites: %w", err)
	}
	liveSiteSlugs := make(map[string]struct{}, len(siteItems))
	for _, item := range siteItems {
		liveSiteSlugs[strings.TrimPrefix(string(item.Key), "sites/")] = struct{}{}
	}

	markers, err := r.aggregatePrefix(prefixIdentitySiteReleaseAll)
	if err != nil {
		return fmt.Errorf("reconcile site releases: scan release markers: %w", err)
	}
	for _, item := range markers {
		m, err := parseReservationMarker(item.Value)
		if err != nil {
			// Idempotent reconcile: skip + log one undecodable release marker and
			// continue; the next tick retries it. Same per-marker discipline the
			// reserve reconciler uses.
			r.repoLog().Printf("reconcile site releases: skip undecodable release marker %s: %v", item.Key, err)
			continue
		}
		if markerInGrace(m, now, reserveGrace) {
			continue // in-flight delete; leave it for the consume step to drop
		}
		slug := siteReleaseMarkerSlugFromKey(item.Key)
		if _, present := liveSiteSlugs[slug]; present {
			// Row present: bytes are live + counted. DROP the marker WITHOUT
			// touching the counter, gated on the marker re-reading as still past
			// grace (never remove a fresh delete's young marker).
			if err := r.dropSiteReleaseMarkerIfStale(item.Key, now, reserveGrace); err != nil {
				return fmt.Errorf("reconcile site releases: drop %s: %w", item.Key, err)
			}
			continue
		}
		// Row absent: complete the lost decrement atomically (at most once).
		// Passes now+grace so the CAS can re-check the marker's age and leave a
		// freshly re-stamped young marker alone (never under-count a live site).
		if err := r.completeSiteReleaseMarker(item.Key, now, reserveGrace); err != nil {
			return fmt.Errorf("reconcile site releases: complete %s: %w", item.Key, err)
		}
	}
	return nil
}

// completeSiteReleaseMarker finishes an abandoned SITE delete whose tombstone
// committed but whose counter decrement was lost: one Transact pinned on the
// site counter (which co-shards with the marker, both keyed by {id}) reads the
// counter + the marker, and if the marker is still present subtracts
// marker.bytes AND deletes the marker, atomically. Idempotent: a marker already
// consumed reads as absent here and the CAS is a no-op, so the bytes are never
// double-freed (this is why a double reconcile does not double-decrement). It
// is the delete-side twin of orphanReleaseSiteMarker.
//
// The in-grace re-check inside the CAS is the never-under-count guard, and it
// mirrors dropSiteReleaseMarkerIfStale exactly: the reconciler chose this
// "complete" branch from a snapshot taken at scan time (marker past grace, slug
// absent), but between that scan and this CAS a concurrent re-deploy + fresh
// delete of the SAME slug can overwrite the single per-(id,slug) slot with a
// YOUNG marker whose bytes are still LIVE (the fresh delete may not have
// tombstoned yet, or may crash before it does). Decrementing that young
// marker's bytes here - and deleting it - would under-count the live site with
// no record left to recover from. So if the marker re-reads as still in grace,
// leave it strictly alone for that delete's own consume (step 4).
func (r *ShaleRepo) completeSiteReleaseMarker(releaseKey []byte, now time.Time, reserveGrace time.Duration) error {
	owner := siteReleaseMarkerOwnerFromKey(releaseKey)
	counterKey := shaleKeyIdentitySiteBytes(owner)
	return r.cluster.Transact(counterKey, func(tx backend.Transaction) error {
		raw, err := tx.Get(releaseKey)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return nil // already consumed; no-op (never double-decrement)
			}
			return err
		}
		m, err := parseReservationMarker(raw)
		if err != nil {
			return err
		}
		if markerInGrace(m, now, reserveGrace) {
			// A fresh delete re-stamped the slot after the scan; its bytes may
			// be live. Leave it for that delete's own consume. Never decrement.
			return nil
		}
		cur, err := txGetCounter(tx, counterKey)
		if err != nil {
			return err
		}
		if err := tx.Put(counterKey, formatCounter(cur-m.Bytes)); err != nil {
			return err
		}
		return tx.Delete(releaseKey)
	})
}

// dropSiteReleaseMarkerIfStale deletes a release marker WITHOUT touching the
// counter (the site-present branch), but only if the marker re-reads as still
// past grace inside the CAS. The re-check protects a fresh delete that re-wrote
// the marker (young created_at) between the reconciler's scan and this CAS: its
// young marker is left for that delete's own consume, never removed here.
func (r *ShaleRepo) dropSiteReleaseMarkerIfStale(releaseKey []byte, now time.Time, reserveGrace time.Duration) error {
	return r.cluster.Transact(releaseKey, func(tx backend.Transaction) error {
		raw, err := tx.Get(releaseKey)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return nil // already gone
			}
			return err
		}
		m, err := parseReservationMarker(raw)
		if err != nil {
			return err
		}
		if markerInGrace(m, now, reserveGrace) {
			return nil // a fresh delete re-stamped it; leave it for that delete's consume
		}
		return tx.Delete(releaseKey)
	})
}

// siteReleaseMarkerOwnerFromKey extracts <id> from an
// identity_site_release/<id>/<slug> key (everything between the prefix and the
// FIRST '/').
func siteReleaseMarkerOwnerFromKey(key []byte) string {
	rest := strings.TrimPrefix(string(key), "identity_site_release/")
	if before, _, ok := strings.Cut(rest, "/"); ok {
		return before
	}
	return rest
}

// siteReleaseMarkerSlugFromKey extracts <slug> from an
// identity_site_release/<id>/<slug> key (everything after the FIRST '/'; an
// identity never contains a '/').
func siteReleaseMarkerSlugFromKey(key []byte) string {
	rest := strings.TrimPrefix(string(key), "identity_site_release/")
	if _, after, ok := strings.Cut(rest, "/"); ok {
		return after
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
			// FAIL CLOSED, never skip. Site sibling of the paste ref-set scan:
			// skipping an undecodable site row would under-count the blob
			// keep-set, so a blob the site's manifest still references would
			// look orphaned and be deleted (irreversible). Abort the pass with
			// the error; the sweep treats any error here as "delete nothing
			// this pass". See docs/SPEC.md "Decode tolerance is
			// per-scan-semantics", Policy 2.
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
