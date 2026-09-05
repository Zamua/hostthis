package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestCompressedBlobStore_StoresMagicHeader(t *testing.T) {
	body := []byte("compressible text - repeats well " + strings.Repeat("xy", 1000))
	sha := shaOf(body)
	inner := newFakeDurable()
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
	body := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 1000)
	sha := shaOf(body)
	inner := newFakeDurable()
	c := NewCompressedBlobStore(inner)
	if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stored := inner.rawSize(sha)
	if stored >= len(body) {
		t.Fatalf("compression did not reduce size: input=%d stored=%d", len(body), stored)
	}
	// Redundant text should hit at least 5x under zstd.
	if ratio := float64(len(body)) / float64(stored); ratio < 5 {
		t.Fatalf("compression ratio too low: %.1fx (input=%d, stored=%d)", ratio, len(body), stored)
	}
}

func TestCompressedBlobStore_GetPropagatesNotFound(t *testing.T) {
	inner := newFakeDurable()
	c := NewCompressedBlobStore(inner)
	_, err := c.Get(shaOf([]byte("missing")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestCompressedBlobStore_GetReaderMatchesGet pins that every blob shape,
// legacy uncompressed bodies included, round-trips through the buffered Get
// and byte-identically through the streaming GetReader.
func TestCompressedBlobStore_GetReaderMatchesGet(t *testing.T) {
	cases := map[string]struct {
		body   []byte
		legacy bool
	}{
		"compressible": {body: bytes.Repeat([]byte("the quick brown fox\n"), 5000)},
		"tiny":         {body: []byte("<h1>hi</h1>")},
		"empty":        {body: nil},
		"legacy":       {body: []byte("<!doctype html><h1>legacy</h1>"), legacy: true},
		"legacy-short": {body: []byte("hi\n"), legacy: true},
		"binary":       {body: []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 'H', 'Z', 0x00}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sha := shaOf(tc.body)
			inner := newFakeDurable()
			c := NewCompressedBlobStore(inner)
			if tc.legacy {
				inner.putRaw(sha, tc.body)
			} else if err := c.Put(sha, bytes.NewReader(tc.body), int64(len(tc.body))); err != nil {
				t.Fatalf("Put: %v", err)
			}

			want, err := c.Get(sha)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(want, tc.body) {
				t.Fatalf("Get round-trip: want %d bytes, got %d", len(tc.body), len(want))
			}
			rc, _, err := c.GetReader(sha)
			if err != nil {
				t.Fatalf("GetReader: %v", err)
			}
			defer rc.Close() //nolint:errcheck
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("GetReader != Get: want %d bytes %q, got %d bytes %q",
					len(want), truncForLog(want), len(got), truncForLog(got))
			}
		})
	}
}

func TestCompressedBlobStore_GetReaderPropagatesNotFound(t *testing.T) {
	inner := newFakeDurable()
	c := NewCompressedBlobStore(inner)
	_, _, err := c.GetReader(shaOf([]byte("missing")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func truncForLog(b []byte) []byte {
	if len(b) > 32 {
		return b[:32]
	}
	return b
}
