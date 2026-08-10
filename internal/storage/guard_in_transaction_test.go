package storage_test

// Guards that decide a destructive act must be re-taken INSIDE the transaction
// that acts. Each test here opens the window between a guard read and the write
// it protects and commits the operation that makes the guard's answer wrong.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/durable"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// twoVersionPaste builds a paste serving v2 unpinned, with v1 live and
// unserved: the shape every version guard is argued over.
func twoVersionPaste(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, owner string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: string(slug) + "-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, string(slug)+"-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
}

// The service's served-version check runs before the repo's transaction, and a
// pin committing in between makes the head serve the very version being
// tombstoned. The transaction re-takes the check from the head and the record
// it already holds, so the answer cannot be raced.
func TestDeleteVersion_APinLandingAfterTheServiceGuardIsStillRefused(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vrace001")
	owner := "key:vrace"
	twoVersionPaste(t, repo, slug, owner, now)

	var once sync.Once
	repo.SetVersionsDocPlannedHookForTest(func(s domain.Slug) {
		once.Do(func() {
			v1, err := repo.GetVersion(s, 1)
			if err != nil {
				t.Errorf("read v1 for the racing pin: %v", err)
				return
			}
			if err := repo.SetPinnedVersion(s, v1); err != nil {
				t.Errorf("racing pin: %v", err)
			}
		})
	})
	manage := &service.Manage{Repo: repo, Now: func() time.Time { return now }}
	_, err := manage.DeleteVersion(slug, owner, 1)
	repo.SetVersionsDocPlannedHookForTest(nil)

	if !errors.Is(err, service.ErrVersionCurrentlyServed) {
		t.Fatalf("a pin landing in the window must still refuse the tombstone, got %v", err)
	}
	v1, err := repo.GetVersion(slug, 1)
	if err != nil || v1.Deleted {
		t.Fatalf("the version the head now serves must survive: %+v err=%v", v1, err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.PinnedVersion != 1 || head.ContentSHA != v1.ContentSHA {
		t.Fatalf("the head must still serve the pinned v1: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
}

// Pinning a tombstone needs no race at all: the version's blob is unbound the
// moment it is deleted, so the roll would publish bytes already queued for
// reclamation.
func TestPin_RefusesATombstonedVersion(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vpin0001")
	owner := "key:vpin"
	twoVersionPaste(t, repo, slug, owner, now)

	manage := &service.Manage{Repo: repo, Now: func() time.Time { return now }}
	if _, err := manage.DeleteVersion(slug, owner, 1); err != nil {
		t.Fatalf("delete the unserved v1: %v", err)
	}
	if _, err := manage.Pin(slug, owner, 1); !errors.Is(err, service.ErrVersionDeleted) {
		t.Fatalf("pinning a tombstone must be refused, got %v", err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.PinnedVersion != 0 || head.ContentSHA != string(slug)+"-v2" {
		t.Fatalf("a refused pin must apply nothing: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
}

// The same refusal from inside the transaction, for a caller holding a record
// read before the delete landed - which is what the service holds.
func TestSetPinnedVersion_RefusesATombstoneThatLandedAfterTheRead(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vpin0002")
	twoVersionPaste(t, repo, slug, "key:vpin2", now)

	stale, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if stale.Deleted {
		t.Fatal("fixture: v1 must read live before the delete")
	}
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, stale); !errors.Is(err, storage.ErrVersionDeleted) {
		t.Fatalf("a stale live record must not pin a tombstone, got %v", err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.PinnedVersion != 0 {
		t.Fatalf("a refused pin must apply nothing, got pin=%d", head.PinnedVersion)
	}
}

// A whole-paste delete returns the slug to the mint, so the owner can re-mint
// it before the second step removes their enumeration entry. The created time
// the delete carries is what tells the two pastes apart; without it the fresh
// paste goes missing from its owner's listing and stops being charged.
func TestDelete_DoesNotEatAReMintOfTheSameSlug(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	slug := domain.Slug("remint01")
	owner := "key:remint"
	ctx := context.Background()

	first := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-remint-a", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, first, 0, now); err != nil {
		t.Fatalf("insert the first paste: %v", err)
	}
	repo.WaitPendingConfirms()

	var once sync.Once
	repo.SetOwnerEntryDroppingHookForTest(func(_, _ string) {
		once.Do(func() {
			second := domain.Paste{
				Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
				ContentSHA: "sha-remint-b", Size: 700, CreatedAt: later, UpdatedAt: later}
			if err := repo.InsertWithQuotaCheck(ctx, second, 0, later); err != nil {
				t.Errorf("re-mint in the window: %v", err)
			}
			repo.WaitPendingConfirms()
		})
	})
	if err := repo.Delete(slug); err != nil {
		t.Fatalf("delete: %v", err)
	}
	repo.SetOwnerEntryDroppingHookForTest(nil)

	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Slug != slug {
		t.Fatalf("the re-minted paste must still be listed, got %+v", list)
	}
	sum, err := repo.SumActiveBytesByOwner(owner, later)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != 700 {
		t.Fatalf("the re-minted paste must still be charged, got %d bytes (want 700)", sum)
	}
}

// Once the {slug} rows are gone nothing identifies the owner any more, so a
// retry of the delete has no subject: the durable intent is what carries the
// owner, the slug and the entry's own bytes to a sweep that can finish the job.
func TestDeleteIntent_SweepPrunesTheOwnerEntryLeftBehind(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("delint01")
	owner := "key:delint"
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-delint", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	entryKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	guard, err := repo.GetRawForTest(entryKey)
	if err != nil || len(guard) == 0 {
		t.Fatalf("read the entry bytes: err=%v len=%d", err, len(guard))
	}
	// The state a failed second step leaves: the authoritative rows gone, the
	// owner's entry and document still naming the paste.
	for _, key := range [][]byte{
		storage.LegacyPasteKeyForTest(slug),
		storage.LegacySlugOwnerKeyForTest(slug),
		storage.LegacyVersionKeyForTest(slug, 1),
		storage.VersionsDocKeyForTest(slug),
	} {
		if derr := repo.DeleteRawForTest(key); derr != nil {
			t.Fatalf("clear %s: %v", key, derr)
		}
	}
	if list, lerr := repo.ListByOwner(owner); lerr != nil || len(list) != 1 {
		t.Fatalf("fixture: the phantom must be listed, got %+v err=%v", list, lerr)
	}

	in := durable.Intent{
		ID: durable.ID(slug.String()), Kind: durable.KindDeletePaste,
		Scope: durable.Scope(owner), Subject: slug.String(),
		Guard: guard, StartedAt: now.Add(-2 * storage.ResolveGrace),
	}
	settled, err := repo.ResolveIntents(ctx, []durable.Intent{in}, now)
	if err != nil || settled != 1 {
		t.Fatalf("resolve the delete intent: settled=%d err=%v", settled, err)
	}
	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("the phantom must be gone from both representations, got %+v", list)
	}
	sum, err := repo.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != 0 {
		t.Fatalf("the phantom must stop being charged, got %d bytes", sum)
	}
}

// The rename's target owner comes OUT of the transaction that renamed the row.
// slug_owner can name a DIFFERENT identity by the time the projection is
// written - a delete plus a re-mint - and a rename written there lands on
// somebody else's paste.
func TestSetName_TargetsTheOwnerOfTheRowItRenamed(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("rename01")
	owner := "key:rename"
	other := "key:rename-other"
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-rename", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	// What a re-mint of the slug by another identity leaves behind.
	if err := repo.PutRawForTest(storage.LegacySlugOwnerKeyForTest(slug), []byte(other)); err != nil {
		t.Fatalf("repoint slug_owner: %v", err)
	}
	if err := repo.SetName(slug, "labelled"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	list, err := repo.ListByOwner(owner)
	if err != nil || len(list) != 1 {
		t.Fatalf("owner listing: %+v err=%v", list, err)
	}
	if list[0].Name != "labelled" {
		t.Fatalf("the rename must reach the renamed paste's own owner, got name %q", list[0].Name)
	}
	if strangers, err := repo.ListByOwner(other); err != nil || len(strangers) != 0 {
		t.Fatalf("no other identity's index may be touched, got %+v err=%v", strangers, err)
	}
}

// The pre-migration unpin chooses its roll target from a scan outside the
// transaction, and its candidate-key probe only detects a writer that ADDS a
// number. A per-version delete rewrites an EXISTING row, so the target is
// re-read inside the transaction; otherwise the head rolls onto a blob that
// same tombstone unbound.
func TestUnpinFromLegacyRows_RefusesATargetTombstonedAfterTheScan(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("unpinl01")
	twoVersionPaste(t, repo, slug, "key:unpinl", now)
	v1, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	deleteVersionsDoc(t, repo, slug) // pre-migration shape: the row path runs

	var once sync.Once
	repo.SetLegacyUnpinScannedHookForTest(func(s domain.Slug) {
		once.Do(func() {
			if derr := repo.DeleteVersion(s, 2); derr != nil {
				t.Errorf("racing tombstone of the roll target: %v", derr)
			}
		})
	})
	err = repo.Unpin(slug)
	repo.SetLegacyUnpinScannedHookForTest(nil)

	if !errors.Is(err, domain.ErrConcurrentChange) {
		t.Fatalf("a roll onto a version tombstoned since the scan must abort, got %v", err)
	}
	head, herr := repo.Get(slug)
	if herr != nil {
		t.Fatalf("head: %v", herr)
	}
	if head.PinnedVersion != 1 || head.ContentSHA != v1.ContentSHA {
		t.Fatalf("nothing may be applied: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
}

// The dispatch between the document path and the row path is also read outside
// any transaction. A document appearing in that window means the decision
// belongs to the document path, which applies a completeness gate the row path
// has no way to.
func TestUnpinFromLegacyRows_RefusesWhenADocumentAppearsAfterTheDispatch(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("unpinl02")
	twoVersionPaste(t, repo, slug, "key:unpinl2", now)
	docKey := storage.VersionsDocKeyForTest(slug)
	stored, err := repo.GetRawForTest(docKey)
	if err != nil || len(stored) == 0 {
		t.Fatalf("read the document: err=%v len=%d", err, len(stored))
	}
	deleteVersionsDoc(t, repo, slug)

	var once sync.Once
	repo.SetLegacyUnpinScannedHookForTest(func(domain.Slug) {
		once.Do(func() {
			// A heal restores the document without adding a version, so the
			// candidate-key probe sees nothing.
			if perr := repo.PutRawForTest(docKey, stored); perr != nil {
				t.Errorf("racing heal: %v", perr)
			}
		})
	})
	err = repo.Unpin(slug)
	repo.SetLegacyUnpinScannedHookForTest(nil)

	if !errors.Is(err, domain.ErrConcurrentChange) {
		t.Fatalf("a document appearing after the dispatch must abort the row path, got %v", err)
	}
}

// A fail-open walk cannot be built on a strict scan: envelope damage aborts
// before any per-record decision, so one entry would deny the owner their
// listing, their quota figure and their next write all at once.
func TestOwnerIndex_EnvelopeDamagedEntryDoesNotBrickTheOwner(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:ownenv"
	ctx := context.Background()

	for i, slug := range []domain.Slug{"ownenv01", "ownenv02"} {
		p := domain.Paste{
			Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
			ContentSHA: string(slug), Size: 100 * (i + 1), CreatedAt: now, UpdatedAt: now}
		if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", slug, err)
		}
	}
	repo.WaitPendingConfirms()
	// A pre-migration owner, so every walk goes over the entry rows.
	if err := repo.DeleteRawForTest(storage.OwnerDocKeyForTest(owner)); err != nil {
		t.Fatalf("drop the owner doc: %v", err)
	}
	if err := repo.PutRawForTest(storage.IdentityPasteKeyForTest(owner, "ownenv01"), truncatedEnvelope); err != nil {
		t.Fatalf("truncate the entry envelope: %v", err)
	}

	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("one damaged entry must not deny the listing: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("the damaged entry must render through its authoritative row, got %+v", list)
	}
	if _, err := repo.SumActiveBytesByOwner(owner, now); err != nil {
		t.Fatalf("one damaged entry must not deny the quota figure: %v", err)
	}
	if _, err := repo.CountByOwner(owner); err != nil {
		t.Fatalf("one damaged entry must not deny the count: %v", err)
	}
	// The write surface heals the owner document from the same walk, and the
	// damaged entry's paste is read through its head rather than dropped.
	if err := repo.SetName("ownenv02", "labelled"); err != nil {
		t.Fatalf("one damaged entry must not deny the owner's writes: %v", err)
	}
	healed, err := repo.ListByOwner(owner)
	if err != nil || len(healed) != 2 {
		t.Fatalf("the healed document must carry both pastes, got %+v err=%v", healed, err)
	}
}
