package storage

import (
	"errors"
	"fmt"
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

// Put writes content addressed by sha. If a file already exists at
// the destination, we trust the existing bytes (content-addressed —
// sha collision is the only way to "overwrite" and that's not a real
// concern at sha256).
func (b *BlobStore) Put(sha string, content []byte) error {
	if len(sha) < 2 {
		return fmt.Errorf("blob: sha too short")
	}
	dir := filepath.Join(b.root, sha[:2])
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("blob mkdir %q: %w", dir, err)
	}
	dst := filepath.Join(dir, sha)
	if _, err := os.Stat(dst); err == nil {
		// Already there — no-op. Content-addressed means same bytes.
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
	defer os.Remove(tmpName) // safe — if rename succeeded, file no longer exists
	if _, err := tmp.Write(content); err != nil {
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
