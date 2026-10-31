package service

// Delete's heal decision (docs/SPEC.md "Delete heals its own lost tail"). The
// end-to-end version against the real repo lives with the storage tests,
// which own the raw seams that plant the damage.

import (
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// strandedIndex holds owner index entries with no paste row behind them, as a
// crash between delete's two transactions leaves.
type strandedIndex struct {
	PasteAdmin
	entries map[string]map[domain.Slug]bool
	dropErr error
}

func (s *strandedIndex) Get(domain.Slug) (domain.Paste, error) {
	return domain.Paste{}, ErrNotFound
}

func (s *strandedIndex) DropStaleOwnerEntry(slug domain.Slug, owner string) (bool, error) {
	if s.dropErr != nil {
		return false, s.dropErr
	}
	if !s.entries[owner][slug] {
		return false, nil
	}
	delete(s.entries[owner], slug)
	return true, nil
}

func TestDelete_HealDecisionFlow(t *testing.T) {
	const owner, other = "key:heal-flow", "key:heal-other"
	slug := domain.Slug("healflow")

	t.Run("the caller's residue is dropped and the delete succeeds", func(t *testing.T) {
		idx := &strandedIndex{entries: map[string]map[domain.Slug]bool{owner: {slug: true}}}
		m := &Manage{Repo: idx}
		if err := m.Delete(slug, ownerOf(owner)); err != nil {
			t.Fatalf("delete = %v, want nil", err)
		}
		if idx.entries[owner][slug] {
			t.Fatal("residue still indexed after the delete")
		}
		if err := m.Delete(slug, ownerOf(owner)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("another owner's residue stays not-found and untouched", func(t *testing.T) {
		idx := &strandedIndex{entries: map[string]map[domain.Slug]bool{other: {slug: true}}}
		m := &Manage{Repo: idx}
		if err := m.Delete(slug, ownerOf(owner)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete = %v, want ErrNotFound", err)
		}
		if !idx.entries[other][slug] {
			t.Fatal("dropped another owner's index entry")
		}
	})

	t.Run("heal failure surfaces, not swallowed", func(t *testing.T) {
		boom := errors.New("cas conflict")
		m := &Manage{Repo: &strandedIndex{dropErr: boom}}
		err := m.Delete(slug, ownerOf(owner))
		if !errors.Is(err, boom) {
			t.Fatalf("delete = %v, want the heal error", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatal("a failed heal must not masquerade as not-found")
		}
	})
}
