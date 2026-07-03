//go:build slatedb

package storage_test

// Crash-durable-delete + markerless-residual behavior for the shale SITE quota
// counter.
//
// A site DELETE is NOT one transaction: it spans the {slug} shard (the
// authoritative sites/<slug> row) and the {id} shard (the identity_site_bytes
// counter), which cannot be one CAS. The crash-durable protocol makes the two
// halves symmetric with the reserve path: DeleteSite writes a RELEASE MARKER on
// the {id} shard (identity_site_release/<id>/<slug>, value {bytes, created_at})
// BEFORE the authoritative tombstone, then decrements+consumes the marker in a
// final {id} CAS. If the process crashes AFTER the tombstone committed but
// BEFORE that decrement, the marker survives and the reconciler completes it
// (counter -= marker.bytes, at most once via the marker-consume).
//
// The subtests:
//
//   - A (LEGACY markerless residual, still unhealed): a half-committed delete
//     that removed the row + indexes but left NO release marker (a pre-marker
//     delete, or an operator's manual surgery). The reconciler still cannot heal
//     it - the counter only moves via a marker-driven {id} delta, and there is
//     no marker. This residual is what enhancement #2 (the offline audit)
//     exists to correct; the reconciler deliberately never derives the counter
//     from a cross-shard scan online. A normal delete no longer produces this
//     state (it writes a marker - see C).
//   - B (control, reserve marker healed): an orphaned RESERVE marker IS released
//     by the reconciler, isolating A's non-healing to the missing marker.
//   - C (crash-durable delete healed): a half-committed delete WITH a release
//     marker (tombstone + marker written, counter decrement lost) IS now healed
//     by the reconciler (counter -> 0), and a DOUBLE reconcile does NOT
//     double-decrement.
//   - D (never-under-count guard): a release marker whose site row is STILL
//     PRESENT (a delete that crashed before its tombstone, or a re-deploy) is
//     DROPPED without decrementing - the live bytes stay counted. This is the
//     invariant that ranks all others: the counter must never under-count.
//   - E (hot-path consume): the real DeleteSite writes AND consumes its own
//     marker, leaving the counter correct and NO lingering marker.
//
// All are //go:build slatedb and need a live MinIO; they skip cleanly unless
// MINIO_TEST_ENDPOINT is set.
//
//	go test -tags slatedb -run TestShaleMarkerlessResidual ./internal/storage

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleMarkerlessResidual(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale markerless-residual test (start dev MinIO first)")
	}

	// A generous grace so no marker is ever mistaken for in-flight; the
	// residual case has no marker at all, and the marker-backed cases stamp
	// their markers far in the past on purpose.
	const reserveGrace = 10 * time.Minute

	// siteSum reads the owner's SITE counter (identity_site_bytes/<id>) via the
	// public sum, the single-read the quota check trusts.
	siteSum := func(t *testing.T, sr *storage.ShaleSiteRepo, owner string, now time.Time) int64 {
		t.Helper()
		n, err := sr.SumActiveBytesByOwner(owner, now)
		if err != nil {
			t.Fatalf("SumActiveBytesByOwner(%s): %v", owner, err)
		}
		return n
	}

	// --- Subtest A: LEGACY markerless residual is NOT healed -----------------
	t.Run("A_markerless_residual_unhealed", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:test-markerless"
			slug  = "mkless11"
			X     = 500 // the site's DedupedSize == the bytes charged to the counter
		)
		s := siteOf(slug, owner, X) // stamped at fixedNow with the standard retention window
		insertSite(t, sr, s)

		// The counter is set to X after a clean deploy (confirm ran synchronously,
		// so no reserve marker lingers).
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after deploy: site counter = %d, want %d", got, X)
		}

		// --- simulate a LEGACY half-committed delete (NO release marker) ------
		// A pre-marker delete (or manual surgery) removed sites/<slug>, the
		// identity_sites/<id>/<slug> index entry, and the expiry_sites/<ts>/<slug>
		// entry but LEFT the counter untouched, WITHOUT writing a release marker.
		// A crash-durable delete would have left a marker (subtest C); this one
		// models the pre-marker world / an operator edit, so it is truly
		// markerless.
		if err := repo.DeleteRawForTest(storage.SiteKeyForTest(s.Slug)); err != nil {
			t.Fatalf("delete sites/<slug>: %v", err)
		}
		if err := repo.DeleteRawForTest(storage.IdentitySiteKeyForTest(owner, slug)); err != nil {
			t.Fatalf("delete identity_sites/<id>/<slug>: %v", err)
		}
		if err := repo.DeleteRawForTest(storage.ExpirySiteKeyForTest(s.ExpiresAt, s.Slug)); err != nil {
			t.Fatalf("delete expiry_sites/<ts>/<slug>: %v", err)
		}

		// The authoritative row is gone from every read surface...
		if _, err := sr.Get(s.Slug); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("after half-commit, Get(%s) = %v, want ErrNotFound (row must be gone)", slug, err)
		}
		if list, err := sr.ListSitesByOwner(owner, fixedNow); err != nil {
			t.Fatalf("ListSitesByOwner: %v", err)
		} else if len(list) != 0 {
			t.Fatalf("after half-commit, ListSitesByOwner = %d sites, want 0 (row must be gone)", len(list))
		}
		// ...yet the counter STILL reads X (the lost decrement left the bytes behind).
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after half-commit: site counter = %d, want %d (the bytes must still be counted with no row)", got, X)
		}

		// --- reconcile past grace: the residual must survive (no marker) ------
		reconcileAt := fixedNow.Add(time.Hour) // well past grace; there are no markers anyway
		if err := repo.ReconcileForTest(reconcileAt, reserveGrace); err != nil {
			t.Fatalf("ReconcileForTest: %v", err)
		}

		got := siteSum(t, sr, owner, reconcileAt)
		if got == 0 {
			t.Fatalf("UNEXPECTED: the reconciler HEALED a MARKERLESS residual (counter dropped %d -> 0). "+
				"A markerless residual must stay put; the release reconciler only ever moves the counter via a release MARKER.", X)
		}
		if got != X {
			t.Fatalf("markerless residual moved unexpectedly: counter = %d, want %d (reconcile should leave it untouched)", got, X)
		}
		t.Logf("PROVEN: LEGACY markerless residual UNHEALED - site counter still %d after a past-grace reconcile "+
			"(no release marker exists for the reconciler to complete; this is the offline-audit's job)", got)
	})

	// --- Subtest B: control - a leaked/orphaned RESERVE marker *is* healed ----
	t.Run("B_orphaned_marker_healed", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:test-leaked-marker"
			slug  = "lkmark11"
			X     = 500
		)
		now := fixedNow

		// Manufacture an ABANDONED site reservation: a marker at
		// identity_site_reserve/<id>/<slug> stamped OLDER than grace, its bytes
		// folded into the counter exactly as reserveSiteBytes would have, and NO
		// authoritative sites/<slug> row (the deploy was abandoned before the
		// authoritative write). This is the reserve twin of subtest C's release
		// residual: the bytes are over-counted, but here a RESERVE marker records
		// them.
		staleAt := now.Add(-(reserveGrace + time.Hour))
		marker, err := storage.EncodeReservationMarkerForTest(X, staleAt)
		if err != nil {
			t.Fatalf("encode reservation marker: %v", err)
		}
		mustPutRaw(t, repo, storage.IdentitySiteReserveKeyForTest(owner, slug), marker)
		mustPutRaw(t, repo, storage.IdentitySiteBytesKeyForTest(owner), []byte("500"))

		// Baseline: the counter is inflated by the abandoned reservation.
		if got := siteSum(t, sr, owner, now); got != X {
			t.Fatalf("baseline: site counter = %d, want %d", got, X)
		}

		// --- reconcile: the past-grace orphan marker must be released ---------
		if err := repo.ReconcileForTest(now, reserveGrace); err != nil {
			t.Fatalf("ReconcileForTest: %v", err)
		}

		// The counter drops to 0 (orphan-released) ...
		if got := siteSum(t, sr, owner, now); got != 0 {
			t.Fatalf("CONTROL FAILED: reconcile did NOT heal the orphaned reserve marker: counter = %d, want 0 "+
				"(a past-grace reserve marker for an absent site must be orphan-released)", got)
		}
		// ... and the marker itself is gone (no unbounded leak).
		if raw, err := repo.GetRawForTest(storage.IdentitySiteReserveKeyForTest(owner, slug)); err != nil {
			t.Fatalf("read marker post-reconcile: %v", err)
		} else if len(raw) != 0 {
			t.Fatalf("orphan reserve marker not deleted post-reconcile: got %q", raw)
		}
		t.Logf("CONTROL PROVEN: the reconciler orphan-released a past-grace reserve marker (counter %d -> 0), "+
			"so it DOES cure marker-backed drift; subtest A's non-healing is specifically the MISSING marker", X)
	})

	// --- Subtest C: crash-durable delete IS healed by the release marker ------
	t.Run("C_release_marker_half_commit_healed", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:test-release-heal"
			slug  = "relheal1"
			X     = 500
		)
		s := siteOf(slug, owner, X)
		insertSite(t, sr, s)
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after deploy: site counter = %d, want %d", got, X)
		}

		// --- simulate a HALF-COMMITTED crash-durable delete -------------------
		// The delete's protocol: step 2 writes the release marker on the {id}
		// shard, step 3 tombstones the row on the {slug} shard, step 4 consumes
		// the marker (counter -= bytes). Model a crash AFTER step 3 but BEFORE
		// step 4: the release marker is present (past grace), the row + expiry
		// index are gone, and the counter is NOT yet decremented. (The
		// identity_sites enumeration index is left in place - step 4 is what drops
		// it - which is faithful to the interrupt point and irrelevant to the
		// counter, which reads identity_site_bytes.)
		staleAt := fixedNow // stamped in the past relative to the reconcile time below
		marker, err := storage.EncodeReservationMarkerForTest(X, staleAt)
		if err != nil {
			t.Fatalf("encode release marker: %v", err)
		}
		mustPutRaw(t, repo, storage.IdentitySiteReleaseKeyForTest(owner, slug), marker)
		if err := repo.DeleteRawForTest(storage.SiteKeyForTest(s.Slug)); err != nil {
			t.Fatalf("tombstone sites/<slug>: %v", err)
		}
		if err := repo.DeleteRawForTest(storage.ExpirySiteKeyForTest(s.ExpiresAt, s.Slug)); err != nil {
			t.Fatalf("tombstone expiry_sites/<ts>/<slug>: %v", err)
		}

		// The row is gone but the counter still reads X (the lost decrement).
		if _, err := sr.Get(s.Slug); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("after tombstone, Get(%s) = %v, want ErrNotFound", slug, err)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after half-commit: site counter = %d, want %d (decrement not yet applied)", got, X)
		}

		// --- reconcile past grace: the release marker must complete the heal --
		reconcileAt := fixedNow.Add(time.Hour) // now - staleAt = 1h > grace
		if err := repo.ReconcileForTest(reconcileAt, reserveGrace); err != nil {
			t.Fatalf("ReconcileForTest: %v", err)
		}
		if got := siteSum(t, sr, owner, reconcileAt); got != 0 {
			t.Fatalf("HEAL FAILED: crash-durable delete NOT healed by the release marker: counter = %d, want 0 "+
				"(a past-grace release marker whose site is absent must complete the lost decrement)", got)
		}
		// The marker itself is consumed (no unbounded leak).
		if raw, err := repo.GetRawForTest(storage.IdentitySiteReleaseKeyForTest(owner, slug)); err != nil {
			t.Fatalf("read release marker post-reconcile: %v", err)
		} else if len(raw) != 0 {
			t.Fatalf("release marker not deleted post-reconcile: got %q", raw)
		}

		// --- DOUBLE reconcile: must NOT double-decrement ----------------------
		// The counter is at 0; a second pass finds the marker already consumed
		// (absent), so the read-checked CAS is a no-op. This is the at-most-once
		// guarantee that keeps the completion from ever under-counting.
		if err := repo.ReconcileForTest(reconcileAt.Add(time.Minute), reserveGrace); err != nil {
			t.Fatalf("second ReconcileForTest: %v", err)
		}
		if got := siteSum(t, sr, owner, reconcileAt.Add(time.Minute)); got != 0 {
			t.Fatalf("DOUBLE-RECONCILE UNSAFE: counter moved on the second pass to %d, want 0 "+
				"(a consumed release marker must never be re-applied)", got)
		}
		t.Logf("PROVEN: the release marker HEALED the crash-durable delete (counter %d -> 0), and a double reconcile "+
			"did not double-decrement (marker consumed exactly once)", X)
	})

	// --- Subtest D: release marker with a LIVE row must NOT decrement ---------
	t.Run("D_release_marker_row_present_not_decremented", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:test-release-live"
			slug  = "rellive1"
			X     = 500
		)
		s := siteOf(slug, owner, X)
		insertSite(t, sr, s)
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after deploy: site counter = %d, want %d", got, X)
		}

		// A release marker whose site row is STILL PRESENT: a delete that crashed
		// BETWEEN its marker write (step 2) and its tombstone (step 3), or a slug
		// that was re-deployed. The bytes are LIVE and correctly counted, so the
		// reconciler must DROP the marker WITHOUT decrementing (decrementing here
		// would under-count the live site - the invariant that ranks all others).
		staleAt := fixedNow
		marker, err := storage.EncodeReservationMarkerForTest(X, staleAt)
		if err != nil {
			t.Fatalf("encode release marker: %v", err)
		}
		mustPutRaw(t, repo, storage.IdentitySiteReleaseKeyForTest(owner, slug), marker)

		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("baseline with live row + marker: site counter = %d, want %d", got, X)
		}

		reconcileAt := fixedNow.Add(time.Hour) // marker is past grace
		if err := repo.ReconcileForTest(reconcileAt, reserveGrace); err != nil {
			t.Fatalf("ReconcileForTest: %v", err)
		}
		// The counter is UNTOUCHED (the live site's bytes stay counted) ...
		if got := siteSum(t, sr, owner, reconcileAt); got != X {
			t.Fatalf("UNDER-COUNT: reconcile decremented a LIVE site's release marker: counter = %d, want %d "+
				"(a release marker whose row is present must be dropped, never decremented)", got, X)
		}
		// ... and the row is still readable + listable (never removed).
		if _, err := sr.Get(s.Slug); err != nil {
			t.Fatalf("live site must survive the reconcile, Get(%s) = %v", slug, err)
		}
		// ... and the stale marker is dropped (no leak).
		if raw, err := repo.GetRawForTest(storage.IdentitySiteReleaseKeyForTest(owner, slug)); err != nil {
			t.Fatalf("read release marker post-reconcile: %v", err)
		} else if len(raw) != 0 {
			t.Fatalf("stale release marker for a live row not dropped: got %q", raw)
		}
		t.Logf("PROVEN: a past-grace release marker for a LIVE site was DROPPED without decrementing "+
			"(counter stayed %d) - never under-count", X)
	})

	// --- Subtest E: the real DeleteSite consumes its own release marker -------
	t.Run("E_real_delete_consumes_marker", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:test-real-delete"
			slug  = "realdel1"
			X     = 500
		)
		s := siteOf(slug, owner, X)
		insertSite(t, sr, s)
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after deploy: site counter = %d, want %d", got, X)
		}

		// The real DeleteSite: writes the release marker (step 2), tombstones
		// (step 3), then decrements + consumes (step 4) in one flow.
		if err := sr.Delete(s.Slug); err != nil {
			t.Fatalf("DeleteSite: %v", err)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != 0 {
			t.Fatalf("after DeleteSite: site counter = %d, want 0 (the hot path must decrement)", got)
		}
		if _, err := sr.Get(s.Slug); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("after DeleteSite, Get(%s) = %v, want ErrNotFound", slug, err)
		}
		// The hot path consumed its own marker: NOTHING should linger for the
		// reconciler to act on.
		if raw, err := repo.GetRawForTest(storage.IdentitySiteReleaseKeyForTest(owner, slug)); err != nil {
			t.Fatalf("read release marker post-delete: %v", err)
		} else if len(raw) != 0 {
			t.Fatalf("DeleteSite left a lingering release marker: got %q (step 4 must consume it)", raw)
		}

		// A reconcile has nothing to do and must not move the counter.
		if err := repo.ReconcileForTest(fixedNow.Add(time.Hour), reserveGrace); err != nil {
			t.Fatalf("ReconcileForTest: %v", err)
		}
		if got := siteSum(t, sr, owner, fixedNow.Add(time.Hour)); got != 0 {
			t.Fatalf("post-delete reconcile moved the counter to %d, want 0", got)
		}
		t.Logf("PROVEN: the real DeleteSite wrote AND consumed its release marker (counter %d -> 0, no lingering marker)", X)
	})
}
