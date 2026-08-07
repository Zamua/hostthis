// The site port served by the artifact families.

//go:build !slatedb

package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func twoFileManifest(indexSHA string) domain.Manifest {
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{
		SHA: indexSHA, Size: 100, CompressedSize: 40, ContentType: "text/html"})
	m.Add("app.css", domain.ManifestEntry{
		SHA: "sha-css", Size: 20, CompressedSize: 9, ContentType: "text/css"})
	return m
}

func TestArtifactSites_InsertGetAndList(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	sites := storage.NewArtifactSites(repo, nil)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-s")

	s := domain.Site{
		Slug: "sitea234", Identity: owner,
		Manifest: twoFileManifest("sha-index"), CreatedAt: now, UpdatedAt: now,
	}
	if err := sites.InsertWithQuotaCheck(context.Background(), s, 49, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := sites.Get(s.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Manifest.Files) != 2 || got.Identity != owner {
		t.Fatalf("round trip = %+v", got)
	}

	// The site listing reports LEGACY rows only: an artifact-backed directory
	// is already in the artifact listing the caller concatenates this onto, so
	// returning it here too would show it twice.
	listed, err := sites.ListSitesByOwner(owner.String(), now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("list = %+v, want empty: the artifact listing already carries it", listed)
	}
	arts, err := repo.ListByOwner(owner.String())
	if err != nil {
		t.Fatalf("artifact list: %v", err)
	}
	if len(arts) != 1 || arts[0].Slug != s.Slug || arts[0].Kind != domain.KindSite {
		t.Fatalf("artifact listing = %+v, want the directory", arts)
	}
}

// A DOCUMENT must not be reachable through the site port, or a paste could be
// served raw through the directory path.
func TestArtifactSites_DocumentIsNotASite(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	sites := storage.NewArtifactSites(repo, nil)
	now := time.Now().UTC().Truncate(time.Second)

	if err := repo.InsertWithQuotaCheck(context.Background(), domain.Paste{
		Slug: "docb2345", Identity: "key:owner-s", Status: domain.PasteStatusReady,
		Kind: domain.KindMarkdown, ContentSHA: "sha-doc", Size: 5,
		CreatedAt: now, UpdatedAt: now}, 0, now); err != nil {
		t.Fatalf("insert document: %v", err)
	}

	if _, err := sites.Get("docb2345"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("document via the site port = %v, want not-found", err)
	}
	listed, err := sites.ListSitesByOwner("key:owner-s", now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("a document must not appear as a site: %+v", listed)
	}
}

// A directory's bytes are already in the ARTIFACT sum the service adds this to,
// so reporting them again would bill every directory twice.
func TestArtifactSites_SumIsZeroToAvoidDoubleCounting(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	sites := storage.NewArtifactSites(repo, nil)
	now := time.Now().UTC().Truncate(time.Second)
	owner := "key:owner-s"

	s := domain.Site{
		Slug: "sitec234", Identity: domain.Identity(owner),
		Manifest: twoFileManifest("sha-index"), CreatedAt: now, UpdatedAt: now,
	}
	if err := sites.InsertWithQuotaCheck(context.Background(), s, 49, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The artifact sum must ALREADY carry the charge...
	artifactSum, err := repo.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("artifact sum: %v", err)
	}
	if artifactSum != 49 {
		t.Fatalf("artifact sum = %d, want the directory's 49", artifactSum)
	}
	// ...so the site sum must add nothing.
	siteSum, err := sites.SumActiveBytesByOwner(owner, now)
	if err != nil {
		t.Fatalf("site sum: %v", err)
	}
	if siteSum != 0 {
		t.Fatalf("site sum = %d, want 0: the artifact sum already counts it", siteSum)
	}
}

// A redeploy APPENDS a version, which is what gives a directory the history and
// rollback a document has.
func TestArtifactSites_RedeployAppendsAVersion(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	sites := storage.NewArtifactSites(repo, nil)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-s")

	s := domain.Site{
		Slug: "sited234", Identity: owner,
		Manifest: twoFileManifest("sha-v1"), CreatedAt: now, UpdatedAt: now,
	}
	if err := sites.InsertWithQuotaCheck(context.Background(), s, 49, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	s.Manifest = twoFileManifest("sha-v2")
	if err := sites.ReplaceWithQuotaCheck(context.Background(), s, 55, 0, now); err != nil {
		t.Fatalf("replace: %v", err)
	}

	vs, err := repo.ListVersions(s.Slug)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("versions = %d, want 2 (a redeploy is a new version)", len(vs))
	}
	got, err := sites.Get(s.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e, _ := got.Manifest.Lookup("/"); e.SHA != "sha-v2" {
		t.Fatalf("served root = %q, want the redeployed sha-v2", e.SHA)
	}
}

// Another identity's directory is not replaceable, and says so with the same
// sentinel a missing slug yields.
func TestArtifactSites_ReplaceRejectsForeignOwner(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	sites := storage.NewArtifactSites(repo, nil)
	now := time.Now().UTC().Truncate(time.Second)

	s := domain.Site{
		Slug: "sitee234", Identity: "key:owner-a",
		Manifest: twoFileManifest("sha-v1"), CreatedAt: now, UpdatedAt: now,
	}
	if err := sites.InsertWithQuotaCheck(context.Background(), s, 49, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	s.Identity = "key:owner-b"
	if err := sites.ReplaceWithQuotaCheck(context.Background(), s, 49, 0, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign replace = %v, want not-found", err)
	}
}

// fakeLegacy stands in for the pre-unification site family.
type fakeLegacy struct {
	sites map[domain.Slug]domain.Site
	bytes int64
}

func (f fakeLegacy) Get(slug domain.Slug) (domain.Site, error) {
	s, ok := f.sites[slug]
	if !ok {
		return domain.Site{}, storage.ErrNotFound
	}
	return s, nil
}

func (f fakeLegacy) ListSitesByOwner(string, time.Time) ([]domain.Site, error) {
	var out []domain.Site
	for _, s := range f.sites {
		out = append(out, s)
	}
	return out, nil
}

func (f fakeLegacy) SumActiveBytesByOwner(string, time.Time) (int64, error) {
	if len(f.sites) == 0 {
		return 0, nil
	}
	return f.bytes, nil
}

func (f fakeLegacy) Delete(slug domain.Slug) error {
	delete(f.sites, slug)
	return nil
}

// A directory deployed before the collapse keeps serving, keeps listing, and
// keeps being charged, until the migration moves it.
func TestArtifactSites_FallsBackToLegacyRows(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-s")

	old := domain.Site{
		Slug: "oldsite2", Identity: owner,
		Manifest: twoFileManifest("sha-old"), CreatedAt: now, UpdatedAt: now,
	}
	sites := storage.NewArtifactSites(repo, fakeLegacy{
		sites: map[domain.Slug]domain.Site{old.Slug: old}, bytes: 70,
	})

	got, err := sites.Get(old.Slug)
	if err != nil {
		t.Fatalf("legacy get: %v", err)
	}
	if len(got.Manifest.Files) != 2 {
		t.Fatalf("legacy site lost its manifest: %+v", got)
	}

	// A NEW directory lands as an artifact; both must list.
	fresh := domain.Site{
		Slug: "newsite2", Identity: owner,
		Manifest: twoFileManifest("sha-new"), CreatedAt: now, UpdatedAt: now,
	}
	if err := sites.InsertWithQuotaCheck(context.Background(), fresh, 49, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Each shape is listed exactly once, from the family that owns it.
	listed, err := sites.ListSitesByOwner(owner.String(), now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Slug != old.Slug {
		t.Fatalf("site listing = %+v, want just the legacy row", listed)
	}
	arts, err := repo.ListByOwner(owner.String())
	if err != nil {
		t.Fatalf("artifact list: %v", err)
	}
	if len(arts) != 1 || arts[0].Slug != fresh.Slug {
		t.Fatalf("artifact listing = %+v, want just the new directory", arts)
	}

	// The legacy bytes are NOT in the artifact sum, so they must still be
	// reported here or the owner is under-charged mid-migration.
	sum, err := sites.SumActiveBytesByOwner(owner.String(), now)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != 70 {
		t.Fatalf("legacy sum = %d, want 70", sum)
	}
}

// Redeploying a directory that predates the collapse must WORK, and must leave
// it as an artifact. Without this a legacy site becomes permanently
// un-redeployable the moment the artifact path is wired.
func TestArtifactSites_RedeployMigratesALegacyRow(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-s")

	old := domain.Site{
		Slug: "legacy23", Identity: owner,
		Manifest: twoFileManifest("sha-old"), CreatedAt: now, UpdatedAt: now,
	}
	sites := storage.NewArtifactSites(repo, fakeLegacy{
		sites: map[domain.Slug]domain.Site{old.Slug: old}, bytes: 49,
	})

	next := old
	next.Manifest = twoFileManifest("sha-new")
	if err := sites.ReplaceWithQuotaCheck(context.Background(), next, 55, 0, now); err != nil {
		t.Fatalf("redeploy of a legacy directory: %v", err)
	}

	got, err := sites.Get(old.Slug)
	if err != nil {
		t.Fatalf("get after redeploy: %v", err)
	}
	if e, _ := got.Manifest.Lookup("/"); e.SHA != "sha-new" {
		t.Fatalf("served root = %q, want the redeployed sha-new", e.SHA)
	}
	// It must now be an ARTIFACT, or it stays on the legacy path forever.
	p, err := repo.Get(old.Slug)
	if err != nil {
		t.Fatalf("redeploy did not create an artifact: %v", err)
	}
	if p.Kind != domain.KindSite {
		t.Fatalf("artifact kind = %q, want site", p.Kind)
	}

	// The legacy row must be GONE. Leaving it would have the owner listed and
	// charged for the same directory twice, once through each family.
	if listed, lerr := sites.ListSitesByOwner(owner.String(), now); lerr != nil || len(listed) != 0 {
		t.Fatalf("legacy row survived the migration: %+v (err %v)", listed, lerr)
	}
	if sum, serr := sites.SumActiveBytesByOwner(owner.String(), now); serr != nil || sum != 0 {
		t.Fatalf("legacy charge survived the migration: %d (err %v)", sum, serr)
	}
}
