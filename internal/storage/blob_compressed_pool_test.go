package storage

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"testing"
)

// TestDecoderPoolReuse exercises the pooled streaming zstd decoder across
// many interleaved reads (which force the pool to reuse decoders) plus an
// aborted read (Close before EOF), asserting every decode is byte-exact.
// It is the correctness guard for the pooling change: a decoder whose
// state leaked from a prior read would corrupt a later one.
func TestDecoderPoolReuse(t *testing.T) {
	c := NewCompressedBlobStore(newFakeRawStore())

	blobs := map[string][]byte{}
	for i, n := range []int{10, 4096, 200_000, 1 << 20} {
		body := bytes.Repeat(fmt.Appendf(nil, "blob-%d-payload-", i), n/16+1)
		sha := shaOf(body)
		if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("Put: %v", err)
		}
		blobs[sha] = body
	}

	readFull := func(sha string) []byte {
		rc, _, err := c.GetReader(sha)
		if err != nil {
			t.Fatalf("GetReader: %v", err)
		}
		defer func() { _ = rc.Close() }()
		out, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		return out
	}

	// Many interleaved full reads: the pool hands a reused decoder back on
	// most of these, and each decode must reproduce the exact bytes.
	for round := range 50 {
		for sha, want := range blobs {
			if got := readFull(sha); !bytes.Equal(got, want) {
				t.Fatalf("round %d: decode mismatch (got %d, want %d bytes)", round, len(got), len(want))
			}
		}
	}

	// Aborted download: read a few bytes then Close before EOF. The decoder
	// returns to the pool via Reset(nil); the next borrower must be clean.
	for sha := range blobs {
		rc, _, err := c.GetReader(sha)
		if err != nil {
			t.Fatalf("GetReader: %v", err)
		}
		_, _ = rc.Read(make([]byte, 8))
		_ = rc.Close()
		break
	}
	for sha, want := range blobs {
		if got := readFull(sha); !bytes.Equal(got, want) {
			t.Fatalf("after partial-close, decode mismatch (got %d, want %d bytes)", len(got), len(want))
		}
	}
}

// TestDecoderPoolConcurrent hammers GetReader from many goroutines so the
// pool hands out decoders concurrently. Run with -race: it proves a pooled
// decoder is never shared across two readers at once (Get removes it from
// the pool) and that concurrent decode stays correct.
func TestDecoderPoolConcurrent(t *testing.T) {
	c := NewCompressedBlobStore(newFakeRawStore())
	body := bytes.Repeat([]byte("concurrent-zstd-pool-payload "), 50_000)
	sha := shaOf(body)
	if err := c.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 30 {
				rc, _, err := c.GetReader(sha)
				if err != nil {
					t.Errorf("GetReader: %v", err)
					return
				}
				out, rerr := io.ReadAll(rc)
				_ = rc.Close()
				if rerr != nil || !bytes.Equal(out, body) {
					t.Errorf("concurrent decode mismatch: err=%v len=%d", rerr, len(out))
					return
				}
			}
		})
	}
	wg.Wait()
}
