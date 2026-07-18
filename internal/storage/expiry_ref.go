//go:build slatedb

package storage

// Shared expiry-index helpers for the index-backed metadata backends
// (slatedb, shale). Both keep the paste expiry index under the
// "expiry/<ts>/<slug>" key shape, the site index under
// "expiry_sites/<ts>/<slug>", and the room index under
// "roomexpiry/<ts>/<app-slug>/<uuid>". The Expired* scans and the
// DeleteExpired* cascades on both backends share one generic shape
// (scanExpiredRefs / deleteExpiredRef below), differing only in the
// prefix, the key parser, the record fetch, and the cascade; the
// checkedIndexKey family is the fail-closed gate between an opaque
// IndexRef and the raw entry key those cascades remove.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// --- expiry-index scan (Expired* family) -------------------------------------

// scanExpiredRefs scans one expiry-index family and returns a typed ref for
// every entry whose timestamp segment is at or before now (inclusive
// boundary), preserving scan order. scan is the backend's prefix scanner
// (SlateRepo.scanPrefix or ShaleRepo.aggregatePrefix); parse extracts the
// timestamp segment and builds the typed ref from an entry's full key
// (ok=false skips a malformed key).
//
// The timestamp layout is a PARAMETER on purpose: the paste family still
// writes variable-width time.RFC3339Nano timestamps while the site + room
// families use the fixed-width expirySiteTimeFormat, and the cutoff must be
// formatted with the SAME layout its family's keys use for the string
// compare to stay aligned. Unifying the paste keys onto the fixed-width
// layout is a separate, explicitly-deferred key-format migration; do NOT
// fold the formats here.
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

// parseExpiredPasteKey parses one "expiry/<rfc3339>/<slug>" entry key into
// its timestamp segment plus the typed ref (IndexRef = the full key).
func parseExpiredPasteKey(k string) (string, domain.ExpiredPaste, bool) {
	ts, slug, ok := splitExpiryKey(k, "expiry/")
	if !ok {
		return "", domain.ExpiredPaste{}, false
	}
	return ts, domain.ExpiredPaste{Slug: domain.Slug(slug), IndexRef: k}, true
}

// parseExpiredSiteKey parses one "expiry_sites/<ts>/<slug>" entry key into
// its timestamp segment plus the typed ref (IndexRef = the full key).
func parseExpiredSiteKey(k string) (string, domain.ExpiredSite, bool) {
	ts, slug, ok := splitExpiryKey(k, "expiry_sites/")
	if !ok {
		return "", domain.ExpiredSite{}, false
	}
	return ts, domain.ExpiredSite{Slug: domain.Slug(slug), IndexRef: k}, true
}

// parseExpiredRoomKey parses one "roomexpiry/<ts>/<app-slug>/<uuid>" entry
// key into its timestamp segment plus the typed ref (IndexRef = the full
// key). The room subject is TWO trailing segments (slug + uuid, both
// slash-free), unlike the paste/site single-slug suffix, and the <ts> is
// fixed-width (no '/'), so two Cuts split it exactly.
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

// splitExpiryKey splits a "<family><ts>/<slug>" entry key into its timestamp
// and slug segments on the LAST '/' (slugs are slash-free; a paste-family
// RFC3339 timestamp may itself contain no '/' either, but LastIndex keeps
// the split correct regardless).
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
// index-backed backends: validate the ref's index-entry key (fail-closed;
// entryKeyOf is one of the expiry*IndexKey validators), fetch the
// authoritative record, run the full cascade delete when the record is
// still live (an ErrNotFound record is an orphaned entry: nothing to
// cascade), and then - in EVERY case - drop the exact index entry the scan
// surfaced via dropEntry (the cascade removes the DERIVED key; this removes
// the OBSERVED one, which is what keeps an orphaned or drifted entry from
// resurfacing on every scan forever). Idempotent: a missing record and a
// missing entry are both no-ops. Returns whether a record was actually
// deleted - the honest-vs-orphan accounting the sweep's counters rely on.
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
