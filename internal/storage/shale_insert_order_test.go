//go:build slatedb

package storage_test

// The insert writes the enumeration entry BEFORE the authoritative row, so the
// only crash-reachable inconsistency is an entry with no row - which a
// single-shard read (ListByOwner) can see and repair. The opposite order leaves
// a row no single-shard read can find, which is what used to require a
// cluster-wide reconcile pass.
//
//	go test -tags slatedb -run TestShaleInsertOrder ./internal/storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// A slug already taken is rejected BEFORE any entry is written. Without the
// pre-check every attempt of the upload's re-mint loop would strand an entry
// that counts against the owner's quota until they next list.
func TestShaleInsertOrder_TakenSlugWritesNoEntry(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping shale insert-order test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	const first = "key:order-a"
	const second = "key:order-b"
	slug := domain.Slug("ordr1234")

	held := domain.Paste{
		Slug: slug, Identity: domain.Identity(first), Kind: domain.KindHTML,
		ContentSHA: "sha-order-1", Size: 100, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), held, 0, now); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	// A DIFFERENT owner colliding on that slug must be refused...
	clash := domain.Paste{
		Slug: slug, Identity: domain.Identity(second), Kind: domain.KindHTML,
		ContentSHA: "sha-order-2", Size: 400, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), clash, 0, now); !errors.Is(err, storage.ErrSlugTaken) {
		t.Fatalf("colliding insert: got %v, want ErrSlugTaken", err)
	}

	// ...without leaving anything charged to them.
	if got := mustSum(t, repo, second, now); got != 0 {
		t.Fatalf("a refused insert must charge the would-be owner nothing; got %d bytes. "+
			"An entry written before the slug pre-check leaks one per re-mint attempt.", got)
	}
	if raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(second, slug.String())); err != nil {
		t.Fatalf("read would-be entry: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("a refused insert must write NO enumeration entry; found %q", raw)
	}

	// And the holder is untouched.
	if got := mustSum(t, repo, first, now); got != 100 {
		t.Fatalf("holder's bytes after a failed collision: got %d, want 100", got)
	}
}
