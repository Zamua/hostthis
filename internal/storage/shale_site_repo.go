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
//	identity_sites/<id>/<slug>         -> {id}   shard  (per-owner enumeration index, one-byte marker value)
//
// # Scan-derived per-owner site quota (no counter)
//
// There is deliberately NO stored site-byte counter and no reservation /
// release marker. The per-owner SITE bytes are DERIVED by scanning the
// per-owner enumeration index identity_sites/<id>/<slug> and summing each
// live sites/<slug> row's DedupedSize (SumActiveSiteBytesByOwner), exactly
// the way slatedb's sumActiveSiteBytesForOwner works and the same index
// ListSitesByOwner uses. The service (service.DeploySite) computes the budget
// as `UserQuota - paste_bytes - site_bytes`, reading the paste sum and the
// site sum SEPARATELY and adding them, so the two scans MUST count disjoint
// sets: the paste sum enumerates identity_pastes/, the site sum enumerates
// identity_sites/, and the two families never overlap. The per-owner CAP
// check sums BOTH kinds (paste + site) against the cap, exactly like the
// sqlite identityActiveBytes and the slatedb per-identity check, so the
// ceiling holds however an owner splits their quota.
//
// # A deploy is a plain sequence (no reservation)
//
// A site deploy spans the {slug} shard (the authoritative sites/<slug> row +
// the cross-family paste-slug collision read) and the {id} shard (the
// enumeration-index entry), which cannot be one CAS transaction, but there is
// no counter to reserve against, so it is a plain sequence:
//
//  1. check quota (scan the owner's combined paste+site used bytes),
//  2. authoritative write on the {slug} shard (the sites/<slug> row + the
//     expiry index, with BOTH the sites/<slug> AND pastes/<slug> collision
//     reads in the CAS read-set),
//  3. write the identity_sites/<id>/<slug> enumeration entry on the {id}
//     shard (best-effort; a lost write leaves a missing entry the reconciler
//     reprojects, never a failed deploy).
//
// Expiry frees quota at READ time (the scan filters expires_at > now), so
// conformCaps.ExpiryFreesQuotaAtReadTime = true for shale sites (matching
// sqlite + slatedb); the check and the write are not atomic, so a bounded
// same-owner over-admit is accepted (StrictIdentityQuotaUnderConcurrency =
// false). See docs/SPEC.md "Scan-derived quota" / "The correctness argument".

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

// shaleKeyIdentitySite / shalePrefixIdentitySites are the per-owner site
// ENUMERATION index (mirror shaleKeyIdentityPaste / shalePrefixIdentityPastes).
// The entry carries a one-byte marker value (shale's Put rejects an empty
// value): both ListSitesByOwner and the quota scan re-read the authoritative
// sites/<slug> row for the returned fields + size, so the index only needs to
// enumerate the owner's slugs. It co-shards on <id> with identity_pastes/, so
// an owner's paste-index and site-index scans each stay single-shard.
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

// --- Site KV operations (on ShaleRepo) -------------------------------------

// InsertSiteWithQuotaCheck deploys a site. The per-owner cap is enforced by a
// scan-and-compare BEFORE the authoritative write (the owner's combined
// paste+site used bytes plus this deploy's bytes must not exceed the cap),
// and the slug-collision check is BOTH directions inside the authoritative
// CAS (reject if a site OR a paste already owns the slug). The check and the
// write are not atomic (bounded same-owner over-admit), the same tradeoff
// InsertWithQuotaCheck documents.
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

	// Quota CHECK (scan-based, combined paste+site) before the authoritative
	// write.
	if userCap > 0 {
		used, err := r.combinedActiveBytes(identity, now)
		if err != nil {
			return err
		}
		if used+body > userCap {
			return ErrOverUserQuota
		}
	}

	// Authoritative {slug}-shard write.
	if err := r.insertSiteAuthoritative(s, dedupedSize, binds); err != nil {
		return err
	}

	// Enumeration-index maintenance on the {id} shard (the scan-based quota
	// depends on it to enumerate the owner's sites). Best-effort +
	// reconciler-healed: a failure leaves a site the index does not list (a
	// transient under-count the reconciler heals), never a failed deploy, so
	// the site (already durable) is returned as success.
	if err := r.confirmSiteInsert(identity, slug); err != nil {
		r.repoLog().Printf("shale: site index maintenance for %s: %v (index lag; reconciler will heal)", s.Slug, err)
	}
	return nil
}

// ReplaceSiteWithQuotaCheck re-deploys an existing OWNED site in place via the
// same scan-based sequence InsertSiteWithQuotaCheck uses, charging the REPLACE
// DELTA rather than the full new size:
//
//  1. up-front Get to size the delta (read oldBody) and gate ownership +
//     existence,
//  2. quota CHECK (scan the owner's combined paste+site used bytes) charging
//     the delta: when the old row is LIVE the scan's `used` already counts its
//     oldBody, so the post-swap total is used + (newBody-oldBody) - a same-size
//     re-deploy nets zero, a smaller one frees the difference, a larger one is
//     rejected when it would breach the cap. When the old row is EXPIRED-unswept
//     it is NOT in `used`, so oldBody is credited as 0 and the replace charges
//     the full new size (reviving an expired site is a fresh write of that size),
//  3. authoritative swap on the {slug} shard (re-read sites/<slug> in the CAS
//     read-set for ownership + the old expiry key, overwrite the row, re-key
//     the expiry index),
//  4. refresh the identity_sites/<id>/<slug> enumeration entry (best-effort;
//     the site scan reads DedupedSize from the freshly-swapped row).
//
// Ownership/existence: the slug must already be a site owned by s.Identity.
// A missing row OR a foreign-owned row both collapse to ErrNotFound (the SAME
// sentinel a missing slug yields), so existence/ownership never leaks. The
// gate is enforced TWICE: an up-front Get to size the delta (read oldBody +
// reject a foreign/missing row before the swap), and again INSIDE the
// authoritative CAS so a concurrent delete/re-deploy conflicts.
//
// The check and the swap are not atomic (bounded same-owner over-admit), and
// the durable total-bytes ceiling is NOT checked here: it is the object-store
// bucket quota, enforced when a blob Put is rejected (see SPEC "Limits ->
// Durable total-bytes ceiling: an object-store quota").
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
	// rejects a missing/foreign row before the swap. A missing row and a
	// foreign-owned row both collapse to ErrNotFound.
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
	// Credit the old bytes back ONLY if the old row is still LIVE: the
	// scan-based `used` filters expires_at > now, so an expired-but-unswept old
	// row is NOT in it. Crediting an expired old row would double-subtract and
	// admit an over-cap replace - reviving an expired site must charge the full
	// new size (docs/SPEC.md "Reviving an expired-but-unswept record charges its
	// FULL post-revival size"). Mirrors the sqlite + slatedb ReplaceWithQuotaCheck.
	oldBody := int64(existing.DedupedSize)
	if !existing.ExpiresAt.After(now) {
		oldBody = 0
	}

	// Quota CHECK (scan-based) before the swap, charging the DELTA. When the old
	// row is live the scan's `used` already counts its oldBody, so the post-swap
	// owner total is used - oldBody + newBody = used + (newBody-oldBody): a
	// same-size re-deploy nets zero, a smaller one frees the difference, a larger
	// one is rejected when it would breach the cap. When the old row is expired
	// (oldBody credited as 0) the replace charges the full new size.
	if userCap > 0 {
		used, err := r.combinedActiveBytes(identity, now)
		if err != nil {
			return err
		}
		if used+(newBody-oldBody) > userCap {
			return ErrOverUserQuota
		}
	}

	// Authoritative {slug}-shard swap.
	if err := r.replaceSiteAuthoritative(s, dedupedSize, binds); err != nil {
		return err
	}

	// Refresh the enumeration-index entry on the {id} shard (idempotent
	// marker Put; the site scan reads DedupedSize from the freshly-swapped
	// authoritative row). Best-effort + reconciler-healed.
	if err := r.confirmSiteInsert(identity, slug); err != nil {
		r.repoLog().Printf("shale: site index refresh for %s: %v (index lag; reconciler will heal)", s.Slug, err)
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

// confirmSiteInsert writes the identity_sites/<id>/<slug> enumeration index
// entry on the {id} shard, one CAS. The entry is a one-byte marker
// (SumActiveSiteBytesByOwner / ListSitesByOwner re-read the authoritative
// sites/<slug> row for the fields + size). Called by BOTH insert and replace,
// so the write also refreshes an in-place re-deploy (the marker is idempotent
// - a Put over an existing marker is a no-op). Best-effort + reconciler-
// healed: a lost write leaves a missing index entry the reconciler rebuilds,
// never a failed deploy.
func (r *ShaleRepo) confirmSiteInsert(identity, slug string) error {
	indexKey := shaleKeyIdentitySite(identity, slug)
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
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

// SumActiveSiteBytesByOwner derives the identity's active SITE bytes by
// SCANNING the per-identity enumeration index (identity_sites/<id>/) and
// summing the authoritative DedupedSize of each non-expired site,
// byte-identical in shape to slatedb's sumActiveSiteBytesForOwner. There is
// no stored site counter: the number is computed from the authoritative
// sites/<slug> rows on every read, so it is idempotently self-correct. The
// service layer adds the paste-side sum where it needs the combined figure.
// now IS used: an expired-but-unswept site is excluded at read time
// (ExpiryFreesQuotaAtReadTime = true on shale now), matching sqlite + slatedb.
func (r *ShaleRepo) SumActiveSiteBytesByOwner(owner string, now time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	return r.sumActiveSiteBytesForOwner(owner, now)
}

// sumActiveSiteBytesForOwner walks identity_sites/<owner>/ and sums
// DedupedSize of the rows whose ExpiresAt > now. A stale index entry whose
// authoritative row is gone is skipped; an undecodable row HARD-FAILS the
// scan (Policy 3: a synchronous write-path read fails safe by rejecting, not
// under-counting).
func (r *ShaleRepo) sumActiveSiteBytesForOwner(owner string, now time.Time) (int64, error) {
	idx, err := r.scanPrefix(shalePrefixIdentitySites(owner))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range idx {
		slug := domain.Slug(extractSlug(item.Key))
		var row siteRow
		if err := r.getJSON(shaleKeySite(slug), &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // stale index entry
			}
			return 0, err
		}
		if !row.ExpiresAt.After(now) {
			continue // expired-unswept: stops counting at read time
		}
		total += int64(row.DedupedSize)
	}
	return total, nil
}

// DeleteSite removes a site: the authoritative {slug} rows go away and the
// {id} enumeration-index entry is dropped, so the owner's scan-derived quota
// sum stops counting the site. There is no byte counter to decrement (the
// freed bytes leave the owner's sum the instant the authoritative row + its
// index entry vanish), so no release marker / crash-durable-decrement
// protocol is needed.
//
//  1. Read sites/<slug> -> owner + expiry + blobs. Absent -> no-op return
//     (idempotent; the sweep re-calls this for already-gone slugs, matching
//     the sqlite/slate DeleteSite + paste Delete).
//  2. Tombstone on the {slug} shard, one CAS: delete sites/<slug>, delete
//     expiry_sites/<ts>/<slug>, and unbind the file blobs - atomic on the
//     transactional shale-blob path so the bytes go unreferenced exactly when
//     the manifest vanishes (SweepOrphans reclaims them after the grace).
//  3. Drop the identity_sites/<id>/<slug> enumeration entry on the {id} shard
//     (idempotent) so the site leaves the owner's scan + ListSitesByOwner.
func (r *ShaleRepo) DeleteSite(slug domain.Slug) error {
	siteKey := shaleKeySite(slug)
	// Step 1: read the authoritative row (owner for the index drop).
	var row siteRow
	if err := r.getJSON(siteKey, &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	identity := row.Identity

	// Step 2: authoritative tombstone on the {slug} shard. Re-read the row IN
	// the CAS so the expiry index and blob unbinds match the row this
	// transaction actually removes (a same-size re-deploy could have moved
	// ExpiresAt / FileBlobs between step 1 and here).
	delSiteBody := func(tx shaleKVTx, unbind func(blobID string) error) error {
		var cur siteRow
		if err := shaleTxGetJSON(tx, siteKey, &cur); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil // a concurrent delete already tombstoned it
			}
			return err
		}
		if err := tx.Delete(siteKey); err != nil {
			return err
		}
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
	if delErr != nil {
		return delErr
	}

	// Step 3: drop the enumeration-index entry on the {id} shard. Idempotent.
	indexKey := shaleKeyIdentitySite(identity, slug.String())
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(indexKey); err == nil {
			return tx.Delete(indexKey)
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		return nil
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

// reconcileSiteIndexPass reprojects the identity_sites enumeration index from
// the authoritative sites/ rows across all shards - the SITE parallel of the
// paste reconciler's index reprojection, re-homed here as a standalone step in
// Reconcile (it was previously reached only through the now-deleted
// site-reservation pass). It scans sites/, groups each live site under its
// owner, and reprojects (add missing entries, drop orphans) so a site's bytes
// are enumerated by the owner's quota scan even after a crash between the
// authoritative write and the index write - and so sites deployed before the
// index existed appear in ListSitesByOwner. Cross-shard via aggregate; every
// write is an idempotent single-{id}-shard CAS, safe under live traffic.
func (r *ShaleRepo) reconcileSiteIndexPass() error {
	siteItems, err := r.aggregatePrefix(prefixSites)
	if err != nil {
		return fmt.Errorf("reconcile sites: scan sites: %w", err)
	}
	// sitesByOwner drives the identity_sites enumeration-index reprojection:
	// every authoritative site reprojected into its owner's index. Mirrors the
	// paste reconciler's pastesByOwner.
	sitesByOwner := make(map[string]map[string]struct{})
	for _, item := range siteItems {
		slug := strings.TrimPrefix(string(item.Key), "sites/")
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			// Idempotent reconcile: one poisoned site row must not stall the pass
			// (Policy 1). But dropping it silently would leave a durable
			// UNDER-count if its identity_sites entry was ALSO lost: the site
			// quota scan reads THROUGH the enumeration index, so an un-indexed
			// undecodable row is invisible to it. Derive the owner
			// decode-independently from slug_owner/<slug> (written by the site
			// deploy's pre-claim on the transactional shale-blob path) and still
			// project the enumeration entry, so the next scan enumerates the slug
			// and re-reads the authoritative row (which then hard-fails the scan =
			// fail-closed reject, never a silent under-count). See docs/SPEC.md
			// "Decode tolerance of the quota scan".
			owner := r.ownerOfSlug(domain.Slug(slug))
			if owner == "" {
				// No slug_owner to derive the owner (reachable only on the
				// metadata-only test path that skips the pre-claim; prod always
				// writes slug_owner). Cannot project the entry; log loudly - the
				// one residual where the row can stay un-enumerated until repaired.
				r.repoLog().Printf("reconcile sites: undecodable site %s AND no slug_owner: cannot project enumeration entry; site quota may under-count this slug until it is repaired: %v", item.Key, err)
				continue
			}
			if sitesByOwner[owner] == nil {
				sitesByOwner[owner] = make(map[string]struct{})
			}
			sitesByOwner[owner][slug] = struct{}{}
			r.repoLog().Printf("reconcile sites: undecodable site %s: projected placeholder enumeration entry under owner %s (fail-closed): %v", item.Key, owner, err)
			continue
		}
		if sitesByOwner[row.Identity] == nil {
			sitesByOwner[row.Identity] = make(map[string]struct{})
		}
		sitesByOwner[row.Identity][slug] = struct{}{}
	}
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
