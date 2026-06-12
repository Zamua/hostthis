package storage

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestBlobStore_GetReaderMatchesGet pins that the disk store's
// streaming GetReader yields byte-identical bytes to the buffered Get,
// and reports the on-disk size.
func TestBlobStore_GetReaderMatchesGet(t *testing.T) {
	b, err := NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	body := bytes.Repeat([]byte("disk-store streaming read\n"), 4096)
	sha := shaOf(body)
	if err := b.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want, err := b.Get(sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rc, size, err := b.GetReader(sha)
	if err != nil {
		t.Fatalf("GetReader: %v", err)
	}
	defer rc.Close()
	if size != int64(len(body)) {
		t.Fatalf("size: want %d got %d", len(body), size)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GetReader != Get (%d vs %d bytes)", len(got), len(want))
	}
}

func TestBlobStore_GetReaderNotFound(t *testing.T) {
	b, err := NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	_, _, err = b.GetReader(shaOf([]byte("missing")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestCompressedOverDisk_GetReaderMatchesGet exercises the real serve
// stack: CompressedBlobStore wrapping the on-disk BlobStore, streaming
// decode end-to-end, asserted byte-identical to the buffered Get.
func TestCompressedOverDisk_GetReaderMatchesGet(t *testing.T) {
	disk, err := NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	c := NewCompressedBlobStore(disk)
	body := bytes.Repeat([]byte("<p>hello world</p>\n"), 100000) // ~1.8 MiB
	sha := shaOf(body)
	if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	want, err := c.Get(sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rc, _, err := c.GetReader(sha)
	if err != nil {
		t.Fatalf("GetReader: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GetReader != Get (%d vs %d bytes)", len(got), len(want))
	}
}
