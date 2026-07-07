//go:build slatedb

package storage

// Shared expiry-index reference validation for the index-backed metadata
// backends (slatedb, shale). Both keep the paste expiry index under the
// same "expiry/<ts>/<slug>" key shape, and both DeleteExpired impls remove
// the exact entry the scan surfaced; this helper is the fail-closed gate
// between the opaque domain.ExpiredPaste.IndexRef and that raw key.

import (
	"fmt"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// expiryIndexKey validates that ref.IndexRef names an expiry-index entry
// for ref.Slug ("expiry/<ts>/<slug>") and returns it as a key, or nil when
// the ref carries no index entry. Fail-closed: a non-empty ref that is
// malformed or names a different slug is a wiring bug, and erroring beats
// deleting an arbitrary key.
func expiryIndexKey(ref domain.ExpiredPaste) ([]byte, error) {
	if ref.IndexRef == "" {
		return nil, nil
	}
	if !strings.HasPrefix(ref.IndexRef, "expiry/") || !strings.HasSuffix(ref.IndexRef, "/"+ref.Slug.String()) {
		return nil, fmt.Errorf("expiry index ref %q does not name an expiry entry for slug %q", ref.IndexRef, ref.Slug)
	}
	return []byte(ref.IndexRef), nil
}
