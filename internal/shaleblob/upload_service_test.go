//go:build slatedb

// SERVICE-LEVEL tests for Upload.Create across both blob backends. They drive
// the real service.Upload, not just the seam, so they catch what a seam-only
// test cannot: on the transactional shale path Create must STAGE->COMMIT (bind
// the blob, commit the row READY) rather than run the pending/finalizer model
// and discard the staged handle, which commits the row with BlobID="",
// permanently unreadable and later swept.
//
// The shale case wires Upload with a blob-capable ShaleRepo (real
// slate-over-MinIO metadata, in-memory blobmem byte plane) + the shaleblob.Unit
// seam; the standalone case wires sqlite + the detached disk store.

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
	"github.com/Zamua/hostthis/internal/storagetest"
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

// TestUpload_Create_Shale_BindsAndReadsBack pins that Create on the
// transactional shale path binds the blob and commits the row READY, so the
// paste reads back.
func TestUpload_Create_Shale_BindsAndReadsBack(t *testing.T) {
	repo, unit, _ := newBlobRepo(t) // skips when MinIO is absent
	u := service.NewUpload(repo, unit)

	// The wired seam must be transactional, so Create takes the READY-direct
	// path; otherwise the standalone path runs and the assertions below never
	// exercise the bind.
	if !unit.IsTransactional() {
		t.Fatalf("shale unit IsTransactional() = false, want true")
	}

	raw := []byte("<!doctype html><h1>shale create</h1>")
	res, err := u.Create(bytes.NewReader(raw), "owner-shale", "demo", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Pending-collapse: the returned paste is READY directly, no finalizer.
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
	// seam. A discarded staged handle leaves ResolveBlobID looking at a
	// BlobID="" row and Read 404s.
	out, rerr := readAll(t, unit, string(res.Paste.Slug), got.ContentSHA)
	if rerr != nil {
		t.Fatalf("read back created paste: %v (the blob was not bound on Create)", rerr)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("read back = %q, want %q", out, raw)
	}
}

// TestUpload_Create_Shale_CountsTowardQuota pins that the quota reserve runs
// inside InsertWithQuotaCheck on the READY-direct path: a created shale paste
// charges its compressed bytes to the owner immediately.
func TestUpload_Create_Shale_CountsTowardQuota(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	u := service.NewUpload(repo, unit)

	owner := "owner-quota-shale"
	res, err := u.Create(bytes.NewReader([]byte("<!doctype html><p>counts</p>")), owner, "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// WaitPendingConfirms drains the deferred confirm so the derived byte index
	// reflects the insert deterministically.
	repo.WaitPendingConfirms()
	n, err := repo.SumActiveBytesByOwner(owner, u.Now().UTC())
	if err != nil {
		t.Fatalf("SumActiveBytesByOwner: %v", err)
	}
	if n != res.Paste.Size {
		t.Fatalf("active bytes = %d, want %d (created paste counts toward quota)", n, res.Paste.Size)
	}
}

// TestUpload_Create_Standalone_PendingPathIntact pins the detached-store path:
// a standalone-backed Create returns a PENDING paste and the background
// finalizer flips it to READY and writes the blob.
func TestUpload_Create_Standalone_PendingPathIntact(t *testing.T) {
	dir := t.TempDir()
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	standalone := service.NewStandaloneBlobUnit(blobs)
	if standalone.IsTransactional() {
		t.Fatalf("standalone unit IsTransactional() = true, want false")
	}
	repo := storagetest.NewRepo(t)
	u := service.NewUpload(repo, standalone)

	raw := []byte("<!doctype html><h1>standalone create</h1>")
	res, err := u.Create(bytes.NewReader(raw), "owner-standalone", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Create returns PENDING before the blob lands.
	if res.Paste.Status != domain.PasteStatusPending {
		t.Fatalf("standalone Create status = %q, want pending", res.Paste.Status)
	}

	// The finalizer writes the blob and flips the row to READY.
	u.WaitFinalize()
	got, gerr := repo.Get(res.Paste.Slug)
	if gerr != nil {
		t.Fatalf("Get after finalize: %v", gerr)
	}
	if got.Status != domain.PasteStatusReady {
		t.Fatalf("status after finalize = %q, want ready", got.Status)
	}
	// The bytes read back through the standalone seam.
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
