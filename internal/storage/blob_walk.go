package storage

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WalkBlobs visits every blob file under root and calls fn(sha) with
// the sha encoded in the filename. Stops on the first non-nil error.
func (b *BlobStore) WalkBlobs(fn func(sha string) error) error {
	return filepath.WalkDir(b.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		// Skip our atomic-write tmp files (".tmp-*").
		if strings.HasPrefix(base, ".tmp-") {
			return nil
		}
		return fn(base)
	})
}

// Remove deletes a blob by sha. No-op when the file is already gone
// (treat as success — the GC may have removed it under us).
func (b *BlobStore) Remove(sha string) error {
	if len(sha) < 2 {
		return nil
	}
	path := filepath.Join(b.root, sha[:2], sha)
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
