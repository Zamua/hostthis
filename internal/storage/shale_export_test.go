package storage

// Test-only exports for the shale migration + reconciler tests, which live in
// the external storage_test package and so cannot reach ShaleRepo's unexported
// internals (the cluster handle, the row JSON shape, the derived-index keys).
// These helpers expose exactly the surface those tests need and nothing more.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/durable"
)

// ClusterForTest exposes the underlying shale cluster handle so the multi-node
// rebalance test can drive assertions a host process would not: gate on the
// post-join rebalance settling (cluster.WaitForRebalanceIdle) and assert ring
// ownership moved (cluster.Members / OwnsKey). Returns nil for a closed repo.
func (r *ShaleRepo) ClusterForTest() *cluster.Cluster { return r.cluster }

// LocalKeyCountForTest counts the keys with the given prefix physically
// resident on THIS node's local backend, bypassing ring routing. The rebalance
// test uses it to prove keys were redistributed: after a 2-node join, node B's
// local backend must hold some of the metadata keys, not zero. Pass nil prefix
// to count every local key.
func (r *ShaleRepo) LocalKeyCountForTest(prefix []byte) (int, error) {
	it, err := r.cluster.LocalScanPrefix(prefix)
	if err != nil {
		return 0, err
	}
	defer it.Close() //nolint:errcheck
	n := 0
	for {
		k, _, err := it.Next()
		if err != nil {
			return 0, err
		}
		if k == nil {
			return n, nil
		}
		n++
	}
}

// WaitForRebalanceIdleForTest blocks until every in-flight migration on this
// node has reached a terminal state or ctx is canceled.
func (r *ShaleRepo) WaitForRebalanceIdleForTest(ctx context.Context) error {
	return r.cluster.WaitForRebalanceIdle(ctx)
}

// OwnsKeyForTest reports whether THIS node is the ring owner of key.
// Diagnostic-only: lets the rebalance test pinpoint which node a routed
// op resolves to when a read unexpectedly fails.
func (r *ShaleRepo) OwnsKeyForTest(key []byte) bool { return r.cluster.OwnsKey(key) }

// LocalGetPresentForTest reports whether key is physically present on THIS
// node's local backend (bypassing routing), independent of whether the ring
// says this node owns it. Diagnoses a handoff that moved the ring pointer but
// not, or wrongly, the bytes.
func (r *ShaleRepo) LocalGetPresentForTest(key []byte) bool {
	v, err := r.cluster.LocalGet(key)
	return err == nil && v != nil
}

// PutRawForTest writes value under key straight through the cluster's Put path.
// At ReplicationFactor=1 that path calls backend.Put verbatim, with no
// last-write-wins envelope and no CAS read-set, so the bytes land exactly as a
// plain SlateDB.Put leaves them. That is what makes the migration test a
// faithful legacy raw value rather than one re-shaped by an Insert transaction.
func (r *ShaleRepo) PutRawForTest(key, value []byte) error {
	return r.cluster.Put(key, value)
}

// GetRawForTest reads value at key via the routed cluster Get, returning
// (nil, nil) when absent.
func (r *ShaleRepo) GetRawForTest(key []byte) ([]byte, error) {
	return r.getRaw(key)
}

// PutEmptyBackendForTest plants an EMPTY value under key straight on the local
// backend(s), bypassing the cluster Put path (which rejects empty values with
// cluster.ErrEmptyValue). That is the only way to reproduce the legacy
// identity_pastes shape a migrated slatedb deployment carries on disk: bare
// empty markers the shale scans still encounter even though no shale write path
// can produce them. Only meaningful on a single-node test repo, since the write
// bypasses ring routing.
func (r *ShaleRepo) PutEmptyBackendForTest(key []byte) error {
	results := r.cluster.Aggregate(func(b backend.Backend) any {
		return b.Put(key, []byte{})
	})
	for _, res := range results {
		if res.Err != nil {
			return res.Err
		}
		if err, ok := res.Value.(error); ok && err != nil {
			return err
		}
	}
	return nil
}

// DeleteRawForTest removes key straight through the cluster's Delete path. Used
// to desync a derived index (e.g. drop an identity_pastes entry whose paste
// still exists) and prove the reconciler rebuilds it.
func (r *ShaleRepo) DeleteRawForTest(key []byte) error {
	return r.cluster.Delete(key)
}

// SetGuardedIndexWriteHookForTest installs the fault-injection seam at the top
// of every guarded index write: a non-nil return from fn fails that write with
// the returned error. Pass nil to clear.
func (r *ShaleRepo) SetGuardedIndexWriteHookForTest(fn func(key []byte) error) {
	r.testHookGuardedIndexWrite = fn
}

// SetVersionsDocPlannedHookForTest installs the fault-injection seam that runs
// once a version write has planned its document state, before the transaction
// re-reads it. Damaging the document from fn is the only way to reach the
// window where the plan carries no heal seed and the transaction finds a value
// it cannot decode. Pass nil to clear.
func (r *ShaleRepo) SetVersionsDocPlannedHookForTest(fn func(slug domain.Slug)) {
	r.testHookVersionsDocPlanned = fn
}

// SetVersionsCascadeMidReadHookForTest installs the fault-injection seam
// BETWEEN the whole-paste delete's two enumeration reads. Committing a version
// from fn is the only way to test that the cascade's ExpectAbsent probe covers
// exactly the keys it will remove: the window sits behind whichever read runs
// first, so a swap of the two makes a version escape the cascade. Pass nil to
// clear.
func (r *ShaleRepo) SetVersionsCascadeMidReadHookForTest(fn func(slug domain.Slug)) {
	r.testHookVersionsCascadeMidRead = fn
}

// --- legacy-value builders (the slatedb on-disk shape) ---------------------

// LegacyPasteKeyForTest returns the authoritative "pastes/<slug>" key. The
// layout is identical across backends; only the routing differs.
func LegacyPasteKeyForTest(slug domain.Slug) []byte { return shaleKeyPaste(slug) }

// LegacyVersionKeyForTest returns the "versions/<slug>/<NNNN>" key.
func LegacyVersionKeyForTest(slug domain.Slug, ver int) []byte {
	return shaleKeyVersion(slug, ver)
}

// LegacySlugOwnerKeyForTest returns the "slug_owner/<slug>" key.
func LegacySlugOwnerKeyForTest(slug domain.Slug) []byte { return shaleKeySlugOwner(slug) }

// LegacyPasteValueForTest encodes p into the exact pasteRow JSON a slatedb
// deployment stored. The migration claim is that ShaleRepo.Get decodes this raw
// value with no conversion step.
func LegacyPasteValueForTest(p domain.Paste) ([]byte, error) {
	return json.Marshal(pasteFromDomain(p))
}

// LegacyVersionValueForTest encodes a versionRow as the legacy slatedb
// version row JSON.
func LegacyVersionValueForTest(verNum int, kind domain.ContentKind, contentSHA string, size int, createdAt time.Time, deleted bool) ([]byte, error) {
	return json.Marshal(versionRow{
		VerNum:     verNum,
		contentRef: contentRef{Kind: string(kind), ContentSHA: contentSHA, Size: size},
		CreatedAt:  createdAt,
		Deleted:    deleted,
	})
}

// --- per-owner enumeration index keys (what the reconciler reprojects) ------

// IdentityPasteKeyForTest returns the "identity_pastes/<id>/<slug>"
// per-owner enumeration-index key.
func IdentityPasteKeyForTest(identity, slug string) []byte {
	return shaleKeyIdentityPaste(identity, slug)
}

// VersionsDocKeyForTest returns the "versions_doc/<slug>" key. Deleting it
// turns a paste back into the pre-migration shape (legacy version rows only),
// which is how the heal-on-write and read-fallback tests set their stage.
func VersionsDocKeyForTest(slug domain.Slug) []byte { return shaleKeyVersionsDoc(slug) }

// LegacyVersionManifestValueForTest encodes a version row carrying an explicit
// manifest string, so a test can plant a row whose manifest is absent,
// undecodable or empty.
func LegacyVersionManifestValueForTest(verNum int, kind domain.ContentKind, contentSHA string, size int, createdAt time.Time, manifest string) ([]byte, error) {
	return json.Marshal(versionRow{
		VerNum:     verNum,
		contentRef: contentRef{Kind: string(kind), ContentSHA: contentSHA, Size: size, Manifest: manifest},
		CreatedAt:  createdAt,
	})
}

// OwnerDocKeyForTest returns the "owner_doc/<identity>" key. Deleting it turns
// an owner back into the pre-migration shape (legacy rows only), which is how
// the heal-on-write and read-fallback tests set their stage.
func OwnerDocKeyForTest(identity string) []byte { return shaleKeyOwnerDoc(identity) }

// MarkerValueForTest is the non-empty placeholder value index families use
// (shale rejects empty Put values).
func MarkerValueForTest() []byte { return markerValue }

// IntentLogForTest exposes the repo's durable log so crash-boundary tests can
// plant an intent the way a dead process would have left one.
func (r *ShaleRepo) IntentLogForTest() durable.Log { return r.intents }
