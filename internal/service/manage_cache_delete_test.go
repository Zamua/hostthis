package service_test

import (
	"fmt"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// stubManager satisfies service.PasteManager by embedding it, so only the one
// method under test needs a body. Any other call would nil-panic, which is the
// point: it proves the decorator touched nothing else.
type stubManager struct {
	service.PasteManager
	err error
}

func (s stubManager) Delete(domain.Slug, string) error { return s.err }

// A delete purges unless the error proves the paste was never touched. The
// ambiguous middle - a removal that landed and then reported failure - is the
// case that leaves deleted content on the edge until max-age expires.
func TestDeletePurgesUnlessNothingWasTouched(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantPurge bool
	}{
		{"success", nil, true},
		{"transient storage error", assertErr("commit failed"), true},
		{"wrapped transient error", fmt.Errorf("delete: %w", assertErr("timeout")), true},

		// Pre-mutation rejections: no write was attempted, and purging here
		// would let any caller spend the CDN purge budget on slugs they do
		// not own.
		{"not found", service.ErrNotFound, false},
		{"not owner", service.ErrNotOwner, false},
		{"anonymous", service.ErrEmptyOwner, false},
		{"wrapped not owner", fmt.Errorf("check: %w", service.ErrNotOwner), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			purger := &recordingPurger{}
			mgr := service.NewCacheInvalidating(stubManager{err: tc.err}, purger)

			if got := mgr.Delete(domain.Slug("abc12345"), testOwner); got != tc.err {
				t.Fatalf("error passthrough: got %v, want %v", got, tc.err)
			}
			if gotPurge := len(purger.calls()) == 1; gotPurge != tc.wantPurge {
				t.Fatalf("purged=%v, want %v (err=%v)", gotPurge, tc.wantPurge, tc.err)
			}
		})
	}
}

// The stub test above proves the decorator's rule. This proves the rule matches
// the errors the REAL service actually produces, which is the half a stub
// cannot: if requireOwner ever stopped returning these sentinels, the exclusion
// would silently stop matching and unowned slugs would become purgeable.
func TestDeletePurgeExclusionsMatchRealServiceErrors(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	mgr := service.NewCacheInvalidating(manage, purger)

	t.Run("own paste purges", func(t *testing.T) {
		purger.reset()
		slug := newPaste(t, upload)
		if err := mgr.Delete(slug, testOwner); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("purge: got %v, want [%s]", got, slug)
		}
	})

	// Both of these must NOT purge, or attempting deletes becomes a way to
	// spend the CDN purge budget on slug you have no rights to.
	t.Run("absent slug does not purge", func(t *testing.T) {
		purger.reset()
		if err := mgr.Delete(domain.Slug("nosuchpp"), testOwner); err == nil {
			t.Fatal("want an error deleting an absent slug")
		}
		if got := purger.calls(); len(got) != 0 {
			t.Fatalf("absent slug purged %v; the exclusion no longer matches the real error", got)
		}
	})

	t.Run("another owner's paste does not purge", func(t *testing.T) {
		slug := newPaste(t, upload)
		purger.reset()
		if err := mgr.Delete(slug, "key:someone-else"); err == nil {
			t.Fatal("want an error deleting another owner's paste")
		}
		if got := purger.calls(); len(got) != 0 {
			t.Fatalf("other owner's slug purged %v; the exclusion no longer matches the real error", got)
		}
	})

	t.Run("anonymous does not purge", func(t *testing.T) {
		slug := newPaste(t, upload)
		purger.reset()
		if err := mgr.Delete(slug, ""); err == nil {
			t.Fatal("want an error deleting anonymously")
		}
		if got := purger.calls(); len(got) != 0 {
			t.Fatalf("anonymous delete purged %v", got)
		}
	})
}
