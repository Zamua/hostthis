//go:build slatedb

package storage_test

// The scan-derived quota reads AUTHORITATIVE version-row sizes, never the
// denormalized size cached on the identity_pastes enumeration index.
//
// docs/SPEC.md "Scan-derived quota" / "Read the authoritative size, not the
// denormalized index projection": the identity_pastes/<id>/<slug> entry caches
// name/size/created_at/expires_at so ListByOwner can render without fanning out.
// The quota scan uses that index ONLY to ENUMERATE the owner's slugs; it reads
// the SIZE from the authoritative pastes/versions rows on each slug's {slug}
// shard. This is the load-bearing decision of the whole counter->scan refactor:
// a paste's live size drifts from the cached index size after the entry is
// written (AppendVersion adds a version, DeleteVersion tombstones one), so
// summing the cached size could OVER- or UNDER-count, whereas summing the live
// version rows never can. Under-counting is the dangerous direction: it would
// let an owner slip past the cap. This test pins that the cached size is inert
// to the quota, in both the natural-drift and adversarial-corruption cases.
//
//	go test -tags slatedb -run TestShaleQuotaScanUsesAuthoritativeSize ./internal/storage
//
// Site note: the identity_sites index is a one-byte MARKER (it caches no size),
// and SumActiveSiteBytesByOwner reads DedupedSize off the authoritative
// sites/<slug> row, so the "stale denormalized size" failure mode simply does
// not exist on the site side - there is nothing to get stale. The paste index
// is the only value-bearing enumeration index, so it is the only place this
// property is meaningful, and this test covers it.
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleQuotaScanUsesAuthoritativeSize(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale authoritative-size test (start dev MinIO first)")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	owner := "key:authsz"
	slug := domain.Slug("authsz1a")

	// v1 = 300. The identity_pastes entry confirmInsert writes caches size=300.
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner),
		Kind: domain.KindHTML, ContentSHA: "sha-authsz-v1", Size: 300,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.DefaultRetentionWindow),
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert v1: %v", err)
	}
	// Append v2 = +200. The paste's live size is now 500 (both versions live),
	// but confirmAppend refreshes only the cached expiry, NOT the cached size,
	// so the index entry still says 300 - a NATURAL drift the quota must ignore.
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug, domain.KindHTML, "sha-authsz-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	repo.WaitPendingConfirms() // settle the deferred index confirm

	const wantLive = 500 // 300 (v1) + 200 (v2), both non-deleted

	// The cached index size is stale at 300 while the authoritative live sum is
	// 500. Read the raw index entry to make the drift explicit (and to prove the
	// premise: if a future change refreshed the cached size on append, this guard
	// tells us so instead of silently coupling to a stale assumption).
	idxKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	cachedSize := readCachedIndexSize(t, repo, idxKey)
	if cachedSize != 300 {
		t.Fatalf("premise: identity_pastes cached size should be the stale v1 size 300, got %d", cachedSize)
	}

	// Natural-drift case: the scan sums the authoritative version rows (500),
	// NOT the stale cached size (300).
	if got := mustSum(t, repo, owner, now); got != wantLive {
		t.Fatalf("scan must sum authoritative version rows (500), not the stale cached index size (300): got %d", got)
	}

	// Adversarial UNDER-count corruption: force the cached size to a tiny wrong
	// value. If the scan trusted the cache it would report 1 and wave through
	// uploads that should be blocked - the dangerous direction. The scan must
	// still report the authoritative 500.
	writeCachedIndexSize(t, repo, idxKey, 1)
	if got := readCachedIndexSize(t, repo, idxKey); got != 1 {
		t.Fatalf("corruption did not land: cached size %d, want 1", got)
	}
	if got := mustSum(t, repo, owner, now); got != wantLive {
		t.Fatalf("a cached index size that UNDER-counts must not deflate the quota (would let an owner exceed the cap): got %d, want %d", got, wantLive)
	}

	// Adversarial OVER-count corruption: force the cached size huge. The scan
	// must still report the authoritative 500 (a cache over-count must not
	// wrongly reject an owner's legitimate upload either).
	writeCachedIndexSize(t, repo, idxKey, 999999)
	if got := readCachedIndexSize(t, repo, idxKey); got != 999999 {
		t.Fatalf("corruption did not land: cached size %d, want 999999", got)
	}
	if got := mustSum(t, repo, owner, now); got != wantLive {
		t.Fatalf("a cached index size that OVER-counts must not inflate the quota: got %d, want %d", got, wantLive)
	}
}

// readCachedIndexSize reads the identity_pastes enumeration entry at idxKey and
// returns its cached "size" field. Fails the test if the entry is absent or
// undecodable.
func readCachedIndexSize(t *testing.T, repo *storage.ShaleRepo, idxKey []byte) int {
	t.Helper()
	raw, err := repo.GetRawForTest(idxKey)
	if err != nil {
		t.Fatalf("read index entry: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("index entry %q absent", idxKey)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode index entry: %v", err)
	}
	sz, ok := m["size"].(float64)
	if !ok {
		t.Fatalf("index entry has no numeric size field: %v", m)
	}
	return int(sz)
}

// writeCachedIndexSize overwrites the cached "size" on the identity_pastes entry
// at idxKey to want, preserving the other cached fields, and writes it back raw
// (bypassing the CAS write path, the same way the reconciler + migration tests
// seed derived-index state). It models a stale/corrupt denormalized cache: a
// valid entry whose cached size disagrees with the authoritative rows.
func writeCachedIndexSize(t *testing.T, repo *storage.ShaleRepo, idxKey []byte, want int) {
	t.Helper()
	raw, err := repo.GetRawForTest(idxKey)
	if err != nil {
		t.Fatalf("read index entry for corruption: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode index entry for corruption: %v", err)
	}
	m["size"] = want
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode corrupted index entry: %v", err)
	}
	if err := repo.PutRawForTest(idxKey, out); err != nil {
		t.Fatalf("write corrupted index entry: %v", err)
	}
}
