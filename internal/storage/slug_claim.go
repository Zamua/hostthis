// Staking a slug before a directory upload consumes its stream.

package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

// PreClaimSlug stakes a metadata-only claim on slug BEFORE a transactional
// directory deploy consumes its untar stream, so every file stages under the
// {slug} shard and the pointers co-bind with the manifest at commit.
//
// A single {slug}-shard CAS writing slug_owner/<slug> IFF the slug is free and
// not already pre-claimed. Both reads join the read-set, so concurrent claims
// serialize and one loses. Returns ErrSlugTaken on any collision; the caller
// re-mints and re-claims.
//
// A claim is durable and no sweep reclaims one, so a deploy that does not commit
// under the slug MUST hand it back via ReleaseSlugClaim or the slug is spent.
// Only a crash in that window leaves a marker with no artifact, and a later
// insert minting the slug overwrites it unconditionally.
//
// owner and now are accepted for symmetry with the seam; a claim is a stake, not
// a byte reservation, so it charges no quota.
func (r *ShaleRepo) PreClaimSlug(_ context.Context, slug domain.Slug, owner string, _ time.Time) error {
	pasteKey := shaleKeyPaste(slug)
	ownerKey := shaleKeySlugOwner(slug)
	return r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
		// Each Get joins the CAS read-set so a racing insert conflicts.
		if _, err := tx.Get(pasteKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("preclaim artifact check: %w", err)
		}
		// slug_owner present means an artifact owns the slug OR a concurrent
		// deploy claimed it first; either way it is taken.
		if _, err := tx.Get(ownerKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("preclaim slug_owner check: %w", err)
		}
		return tx.Put(ownerKey, []byte(owner))
	})
}

// ReleaseSlugClaim hands a pre-claimed slug back when the deploy that staked it
// never committed, so an aborted deploy does not burn the slug: PreClaimSlug
// rejects any slug carrying a marker, and nothing else removes one.
//
// A single {slug}-shard CAS deleting slug_owner/<slug> IFF it still holds
// owner's stake AND no artifact exists. Both reads join the read-set, so a
// racing insert that makes the marker load-bearing conflicts instead of losing
// its owner pointer, and an ambiguous commit that actually landed keeps its
// marker. A missing or foreign claim is a no-op, so a repeated release is
// harmless.
func (r *ShaleRepo) ReleaseSlugClaim(_ context.Context, slug domain.Slug, owner string) error {
	pasteKey := shaleKeyPaste(slug)
	ownerKey := shaleKeySlugOwner(slug)
	return r.cluster.Transact(pasteKey, func(tx backend.Transaction) error {
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
			return nil // the deploy landed after all
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("release artifact check: %w", err)
		}
		return tx.Delete(ownerKey)
	})
}
