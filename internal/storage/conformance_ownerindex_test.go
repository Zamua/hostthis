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
	SumActiveBytesByOwner(owner string, now time.Time) (int, error)
	Delete(slug domain.Slug, wantIdentity domain.Identity, wantCreatedAt time.Time) error
}

func chargedBytes(t *testing.T, r ownerIndexRepo, owner string) (int, error) {
	t.Helper()
	return r.SumActiveBytesByOwner(owner, fixedNow)
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

// A REPEATED DELETE does not double-release.
//
// NOTE what this does and does not reach. At the port, the only observable is
// calling the delete twice, and both backends are protected there by a status
// guard: the second call sees an already-settled record and does no release at
// all. So this pins the GUARD, which is real protection worth having pinned.
//
// It does NOT pin release itself being idempotent. A resolver replaying after a
// crash calls release directly, below the guard, and no port-level assertion
// can see that. Verified the hard way: an arithmetic-release sabotage in the
// celld adapter PASSED this test, because the guard stopped the second call
// before it reached the sabotaged code. The internal invariant is pinned in
// internal/celld instead, where the release can be driven directly.
//
// Creation reserves and confirms; deletion RELEASES, and release carries an
// idempotency requirement reservation does not. A resolver gives at-least-once,
// so a crash between releasing and discharging its intent means the release
// runs again. Expressed as arithmetic - "subtract N from the charged total" -
// that second run under-charges the owner permanently, and nothing surfaces it:
// the number is simply wrong forever.
//
// Expressed as MEMBERSHIP - "ensure this slug is no longer counted" - re-running
// is a no-op by construction, and the mechanism only has to promise
// at-least-once. This asserts the observable consequence rather than the
// implementation: releasing twice must leave the same total as releasing once.
func conformReleaseIsIdempotent(t *testing.T, r ownerIndexRepo) {
	const owner = "key:oi-replay"
	p := pasteOf("oia23456", owner, 700)
	p.Status = domain.PasteStatusPending
	ownerInsert(t, r, p)

	// A second paste stays live, so the assertion distinguishes "released once"
	// from "released into the negative": with arithmetic double-release the
	// total would fall BELOW the survivor's size.
	keep := pasteOf("oib23456", owner, 300)
	ownerInsert(t, r, keep)

	if err := r.MarkFailed(p.Slug); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	after, err := chargedBytes(t, r, owner)
	if err != nil {
		t.Fatalf("charged after first release: %v", err)
	}
	if after != 300 {
		t.Fatalf("charged = %d after releasing the 700; want 300, the survivor", after)
	}

	// The replay. A resolver that ran twice must not charge differently.
	if err := r.MarkFailed(p.Slug); err != nil {
		t.Fatalf("MarkFailed replay: %v", err)
	}
	replayed, err := chargedBytes(t, r, owner)
	if err != nil {
		t.Fatalf("charged after replay: %v", err)
	}
	if replayed != after {
		t.Fatalf("charged = %d after a REPLAYED release, was %d. Release must be membership "+
			"(\"this slug is no longer counted\"), not arithmetic (\"subtract N\"), because a "+
			"resolver only promises at-least-once.", replayed, after)
	}
}

// Deleting stops the charge and removes the listing entry, and both must hold
// together: a paste absent from the listing while still charging is quota an
// owner cannot see or free, and one charging nothing while still listed offers
// them something that no longer exists.
//
// Also pins the ownership guard, because delete is destructive: a slug deleted
// and re-minted by someone else must not be removable by a request holding the
// old owner's details.
func conformDeleteReleasesAndDelists(t *testing.T, r ownerIndexRepo) {
	const owner = "key:oi-delete"
	doomed := pasteOf("oic23456", owner, 700)
	keep := pasteOf("oid23456", owner, 300)
	ownerInsert(t, r, doomed)
	ownerInsert(t, r, keep)

	// A foreign owner cannot delete it.
	if err := r.Delete(doomed.Slug, domain.Identity("key:someone-else"), doomed.CreatedAt); err == nil {
		t.Fatal("a foreign identity deleted a paste it does not own")
	}
	if n, err := chargedBytes(t, r, owner); err != nil || n != 1000 {
		t.Fatalf("charged = %d after a refused delete (err %v); want 1000 unchanged", n, err)
	}

	if err := r.Delete(doomed.Slug, domain.Identity(owner), doomed.CreatedAt); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if d, ok := r.(pendingConfirmsDrainer); ok {
		d.WaitPendingConfirms()
	}

	n, err := chargedBytes(t, r, owner)
	if err != nil {
		t.Fatalf("charged after delete: %v", err)
	}
	if n != 300 {
		t.Fatalf("charged = %d after deleting the 700; want 300, the survivor", n)
	}
	got, err := r.ListByOwner(owner)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	for _, p := range got {
		if p.Slug == doomed.Slug {
			t.Fatalf("%s still listed after deletion; the listing and the charge must agree", p.Slug)
		}
	}
	if len(got) != 1 {
		t.Fatalf("listing = %d pastes; want 1, the survivor", len(got))
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
	t.Run(name+"/ReleaseIsIdempotent", func(t *testing.T) { conformReleaseIsIdempotent(t, newRepo(t)) })
	t.Run(name+"/DeleteReleasesAndDelists", func(t *testing.T) { conformDeleteReleasesAndDelists(t, newRepo(t)) })
}
