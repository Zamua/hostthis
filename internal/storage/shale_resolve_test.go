//go:build slatedb

package storage_test

// The durable-intent contract, pinned at each crash boundary:
//
//   - a crash after T1 leaves an entry with no row -> the sweep rolls BACK,
//   - a crash after T2 leaves a complete paste -> the sweep rolls FORWARD and
//     must NOT delete it,
//   - an intent younger than the grace is untouched, because a live upload on
//     another node is indistinguishable from an abandoned one,
//   - the compensating delete is value-guarded, so a re-upload that landed
//     after the crash survives the rollback,
//   - resolving twice converges.
//
//	go test -tags slatedb -run 'TestShaleIntent' ./internal/storage
//
// Skips unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/durable"
	"github.com/Zamua/hostthis/internal/storage"
)

// crashedInsert reproduces a process death between T1 and T2: the intent and
// the enumeration entry exist, the authoritative row never landed. It writes
// the same shapes the insert path writes rather than calling it, because the
// real path completes and there is no way to kill it midway in-process.
func crashedInsert(t *testing.T, repo *storage.ShaleRepo, owner, slug string, size int, started time.Time) durable.Intent {
	t.Helper()
	idxKey := storage.IdentityPasteKeyForTest(owner, slug)
	entry, err := json.Marshal(map[string]any{
		"name": "", "size": size, "served_size": size, "created_at": started,
		"kind": string(domain.KindHTML), "latest_version": 1, "updated_at": started,
	})
	if err != nil {
		t.Fatalf("encode entry: %v", err)
	}
	if err := repo.PutRawForTest(idxKey, entry); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	stored, err := repo.GetRawForTest(idxKey)
	if err != nil {
		t.Fatalf("read entry guard: %v", err)
	}
	in := durable.Intent{
		ID: durable.ID(slug), Kind: durable.KindCreatePaste,
		Scope: durable.Scope(owner), Subject: slug,
		Reached: []durable.StepName{storage.StepEntryWritten},
		Guard:   stored, StartedAt: started,
	}
	if err := repo.IntentLogForTest().Begin(context.Background(), in); err != nil {
		t.Fatalf("begin intent: %v", err)
	}
	return in
}

func TestShaleIntentSweepRollsBackAnEntryWithNoRow(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping intent sweep test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner := "key:introll"

	crashedInsert(t, repo, owner, "introll1", 400, now.Add(-time.Hour))
	if got := mustSum(t, repo, owner, now); got != 400 {
		t.Fatalf("fixture: the phantom must charge quota before the sweep, got %d", got)
	}

	settled, err := repo.SweepIntents(context.Background(), now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled: got %d, want 1", settled)
	}
	if got := mustSum(t, repo, owner, now); got != 0 {
		t.Fatalf("the rollback must release the phantom's bytes: got %d, want 0", got)
	}
	if list, err := repo.ListByOwner(owner); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("the phantom must be gone from the listing: got %+v", list)
	}
}

// A crash after T2 leaves a REAL paste whose intent was never forgotten.
// Rolling that back would delete live content, so the sweep must roll forward.
func TestShaleIntentSweepRollsForwardWhenTheRowLanded(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping intent sweep test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner := "key:intfwd"
	slug := domain.Slug("intfwd11")

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-intfwd", Size: 250, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	// Re-open an intent for the COMPLETED paste, as a lost T3 would leave.
	idxKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	guard, err := repo.GetRawForTest(idxKey)
	if err != nil {
		t.Fatalf("read guard: %v", err)
	}
	if err := repo.IntentLogForTest().Begin(context.Background(), durable.Intent{
		ID: durable.ID(slug), Kind: durable.KindCreatePaste, Scope: durable.Scope(owner),
		Subject: slug.String(), Reached: []durable.StepName{storage.StepEntryWritten},
		Guard: guard, StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("begin intent: %v", err)
	}

	if _, err := repo.SweepIntents(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := repo.Get(slug); err != nil {
		t.Fatalf("the sweep must NOT delete a paste whose row landed: %v", err)
	}
	if got := mustSum(t, repo, owner, now); got != 250 {
		t.Fatalf("bytes after roll-forward: got %d, want 250", got)
	}
	if got := mustSum(t, repo, owner, now); got != 250 {
		t.Fatalf("roll-forward must be idempotent: got %d, want 250", got)
	}
}

// The grace is the only thing separating an in-flight upload on another node
// from an abandoned one. Without it the sweep deletes live work.
func TestShaleIntentSweepLeavesFreshIntentsAlone(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping intent grace test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner := "key:intfresh"

	// Started one second ago: another node could still be mid-write.
	crashedInsert(t, repo, owner, "intfrsh1", 300, now.Add(-time.Second))

	settled, err := repo.SweepIntents(context.Background(), now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if settled != 0 {
		t.Fatalf("a fresh intent must not be resolved: settled %d", settled)
	}
	if got := mustSum(t, repo, owner, now); got != 300 {
		t.Fatalf("the in-flight entry must survive: got %d, want 300", got)
	}

	// Once it ages past the grace, the same sweep settles it.
	later := now.Add(storage.ResolveGrace + time.Minute)
	if settled, err = repo.SweepIntents(context.Background(), later); err != nil {
		t.Fatalf("sweep (aged): %v", err)
	}
	if settled != 1 {
		t.Fatalf("an aged intent must be resolved: settled %d, want 1", settled)
	}
}

// The hazard the guard exists for: the sweep decided to roll back, and a
// FRESHER entry for that slug landed before its delete ran. Deleting then would
// remove a live entry and leave a row with no entry - an UNDER-count, the
// direction that lets an owner breach the cap.
//
// The interleaving is built directly rather than by re-uploading, because a
// re-upload of the same slug reuses the intent ID, overwrites the crashed
// intent and then completes it - leaving the sweep nothing to resolve and the
// guard unexercised. That version of this test passed with the guard removed.
func TestShaleIntentRollbackSparesAFresherEntry(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping intent guard test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner := "key:intguard"
	slug := "intgrd11"

	crashedInsert(t, repo, owner, slug, 400, now.Add(-time.Hour))

	// A fresher entry lands for the same slug, without completing the intent -
	// exactly what the sweep must not clobber.
	idxKey := storage.IdentityPasteKeyForTest(owner, slug)
	fresher, err := json.Marshal(map[string]any{
		"name": "fresher", "size": 120, "served_size": 120, "created_at": now,
		"kind": string(domain.KindHTML), "latest_version": 1, "updated_at": now,
	})
	if err != nil {
		t.Fatalf("encode fresher entry: %v", err)
	}
	if err := repo.PutRawForTest(idxKey, fresher); err != nil {
		t.Fatalf("write fresher entry: %v", err)
	}

	if _, err := repo.SweepIntents(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	raw, err := repo.GetRawForTest(idxKey)
	if err != nil {
		t.Fatalf("read entry after sweep: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("the guarded rollback deleted a fresher entry")
	}
	if got := mustSum(t, repo, owner, now); got != 120 {
		t.Fatalf("the fresher entry must survive intact: got %d, want 120", got)
	}
}

// Units are replicated, so more than one node can resolve the same intent.
func TestShaleIntentSweepIsIdempotent(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping intent idempotence test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner := "key:intidem"

	crashedInsert(t, repo, owner, "intidem1", 500, now.Add(-time.Hour))
	for i := range 3 {
		if _, err := repo.SweepIntents(context.Background(), now); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
		if got := mustSum(t, repo, owner, now); got != 0 {
			t.Fatalf("after sweep %d: got %d, want 0", i, got)
		}
	}
}

// A completed insert must leave nothing behind for a sweep to find.
func TestShaleIntentCompletedInsertLeavesNoIntent(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping intent cleanup test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner := "key:intclean"

	p := domain.Paste{
		Slug: "intcln11", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-intcln", Size: 90, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	out, err := repo.IntentLogForTest().Outstanding(context.Background(), durable.Scope(owner))
	if err != nil {
		t.Fatalf("outstanding: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a completed insert must forget its intent: got %+v", out)
	}
}

// A site deploy has the paste path's dual write, so it needs the same
// protection. This also exercises the resolver's site branch, which reads
// sites/<slug> and rolls back identity_sites/<id>/<slug>.
func TestShaleIntentSweepRollsBackACrashedSiteDeploy(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping site intent test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner := "key:intsite"
	slug := "intsite1"

	// A crashed deploy: site entry written, sites/<slug> never landed.
	idxKey := storage.IdentitySiteKeyForTest(owner, slug)
	entry, err := json.Marshal(map[string]any{
		"size": 900, "created_at": now.Add(-time.Hour), "updated_at": now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("encode site entry: %v", err)
	}
	if err := repo.PutRawForTest(idxKey, entry); err != nil {
		t.Fatalf("write site entry: %v", err)
	}
	stored, err := repo.GetRawForTest(idxKey)
	if err != nil {
		t.Fatalf("read guard: %v", err)
	}
	if err := repo.IntentLogForTest().Begin(context.Background(), durable.Intent{
		ID: durable.ID(slug), Kind: durable.KindDeploySite, Scope: durable.Scope(owner),
		Subject: slug, Reached: []durable.StepName{storage.StepEntryWritten},
		Guard: stored, StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("begin intent: %v", err)
	}

	if got := mustSiteSum(t, repo, owner, now); got != 900 {
		t.Fatalf("fixture: the site phantom must charge quota, got %d", got)
	}
	if _, err := repo.SweepIntents(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := mustSiteSum(t, repo, owner, now); got != 0 {
		t.Fatalf("the rollback must release the site phantom's bytes: got %d, want 0", got)
	}
}

// A completed site deploy leaves no intent for a sweep to act on.
func TestShaleIntentCompletedSiteDeployLeavesNoIntent(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set; skipping site intent cleanup test")
	}
	repo := newShaleRepoOnUniqueDB(t, endpoint)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	owner := "key:intsitec"

	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{
		SHA: "sha-intsitec", Size: 300, ContentType: "text/html; charset=utf-8",
	})
	site := domain.Site{
		Slug: "intstc11", Identity: domain.Identity(owner), Manifest: man,
		CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertSiteWithQuotaCheck(context.Background(), site, 300, 0, now); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	out, err := repo.IntentLogForTest().Outstanding(context.Background(), durable.Scope(owner))
	if err != nil {
		t.Fatalf("outstanding: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a completed site deploy must forget its intent: got %+v", out)
	}
}
