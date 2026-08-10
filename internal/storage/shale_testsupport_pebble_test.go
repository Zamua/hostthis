// The default build's engine factory: shale over an in-memory pebble unit.
//
// Nothing to skip on. Every shale test that uses this runs in a plain
// `go test ./...`, which is the point of the storage-engine seam.

//go:build !slatedb

package storage_test

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Zamua/shale/pkg/blob/blobmem"

	"github.com/Zamua/hostthis/internal/storage"
)

var pebbleSupportSeq atomic.Int64

func newShaleRepoForTest(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	// Each call gets its own in-memory FS, so the name only has to be distinct
	// enough to read in a failure message.
	seq := pebbleSupportSeq.Add(1)
	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            fmt.Sprintf("pebble-node-%d", seq),
		DbName:            fmt.Sprintf("pebble-support-%d", seq),
		ReplicationFactor: 1,
	})
	if err != nil {
		t.Fatalf("NewShaleRepo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// newBlobShaleRepoForTest is the same repo with the transactional blob plane
// wired to an in-memory byte store, so a test can assert on blob POINTERS
// (bref keys) rather than only on metadata. Without a blob plane every unbind
// is a no-op and a leaked blob is invisible.
func newBlobShaleRepoForTest(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	seq := pebbleSupportSeq.Add(1)
	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            fmt.Sprintf("pebble-blob-node-%d", seq),
		DbName:            fmt.Sprintf("pebble-blob-support-%d", seq),
		ReplicationFactor: 1,
		BlobStore:         blobmem.New(),
	})
	if err != nil {
		t.Fatalf("NewShaleRepo (blob): %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}
