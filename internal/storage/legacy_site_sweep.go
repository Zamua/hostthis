// Draining the pre-unification site family.
//
// A directory deployed before the collapse migrates when it is redeployed, but
// one nobody touches would sit on the legacy read path forever - and the old
// family cannot be deleted while any row remains. This sweep converts the rest.

package storage

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// LocalLegacySiteSlugs returns the legacy site slugs stored on the units THIS
// node has mounted.
//
// Node-local: cluster.LocalScanPrefix walks this node's own units, so no
// network fan-out happens even though the family spans every shard. Across the
// fleet every unit is covered, because every unit is mounted by someone.
func (r *ShaleRepo) LocalLegacySiteSlugs() ([]domain.Slug, error) {
	var out []domain.Slug
	err := retryAcquiring(bootRetry, r.repoLog(), "legacy-site-sweep", func() error {
		out = nil
		it, err := r.cluster.LocalScanPrefix(prefixSites)
		if err != nil {
			return err
		}
		defer it.Close() //nolint:errcheck
		for {
			k, v, err := it.Next()
			if err != nil {
				return err
			}
			if k == nil && v == nil {
				return nil
			}
			// A tombstone carries no payload; the row is already gone.
			if payload, perr := stripEnvelope(v); perr != nil || len(payload) == 0 {
				continue
			}
			slug, ok := bytes.CutPrefix(k, prefixSites)
			if !ok || len(slug) == 0 {
				continue
			}
			out = append(out, domain.Slug(slug))
		}
	})
	return out, err
}

// SweepLegacySites migrates every legacy directory this node can see onto the
// artifact families, and reports how many it moved.
//
// Call it after the node is serving, not before: converting a row writes to the
// artifact families, which live on DIFFERENT shards that may not be mounted
// anywhere yet during a cold start.
//
// Safe to run concurrently with other nodes and with live traffic. A directory
// already migrated is skipped, and a redeploy racing the sweep wins on its own
// merits - both paths write the same artifact through the same insert, and the
// slug's cross-family claim serializes them.
//
// One bad row does not stop the sweep: this runs at boot on a serving node, so
// a single unconvertible record must not deny the drain to every other.
func (a *ArtifactSites) SweepLegacySites(ctx context.Context, now time.Time) (int, error) {
	if a.legacy == nil {
		return 0, nil
	}
	slugs, err := a.repo.LocalLegacySiteSlugs()
	if err != nil {
		return 0, err
	}
	var moved int
	var firstErr error
	for _, slug := range slugs {
		switch err := a.migrateLegacySite(ctx, slug, now); {
		case err == nil:
			moved++
		case errors.Is(err, ErrNotFound):
			// Raced with a redeploy or a delete; nothing left to move.
		default:
			a.repo.repoLog().Printf("shale: migrating legacy site %s: %v", slug, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(slugs) > 0 {
		a.repo.repoLog().Printf("shale: legacy-site sweep: %d found on mounted units, %d migrated", len(slugs), moved)
	}
	return moved, firstErr
}

// migrateLegacySite converts ONE row, atomically: the artifact is written and
// the legacy row deleted in the same transaction, so the directory is never in
// both families at once (listed twice, charged twice) and never in neither.
//
// The charged size is the row's own deduped total, carried over verbatim: a
// migration must not re-price what the owner is already paying. The cap is
// skipped for the same reason - these bytes are already counted.
func (a *ArtifactSites) migrateLegacySite(ctx context.Context, slug domain.Slug, now time.Time) error {
	site, err := a.legacy.Get(slug)
	if err != nil {
		return err
	}
	// Already converted, by a redeploy or an earlier sweep that did not finish:
	// all that is left is the owner-sharded entry, which is a SECOND copy of the
	// directory in that owner's listing until it goes.
	if _, gerr := a.repo.Get(slug); gerr == nil {
		return a.repo.DropLegacySiteEntry(site.Identity.String(), slug)
	} else if !errors.Is(gerr, ErrNotFound) {
		return gerr
	}
	root, _ := site.Manifest.Lookup("/")
	if err := a.repo.InsertSupersedingSite(ctx, domain.Paste{
		Slug:       site.Slug,
		Identity:   site.Identity,
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindSite,
		ContentSHA: root.SHA,
		Size:       site.Manifest.CompressedDedupedSize(),
		CreatedAt:  site.CreatedAt,
		UpdatedAt:  site.UpdatedAt,
		Manifest:   site.Manifest,
	}, now); err != nil {
		return err
	}
	// The entry shards on the OWNER, so it cannot ride the transaction above.
	// A failure here leaves a duplicate listing rather than a lost directory,
	// and the next sweep clears it through the already-converted path.
	return a.repo.DropLegacySiteEntry(site.Identity.String(), slug)
}
