package storage

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCompressedBlobStore_StoresMagicHeader(t *testing.T) {
	body := []byte("compressible text - repeats well " + strings.Repeat("xy", 1000))
	inner := newFakeDurable()
	putEncoded(t, NewCompressedBlobStore(inner), "uploads/u1/0", body)
	raw := readKey(t, inner, "uploads/u1/0")
	if !hasMagicV1(raw) {
		t.Fatalf("inner store missing magic prefix: first 4 bytes = % x", raw[:4])
	}
}

func TestCompressedBlobStore_ActuallyCompresses(t *testing.T) {
	body := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 1000)
	inner := newFakeDurable()
	putEncoded(t, NewCompressedBlobStore(inner), "uploads/u1/0", body)
	stored := inner.rawSize("uploads/u1/0")
	if stored >= len(body) {
		t.Fatalf("compression did not reduce size: input=%d stored=%d", len(body), stored)
	}
	// Redundant text should hit at least 5x under zstd.
	if ratio := float64(len(body)) / float64(stored); ratio < 5 {
		t.Fatalf("compression ratio too low: %.1fx (input=%d, stored=%d)", ratio, len(body), stored)
	}
}

// Every stored shape decodes byte-identically, including objects stored
// without the magic prefix.
func TestCompressedBlobStore_ReadsRoundTrip(t *testing.T) {
	cases := map[string]struct {
		body []byte
		raw  bool
	}{
		"compressible": {body: bytes.Repeat([]byte("the quick brown fox\n"), 5000)},
		"tiny":         {body: []byte("<h1>hi</h1>")},
		"empty":        {body: nil},
		"uncompressed": {body: []byte("<!doctype html><h1>raw</h1>"), raw: true},
		"short-raw":    {body: []byte("hi\n"), raw: true},
		"binary":       {body: []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 'H', 'Z', 0x00}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			inner := newFakeDurable()
			c := NewCompressedBlobStore(inner)
			if tc.raw {
				inner.putRaw("uploads/u1/0", tc.body)
			} else {
				putEncoded(t, c, "uploads/u1/0", tc.body)
			}
			if got := readKey(t, c, "uploads/u1/0"); !bytes.Equal(got, tc.body) {
				t.Fatalf("GetReader: got %d bytes, want %d", len(got), len(tc.body))
			}
		})
	}
}

func TestCompressedBlobStore_ReadsPropagateNotFound(t *testing.T) {
	c := NewCompressedBlobStore(newFakeDurable())
	if _, _, err := c.GetReader("uploads/missing/0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetReader: expected ErrNotFound, got %v", err)
	}
}

// EncodeTo reports the payload size the quota charges, which excludes framing.
func TestCompressedBlobStore_EncodeToSizes(t *testing.T) {
	c := NewCompressedBlobStore(newFakeDurable())
	var buf bytes.Buffer
	payload, total, err := c.EncodeTo(&buf, strings.NewReader(strings.Repeat("abc", 500)))
	if err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}
	if total != int64(buf.Len()) || payload != buf.Len()-CompressedBodyPrefixLen {
		t.Fatalf("payload/total = %d/%d for %d written bytes", payload, total, buf.Len())
	}
}
