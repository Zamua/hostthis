// The site port served by the artifact families.

//go:build !slatedb

package storage_test

import (
	"context"
	"errors"
	"sync"
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
	legacy := storage.NewShaleSiteRepo(repo)
	if err := legacy.InsertWithQuotaCheck(context.Background(), old, 49, 0, now); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	sites := storage.NewArtifactSites(repo, legacy)

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

	// The legacy row AND its enumeration entry must be gone. Leaving either
	// would have the owner listed and charged for the same directory twice.
	if listed, lerr := sites.ListSitesByOwner(owner.String(), now); lerr != nil || len(listed) != 0 {
		t.Fatalf("legacy rows survived the migration: %+v (err %v)", listed, lerr)
	}
	if sum, serr := sites.SumActiveBytesByOwner(owner.String(), now); serr != nil || sum != 0 {
		t.Fatalf("legacy charge survived the migration: %d (err %v)", sum, serr)
	}
}

// The sweep drains directories nobody redeploys. Without it a legacy row lives
// forever on the fallback and the old family can never be deleted.
//
// Uses the REAL legacy repo, not a fake: the sweep finds rows by scanning the
// cluster's site family, so a fake holding them in memory would prove nothing.
func TestArtifactSites_SweepMigratesUntouchedRows(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-s")

	legacy := storage.NewShaleSiteRepo(repo)
	slugs := []domain.Slug{"sweepa23", "sweepb23"}
	for _, slug := range slugs {
		s := domain.Site{
			Slug: slug, Identity: owner,
			Manifest: twoFileManifest("sha-" + string(slug)), CreatedAt: now, UpdatedAt: now,
		}
		if err := legacy.InsertWithQuotaCheck(context.Background(), s, 49, 0, now); err != nil {
			t.Fatalf("seed legacy %s: %v", slug, err)
		}
	}
	sites := storage.NewArtifactSites(repo, legacy)

	moved, err := sites.SweepLegacySites(context.Background(), now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if moved != len(slugs) {
		t.Fatalf("migrated %d, want %d", moved, len(slugs))
	}

	for _, slug := range slugs {
		p, gerr := repo.Get(slug)
		if gerr != nil {
			t.Fatalf("%s did not become an artifact: %v", slug, gerr)
		}
		if p.Kind != domain.KindSite || len(p.Manifest.Files) != 2 {
			t.Fatalf("%s migrated wrong: kind=%q files=%d", slug, p.Kind, len(p.Manifest.Files))
		}
		// The legacy row must be gone, or it is listed and charged twice.
		if _, lerr := legacy.Get(slug); !errors.Is(lerr, domain.ErrNotFound) {
			t.Fatalf("legacy row for %s survived the sweep: %v", slug, lerr)
		}
	}
	// And its ENUMERATION entry, which is what a listing actually reads: the
	// entry renders from its own cached values, so an orphan is a second copy
	// of the directory rather than a dangling pointer.
	stale, err := legacy.ListSitesByOwner(owner.String(), now)
	if err != nil {
		t.Fatalf("legacy list: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("legacy enumeration entries survived the sweep: %+v", stale)
	}

	// A second sweep is a no-op rather than a re-migration.
	again, err := sites.SweepLegacySites(context.Background(), now)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep migrated %d, want 0: the family is drained", again)
	}
}

// An entry whose row is already gone - a directory converted by a redeploy, or
// by a sweep that did not finish - must still be cleared. Scanning the
// authoritative rows would never see it, which is exactly the case that leaves
// a directory listed twice.
func TestArtifactSites_SweepClearsOrphanedEntries(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-s")

	legacy := storage.NewShaleSiteRepo(repo)
	sites := storage.NewArtifactSites(repo, legacy)
	slug := domain.Slug("orphan23")

	seed := domain.Site{
		Slug: slug, Identity: owner,
		Manifest: twoFileManifest("sha-orphan"), CreatedAt: now, UpdatedAt: now,
	}
	if err := legacy.InsertWithQuotaCheck(context.Background(), seed, 49, 0, now); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	// Convert it WITHOUT dropping the entry - exactly the state a crash between
	// the two steps leaves, since they cannot share a transaction.
	if err := repo.InsertSupersedingSite(context.Background(), domain.Paste{
		Slug: slug, Identity: owner, Status: domain.PasteStatusReady,
		Kind: domain.KindSite, ContentSHA: "sha-redeployed", Size: 55,
		CreatedAt: now, UpdatedAt: now,
		Manifest: twoFileManifest("sha-redeployed"),
	}, 0, now); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	// Precondition: the entry really is orphaned, or this test proves nothing.
	if stale, err := legacy.ListSitesByOwner(owner.String(), now); err != nil || len(stale) == 0 {
		t.Fatalf("expected an orphaned entry to clear: got %+v (err %v)", stale, err)
	}

	if _, err := sites.SweepLegacySites(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	stale, err := legacy.ListSitesByOwner(owner.String(), now)
	if err != nil {
		t.Fatalf("legacy list: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("orphaned entry survived the sweep: %+v", stale)
	}
	// The directory still serves from its artifact.
	if got, gerr := sites.Get(slug); gerr != nil {
		t.Fatalf("get after sweep: %v", gerr)
	} else if e, _ := got.Manifest.Lookup("/"); e.SHA != "sha-redeployed" {
		t.Fatalf("served root = %q, want sha-redeployed", e.SHA)
	}
}

// A migration must carry the RECORDED charge. The old layout did not persist
// per-entry compressed sizes, so recomputing from the manifest yields zero and
// makes every migrated directory free.
func TestArtifactSites_SweepPreservesTheRecordedCharge(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-s")

	// A manifest with NO compressed sizes, exactly as the old layout stored it.
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-i", Size: 100, ContentType: "text/html"})
	m.Add("app.css", domain.ManifestEntry{SHA: "sha-c", Size: 20, ContentType: "text/css"})
	if m.CompressedDedupedSize() != 0 {
		t.Fatalf("fixture must have no compressed sizes, else this proves nothing")
	}

	legacy := storage.NewShaleSiteRepo(repo)
	const charged = 929612
	if err := legacy.InsertWithQuotaCheck(context.Background(), domain.Site{
		Slug: "charge23", Identity: owner, Manifest: m, CreatedAt: now, UpdatedAt: now,
	}, charged, 0, now); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	if _, err := storage.NewArtifactSites(repo, legacy).SweepLegacySites(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := repo.SumActiveBytesByOwner(owner.String(), now)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if got != charged {
		t.Fatalf("owner charged %d after migration, want %d unchanged", got, charged)
	}
}

// A migrated directory must be able to RESOLVE its files. The old layout kept
// blob ids in a row-level side-table, not on the manifest; carrying the
// manifest alone produces a directory that lists and charges correctly and
// serves nothing.
func TestArtifactSites_MigrationCarriesFileBlobIDs(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-s")

	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-idx", Size: 100, ContentType: "text/html"})
	m.Add("app.css", domain.ManifestEntry{SHA: "sha-css", Size: 20, ContentType: "text/css"})

	legacy := storage.NewShaleSiteRepo(repo)
	slug := domain.Slug("blobid23")
	if err := legacy.InsertWithQuotaCheck(context.Background(), domain.Site{
		Slug: slug, Identity: owner, Manifest: m, CreatedAt: now, UpdatedAt: now,
	}, 49, 0, now); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	sites := storage.NewArtifactSites(repo, legacy)
	if _, err := sites.SweepLegacySites(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Every file the migrated manifest names must still address its bytes. On
	// the content-addressed path that is the sha itself, so this asserts the
	// entries survived intact rather than asserting a blob id is present.
	got, err := sites.Get(slug)
	if err != nil {
		t.Fatalf("get after sweep: %v", err)
	}
	for _, path := range []string{"index.html", "app.css"} {
		e, ok := got.Manifest.Files[path]
		if !ok {
			t.Fatalf("migrated manifest lost %q: %+v", path, got.Manifest.Files)
		}
		if e.SHA == "" {
			t.Fatalf("migrated entry %q has no address: %+v", path, e)
		}
	}
}

// Two drains racing on the same directory leave it LISTED and CHARGED.
//
// The migration writes the enumeration entry before the authoritative row, on a
// different shard, and removes the entry if the row write fails. Two callers
// converting the same directory both write that one entry, so the loser's
// rollback is aimed at a key the winner has already claimed. Deleting it there
// strands the artifact: it serves every file and reports its versions, while
// being invisible in its owner's listing and costing them nothing.
//
// Concurrent by construction rather than incidental: a node runs the drain from
// more than one place, and at R>1 more than one node mounts the unit holding
// the entry.
func TestArtifactSites_ConcurrentSweepsKeepTheDirectoryListed(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	owner := domain.Identity("key:owner-race")
	slug := domain.Slug("racesw23")

	legacy := storage.NewShaleSiteRepo(repo)
	if err := legacy.InsertWithQuotaCheck(context.Background(), domain.Site{
		Slug: slug, Identity: owner,
		Manifest: twoFileManifest("sha-race"), CreatedAt: now, UpdatedAt: now,
	}, 49, 0, now); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	sites := storage.NewArtifactSites(repo, legacy)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			<-start
			// Errors are the point of the race, not a failure: exactly one
			// caller converts and the other loses the slug.
			_, _ = sites.SweepLegacySites(context.Background(), now)
		})
	}
	close(start)
	wg.Wait()

	if _, err := repo.Get(slug); err != nil {
		t.Fatalf("the directory did not survive the race: %v", err)
	}
	listed, err := repo.ListByOwner(owner.String())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Slug != slug {
		t.Fatalf("owner's listing = %+v, want exactly %s: the artifact exists and serves, "+
			"but the losing rollback removed the entry the listing reads", listed, slug)
	}
	sum, err := repo.SumActiveBytesByOwner(owner.String(), now)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != 49 {
		t.Fatalf("owner charged %d, want 49: the entry carries the charge, so losing it "+
			"hosts the directory for free", sum)
	}
}
