//go:build slatedb

package storage

// Shared expiry-index helpers for the index-backed metadata backends (slatedb,
// shale). Entry key shapes:
//
//	expiry/<ts>/<slug>                   pastes
//	expiry_sites/<ts>/<slug>             sites
//	roomexpiry/<ts>/<app-slug>/<uuid>    rooms
//
// The Expired* scans and DeleteExpired* cascades on both backends share one
// generic shape (scanExpiredRefs / deleteExpiredRef), differing only in the
// prefix, the key parser, the record fetch, and the cascade. The
// checkedIndexKey family is the fail-closed gate between an opaque IndexRef
// and the raw entry key those cascades remove.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// --- expiry-index scan (Expired* family) -------------------------------------

// scanExpiredRefs returns a typed ref for every entry of one expiry-index
// family whose timestamp segment is at or before now (inclusive boundary),
// preserving scan order. parse returns ok=false to skip a malformed key.
//
// layout is a PARAMETER on purpose: the paste family writes variable-width
// time.RFC3339Nano timestamps while the site and room families use the
// fixed-width expirySiteTimeFormat, and the cutoff must be formatted with the
// SAME layout its family's keys use or the string compare misaligns. Do NOT
// fold the formats here; unifying them is a key-format migration.
func scanExpiredRefs[T any](scan func(prefix []byte) ([]scanItem, error), prefix []byte, now time.Time, layout string, parse func(key string) (ts string, ref T, ok bool)) ([]T, error) {
	items, err := scan(prefix)
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(layout)
	var out []T
	for _, item := range items {
		ts, ref, ok := parse(string(item.Key))
		if !ok {
			continue
		}
		if ts <= cutoff {
			out = append(out, ref)
		}
	}
	return out, nil
}

// parseExpiredPasteKey splits "expiry/<rfc3339>/<slug>"; IndexRef is the full key.
func parseExpiredPasteKey(k string) (string, domain.ExpiredPaste, bool) {
	ts, slug, ok := splitExpiryKey(k, "expiry/")
	if !ok {
		return "", domain.ExpiredPaste{}, false
	}
	return ts, domain.ExpiredPaste{Slug: domain.Slug(slug), IndexRef: k}, true
}

// parseExpiredSiteKey splits "expiry_sites/<ts>/<slug>"; IndexRef is the full key.
func parseExpiredSiteKey(k string) (string, domain.ExpiredSite, bool) {
	ts, slug, ok := splitExpiryKey(k, "expiry_sites/")
	if !ok {
		return "", domain.ExpiredSite{}, false
	}
	return ts, domain.ExpiredSite{Slug: domain.Slug(slug), IndexRef: k}, true
}

// parseExpiredRoomKey splits "roomexpiry/<ts>/<app-slug>/<uuid>"; IndexRef is
// the full key. The room subject is TWO trailing segments (both slash-free),
// unlike the paste/site single-slug suffix, and <ts> is fixed-width with no
// '/', so two Cuts split it exactly.
func parseExpiredRoomKey(k string) (string, domain.ExpiredRoom, bool) {
	rest := strings.TrimPrefix(k, "roomexpiry/")
	ts, appAndID, ok := strings.Cut(rest, "/")
	if !ok {
		return "", domain.ExpiredRoom{}, false
	}
	app, id, ok := strings.Cut(appAndID, "/")
	if !ok {
		return "", domain.ExpiredRoom{}, false
	}
	return ts, domain.ExpiredRoom{AppSlug: domain.Slug(app), ID: domain.RoomID(id), IndexRef: k}, true
}

// splitExpiryKey splits "<family><ts>/<slug>" on the LAST '/': slugs are
// slash-free, so that boundary is exact whatever the timestamp contains.
func splitExpiryKey(k, family string) (ts, slug string, ok bool) {
	rest := strings.TrimPrefix(k, family)
	idx := strings.LastIndex(rest, "/")
	if idx < 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

// --- expired-ref delete (DeleteExpired* family) ------------------------------

// deleteExpiredRef is the shared shape of every DeleteExpired* on the
// index-backed backends: validate the ref's index-entry key (fail-closed),
// fetch the authoritative record, cascade-delete when it is still live (an
// ErrNotFound record is an orphaned entry, nothing to cascade), and in EVERY
// case drop the exact index entry the scan surfaced. That last step is the
// point: the cascade removes the DERIVED key, dropEntry removes the OBSERVED
// one, which is what stops an orphaned or drifted entry resurfacing on every
// scan forever. Idempotent. Reports whether a record was actually deleted, the
// honest-vs-orphan accounting the sweep's counters rely on.
func deleteExpiredRef[R any](ref R, entryKeyOf func(R) ([]byte, error), fetch func() error, cascade func() error, dropEntry func(entryKey []byte) error) (bool, error) {
	entryKey, err := entryKeyOf(ref)
	if err != nil {
		return false, err
	}
	deleted := false
	switch err := fetch(); {
	case errors.Is(err, ErrNotFound):
		// Orphaned entry: nothing to cascade, just clean the entry below.
	case err != nil:
		return false, err
	default:
		if err := cascade(); err != nil {
			return false, err
		}
		deleted = true
	}
	if entryKey != nil {
		if err := dropEntry(entryKey); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// --- IndexRef validation ------------------------------------------------------

// expiryIndexKey returns ref.IndexRef as a key once it is confirmed to name an
// "expiry/<ts>/<slug>" entry for ref.Slug, or nil when the ref carries no index
// entry. Fail-closed: a non-empty ref that is malformed or names a different
// slug is a wiring bug, and erroring beats deleting an arbitrary key.
func expiryIndexKey(ref domain.ExpiredPaste) ([]byte, error) {
	return checkedIndexKey("expiry/", ref.IndexRef, ref.Slug)
}

// expirySiteIndexKey is the site twin, over "expiry_sites/<ts>/<slug>".
func expirySiteIndexKey(ref domain.ExpiredSite) ([]byte, error) {
	return checkedIndexKey("expiry_sites/", ref.IndexRef, ref.Slug)
}

// expiryRoomIndexKey is the room twin, over "roomexpiry/<ts>/<app-slug>/<uuid>".
// It cannot reuse checkedIndexKey: the room subject is the TWO trailing
// segments, not a single slug.
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
