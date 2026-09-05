package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// fixedNow is the clock every real-stack fixture injects.
var fixedNow = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

// realBlobs is the production blob stack (compressed over disk) under
// t.TempDir().
func realBlobs(t *testing.T) *storage.CompressedBlobStore {
	t.Helper()
	disk, err := storage.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	return storage.NewCompressedBlobStore(disk)
}

// newStack wires the upload and manage services over real metadata and real
// blobs: the same stack production runs, no mocks.
func newStack(t *testing.T) (*Upload, *Manage, *storage.MemRepo) {
	t.Helper()
	blobs := realBlobs(t)
	repo := storagetest.NewRepo(t)
	upload := NewUpload(repo, NewStandaloneBlobUnit(blobs))
	// Blobs are written by a background finalizer goroutine. Drain it before
	// the t.TempDir() cleanup (registered above, so LIFO runs it later)
	// RemoveAll's the blob dir out from under an in-flight finalize.
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

// sha256Hex is the content address the blob path stores bytes under.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
