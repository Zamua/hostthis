package service

// Delete's heal decision (docs/SPEC.md "Delete heals its own lost tail"):
// requireOwner's not-found routes through DropStaleOwnerEntry exactly once;
// a drop means success, no drop means not-found, a heal error surfaces. The
// end-to-end version against the real repo lives with the storage tests
// (service_heal_e2e_test.go), which own the raw seams that plant the damage.

import (
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// healRepoStub provides ONLY Get (not found) and DropStaleOwnerEntry; the
// embedded nil PasteAdmin makes any other repo call panic, and the nil Blob
// on Manage makes any blob call panic, so a pass proves the heal path touches
// neither.
type healRepoStub struct {
	PasteAdmin
	dropped bool
	dropErr error
	calls   int
	gotSlug domain.Slug
	gotOwn  string
}

func (s *healRepoStub) Get(domain.Slug) (domain.Paste, error) {
	return domain.Paste{}, ErrNotFound
}

func (s *healRepoStub) DropStaleOwnerEntry(slug domain.Slug, owner string) (bool, error) {
	s.calls++
	s.gotSlug, s.gotOwn = slug, owner
	return s.dropped, s.dropErr
}

func TestDelete_HealDecisionFlow(t *testing.T) {
	const owner = "key:heal-flow"
	slug := domain.Slug("healflow")

	t.Run("dropped residue is success", func(t *testing.T) {
		stub := &healRepoStub{dropped: true}
		m := &Manage{Repo: stub}
		if err := m.Delete(slug, owner); err != nil {
			t.Fatalf("delete = %v, want nil", err)
		}
		if stub.calls != 1 || stub.gotSlug != slug || stub.gotOwn != owner {
			t.Fatalf("heal called %d times with (%s, %s), want once with (%s, %s)",
				stub.calls, stub.gotSlug, stub.gotOwn, slug, owner)
		}
	})

	t.Run("nothing to drop stays not-found", func(t *testing.T) {
		stub := &healRepoStub{dropped: false}
		m := &Manage{Repo: stub}
		if err := m.Delete(slug, owner); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete = %v, want ErrNotFound", err)
		}
		if stub.calls != 1 {
			t.Fatalf("heal calls = %d, want 1", stub.calls)
		}
	})

	t.Run("heal failure surfaces, not swallowed", func(t *testing.T) {
		boom := errors.New("cas conflict")
		stub := &healRepoStub{dropErr: boom}
		m := &Manage{Repo: stub}
		err := m.Delete(slug, owner)
		if !errors.Is(err, boom) {
			t.Fatalf("delete = %v, want the heal error", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatal("a failed heal must not masquerade as not-found")
		}
	})

	t.Run("anonymous caller never reaches the heal", func(t *testing.T) {
		stub := &healRepoStub{dropped: true}
		m := &Manage{Repo: stub}
		if err := m.Delete(slug, ""); !errors.Is(err, ErrEmptyOwner) {
			t.Fatalf("delete = %v, want ErrEmptyOwner", err)
		}
		if stub.calls != 0 {
			t.Fatalf("heal calls = %d, want 0", stub.calls)
		}
	})
}
