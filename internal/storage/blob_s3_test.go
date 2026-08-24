package storage_test

// The S3 blob store against a real object store.
//
// Stated as PROPERTIES the disk store also holds, because the two are meant to
// be interchangeable: anything asserted here that disk would fail is a
// divergence, not a feature. Skipped unless MINIO_TEST_ENDPOINT is set.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

func newS3Blobs(t *testing.T) *storage.S3BlobStore {
	t.Helper()
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping the s3 blob store tests")
	}
	// A per-run prefix, so a rerun cannot see the previous run's blobs and the
	// "absent" assertions stay meaningful against a durable bucket.
	bs, err := storage.NewS3BlobStore(storage.S3BlobConfig{
		Endpoint:  endpoint,
		Bucket:    envOrDefaultTest("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata"),
		Region:    "us-east-1",
		AccessKey: envOrDefaultTest("MINIO_TEST_ACCESS_KEY", "admin"),
		SecretKey: envOrDefaultTest("MINIO_TEST_SECRET_KEY", "supersecret"),
		Prefix:    fmt.Sprintf("blobtest/%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("new s3 blob store: %v", err)
	}
	return bs
}

func envOrDefaultTest(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Bytes come back exactly as written, through both read paths.
func TestS3BlobRoundTrip(t *testing.T) {
	bs := newS3Blobs(t)
	body := []byte("the bytes a paste is made of\n")
	sha := shaOf(body)
	if err := bs.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := bs.Get(sha)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("Get returned %q, want %q", got, body)
	}

	rc, n, err := bs.GetReader(sha)
	if err != nil {
		t.Fatalf("get reader: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	streamed, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if !bytes.Equal(streamed, body) {
		t.Fatalf("GetReader streamed %q, want %q", streamed, body)
	}
	if n != int64(len(body)) {
		t.Fatalf("GetReader reported length %d, want %d", n, len(body))
	}
}

// A missing blob is ErrNotFound, NOT an opaque transport error: the serving
// path distinguishes "no such paste" from "the store is broken", and it can
// only do that if this translation happens here.
func TestS3BlobMissingIsNotFound(t *testing.T) {
	bs := newS3Blobs(t)
	if _, err := bs.Get(shaOf([]byte("never written"))); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get of an absent blob = %v, want storage.ErrNotFound", err)
	}
	if _, _, err := bs.GetReader(shaOf([]byte("never written"))); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetReader of an absent blob = %v, want storage.ErrNotFound", err)
	}
}

// Re-putting the same sha is harmless. Content addressing means the bytes are
// identical by construction, so a racing double write must not be an error.
func TestS3BlobPutIsIdempotent(t *testing.T) {
	bs := newS3Blobs(t)
	body := []byte("written twice")
	sha := shaOf(body)
	for i := range 2 {
		if err := bs.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	got, err := bs.Get(sha)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("after a repeated put, Get = (%q, %v)", got, err)
	}
}

// Removing an absent blob is success. The sweep races real deletes, and an
// error here would turn a benign race into a failed reclaim.
func TestS3BlobRemoveIsIdempotent(t *testing.T) {
	bs := newS3Blobs(t)
	body := []byte("removed twice")
	sha := shaOf(body)
	if err := bs.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("put: %v", err)
	}
	for i := range 2 {
		if err := bs.Remove(sha); err != nil {
			t.Fatalf("remove %d: %v", i, err)
		}
	}
	if _, err := bs.Get(sha); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("after remove, Get = %v, want ErrNotFound", err)
	}
}

// The walk reports exactly the shas stored, which is what the offline sweep
// compares against the live records.
func TestS3BlobWalkListsWhatWasWritten(t *testing.T) {
	bs := newS3Blobs(t)
	want := map[string]bool{}
	for i := range 3 {
		body := fmt.Appendf(nil, "walk body %d", i)
		sha := shaOf(body)
		want[sha] = true
		if err := bs.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	got := map[string]bool{}
	if err := bs.WalkBlobs(func(sha string) error { got[sha] = true; return nil }); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for sha := range want {
		if !got[sha] {
			t.Fatalf("walk omitted %s; a sweep that cannot see a blob can never reclaim it", sha)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("walk returned %d shas, want %d", len(got), len(want))
	}
}
