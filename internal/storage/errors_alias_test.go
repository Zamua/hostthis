package storage_test

import (
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestSentinelAliases pins the domain-owned error vocabulary (docs/SPEC.md
// "The storage contract"): every storage.Err... sentinel is an ALIAS of its
// domain twin - the same error value, not a same-text copy - so errors.Is
// (and legacy ==) matches through either name.
func TestSentinelAliases(t *testing.T) {
	cases := []struct {
		name    string
		storage error
		domain  error
	}{
		{"ErrNotFound", storage.ErrNotFound, domain.ErrNotFound},
		{"ErrSlugTaken", storage.ErrSlugTaken, domain.ErrSlugTaken},
		{"ErrOverUserQuota", storage.ErrOverUserQuota, domain.ErrOverUserQuota},
		{"ErrServiceFull", storage.ErrServiceFull, domain.ErrServiceFull},
		{"ErrRoomDataFull", storage.ErrRoomDataFull, domain.ErrRoomDataFull},
		{"ErrAppRoomsFull", storage.ErrAppRoomsFull, domain.ErrAppRoomsFull},
		{"ErrTooManyNewKeys", storage.ErrTooManyNewKeys, domain.ErrTooManyNewKeys},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.storage, tc.domain) {
				t.Fatalf("errors.Is(storage.%s, domain.%s) = false; alias identity broken", tc.name, tc.name)
			}
			if !errors.Is(tc.domain, tc.storage) {
				t.Fatalf("errors.Is(domain.%s, storage.%s) = false; alias identity broken", tc.name, tc.name)
			}
			if tc.storage != tc.domain {
				t.Fatalf("storage.%s and domain.%s are distinct values; must be the SAME error", tc.name, tc.name)
			}
		})
	}
}
