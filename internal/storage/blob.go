package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// BlobStore is the on-disk object store. An object lives at <root>/<key>; a
// legacy content-addressed object at <root>/<sha[:2]>/<sha>.
//
// It does NOT satisfy service.BlobStore: the at-rest encoding is
// CompressedBlobStore's. Giving the raw store the encoder methods would let a
// mis-wiring compile and then serve undecoded bytes.
type BlobStore struct {
	root string
}

func NewBlobStore(root string) (*BlobStore, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("blob store mkdir %q: %w", root, err)
	}
	return &BlobStore{root: root}, nil
}

func (b *BlobStore) path(key string) string {
	return filepath.Join(b.root, filepath.FromSlash(key))
}

// Put streams r to key. size is for parity with S3-shaped backends.
func (b *BlobStore) Put(key string, r io.Reader, _ int64) error {
	if err := checkKey(key); err != nil {
		return err
	}
	dst := b.path(key)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("blob mkdir %q: %w", dir, err)
	}
	// tmp + fsync + rename: a crash leaves the complete file or nothing, never
	// a partial object visible under the final name.
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

// GetReader streams the object at key, or ErrNotFound. The caller MUST Close
// it. The int64 is the stored length: for objects written through
// CompressedBlobStore that is the compressed length, not the decoded one.
func (b *BlobStore) GetReader(key string) (io.ReadCloser, int64, error) {
	if err := checkKey(key); err != nil {
		return nil, 0, err
	}
	return b.open(b.path(key), key)
}

// GetLegacyReader streams a legacy content-addressed object, or ErrNotFound.
func (b *BlobStore) GetLegacyReader(sha string) (io.ReadCloser, int64, error) {
	if err := checkSHA(sha); err != nil {
		return nil, 0, err
	}
	return b.open(filepath.Join(b.root, sha[:2], sha), sha)
}

func (b *BlobStore) open(path, label string) (io.ReadCloser, int64, error) {
	f, err := os.Open(path) //nolint:gosec // path built from a checked key or sha
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("blob open %q: %w", label, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("blob stat %q: %w", label, err)
	}
	return f, fi.Size(), nil
}

// DeletePrefix removes every object under prefix. A prefix is a directory
// here, so this is one tree removal; an absent one is already deleted.
func (b *BlobStore) DeletePrefix(prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	if err := os.RemoveAll(b.path(strings.TrimSuffix(prefix, "/"))); err != nil {
		return fmt.Errorf("blob delete prefix %q: %w", prefix, err)
	}
	return nil
}
