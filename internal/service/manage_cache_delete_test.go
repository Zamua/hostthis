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
