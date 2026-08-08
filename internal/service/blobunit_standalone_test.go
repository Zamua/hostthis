package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// newStandaloneUnit builds a StandaloneBlobUnit over a real compressed disk
// blob store, the same stack the composition root wires, so the seam is
// exercised end to end with no mocks and no network.
func newStandaloneUnit(t *testing.T) (*StandaloneBlobUnit, *storage.CompressedBlobStore) {
	t.Helper()
	disk, err := storage.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	store := storage.NewCompressedBlobStore(disk)
	return NewStandaloneBlobUnit(store), store
}

// stage encodes raw bytes the way the streaming upload pipeline does and stages
// them. Stage takes precompressed bytes because the upload service always hands
// it an already-encoded body, so streamUpload produces them here too.
func stage(t *testing.T, u *StandaloneBlobUnit, slug string, raw []byte) (string, BlobHandle) {
	t.Helper()
	staged, err := streamUpload(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("streamUpload: %v", err)
	}
	h, err := u.Stage(context.Background(), slug, staged.SHA, stagedBytes(t, staged))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	return staged.SHA, h
}

// Staged bytes read back DECOMPRESSED and byte-identical through both Read
// (streaming) and ReadAll (buffered).
func TestStandalone_StageRead_RoundTrip(t *testing.T) {
	u, _ := newStandaloneUnit(t)
	raw := []byte("<!doctype html><h1>round trip</h1>")
	sha, _ := stage(t, u, "slug0001", raw)

	rc, _, err := u.Read(context.Background(), "slug0001", sha)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if err != nil {
		t.Fatalf("read all from stream: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("Read bytes: got %q, want %q", got, raw)
	}

	gotAll, err := u.ReadAll(context.Background(), "slug0001", sha)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(gotAll, raw) {
		t.Fatalf("ReadAll bytes: got %q, want %q", gotAll, raw)
	}
}

// The streaming-stage path used by the site deploy sink round-trips: bytes go
// in uncompressed through StageStream and read back identical.
func TestStandalone_StageStream_RoundTrip(t *testing.T) {
	u, _ := newStandaloneUnit(t)
	raw := []byte("body{margin:0}\n/* a stylesheet a site file would carry */")
	sha := domain.HashContent(raw)

	if _, err := u.StageStream(context.Background(), "site0001", sha, bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatalf("StageStream: %v", err)
	}
	got, err := u.ReadAll(context.Background(), "site0001", sha)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("StageStream round-trip: got %q, want %q", got, raw)
	}
}

// Commit runs the metadata closure exactly once and returns its error verbatim,
// so a caller's retry/translate switch sees it unchanged.
func TestStandalone_Commit_RunsMetaWrite(t *testing.T) {
	u, _ := newStandaloneUnit(t)
	_, h := stage(t, u, "slug0002", []byte("<p>commit</p>"))

	calls := 0
	if err := u.Commit(context.Background(), []BlobHandle{h}, func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Commit (ok): %v", err)
	}
	if calls != 1 {
		t.Fatalf("metaWrite call count: got %d, want 1", calls)
	}

	sentinel := errors.New("metadata write failed")
	if err := u.Commit(context.Background(), []BlobHandle{h}, func(context.Context) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Commit (err): got %v, want %v", err, sentinel)
	}
}

// UnbindOnDelete does NOT remove bytes on the standalone path: the global
// content-addressed sweep owns reclamation, not a per-record unbind.
func TestStandalone_UnbindOnDelete_IsNoop(t *testing.T) {
	u, _ := newStandaloneUnit(t)
	raw := []byte("<p>still here after unbind</p>")
	sha, _ := stage(t, u, "slug0003", raw)

	if err := u.UnbindOnDelete(context.Background(), "slug0003", []string{sha}); err != nil {
		t.Fatalf("UnbindOnDelete: %v", err)
	}
	got, err := u.ReadAll(context.Background(), "slug0003", sha)
	if err != nil {
		t.Fatalf("ReadAll after unbind (should still exist): %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("bytes after unbind: got %q, want %q", got, raw)
	}
}

// Reading a never-staged sha surfaces storage.ErrNotFound through the seam,
// which upstream turns into a 404 or a loading retry.
func TestStandalone_Read_NotFound(t *testing.T) {
	u, _ := newStandaloneUnit(t)
	if _, _, err := u.Read(context.Background(), "slug0004", "deadbeef"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Read missing: got %v, want storage.ErrNotFound", err)
	}
	if _, err := u.ReadAll(context.Background(), "slug0004", "deadbeef"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("ReadAll missing: got %v, want storage.ErrNotFound", err)
	}
}
