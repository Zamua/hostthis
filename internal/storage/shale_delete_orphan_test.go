//go:build slatedb

package storage_test

// Pins two shale {slug}-shard rules the audit found broken together:
//
//   - a whole-paste Delete leaves NO version row behind. The version set is
//     enumerated outside the CAS transaction, so without a read-check joining
//     the head row and the next version number to the read set the tx commits
//     unconditionally and a racing append's row survives its parent - an orphan
//     no scan reaches, whose ContentSHA pins its blob out of the GC's reach.
//   - Unpin rolls the head to the latest LIVE version, never a tombstone.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestShaleDelete_RacingAppendLeavesNoOrphanVersion(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; start dev MinIO first")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Now().UTC()

	// The window is delete's scan-to-commit gap. Racing both calls from a
	// standing start lands in it only rarely (and a coarse offset misses it
	// entirely in the other direction, letting the append finish first), so the
	// delete's start is swept in small steps across the append's critical
	// section rather than left to scheduler luck.
	const (
		iters     = 40
		stepDelay = 20 * time.Microsecond
	)
	orphans, contended := 0, 0
	for i := range iters {
		slug := domain.Slug(fmt.Sprintf("orph%04d", i))
		p := pasteOf(slug.String(), fmt.Sprintf("key:orphan-%d", i), 100)
		p.ExpiresAt = now.Add(domain.DefaultRetentionWindow)
		if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
			t.Fatalf("iter %d insert: %v", i, err)
		}
		repo.WaitPendingConfirms()

		var wg sync.WaitGroup
		var appErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * stepDelay)
			_ = repo.Delete(slug)
		}()
		go func() {
			defer wg.Done()
			// Either outcome is legal: the append lands before the delete and
			// must then be cascaded with it, or it finds the paste gone.
			_, appErr = repo.AppendVersionWithQuotaCheck(context.Background(), slug,
				domain.KindHTML, "sha-"+slug.String()+"-v2", 60, 0, now)
		}()
		wg.Wait()

		if _, err := repo.Get(slug); !errors.Is(err, storage.ErrNotFound) {
			continue // the paste survived; its versions legitimately survive too
		}
		if appErr == nil {
			// Both writes committed and the paste is gone: exactly the shape
			// that can strand the appended version row.
			contended++
		}
		versions, err := repo.ListVersions(slug)
		if err != nil {
			t.Fatalf("iter %d list versions: %v", i, err)
		}
		if len(versions) != 0 {
			orphans++
			t.Errorf("iter %d: paste deleted but %d version row(s) survive: %+v", i, len(versions), versions)
		}
	}
	// An iteration where the append never committed, or where the delete lost,
	// cannot strand anything, so a run of only those would pass without
	// exercising the cascade at all. Fail loudly rather than pass vacuously.
	if contended == 0 {
		t.Fatalf("no iteration had both an appended version and a deleted paste in %d tries: the race window was never entered, so this test proved nothing", iters)
	}
	if orphans > 0 {
		t.Fatalf("%d/%d iterations orphaned version rows under a delete/append race", orphans, iters)
	}
	t.Logf("%d/%d iterations resolved with both writes committed; no orphans", contended, iters)
}

func TestShaleUnpin_SkipsTombstonedVersion(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; start dev MinIO first")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Now().UTC()

	slug := domain.Slug("shunpin1")
	p := pasteOf(slug.String(), "key:shunpin", 100)
	p.ContentSHA = "sha-shunpin-v1"
	p.ExpiresAt = now.Add(domain.DefaultRetentionWindow)
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(context.Background(), slug,
		domain.KindHTML, "sha-shunpin-v2", 60, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}

	v1, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	// v2 is no longer served, so the service layer permits tombstoning it.
	if err := repo.DeleteVersion(slug, 2); err != nil {
		t.Fatalf("delete v2: %v", err)
	}
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin: %v", err)
	}

	got, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PinnedVersion != 0 {
		t.Fatalf("PinnedVersion after unpin: got %d, want 0", got.PinnedVersion)
	}
	if got.ContentSHA != "sha-shunpin-v1" {
		t.Fatalf("head rolled onto a tombstoned version: ContentSHA %q, want %q", got.ContentSHA, "sha-shunpin-v1")
	}
}
