package storage

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"testing"
)

// TestDecoderPoolReuse pins that a pooled zstd decoder carries no state between
// borrows: every decode stays byte-exact across many reuses and after an
// aborted read (Close before EOF).
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

	// Interleaving forces the pool to hand back reused decoders.
	for round := range 50 {
		for sha, want := range blobs {
			if got := readFull(sha); !bytes.Equal(got, want) {
				t.Fatalf("round %d: decode mismatch (got %d, want %d bytes)", round, len(got), len(want))
			}
		}
	}

	// Aborted download: the decoder returns to the pool via Reset(nil), and
	// the next borrower must still be clean.
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

// TestDecoderPoolConcurrent pins that a pooled decoder is never handed to two
// readers at once and that concurrent decodes stay byte-exact. Run with -race.
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
