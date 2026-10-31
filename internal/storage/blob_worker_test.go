package storage_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/Zamua/hostthis/internal/workertest"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestWorkerBlobContract runs the object store contract against a live
// Worker: the local runtime, or the node WORKER_TEST_ENDPOINT names.
func TestWorkerBlobContract(t *testing.T) {
	base := workertest.Target(t)
	runBlobContract(t, func(t *testing.T) storage.InnerBlobStore {
		bs, err := storage.NewWorkerBlobStore(base, nil)
		if err != nil {
			t.Fatalf("new worker blob store: %v", err)
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
func TestWorkerBlob_StreamsLargeBodies(t *testing.T) {
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
	bs, err := storage.NewWorkerBlobStore(srv.URL, nil)
	if err != nil {
		t.Fatalf("new worker blob store: %v", err)
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
func TestWorkerBlob_StatusMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	bs, err := storage.NewWorkerBlobStore(srv.URL, nil)
	if err != nil {
		t.Fatalf("new worker blob store: %v", err)
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

// A read whose connection drops before any response is retried once.
func TestWorkerBlob_RetriesADroppedRead(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		w.Header().Set("X-Blob-Size", "2")
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	s, err := storage.NewWorkerBlobStore(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.GetReader("uploads/u/0")
	if err != nil {
		t.Fatalf("get after one dropped connection: %v", err)
	}
	_ = rc.Close()
	calls.Store(0)
	rc, err = s.GetRange("uploads/u/0", 0, 2)
	if err != nil {
		t.Fatalf("range after one dropped connection: %v", err)
	}
	_ = rc.Close()
}

// A seekable put whose connection drops mid-body is resent whole, once; a
// streamed body is not, since it cannot be replayed.
func TestWorkerBlob_RetriesADroppedSeekablePut(t *testing.T) {
	var calls atomic.Int32
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.CopyN(io.Discard, r.Body, 100)
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s, err := storage.NewWorkerBlobStore(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("hostthis"), 64<<10)
	spill, err := os.CreateTemp(t.TempDir(), "spill-*")
	if err != nil {
		t.Fatal(err)
	}
	defer spill.Close() //nolint:errcheck
	if _, err := spill.Write(body); err != nil {
		t.Fatal(err)
	}
	if _, err := spill.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("uploads/u/0", spill, int64(len(body))); err != nil {
		t.Fatalf("put after one dropped connection: %v", err)
	}
	if calls.Load() != 2 || !bytes.Equal(got, body) {
		t.Fatalf("calls %d, resent %d of %d bytes intact: %v", calls.Load(), len(got), len(body), bytes.Equal(got, body))
	}

	calls.Store(0)
	if err := s.Put("uploads/u/1", &patternReader{left: int64(len(body))}, int64(len(body))); err == nil {
		t.Fatal("streamed put after a dropped connection = nil, want the error")
	}
	if calls.Load() != 1 {
		t.Fatalf("streamed put sent %d times, want 1", calls.Load())
	}
}
