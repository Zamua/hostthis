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

// legacySiteEntry is one row of the old per-owner enumeration index.
type legacySiteEntry struct {
	Owner string
	Slug  domain.Slug
}

// LocalLegacySiteEntries returns the legacy site ENUMERATION entries stored on
// the units THIS node has mounted.
//
// The entries are what must reach zero, so they are what the sweep enumerates.
// Scanning the authoritative ROWS instead would miss the case that matters: a
// directory already converted, whose row is gone but whose entry survives. That
// entry is not a dangling pointer - it carries its own rendered values, so it
// is a second copy of the directory in its owner's listing.
//
// Node-local: cluster.LocalScanPrefix walks this node's own units, so no
// network fan-out happens even though the family spans every shard. Across the
// fleet every unit is covered, because every unit is mounted by someone.
func (r *ShaleRepo) LocalLegacySiteEntries() ([]legacySiteEntry, error) {
	var out []legacySiteEntry
	err := retryAcquiring(bootRetry, r.repoLog(), "legacy-site-sweep", func() error {
		out = nil
		it, err := r.cluster.LocalScanPrefix(prefixIdentitySitesAll)
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
			// A tombstone carries no payload; the entry is already gone.
			if payload, perr := stripEnvelope(v); perr != nil || len(payload) == 0 {
				continue
			}
			owner, slug, ok := splitIdentitySiteKey(k)
			if !ok {
				continue
			}
			out = append(out, legacySiteEntry{Owner: owner, Slug: slug})
		}
	})
	return out, err
}

// splitIdentitySiteKey pulls owner and slug back out of
// identity_sites/<owner>/<slug>. The owner contains no slash (it is an
// identity), so one split is enough.
func splitIdentitySiteKey(k []byte) (owner string, slug domain.Slug, ok bool) {
	rest, found := bytes.CutPrefix(k, prefixIdentitySitesAll)
	if !found {
		return "", "", false
	}
	o, sl, found := bytes.Cut(rest, []byte("/"))
	if !found || len(o) == 0 || len(sl) == 0 {
		return "", "", false
	}
	return string(o), domain.Slug(sl), true
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
	entries, err := a.repo.LocalLegacySiteEntries()
	if err != nil {
		return 0, err
	}
	var moved int
	var firstErr error
	for _, e := range entries {
		switch err := a.migrateLegacySite(ctx, e, now); {
		case err == nil:
			moved++
		case errors.Is(err, ErrNotFound):
			// Raced with a redeploy or a delete; nothing left to move.
		default:
			a.repo.repoLog().Printf("shale: migrating legacy site %s: %v", e.Slug, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(entries) > 0 {
		a.repo.repoLog().Printf("shale: legacy-site sweep: %d entry(s) found on mounted units, %d cleared", len(entries), moved)
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
func (a *ArtifactSites) migrateLegacySite(ctx context.Context, e legacySiteEntry, now time.Time) error {
	slug := e.Slug
	// Already converted, by a redeploy or an earlier sweep that did not finish:
	// all that is left is the entry, which is a SECOND copy of the directory in
	// its owner's listing until it goes. Checked BEFORE reading the row,
	// because in exactly this case the row is what is missing.
	if _, gerr := a.repo.Get(slug); gerr == nil {
		return a.repo.DropLegacySiteEntry(e.Owner, slug)
	} else if !errors.Is(gerr, ErrNotFound) {
		return gerr
	}
	site, err := a.legacy.Get(slug)
	if err != nil {
		// The entry outlived its row without an artifact to show for it: a
		// phantom. Dropping it is the only thing that can be done, and leaving
		// it would list a directory that cannot be served.
		if errors.Is(err, ErrNotFound) {
			return a.repo.DropLegacySiteEntry(e.Owner, slug)
		}
		return err
	}
	root, _ := site.Manifest.Lookup("/")
	if err := a.repo.InsertSupersedingSite(ctx, domain.Paste{
		Slug:       site.Slug,
		Identity:   site.Identity,
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindSite,
		ContentSHA: root.SHA,
		// The row's RECORDED charge, never a recomputation: the per-entry
		// compressed sizes were not persisted by the old layout, so deriving
		// it from the manifest yields zero and silently makes the directory
		// free. A migration must not re-price what the owner already pays.
		Size:      site.StoredBytes,
		CreatedAt: site.CreatedAt,
		UpdatedAt: site.UpdatedAt,
		Manifest:  site.Manifest,
		// 0: the sweep MOVES bytes the owner is already charged for.
	}, 0, now); err != nil {
		return err
	}
	// The entry shards on the OWNER, so it cannot ride the transaction above.
	// A failure here leaves a duplicate listing rather than a lost directory,
	// and the next sweep clears it through the already-converted path.
	return a.repo.DropLegacySiteEntry(site.Identity.String(), slug)
}
