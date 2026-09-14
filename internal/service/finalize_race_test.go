package service

import (
	"slices"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// A paste deleted while its create's background write is in flight gets that
// write's object removed once the write lands.
func TestUpload_FinalizeAfterDeleteRemovesTheObject(t *testing.T) {
	blobs := newFakeBlobs()
	blobs.holdPut = make(chan struct{})
	u, repo, done := newStackWithBlobs(t, blobs)
	res, err := u.Create(strings.NewReader("<!doctype html><p>gone</p>"), "key:owner", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Delete(res.Paste.Slug, res.Paste.Identity, res.Paste.CreatedAt); err != nil {
		t.Fatalf("delete: %v", err)
	}
	close(blobs.holdPut)
	waitFinalize(t, done)

	if key := res.Paste.RootEntry().Key; blobs.has(key) {
		t.Fatalf("object %s survived its deleted paste", key)
	}
	if prefix := domain.UploadPrefix(res.Paste.UploadID); !slices.Contains(blobs.deletedPrefixes(), prefix) {
		t.Fatalf("deleted prefixes = %v, want %s", blobs.deletedPrefixes(), prefix)
	}
}
