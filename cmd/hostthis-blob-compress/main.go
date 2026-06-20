// Command hostthis-blob-compress walks every blob in the on-disk standalone
// store and re-encodes legacy uncompressed blobs as `HZ\0\x01 + zstd(bytes)` -
// the format the runtime now writes.
//
// Idempotent: blobs that already carry the magic header are skipped.
// Safe to re-run; safe to run while hostthisd is up (the runtime's
// read path tolerates both encodings).
//
// Usage:
//
//	HOSTTHIS_DATA_DIR=./data go run ./cmd/hostthis-blob-compress
//
// After it completes the blob store occupies ~5-10x less space on
// typical HTML/Markdown payloads. (The detached S3 standalone backend was
// retired with the shale-collocated blob work; production blobs live in the
// shale-managed object store, which writes the compressed format natively.)
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/Zamua/hostthis/internal/storage"
)

// rawBlobStore is the minimum we need from a backend: walk shas + read
// raw bytes + write raw bytes (no compression-layer wrapping).
// *storage.BlobStore (the disk store) satisfies this.
type rawBlobStore interface {
	WalkBlobs(fn func(sha string) error) error
	Get(sha string) ([]byte, error)
	Put(sha string, r io.Reader, size int64) error
}

var magicV1 = [4]byte{'H', 'Z', 0x00, 0x01}

func hasMagic(b []byte) bool {
	return len(b) >= 4 && b[0] == magicV1[0] && b[1] == magicV1[1] && b[2] == magicV1[2] && b[3] == magicV1[3]
}

func main() {
	dataDir := flag.String("data-dir", envOr("HOSTTHIS_DATA_DIR", "./data"), "data dir root")
	dryRun := flag.Bool("dry-run", false, "report what would be compressed; don't write")
	flag.Parse()
	logger := log.New(os.Stderr, "blob-compress ", log.LstdFlags|log.LUTC)

	bs, err := storage.NewBlobStore(filepath.Join(*dataDir, "blobs"))
	if err != nil {
		logger.Fatalf("open disk store: %v", err)
	}
	var store rawBlobStore = bs
	logger.Printf("backend: disk at %s/blobs", *dataDir)

	if *dryRun {
		logger.Print("DRY RUN - no writes will happen")
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		logger.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()

	start := time.Now()
	var (
		total              int
		alreadyCompressed  int
		compressed         int
		failures           int
		rawBytesIn         int64
		compressedBytesOut int64
	)
	walkErr := store.WalkBlobs(func(sha string) error {
		total++
		body, err := store.Get(sha)
		if err != nil {
			logger.Printf("  FAIL get %s: %v", sha, err)
			failures++
			return nil
		}
		if hasMagic(body) {
			alreadyCompressed++
			return nil
		}
		rawBytesIn += int64(len(body))
		// magic + zstd(body)
		out := make([]byte, 0, len(magicV1)+len(body))
		out = append(out, magicV1[:]...)
		out = enc.EncodeAll(body, out)
		compressedBytesOut += int64(len(out))
		if *dryRun {
			compressed++
			return nil
		}
		// Re-Put with the compressed body. The disk store's Put is
		// content-addressed but we want it to OVERWRITE here - Put is a
		// no-op when the file already exists, so we hit a sub-API directly.
		if err := putRawOverwrite(store, sha, out); err != nil {
			logger.Printf("  FAIL put %s: %v", sha, err)
			failures++
			return nil
		}
		compressed++
		return nil
	})
	if walkErr != nil {
		logger.Fatalf("walk: %v", walkErr)
	}

	logger.Printf("done in %v: walked=%d already-compressed=%d newly-compressed=%d failed=%d",
		time.Since(start), total, alreadyCompressed, compressed, failures)
	if rawBytesIn > 0 {
		ratio := float64(rawBytesIn) / float64(compressedBytesOut)
		logger.Printf("compressed: %s → %s (%.1fx)", humanBytes(rawBytesIn), humanBytes(compressedBytesOut), ratio)
	}
	if failures > 0 {
		os.Exit(1)
	}
}

// putRawOverwrite forces an overwrite by routing through the disk store's
// own overwrite-capable API (PutBytesOverwrite), which bypasses the
// content-addressed skip.
func putRawOverwrite(store rawBlobStore, sha string, body []byte) error {
	type overwriter interface {
		PutBytesOverwrite(sha string, body []byte) error
	}
	if ow, ok := store.(overwriter); ok {
		return ow.PutBytesOverwrite(sha, body)
	}
	// Fallback (shouldn't happen - the disk store implements the overwrite
	// path). Try the regular Put; if the backend is content-addressed-skip,
	// this no-ops.
	return store.Put(sha, bytes.NewReader(body), int64(len(body)))
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1fG", float64(n)/(1024*1024*1024))
	}
}
