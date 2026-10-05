package storage_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestCelldBlobContract runs the object store contract against a live celld
// Worker. Skipped unless CELLD_TEST_ENDPOINT is set.
func TestCelldBlobContract(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld blob store tests")
	}
	runBlobContract(t, func(t *testing.T) rawBlobStore {
		bs, err := storage.NewCelldBlobStore(base, nil)
		if err != nil {
			t.Fatalf("new celld blob store: %v", err)
		}
		return bs
	})
}

// patternReader yields n deterministic bytes and records the largest buffer a
// Read was asked to fill, which tells bounded streaming from a slurp.
type patternReader struct {
	left   int64
	maxLen int
}

func (p *patternReader) Read(b []byte) (int, error) {
	p.maxLen = max(p.maxLen, len(b))
	if p.left == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(b)), p.left))
	for i := range b[:n] {
		b[i] = byte(i)
	}
	p.left -= int64(n)
	return n, nil
}

func heapAlloc() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A large Put and Get stream through the adapter: the body is sent with its
// length, read in bounded chunks, and neither direction grows the heap with it.
func TestCelldBlob_StreamsLargeBodies(t *testing.T) {
	const size = 64 << 20
	key := domain.UploadObjectKey(domain.NewUploadID(), 0)
	var gotLen int64
	var gotChunked bool
	var received int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			gotLen, gotChunked = r.ContentLength, len(r.TransferEncoding) > 0
			received, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			w.Header().Set("X-Blob-Size", strconv.Itoa(size))
			_, _ = io.Copy(w, &patternReader{left: size})
		}
	}))
	defer srv.Close()
	bs, err := storage.NewCelldBlobStore(srv.URL, nil)
	if err != nil {
		t.Fatalf("new celld blob store: %v", err)
	}

	before := heapAlloc()
	src := &patternReader{left: size}
	if err := bs.Put(key, src, size); err != nil {
		t.Fatalf("put: %v", err)
	}
	if gotLen != size || gotChunked || received != size {
		t.Fatalf("server saw Content-Length %d, chunked %v, %d bytes; want %d, false, %d", gotLen, gotChunked, received, size, size)
	}
	if src.maxLen > 256<<10 {
		t.Fatalf("largest Put read buffer = %d bytes; the body is being slurped", src.maxLen)
	}

	rc, n, err := bs.GetReader(key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	read, err := io.Copy(io.Discard, rc)
	_ = rc.Close()
	if err != nil || read != size || n != size {
		t.Fatalf("get = %d bytes (length %d, err %v), want %d", read, n, err, size)
	}
	if delta := int64(heapAlloc()) - int64(before); delta > 16<<20 {
		t.Fatalf("heap grew %d bytes across a %d-byte put and get; a body is being buffered", delta, size)
	}
}

// Only a 404 is ErrNotFound; any other refusal is an opaque error, so the read
// path never mistakes a broken Worker for a missing object.
func TestCelldBlob_StatusMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	bs, err := storage.NewCelldBlobStore(srv.URL, nil)
	if err != nil {
		t.Fatalf("new celld blob store: %v", err)
	}
	key := domain.UploadObjectKey(domain.NewUploadID(), 0)
	if _, _, err := bs.GetReader(key); err == nil || errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetReader on a 500 = %v, want a non-not-found error", err)
	}
	if err := bs.Put(key, &patternReader{left: 1}, 1); err == nil {
		t.Fatal("Put on a 500 = nil, want an error")
	}
	if err := bs.DeletePrefix(domain.UploadPrefix(domain.NewUploadID())); err == nil {
		t.Fatal("DeletePrefix on a 500 = nil, want an error")
	}
}
