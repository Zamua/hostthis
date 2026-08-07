package storage_test

// slug_owner lifecycle for SITES, pinned against the REAL shale backend.
//
// PreClaimSiteSlug writes a durable slug_owner marker and rejects any slug that
// already carries one, so every marker left behind removes a slug from the site
// namespace permanently. Two paths have to give it back: DeleteSite (the site
// is gone) and ReleaseSiteSlugClaim (the deploy never committed).
//
//	go test -tags slatedb -run TestShaleSite_SlugOwner ./internal/storage
//
// Skips cleanly unless MINIO_TEST_ENDPOINT is set.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// The shale site repo carries the compensating half of the pre-claim port; the
// deploy path finds it by type assertion, so a rename would silently disable
// the release.
var _ service.SlugClaimReleaser = (*storage.ShaleSiteRepo)(nil)

const slugOwnerTestOwner = "key:claimant"

var slugOwnerTestNow = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

// slugOwnerSite builds a one-file site for slug, owned by owner.
func slugOwnerSite(slug, owner string) domain.Site {
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{
		SHA:         "sha-" + slug + "-index",
		Size:        64,
		ContentType: "text/html; charset=utf-8",
	})
	return domain.Site{
		Slug:      domain.Slug(slug),
		Identity:  domain.Identity(owner),
		Manifest:  man,
		CreatedAt: slugOwnerTestNow,
		UpdatedAt: slugOwnerTestNow,
	}
}

func slugOwnerRepo(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	return newShaleRepoForTest(t)
}

func slugOwnerPresent(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug) bool {
	t.Helper()
	raw, err := repo.GetRawForTest(storage.LegacySlugOwnerKeyForTest(slug))
	if err != nil {
		t.Fatalf("read slug_owner/%s: %v", slug, err)
	}
	return raw != nil
}

// TestShaleSite_SlugOwnerReleasedOnDelete: deleting a site returns its slug to
// the namespace. A marker outliving the row would make the slug undeployable
// for the life of the cluster.
func TestShaleSite_SlugOwnerReleasedOnDelete(t *testing.T) {
	repo := slugOwnerRepo(t)
	ctx := context.Background()
	slug := domain.Slug("delsite1")

	if err := repo.PreClaimSiteSlug(ctx, slug, slugOwnerTestOwner, slugOwnerTestNow); err != nil {
		t.Fatalf("PreClaimSiteSlug: %v", err)
	}
	if !slugOwnerPresent(t, repo, slug) {
		t.Fatal("pre-claim did not write slug_owner")
	}
	site := slugOwnerSite(slug.String(), slugOwnerTestOwner)
	if err := repo.InsertSiteWithQuotaCheck(ctx, site, 64, 0, slugOwnerTestNow); err != nil {
		t.Fatalf("InsertSiteWithQuotaCheck: %v", err)
	}

	if err := repo.DeleteSite(slug); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	if slugOwnerPresent(t, repo, slug) {
		t.Fatal("slug_owner survived DeleteSite: the slug is burned out of the site namespace")
	}
	// The observable consequence: the slug can be deployed again.
	if err := repo.PreClaimSiteSlug(ctx, slug, slugOwnerTestOwner, slugOwnerTestNow); err != nil {
		t.Fatalf("re-claim after delete: %v, want nil", err)
	}
}

// TestShaleSite_SlugOwnerReleasedOnAbortedDeploy: a claim staked for a deploy
// that never committed is handed back, so a client feeding bad archives cannot
// erode the namespace.
func TestShaleSite_SlugOwnerReleasedOnAbortedDeploy(t *testing.T) {
	repo := slugOwnerRepo(t)
	ctx := context.Background()
	slug := domain.Slug("abort111")

	if err := repo.PreClaimSiteSlug(ctx, slug, slugOwnerTestOwner, slugOwnerTestNow); err != nil {
		t.Fatalf("PreClaimSiteSlug: %v", err)
	}
	if err := repo.ReleaseSiteSlugClaim(ctx, slug, slugOwnerTestOwner); err != nil {
		t.Fatalf("ReleaseSiteSlugClaim: %v", err)
	}
	if slugOwnerPresent(t, repo, slug) {
		t.Fatal("slug_owner survived the release")
	}
	if err := repo.PreClaimSiteSlug(ctx, slug, slugOwnerTestOwner, slugOwnerTestNow); err != nil {
		t.Fatalf("re-claim after release: %v, want nil", err)
	}

	// Idempotent: a repeated release of an already-released claim is a no-op,
	// and it must not drop the claim just re-staked by a different call.
	if err := repo.ReleaseSiteSlugClaim(ctx, slug, "key:someone-else"); err != nil {
		t.Fatalf("release of a foreign claim: %v, want nil", err)
	}
	if !slugOwnerPresent(t, repo, slug) {
		t.Fatal("release dropped another identity's claim")
	}
	if err := repo.ReleaseSiteSlugClaim(ctx, domain.Slug("neverset"), slugOwnerTestOwner); err != nil {
		t.Fatalf("release of an unclaimed slug: %v, want nil", err)
	}
}

// TestShaleSite_SlugOwnerKeptForCommittedRecord: the release must never strip
// the marker off a slug a record actually landed on, in either family. Doing so
// would let a second site claim a live slug and would cost the reconciler the
// only decode-independent owner pointer it has.
func TestShaleSite_SlugOwnerKeptForCommittedRecord(t *testing.T) {
	repo := slugOwnerRepo(t)
	ctx := context.Background()

	siteSlug := domain.Slug("livesite")
	if err := repo.PreClaimSiteSlug(ctx, siteSlug, slugOwnerTestOwner, slugOwnerTestNow); err != nil {
		t.Fatalf("PreClaimSiteSlug: %v", err)
	}
	site := slugOwnerSite(siteSlug.String(), slugOwnerTestOwner)
	if err := repo.InsertSiteWithQuotaCheck(ctx, site, 64, 0, slugOwnerTestNow); err != nil {
		t.Fatalf("InsertSiteWithQuotaCheck: %v", err)
	}
	if err := repo.ReleaseSiteSlugClaim(ctx, siteSlug, slugOwnerTestOwner); err != nil {
		t.Fatalf("ReleaseSiteSlugClaim: %v", err)
	}
	if !slugOwnerPresent(t, repo, siteSlug) {
		t.Fatal("release dropped the marker of a committed site")
	}
	if err := repo.PreClaimSiteSlug(ctx, siteSlug, slugOwnerTestOwner, slugOwnerTestNow); !errors.Is(err, storage.ErrSlugTaken) {
		t.Fatalf("re-claim of a live site slug = %v, want ErrSlugTaken", err)
	}

	pasteSlug := domain.Slug("livepast")
	p := domain.Paste{
		Slug:       pasteSlug,
		Identity:   domain.Identity(slugOwnerTestOwner),
		Kind:       domain.KindHTML,
		ContentSHA: "sha-livepast-v1",
		Size:       32,
		CreatedAt:  slugOwnerTestNow,
		UpdatedAt:  slugOwnerTestNow,
	}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, slugOwnerTestNow); err != nil {
		t.Fatalf("insert paste: %v", err)
	}
	// Same identity, so only the committed-record check can save the marker.
	if err := repo.ReleaseSiteSlugClaim(ctx, pasteSlug, slugOwnerTestOwner); err != nil {
		t.Fatalf("ReleaseSiteSlugClaim over a paste: %v", err)
	}
	if !slugOwnerPresent(t, repo, pasteSlug) {
		t.Fatal("release dropped a live paste's slug_owner")
	}
}
