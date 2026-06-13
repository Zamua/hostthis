package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestS3BlobStore_RoundTrip exercises the full Put/Get/Walk/Remove
// surface against a live S3-compatible endpoint. Skipped unless the
// MINIO_TEST_ENDPOINT env var is set - usually via `make test-s3`,
// which brings up a MinIO via docker compose first.
//
// The test creates blobs with unique random-ish content so re-runs
// against the same bucket don't pollute later runs and don't see each
// other's data (each test sees only the keys it just wrote).
func TestS3BlobStore_RoundTrip(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping (start dev MinIO first)")
	}
	bucket := envOr(t, "MINIO_TEST_BUCKET", "hostthis-blobs")
	access := envOr(t, "MINIO_TEST_ACCESS_KEY", "admin")
	secret := envOr(t, "MINIO_TEST_SECRET_KEY", "supersecret")

	store, err := NewS3BlobStore(S3Config{
		EndpointURL: endpoint,
		Bucket:      bucket,
		Region:      "us-east-1",
		AccessKey:   access,
		SecretKey:   secret,
		UseSSL:      false,
	})
	if err != nil {
		t.Fatalf("NewS3BlobStore: %v", err)
	}

	// Random-ish keyspace per test run so concurrent CI doesn't collide.
	tag := t.Name()
	put := func(body []byte) string {
		t.Helper()
		sum := sha256.Sum256(append([]byte(tag+":"), body...))
		sha := hex.EncodeToString(sum[:])
		if err := store.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("Put(%s): %v", sha, err)
		}
		t.Cleanup(func() { _ = store.Remove(sha) })
		return sha
	}

	bodyA := []byte("blob-a-content")
	bodyB := []byte("blob-b-content (different)")
	shaA := put(bodyA)
	shaB := put(bodyB)

	gotA, err := store.Get(shaA)
	if err != nil {
		t.Fatalf("Get(A): %v", err)
	}
	if !bytes.Equal(gotA, bodyA) {
		t.Fatalf("Get(A) returned %q, want %q", gotA, bodyA)
	}

	gotB, err := store.Get(shaB)
	if err != nil {
		t.Fatalf("Get(B): %v", err)
	}
	if !bytes.Equal(gotB, bodyB) {
		t.Fatalf("Get(B) returned %q, want %q", gotB, bodyB)
	}

	// Put-idempotency: re-Putting an existing sha is a no-op and doesn't error.
	if err := store.Put(shaA, bytes.NewReader(bodyA), int64(len(bodyA))); err != nil {
		t.Fatalf("Put-idempotent(A): %v", err)
	}

	// Walk visits at least the two we wrote (other tests may have left junk).
	var seen []string
	err = store.WalkBlobs(func(sha string) error {
		seen = append(seen, sha)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkBlobs: %v", err)
	}
	sort.Strings(seen)
	if !contains(seen, shaA) || !contains(seen, shaB) {
		t.Fatalf("WalkBlobs missed our keys; got %d entries, neither %s nor %s found in result", len(seen), shaA, shaB)
	}

	// Remove(A) → Get(A) returns ErrNotFound.
	if err := store.Remove(shaA); err != nil {
		t.Fatalf("Remove(A): %v", err)
	}
	if _, err := store.Get(shaA); !errors.Is(err, ErrNotFound) {
		// MinIO surface for unknown key: depends on driver version.
		// We accept either ErrNotFound OR an error string containing
		// "NoSuchKey" - both indicate the desired absent state.
		if err == nil || !containsString(err.Error(), "NoSuchKey") {
			t.Fatalf("Get(A) after remove: got %v, want ErrNotFound or NoSuchKey", err)
		}
	}

	// Remove of missing key is a no-op.
	if err := store.Remove(shaA); err != nil {
		t.Errorf("Remove of missing key: got %v, want nil", err)
	}
}

func envOr(t *testing.T, key, fallback string) string {
	t.Helper()
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func contains(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

func containsString(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && bytes.Contains([]byte(haystack), []byte(needle))
}

// TestIsS3QuotaExceeded pins the robust detection of an object-store
// bucket-quota rejection. The blob Put maps these to storage.ErrServiceFull
// (the durable total-bytes ceiling), so the matcher must catch every spelling
// a backend uses: the 507 status AND the MinIO XMinioStorageFull /
// QuotaExceeded error-code family. A genuine not-found / generic error must
// NOT be mistaken for a quota rejection.
func TestIsS3QuotaExceeded(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"507 status", minio.ErrorResponse{StatusCode: http.StatusInsufficientStorage}, true},
		{"XMinioStorageFull code", minio.ErrorResponse{Code: "XMinioStorageFull", StatusCode: 507}, true},
		{"QuotaExceeded code", minio.ErrorResponse{Code: "QuotaExceeded", StatusCode: 400}, true},
		{"admin bucket quota code", minio.ErrorResponse{Code: "XMinioAdminBucketQuotaExceeded", StatusCode: 400}, true},
		{"wrapped 507", fmt.Errorf("s3 put abc: %w", minio.ErrorResponse{StatusCode: http.StatusInsufficientStorage}), true},
		{"not found is not quota", minio.ErrorResponse{Code: "NoSuchKey", StatusCode: 404}, false},
		{"generic error is not quota", errors.New("connection reset"), false},
		{"access denied is not quota", minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isS3QuotaExceeded(tc.err); got != tc.want {
				t.Fatalf("isS3QuotaExceeded(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
