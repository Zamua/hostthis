//go:build slatedb

package storage_test

// Never-under-count proofs for the crash-safe SITE delete's SINGLE-SLOT release
// marker (identity_site_release/<id>/<slug>).
//
// The release marker occupies ONE per-(id,slug) key that is REUSED across
// delete instances. A concurrent re-deploy + re-delete of the same slug can
// overwrite that slot between one delete's marker-write (step 2) and its consume
// (step 4). If either the hot-path consume or the reconciler's completion blindly
// decrements "whatever marker is in the slot", it can subtract bytes recorded by
// a DIFFERENT delete whose bytes are still LIVE - an UNDER-count, the one
// forbidden failure (an owner could exceed the cap). Two guards close this:
//
//   - F1 (reconciler completion): completeSiteReleaseMarker re-checks the marker's
//     age INSIDE its CAS and leaves a freshly re-stamped YOUNG marker alone
//     (mirrors dropSiteReleaseMarkerIfStale). Without it, a young marker written
//     after the reconciler's scan gets its live bytes decremented.
//   - F2 (hot-path consume): the release marker carries a per-delete NONCE;
//     consumeSiteReleaseMarker decrements + deletes ONLY the marker still carrying
//     THIS delete's nonce. Without it, a stale delete resuming its consume
//     decrements the bytes of a concurrent re-delete's marker.
//
// The reproductions are DETERMINISTIC: they seed the exact post-overwrite slot
// state with the raw helpers, then drive the completion / consume in isolation
// (CompleteSiteReleaseMarkerForTest / ConsumeSiteReleaseMarkerForTest) and assert
// the counter never drops below the authoritative truth (the sum of LIVE rows).
//
// All are //go:build slatedb and need a live MinIO; they skip unless
// MINIO_TEST_ENDPOINT is set.
//
//	go test -tags slatedb -run TestShaleSiteReleaseMarkerSlotReuse ./internal/storage

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleSiteReleaseMarkerSlotReuse(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale release-marker slot-reuse test (start dev MinIO first)")
	}

	// Generous grace: a marker stamped "now" is unambiguously in-grace, one
	// stamped far in the past is unambiguously stale.
	const reserveGrace = 10 * time.Minute

	siteSum := func(t *testing.T, sr *storage.ShaleSiteRepo, owner string, now time.Time) int64 {
		t.Helper()
		n, err := sr.SumActiveBytesByOwner(owner, now)
		if err != nil {
			t.Fatalf("SumActiveBytesByOwner(%s): %v", owner, err)
		}
		return n
	}
	releaseMarkerBytes := func(t *testing.T, repo *storage.ShaleRepo, owner, slug string) []byte {
		t.Helper()
		raw, err := repo.GetRawForTest(storage.IdentitySiteReleaseKeyForTest(owner, slug))
		if err != nil {
			t.Fatalf("read release marker: %v", err)
		}
		return raw
	}

	// --- F1: reconciler completion must NOT decrement a young re-stamped marker
	// The reconciler picked the "complete" branch from a SCAN snapshot (marker
	// past grace, slug absent). Between that scan and its CAS, the slug was
	// re-deployed (row LIVE again) and a fresh delete B re-stamped the slot with a
	// YOUNG marker recording B's live bytes. If completeSiteReleaseMarker
	// decrements that young marker, the live site under-counts (and if B crashes
	// before tombstoning, permanently, with no record left to recover from).
	t.Run("F1_reconciler_complete_leaves_young_restamped_marker", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:test-slot-f1"
			slug  = "slotf1aa"
			Y     = 700 // the re-deployed, LIVE site's bytes
		)
		// The re-deployed site is LIVE (row present) and its bytes are counted.
		s := siteOf(slug, owner, Y)
		insertSite(t, sr, s)
		if got := siteSum(t, sr, owner, fixedNow); got != Y {
			t.Fatalf("after re-deploy: site counter = %d, want %d", got, Y)
		}

		// Fresh delete B re-stamped the slot with a YOUNG marker (created_at=now)
		// carrying B's live bytes. B is between its step 2 and step 3, so the row
		// is still LIVE. This is the exact slot state the reconciler's CAS meets
		// after a scan that chose "complete".
		youngAt := fixedNow // the CAS below runs at fixedNow, so this is in-grace
		mB, err := storage.EncodeReleaseMarkerWithNonceForTest(Y, youngAt, "nonce-B")
		if err != nil {
			t.Fatalf("encode young release marker: %v", err)
		}
		mustPutRaw(t, repo, storage.IdentitySiteReleaseKeyForTest(owner, slug), mB)

		// Authoritative truth = sum of live rows = Y (the site is live).
		// Drive the reconciler's completion CAS directly at fixedNow (marker is
		// young => in grace). The in-grace re-check must leave it alone.
		if err := repo.CompleteSiteReleaseMarkerForTest(owner, slug, fixedNow, reserveGrace); err != nil {
			t.Fatalf("CompleteSiteReleaseMarkerForTest: %v", err)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != Y {
			t.Fatalf("UNDER-COUNT: completion decremented a YOUNG re-stamped marker for a LIVE site: "+
				"counter = %d, want %d (a fresh delete's young marker must be left for its own consume)", got, Y)
		}
		// The young marker must survive (left for B's consume), not be deleted.
		if len(releaseMarkerBytes(t, repo, owner, slug)) == 0 {
			t.Fatalf("completion DELETED a young re-stamped marker; it must be left for the fresh delete's consume")
		}
		// The live row must still be readable (never removed by the completion).
		if _, err := sr.Get(s.Slug); err != nil {
			t.Fatalf("live site must survive the completion, Get(%s) = %v", slug, err)
		}
		t.Logf("PROVEN (F1): completeSiteReleaseMarker's in-grace re-check left a young re-stamped marker "+
			"for a LIVE site untouched (counter stayed %d) - never under-count", Y)
	})

	// --- F2: hot-path consume must NOT decrement another delete's marker -------
	// Delete A wrote its marker with nonce A, tombstoned its (old) site, then
	// stalled before step 4. The slug was re-deployed (LIVE, bytes Y) and a fresh
	// delete B overwrote the single slot with nonce B. A now resumes its step-4
	// consume. Without the nonce gate it decrements B's bytes (still LIVE) and
	// deletes B's marker - under-counting the live site with no record left.
	t.Run("F2_consume_leaves_other_deletes_marker", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:test-slot-f2"
			slug  = "slotf2aa"
			Y     = 700 // the re-deployed, LIVE site's bytes (recorded by B's marker)
		)
		// The re-deployed site is LIVE and counted.
		s := siteOf(slug, owner, Y)
		insertSite(t, sr, s)
		if got := siteSum(t, sr, owner, fixedNow); got != Y {
			t.Fatalf("after re-deploy: site counter = %d, want %d", got, Y)
		}

		// Delete B (mid-flight between step 2 and step 3, row still LIVE) owns the
		// slot with nonce B.
		mB, err := storage.EncodeReleaseMarkerWithNonceForTest(Y, fixedNow, "nonce-B")
		if err != nil {
			t.Fatalf("encode B's release marker: %v", err)
		}
		mustPutRaw(t, repo, storage.IdentitySiteReleaseKeyForTest(owner, slug), mB)

		// Stale delete A resumes its step-4 consume with ITS OWN nonce (A). The
		// slot no longer carries A's nonce, so the consume must be a no-op.
		if err := repo.ConsumeSiteReleaseMarkerForTest(owner, slug, "nonce-A"); err != nil {
			t.Fatalf("ConsumeSiteReleaseMarkerForTest(nonce-A): %v", err)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != Y {
			t.Fatalf("UNDER-COUNT: A's consume decremented B's marker (bytes still LIVE): "+
				"counter = %d, want %d (a consume must only decrement the marker carrying ITS nonce)", got, Y)
		}
		// B's marker must survive A's consume (A must not delete another owner's).
		if len(releaseMarkerBytes(t, repo, owner, slug)) == 0 {
			t.Fatalf("A's consume DELETED B's marker; it must be left for B's own consume")
		}

		// Positive path: B's own consume DOES decrement + delete once the row is
		// tombstoned (B's step 3). Proves the nonce gate still consumes the
		// MATCHING marker - no functional regression.
		if err := repo.DeleteRawForTest(storage.SiteKeyForTest(s.Slug)); err != nil {
			t.Fatalf("tombstone sites/<slug> (B's step 3): %v", err)
		}
		if err := repo.ConsumeSiteReleaseMarkerForTest(owner, slug, "nonce-B"); err != nil {
			t.Fatalf("ConsumeSiteReleaseMarkerForTest(nonce-B): %v", err)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != 0 {
			t.Fatalf("nonce-match consume did not decrement: counter = %d, want 0", got)
		}
		if len(releaseMarkerBytes(t, repo, owner, slug)) != 0 {
			t.Fatalf("nonce-match consume left B's marker behind; step 4 must delete it")
		}
		t.Logf("PROVEN (F2): a stale delete's consume LEFT another delete's marker untouched (counter stayed %d), "+
			"and the nonce-matching consume decremented exactly once (-> 0)", Y)
	})

	// --- F3: a nonce-bearing crash-durable delete still heals + double-reconcile
	// idempotent. The tombstone committed (row ABSENT) but the decrement was lost;
	// the marker carries a nonce (as the real DeleteSite now writes). The
	// reconciler must complete it once (counter -> 0), and a SECOND pass must not
	// double-decrement.
	t.Run("F3_nonce_marker_heals_and_double_reconcile_idempotent", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:test-slot-f3"
			slug  = "slotf3aa"
			X     = 500
		)
		s := siteOf(slug, owner, X)
		insertSite(t, sr, s)
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after deploy: site counter = %d, want %d", got, X)
		}

		// Simulate a crash AFTER the tombstone but BEFORE step 4: a PAST-GRACE
		// nonce-bearing release marker, row + expiry gone, counter not decremented.
		staleAt := fixedNow // past grace relative to the reconcile time below
		marker, err := storage.EncodeReleaseMarkerWithNonceForTest(X, staleAt, "nonce-crashed")
		if err != nil {
			t.Fatalf("encode nonce release marker: %v", err)
		}
		mustPutRaw(t, repo, storage.IdentitySiteReleaseKeyForTest(owner, slug), marker)
		if err := repo.DeleteRawForTest(storage.SiteKeyForTest(s.Slug)); err != nil {
			t.Fatalf("tombstone sites/<slug>: %v", err)
		}
		if err := repo.DeleteRawForTest(storage.ExpirySiteKeyForTest(s.ExpiresAt, s.Slug)); err != nil {
			t.Fatalf("tombstone expiry_sites/<ts>/<slug>: %v", err)
		}
		if _, err := sr.Get(s.Slug); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("after tombstone, Get(%s) = %v, want ErrNotFound", slug, err)
		}

		reconcileAt := fixedNow.Add(time.Hour) // now - staleAt = 1h > grace
		if err := repo.ReconcileForTest(reconcileAt, reserveGrace); err != nil {
			t.Fatalf("first ReconcileForTest: %v", err)
		}
		if got := siteSum(t, sr, owner, reconcileAt); got != 0 {
			t.Fatalf("HEAL FAILED: nonce-bearing crash-durable delete not healed: counter = %d, want 0", got)
		}
		if len(releaseMarkerBytes(t, repo, owner, slug)) != 0 {
			t.Fatalf("release marker not consumed post-reconcile")
		}

		// Double reconcile: the consumed marker is absent, so the completion CAS
		// is a no-op - no double-decrement.
		if err := repo.ReconcileForTest(reconcileAt.Add(time.Minute), reserveGrace); err != nil {
			t.Fatalf("second ReconcileForTest: %v", err)
		}
		if got := siteSum(t, sr, owner, reconcileAt.Add(time.Minute)); got != 0 {
			t.Fatalf("DOUBLE-RECONCILE UNSAFE: counter moved on the second pass to %d, want 0", got)
		}
		t.Logf("PROVEN (F3): a nonce-bearing release marker healed the crash-durable delete (counter -> 0) " +
			"and a double reconcile did not double-decrement")
	})
}
