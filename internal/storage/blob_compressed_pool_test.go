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
	c := NewCompressedBlobStore(newFakeDurable())

	blobs := map[string][]byte{}
	for i, n := range []int{10, 4096, 200_000, 1 << 20} {
		body := bytes.Repeat(fmt.Appendf(nil, "blob-%d-payload-", i), n/16+1)
		key := fmt.Sprintf("uploads/pool/%d", i)
		putEncoded(t, c, key, body)
		blobs[key] = body
	}

	readFull := func(key string) []byte {
		return readKey(t, c, key)
	}

	// Interleaving forces the pool to hand back reused decoders.
	for round := range 50 {
		for key, want := range blobs {
			if got := readFull(key); !bytes.Equal(got, want) {
				t.Fatalf("round %d: decode mismatch (got %d, want %d bytes)", round, len(got), len(want))
			}
		}
	}

	// Aborted download: the decoder returns to the pool via Reset(nil), and
	// the next borrower must still be clean.
	for key := range blobs {
		rc, _, err := c.GetReader(key)
		if err != nil {
			t.Fatalf("GetReader: %v", err)
		}
		_, _ = rc.Read(make([]byte, 8))
		_ = rc.Close()
		break
	}
	for key, want := range blobs {
		if got := readFull(key); !bytes.Equal(got, want) {
			t.Fatalf("after partial-close, decode mismatch (got %d, want %d bytes)", len(got), len(want))
		}
	}
}

// TestDecoderPoolConcurrent pins that a pooled decoder is never handed to two
// readers at once and that concurrent decodes stay byte-exact. Run with -race.
func TestDecoderPoolConcurrent(t *testing.T) {
	c := NewCompressedBlobStore(newFakeDurable())
	body := bytes.Repeat([]byte("concurrent-zstd-pool-payload "), 50_000)
	putEncoded(t, c, "uploads/pool/0", body)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 30 {
				rc, _, err := c.GetReader("uploads/pool/0")
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
