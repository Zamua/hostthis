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
	"encoding/json"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

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
func (r *ShaleRepo) ReconcileForTest(now time.Time, reserveGrace time.Duration) error {
	return r.Reconcile(now, reserveGrace)
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
		Kind:       string(kind),
		ContentSHA: contentSHA,
		Size:       size,
		CreatedAt:  createdAt,
		Deleted:    deleted,
	})
}

// --- derived-index keys (what the reconciler rebuilds) ---------------------

// IdentityBytesKeyForTest returns the "identity_bytes/<id>" counter key.
func IdentityBytesKeyForTest(identity string) []byte { return shaleKeyIdentityBytes(identity) }

// IdentityPasteKeyForTest returns the "identity_pastes/<id>/<slug>"
// derived-index key.
func IdentityPasteKeyForTest(identity, slug string) []byte {
	return shaleKeyIdentityPaste(identity, slug)
}

// IdentityReserveKeyForTest returns the "identity_reserve/<id>/<slug>"
// reservation-marker key. Exposed so the reconciler test can seed an
// orphan reservation and prove it is released.
func IdentityReserveKeyForTest(identity, slug string) []byte {
	return shaleKeyIdentityReserve(identity, slug)
}

// MarkerValueForTest is the non-empty placeholder value index families
// use (shale rejects empty Put values). Exposed so the migration test
// can seed the expiry index marker exactly as the backend writes it.
func MarkerValueForTest() []byte { return markerValue }
