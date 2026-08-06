// Package storage's shale-cluster-backed static-site persistence.
// Co-location is by shaleShardKey.
//
// # Key layout
//
//	sites/<slug>                authoritative JSON row      -> {slug}
//	identity_sites/<id>/<slug>  per-owner enumeration index -> {id}
//	                            (value-bearing: cached size)
//
// # Scan-derived site quota
//
// No stored counter, no reservation: per-owner site bytes are one prefix scan
// of identity_sites/<id>/ summing the cached size each entry carries.
//
// The service budget is UserQuota - paste_bytes - site_bytes, read as two
// separate sums, so the two scans MUST count disjoint sets: identity_pastes/
// and identity_sites/ never overlap.
//
// # A deploy is a plain sequence
//
// It spans the {slug} shard (authoritative row, plus the cross-family paste
// collision read) and the {id} shard (enumeration entry), so it cannot be one
// transaction:
//
//  1. check quota (scan combined paste+site bytes),
//  2. authoritative write on {slug}, with BOTH sites/<slug> and pastes/<slug>
//     collision reads in the CAS read-set,
//  3. write the enumeration entry on {id}, best-effort: a lost write leaves an
//     entry the reconciler reprojects, never a failed deploy.
//
// Check and write are not atomic, so a bounded same-owner over-admit is
// accepted. See docs/SPEC.md "Scan-derived quota".

//go:build slatedb

package storage

import (
	"bytes"
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

// ShaleSiteRepo adapts a ShaleRepo to service.SiteRepo + service.SweepSites,
// so the site repo shares the paste repo's cluster handle and shard routing.
//
// The interface method names collide with ShaleRepo's paste methods under
// different signatures, so both cannot live on ShaleRepo; the KV operations
// are `...Site` methods there and this adapter exposes them under the
// interface names.
type ShaleSiteRepo struct {
	repo *ShaleRepo
}

// NewShaleSiteRepo wraps a ShaleRepo so static-site hosting runs on the shale
// backend.
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

// service.SlugClaimReleaser
func (s *ShaleSiteRepo) ReleaseSlugClaim(ctx context.Context, slug domain.Slug, owner string) error {
	return s.repo.ReleaseSiteSlugClaim(ctx, slug, owner)
}

// service.SweepSites (Delete also serves the owner-facing removal path)
func (s *ShaleSiteRepo) Delete(slug domain.Slug) error { return s.repo.DeleteSite(slug) }
func (s *ShaleSiteRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	return s.repo.ReferencedSiteBlobSHAs()
}

// --- key builders ----------------------------------------------------------

func shaleKeySite(slug domain.Slug) []byte { return []byte("sites/" + slug.String()) }

// shaleKeyIdentitySite / shalePrefixIdentitySites are the per-owner site
// ENUMERATION index. The entry is VALUE-BEARING (identitySiteRow): it caches
// the deduped size the quota scan sums, so SumActiveSiteBytesByOwner
// is one prefix scan with zero per-entry row reads. It co-shards on <id> with
// identity_pastes/, so an owner's paste-index and site-index scans each stay
// single-shard. A LEGACY entry carries a one-byte marker or an empty value and
// is read through its authoritative row until the reconciler's reprojection
// overwrites it with the JSON projection.
func shaleKeyIdentitySite(identity, slug string) []byte {
	return []byte("identity_sites/" + identity + "/" + slug)
}

func shalePrefixIdentitySites(identity string) []byte {
	return []byte("identity_sites/" + identity + "/")
}

// identitySiteRow is the value-bearing projection stored at
// identity_sites/<id>/<slug>: the cached deduped size the site quota scan
// sums. Derived and eventually consistent - the reconciler rebuilds
// it from the authoritative sites/<slug> row, so cached-value error is bounded
// by a reconcile cycle.
type identitySiteRow struct {
	Size int `json:"size"`

	// Placeholder marks a fail-closed entry the reconciler projects for a
	// slug whose authoritative sites/<slug> row cannot be decoded: the quota
	// scan HARD-FAILS on it rather than silently under-counting (docs/SPEC.md
	// "Decode tolerance of the quota scan"). Cleared when the row decodes
	// again or is removed.
	Placeholder bool `json:"placeholder,omitempty"`
}

// --- JSON row schema -------------------------------------------------------

// shaleSiteRowFromDomain builds the persisted siteRow (defined in
// slate_site_repo.go, the shape both slatedb-tagged backends store).
// DedupedSize is stored so the quota scans never decode a manifest just to
// sum bytes.
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
	}, nil
}

// --- Site KV operations (on ShaleRepo) -------------------------------------

// InsertSiteWithQuotaCheck deploys a site. The per-owner cap is a
// scan-and-compare BEFORE the authoritative write (combined paste+site used
// bytes plus this deploy's bytes must not exceed the cap), and the
// slug-collision check runs BOTH directions inside the authoritative CAS
// (reject if a site OR a paste already owns the slug). Check and write are not
// atomic (bounded same-owner over-admit).
//
// The durable total-bytes ceiling is NOT checked here: it is the object-store
// bucket quota, enforced when a blob Put is rejected.
//
// Returns nil / ErrSlugTaken / ErrOverUserQuota.
func (r *ShaleRepo) InsertSiteWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	identity := s.Identity.String()
	slug := s.Slug.String()
	body := int64(dedupedSize)

	// The deploy's staged file refs ride this call's context, isolated from any
	// concurrent same-slug write. Read once and pass them down so the
	// authoritative {slug} transaction binds exactly this call's blobs.
	binds := pendingBindsFromContext(ctx)

	if userCap > 0 {
		used, err := r.combinedActiveBytes(identity, now)
		if err != nil {
			return err
		}
		if err := (domain.Allowance{Cap: userCap, Used: used}).Admit(body); err != nil {
			return err
		}
	}

	if err := r.insertSiteAuthoritative(s, dedupedSize, binds); err != nil {
		return err
	}

	// Enumeration-index maintenance on the {id} shard, best-effort: a failure
	// leaves a site the quota scan does not count (a transient under-count the
	// reconciler heals), never a failed deploy, so the already-durable site
	// returns success.
	if err := r.confirmSiteInsert(identity, slug, dedupedSize); err != nil {
		r.repoLog().Printf("shale: site index maintenance for %s: %v (index lag; reconciler will heal)", s.Slug, err)
	}
	return nil
}

// ReplaceSiteWithQuotaCheck re-deploys an owned site in place, charging the
// replace DELTA rather than the full new size. When the old row is LIVE the
// scan's `used` already counts its bytes, so a same-size re-deploy nets zero
// and a smaller one frees the difference.
//
// A missing row and a foreign-owned row both collapse to ErrNotFound, so
// existence and ownership never leak. The gate is enforced twice: up front, and
// again inside the CAS so a concurrent delete or re-deploy conflicts.
//
// Check and swap are not atomic (bounded same-owner over-admit). The durable
// total-bytes ceiling is not checked here: it is the object-store bucket quota,
// surfaced when a blob Put is rejected.
//
// Returns nil / ErrNotFound / ErrOverUserQuota.
func (r *ShaleRepo) ReplaceSiteWithQuotaCheck(ctx context.Context, s domain.Site, dedupedSize int, userCap int64, now time.Time) error {
	identity := s.Identity.String()
	slug := s.Slug.String()
	newBody := int64(dedupedSize)

	// The redeploy's staged file refs ride this call's context, isolated from
	// any concurrent same-slug write.
	binds := pendingBindsFromContext(ctx)

	// Up-front ownership + existence gate, also sizing the delta.
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

	if userCap > 0 {
		used, err := r.combinedActiveBytes(identity, now)
		if err != nil {
			return err
		}
		if err := (domain.Allowance{Cap: userCap, Used: used}).AdmitReplacing(oldBody, newBody); err != nil {
			return err
		}
	}

	if err := r.replaceSiteAuthoritative(s, dedupedSize, binds); err != nil {
		return err
	}

	// Refresh the enumeration entry's cached size on the {id} shard,
	// best-effort: a lost refresh leaves a stale cached size until the next
	// reconciler reprojection (bounded drift).
	if err := r.confirmSiteInsert(identity, slug, dedupedSize); err != nil {
		r.repoLog().Printf("shale: site index refresh for %s: %v (index lag; reconciler will heal)", s.Slug, err)
	}
	return nil
}

// replaceSiteAuthoritative swaps the {slug}-shard authoritative rows in one
// CAS: re-read sites/<slug> (the ownership re-check must be INSIDE the
// read-set so a racing delete/re-deploy conflicts) and overwrite it. A missing
// or foreign-owned row inside the CAS collapses to ErrNotFound.
func (r *ShaleRepo) replaceSiteAuthoritative(s domain.Site, dedupedSize int, refs []cluster.BlobRef) error {
	row, err := shaleSiteRowFromDomain(s, dedupedSize)
	if err != nil {
		return err
	}
	siteKey := shaleKeySite(s.Slug)
	// On the transactional shale-blob path the redeploy's staged files bind in
	// this swap transaction and EVERY blob the OLD row referenced is unbound in
	// the same transaction, so a file carried across the redeploy is re-staged
	// under a fresh blob id while its old blob goes unreferenced for
	// SweepOrphans. This re-uploads unchanged bytes but never leaks. The new
	// FileBlobs side-table is authoritative for the read path.
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
		// created_at is the slug's birth time, immutable across a re-deploy. Pin
		// it from the current row: the storage contract must not trust the
		// caller's value.
		row.CreatedAt = cur.CreatedAt
		if err := shaleTxPutJSON(tx, siteKey, row); err != nil {
			return err
		}
		// Unbind every blob the OLD row referenced; the new manifest's files are
		// freshly staged + bound below. SweepOrphans reclaims the freed bytes
		// after the grace.
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
// site deploy consumes its untar stream, so every file stages under the {slug}
// shard and the pointers co-bind with the manifest at commit.
//
// A single {slug}-shard CAS writing slug_owner/<slug> IFF the slug is free in
// BOTH directions (no pastes/, no sites/) and not already pre-claimed. All three
// reads join the read-set, so concurrent claims serialize and one loses.
// Returns ErrSlugTaken on any collision; the caller re-mints and re-claims.
//
// slug_owner/<slug> is the same key a paste insert writes. The site
// authoritative insert checks only sites/ and pastes/ for its collision, so a
// deploy does not reject the claim it just made.
//
// A claim is durable and no sweep reclaims one, so a deploy that does not
// commit under the slug MUST hand it back via ReleaseSiteSlugClaim or the slug
// leaves the site namespace for good. Only a crash in that window leaves a
// marker with no row, and a later paste minting the slug overwrites it
// unconditionally.
//
// owner and now are accepted for symmetry with the seam; a claim is a stake,
// not a byte reservation, so it charges no quota.
func (r *ShaleRepo) PreClaimSiteSlug(_ context.Context, slug domain.Slug, owner string, _ time.Time) error {
	pasteKey := shaleKeyPaste(slug)
	siteKey := shaleKeySite(slug)
	ownerKey := shaleKeySlugOwner(slug)
	return r.cluster.Transact(siteKey, func(tx backend.Transaction) error {
		// Each Get joins the CAS read-set so a racing insert conflicts.
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
		// slug_owner present means a paste owns the slug OR a concurrent site
		// deploy claimed it first; either way it is taken.
		if _, err := tx.Get(ownerKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("preclaim slug_owner check: %w", err)
		}
		return tx.Put(ownerKey, []byte(owner))
	})
}

// ReleaseSiteSlugClaim hands a pre-claimed slug back when the deploy that
// staked it never committed a site under it, so an aborted deploy does not burn
// the slug: PreClaimSiteSlug rejects any slug carrying a marker, and nothing
// else removes one.
//
// A single {slug}-shard CAS deleting slug_owner/<slug> IFF it still holds
// owner's stake AND neither pastes/<slug> nor sites/<slug> exists. All three
// reads join the read-set, so a racing insert that makes the marker load-bearing
// conflicts instead of losing its owner pointer, and an ambiguous commit that
// actually landed keeps its marker. A missing or foreign claim is a no-op, so a
// repeated release is harmless.
func (r *ShaleRepo) ReleaseSiteSlugClaim(_ context.Context, slug domain.Slug, owner string) error {
	pasteKey := shaleKeyPaste(slug)
	siteKey := shaleKeySite(slug)
	ownerKey := shaleKeySlugOwner(slug)
	return r.cluster.Transact(siteKey, func(tx backend.Transaction) error {
		raw, err := tx.Get(ownerKey)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				return nil // nothing claimed
			}
			return fmt.Errorf("release slug_owner check: %w", err)
		}
		claimed, err := stripEnvelope(raw)
		if err != nil {
			return fmt.Errorf("release slug_owner strip: %w", err)
		}
		if string(claimed) != owner {
			return nil // another identity's stake
		}
		if _, err := tx.Get(pasteKey); err == nil {
			return nil // a paste owns the slug; the marker is its own
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("release paste check: %w", err)
		}
		if _, err := tx.Get(siteKey); err == nil {
			return nil // the deploy landed after all
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("release site check: %w", err)
		}
		return tx.Delete(ownerKey)
	})
}

// insertSiteAuthoritative writes sites/<slug> in one {slug}-shard CAS. The
// slug-collision check is BOTH directions (sites/ AND
// pastes/), and both reads participate in the read-set so a racing insert of
// the same slug conflicts.
func (r *ShaleRepo) insertSiteAuthoritative(s domain.Site, dedupedSize int, refs []cluster.BlobRef) error {
	row, err := shaleSiteRowFromDomain(s, dedupedSize)
	if err != nil {
		return err
	}
	siteKey := shaleKeySite(s.Slug)
	// On the transactional shale-blob path the deploy's staged files are ALL
	// bound in this one {slug} transaction with the manifest - no reservation
	// needed, since the files are reader-invisible until the bind co-commits
	// with the manifest row. The sha -> blob-id side-table lands on the row so
	// the read path resolves a manifest sha to the blob id GetBlob needs.
	row.FileBlobs = fileBlobsFromRefs(refs)
	return translateCrossShard(r.runAuthoritative(siteKey, refs, func(tx shaleKVTx, bind func() error) error {
		// A found site OR paste is ErrSlugTaken; the ExpectAbsent read-checks
		// make a concurrent insert of the same slug conflict.
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
		// Bind every staged file's pointer, co-committed with the manifest.
		return bind()
	}))
}

// translateCrossShard maps shale's cross-shard guard sentinel
// (backend.ErrCrossShard) into the domain vocabulary the deploy service checks
// (domain.ErrCrossShardDeploy), so the service layer never imports a shale
// package. Both sentinels stay in the wrap chain (%w twice) so errors.Is holds
// for either and the log keeps the original text. Any other error passes
// through untouched.
func translateCrossShard(err error) error {
	if err != nil && errors.Is(err, backend.ErrCrossShard) {
		return fmt.Errorf("%w: %w", domain.ErrCrossShardDeploy, err)
	}
	return err
}

// confirmSiteInsert writes the value-bearing identity_sites/<id>/<slug> entry
// on the {id} shard in one CAS. Called by BOTH insert and replace (idempotent
// overwrite), so it also refreshes an in-place re-deploy's cached values.
// Best-effort: a lost write leaves a missing or stale entry the reconciler
// rebuilds, never a failed deploy.
func (r *ShaleRepo) confirmSiteInsert(identity, slug string, dedupedSize int) error {
	indexKey := shaleKeyIdentitySite(identity, slug)
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		return shaleTxPutJSON(tx, indexKey, identitySiteRow{Size: dedupedSize})
	})
}

// GetSite returns the site for slug, or ErrNotFound.
func (r *ShaleRepo) GetSite(slug domain.Slug) (domain.Site, error) {
	var row siteRow
	if err := r.getJSON(shaleKeySite(slug), &row); err != nil {
		return domain.Site{}, err
	}
	return row.toDomain(slug)
}

// ListSitesByOwner enumerates the owner's identity_sites/<id>/ index on the
// {id} shard and re-reads each authoritative sites/<slug> row. A stale index
// entry whose row is gone is skipped and best-effort deleted
// (repair-on-read).
func (r *ShaleRepo) ListSitesByOwner(owner string, _ time.Time) ([]domain.Site, error) {
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

// SumActiveSiteBytesByOwner derives the identity's active SITE bytes from ONE
// prefix scan of identity_sites/<id>/, summing each value-bearing entry's
// cached deduped size with zero per-entry row reads. There is no stored site
// counter: the reconciler rebuilds the cached values from the authoritative
// rows, so drift is bounded by a reconcile cycle. The service layer adds the
// paste-side sum where it needs the combined figure.
func (r *ShaleRepo) SumActiveSiteBytesByOwner(owner string, _ time.Time) (int64, error) {
	if owner == "" {
		return 0, nil
	}
	return r.sumActiveSiteBytesForOwner(owner)
}

// sumActiveSiteBytesForOwner scans identity_sites/<owner>/ once and sums each
// entry's cached size. Fail-closed (Policy 3, a synchronous
// write-path read): an entry that does not decode, or that carries the
// reconciler's Placeholder marker, HARD-FAILS the scan. The one exception is a
// LEGACY entry recognized by shape (a one-byte marker or an empty value), read
// through its authoritative sites/<slug> row until the reconciler enriches it.
func (r *ShaleRepo) sumActiveSiteBytesForOwner(owner string) (int64, error) {
	idx, err := r.scanPrefix(shalePrefixIdentitySites(owner))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range idx {
		if len(item.Value) == 0 || bytes.Equal(item.Value, markerValue) {
			n, err := r.legacySiteEntryBytes(item.Key)
			if err != nil {
				return 0, err
			}
			total += n
			continue
		}
		var row identitySiteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if row.Placeholder {
			return 0, fmt.Errorf("site quota scan: %s is a fail-closed placeholder (authoritative row undecodable; the reconciler clears it once the row is repaired)", item.Key)
		}
		total += int64(row.Size)
	}
	return total, nil
}

// legacySiteEntryBytes resolves a LEGACY (marker-valued) identity_sites entry
// against its authoritative sites/<slug> row, so a deployment upgrades without
// a flag day. A stale legacy entry (row gone) is skipped; an undecodable row
// HARD-FAILS (Policy 3); a live row contributes its DedupedSize.
func (r *ShaleRepo) legacySiteEntryBytes(indexKey []byte) (int64, error) {
	slug := domain.Slug(extractSlug(indexKey))
	var row siteRow
	if err := r.getJSON(shaleKeySite(slug), &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil // stale legacy entry; the reconciler prunes it
		}
		return 0, err
	}
	return int64(row.DedupedSize), nil
}

// DeleteSite removes a site: the authoritative {slug} rows and the {id}
// enumeration-index entry. There is no byte counter to decrement (freed bytes
// leave the owner's sum the instant the row and its index entry vanish), so no
// release marker / crash-durable-decrement protocol is needed.
//
//  1. Read sites/<slug>. Absent -> no-op return (idempotent; the sweep
//     re-calls this for already-gone slugs).
//  2. Tombstone on the {slug} shard, one CAS: delete sites/<slug>, delete
//     slug_owner/<slug>, and unbind the file
//     blobs - atomic on the transactional shale-blob path so the bytes go
//     unreferenced exactly when the manifest vanishes (SweepOrphans reclaims
//     them after the grace). Dropping slug_owner is what returns the slug to
//     the namespace: PreClaimSiteSlug rejects a slug that still carries one, so
//     a marker left behind would make the slug undeployable forever. The paste
//     delete drops the same key.
//  3. Drop identity_sites/<id>/<slug> on the {id} shard (idempotent).
func (r *ShaleRepo) DeleteSite(slug domain.Slug) error {
	siteKey := shaleKeySite(slug)
	// The owner is needed for the step-3 index drop.
	var row siteRow
	if err := r.getJSON(siteKey, &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	identity := row.Identity

	// Re-read the row IN the CAS so the blob unbinds match the row this
	// transaction actually removes: a re-deploy could have moved FileBlobs since
	// the read above.
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
		// slug_owner co-shards with sites/<slug>, so the claim the deploy staked
		// clears in the same CAS the row it belongs to does. Deleting an absent
		// key is a no-op, which covers a site deployed on a path that never
		// pre-claimed.
		if err := tx.Delete(shaleKeySlugOwner(slug)); err != nil {
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


// reconcileSiteIndexPass reprojects the identity_sites enumeration index from
// the authoritative sites/ rows across all shards: group each live site under
// its owner with the cached quota values, then add missing entries, refresh
// cached values, and drop orphans. That is what makes a site's bytes count
// after a crash between the authoritative write and the index write, and what
// enriches an entry still holding the legacy marker. Every write is an
// idempotent single-{id}-shard CAS, safe under live traffic.
func (r *ShaleRepo) reconcileSiteIndexPass() error {
	// Snapshot the index STRICTLY BEFORE the authoritative sites/ scan: a
	// baseline that predates the scan is what makes "entry unchanged" prove the
	// computed value is at least as fresh.
	siteIdx, err := r.aggregateForBackground(prefixIdentitySitesAll)
	if err != nil {
		return fmt.Errorf("reconcile sites: scan identity_sites: %w", err)
	}
	siteItems, err := r.aggregateForBackground(prefixSites)
	if err != nil {
		return fmt.Errorf("reconcile sites: scan sites: %w", err)
	}
	// sitesByOwner drives the reprojection: every authoritative site under its
	// owner's index with its cached quota values.
	sitesByOwner := make(map[string]map[string]identitySiteRow)
	// Tallied, not logged per record. See corrupt_tally.go.
	var corrupt corruptTally
	for _, item := range siteItems {
		slug := strings.TrimPrefix(string(item.Key), "sites/")
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			// Idempotent reconcile: one poisoned site row must not stall the pass
			// (Policy 1). Dropping it silently would leave a durable UNDER-count
			// if its identity_sites entry was ALSO lost, since the quota scan sums
			// the enumeration entries and an un-indexed undecodable row is
			// invisible to it. Derive the owner decode-independently from
			// slug_owner/<slug> and project a fail-closed PLACEHOLDER, so the
			// owner's next scan hard-fails instead of under-counting. See
			// docs/SPEC.md "Decode tolerance of the quota scan".
			owner := r.ownerOfSlug(domain.Slug(slug))
			if owner == "" {
				// No slug_owner: the owner cannot be derived and no enumeration
				// entry can be projected, so the row stays un-enumerated until
				// repaired. Counted rather than logged per record - the set is not
				// self-repairing, so per-record logging is an unbounded permanent
				// cost.
				corrupt.noteUnrepairable(slug)
				continue
			}
			if sitesByOwner[owner] == nil {
				sitesByOwner[owner] = make(map[string]identitySiteRow)
			}
			sitesByOwner[owner][slug] = identitySiteRow{Placeholder: true}
			corrupt.notePlaceholder(slug)
			continue
		}
		if sitesByOwner[row.Identity] == nil {
			sitesByOwner[row.Identity] = make(map[string]identitySiteRow)
		}
		sitesByOwner[row.Identity][slug] = identitySiteRow{Size: row.DedupedSize}
	}
	if line, ok := corrupt.summary("sites"); ok {
		r.repoLog().Print(line)
	}
	return r.reconcileSiteIndexes(sitesByOwner, siteIdx)
}

// reconcileSiteIndexes rebuilds the per-owner identity_sites index to match
// the authoritative sites present, one {id}-shard CAS per entry (a
// value-bearing overwrite, which is also what enriches a legacy marker entry
// to the JSON projection). have is the pass's index snapshot, captured BEFORE
// the authoritative sites/ scan. The mechanics (orphan prune, guarded
// reprojection, Policy 1 error handling) live in reconcileEnumerationIndex;
// this wrapper supplies the site family's prefix, key builder, and prune step.
func (r *ShaleRepo) reconcileSiteIndexes(sitesByOwner map[string]map[string]identitySiteRow, have []scanItem) error {
	return reconcileEnumerationIndex(r, sitesByOwner, have, prefixIdentitySitesAll,
		shaleKeyIdentitySite, r.pruneOrphanSiteEntry, "reconcile sites", "identity_sites")
}

// pruneOrphanSiteEntry classifies one identity_sites entry whose (owner,
// slug) is missing from the reprojection set, confirming against the
// authoritative row so a fresh deploy racing the pass snapshot is kept.
// A site is live or gone, so only a confirmed-gone row prunes.
func (r *ShaleRepo) pruneOrphanSiteEntry(slug string) bool {
	var row siteRow
	switch gerr := r.getJSON(shaleKeySite(domain.Slug(slug)), &row); {
	case errors.Is(gerr, ErrNotFound):
		return true
	case gerr != nil:
		// Undecodable row: keep the entry; the fail-closed placeholder
		// projection handles it.
		return false
	default:
		// Live row that raced the snapshot: keep; the next pass reprojects it.
		return false
	}
}

// ReferencedSiteBlobSHAs returns every distinct blob SHA referenced by any
// live site's manifest, aggregated across all {slug} shards. The sweep unions
// this with the paste-side referenced set, so a blob shared between records
// survives as long as ANY live record references it. A site manifest
// references a blob unconditionally (no per-file tombstone), so a live site
// with files always contributes a non-empty set.
func (r *ShaleRepo) ReferencedSiteBlobSHAs() ([]string, error) {
	sites, err := r.aggregateForBackground(prefixSites)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(sites))
	for _, item := range sites {
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			// FAIL CLOSED, never skip: skipping an undecodable site row would
			// under-count the blob keep-set, so a blob the manifest still
			// references would look orphaned and be deleted (irreversible). The
			// sweep treats any error here as "delete nothing this pass". See
			// docs/SPEC.md "Decode tolerance is per-scan-semantics", Policy 2.
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
