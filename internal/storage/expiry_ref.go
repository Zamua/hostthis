//go:build slatedb

package storage

// Shared expiry-index reference validation for the index-backed metadata
// backends (slatedb, shale). Both keep the paste expiry index under the
// "expiry/<ts>/<slug>" key shape and the site index under
// "expiry_sites/<ts>/<slug>", and the DeleteExpired / DeleteExpiredSite
// impls remove the exact entry the scan surfaced; these helpers are the
// fail-closed gate between the opaque IndexRef and that raw key.

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
	return checkedIndexKey("expiry/", ref.IndexRef, ref.Slug)
}

// expirySiteIndexKey is the site twin: validates that ref.IndexRef names
// an "expiry_sites/<ts>/<slug>" entry for ref.Slug, same fail-closed rules.
func expirySiteIndexKey(ref domain.ExpiredSite) ([]byte, error) {
	return checkedIndexKey("expiry_sites/", ref.IndexRef, ref.Slug)
}

// expiryRoomIndexKey is the room twin: validates that ref.IndexRef names a
// "roomexpiry/<ts>/<app-slug>/<uuid>" entry for ref's (AppSlug, ID) pair -
// the room subject is the TWO trailing segments, unlike the paste/site
// single-slug suffix - and returns it as a key, or nil when the ref carries
// no index entry. Same fail-closed rules: a non-empty ref that is malformed
// or names a different room is a wiring bug, and erroring beats deleting an
// arbitrary key.
func expiryRoomIndexKey(ref domain.ExpiredRoom) ([]byte, error) {
	if ref.IndexRef == "" {
		return nil, nil
	}
	suffix := "/" + ref.AppSlug.String() + "/" + ref.ID.String()
	if !strings.HasPrefix(ref.IndexRef, "roomexpiry/") || !strings.HasSuffix(ref.IndexRef, suffix) {
		return nil, fmt.Errorf("expiry index ref %q does not name a roomexpiry/ entry for room %s/%s", ref.IndexRef, ref.AppSlug, ref.ID)
	}
	return []byte(ref.IndexRef), nil
}

func checkedIndexKey(family, indexRef string, slug domain.Slug) ([]byte, error) {
	if indexRef == "" {
		return nil, nil
	}
	if !strings.HasPrefix(indexRef, family) || !strings.HasSuffix(indexRef, "/"+slug.String()) {
		return nil, fmt.Errorf("expiry index ref %q does not name a %s entry for slug %q", indexRef, family, slug)
	}
	return []byte(indexRef), nil
}
