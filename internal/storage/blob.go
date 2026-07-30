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

// BlobStore is a content-addressed on-disk store. Bytes live at
// <root>/<sha256[:2]>/<sha256> so directory fanout stays sane; records pointing
// at the same bytes share one file.
type BlobStore struct {
	root string
}

func NewBlobStore(root string) (*BlobStore, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("blob store mkdir %q: %w", root, err)
	}
	return &BlobStore{root: root}, nil
}

// Put streams r into the content-addressed location for sha. An existing file
// at the destination is trusted as-is: only a sha256 collision could reach it
// with different bytes. size is accepted for parity with S3-shaped backends
// that need Content-Length up front; the disk store does not need it.
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
		// Drain r so a caller streaming a request body does not block.
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	// tmp + fsync + rename: a crash leaves the complete file or nothing, never
	// a partial blob visible under the final name.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("blob tmp create: %w", err)
	}
	tmpName := tmp.Name()
	// Fails with ENOENT once the rename has succeeded, which is the normal path.
	defer os.Remove(tmpName) //nolint:errcheck
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

// PutPrecompressed writes already-encoded bytes (magic prefix + zstd) straight
// to storage. Present so the disk store satisfies the same service-layer
// interface as CompressedBlobStore.
func (b *BlobStore) PutPrecompressed(sha string, body []byte) error {
	return b.Put(sha, bytes.NewReader(body), int64(len(body)))
}

// GetReader returns a streaming reader over the bytes for sha, or ErrNotFound.
// The caller MUST Close it. The int64 is the on-disk length of the raw stored
// bytes: for blobs wrapped by CompressedBlobStore that is the compressed
// length, not the decoded one.
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

// EncodeBody encodes uncompressed bytes into the at-rest format and reports the
// payload size excluding the framing prefix, so callers ask the store that owns
// the format instead of doing the framing arithmetic themselves.
func (s *BlobStore) EncodeBody(r io.Reader) ([]byte, int, error) {
	body, err := EncodeCompressedBody(r)
	if err != nil {
		return nil, 0, err
	}
	return body, len(body) - CompressedBodyPrefixLen, nil
}
