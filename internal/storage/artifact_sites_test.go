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
	sites := storage.NewArtifactSites(repo)
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

	listed, err := sites.ListSitesByOwner(owner.String(), now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Slug != s.Slug {
		t.Fatalf("list = %+v, want just %s", listed, s.Slug)
	}
}

// A DOCUMENT must not be reachable through the site port, or a paste could be
// served raw through the directory path.
func TestArtifactSites_DocumentIsNotASite(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	sites := storage.NewArtifactSites(repo)
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
	sites := storage.NewArtifactSites(repo)
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
	sites := storage.NewArtifactSites(repo)
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
	sites := storage.NewArtifactSites(repo)
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
