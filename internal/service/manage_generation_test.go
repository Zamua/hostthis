package service

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

type generationSwapRepo struct {
	*storage.MemRepo
	swap func()
}

func (r *generationSwapRepo) runSwap() {
	if r.swap != nil {
		swap := r.swap
		r.swap = nil
		swap()
	}
}

func (r *generationSwapRepo) AppendVersionWithQuotaCheck(ctx context.Context, slug domain.Slug, generation string,
	kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time,
) (domain.AppendResult, error) {
	r.runSwap()
	return r.MemRepo.AppendVersionWithQuotaCheck(ctx, slug, generation, kind, contentSHA, size, userCap, now)
}

func (r *generationSwapRepo) SetPinnedVersion(slug domain.Slug, generation string, version domain.Version) error {
	r.runSwap()
	return r.MemRepo.SetPinnedVersion(slug, generation, version)
}

func (r *generationSwapRepo) Unpin(slug domain.Slug, generation string) error {
	r.runSwap()
	return r.MemRepo.Unpin(slug, generation)
}

func (r *generationSwapRepo) DeleteVersion(slug domain.Slug, generation string, version int) error {
	r.runSwap()
	return r.MemRepo.DeleteVersion(slug, generation, version)
}

func seedGenerationPaste(t *testing.T, repo *storage.MemRepo) domain.Paste {
	t.Helper()
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	paste := domain.Paste{
		Slug:       "fence234",
		Generation: "old-generation",
		Identity:   "key:owner",
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindHTML,
		ContentSHA: "old-sha",
		Size:       10,
		CreatedAt:  at,
		UpdatedAt:  at,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), paste, 0, at); err != nil {
		t.Fatalf("insert old incarnation: %v", err)
	}
	return paste
}

func armGenerationReplacement(t *testing.T, repo *generationSwapRepo, old domain.Paste) domain.Paste {
	t.Helper()
	replacement := old
	replacement.Generation = "replacement-generation"
	replacement.ContentSHA = "replacement-sha"
	replacement.Size = 7
	replacement.CreatedAt = old.CreatedAt.Add(time.Second)
	replacement.UpdatedAt = replacement.CreatedAt
	replacement.PinnedVersion = 0
	repo.swap = func() {
		if err := repo.Delete(old.Slug, old.Identity, old.CreatedAt); err != nil {
			t.Fatalf("delete old incarnation: %v", err)
		}
		if err := repo.InsertWithQuotaCheck(context.Background(), replacement, 0, replacement.CreatedAt); err != nil {
			t.Fatalf("insert replacement: %v", err)
		}
	}
	return replacement
}

func assertReplacementUnchanged(t *testing.T, repo *storage.MemRepo, want domain.Paste) {
	t.Helper()
	got, err := repo.Get(want.Slug)
	if err != nil {
		t.Fatalf("get replacement: %v", err)
	}
	if got.Generation != want.Generation || got.ContentSHA != want.ContentSHA ||
		got.PinnedVersion != want.PinnedVersion || got.LatestVersion != 1 {
		t.Fatalf("replacement changed: got %+v, want generation=%q sha=%q pin=%d latest=1",
			got, want.Generation, want.ContentSHA, want.PinnedVersion)
	}
}

// Owner authorization for one incarnation cannot mutate its replacement.
func TestManageMutationsFenceReplacementIncarnation(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed prepares the old incarnation beyond the bare paste.
		seed func(t *testing.T, inner *storage.MemRepo, old domain.Paste)
		// mutate runs the verb under test as the old incarnation's owner.
		mutate func(m *Manage, old domain.Paste) error
	}{
		{"update", nil, func(m *Manage, old domain.Paste) error {
			_, err := m.Update(old.Slug, old.Identity.String(), bytes.NewBufferString("# update"), "")
			return err
		}},
		{"pin", nil, func(m *Manage, old domain.Paste) error {
			_, err := m.Pin(old.Slug, old.Identity.String(), 1)
			return err
		}},
		{"unpin", func(t *testing.T, inner *storage.MemRepo, old domain.Paste) {
			v1, err := inner.GetVersion(old.Slug, 1)
			if err != nil {
				t.Fatalf("get v1: %v", err)
			}
			if err := inner.SetPinnedVersion(old.Slug, old.Generation, v1); err != nil {
				t.Fatalf("seed pin: %v", err)
			}
		}, func(m *Manage, old domain.Paste) error {
			return m.Unpin(old.Slug, old.Identity.String())
		}},
		{"delete version", func(t *testing.T, inner *storage.MemRepo, old domain.Paste) {
			if _, err := inner.AppendVersionWithQuotaCheck(context.Background(), old.Slug, old.Generation,
				domain.KindHTML, "old-v2", 4, 0, old.UpdatedAt.Add(time.Second)); err != nil {
				t.Fatalf("append v2: %v", err)
			}
		}, func(m *Manage, old domain.Paste) error {
			_, err := m.DeleteVersion(old.Slug, old.Identity.String(), 1)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := storage.NewMemRepo()
			old := seedGenerationPaste(t, inner)
			if tc.seed != nil {
				tc.seed(t, inner, old)
			}
			repo := &generationSwapRepo{MemRepo: inner}
			replacement := armGenerationReplacement(t, repo, old)
			manage := NewManage(repo, NewStandaloneBlobUnit(newFakeBlobs()))

			if err := tc.mutate(manage, old); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("%s error = %v, want ErrNotFound", tc.name, err)
			}
			assertReplacementUnchanged(t, inner, replacement)
		})
	}
}
