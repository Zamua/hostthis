package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDurable is an in-memory durableBlobStore that can be told to fail Put a
// fixed number of times, or always, and counts the attempts.
type fakeDurable struct {
	mu         sync.Mutex
	objs       map[string][]byte
	putCalls   int32
	failFirst  int32 // fail this many Put attempts before succeeding
	failAlways bool  // every Put fails, so entries stay permanently pinned
	putErr     error // error to return while failing
}

func newFakeDurable() *fakeDurable {
	return &fakeDurable{objs: map[string][]byte{}, putErr: errors.New("durable down")}
}

func (f *fakeDurable) Put(sha string, r io.Reader, size int64) error {
	atomic.AddInt32(&f.putCalls, 1)
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAlways {
		return f.putErr
	}
	if f.failFirst > 0 {
		f.failFirst--
		return f.putErr
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	f.objs[sha] = cp
	return nil
}

func (f *fakeDurable) Get(sha string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objs[sha]
	if !ok {
		return nil, ErrNotFound
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

func (f *fakeDurable) GetReader(sha string) (io.ReadCloser, int64, error) {
	b, err := f.Get(sha)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f *fakeDurable) WalkBlobs(fn func(sha string) error) error {
	f.mu.Lock()
	keys := make([]string, 0, len(f.objs))
	for k := range f.objs {
		keys = append(keys, k)
	}
	f.mu.Unlock()
	for _, k := range keys {
		if err := fn(k); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeDurable) Remove(sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objs, sha)
	return nil
}

func (f *fakeDurable) has(sha string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objs[sha]
	return ok
}

func wbShaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newTestWriteBack(t *testing.T, durable durableBlobStore, cfg WriteBackConfig) *WriteBackBlobStore {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	// Fast retries so tests don't sleep seconds.
	cfg.retryBackoff = 2 * time.Millisecond
	cfg.maxRetryBackoff = 20 * time.Millisecond
	wb, err := NewWriteBackBlobStore(durable, cfg)
	if err != nil {
		t.Fatalf("NewWriteBackBlobStore: %v", err)
	}
	t.Cleanup(wb.Close)
	return wb
}

// Put returns with the blob in the local cache; the background uploader then
// makes it durable.
func TestWriteBack_PutLocalThenAsyncUpload(t *testing.T) {
	durable := newFakeDurable()
	wb := newTestWriteBack(t, durable, WriteBackConfig{})

	body := []byte("hello write-back cache")
	sha := wbShaOf(body)

	if err := wb.PutPrecompressed(sha, body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(wb.blobPath(sha)); err != nil {
		t.Fatalf("expected blob in local cache: %v", err)
	}
	if !wb.drainForTest(2 * time.Second) {
		t.Fatal("uploader did not drain in time")
	}
	if !durable.has(sha) {
		t.Fatal("blob never reached the durable backend")
	}
	if !wb.isUploaded(sha) {
		t.Fatal("uploaded marker not written")
	}
}

// Get serves from the cache while the blob is local, and falls through to the
// durable backend once it is gone.
func TestWriteBack_GetCacheThenDurable(t *testing.T) {
	durable := newFakeDurable()
	wb := newTestWriteBack(t, durable, WriteBackConfig{})

	body := []byte("read path content")
	sha := wbShaOf(body)
	if err := wb.PutPrecompressed(sha, body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := wb.Get(sha)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("cache Get: got %q err %v", got, err)
	}
	// Wait for durability, then drop the local copy to force a durable read.
	wb.drainForTest(2 * time.Second)
	if err := os.Remove(wb.blobPath(sha)); err != nil {
		t.Fatalf("remove local: %v", err)
	}
	got, err = wb.Get(sha)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("durable-fallback Get: got %q err %v", got, err)
	}
	rc, _, err := wb.GetReader(sha)
	if err != nil {
		t.Fatalf("durable-fallback GetReader: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	rb, _ := io.ReadAll(rc)
	if !bytes.Equal(rb, body) {
		t.Fatalf("GetReader bytes mismatch: %q", rb)
	}
}

// A durable backend that fails its first Puts is retried until it succeeds,
// without losing the blob.
func TestWriteBack_UploaderRetries(t *testing.T) {
	durable := newFakeDurable()
	durable.failFirst = 3
	wb := newTestWriteBack(t, durable, WriteBackConfig{})

	body := []byte("retry me")
	sha := wbShaOf(body)
	if err := wb.PutPrecompressed(sha, body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !wb.drainForTest(2 * time.Second) {
		t.Fatal("uploader did not drain after retries")
	}
	if !durable.has(sha) {
		t.Fatal("blob not durable after retries")
	}
	if got := atomic.LoadInt32(&durable.putCalls); got < 4 {
		t.Fatalf("expected at least 4 Put attempts (3 fail + 1 ok), got %d", got)
	}
}

// A blob left in the cache dir with no uploaded marker (a crash mid-upload) is
// re-enqueued and uploaded by the next process's startup rescan.
func TestWriteBack_StartupRescanReenqueues(t *testing.T) {
	dir := t.TempDir()
	body := []byte("survived a crash")
	sha := wbShaOf(body)

	// Seed the cache dir as a prior process would leave it after dying between
	// the local write and the upload: blob present, NO ".up" marker.
	shardDir := filepath.Join(dir, sha[:2])
	if err := os.MkdirAll(shardDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shardDir, sha), body, 0o640); err != nil {
		t.Fatal(err)
	}

	durable := newFakeDurable()
	wb := newTestWriteBack(t, durable, WriteBackConfig{Dir: dir})

	if !wb.drainForTest(2 * time.Second) {
		t.Fatal("rescan uploader did not drain")
	}
	if !durable.has(sha) {
		t.Fatal("rescan did not upload the pending blob")
	}
}

// A not-yet-uploaded entry is never evicted even over the byte cap: the local
// copy is the ONLY copy.
func TestWriteBack_PinnedNotEvicted(t *testing.T) {
	durable := newFakeDurable()
	durable.failAlways = true
	wb := newTestWriteBack(t, durable, WriteBackConfig{MaxBytes: 64})

	var shas []string
	for i := range 5 {
		body := bytes.Repeat([]byte{byte('a' + i)}, 40) // ~200 bytes total, over the 64 cap
		sha := wbShaOf(body)
		shas = append(shas, sha)
		if err := wb.PutPrecompressed(sha, body); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	// Nothing was uploaded, so no eviction pass may drop anything.
	wb.evictIfNeeded()
	wb.evictIfNeeded()
	for i, sha := range shas {
		if _, err := os.Stat(wb.blobPath(sha)); err != nil {
			t.Fatalf("pinned (un-uploaded) blob %d was evicted: %v", i, err)
		}
	}
}

// Once a blob is durable it becomes evictable and is dropped when over the cap,
// while staying readable from the durable backend.
func TestWriteBack_EvictionAfterUpload(t *testing.T) {
	durable := newFakeDurable()
	wb := newTestWriteBack(t, durable, WriteBackConfig{MaxBytes: 64})

	var shas []string
	for i := range 5 {
		body := bytes.Repeat([]byte{byte('a' + i)}, 40)
		sha := wbShaOf(body)
		shas = append(shas, sha)
		if err := wb.PutPrecompressed(sha, body); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	if !wb.drainForTest(2 * time.Second) {
		t.Fatal("uploads did not drain")
	}
	wb.evictIfNeeded()
	_, total, err := wb.scanEntries()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if total > 64 {
		t.Fatalf("cache still over cap after eviction: %d bytes", total)
	}
	// Eviction drops only the local copy.
	for i, sha := range shas {
		if !durable.has(sha) {
			t.Fatalf("blob %d lost from durable after eviction", i)
		}
	}
}

// When the durable backend already holds the object, Put neither writes locally
// nor enqueues an upload.
func TestWriteBack_DurableDedupSkip(t *testing.T) {
	durable := newFakeDurable()
	body := []byte("already durable")
	sha := wbShaOf(body)
	_ = durable.Put(sha, bytes.NewReader(body), int64(len(body)))
	before := atomic.LoadInt32(&durable.putCalls)

	wb := newTestWriteBack(t, durable, WriteBackConfig{})
	if err := wb.PutPrecompressed(sha, body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(wb.blobPath(sha)); err == nil {
		t.Fatal("expected no local cache write on durable dedup hit")
	}
	wb.drainForTest(200 * time.Millisecond)
	if got := atomic.LoadInt32(&durable.putCalls); got != before {
		t.Fatalf("expected no extra durable Put on dedup hit, calls went %d -> %d", before, got)
	}
	// The read still works, falling through to durable.
	got, err := wb.Get(sha)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("Get on dedup blob: %q %v", got, err)
	}
}
