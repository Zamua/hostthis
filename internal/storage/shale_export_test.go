//go:build slatedb

package storage

// Test-only exports for the shale migration + reconciler tests, which
// live in the external storage_test package and so cannot reach
// ShaleRepo's unexported internals (the cluster handle, the row JSON
// shape, the derived-index keys). These helpers expose exactly the
// surface those tests need and nothing more: a raw non-CAS write that
// mirrors a legacy slatedb deployment's plain db.Put, builders for the
// legacy paste/version keys + their JSON bytes, the derived-index keys,
// a raw read for asserting the counter/index, and Reconcile.
//
// All helpers are //go:build slatedb (the only build that compiles
// ShaleRepo), so the default build never sees them.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
)

// ClusterForTest exposes the underlying shale cluster handle so the
// multi-node rebalance test (external storage_test package) can drive
// assertions a host process would not: gate on the post-join rebalance
// settling (cluster.WaitForRebalanceIdle) and assert ring ownership moved
// (cluster.Members / OwnsKey). The peer-forwarding rpc.Server is now stood
// up by NewShaleRepo itself in multi-node mode (the production path), so
// the test no longer registers one through this handle.
//
// Returns nil for a closed repo. Test-only; no production path reads it.
func (r *ShaleRepo) ClusterForTest() *cluster.Cluster { return r.cluster }

// LocalKeyCountForTest counts the keys with the given prefix physically
// resident on THIS node's local backend, bypassing ring routing
// (cluster.LocalScanPrefix). The rebalance test uses it to prove keys
// were redistributed: after a 2-node join, node B's local backend must
// hold some of the metadata keys (the ring moved roughly half of them),
// not zero. Pass nil prefix to count every local key.
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

// WaitForRebalanceIdleForTest blocks until every in-flight migration on
// this node has reached a terminal state or ctx is canceled. Thin
// pass-through to cluster.WaitForRebalanceIdle so the test does not need
// the cluster handle's full surface.
func (r *ShaleRepo) WaitForRebalanceIdleForTest(ctx context.Context) error {
	return r.cluster.WaitForRebalanceIdle(ctx)
}

// OwnsKeyForTest reports whether THIS node is the ring owner of key.
// Diagnostic-only: lets the rebalance test pinpoint which node a routed
// op resolves to when a read unexpectedly fails.
func (r *ShaleRepo) OwnsKeyForTest(key []byte) bool { return r.cluster.OwnsKey(key) }

// LocalGetPresentForTest reports whether key is physically present on
// THIS node's local backend (bypassing routing), independent of whether
// the ring says this node owns it. Used to diagnose a handoff that moved
// the ring pointer but not (or wrongly) the bytes.
func (r *ShaleRepo) LocalGetPresentForTest(key []byte) bool {
	v, err := r.cluster.LocalGet(key)
	return err == nil && v != nil
}

// PutRawForTest writes value under key straight through the cluster's
// Put path. At ReplicationFactor=1 that path calls backend.Put verbatim
// with no last-write-wins envelope wrapping and no CAS read-set, so the
// bytes land exactly as a pre-shale slatedb deployment's plain
// SlateDB.Put would have left them. This is what makes the migration
// test a faithful "legacy raw value" rather than a value already
// re-shaped by an Insert transaction.
func (r *ShaleRepo) PutRawForTest(key, value []byte) error {
	return r.cluster.Put(key, value)
}

// GetRawForTest reads value at key via the routed cluster Get, returning
// (nil, nil) when absent. Used to assert the raw counter / index bytes.
func (r *ShaleRepo) GetRawForTest(key []byte) ([]byte, error) {
	return r.getRaw(key)
}

// DeleteRawForTest removes key straight through the cluster's Delete
// path. Used by the reconciler test to desync a derived index (e.g.
// drop an identity_pastes entry whose paste still exists) and prove the
// reconciler rebuilds it.
func (r *ShaleRepo) DeleteRawForTest(key []byte) error {
	return r.cluster.Delete(key)
}

// ReconcileForTest exposes Reconcile under a stable test name (Reconcile
// is already exported, but routing the reconciler test through this
// helper keeps the test's dependency surface explicit).
func (r *ShaleRepo) ReconcileForTest(now time.Time) error {
	return r.Reconcile(now)
}

// --- legacy-value builders (the slatedb on-disk shape) ---------------------

// LegacyPasteKeyForTest returns the authoritative paste key for slug, the
// same "pastes/<slug>" name a slatedb deployment used (the layout is
// unchanged across backends; only the routing differs).
func LegacyPasteKeyForTest(slug domain.Slug) []byte { return shaleKeyPaste(slug) }

// LegacyVersionKeyForTest returns the "versions/<slug>/<NNNN>" key.
func LegacyVersionKeyForTest(slug domain.Slug, ver int) []byte {
	return shaleKeyVersion(slug, ver)
}

// LegacySlugOwnerKeyForTest returns the "slug_owner/<slug>" key.
func LegacySlugOwnerKeyForTest(slug domain.Slug) []byte { return shaleKeySlugOwner(slug) }

// LegacyExpiryKeyForTest returns the "expiry/<rfc3339>/<slug>" key.
func LegacyExpiryKeyForTest(expiresAt time.Time, slug domain.Slug) []byte {
	return shaleKeyExpiry(expiresAt, slug)
}

// LegacyPasteValueForTest encodes p into the exact JSON a slatedb paste
// row used (pasteRow). The migration claim is that ShaleRepo.Get decodes
// this raw value with no conversion step.
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

// IdentitySiteKeyForTest returns the "identity_sites/<id>/<slug>" per-owner
// site enumeration-index key (the {id}-shard entry a deploy writes, DeleteSite
// drops, and the reconciler reprojects). Exposed so the reconciler test can
// drop it while the authoritative site still exists and prove the reconciler
// rebuilds it (closing the crash-between-row-and-index under-count window).
func IdentitySiteKeyForTest(identity, slug string) []byte {
	return shaleKeyIdentitySite(identity, slug)
}

// MarkerValueForTest is the non-empty placeholder value index families
// use (shale rejects empty Put values). Exposed so the migration test
// can seed the expiry index marker exactly as the backend writes it.
func MarkerValueForTest() []byte { return markerValue }

// RoomExpiryKeyForTest returns the "roomexpiry/<ts>/<app-slug>/<uuid>" sweep
// index key, exactly as the backend writes it. Exposed so the orphan-drain
// test can plant an entry with NO room record behind it and prove one expiry
// pass removes the exact observed entry.
func RoomExpiryKeyForTest(expiresAt time.Time, appSlug domain.Slug, id domain.RoomID) []byte {
	return shaleKeyRoomExpiry(expiresAt, appSlug, id)
}

// SiteKeyForTest returns the "sites/<slug>" authoritative site row key.
// Exposed so the decode-tolerance test can seed a poisoned site row and prove
// Reconcile's site-index reprojection skips + continues (Policy 1).
func SiteKeyForTest(slug domain.Slug) []byte { return shaleKeySite(slug) }
