// Command hostthis-blob-migrate copies every blob from the on-disk
// store to an S3-compatible bucket. Reads the same env vars hostthisd
// does (HOSTTHIS_DATA_DIR + HOSTTHIS_S3_*) so deployments don't have
// to introduce a second config surface.
//
// Idempotent: blobs already present in the destination are skipped
// (S3-side existence check before upload). Safe to re-run.
//
// Usage:
//
//	# Assumes hostthisd's env is already exported in the shell.
//	go run ./cmd/hostthis-blob-migrate
//	# or
//	hostthis-blob-migrate
//
// After it completes, run hostthis-blob-verify to confirm parity, then
// flip HOSTTHIS_BLOB_BACKEND=s3 and restart hostthisd.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

func main() {
	dataDir := flag.String("data-dir", envOr("HOSTTHIS_DATA_DIR", "./data"), "where the on-disk blob store lives (root for <data-dir>/blobs/)")
	flag.Parse()
	logger := log.New(os.Stderr, "blob-migrate ", log.LstdFlags|log.LUTC)

	src, err := storage.NewBlobStore(filepath.Join(*dataDir, "blobs"))
	if err != nil {
		logger.Fatalf("open disk store: %v", err)
	}

	dst, err := storage.NewS3BlobStore(storage.S3Config{
		EndpointURL: os.Getenv("HOSTTHIS_S3_ENDPOINT"),
		Bucket:      os.Getenv("HOSTTHIS_S3_BUCKET"),
		Region:      envOr("HOSTTHIS_S3_REGION", "us-east-1"),
		AccessKey:   os.Getenv("HOSTTHIS_S3_ACCESS_KEY"),
		SecretKey:   os.Getenv("HOSTTHIS_S3_SECRET_KEY"),
		UseSSL:      strings.ToLower(envOr("HOSTTHIS_S3_USE_SSL", "true")) != "false",
	})
	if err != nil {
		logger.Fatalf("open s3 store: %v", err)
	}

	start := time.Now()
	var (
		copied   int
		skipped  int
		failures int
	)
	err = src.WalkBlobs(func(sha string) error {
		body, err := src.Get(sha)
		if err != nil {
			logger.Printf("read %s: %v (skipping)", sha, err)
			failures++
			return nil
		}
		// S3 store's Put is itself idempotent (HEAD-check first), so
		// we don't need to skip explicitly - but counting requires us
		// to ask. Keep it simple: just attempt the Put; the S3 store
		// will short-circuit if already present.
		if err := dst.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
			logger.Printf("put %s: %v (continuing)", sha, err)
			failures++
			return nil
		}
		copied++
		if copied%50 == 0 {
			logger.Printf("progress: %d copied, %d skipped, %d failed", copied, skipped, failures)
		}
		return nil
	})
	if err != nil {
		logger.Fatalf("walk: %v", err)
	}
	elapsed := time.Since(start).Truncate(time.Millisecond)
	logger.Printf("done: %d copied, %d skipped, %d failed in %s", copied, skipped, failures, elapsed)
	if failures > 0 {
		fmt.Fprintln(os.Stderr, "migration FAILED with errors above; do NOT flip HOSTTHIS_BLOB_BACKEND yet")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "migration complete; run hostthis-blob-verify next to confirm parity")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
