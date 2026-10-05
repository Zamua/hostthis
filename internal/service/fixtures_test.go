package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	crand "crypto/rand"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/celldtest"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// fixedNow is the clock every real-stack fixture injects.
var fixedNow = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

// realBlobs is the production blob stack (compressed over celld) on the test's
// local runtime namespace.
func realBlobs(t *testing.T) *storage.CompressedBlobStore {
	t.Helper()
	blobs, _ := realBlobsAt(t)
	return blobs
}

// realBlobsAt is realBlobs plus a ledger of the keys written through it, for
// tests that inspect what the store holds.
func realBlobsAt(t *testing.T) (*storage.CompressedBlobStore, *keyLedger) {
	t.Helper()
	raw, err := storage.NewCelldBlobStore(celldtest.Endpoint(t), nil)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	ledger := &keyLedger{InnerBlobStore: raw}
	return storage.NewCompressedBlobStore(ledger), ledger
}

// newRepo is the test's metadata repo. Every call in one test addresses the
// same cells, so pastes and sites share one slug space as in production.
func newRepo(t *testing.T) *celld.PasteRepo {
	t.Helper()
	return celld.NewPasteRepo(celldtest.Endpoint(t), nil)
}

// newStack wires the upload and manage services over real metadata and real
// blobs: the same stack production runs, no mocks.
func newStack(t *testing.T) (*Upload, *Manage, *celld.PasteRepo) {
	t.Helper()
	blobs := realBlobs(t)
	repo := newRepo(t)
	upload := NewUpload(repo, NewStandaloneBlobUnit(blobs))
	t.Cleanup(upload.WaitFinalize)
	manage := NewManage(repo, NewStandaloneBlobUnit(blobs))
	upload.Now = func() time.Time { return fixedNow }
	manage.Now = func() time.Time { return fixedNow }
	return upload, manage, repo
}

// withSmallQuota shrinks the per-identity quota for one test and restores it.
// The enforcement path is identical at any cap value, and a small cap keeps
// the bodies that breach it small. A test asserting on the PRODUCTION number
// must read domain.UserQuotaBytes rather than hardcode it.
func withSmallQuota(t *testing.T, n int) {
	t.Helper()
	orig := domain.UserQuotaBytes
	domain.UserQuotaBytes = n
	t.Cleanup(func() { domain.UserQuotaBytes = orig })
}

// htmlBody returns an HTML-detected payload of about n bytes of HIGH-ENTROPY
// ASCII, so zstd barely shrinks it and compressed size approximates n. Quota is
// charged on compressed bytes, so an N-byte input must cost about N. The LCG
// keeps it deterministic.
func htmlBody(n int) []byte {
	const head = "<!doctype html><body>"
	if n <= len(head) {
		return []byte(head[:n])
	}
	out := make([]byte, n)
	copy(out, head)
	const printableSpan = 0x7e - 0x21 + 1 // '!' through '~'
	seed := uint32(0x12345678)
	for i := len(head); i < n; i++ {
		seed = seed*1103515245 + 12345
		out[i] = byte(0x21 + int(seed>>16)%printableSpan)
	}
	return out
}

// incompressible is n random bytes, so a "fill the budget" fixture's
// compressed charge is ~n rather than squashing to nothing.
func incompressible(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return string(b)
}

func gzipTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	entries := make([][2]string, 0, len(files))
	for name, body := range files {
		entries = append(entries, [2]string{name, body})
	}
	return gzipTarEntries(t, entries)
}

// gzipTarEntries writes the entries in order, so a duplicate path can be
// staged deliberately.
func gzipTarEntries(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, file := range files {
		name, body := file[0], file[1]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("hdr %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("body %q: %v", name, err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// readObject drains one stored object through the production decoder.
func readObject(t *testing.T, blobs *storage.CompressedBlobStore, key string) ([]byte, error) {
	t.Helper()
	rc, _, err := blobs.GetReader(key)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}

// keyLedger records every key written through it, so a test can ask the real
// store which of them still exist.
type keyLedger struct {
	storage.InnerBlobStore
	mu   sync.Mutex
	keys []string
}

func (l *keyLedger) Put(key string, r io.Reader, size int64) error {
	l.mu.Lock()
	l.keys = append(l.keys, key)
	l.mu.Unlock()
	return l.InnerBlobStore.Put(key, r, size)
}

// objectsUnder counts the objects under uploads/ that the store still holds,
// so a test can prove a failed upload left nothing behind.
func objectsUnder(t *testing.T, l *keyLedger) int {
	t.Helper()
	l.mu.Lock()
	keys := slices.Clone(l.keys)
	l.mu.Unlock()
	slices.Sort(keys)
	n := 0
	for _, key := range slices.Compact(keys) {
		if !strings.HasPrefix(key, "uploads/") {
			continue
		}
		rc, _, err := l.GetReader(key)
		switch {
		case err == nil:
			_ = rc.Close()
			n++
		case !errors.Is(err, storage.ErrNotFound):
			t.Fatalf("read %s: %v", key, err)
		}
	}
	return n
}

// newRoomRepo is the room surface over the test's cells.
func newRoomRepo(t *testing.T) *celld.RoomRepo {
	t.Helper()
	rooms := celld.NewRoomRepo(celldtest.Endpoint(t), nil)
	rooms.PushSubject = "https://paste.test"
	return rooms
}
