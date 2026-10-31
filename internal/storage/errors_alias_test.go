package storage_test

import (
	"errors"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestSentinelAliases pins the domain-owned error vocabulary (docs/SPEC.md
// "The storage contract"): every storage.Err... sentinel matches its domain
// twin under errors.Is in both directions.
func TestSentinelAliases(t *testing.T) {
	cases := []struct {
		name    string
		storage error
		domain  error
	}{
		{"ErrNotFound", storage.ErrNotFound, domain.ErrNotFound},
		{"ErrSlugTaken", storage.ErrSlugTaken, domain.ErrSlugTaken},
		{"ErrOverUserQuota", storage.ErrOverUserQuota, domain.ErrOverUserQuota},
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
		})
	}
}
