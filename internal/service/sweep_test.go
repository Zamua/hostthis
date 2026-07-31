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

// The sweep deletes expired pastes and GCs their blobs.
//
// Uploads go through NewCompressedBlobStore, as the composition root wires
// them: that wrapper owns the at-rest encoding, and handing the raw disk store
// to the blob unit would store encoded bytes it cannot decode on read. The
// sweep itself takes the raw store, which is also production's shape - it walks
// and removes by sha and never reads a body.
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

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
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
	// Blob writes finalize in the background; drain before the sweep walks them.
	upload.WaitFinalize()

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(repo, blobs, logger)

	pastes, gcBlobs, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if pastes != 0 || gcBlobs != 0 {
		t.Fatalf("nothing should sweep yet: pastes=%d blobs=%d", pastes, gcBlobs)
	}

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

	if _, err := repo.Get(r1.Paste.Slug); err == nil {
		t.Fatalf("paste 1 should be deleted")
	}
	if _, err := repo.Get(r2.Paste.Slug); err == nil {
		t.Fatalf("paste 2 should be deleted")
	}
}

// A paste stamped NeverExpires is never swept, however far in the future the
// sweep runs.
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

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
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
	// A century out: any finite TTL would be long gone.
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

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
	t.Cleanup(upload.WaitFinalize)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	upload.Now = func() time.Time { return now }

	r, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>still here</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	sweep := service.NewSweep(repo, blobs, log.New(io.Discard, "", 0))
	// Well within retention.
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

	// A referenced blob, via the upload path.
	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
	t.Cleanup(upload.WaitFinalize)
	upload.Now = func() time.Time { return time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC) }
	_, err := upload.Create(bytes.NewReader([]byte("<!doctype html><p>ref</p>")), "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// An orphan blob, written straight to the store.
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
	if _, err := blobs.Get(orphanSHA); err == nil {
		t.Fatalf("orphan should be gone")
	}
}

// A repo reporting zero referenced shas while blobs exist and nothing was just
// deleted must make the sweep REFUSE to GC rather than wipe the store.
func TestSweep_GuardsAgainstBuggyRepoZeroRefs(t *testing.T) {
	dir := t.TempDir()
	blobs, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))

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
	for _, sha := range []string{sha1, sha2} {
		if _, err := blobs.Get(sha); err != nil {
			t.Fatalf("blob %s should survive a buggy repo: %v", sha, err)
		}
	}
}

// For index-backed backends the expiry pass drains in ONE pass: an entry whose
// paste is already gone is removed (a second pass sees nothing), the
// deleted-count counts real paste deletions and not index no-ops, and the
// orphaned entries cleaned are reported.
func TestSweep_OrphanExpiryIndexEntries(t *testing.T) {
	repo := &indexedSweepRepo{
		pastes: map[string]bool{"live1234": true},
		index: map[string]string{
			"expiry/2026-01-01T00:00:00Z/live1234": "live1234",
			// The orphan: its paste record is already gone.
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
	// Only live1234 had a paste record; an orphan entry is an index cleanup.
	if pastes != 1 {
		t.Fatalf("deleted-count must reflect real paste deletions only: got %d, want 1", pastes)
	}
	// live1234's entry goes via the paste delete, gone1234's via orphan cleanup.
	if len(repo.index) != 0 {
		t.Fatalf("expiry index must drain in one pass; %d entr(ies) remain: %v", len(repo.index), repo.index)
	}
	if !bytes.Contains(logbuf.Bytes(), []byte("orphaned expiry-index")) {
		t.Fatalf("sweep should log the orphaned index-entry cleanup; got:\n%s", logbuf.String())
	}

	pastes, _, err = sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if pastes != 0 {
		t.Fatalf("second pass must see zero expired entries, got %d", pastes)
	}
}

// indexedSweepRepo mimics an index-backed metadata backend: ExpiredPastes scans
// a standalone expiry index (each entry's key is the opaque IndexRef), and
// DeleteExpired cascades the paste delete when the record still exists while
// removing the OBSERVED entry regardless. That is the repo-side contract in
// docs/SPEC.md "The storage contract" (Expiry).
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
		// The paste delete cascades to the DERIVED index entry ...
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

// A ref whose processing reports success but resurfaces on the next scan (data
// placed where routed deletes cannot reach it, or a diverged replica
// resurrecting a deleted record) is classified UNREACHABLE: attempted once then
// skipped, kept out of the deleted/cleaned counters, and reported as skipped.
// Without the guard the sweep re-"cleans" the same refs on every pass forever.
func TestSweep_UnreachableRefsSkippedAfterResurface(t *testing.T) {
	// Every scan returns this ref and DeleteExpired reports success, but
	// nothing drains.
	stickyKey := "expiry/2026-07-01T03:20:59.990221663Z/8ajitdpm"
	repo := &stickySweepRepo{ref: domain.ExpiredPaste{Slug: "8ajitdpm", IndexRef: stickyKey}}

	var logbuf bytes.Buffer
	sweep := service.NewSweep(repo, nil, log.New(&logbuf, "", 0))
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	// Pass 1: first sight, so the cleanup is attempted.
	if _, _, err := sweep.Once(now); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if repo.deleteCalls != 1 {
		t.Fatalf("pass 1 should attempt the ref once, got %d calls", repo.deleteCalls)
	}

	// Pass 2: the ref resurfaced, so the delete provably did not persist. No
	// second attempt, no cleaned-count, and an unreachable report.
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

	// Pass 3: the classification is sticky while the ref keeps appearing.
	if _, _, err := sweep.Once(now); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if repo.deleteCalls != 1 {
		t.Fatalf("unreachable ref must stay skipped: want 1 total call, got %d", repo.deleteCalls)
	}
}

// A ref that stops appearing leaves the guard's memory, so a later record with
// the same id is treated as new and processed again.
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

	// It drains externally: a pass with an empty scan prunes it.
	repo.gone = true
	_, _, _ = sweep.Once(now)

	// A fresh record with the same identity is processed anew.
	repo.gone = false
	_, _, _ = sweep.Once(now)
	if repo.deleteCalls != 2 {
		t.Fatalf("a drained-then-reborn ref must be attempted again: want 2 calls, got %d", repo.deleteCalls)
	}
}

// stickySweepRepo's expiry scan keeps returning the same ref no matter how
// often DeleteExpired reports success: the entry's placement is unreachable by
// the routed delete, so the mutation never lands where the scan reads. Setting
// gone empties the scan, modelling an external cleanup.
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

// buggyRepo's ReferencedBlobSHAs always reports no shas referenced, even when
// paste rows exist. Unused methods panic so an unexpected call surfaces.
type buggyRepo struct{}

func (buggyRepo) ExpiredPastes(_ time.Time) ([]domain.ExpiredPaste, error) { return nil, nil }
func (buggyRepo) DeleteExpired(_ domain.ExpiredPaste) (bool, error) {
	panic("not expected")
}
func (buggyRepo) ReferencedBlobSHAs() ([]string, error) { return nil, nil }

// In dry-run the sweep computes and logs what it would expire or GC while
// mutating nothing. The blob-GC count covers only blobs ALREADY orphaned: a
// blob freed by this tick's would-be expiry stays referenced, because dry-run
// does not delete the paste.
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

	upload := service.NewUpload(repo, service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(blobs)))
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

	// Delete B so its blob is already orphaned.
	if err := repo.Delete(rB.Paste.Slug); err != nil {
		t.Fatalf("delete B: %v", err)
	}

	var logbuf bytes.Buffer
	sweep := service.NewSweep(repo, blobs, log.New(&logbuf, "", 0))
	sweep.DryRun = true

	// Past the retention window: A would expire and B's blob would be GC'd.
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

	// Nothing was mutated: A still exists and both blobs survive on disk.
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
	if !bytes.Contains(logbuf.Bytes(), []byte("would expire paste")) {
		t.Fatalf("dry-run should log 'would expire paste'; got:\n%s", logbuf.String())
	}
	if !bytes.Contains(logbuf.Bytes(), []byte("would gc orphan blob")) {
		t.Fatalf("dry-run should log 'would gc orphan blob'; got:\n%s", logbuf.String())
	}
}
