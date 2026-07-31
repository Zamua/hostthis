//go:build slatedb

package storage_test

// Pins that the slatedb backend's per-subnet Sybil cap holds under concurrent
// first-sight keys. The budget is a prefix scan, which snapshot isolation
// cannot serialize: concurrent admits write different keygate keys and so
// conflict on nothing, and only the per-subnet stripe stops them all reading
// the same pre-admit count.

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/storage"
)

func TestSlateKeyGate_ConcurrentAdmitsRespectSubnetCap(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; start dev MinIO first")
	}
	repo, err := storage.NewSlateRepo(storage.SlateConfig{
		Endpoint:  endpoint,
		Region:    "us-east-1",
		Bucket:    envOrDefault("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata"),
		AccessKey: envOrDefault("MINIO_TEST_ACCESS_KEY", "admin"),
		SecretKey: envOrDefault("MINIO_TEST_SECRET_KEY", "supersecret"),
		UseSSL:    false,
		DbName:    fmt.Sprintf("kgconc-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("NewSlateRepo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	const (
		limit   = 3
		racers  = 12
		subnet  = "198.51.100.0/24"
		window  = 24 * time.Hour
		timeout = 60 * time.Second
	)
	now := time.Now().UTC()

	var wg sync.WaitGroup
	admitted := make([]bool, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			known, err := repo.AdmitNewKey(fmt.Sprintf("SHA256:kgconc%02d", i), subnet, now, limit, window)
			switch {
			case err == nil && !known:
				admitted[i] = true
			case errors.Is(err, storage.ErrTooManyNewKeys):
				// refused, as intended once the budget is spent
			default:
				errs[i] = err
			}
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("concurrent admits did not finish within %s", timeout)
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: unexpected error: %v", i, err)
		}
	}
	got := 0
	for _, ok := range admitted {
		if ok {
			got++
		}
	}
	if got != limit {
		t.Fatalf("admitted %d fresh keys into a subnet capped at %d", got, limit)
	}
	// The durable row count must agree with what the callers were told.
	count, _, err := repo.SubnetSnapshot(subnet, now, window)
	if err != nil {
		t.Fatalf("SubnetSnapshot: %v", err)
	}
	if count != limit {
		t.Fatalf("durable keygate rows: got %d, want %d", count, limit)
	}
}
