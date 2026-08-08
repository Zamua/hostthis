// Remembering which blob objects an upload staged, so recovery can reclaim an
// exact list instead of scanning for what looks unreferenced.

package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
)

// shaleKeyStaged builds staged/<slug>/<blobid>.
//
// Sharded on the SLUG, which puts these on the same shard as the paste row, the
// bref they describe and the blobowner fence that guards them - everything about
// one upload in one place. Recovery finds the slug on the intent it is
// resolving, so reaching them is a routed read rather than a scan.
//
// Blobid last so each staged object is its own key: appending a record is O(1),
// where a growing value would be rewritten once per file.
func shaleKeyStaged(slug, blobID string) []byte {
	return []byte("staged/" + slug + "/" + blobID)
}

// shalePrefixStaged is every staged record for one upload.
func shalePrefixStaged(slug string) []byte {
	return []byte("staged/" + slug + "/")
}

// RecordStagedRef durably remembers one staged blob object before the upload
// moves on to the next file.
//
// The ref is encoded WHOLE, never field by field. shale derives both the
// bound-ref guard read and the delete key from the ref's fields, so a field
// this round-trip loses or corrupts does not fail loudly - it addresses a
// different key than BindBlob wrote, reads as "unbound" for a blob that is
// bound, and deletes committed bytes. shale's validation catches a MISSING
// key-forming field; a corrupted-but-present route shard it cannot. Marshalling
// the struct is also what lets a field shale adds later ride along without this
// code being taught about it.
func (r *ShaleRepo) RecordStagedRef(_ context.Context, slug domain.Slug, ref cluster.BlobRef) error {
	if ref.BlobID == "" {
		return fmt.Errorf("shale: record staged ref for %s: empty blob id", slug)
	}
	enc, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("shale: encode staged ref for %s: %w", slug, err)
	}
	key := shaleKeyStaged(slug.String(), ref.BlobID)
	return r.cluster.Transact(key, func(tx backend.Transaction) error {
		return tx.Put(key, enc)
	})
}

// StagedRefs returns every ref recorded for one upload.
//
// A record that will not decode is returned as an error rather than skipped:
// the caller deletes bytes from this list, and silently dropping an
// unreadable entry would leak rather than mislead - but silently dropping a
// PARTIALLY readable one could delete the wrong object, so neither is a choice
// this function is entitled to make.
func (r *ShaleRepo) StagedRefs(slug domain.Slug) ([]cluster.BlobRef, error) {
	prefix := shalePrefixStaged(slug.String())
	var out []cluster.BlobRef
	err := retryAcquiring(bootRetry, r.repoLog(), "staged-refs", func() error {
		out = nil
		it, err := r.cluster.ScanPrefix(prefix)
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
			payload, perr := stripEnvelope(v)
			if perr != nil {
				return fmt.Errorf("decode staged record %s: %w", k, perr)
			}
			if len(payload) == 0 {
				continue // tombstone: the record is already cleared
			}
			var ref cluster.BlobRef
			if uerr := json.Unmarshal(payload, &ref); uerr != nil {
				return fmt.Errorf("decode staged record %s: %w", k, uerr)
			}
			out = append(out, ref)
		}
	})
	return out, err
}

// ClearStagedRefs drops an upload's staged records.
//
// Called when the write COMMITS: those bytes are bound now, and a record that
// outlives its commit is a standing instruction to delete live data. Also
// called after recovery unstages them, where it is ordinary cleanup.
//
// Idempotent, so a partial clear converges on the next pass.
func (r *ShaleRepo) ClearStagedRefs(_ context.Context, slug domain.Slug) error {
	prefix := shalePrefixStaged(slug.String())
	var keys [][]byte
	err := retryAcquiring(bootRetry, r.repoLog(), "staged-refs-clear", func() error {
		keys = nil
		it, err := r.cluster.ScanPrefix(prefix)
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
			keys = append(keys, bytes.Clone(k))
		}
	})
	if err != nil {
		return err
	}
	for _, k := range keys {
		if derr := r.cluster.Transact(k, func(tx backend.Transaction) error {
			return tx.Delete(k)
		}); derr != nil {
			return fmt.Errorf("clear staged record %s: %w", k, derr)
		}
	}
	return nil
}
