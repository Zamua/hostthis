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
// (and legacy ==) matches through either name. It also pins the message
// text VERBATIM: sentinel messages reach user-facing output, so moving the
// definitions into domain must not change a byte of them.
func TestSentinelAliases(t *testing.T) {
	cases := []struct {
		name    string
		storage error
		domain  error
		msg     string
	}{
		{"ErrNotFound", storage.ErrNotFound, domain.ErrNotFound, "storage: not found"},
		{"ErrSlugTaken", storage.ErrSlugTaken, domain.ErrSlugTaken, "storage: slug already taken"},
		{"ErrOverUserQuota", storage.ErrOverUserQuota, domain.ErrOverUserQuota, "storage: would exceed user quota"},
		{"ErrServiceFull", storage.ErrServiceFull, domain.ErrServiceFull, "storage: service is at capacity"},
		{"ErrRoomDataFull", storage.ErrRoomDataFull, domain.ErrRoomDataFull, "storage: room is at its data cap"},
		{"ErrAppRoomsFull", storage.ErrAppRoomsFull, domain.ErrAppRoomsFull, "storage: app room storage is at capacity"},
		{"ErrTooManyNewKeys", storage.ErrTooManyNewKeys, domain.ErrTooManyNewKeys, "storage: too many new keys from this network"},
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
			if got := tc.domain.Error(); got != tc.msg {
				t.Fatalf("%s message = %q, want %q (verbatim; message text is user-facing contract)", tc.name, got, tc.msg)
			}
		})
	}
}
