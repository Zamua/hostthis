//go:build slatedb

// SERVICE-LEVEL tests for Upload.Create across both blob backends. These drive
// the real service.Upload (not just the seam) so they catch the P0 the
// seam-only tests missed: Upload.Create must STAGE->COMMIT on the shale path
// (binding the blob, committing the row READY) instead of running the
// pending/finalizer model and discarding the staged handle (which left the row
// BlobID="" - permanently unreadable, then swept).
//
// The shale case wires Upload with a blob-capable ShaleRepo (real slate-over-
// MinIO metadata, in-memory blobmem byte plane) + the shaleblob.Unit seam, then
// asserts the created paste READS BACK (the blob is bound + served) and the row
// is READY directly (no pending). The standalone case wires Upload with sqlite
// + the detached disk store and asserts the pending/finalizer path is intact.

package shaleblob_test

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// readAllRC drains a ReadCloser to bytes (the caller still owns Close).
func readAllRC(t *testing.T, rc io.Reader) []byte {
	t.Helper()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	return out
}

// TestUpload_Create_Shale_BindsAndReadsBack is the regression for the P0:
// Create on the transactional shale path must bind the blob + commit the row
// READY so the paste reads back. Before the fix Create ran the pending model
// and discarded the staged handle, so the row committed with BlobID="" and the
// content was unreachable (and later swept as an orphan).
func TestUpload_Create_Shale_BindsAndReadsBack(t *testing.T) {
	repo, unit, _ := newBlobRepo(t) // skips when MinIO is absent
	u := service.NewUpload(repo, unit)

	// Sanity: the wired seam IS transactional, so Create takes the READY-direct
	// path (the whole point of the fix). A false here means the standalone path
	// would run and the assertions below would not exercise the bind.
	if !unit.IsTransactional() {
		t.Fatalf("shale unit IsTransactional() = false, want true")
	}

	raw := []byte("<!doctype html><h1>shale create</h1>")
	res, err := u.Create(bytes.NewReader(raw), "owner-shale", "demo", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Pending-collapse: the returned paste is READY directly - no pending row,
	// no finalizer.
	if res.Paste.Status != domain.PasteStatusReady {
		t.Fatalf("Create result status = %q, want ready (READY-direct)", res.Paste.Status)
	}
	if res.Paste.Kind != domain.KindHTML {
		t.Fatalf("kind = %q, want html", res.Paste.Kind)
	}

	// The persisted row is READY too, immediately (no background flip).
	got, gerr := repo.Get(res.Paste.Slug)
	if gerr != nil {
		t.Fatalf("Get after Create: %v", gerr)
	}
	if got.Status != domain.PasteStatusReady {
		t.Fatalf("persisted status = %q, want ready", got.Status)
	}

	// THE LOAD-BEARING ASSERTION: the blob is bound and reads back through the
	// seam. If Create had discarded the staged handle (the P0), ResolveBlobID
	// would find a BlobID="" row and Read would 404.
	out, rerr := readAll(t, unit, string(res.Paste.Slug), got.ContentSHA)
	if rerr != nil {
		t.Fatalf("read back created paste: %v (the blob was not bound on Create)", rerr)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("read back = %q, want %q", out, raw)
	}
}

// TestUpload_Create_Shale_QuotaStillEnforced confirms the quota reserve still
// runs inside InsertWithQuotaCheck on the READY-direct path: a pending paste
// counts against quota the same way a READY one does, so the synchronous quota
// gate is unchanged by the pending-collapse. (A created shale paste counts its
// compressed bytes immediately.)
func TestUpload_Create_Shale_CountsTowardQuota(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	u := service.NewUpload(repo, unit)

	owner := "owner-quota-shale"
	res, err := u.Create(bytes.NewReader([]byte("<!doctype html><p>counts</p>")), owner, "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The created paste's bytes are charged to the owner right away (the reserve
	// step committed). WaitPendingConfirms drains the deferred confirm so the
	// derived byte index reflects the insert deterministically.
	repo.WaitPendingConfirms()
	n, err := repo.SumActiveBytesByOwner(owner, u.Now().UTC())
	if err != nil {
		t.Fatalf("SumActiveBytesByOwner: %v", err)
	}
	if n != res.Paste.Size {
		t.Fatalf("active bytes = %d, want %d (created paste counts toward quota)", n, res.Paste.Size)
	}
}

// TestUpload_Create_Standalone_PendingPathIntact confirms the fix did NOT
// disturb the detached-store path: a standalone-backed Create still returns a
// PENDING paste and the background finalizer flips it to READY + writes the
// blob. This is the half the shale branch must leave alone.
func TestUpload_Create_Standalone_PendingPathIntact(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	standalone := service.NewStandaloneBlobUnit(blobs)
	if standalone.IsTransactional() {
		t.Fatalf("standalone unit IsTransactional() = true, want false")
	}
	repo := storage.NewPasteRepo(db)
	u := service.NewUpload(repo, standalone)

	raw := []byte("<!doctype html><h1>standalone create</h1>")
	res, err := u.Create(bytes.NewReader(raw), "owner-standalone", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Pending path intact: Create returns PENDING before the blob lands.
	if res.Paste.Status != domain.PasteStatusPending {
		t.Fatalf("standalone Create status = %q, want pending", res.Paste.Status)
	}

	// The finalizer writes the blob + flips the row to READY.
	u.WaitFinalize()
	got, gerr := repo.Get(res.Paste.Slug)
	if gerr != nil {
		t.Fatalf("Get after finalize: %v", gerr)
	}
	if got.Status != domain.PasteStatusReady {
		t.Fatalf("status after finalize = %q, want ready", got.Status)
	}
	// And the bytes read back through the standalone seam.
	rc, _, rerr := standalone.Read(context.Background(), string(res.Paste.Slug), got.ContentSHA)
	if rerr != nil {
		t.Fatalf("standalone read after finalize: %v", rerr)
	}
	defer rc.Close() //nolint:errcheck
	out := readAllRC(t, rc)
	if !bytes.Equal(out, raw) {
		t.Fatalf("standalone read = %q, want %q", out, raw)
	}
}
