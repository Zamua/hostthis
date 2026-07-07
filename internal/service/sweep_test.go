package service_test

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestSweep_Once - drives the sweep against a real sqlite + blob store.
// Uploads two pastes, marks one expired by setting Now to the future,
// asserts the expired one is gone and the still-active one survives.
func TestSweep_Once(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	repo := storage.NewPasteRepo(db)

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	r1, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>a</p>")), "owner-a", "", "")
	if err != nil {
		t.Fatalf("upload 1: %v", err)
	}
	r2, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>b</p>")), "owner-b", "", "")
	if err != nil {
		t.Fatalf("upload 2: %v", err)
	}
	// The blob writes finalize in the background; drain them so both blobs
	// are on disk before the sweep walks them.
	upload.WaitFinalize()

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(repo, blobs, logger)

	// At the moment of upload, nothing expired.
	pastes, gcBlobs, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if pastes != 0 || gcBlobs != 0 {
		t.Fatalf("nothing should sweep yet: pastes=%d blobs=%d", pastes, gcBlobs)
	}

	// Past the retention window, both have expired.
	future := now.Add(domain.DefaultRetentionWindow + 24*time.Hour)
	pastes, gcBlobs, err = sweep.Once(future)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if pastes != 2 {
		t.Fatalf("expected 2 expired pastes, got %d", pastes)
	}
	if gcBlobs != 2 {
		t.Fatalf("expected 2 blobs GC'd, got %d", gcBlobs)
	}

	// Both records are gone.
	if _, err := repo.Get(r1.Paste.Slug); err == nil {
		t.Fatalf("paste 1 should be deleted")
	}
	if _, err := repo.Get(r2.Paste.Slug); err == nil {
		t.Fatalf("paste 2 should be deleted")
	}
}

// TestSweep_NeverExpiresSurvives is the safety property behind a no-expiry
// retention policy: a paste stamped with the NeverExpires sentinel is NEVER
// deleted by the sweep, even running far in the future.
func TestSweep_NeverExpiresSurvives(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "never.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	repo := storage.NewPasteRepo(db)

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
	upload.Retention = domain.Retention{Window: 0} // no expiry
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	r, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>forever</p>")), "owner", "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	upload.WaitFinalize()
	if !r.Paste.ExpiresAt.Equal(domain.NeverExpires) {
		t.Fatalf("ExpiresAt: got %v, want NeverExpires", r.Paste.ExpiresAt)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	// Run the sweep a century out: a finite-TTL paste would be long gone.
	pastes, gcBlobs, err := sweep.Once(now.AddDate(100, 0, 0))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if pastes != 0 || gcBlobs != 0 {
		t.Fatalf("no-expiry paste must survive: swept pastes=%d blobs=%d", pastes, gcBlobs)
	}
	if _, err := repo.Get(r.Paste.Slug); err != nil {
		t.Fatalf("no-expiry paste should still exist: %v", err)
	}
}

func TestSweep_KeepsActive(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	repo := storage.NewPasteRepo(db)

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	r, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>still here</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	// 1 hour later - well within retention
	pastes, _, _ := sweep.Once(now.Add(time.Hour))
	if pastes != 0 {
		t.Fatalf("active paste should not be swept, got %d", pastes)
	}
	if _, err := repo.Get(r.Paste.Slug); err != nil {
		t.Fatalf("paste should still exist: %v", err)
	}
}

func TestSweep_GCsOrphanBlobOnly(t *testing.T) {
	dir := t.TempDir()
	db, _ := storage.Open(filepath.Join(dir, "g.db"))
	t.Cleanup(func() { _ = db.Close() })
	blobs, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	repo := storage.NewPasteRepo(db)

	// Write a referenced blob via the upload path.
	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	upload.Now = func() time.Time { return time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC) }
	_, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>ref</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Write an orphan blob directly.
	orphanSHA := domain.HashContent([]byte("orphan"))
	if err := blobs.Put(orphanSHA, bytes.NewReader([]byte("orphan")), int64(len("orphan"))); err != nil {
		t.Fatal(err)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	_, gc, err := sweep.Once(time.Date(2026, 6, 5, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if gc != 1 {
		t.Fatalf("expected 1 orphan GC, got %d", gc)
	}
	// Orphan gone, referenced blob still there.
	if _, err := blobs.Get(orphanSHA); err == nil {
		t.Fatalf("orphan should be gone")
	}
}

// TestSweep_GuardsAgainstBuggyRepoZeroRefs pins the data-loss
// guard: if a buggy metadata-repo impl returns zero referenced shas
// while blobs exist AND no pastes were just deleted, the sweep MUST
// refuse to GC instead of wiping the bucket. We model the bug with
// a fake repo whose ReferencedBlobSHAs always returns nil.
func TestSweep_GuardsAgainstBuggyRepoZeroRefs(t *testing.T) {
	dir := t.TempDir()
	blobs, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))

	// Two real blobs in the store.
	sha1 := domain.HashContent([]byte("aaa"))
	sha2 := domain.HashContent([]byte("bbb"))
	for sha, body := range map[string][]byte{sha1: []byte("aaa"), sha2: []byte("bbb")} {
		if err := blobs.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}

	repo := &buggyRepo{} // returns 0 referenced, 0 expired
	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))

	_, gc, err := sweep.Once(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sweep should not error, just refuse: %v", err)
	}
	if gc != 0 {
		t.Fatalf("guard MUST refuse GC, got gc=%d", gc)
	}
	// Both blobs must still exist.
	for _, sha := range []string{sha1, sha2} {
		if _, err := blobs.Get(sha); err != nil {
			t.Fatalf("blob %s should survive a buggy repo: %v", sha, err)
		}
	}
}

// TestSweep_OrphanExpiryIndexEntries pins the expiry pass's contract for
// index-backed backends (slatedb/shale keep a standalone expiry index):
//
//   - an index entry whose paste is ALREADY GONE is removed by one pass, so
//     a second pass sees zero expired entries (the pass drains, it does not
//     loop on the same legacy entries forever);
//   - the deleted-count reflects pastes actually deleted, NOT index no-ops;
//   - the pass reports the orphaned index entries it cleaned.
//
// The fake mirrors the real index-backed repos: the expiry scan reads the
// standalone index, and the paste-delete cascade removes the index entry
// only when the paste record still exists.
func TestSweep_OrphanExpiryIndexEntries(t *testing.T) {
	repo := &indexedSweepRepo{
		pastes: map[string]bool{"live1234": true},
		index: map[string]string{
			"expiry/2026-01-01T00:00:00Z/live1234": "live1234",
			// The orphan: its paste is already gone (a legacy TTL-era
			// entry whose record was removed without index cleanup).
			"expiry/2025-01-01T00:00:00Z/gone1234": "gone1234",
		},
	}

	var logbuf bytes.Buffer
	sweep := service.NewSweep(repo, nil, log.New(&logbuf, "", 0))

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	pastes, _, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	// Only live1234's paste record actually existed: the deleted-count is 1,
	// not 2 (the orphan entry is an index cleanup, not a paste deletion).
	if pastes != 1 {
		t.Fatalf("deleted-count must reflect real paste deletions only: got %d, want 1", pastes)
	}
	// BOTH index entries are gone after one pass: live1234's via the paste
	// delete, gone1234's via the orphan cleanup.
	if len(repo.index) != 0 {
		t.Fatalf("expiry index must drain in one pass; %d entr(ies) remain: %v", len(repo.index), repo.index)
	}
	// The pass reports the orphaned entries it cleaned.
	if !bytes.Contains(logbuf.Bytes(), []byte("orphaned expiry-index")) {
		t.Fatalf("sweep should log the orphaned index-entry cleanup; got:\n%s", logbuf.String())
	}

	// A second pass finds nothing: zero entries, zero deletions.
	pastes, _, err = sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if pastes != 0 {
		t.Fatalf("second pass must see zero expired entries, got %d", pastes)
	}
}

// indexedSweepRepo mimics an index-backed metadata backend (slatedb/shale):
// ExpiredPastes scans a standalone expiry index (returning each entry's key
// as the opaque IndexRef), and DeleteExpired cascades the paste delete when
// the record still exists and removes the OBSERVED index entry regardless -
// exactly the repo-side contract in docs/SPEC.md "The storage contract"
// (Expiry).
type indexedSweepRepo struct {
	pastes map[string]bool   // slug -> record exists
	index  map[string]string // index key -> slug
}

func (r *indexedSweepRepo) ExpiredPastes(_ time.Time) ([]domain.ExpiredPaste, error) {
	var out []domain.ExpiredPaste
	for k, slug := range r.index {
		out = append(out, domain.ExpiredPaste{Slug: domain.Slug(slug), IndexRef: k})
	}
	return out, nil
}

func (r *indexedSweepRepo) DeleteExpired(ref domain.ExpiredPaste) (bool, error) {
	deleted := r.pastes[ref.Slug.String()]
	if deleted {
		delete(r.pastes, ref.Slug.String())
		// The paste-delete cascade removes the DERIVED index entry ...
		for k, s := range r.index {
			if s == ref.Slug.String() {
				delete(r.index, k)
			}
		}
	}
	// ... and the OBSERVED entry is removed regardless, so an orphan drains.
	delete(r.index, ref.IndexRef)
	return deleted, nil
}

func (r *indexedSweepRepo) ReferencedBlobSHAs() ([]string, error) { return []string{"sha-live"}, nil }

// TestSweep_UnreachableRefsSkippedAfterResurface pins the sweep's
// convergence guard: a ref whose processing REPORTS success but does not
// persist (the scan surfaces the same ref again on the next pass - e.g.
// legacy data physically placed where routed deletes cannot reach it, or a
// diverged replica resurrecting a deleted record) is classified UNREACHABLE:
// the sweep stops re-processing it every pass (one attempt, then skip),
// keeps it out of the deleted/cleaned counters, and reports the skipped
// count instead. Without the guard the sweep spins forever re-"cleaning"
// the same refs (observed as a constant cleaned-count every cycle).
//
// The fixture key is byte-for-byte a real stuck staging entry's shape.
func TestSweep_UnreachableRefsSkippedAfterResurface(t *testing.T) {
	// The sticky ref: every scan returns it; DeleteExpired reports the
	// orphan-cleanup "success" but nothing actually drains.
	stickyKey := "expiry/2026-07-01T03:20:59.990221663Z/8ajitdpm"
	repo := &stickySweepRepo{ref: domain.ExpiredPaste{Slug: "8ajitdpm", IndexRef: stickyKey}}

	var logbuf bytes.Buffer
	sweep := service.NewSweep(repo, nil, log.New(&logbuf, "", 0))
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	// Pass 1: first sight - the sweep attempts the cleanup (1 call).
	if _, _, err := sweep.Once(now); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if repo.deleteCalls != 1 {
		t.Fatalf("pass 1 should attempt the ref once, got %d calls", repo.deleteCalls)
	}

	// Pass 2: the ref RESURFACED after a reported-successful processing -
	// the delete provably did not persist. The sweep must NOT attempt it
	// again (no second call), must not count it as cleaned, and must
	// report it as unreachable.
	logbuf.Reset()
	if _, _, err := sweep.Once(now); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if repo.deleteCalls != 1 {
		t.Fatalf("resurfaced ref must not be re-processed: want 1 total call, got %d", repo.deleteCalls)
	}
	if !bytes.Contains(logbuf.Bytes(), []byte("unreachable")) {
		t.Fatalf("pass 2 should report the unreachable ref; got:\n%s", logbuf.String())
	}
	if bytes.Contains(logbuf.Bytes(), []byte("orphaned expiry-index entries")) {
		t.Fatalf("pass 2 must not claim it cleaned anything; got:\n%s", logbuf.String())
	}

	// Pass 3: still skipped (the classification is sticky while the ref
	// keeps appearing).
	if _, _, err := sweep.Once(now); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if repo.deleteCalls != 1 {
		t.Fatalf("unreachable ref must stay skipped: want 1 total call, got %d", repo.deleteCalls)
	}
}

// TestSweep_UnreachableRefForgottenOnceDrained pins the guard's pruning: a
// ref that STOPS appearing (drained externally, e.g. an operator purge)
// leaves the guard's memory, so a later reappearance of the same id (a
// fresh, legitimate record) is treated as new and processed again.
func TestSweep_UnreachableRefForgottenOnceDrained(t *testing.T) {
	stickyKey := "expiry/2026-07-01T03:20:59.990221663Z/8ajitdpm"
	repo := &stickySweepRepo{ref: domain.ExpiredPaste{Slug: "8ajitdpm", IndexRef: stickyKey}}
	sweep := service.NewSweep(repo, nil, log.New(io.Discard, "", 0))
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	// Pass 1 attempts; pass 2 classifies unreachable + skips.
	_, _, _ = sweep.Once(now)
	_, _, _ = sweep.Once(now)
	if repo.deleteCalls != 1 {
		t.Fatalf("setup: want 1 call after 2 passes, got %d", repo.deleteCalls)
	}

	// The ref drains externally: a pass with an empty scan prunes it.
	repo.gone = true
	_, _, _ = sweep.Once(now)

	// It reappears (a fresh record with the same identity): processed anew.
	repo.gone = false
	_, _, _ = sweep.Once(now)
	if repo.deleteCalls != 2 {
		t.Fatalf("a drained-then-reborn ref must be attempted again: want 2 calls, got %d", repo.deleteCalls)
	}
}

// stickySweepRepo is a SweepRepo whose expiry scan keeps returning the same
// ref no matter how often DeleteExpired "succeeds" - the observed staging
// pathology (the entry's physical placement is not reachable by the routed
// delete, so the mutation never lands where the scan reads). Setting gone
// empties the scan (external cleanup).
type stickySweepRepo struct {
	ref         domain.ExpiredPaste
	gone        bool
	deleteCalls int
}

func (r *stickySweepRepo) ExpiredPastes(_ time.Time) ([]domain.ExpiredPaste, error) {
	if r.gone {
		return nil, nil
	}
	return []domain.ExpiredPaste{r.ref}, nil
}

func (r *stickySweepRepo) DeleteExpired(_ domain.ExpiredPaste) (bool, error) {
	r.deleteCalls++
	return false, nil // paste already gone; entry cleanup reported ok (but never persists)
}

func (r *stickySweepRepo) ReferencedBlobSHAs() ([]string, error) { return []string{"sha-x"}, nil }

// buggyRepo simulates the failure mode this test guards against:
// ReferencedBlobSHAs always returns nil (i.e. "no shas are
// referenced") even when paste rows exist. Only methods the sweep
// actually invokes are stubbed; everything else panics so the test
// surfaces unexpected calls.
type buggyRepo struct{}

func (buggyRepo) ExpiredPastes(_ time.Time) ([]domain.ExpiredPaste, error) { return nil, nil }
func (buggyRepo) DeleteExpired(_ domain.ExpiredPaste) (bool, error) {
	panic("not expected")
}
func (buggyRepo) ReferencedBlobSHAs() ([]string, error) { return nil, nil }

// TestSweep_DryRun - in dry-run the sweep COMPUTES + LOGS what it would
// expire/GC but mutates nothing: the expired record and every blob survive.
// This is the "disabled = log what it would clean" mode. (The blob-GC count
// reflects blobs already orphaned, e.g. by a prior delete; a blob freed only
// by THIS tick's would-be expiry stays referenced because dry-run doesn't
// actually delete the paste - it's counted once the expiry is live.)
func TestSweep_DryRun(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	repo := storage.NewPasteRepo(db)

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	rA, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>a</p>")), "owner-a", "", "")
	if err != nil {
		t.Fatalf("upload A: %v", err)
	}
	rB, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>b</p>")), "owner-b", "", "")
	if err != nil {
		t.Fatalf("upload B: %v", err)
	}
	upload.WaitFinalize()

	// Delete B so its blob is already orphaned (a blob the GC would reclaim).
	if err := repo.Delete(rB.Paste.Slug); err != nil {
		t.Fatalf("delete B: %v", err)
	}

	var logbuf bytes.Buffer
	sweep := service.NewSweep(repo, blobs, log.New(&logbuf, "", 0))
	sweep.DryRun = true

	// Past the retention window A has expired. Dry-run: A would expire, B's
	// blob would be GC'd - but nothing is actually touched.
	future := now.Add(domain.DefaultRetentionWindow + 24*time.Hour)
	pastes, gcBlobs, err := sweep.Once(future)
	if err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	if pastes != 1 {
		t.Fatalf("dry-run should report 1 would-expire record, got %d", pastes)
	}
	if gcBlobs != 1 {
		t.Fatalf("dry-run should report 1 would-gc orphan blob, got %d", gcBlobs)
	}

	// NOTHING was mutated: A still exists, and BOTH blobs survive on disk.
	if _, err := repo.Get(rA.Paste.Slug); err != nil {
		t.Fatalf("dry-run must NOT delete the expired paste; Get err %v", err)
	}
	blobCount := 0
	if err := blobs.WalkBlobs(func(string) error { blobCount++; return nil }); err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	if blobCount != 2 {
		t.Fatalf("dry-run must NOT remove any blob; want 2 on disk, got %d", blobCount)
	}
	// And it logged what it WOULD do.
	if !bytes.Contains(logbuf.Bytes(), []byte("would expire paste")) {
		t.Fatalf("dry-run should log 'would expire paste'; got:\n%s", logbuf.String())
	}
	if !bytes.Contains(logbuf.Bytes(), []byte("would gc orphan blob")) {
		t.Fatalf("dry-run should log 'would gc orphan blob'; got:\n%s", logbuf.String())
	}
}
