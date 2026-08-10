// Internal-access regressions for the owner-doc drop guards and the guarded
// index writes, which are unexported and cannot be reached from storage_test.

//go:build !slatedb

package storage

import (
	"context"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/shale/pkg/cluster"
)

func newInternalPebbleRepo(t *testing.T) *ShaleRepo {
	t.Helper()
	repo, err := NewShaleRepo(ShaleConfig{
		NodeID:            "internal-" + t.Name(),
		DbName:            "internal-" + t.Name(),
		ReplicationFactor: 1,
	})
	if err != nil {
		t.Fatalf("NewShaleRepo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func internalTruncatedEnvelope() []byte {
	full := cluster.Encode(cluster.Envelope{
		Stamp:   cluster.Stamp{TimestampNanos: 1, NodeID: "n"},
		Payload: []byte("x"),
	})
	return full[:5] // magic + 4 header bytes; short of the 11-byte minimum
}

// dropOwnerEntry must not clobber a SAME-OWNER re-mint's fresh entry. The window
// is between the {slug} delete (which frees the slug) and this {id} drop; a
// re-mint writes a fresh entry+doc with a new CreatedAt, and the drop keyed off
// the OLD paste's immutable stamp must leave them alone.
func TestDropOwnerEntry_GuardsAgainstSameOwnerReMint(t *testing.T) {
	repo := newInternalPebbleRepo(t)
	ctx := context.Background()
	owner := "key:dropguard"
	slug := domain.Slug("dropgrd1")
	t1 := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	// The re-minted paste currently holds the slug: its entry carries stamp t2.
	p2 := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-p2", Size: 200, CreatedAt: t2, UpdatedAt: t2,
	}
	if err := repo.InsertWithQuotaCheck(ctx, p2, 0, t2); err != nil {
		t.Fatalf("insert re-mint: %v", err)
	}
	repo.WaitPendingConfirms()

	// The OLD paste's tail drop arrives late with the OLD stamp t1: it must NOT
	// remove the re-mint's entry or doc entry.
	if err := repo.dropOwnerEntry(owner, slug.String(), t1); err != nil {
		t.Fatalf("dropOwnerEntry(old stamp): %v", err)
	}
	if list, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(list) != 1 || list[0].Slug != slug {
		t.Fatalf("re-mint entry was clobbered by a stale drop: %+v", list)
	}

	// A matching-stamp drop DOES remove it, so the real removal still works.
	if err := repo.dropOwnerEntry(owner, slug.String(), t2); err != nil {
		t.Fatalf("dropOwnerEntry(matching stamp): %v", err)
	}
	if list, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list after matching drop: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("matching-stamp drop must remove the entry: %+v", list)
	}
}

// The guarded index writes must fail OPEN on a truncated-envelope current entry:
// a value that cannot be read cannot be shown to still hold `expected`, so both
// skip (the documented mismatch outcome) rather than propagating a decode error
// into a repair/rollback path.
func TestGuardedIndexWrite_ToleratesTruncatedEnvelope(t *testing.T) {
	repo := newInternalPebbleRepo(t)
	key := shaleKeyIdentityPaste("key:gied", "gied0001")
	if err := repo.cluster.Put(key, internalTruncatedEnvelope()); err != nil {
		t.Fatalf("plant damaged entry: %v", err)
	}
	row := identityPasteRow{Name: "x", Size: 1, ServedSize: 1, Kind: "html", CreatedAt: time.Now().UTC()}

	written, err := repo.guardedPutIndexEntry(key, []byte("stale-expected"), true, row, nil)
	if err != nil {
		t.Fatalf("guardedPutIndexEntry must fail open on envelope damage, got: %v", err)
	}
	if written {
		t.Fatalf("guardedPutIndexEntry must skip a damaged current entry, not overwrite it")
	}

	deleted, err := repo.guardedDeleteIndexEntry(key, []byte("stale-expected"), nil)
	if err != nil {
		t.Fatalf("guardedDeleteIndexEntry must fail open, got: %v", err)
	}
	if deleted {
		t.Fatalf("guardedDeleteIndexEntry must skip a damaged current entry, not delete it")
	}

	// The damaged entry is left intact for a later reprojection to overwrite.
	if raw, err := repo.cluster.Get(key); err != nil || len(raw) == 0 {
		t.Fatalf("damaged entry must be left in place: raw=%q err=%v", raw, err)
	}
}

// The insert-rollback's value-guarded drop must NOT delete an entry whose bytes
// differ from the loser's captured guard: this is the in-tx backstop that keeps
// two same-identity concurrent inserts of one slug from letting the loser delete
// the winner's live entry (Pattern-A residual left on the value guard).
func TestGuardedDropOwnerEntry_ValueGuardProtectsFresherEntry(t *testing.T) {
	repo := newInternalPebbleRepo(t)
	ctx := context.Background()
	owner := "key:vg"
	slug := domain.Slug("valguard1")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-vg", Size: 10, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	dropped, err := repo.guardedDropOwnerEntry(owner, slug.String(), []byte("stale-loser-guard"))
	if err != nil {
		t.Fatalf("guardedDropOwnerEntry: %v", err)
	}
	if dropped {
		t.Fatalf("value guard must refuse to drop an entry whose bytes differ from the guard")
	}
	if list, err := repo.ListByOwner(owner); err != nil || len(list) != 1 {
		t.Fatalf("winner entry must survive a mismatched-guard drop: %+v err=%v", list, err)
	}
}
