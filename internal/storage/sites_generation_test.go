package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/celldtest"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

type generationSwapSiteRepo struct {
	*celld.PasteRepo
	old         domain.Paste
	replacement domain.Paste
}

func (r *generationSwapSiteRepo) AppendManifestVersion(ctx context.Context, slug domain.Slug, generation string,
	kind domain.ContentKind, uploadID string, manifest domain.Manifest, size int, userCap int64, now time.Time,
) (storage.AppendResult, error) {
	if _, err := r.Delete(r.old.Slug, r.old.Identity, r.old.CreatedAt); err != nil {
		return storage.AppendResult{}, err
	}
	if err := r.InsertWithQuotaCheck(ctx, r.replacement, 0, r.replacement.CreatedAt); err != nil {
		return storage.AppendResult{}, err
	}
	return r.PasteRepo.AppendManifestVersion(ctx, slug, generation, kind, uploadID, manifest, size, userCap, now)
}

func siteManifest(key string) domain.Manifest {
	manifest := domain.NewManifest()
	manifest.Add("index.html", domain.ManifestEntry{Key: key, Size: 7, CompressedSize: 5, ContentType: "text/html"})
	return manifest
}

// A redeploy authorized for one site incarnation cannot append to its replacement.
func TestSitesReplaceFencesReplacementIncarnation(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	inner := celld.NewPasteRepo(celldtest.Endpoint(t), nil)
	sites := storage.NewSites(inner)
	initial := domain.Site{
		Slug: "site2345", Identity: "key:owner", Kind: domain.KindSite,
		Manifest: siteManifest("old-key"), CreatedAt: at, UpdatedAt: at,
	}
	if err := sites.InsertWithQuotaCheck(context.Background(), initial, 5, 0, at); err != nil {
		t.Fatalf("insert initial site: %v", err)
	}
	old, err := inner.Get(initial.Slug)
	if err != nil {
		t.Fatalf("get initial paste: %v", err)
	}
	replacement := old
	replacement.Generation = "replacement-generation"
	replacement.Manifest = siteManifest("replacement-key")
	replacement.CreatedAt = at.Add(time.Second)
	replacement.UpdatedAt = replacement.CreatedAt
	repo := &generationSwapSiteRepo{PasteRepo: inner, old: old, replacement: replacement}
	sites = storage.NewSites(repo)

	update := initial
	update.Manifest = siteManifest("update-key")
	update.UpdatedAt = at.Add(2 * time.Second)
	if err := sites.ReplaceWithQuotaCheck(context.Background(), update, 5, 0, update.UpdatedAt); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("replace error = %v, want storage.ErrNotFound", err)
	}
	got, err := inner.Get(initial.Slug)
	if err != nil {
		t.Fatalf("get replacement: %v", err)
	}
	if got.Generation != replacement.Generation || got.RootEntry().Key != "replacement-key" {
		t.Fatalf("replacement changed: got %+v", got)
	}
	if vs, err := inner.ListVersions(initial.Slug); err != nil || len(vs) != 1 {
		t.Fatalf("replacement versions = %+v (%v), want only v1", vs, err)
	}
}
