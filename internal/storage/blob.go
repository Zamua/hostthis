package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// BlobStore is a content-addressed on-disk store. Bytes are written
// to `<root>/<sha256[:2]>/<sha256>` so directory fanout stays sane.
// Multiple pastes pointing to the same bytes share one file.
type BlobStore struct {
	root string
}

func NewBlobStore(root string) (*BlobStore, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("blob store mkdir %q: %w", root, err)
	}
	return &BlobStore{root: root}, nil
}

// Put streams r into the content-addressed location for sha. If a file
// already exists at the destination, we trust the existing bytes
// (content-addressed - sha collision is the only way to "overwrite"
// and that's not a real concern at sha256). size is the expected byte
// length of r; passed for symmetry with S3-shaped backends that need
// to send Content-Length up front. The disk impl doesn't strictly
// require it but accepts it for interface uniformity.
func (b *BlobStore) Put(sha string, r io.Reader, size int64) error {
	if len(sha) < 2 {
		return fmt.Errorf("blob: sha too short")
	}
	dir := filepath.Join(b.root, sha[:2])
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("blob mkdir %q: %w", dir, err)
	}
	dst := filepath.Join(dir, sha)
	if _, err := os.Stat(dst); err == nil {
		// Already there - no-op. Content-addressed means same bytes.
		// Drain r so a caller streaming a request body doesn't block.
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	// Write to a tmp file, fsync, rename. This makes Put atomic under
	// crash: either the final filename exists with the full bytes, or
	// it doesn't exist at all. Partial writes never become visible.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("blob tmp create: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // safe - if rename succeeded, file no longer exists
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("blob write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("blob sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blob close: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("blob rename: %w", err)
	}
	return nil
}

// PutPrecompressed writes already-encoded bytes (magic prefix + zstd)
// straight to storage. Equivalent to Put for the disk store since the
// disk store doesn't transform bytes; provided so the disk store
// satisfies the same service-layer interface as CompressedBlobStore.
func (b *BlobStore) PutPrecompressed(sha string, body []byte) error {
	return b.Put(sha, bytes.NewReader(body), int64(len(body)))
}

// GetReader returns a streaming reader over the bytes for sha, or
// ErrNotFound. The caller MUST Close the returned reader. The int64 is
// the on-disk byte length (the raw stored bytes; for blobs wrapped by
// CompressedBlobStore this is the compressed length, not the decoded
// length). Streaming avoids buffering the whole blob into memory the
// way Get does - the serve path io.Copy's straight to the client.
func (b *BlobStore) GetReader(sha string) (io.ReadCloser, int64, error) {
	if len(sha) < 2 {
		return nil, 0, fmt.Errorf("blob: sha too short")
	}
	dst := filepath.Join(b.root, sha[:2], sha)
	f, err := os.Open(dst) //nolint:gosec // path derived from validated sha
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("blob open %q: %w", sha, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("blob stat %q: %w", sha, err)
	}
	return f, fi.Size(), nil
}

// Get returns the bytes for sha, or ErrNotFound.
func (b *BlobStore) Get(sha string) ([]byte, error) {
	if len(sha) < 2 {
		return nil, fmt.Errorf("blob: sha too short")
	}
	dst := filepath.Join(b.root, sha[:2], sha)
	body, err := os.ReadFile(dst)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("blob read %q: %w", sha, err)
	}
	return body, nil
}
