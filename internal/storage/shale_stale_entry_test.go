package storage_test

// DropStaleOwnerEntry (docs/SPEC.md "Delete heals its own lost tail"): a
// delete whose {slug} transaction committed but whose {id}-shard index drop
// never ran leaves the owner's doc entry + enumeration row with no paste row.
// The heal drops BOTH representations in one CAS, fires only when the row is
// absent, and never resurrects through a doc rebuild.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// strandDeleteTail reproduces the lost delete tail: the {slug} shard is
// cleared exactly as Delete's first transaction leaves it (head, version rows,
// slug binding, versions doc), while the {id}-shard drop never runs.
func strandDeleteTail(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug) {
	t.Helper()
	for _, key := range [][]byte{
		storage.LegacyPasteKeyForTest(slug),
		storage.LegacyVersionKeyForTest(slug, 1),
		storage.LegacySlugOwnerKeyForTest(slug),
		storage.VersionsDocKeyForTest(slug),
	} {
		if err := repo.DeleteRawForTest(key); err != nil {
			t.Fatalf("strand %s: %v", key, err)
		}
	}
}

func enumerationRowPresent(t *testing.T, repo *storage.ShaleRepo, owner string, slug domain.Slug) bool {
	t.Helper()
	raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(owner, slug.String()))
	if err != nil {
		t.Fatalf("read enumeration row: %v", err)
	}
	return len(raw) > 0
}

func listedSlugs(t *testing.T, repo *storage.ShaleRepo, owner string) map[domain.Slug]bool {
	t.Helper()
	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := map[domain.Slug]bool{}
	for _, p := range list {
		out[p.Slug] = true
	}
	return out
}

func TestDropStaleOwnerEntry_HealsLostDeleteTail(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	owner := "key:stale-heal"

	phantom := pasteOf("stalep01", owner, 48)
	sibling := pasteOf("stales02", owner, 200)
	for _, p := range []domain.Paste{phantom, sibling} {
		if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}
	repo.WaitPendingConfirms()
	strandDeleteTail(t, repo, phantom.Slug)

	// The damage is visible before the heal: list, count and the quota sum
	// all report the phantom. Without this the assertions below could pass
	// against a fixture that never held the damage shape.
	if got := listedSlugs(t, repo, owner); !got[phantom.Slug] {
		t.Fatalf("fixture: phantom not listed pre-heal; damage shape not planted")
	}
	if n := mustCount(t, repo, owner); n != 2 {
		t.Fatalf("fixture: count pre-heal = %d, want 2", n)
	}
	if got := mustSum(t, repo, owner, now); got != 248 {
		t.Fatalf("fixture: sum pre-heal = %d, want 248", got)
	}

	healed, err := repo.DropStaleOwnerEntry(phantom.Slug, owner)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if !healed {
		t.Fatal("heal reported false on the damage shape")
	}

	got := listedSlugs(t, repo, owner)
	if got[phantom.Slug] {
		t.Fatal("phantom still listed after heal (doc entry survived)")
	}
	if !got[sibling.Slug] {
		t.Fatal("sibling vanished: the heal must drop exactly one entry")
	}
	if enumerationRowPresent(t, repo, owner, phantom.Slug) {
		t.Fatal("enumeration row survived the heal (only the doc entry was dropped)")
	}
	if n := mustCount(t, repo, owner); n != 1 {
		t.Fatalf("count after heal = %d, want 1", n)
	}
	if got := mustSum(t, repo, owner, now); got != 200 {
		t.Fatalf("sum after heal = %d, want 200 (phantom bytes still charged)", got)
	}

	// Idempotent: the residue is gone, so a second heal has nothing to drop.
	healed, err = repo.DropStaleOwnerEntry(phantom.Slug, owner)
	if err != nil {
		t.Fatalf("second heal: %v", err)
	}
	if healed {
		t.Fatal("second heal reported true; nothing was left to drop")
	}
}

// A slug whose paste row EXISTS is never healed, whoever owns it: the caller's
// stale entry stays, the live paste stays, and the report is false so the verb
// answers plain not-found (existence collapse).
func TestDropStaleOwnerEntry_LiveRowDropsNothing(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	victim := "key:stale-victim"
	reminter := "key:stale-reminter"

	p := pasteOf("stalerm1", victim, 48)
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	strandDeleteTail(t, repo, p.Slug)

	// The freed slug is re-minted by ANOTHER identity. The victim's index
	// still carries the old stamp; the row now belongs to someone else.
	remint := pasteOf(p.Slug.String(), reminter, 100)
	remint.CreatedAt = now.Add(time.Hour)
	remint.UpdatedAt = remint.CreatedAt
	if err := repo.InsertWithQuotaCheck(context.Background(), remint, 0, now.Add(time.Hour)); err != nil {
		t.Fatalf("re-mint insert: %v", err)
	}
	repo.WaitPendingConfirms()

	healed, err := repo.DropStaleOwnerEntry(p.Slug, victim)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if healed {
		t.Fatal("heal fired on a slug whose row exists: existence leaks and the verb would claim a foreign paste was deleted")
	}
	if _, err := repo.Get(p.Slug); err != nil {
		t.Fatalf("re-minted paste must be untouched: %v", err)
	}
	// The victim's stale entry is deliberately left: it cannot be confirmed
	// safe to drop while a live row holds the slug.
	if got := listedSlugs(t, repo, victim); !got[p.Slug] {
		t.Fatal("victim's index entry was dropped despite a live row")
	}
}

// A SAME-owner re-mint replaces the doc entry with a fresh stamp and a live
// row, so the heal branch never fires and the re-minted paste keeps serving.
func TestDropStaleOwnerEntry_SameOwnerRemintNeverHealed(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	owner := "key:stale-self"

	p := pasteOf("staleslf", owner, 48)
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	strandDeleteTail(t, repo, p.Slug)

	remint := pasteOf(p.Slug.String(), owner, 100)
	remint.CreatedAt = now.Add(time.Hour)
	remint.UpdatedAt = remint.CreatedAt
	if err := repo.InsertWithQuotaCheck(context.Background(), remint, 0, now.Add(time.Hour)); err != nil {
		t.Fatalf("re-mint insert: %v", err)
	}
	repo.WaitPendingConfirms()

	healed, err := repo.DropStaleOwnerEntry(p.Slug, owner)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if healed {
		t.Fatal("heal fired on a re-minted slug: the row exists, so there is nothing stale")
	}
	if _, err := repo.Get(p.Slug); err != nil {
		t.Fatalf("re-minted paste must survive: %v", err)
	}
	if got := listedSlugs(t, repo, owner); !got[p.Slug] {
		t.Fatal("re-minted entry missing from the owner's list")
	}
}

// A pre-doc owner's residue is the enumeration row alone; the stamp comes from
// that row and the drop still lands.
func TestDropStaleOwnerEntry_DoclessOwnerFallsBackToRow(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	owner := "key:stale-nodoc"

	phantom := pasteOf("stalend1", owner, 48)
	sibling := pasteOf("stalend2", owner, 200)
	for _, p := range []domain.Paste{phantom, sibling} {
		if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}
	repo.WaitPendingConfirms()
	strandDeleteTail(t, repo, phantom.Slug)
	deleteOwnerDoc(t, repo, owner) // pre-migration shape: legacy rows only

	healed, err := repo.DropStaleOwnerEntry(phantom.Slug, owner)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if !healed {
		t.Fatal("heal reported false for a docless owner's stale row")
	}
	if enumerationRowPresent(t, repo, owner, phantom.Slug) {
		t.Fatal("enumeration row survived the docless heal")
	}
	got := listedSlugs(t, repo, owner)
	if got[phantom.Slug] {
		t.Fatal("phantom still listed after the docless heal")
	}
	if !got[sibling.Slug] {
		t.Fatal("sibling vanished during the docless heal")
	}
}

// The candidate-rebuild trap: ownerDocCandidate copies a RENDERABLE
// enumeration row into a rebuilt doc without reading its paste, so a heal that
// dropped only the doc entry would see the phantom return on the next rebuild.
// This forces that rebuild after a heal and asserts the phantom stays gone.
func TestDropStaleOwnerEntry_RebuiltDocDoesNotResurrect(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	owner := "key:stale-rebuild"

	phantom := pasteOf("stalerb1", owner, 48)
	sibling := pasteOf("stalerb2", owner, 200)
	for _, p := range []domain.Paste{phantom, sibling} {
		if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}
	repo.WaitPendingConfirms()
	strandDeleteTail(t, repo, phantom.Slug)

	if healed, err := repo.DropStaleOwnerEntry(phantom.Slug, owner); err != nil || !healed {
		t.Fatalf("heal: healed=%v err=%v", healed, err)
	}

	// Lose the doc and force the heal-on-write rebuild from the legacy rows.
	deleteOwnerDoc(t, repo, owner)
	if err := repo.SetName(sibling.Slug, "poke", sibling.Identity, sibling.CreatedAt); err != nil {
		t.Fatalf("rename to trigger rebuild: %v", err)
	}
	if !ownerDocPresent(t, repo, owner) {
		t.Fatal("rebuild did not run: the doc is still absent")
	}

	got := listedSlugs(t, repo, owner)
	if got[phantom.Slug] {
		t.Fatal("phantom resurrected by the doc rebuild: the enumeration row must have survived the heal")
	}
	if !got[sibling.Slug] {
		t.Fatal("sibling missing after rebuild")
	}
	if n := mustCount(t, repo, owner); n != 1 {
		t.Fatalf("count after rebuild = %d, want 1", n)
	}
}

// The age guard: a young index entry is exactly what a mid-insert looks like
// (entry written, row not yet committed), and no reordering can defend state
// whose row does not exist - so the heal must refuse it and leave it for a
// later retry.
func TestDropStaleOwnerEntry_YoungEntryRefused(t *testing.T) {
	repo := newShaleRepoForTest(t)
	owner := "key:young-heal"
	phantom := pasteOf("youngp01", owner, 48)
	if err := repo.InsertWithQuotaCheck(context.Background(), phantom, 0, fixedNow); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	strandDeleteTail(t, repo, phantom.Slug)

	// Clock reads the entry as one minute old: refused, damage intact.
	repo.SetNowForTest(func() time.Time { return phantom.CreatedAt.Add(time.Minute) })
	healed, err := repo.DropStaleOwnerEntry(phantom.Slug, owner)
	if err != nil {
		t.Fatalf("heal (young): %v", err)
	}
	if healed {
		t.Fatal("a young entry was healed; the age guard is not in effect")
	}
	if got := listedSlugs(t, repo, owner); !got[phantom.Slug] {
		t.Fatal("young entry vanished despite the refusal")
	}

	// Two hours old: the same entry heals.
	repo.SetNowForTest(func() time.Time { return phantom.CreatedAt.Add(2 * time.Hour) })
	healed, err = repo.DropStaleOwnerEntry(phantom.Slug, owner)
	if err != nil || !healed {
		t.Fatalf("heal (aged) = (%v, %v), want (true, nil)", healed, err)
	}
	if got := listedSlugs(t, repo, owner); got[phantom.Slug] {
		t.Fatal("aged entry still listed after the heal")
	}
}

// The enumeration-row stamp guard inside the drop CAS: a row whose CreatedAt
// differs from the doc entry's must survive the heal (it is not the entry
// generation the stamp vouches for). Pins entryStampMatches, which a mutation
// pass showed was otherwise unpinned.
func TestDropStaleOwnerEntry_MismatchedRowStampSurvives(t *testing.T) {
	repo := newShaleRepoForTest(t)
	owner := "key:rowstamp"
	phantom := pasteOf("rowstp01", owner, 48)
	if err := repo.InsertWithQuotaCheck(context.Background(), phantom, 0, fixedNow); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	strandDeleteTail(t, repo, phantom.Slug)

	// Rewrite the enumeration row with a DIFFERENT stamp than the doc entry.
	mut := phantom
	mut.CreatedAt = phantom.CreatedAt.Add(30 * time.Minute)
	raw, err := json.Marshal(storage.IdentityPasteRowForTest(mut))
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	if err := repo.PutRawForTest(storage.IdentityPasteKeyForTest(owner, phantom.Slug.String()), raw); err != nil {
		t.Fatalf("plant mismatched row: %v", err)
	}

	repo.SetNowForTest(func() time.Time { return phantom.CreatedAt.Add(2 * time.Hour) })
	if _, err := repo.DropStaleOwnerEntry(phantom.Slug, owner); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(owner, phantom.Slug.String())); err != nil || len(raw) == 0 {
		t.Fatalf("mismatched-stamp enumeration row did not survive the guarded drop (raw=%d, err=%v)", len(raw), err)
	}
}
