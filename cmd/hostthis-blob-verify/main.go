// Command hostthis-blob-verify checks that every blob in the on-disk
// store is present in the S3-compatible bucket with identical bytes.
// Exits non-zero on the first mismatch.
//
// Because blobs are content-addressed by SHA256, "identical bytes" is
// proven by fetching the S3 object and re-hashing - if the hash of
// what we got matches the key we asked for, the bytes are correct.
// (The disk store's filename is the SHA, so we trust it too - but
// we re-hash the disk bytes for symmetry and to catch on-disk bit-rot.)
//
// Run AFTER hostthis-blob-migrate, BEFORE flipping HOSTTHIS_BLOB_BACKEND.
//
// Usage:
//
//	go run ./cmd/hostthis-blob-verify
//	# or
//	hostthis-blob-verify
package main

import (
	"crypto/sha256"
	"encoding/hex"
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
	dataDir := flag.String("data-dir", envOr("HOSTTHIS_DATA_DIR", "./data"), "where the on-disk blob store lives")
	flag.Parse()
	logger := log.New(os.Stderr, "blob-verify ", log.LstdFlags|log.LUTC)

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
		checked    int
		mismatches int
		missing    int
		diskRot    int
	)
	err = src.WalkBlobs(func(sha string) error {
		diskBytes, err := src.Get(sha)
		if err != nil {
			logger.Printf("disk read %s: %v", sha, err)
			mismatches++
			return nil
		}
		diskSum := sha256.Sum256(diskBytes)
		diskHash := hex.EncodeToString(diskSum[:])
		if diskHash != sha {
			logger.Printf("disk ROT: file %s has hash %s - disk corruption, not an s3 issue", sha, diskHash)
			diskRot++
			return nil
		}
		s3Bytes, err := dst.Get(sha)
		if err != nil {
			logger.Printf("MISSING in s3: %s (%v)", sha, err)
			missing++
			return nil
		}
		s3Sum := sha256.Sum256(s3Bytes)
		s3Hash := hex.EncodeToString(s3Sum[:])
		if s3Hash != sha {
			logger.Printf("S3 MISMATCH: %s - s3 returned bytes hashing to %s", sha, s3Hash)
			mismatches++
			return nil
		}
		checked++
		if checked%50 == 0 {
			logger.Printf("progress: %d ok, %d missing, %d mismatches, %d disk rot", checked, missing, mismatches, diskRot)
		}
		return nil
	})
	if err != nil {
		logger.Fatalf("walk: %v", err)
	}

	elapsed := time.Since(start).Truncate(time.Millisecond)
	logger.Printf("done: %d verified, %d missing, %d mismatches, %d disk rot in %s",
		checked, missing, mismatches, diskRot, elapsed)
	if missing > 0 || mismatches > 0 || diskRot > 0 {
		fmt.Fprintln(os.Stderr, "verification FAILED; do NOT flip HOSTTHIS_BLOB_BACKEND yet")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "verification PASSED; safe to set HOSTTHIS_BLOB_BACKEND=s3 and restart")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
