package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

type collisionSiteRepo struct {
	collisions int
	inserts    []domain.Slug
}

func (r *collisionSiteRepo) InsertWithQuotaCheck(_ context.Context, site domain.Site, _ int, _ int64, _ time.Time) error {
	r.inserts = append(r.inserts, site.Slug)
	if len(r.inserts) <= r.collisions {
		return domain.ErrSlugTaken
	}
	return nil
}

func (*collisionSiteRepo) ReplaceWithQuotaCheck(context.Context, domain.Site, int, int64, time.Time) error {
	return errors.New("unused")
}

func (*collisionSiteRepo) Get(domain.Slug) (domain.Site, error) {
	return domain.Site{}, domain.ErrNotFound
}

func (*collisionSiteRepo) Delete(domain.Slug, domain.Identity, time.Time) error { return nil }
func (*collisionSiteRepo) SumActiveBytesByOwner(string, time.Time) (int64, error) {
	return 0, nil
}
func (*collisionSiteRepo) ListSitesByOwner(string, time.Time) ([]domain.Site, error) {
	return nil, nil
}

type zeroPasteBytes struct{}

func (zeroPasteBytes) SumActiveBytesByOwner(string, time.Time) (int, error) { return 0, nil }

type countingBlobUnit struct{ stages int }

func (*countingBlobUnit) StagePrecompressed(context.Context, string, io.Reader, int64) error {
	return errors.New("unused")
}

func (b *countingBlobUnit) StageEncoding(_ context.Context, r io.Reader) (string, int, error) {
	b.stages++
	n, err := io.Copy(io.Discard, r)
	return "sha", int(n), err
}

func (*countingBlobUnit) Read(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, errors.New("unused")
}

// A slug collision retries metadata only; the one-shot archive is staged once.
func TestDeploySite_SlugCollisionDoesNotRestageArchive(t *testing.T) {
	repo := &collisionSiteRepo{collisions: 1}
	blobs := &countingBlobUnit{}
	deploy := NewDeploySite(repo, zeroPasteBytes{}, blobs)
	archive := gzipTar(t, map[string]string{"index.html": "<h1>hi</h1>"})

	result, err := deploy.Deploy(bytes.NewReader(archive), "key:owner")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if blobs.stages != 1 {
		t.Fatalf("blob stages = %d, want 1", blobs.stages)
	}
	if len(repo.inserts) != 2 {
		t.Fatalf("metadata inserts = %d, want 2", len(repo.inserts))
	}
	if repo.inserts[0] == repo.inserts[1] {
		t.Fatalf("collision reused slug %q", repo.inserts[0])
	}
	if result.Site.Slug != repo.inserts[1] {
		t.Fatalf("result slug = %q, want committed slug %q", result.Site.Slug, repo.inserts[1])
	}
}

// Exhausting the collision budget still stages the one-shot archive once.
func TestDeploySite_SlugCollisionBudgetDoesNotRestageArchive(t *testing.T) {
	repo := &collisionSiteRepo{collisions: maxDeployRetries}
	blobs := &countingBlobUnit{}
	deploy := NewDeploySite(repo, zeroPasteBytes{}, blobs)
	archive := gzipTar(t, map[string]string{"index.html": "<h1>hi</h1>"})

	_, err := deploy.Deploy(bytes.NewReader(archive), "key:owner")
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("deploy error = %v, want ErrSlugTaken", err)
	}
	if blobs.stages != 1 {
		t.Fatalf("blob stages = %d, want 1", blobs.stages)
	}
	if len(repo.inserts) != maxDeployRetries {
		t.Fatalf("metadata inserts = %d, want %d", len(repo.inserts), maxDeployRetries)
	}
}
