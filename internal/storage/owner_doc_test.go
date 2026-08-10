package storage_test

// The owner document (docs/SPEC.md "The owner document (v2 owner index)"):
// a pre-doc owner is healed by their first WRITE from the legacy rows, reads
// fall back to the legacy rows READ-ONLY while no doc exists, and a doc-backed
// render is identical to the legacy render for the same state.
//
// The pre-migration shape is staged by writing through the normal API and then
// deleting the owner_doc key: the legacy rows a doc release also maintains are
// exactly what a pre-doc deployment left behind.

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/durable"
	"github.com/Zamua/hostthis/internal/storage"
)

func deleteOwnerDoc(t *testing.T, repo *storage.ShaleRepo, owner string) {
	t.Helper()
	if err := repo.DeleteRawForTest(storage.OwnerDocKeyForTest(owner)); err != nil {
		t.Fatalf("delete owner doc: %v", err)
	}
	if ownerDocPresent(t, repo, owner) {
		t.Fatal("owner doc still present after delete")
	}
}

func ownerDocPresent(t *testing.T, repo *storage.ShaleRepo, owner string) bool {
	t.Helper()
	raw, err := repo.GetRawForTest(storage.OwnerDocKeyForTest(owner))
	if err != nil {
		t.Fatalf("read owner doc: %v", err)
	}
	return len(raw) > 0
}

func TestOwnerDoc_FirstMutationHealsFromLegacyRows(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:docheal"

	a := domain.Paste{
		Slug: "dochl0a1", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-dochl-a", Size: 300,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
	b := domain.Paste{
		Slug: "dochl0b2", Identity: domain.Identity(owner), Kind: domain.KindMarkdown,
		ContentSHA: "sha-dochl-b", Size: 200,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	for _, p := range []domain.Paste{a, b} {
		if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}
	repo.WaitPendingConfirms()

	// Pre-migration shape: legacy rows only.
	deleteOwnerDoc(t, repo, owner)

	// One mutation migrates the owner: the doc must reappear carrying the
	// pre-existing entries, not just the mutation's subject.
	if err := repo.SetName(a.Slug, "renamed", a.Identity, a.CreatedAt); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !ownerDocPresent(t, repo, owner) {
		t.Fatal("a mutation on a pre-doc owner must heal the doc")
	}

	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("healed doc must carry the pre-existing entries: got %d rows %+v, want 2", len(list), list)
	}
	byslug := map[domain.Slug]domain.Paste{list[0].Slug: list[0], list[1].Slug: list[1]}
	if got := byslug[a.Slug]; got.Name != "renamed" || got.Size != 300 {
		t.Fatalf("mutated entry after heal = %+v, want name=renamed size=300", got)
	}
	if got := byslug[b.Slug]; got.Name != "" || got.Size != 200 || got.Kind != domain.KindMarkdown {
		t.Fatalf("pre-existing entry after heal = %+v, want untouched", got)
	}
	if n := mustCount(t, repo, owner); n != 2 {
		t.Fatalf("count after heal: got %d, want 2", n)
	}
	if got := mustSum(t, repo, owner, now); got != 500 {
		t.Fatalf("sum after heal: got %d, want 500", got)
	}
	// first_seen seeds from the legacy identity_first_seen key.
	first, err := repo.OwnerFirstSeen(owner)
	if err != nil {
		t.Fatalf("first seen: %v", err)
	}
	if !first.Equal(a.CreatedAt) {
		t.Fatalf("first seen after heal = %v, want %v (seeded from the legacy key)", first, a.CreatedAt)
	}
}

func TestOwnerDoc_ReadsFallBackWithoutWritingTheDoc(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:docro"

	a := domain.Paste{
		Slug: "docro0a1", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-docro-a", Size: 300, CreatedAt: now, UpdatedAt: now}
	b := domain.Paste{
		Slug: "docro0b2", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-docro-b", Size: 200, CreatedAt: now, UpdatedAt: now}
	for _, p := range []domain.Paste{a, b} {
		if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}
	repo.WaitPendingConfirms()
	deleteOwnerDoc(t, repo, owner)

	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("legacy fallback list: got %d rows, want 2", len(list))
	}
	if n := mustCount(t, repo, owner); n != 2 {
		t.Fatalf("legacy fallback count: got %d, want 2", n)
	}
	if got := mustSum(t, repo, owner, now); got != 500 {
		t.Fatalf("legacy fallback sum: got %d, want 500", got)
	}
	sum, err := repo.OwnerSummary(owner, now)
	if err != nil {
		t.Fatalf("owner summary: %v", err)
	}
	want := domain.OwnerSummary{Active: 2, FirstSeen: a.CreatedAt, PasteBytes: 500}
	if sum.Active != want.Active || !sum.FirstSeen.Equal(want.FirstSeen) ||
		sum.PasteBytes != want.PasteBytes || sum.SiteBytes != 0 {
		t.Fatalf("legacy fallback summary = %+v, want %+v", sum, want)
	}

	// The fallback is read-only by construction: no read may have minted a doc.
	if ownerDocPresent(t, repo, owner) {
		t.Fatal("a read on a pre-doc owner must not write the doc")
	}
}

// A doc-backed render must be indistinguishable from the legacy render of the
// same state: same rows, same fields, same order. Exercised over a mixed set
// (a site, a renamed paste, a pinned multi-version paste, a deleted paste).
func TestOwnerDoc_DocRenderMatchesLegacyRender(t *testing.T) {
	repo := newShaleRepoForTest(t)
	sites := storage.NewSites(repo)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:doceq"
	ctx := context.Background()

	p1 := domain.Paste{
		Slug: "doceq0a1", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-doceq-a", Size: 100,
		CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now.Add(-4 * time.Hour)}
	p2 := domain.Paste{
		Slug: "doceq0b2", Identity: domain.Identity(owner), Kind: domain.KindMarkdown,
		ContentSHA: "sha-doceq-b", Size: 150,
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour)}
	gone := domain.Paste{
		Slug: "doceq0c3", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-doceq-c", Size: 999,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
	for _, p := range []domain.Paste{p1, p2, gone} {
		if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}
	repo.WaitPendingConfirms()

	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{
		SHA: "sha-doceq-site", Size: 80, CompressedSize: 60, ContentType: "text/html"})
	site := domain.Site{
		Slug: "doceq0s4", Identity: domain.Identity(owner), Manifest: m,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	if err := sites.InsertWithQuotaCheck(ctx, site, 60, 0, now); err != nil {
		t.Fatalf("insert site: %v", err)
	}
	repo.WaitPendingConfirms()

	if err := repo.SetName(p1.Slug, "labelled", p1.Identity, p1.CreatedAt); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, p2.Slug, domain.KindMarkdown, "sha-doceq-b2", 50, 0, now.Add(-30*time.Minute)); err != nil {
		t.Fatalf("append: %v", err)
	}
	v1, err := repo.GetVersion(p2.Slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(p2.Slug, v1); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := repo.Delete(gone.Slug, gone.Identity, gone.CreatedAt); err != nil {
		t.Fatalf("delete: %v", err)
	}

	fromDoc, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("doc list: %v", err)
	}
	docCount := mustCount(t, repo, owner)
	docSum := mustSum(t, repo, owner, now)
	docFirst, err := repo.OwnerFirstSeen(owner)
	if err != nil {
		t.Fatalf("doc first seen: %v", err)
	}
	docSummary, err := repo.OwnerSummary(owner, now)
	if err != nil {
		t.Fatalf("doc summary: %v", err)
	}

	// Same state, legacy representation.
	deleteOwnerDoc(t, repo, owner)

	fromLegacy, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("legacy list: %v", err)
	}
	if !reflect.DeepEqual(fromDoc, fromLegacy) {
		t.Fatalf("doc render diverged from the legacy render:\n doc:    %+v\n legacy: %+v", fromDoc, fromLegacy)
	}
	if len(fromDoc) != 3 {
		t.Fatalf("fixture: expected 3 live rows (2 pastes + 1 site), got %+v", fromDoc)
	}
	if n := mustCount(t, repo, owner); n != docCount {
		t.Fatalf("count diverged: doc %d, legacy %d", docCount, n)
	}
	if got := mustSum(t, repo, owner, now); got != docSum {
		t.Fatalf("sum diverged: doc %d, legacy %d", docSum, got)
	}
	legacyFirst, err := repo.OwnerFirstSeen(owner)
	if err != nil {
		t.Fatalf("legacy first seen: %v", err)
	}
	if !docFirst.Equal(legacyFirst) {
		t.Fatalf("first seen diverged: doc %v, legacy %v", docFirst, legacyFirst)
	}
	legacySummary, err := repo.OwnerSummary(owner, now)
	if err != nil {
		t.Fatalf("legacy summary: %v", err)
	}
	if docSummary.Active != legacySummary.Active ||
		!docSummary.FirstSeen.Equal(legacySummary.FirstSeen) ||
		docSummary.PasteBytes != legacySummary.PasteBytes ||
		docSummary.SiteBytes != legacySummary.SiteBytes {
		t.Fatalf("summary diverged: doc %+v, legacy %+v", docSummary, legacySummary)
	}
}

// An insert rollback must drop the doc entry along with the legacy entry.
// Doc-first reads have no pruning path, so a rollback that removed only the
// row would leave the slug-race loser's phantom over-counting the owner's
// quota permanently. The rollback is forced through the intent sweep, which
// shares guardedDropOwnerEntry with insertArtifact's in-line rollback; the
// staged state (entry + doc entry, no authoritative row, open intent) is
// exactly what a crash between the confirm CAS and the authoritative write
// leaves for a doc-present owner.
func TestOwnerDoc_InsertRollbackDropsTheDocEntry(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Now().UTC()
	owner := "key:docroll"
	ctx := context.Background()

	keeper := domain.Paste{
		Slug: "docrl0k1", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-docrl-k", Size: 200, CreatedAt: now, UpdatedAt: now}
	victim := domain.Paste{
		Slug: "docrl0v2", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-docrl-v", Size: 400, CreatedAt: now, UpdatedAt: now}
	for _, p := range []domain.Paste{keeper, victim} {
		if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}
	repo.WaitPendingConfirms()

	// Turn the victim into the crashed-insert shape: entry + doc entry stay,
	// the authoritative rows never landed, the intent is still open.
	for _, key := range [][]byte{
		storage.LegacyPasteKeyForTest(victim.Slug),
		storage.LegacyVersionKeyForTest(victim.Slug, 1),
		storage.LegacySlugOwnerKeyForTest(victim.Slug),
	} {
		if err := repo.DeleteRawForTest(key); err != nil {
			t.Fatalf("strip authoritative row %q: %v", key, err)
		}
	}
	guard, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(owner, victim.Slug.String()))
	if err != nil {
		t.Fatalf("read entry guard: %v", err)
	}
	if err := repo.IntentLogForTest().Begin(ctx, durable.Intent{
		ID: durable.ID(victim.Slug), Kind: durable.KindCreatePaste,
		Scope: durable.Scope(owner), Subject: victim.Slug.String(),
		Reached: []durable.StepName{storage.StepEntryWritten},
		Guard:   guard, StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("begin intent: %v", err)
	}

	// Fixture: the phantom must be charged through the doc before the sweep,
	// or the drop below proves nothing.
	if got := mustSum(t, repo, owner, now); got != 600 {
		t.Fatalf("fixture: doc must carry the phantom before the rollback, got %d, want 600", got)
	}

	if settled, err := repo.SweepIntents(ctx, now); err != nil {
		t.Fatalf("sweep: %v", err)
	} else if settled != 1 {
		t.Fatalf("settled: got %d, want 1", settled)
	}

	if raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(owner, victim.Slug.String())); err != nil {
		t.Fatalf("read entry after rollback: %v", err)
	} else if len(raw) != 0 {
		t.Fatalf("rollback must drop the legacy entry; got %q", raw)
	}
	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Slug != keeper.Slug {
		t.Fatalf("doc must not carry the rolled-back slug: got %+v, want just %q", list, keeper.Slug)
	}
	if got := mustSum(t, repo, owner, now); got != 200 {
		t.Fatalf("sum after rollback: got %d, want 200 (the keeper alone)", got)
	}
	if n := mustCount(t, repo, owner); n != 1 {
		t.Fatalf("count after rollback: got %d, want 1", n)
	}
}

// A rollback whose guard mismatches (a fresher entry landed for the slug)
// must touch NEITHER representation: the fresh write's own path maintains
// the doc, and dropping its entry would under-count.
func TestOwnerDoc_RollbackGuardMismatchLeavesDocAlone(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Now().UTC()
	owner := "key:docgrd"
	ctx := context.Background()

	p := domain.Paste{
		Slug: "docgrd01", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-docgrd", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	// An intent whose guard describes an OLDER entry than the one stored.
	if err := repo.IntentLogForTest().Begin(ctx, durable.Intent{
		ID: durable.ID(p.Slug), Kind: durable.KindCreatePaste,
		Scope: durable.Scope(owner), Subject: p.Slug.String(),
		Reached: []durable.StepName{storage.StepEntryWritten},
		Guard:   []byte(`{"name":"stale","size":1}`), StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("begin intent: %v", err)
	}
	// Strip the authoritative rows so the resolver takes the rollback branch.
	for _, key := range [][]byte{
		storage.LegacyPasteKeyForTest(p.Slug),
		storage.LegacyVersionKeyForTest(p.Slug, 1),
		storage.LegacySlugOwnerKeyForTest(p.Slug),
	} {
		if err := repo.DeleteRawForTest(key); err != nil {
			t.Fatalf("strip authoritative row %q: %v", key, err)
		}
	}

	if _, err := repo.SweepIntents(ctx, now); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// The mismatched guard spares the entry AND the doc entry.
	if raw, err := repo.GetRawForTest(storage.IdentityPasteKeyForTest(owner, p.Slug.String())); err != nil {
		t.Fatalf("read entry: %v", err)
	} else if len(raw) == 0 {
		t.Fatal("guard mismatch must spare the fresher legacy entry")
	}
	if got := mustSum(t, repo, owner, now); got != 300 {
		t.Fatalf("guard mismatch must spare the doc entry: got %d, want 300", got)
	}
}

// Two rows the doc must render exactly as the legacy entry cached them:
// served vs stored size and the pin, straight from one Get.
func TestOwnerDoc_RenderCarriesServedStoredAndPin(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:docfld"
	ctx := context.Background()

	p := domain.Paste{
		Slug: "docfld01", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-docfld-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, p.Slug, domain.KindHTML, "sha-docfld-v2", 50, 0, now); err != nil {
		t.Fatalf("append: %v", err)
	}
	v1, err := repo.GetVersion(p.Slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(p.Slug, v1); err != nil {
		t.Fatalf("pin: %v", err)
	}

	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list: got %d rows, want 1", len(list))
	}
	got := list[0]
	if got.StoredBytes != 350 {
		t.Errorf("stored bytes from the doc: got %d, want 350", got.StoredBytes)
	}
	if got.PinnedVersion != 1 {
		t.Errorf("pinned version from the doc: got %d, want 1", got.PinnedVersion)
	}
	if got.LatestVersion != 2 {
		t.Errorf("latest version from the doc: got %d, want 2", got.LatestVersion)
	}
}

// One unreadable sibling record must not brick the owner's write surface:
// the heal walk skips it (absent from the doc, the under-count direction)
// and the mutation succeeds carrying every healthy entry.
func TestOwnerDoc_HealFailsOpenOnUnreadableSibling(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:dochlfo"
	ctx := context.Background()

	a := domain.Paste{
		Slug: "dochlfa1", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-dochlfo-a", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, a, 0, now); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	repo.WaitPendingConfirms()

	// A sibling whose entry needs the authoritative read-through (thin, no
	// kind) and whose head row is undecodable: the shape that must be
	// skipped, not propagated.
	badSlug := "dochlbad"
	writeIndexEntryJSON(t, repo, storage.IdentityPasteKeyForTest(owner, badSlug), 50, now)
	if err := repo.PutRawForTest(storage.LegacyPasteKeyForTest(domain.Slug(badSlug)), corruptJSON); err != nil {
		t.Fatalf("corrupt sibling head: %v", err)
	}
	deleteOwnerDoc(t, repo, owner)

	// The first mutation must succeed and heal around the damage.
	b := domain.Paste{
		Slug: "dochlfb2", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-dochlfo-b", Size: 200, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, b, 0, now); err != nil {
		t.Fatalf("a mutation must heal AROUND an unreadable sibling, not fail on it: %v", err)
	}
	repo.WaitPendingConfirms()

	if !ownerDocPresent(t, repo, owner) {
		t.Fatal("the mutation must still heal the doc")
	}
	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("healed doc must carry the healthy entries: got %+v, want a+b", list)
	}
	for _, p := range list {
		if p.Slug == domain.Slug(badSlug) {
			t.Fatalf("the unreadable sibling must be absent from the doc: %+v", list)
		}
	}
	if got := mustSum(t, repo, owner, now); got != 500 {
		t.Fatalf("sum after fail-open heal: got %d, want 500", got)
	}
}

// An undecodable owner DOC must not be a per-owner outage: reads fall back
// to the legacy rows, and the next mutation overwrites the bad doc with one
// rebuilt from those rows.
func TestOwnerDoc_CorruptDocReadsFallBackAndWriteRebuilds(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:doccor"
	ctx := context.Background()

	a := domain.Paste{
		Slug: "doccor0a", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-doccor-a", Size: 300,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
	b := domain.Paste{
		Slug: "doccor0b", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-doccor-b", Size: 200,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	for _, p := range []domain.Paste{a, b} {
		if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
			t.Fatalf("insert %s: %v", p.Slug, err)
		}
	}
	repo.WaitPendingConfirms()

	if err := repo.PutRawForTest(storage.OwnerDocKeyForTest(owner), corruptJSON); err != nil {
		t.Fatalf("corrupt the doc: %v", err)
	}

	// Reads fail open to the legacy rows.
	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list over a corrupt doc must fall back, not fail: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("fallback list: got %d rows, want 2", len(list))
	}
	if got := mustSum(t, repo, owner, now); got != 500 {
		t.Fatalf("fallback sum: got %d, want 500", got)
	}
	if n := mustCount(t, repo, owner); n != 2 {
		t.Fatalf("fallback count: got %d, want 2", n)
	}
	sum, err := repo.OwnerSummary(owner, now)
	if err != nil {
		t.Fatalf("fallback summary: %v", err)
	}
	if sum.Active != 2 || sum.PasteBytes != 500 || !sum.FirstSeen.Equal(a.CreatedAt) {
		t.Fatalf("fallback summary = %+v, want active=2 bytes=500 first=%v", sum, a.CreatedAt)
	}

	// The next mutation replaces the corrupt doc with a good one.
	if err := repo.SetName(a.Slug, "fixed", a.Identity, a.CreatedAt); err != nil {
		t.Fatalf("rename over a corrupt doc must rebuild it, not fail: %v", err)
	}
	raw, err := repo.GetRawForTest(storage.OwnerDocKeyForTest(owner))
	if err != nil {
		t.Fatalf("read doc after rebuild: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("rebuilt doc must decode; got %q", raw)
	}
	list, err = repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list after rebuild: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("rebuilt doc must carry the pre-existing entries: got %+v, want 2 rows", list)
	}
	byslug := map[domain.Slug]domain.Paste{list[0].Slug: list[0], list[1].Slug: list[1]}
	if got := byslug[a.Slug]; got.Name != "fixed" {
		t.Fatalf("rebuilt doc must carry the mutation: %+v", got)
	}
	if got := mustSum(t, repo, owner, now); got != 500 {
		t.Fatalf("sum after rebuild: got %d, want 500", got)
	}
}

// A legacy empty-marker entry must not freeze the doc entry: doc-first reads
// never run the list-time enrichment, so the projection refresh itself must
// refresh the doc through the empty entry and recreate a real legacy row.
func TestOwnerDoc_EmptyLegacyEntryStillRefreshesTheDoc(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:docempt"
	ctx := context.Background()

	p := domain.Paste{
		Slug: "docempt1", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-docempt-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	// Regress the legacy entry to the old empty-marker shape.
	idxKey := storage.IdentityPasteKeyForTest(owner, p.Slug.String())
	if err := repo.PutEmptyBackendForTest(idxKey); err != nil {
		t.Fatalf("plant empty legacy entry: %v", err)
	}

	if _, err := repo.AppendVersionWithQuotaCheck(ctx, p.Slug, domain.KindHTML, "sha-docempt-v2", 200, 0, now); err != nil {
		t.Fatalf("append: %v", err)
	}

	list, err := repo.ListByOwner(owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].StoredBytes != 500 || list[0].LatestVersion != 2 {
		t.Fatalf("the refresh must reach the doc through an empty legacy entry: got %+v, want stored=500 latest=2", list)
	}
	if got := mustSum(t, repo, owner, now); got != 500 {
		t.Fatalf("sum after refresh: got %d, want 500", got)
	}
	// The dual-write recreated a real legacy row alongside.
	if got := readCachedIndexSize(t, repo, idxKey); got != 500 {
		t.Fatalf("the refresh must rewrite the legacy entry as a real row: got size %d, want 500", got)
	}
}
