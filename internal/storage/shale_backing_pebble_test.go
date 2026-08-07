// Pins that the default build's storage engine actually serves ShaleRepo.
//
// The point is that this file carries NO build tag and needs no object store:
// if it runs at all, a plain `go test ./...` opened a real shale cluster and
// round-tripped through it.

//go:build !slatedb

package storage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func newPebbleShaleRepo(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            "pebble-node",
		DbName:            fmt.Sprintf("pebble-%s", t.Name()),
		ReplicationFactor: 1,
	})
	if err != nil {
		t.Fatalf("NewShaleRepo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestPebbleBacking_RoundTrip(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)

	want := domain.Paste{
		Slug:       domain.Slug("pebble01"),
		Identity:   domain.Identity("owner-a"),
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindMarkdown,
		ContentSHA: "sha-pebble01",
		Size:       42,
		CreatedAt:  now, UpdatedAt: now,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), want, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.Get(want.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Identity != want.Identity || got.Size != want.Size || got.ContentSHA != want.ContentSHA {
		t.Fatalf("round trip: got %+v, want identity/size/sha %q/%d/%q",
			got, want.Identity, want.Size, want.ContentSHA)
	}

	// The enumeration entry is written on a DIFFERENT key family than the row,
	// so listing proves the engine is routing whole key families, not just
	// serving one lucky key.
	listed, err := repo.ListByOwner(want.Identity.String())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Slug != want.Slug {
		t.Fatalf("list by owner: got %+v, want exactly %s", listed, want.Slug)
	}

	sum, err := repo.SumActiveBytesByOwner(want.Identity.String(), now)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != want.Size {
		t.Fatalf("charged bytes: got %d, want %d", sum, want.Size)
	}
}

// A local engine has no shared store to fence unit mounts against, so asking
// for sharding must fail loudly rather than quietly returning one unit.
func TestPebbleBacking_RefusesMultiBackend(t *testing.T) {
	_, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            "pebble-node",
		DbName:            "pebble-units",
		ReplicationFactor: 1,
		UnitCount:         4,
	})
	if err == nil {
		t.Fatal("UnitCount=4 opened on a local engine; want an error")
	}
}
