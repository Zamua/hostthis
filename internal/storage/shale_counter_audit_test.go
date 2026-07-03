//go:build slatedb

package storage_test

// Offline counter-audit behavior for the shale byte counters. AuditCounters
// recomputes each identity's paste (identity_bytes) and site
// (identity_site_bytes) counter from AUTHORITATIVE truth and, with apply=true,
// overwrites each drifted counter with the recomputed value. It is the
// belt-and-suspenders that corrects drift the crash-durable release marker
// cannot (a residual banked before the marker existed, or an operator's manual
// surgery), and it is SOUND ONLY OFFLINE (writes quiesced), so these tests set
// up a drifted state, run the audit, and assert convergence with truth.
//
// The subtests:
//
//   - A (site markerless residual corrected): the exact state
//     TestShaleMarkerlessResidual subtest A proves the reconciler CANNOT heal -
//     a half-committed delete that removed the sites/<slug> row + indexes but
//     left the site counter over-counted with NO marker. The audit's dry-run
//     REPORTS the drift but writes nothing; --apply sets the counter to the
//     authoritative sum (0, the row is gone); a re-run is a no-op.
//   - B (no-op on a correct counter): a live site whose counter already equals
//     its DedupedSize produces NO finding and is left untouched, even under
//     --apply. The audit only writes counters that actually drifted.
//   - C (paste over-count with a live row -> set DOWN to the live sum): a
//     raw-inflated identity_bytes counter over a still-live paste is corrected
//     DOWN to the sum of the paste's live version sizes (not to 0) - the audit
//     recomputes from authoritative truth, in either direction.
//
// All are //go:build slatedb and need a live MinIO; they skip cleanly unless
// MINIO_TEST_ENDPOINT is set.
//
//	go test -tags slatedb -run TestShaleCounterAudit ./internal/storage

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleCounterAudit(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale counter-audit test (start dev MinIO first)")
	}

	// Generous grace: none of these tests exercises an in-flight (young) marker,
	// so grace never gates a decision here; it is passed through to AuditCounters
	// unchanged.
	const reserveGrace = 10 * time.Minute

	// siteSum reads the owner's SITE counter (identity_site_bytes/<id>).
	siteSum := func(t *testing.T, sr *storage.ShaleSiteRepo, owner string, now time.Time) int64 {
		t.Helper()
		n, err := sr.SumActiveBytesByOwner(owner, now)
		if err != nil {
			t.Fatalf("SumActiveBytesByOwner(site, %s): %v", owner, err)
		}
		return n
	}
	// pasteSum reads the owner's PASTE counter (identity_bytes/<id>).
	pasteSum := func(t *testing.T, repo *storage.ShaleRepo, owner string, now time.Time) int64 {
		t.Helper()
		n, err := repo.SumActiveBytesByOwner(owner, now)
		if err != nil {
			t.Fatalf("SumActiveBytesByOwner(paste, %s): %v", owner, err)
		}
		return int64(n)
	}
	// mustDeleteRaw removes a key straight through the cluster (bypassing the
	// repo's ordered delete), the surgical primitive that manufactures a
	// half-committed state.
	mustDeleteRaw := func(t *testing.T, repo *storage.ShaleRepo, key []byte) {
		t.Helper()
		if err := repo.DeleteRawForTest(key); err != nil {
			t.Fatalf("DeleteRawForTest(%q): %v", key, err)
		}
	}
	// findFinding returns the audit finding for (owner, kind), if any.
	findFinding := func(res storage.CounterAuditResult, owner, kind string) (storage.CounterAuditFinding, bool) {
		for _, f := range res.Findings {
			if f.Identity == owner && f.Kind == kind {
				return f, true
			}
		}
		return storage.CounterAuditFinding{}, false
	}

	// --- Subtest A: site markerless residual corrected by the audit ----------
	t.Run("A_site_markerless_residual_corrected", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:audit-site-resid"
			slug  = "audresid1"
			X     = 500 // the site's DedupedSize == the bytes charged to the counter
		)
		s := siteOf(slug, owner, X)
		insertSite(t, sr, s)
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after deploy: site counter = %d, want %d", got, X)
		}

		// LEGACY markerless half-commit: remove the row + both indexes AND any
		// reserve marker (so the state is truly markerless regardless of confirm
		// timing), leaving the counter untouched at X. This is exactly the
		// residual the reconciler cannot heal.
		mustDeleteRaw(t, repo, storage.SiteKeyForTest(s.Slug))
		mustDeleteRaw(t, repo, storage.IdentitySiteKeyForTest(owner, slug))
		mustDeleteRaw(t, repo, storage.ExpirySiteKeyForTest(s.ExpiresAt, s.Slug))
		mustDeleteRaw(t, repo, storage.IdentitySiteReserveKeyForTest(owner, slug))

		if _, err := sr.Get(s.Slug); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("after half-commit, Get(%s) = %v, want ErrNotFound", slug, err)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after half-commit: site counter = %d, want %d (the residual over-count)", got, X)
		}

		// DRY-RUN: reports the drift, writes NOTHING.
		res, err := repo.AuditCounters(fixedNow, reserveGrace, false)
		if err != nil {
			t.Fatalf("AuditCounters(dry-run): %v", err)
		}
		f, ok := findFinding(res, owner, "site")
		if !ok {
			t.Fatalf("dry-run: expected a site finding for %s, got findings=%+v", owner, res.Findings)
		}
		if f.Stored != X || f.Computed != 0 {
			t.Fatalf("dry-run finding = {stored:%d computed:%d}, want {stored:%d computed:0}", f.Stored, f.Computed, X)
		}
		if res.Applied != 0 {
			t.Fatalf("dry-run: Applied = %d, want 0 (dry-run must not write)", res.Applied)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("dry-run mutated the counter: site counter = %d, want %d (unchanged)", got, X)
		}

		// APPLY: corrects the counter to the authoritative sum (0; row is gone).
		res, err = repo.AuditCounters(fixedNow, reserveGrace, true)
		if err != nil {
			t.Fatalf("AuditCounters(apply): %v", err)
		}
		if f, ok := findFinding(res, owner, "site"); !ok || f.Computed != 0 {
			t.Fatalf("apply: expected site finding computed=0 for %s, got ok=%t finding=%+v", owner, ok, f)
		}
		if res.Applied != 1 {
			t.Fatalf("apply: Applied = %d, want 1", res.Applied)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != 0 {
			t.Fatalf("AUDIT DID NOT CORRECT: site counter = %d after --apply, want 0 (authoritative truth)", got)
		}

		// Idempotent: a second apply finds no drift and writes nothing.
		res, err = repo.AuditCounters(fixedNow, reserveGrace, true)
		if err != nil {
			t.Fatalf("AuditCounters(re-apply): %v", err)
		}
		if len(res.Findings) != 0 || res.Applied != 0 {
			t.Fatalf("re-apply: findings=%+v applied=%d, want none (idempotent)", res.Findings, res.Applied)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != 0 {
			t.Fatalf("re-apply moved a correct counter: %d, want 0", got)
		}
		t.Logf("PROVEN: the offline audit corrected a markerless site residual (counter %d -> 0) the reconciler could not heal", X)
	})

	// --- Subtest B: no-op on an already-correct counter ----------------------
	t.Run("B_noop_on_correct_counter", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)
		sr := storage.NewShaleSiteRepo(repo)

		const (
			owner = "key:audit-correct"
			slug  = "audok1234"
			X     = 700
		)
		insertSite(t, sr, siteOf(slug, owner, X))
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("after deploy: site counter = %d, want %d", got, X)
		}

		res, err := repo.AuditCounters(fixedNow, reserveGrace, true)
		if err != nil {
			t.Fatalf("AuditCounters(apply): %v", err)
		}
		if _, ok := findFinding(res, owner, "site"); ok {
			t.Fatalf("no-op expected: got a site finding for a correct counter: %+v", res.Findings)
		}
		if _, ok := findFinding(res, owner, "paste"); ok {
			t.Fatalf("no-op expected: got a paste finding for an owner with no paste: %+v", res.Findings)
		}
		if res.Applied != 0 {
			t.Fatalf("no-op expected: Applied = %d, want 0", res.Applied)
		}
		if got := siteSum(t, sr, owner, fixedNow); got != X {
			t.Fatalf("audit mutated a CORRECT counter: site counter = %d, want %d", got, X)
		}
		t.Logf("PROVEN: the audit is a no-op on a counter that already matches authoritative truth (stayed %d)", X)
	})

	// --- Subtest C: paste over-count with a live row -> set DOWN to live sum --
	t.Run("C_paste_overcount_set_to_live_sum", func(t *testing.T) {
		repo := newShaleRepoOnUniqueDB(t, endpoint)

		const (
			owner = "key:audit-paste-over"
			slug  = "audpaste1"
			N     = 320 // the paste's live version size == the true counter value
		)
		insert(t, repo, pasteOf(slug, owner, N))
		if got := pasteSum(t, repo, owner, fixedNow); got != N {
			t.Fatalf("after insert: paste counter = %d, want %d", got, N)
		}

		// Bank a raw over-count (a residual the crash window / manual surgery
		// could have left) while the paste row + its live version stay present.
		const inflated = 9999
		mustPutRaw(t, repo, storage.IdentityBytesKeyForTest(owner), []byte("9999"))
		if got := pasteSum(t, repo, owner, fixedNow); got != inflated {
			t.Fatalf("after raw over-count: paste counter = %d, want %d", got, inflated)
		}

		res, err := repo.AuditCounters(fixedNow, reserveGrace, true)
		if err != nil {
			t.Fatalf("AuditCounters(apply): %v", err)
		}
		f, ok := findFinding(res, owner, "paste")
		if !ok {
			t.Fatalf("expected a paste finding for %s, got findings=%+v", owner, res.Findings)
		}
		if f.Stored != inflated || f.Computed != N {
			t.Fatalf("paste finding = {stored:%d computed:%d}, want {stored:%d computed:%d}", f.Stored, f.Computed, inflated, N)
		}
		// The correction moves the counter DOWN to the live authoritative sum,
		// not to 0 (the paste's bytes are still live and correctly counted).
		if got := pasteSum(t, repo, owner, fixedNow); got != N {
			t.Fatalf("AUDIT DID NOT CORRECT: paste counter = %d after --apply, want %d (live version sum)", got, N)
		}
		t.Logf("PROVEN: the audit recomputed a paste over-count DOWN to the live authoritative sum (%d -> %d)", inflated, N)
	})
}
