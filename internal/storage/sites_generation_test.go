package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

type generationSwapSiteRepo struct {
	*MemRepo
	old         domain.Paste
	replacement domain.Paste
}

func (r *generationSwapSiteRepo) AppendManifestVersion(ctx context.Context, slug domain.Slug, generation string,
	manifest domain.Manifest, root domain.ManifestEntry, size int, userCap int64, now time.Time,
) (AppendResult, error) {
	if err := r.Delete(r.old.Slug, r.old.Identity, r.old.CreatedAt); err != nil {
		return AppendResult{}, err
	}
	if err := r.InsertWithQuotaCheck(ctx, r.replacement, 0, r.replacement.CreatedAt); err != nil {
		return AppendResult{}, err
	}
	return r.MemRepo.AppendManifestVersion(ctx, slug, generation, manifest, root, size, userCap, now)
}

func siteManifest(sha string) domain.Manifest {
	manifest := domain.NewManifest()
	manifest.Add("index.html", domain.ManifestEntry{SHA: sha, Size: 7, CompressedSize: 5, ContentType: "text/html"})
	return manifest
}

// A redeploy authorized for one site incarnation cannot append to its replacement.
func TestSitesReplaceFencesReplacementIncarnation(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	inner := NewMemRepo()
	sites := NewSites(inner)
	initial := domain.Site{
		Slug: "site2345", Identity: "key:owner", Manifest: siteManifest("old-sha"),
		CreatedAt: at, UpdatedAt: at,
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
	replacement.ContentSHA = "replacement-sha"
	replacement.Manifest = siteManifest(replacement.ContentSHA)
	replacement.CreatedAt = at.Add(time.Second)
	replacement.UpdatedAt = replacement.CreatedAt
	repo := &generationSwapSiteRepo{MemRepo: inner, old: old, replacement: replacement}
	sites = NewSites(repo)

	update := initial
	update.Manifest = siteManifest("update-sha")
	update.UpdatedAt = at.Add(2 * time.Second)
	if err := sites.ReplaceWithQuotaCheck(context.Background(), update, 5, 0, update.UpdatedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace error = %v, want ErrNotFound", err)
	}
	got, err := inner.Get(initial.Slug)
	if err != nil {
		t.Fatalf("get replacement: %v", err)
	}
	if got.Generation != replacement.Generation || got.ContentSHA != replacement.ContentSHA || got.LatestVersion != 1 {
		t.Fatalf("replacement changed: got %+v", got)
	}
}
