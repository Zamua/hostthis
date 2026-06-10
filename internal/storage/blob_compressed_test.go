package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeRawStore is an in-memory inner BlobStore for testing the
// compression wrapper in isolation. Tracks how many bytes a Put
// actually persisted so tests can assert compression actually
// happened.
type fakeRawStore struct {
	mu    sync.Mutex
	store map[string][]byte
}

func newFakeRawStore() *fakeRawStore {
	return &fakeRawStore{store: make(map[string][]byte)}
}

func (f *fakeRawStore) Put(sha string, r io.Reader, _ int64) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[sha] = body
	return nil
}

func (f *fakeRawStore) Get(sha string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.store[sha]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), body...), nil
}

func (f *fakeRawStore) rawSize(sha string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.store[sha])
}

func (f *fakeRawStore) putRawForLegacyTest(sha string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[sha] = append([]byte(nil), body...)
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestCompressedBlobStore_RoundTrip(t *testing.T) {
	body := []byte("<!doctype html><h1>hello</h1>" + strings.Repeat(" world", 5000))
	sha := shaOf(body)
	inner := newFakeRawStore()
	c := NewCompressedBlobStore(inner)

	if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("roundtrip mismatch: want %d bytes, got %d", len(body), len(got))
	}
}

func TestCompressedBlobStore_StoresMagicHeader(t *testing.T) {
	body := []byte("compressible text - repeats well " + strings.Repeat("xy", 1000))
	sha := shaOf(body)
	inner := newFakeRawStore()
	c := NewCompressedBlobStore(inner)
	if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	raw, err := inner.Get(sha)
	if err != nil {
		t.Fatalf("inner Get: %v", err)
	}
	if !hasMagicV1(raw) {
		t.Fatalf("inner store missing magic prefix: first 4 bytes = % x", raw[:4])
	}
}

func TestCompressedBlobStore_ActuallyCompresses(t *testing.T) {
	// 32 KB of redundant text should compress to <5% of original under zstd.
	body := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 1000)
	sha := shaOf(body)
	inner := newFakeRawStore()
	c := NewCompressedBlobStore(inner)
	if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stored := inner.rawSize(sha)
	if stored >= len(body) {
		t.Fatalf("compression did not reduce size: input=%d stored=%d", len(body), stored)
	}
	// Sanity-check: redundant text should hit at least 5x compression.
	if ratio := float64(len(body)) / float64(stored); ratio < 5 {
		t.Fatalf("compression ratio too low: %.1fx (input=%d, stored=%d)", ratio, len(body), stored)
	}
}

func TestCompressedBlobStore_LegacyUncompressedReadable(t *testing.T) {
	// Simulate a blob written before the compression layer existed:
	// raw bytes, no magic header.
	legacy := []byte("<!doctype html><h1>legacy paste</h1>")
	sha := shaOf(legacy)
	inner := newFakeRawStore()
	inner.putRawForLegacyTest(sha, legacy)
	c := NewCompressedBlobStore(inner)
	got, err := c.Get(sha)
	if err != nil {
		t.Fatalf("Get legacy: %v", err)
	}
	if !bytes.Equal(got, legacy) {
		t.Fatalf("legacy roundtrip mismatch: want %q got %q", legacy, got)
	}
}

func TestCompressedBlobStore_GetPropagatesNotFound(t *testing.T) {
	inner := newFakeRawStore()
	c := NewCompressedBlobStore(inner)
	_, err := c.Get(shaOf([]byte("missing")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCompressedBlobStore_EmptyInput(t *testing.T) {
	sha := shaOf(nil)
	inner := newFakeRawStore()
	c := NewCompressedBlobStore(inner)
	if err := c.Put(sha, bytes.NewReader(nil), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 bytes, got %d", len(got))
	}
}

func TestCompressedBlobStore_LegacyShortBodyTreatedAsLegacy(t *testing.T) {
	// A 3-byte legacy blob can't possibly hold the magic - fallback path.
	short := []byte("hi\n")
	sha := shaOf(short)
	inner := newFakeRawStore()
	inner.putRawForLegacyTest(sha, short)
	c := NewCompressedBlobStore(inner)
	got, err := c.Get(sha)
	if err != nil {
		t.Fatalf("Get short legacy: %v", err)
	}
	if !bytes.Equal(got, short) {
		t.Fatalf("short legacy roundtrip mismatch: want %q got %q", short, got)
	}
}
