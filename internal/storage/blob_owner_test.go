package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// A claim is stable for the duration of one upload: re-reading it does not
// invent a new epoch, or a writer would fence itself mid-upload.
func TestBlobOwnership_ClaimIsStable(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()
	slug := domain.Slug("fence123")

	first, err := repo.ClaimBlobOwnership(ctx, slug)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if first == 0 {
		t.Fatal("a claim must return a non-zero epoch; 0 is the never-claimed sentinel")
	}
	again, err := repo.ClaimBlobOwnership(ctx, slug)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if again != first {
		t.Fatalf("claim moved on its own: %d then %d. A writer whose epoch shifts "+
			"mid-upload fences itself and can never bind", first, again)
	}
}

// Fencing moves the epoch, and the direction matters: recovery must be able to
// invalidate a claim it did not issue.
func TestBlobOwnership_FenceInvalidatesTheClaim(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()
	slug := domain.Slug("fence456")

	claimed, err := repo.ClaimBlobOwnership(ctx, slug)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	fenced, err := repo.FenceBlobOwnership(ctx, slug)
	if err != nil {
		t.Fatalf("fence: %v", err)
	}
	if fenced == claimed {
		t.Fatalf("fence left the epoch at %d: a writer holding the old claim would "+
			"still bind, which is the race the fence exists to close", fenced)
	}
	if fenced < claimed {
		t.Fatalf("fence moved the epoch BACKWARD (%d -> %d): a later claim could "+
			"collide with an earlier one", claimed, fenced)
	}
}

// Fencing an unclaimed slug still produces a usable epoch, so recovery never
// depends on the writer having got far enough to claim.
func TestBlobOwnership_FenceWorksWithoutAClaim(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()

	fenced, err := repo.FenceBlobOwnership(context.Background(), "fence789")
	if err != nil {
		t.Fatalf("fence unclaimed: %v", err)
	}
	if fenced == 0 {
		t.Fatal("fencing an unclaimed slug must still advance past 0")
	}
	// And a claim afterwards does NOT hand back the fenced-out epoch.
	claimed, err := repo.ClaimBlobOwnership(ctx, "fence789")
	if err != nil {
		t.Fatalf("claim after fence: %v", err)
	}
	if claimed < fenced {
		t.Fatalf("claim after fence returned %d, below the fenced epoch %d: the new "+
			"attempt would be born already fenced, or worse, reuse the old identity",
			claimed, fenced)
	}
}

// The sentinel is matchable, because the caller must tell "abort, recovery took
// this over" apart from a transient it should retry.
func TestBlobOwnership_FencedSentinelIsMatchable(t *testing.T) {
	wrapped := errors.Join(errors.New("bind failed"), storage.ErrFenced)
	if !errors.Is(wrapped, storage.ErrFenced) {
		t.Fatal("ErrFenced must survive wrapping: a caller that cannot identify it " +
			"will retry a write that can only ever fail")
	}
}
