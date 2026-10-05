package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/zstdenc"
)

func TestCompressedBlobStore_StoresMagicHeader(t *testing.T) {
	body := []byte("compressible text - repeats well " + strings.Repeat("xy", 1000))
	inner := newFakeDurable()
	putEncoded(t, NewCompressedBlobStore(inner), "uploads/u1/0", body)
	if raw := inner.raw("uploads/u1/0"); !bytes.HasPrefix(raw, zstdenc.Magic[:]) {
		t.Fatalf("inner store missing magic prefix: first 4 bytes = % x", raw[:4])
	}
}

func TestCompressedBlobStore_ActuallyCompresses(t *testing.T) {
	body := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 1000)
	inner := newFakeDurable()
	putEncoded(t, NewCompressedBlobStore(inner), "uploads/u1/0", body)
	stored := len(inner.raw("uploads/u1/0"))
	if stored >= len(body) {
		t.Fatalf("compression did not reduce size: input=%d stored=%d", len(body), stored)
	}
	// Redundant text should hit at least 5x under zstd.
	if ratio := float64(len(body)) / float64(stored); ratio < 5 {
		t.Fatalf("compression ratio too low: %.1fx (input=%d, stored=%d)", ratio, len(body), stored)
	}
}

// Every encoded shape decodes byte-identically.
func TestCompressedBlobStore_ReadsRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"compressible": bytes.Repeat([]byte("the quick brown fox\n"), 5000),
		"tiny":         []byte("<h1>hi</h1>"),
		"empty":        nil,
		"binary":       {0x00, 0x01, 0x02, 0xff, 0xfe, 'H', 'Z', 0x00},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewCompressedBlobStore(newFakeDurable())
			putEncoded(t, c, "uploads/u1/0", body)
			if got := readKey(t, c, "uploads/u1/0"); !bytes.Equal(got, body) {
				t.Fatalf("Read: got %d bytes, want %d", len(got), len(body))
			}
		})
	}
}

// An object without the magic is damaged: its read fails up front, and the
// objects beside it stay readable.
func TestCompressedBlobStore_UnframedObjectFailsAlone(t *testing.T) {
	for name, stored := range map[string][]byte{
		"uncompressed":  []byte("<!doctype html><h1>raw</h1>"),
		"short":         []byte("hi\n"),
		"partial-magic": zstdenc.Magic[:3],
		"empty":         {},
	} {
		t.Run(name, func(t *testing.T) {
			inner := newFakeDurable()
			c := NewCompressedBlobStore(inner)
			inner.putRaw("uploads/u1/0", stored)
			putEncoded(t, c, "uploads/u1/1", []byte("neighbour"))
			if rc, _, err := c.Read(context.Background(), domain.ManifestEntry{Key: "uploads/u1/0"}); !errors.Is(err, errUnframed) || rc != nil {
				t.Fatalf("Read unframed = (%v, %v), want errUnframed", rc, err)
			}
			if got := readKey(t, c, "uploads/u1/1"); string(got) != "neighbour" {
				t.Fatalf("neighbour read %q", got)
			}
		})
	}
}

func TestCompressedBlobStore_ReadsPropagateNotFound(t *testing.T) {
	c := NewCompressedBlobStore(newFakeDurable())
	if _, _, err := c.Read(context.Background(), domain.ManifestEntry{Key: "uploads/missing/0"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read: expected ErrNotFound, got %v", err)
	}
}

// An entry without an object key names no bytes, so its read never reaches the
// inner store: the nil inner panics on any call.
func TestCompressedBlobStore_KeylessReadNeverReachesTheStore(t *testing.T) {
	c := NewCompressedBlobStore(nil)
	entry := domain.ManifestEntry{Size: 3, CompressedSize: 2}
	if rc, _, err := c.Read(context.Background(), entry); !errors.Is(err, domain.ErrNotFound) || rc != nil {
		t.Fatalf("Read keyless entry = (%v, %v), want ErrNotFound", rc, err)
	}
}

// StageEncoding reports the payload size the quota charges, which excludes the
// magic, and hands the store the exact encoded length.
func TestCompressedBlobStore_StageEncodingSizes(t *testing.T) {
	inner := &sizeProbe{fakeDurable: newFakeDurable()}
	payload, err := NewCompressedBlobStore(inner).StageEncoding(context.Background(), "uploads/u1/0", strings.NewReader(strings.Repeat("abc", 500)))
	if err != nil {
		t.Fatalf("StageEncoding: %v", err)
	}
	stored := len(inner.raw("uploads/u1/0"))
	if inner.size != int64(stored) || payload != stored-len(zstdenc.Magic) {
		t.Fatalf("payload/size = %d/%d for %d stored bytes", payload, inner.size, stored)
	}
}

type sizeProbe struct {
	*fakeDurable
	size int64
	r    io.Reader
}

func (p *sizeProbe) Put(key string, r io.Reader, size int64) error {
	p.size, p.r = size, r
	return p.fakeDurable.Put(key, r, size)
}

// StagePrecompressed forwards the caller's stream instead of materializing it.
func TestCompressedBlobStore_StagePrecompressedStreamsOriginalReader(t *testing.T) {
	inner := &sizeProbe{fakeDurable: newFakeDurable()}
	body := bytes.NewReader([]byte("encoded body"))
	if err := NewCompressedBlobStore(inner).StagePrecompressed(context.Background(), "uploads/u/0", body, 12); err != nil {
		t.Fatalf("StagePrecompressed: %v", err)
	}
	if inner.r != body || inner.size != 12 || string(inner.raw("uploads/u/0")) != "encoded body" {
		t.Fatalf("forwarded reader same=%v size=%d body=%q", inner.r == body, inner.size, inner.raw("uploads/u/0"))
	}
}

// DeleteUpload removes one upload's objects only, and refuses ids that are not
// a single prefix segment.
func TestCompressedBlobStore_DeleteUpload(t *testing.T) {
	inner := newFakeDurable()
	c := NewCompressedBlobStore(inner)
	id, other := domain.NewUploadID(), domain.NewUploadID()
	for _, key := range []string{domain.UploadObjectKey(id, 0), domain.UploadObjectKey(id, 1), domain.UploadObjectKey(other, 0)} {
		putEncoded(t, c, key, []byte(key))
	}
	if err := c.DeleteUpload(context.Background(), id); err != nil {
		t.Fatalf("DeleteUpload: %v", err)
	}
	for _, n := range []int{0, 1} {
		if inner.raw(domain.UploadObjectKey(id, n)) != nil {
			t.Fatalf("object %d survived its upload's delete", n)
		}
	}
	if inner.raw(domain.UploadObjectKey(other, 0)) == nil {
		t.Fatal("another upload's object was deleted")
	}
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if err := c.DeleteUpload(context.Background(), bad); err == nil {
			t.Fatalf("DeleteUpload(%q) = nil, want a refusal", bad)
		}
	}
}
