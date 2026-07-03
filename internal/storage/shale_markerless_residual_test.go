//go:build slatedb

package storage_test

// Markerless-residual proof for the shale SITE quota counter.
//
// A site DELETE is NOT one transaction: DeleteSite removes the sites/<slug>
// row (and its derived entries) on the {slug} shard, then decrements the
// identity_site_bytes/<id> counter on the {id} shard as a SEPARATE CAS. If
// that second CAS is lost (a convergence blip) AFTER the row removal
// committed, the freed bytes stay in the counter with NO row behind them and
// NO reservation marker.
//
// The hypothesis under test: the periodic reconciler cannot heal that
// residual. reconcileSiteReservations only ever moves the SITE counter via
// orphanReleaseSiteMarker, which is driven by a reservation MARKER; with no
// marker there is nothing for it to release, so the over-count is permanent.
//
// Subtest A models exactly that half-committed delete and asserts the counter
// stays inflated across a reconcile (hypothesis PROVEN if it stays, FALSE if
// it drops). Subtest B is the control: an orphaned reservation MARKER (the
// marker-backed drift the residual lacks) IS released by the same reconcile,
// proving the reconciler works and isolating A's non-healing to the missing
// marker.
//
// Both are //go:build slatedb and need a live MinIO; they skip cleanly unless
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
	// residual case has no marker at all, and the control's marker is stamped
	// far in the past on purpose.
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

	// --- Subtest A: markerless residual is NOT healed (the hypothesis) -------
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

		// --- simulate a HALF-COMMITTED delete ---------------------------------
		// DeleteSite's {slug}-shard CAS removes the row + its derived entries; the
		// {id}-shard CAS then decrements the counter AND drops the enumeration
		// index. Model the case where the {slug} removal + index drop committed but
		// the counter decrement (the lost CAS) never landed: remove sites/<slug>,
		// the identity_sites/<id>/<slug> index entry, and the expiry_sites/<ts>/<slug>
		// entry - but LEAVE the counter untouched. There is NO reservation marker
		// (a delete never writes one), so this is a truly markerless residual.
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

		// --- reconcile past grace: the residual must survive ------------------
		reconcileAt := fixedNow.Add(time.Hour) // well past grace; there are no markers anyway
		if err := repo.ReconcileForTest(reconcileAt, reserveGrace); err != nil {
			t.Fatalf("ReconcileForTest: %v", err)
		}

		got := siteSum(t, sr, owner, reconcileAt)
		if got == 0 {
			t.Fatalf("HYPOTHESIS FALSE: the reconciler HEALED the markerless residual (counter dropped %d -> 0). "+
				"The site counter can be cured without a marker; the markerless-residual claim is wrong.", X)
		}
		if got != X {
			t.Fatalf("markerless residual moved unexpectedly: counter = %d, want %d (reconcile should leave it untouched)", got, X)
		}
		t.Logf("PROVEN: markerless residual UNHEALED - site counter still %d after a past-grace reconcile "+
			"(no marker exists for the reconciler to release, so the orphaned bytes stay permanently counted)", got)
	})

	// --- Subtest B: control - a leaked/orphaned MARKER *is* healed -----------
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
		// authoritative write). This is the marker-backed twin of subtest A's
		// residual: the bytes are over-counted, but here a marker records them.
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
			t.Fatalf("CONTROL FAILED: reconcile did NOT heal the orphaned marker: counter = %d, want 0 "+
				"(a past-grace marker for an absent site must be orphan-released)", got)
		}
		// ... and the marker itself is gone (no unbounded leak).
		if raw, err := repo.GetRawForTest(storage.IdentitySiteReserveKeyForTest(owner, slug)); err != nil {
			t.Fatalf("read marker post-reconcile: %v", err)
		} else if len(raw) != 0 {
			t.Fatalf("orphan marker not deleted post-reconcile: got %q", raw)
		}
		t.Logf("CONTROL PROVEN: the reconciler orphan-released a past-grace marker (counter %d -> 0), "+
			"so it DOES cure marker-backed drift; subtest A's non-healing is specifically the MISSING marker", X)
	})
}
