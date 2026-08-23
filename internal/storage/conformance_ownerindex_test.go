package storage_test

// Backend-agnostic conformance for the OWNER INDEX: the listing, count and
// first-seen an owner sees, plus the repair that removes an entry whose paste
// is gone.
//
// Stated against a narrow interface for the same reason the lifecycle suite is:
// a partial adapter can be gated by it while it is being built.
//
// The property that matters across backends is not how the index is stored but
// that it AGREES with the pastes: everything inserted appears, nothing failed
// appears, and an entry whose paste has vanished can be repaired away. shale
// maintains a derived document; celld keeps a denormalised summary in the
// identity cell. Both must produce the same answers.

import (
	"context"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

type ownerIndexRepo interface {
	InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error
	Get(domain.Slug) (domain.Paste, error)
	MarkFailed(domain.Slug) error
	ListByOwner(owner string) ([]domain.Paste, error)
	CountByOwner(owner string) (int, error)
	OwnerFirstSeen(owner string) (time.Time, error)
	DropStaleOwnerEntry(slug domain.Slug, owner string) (bool, error)
	SetName(slug domain.Slug, name string, wantIdentity domain.Identity, wantCreatedAt time.Time) error
}

func ownerInsert(t *testing.T, r ownerIndexRepo, p domain.Paste) {
	t.Helper()
	if err := r.InsertWithQuotaCheck(context.Background(), p, 0, fixedNow); err != nil {
		t.Fatalf("insert %q: %v", p.Slug, err)
	}
	if d, ok := r.(pendingConfirmsDrainer); ok {
		d.WaitPendingConfirms()
	}
}

// Everything inserted for an owner appears in that owner's listing, and nobody
// else's does.
func conformOwnerListIsScopedAndComplete(t *testing.T, r ownerIndexRepo) {
	const mine, theirs = "key:oi-mine", "key:oi-theirs"
	ownerInsert(t, r, pasteOf("oi123456", mine, 10))
	ownerInsert(t, r, pasteOf("oi223456", mine, 20))
	ownerInsert(t, r, pasteOf("oi323456", theirs, 30))

	got, err := r.ListByOwner(mine)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listing has %d pastes; want 2", len(got))
	}
	for _, p := range got {
		if p.Identity.String() != mine {
			t.Fatalf("listing contains %s owned by %q; want only %q", p.Slug, p.Identity, mine)
		}
	}
	n, err := r.CountByOwner(mine)
	if err != nil {
		t.Fatalf("CountByOwner: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountByOwner = %d; want 2, agreeing with the listing", n)
	}
}

// A FAILED paste is not offered to its owner: it charges nothing and cannot be
// served, so listing it would show something they cannot act on.
func conformOwnerListExcludesFailed(t *testing.T, r ownerIndexRepo) {
	const owner = "key:oi-failed"
	p := pasteOf("oi423456", owner, 10)
	p.Status = domain.PasteStatusPending
	ownerInsert(t, r, p)
	if err := r.MarkFailed(p.Slug); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, err := r.ListByOwner(owner)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	for _, x := range got {
		if x.Slug == p.Slug {
			t.Fatalf("failed paste %s still listed for its owner", p.Slug)
		}
	}
}

// First-seen is a property of the IDENTITY, not of any paste, so it survives
// the pastes and does not move when a later one is added.
func conformOwnerFirstSeenIsStable(t *testing.T, r ownerIndexRepo) {
	const owner = "key:oi-firstseen"
	ownerInsert(t, r, pasteOf("oi523456", owner, 10))
	first, err := r.OwnerFirstSeen(owner)
	if err != nil {
		t.Fatalf("OwnerFirstSeen: %v", err)
	}
	if first.IsZero() {
		t.Fatal("OwnerFirstSeen is zero after an insert; want the first reservation's time")
	}
	ownerInsert(t, r, pasteOf("oi623456", owner, 10))
	again, err := r.OwnerFirstSeen(owner)
	if err != nil {
		t.Fatalf("OwnerFirstSeen after a second insert: %v", err)
	}
	if !again.Equal(first) {
		t.Fatalf("OwnerFirstSeen moved from %v to %v; it must not track the latest paste", first, again)
	}
}

// The repair, and the guard that makes it safe: an entry is droppable only when
// its paste is actually gone. Dropping one whose paste exists would remove a
// live paste from its owner's listing, which is worse than the residue.
func conformDropStaleEntryOnlyWhenAbsent(t *testing.T, r ownerIndexRepo) {
	const owner = "key:oi-stale"
	p := pasteOf("oi723456", owner, 10)
	ownerInsert(t, r, p)

	dropped, err := r.DropStaleOwnerEntry(p.Slug, owner)
	if err != nil {
		t.Fatalf("DropStaleOwnerEntry on a live paste: %v", err)
	}
	if dropped {
		t.Fatal("dropped the index entry of a paste that still exists")
	}
	if got, err := r.ListByOwner(owner); err != nil || len(got) != 1 {
		t.Fatalf("listing = %d pastes (err %v); want the live paste still there", len(got), err)
	}

	// An owner that never existed has nothing to repair, and asking is not an
	// error: the caller cannot tell "already repaired" from "never there".
	if dropped, err := r.DropStaleOwnerEntry(domain.Slug("oi823456"), owner); err != nil || dropped {
		t.Fatalf("DropStaleOwnerEntry on an absent paste = (%v, %v); want (false, nil)", dropped, err)
	}
}

// FRESHNESS, which membership does not imply. The first four properties are
// about WHICH pastes a listing contains; none of them notices a listing that
// keeps serving an old name. A backend that denormalises anything mutable into
// its index - which both of these do, for good reasons - can pass all four
// while showing the owner stale contents.
//
// Measured against shale before being asserted: its listing DOES reflect a
// rename, so freshness is part of the contract rather than an artifact, and a
// second backend does not get to be lazier.
func conformOwnerListReflectsMutation(t *testing.T, r ownerIndexRepo) {
	const owner = "key:oi-fresh"
	p := pasteOf("oi923456", owner, 10)
	p.Name = "before"
	ownerInsert(t, r, p)

	if err := r.SetName(p.Slug, "after", domain.Identity(owner), p.CreatedAt); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	if d, ok := r.(pendingConfirmsDrainer); ok {
		d.WaitPendingConfirms()
	}

	got, err := r.ListByOwner(owner)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listing = %d pastes; want 1", len(got))
	}
	if got[0].Name != "after" {
		t.Fatalf("listing shows name %q after a rename to %q; the index is stale. "+
			"A denormalised summary must be written by the mutation, not only by the insert.",
			got[0].Name, "after")
	}
}

func runOwnerIndexConformance(t *testing.T, name string, newRepo func(t *testing.T) ownerIndexRepo) {
	t.Helper()
	t.Run(name+"/OwnerListIsScopedAndComplete", func(t *testing.T) {
		conformOwnerListIsScopedAndComplete(t, newRepo(t))
	})
	t.Run(name+"/OwnerListExcludesFailed", func(t *testing.T) { conformOwnerListExcludesFailed(t, newRepo(t)) })
	t.Run(name+"/OwnerFirstSeenIsStable", func(t *testing.T) { conformOwnerFirstSeenIsStable(t, newRepo(t)) })
	t.Run(name+"/DropStaleEntryOnlyWhenAbsent", func(t *testing.T) {
		conformDropStaleEntryOnlyWhenAbsent(t, newRepo(t))
	})
	t.Run(name+"/OwnerListReflectsMutation", func(t *testing.T) {
		conformOwnerListReflectsMutation(t, newRepo(t))
	})
}
