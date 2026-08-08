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
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// A slug already taken is rejected BEFORE any entry is written. Without the
// pre-check every attempt of the upload's re-mint loop would strand an entry
// that counts against the owner's quota until they next list.
func TestShaleInsertOrder_TakenSlugWritesNoEntry(t *testing.T) {
	repo := newShaleRepoForTest(t)
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

// A placeholder must be CLEARED once the authoritative row decodes again.
// Nothing writes placeholders now, so one left behind by an older build would
// otherwise be permanent - and a placeholder counts as zero bytes, so its owner
// keeps that paste for free until something replaces the marker with the real
// size.
func TestShaleInsertOrder_ListClearsStalePlaceholder(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	const owner = "key:placeholder"

	p := domain.Paste{
		Slug: "phld1234", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-phld", Size: 250, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Plant the marker an older build's reconciler would have written.
	idxKey := storage.IdentityPasteKeyForTest(owner, p.Slug.String())
	if err := repo.PutRawForTest(idxKey, []byte(`{"placeholder":true}`)); err != nil {
		t.Fatalf("plant placeholder: %v", err)
	}
	// Fixture: the marker must actually suppress the charge, or clearing it
	// below proves nothing.
	if got, err := repo.SumActiveBytesByOwner(owner, now); err != nil {
		t.Fatalf("a placeholder must not fail the scan: %v", err)
	} else if got != 0 {
		t.Fatalf("fixture: a placeholder must count as zero, got %d", got)
	}

	if _, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list: %v", err)
	}

	// The row decodes fine, so the marker was stale and must be gone.
	got, err := repo.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("quota scan after clearing: %v", err)
	}
	if got != 250 {
		t.Fatalf("cleared placeholder must be replaced by the real size: got %d, want 250", got)
	}
}
