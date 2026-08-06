// ShaleRepo's shared low-level helpers: the key builders (mirroring SlateRepo's
// key layout), the identity_pastes JSON projection row, the get/scan primitives
// (single-shard reads, prefix scans, the cross-shard aggregate), the version-set
// helpers, and the counter / JSON-envelope / tx codec helpers. shale_repo.go
// holds the config, lifecycle and core CRUD; see its package comment for the key
// layout and transaction model.

//go:build slatedb

package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
)

// --- key builders (mirror SlateRepo's layout) ------------------------------

// shaleKey joins a family prefix with the rest of a key. The prefix is always
// one of the vars shaleShardKey routes on, never a re-spelled literal: a key
// whose family the router does not recognise falls through to the whole-key
// fallback, which scatters that family across shards and silently turns every
// single-shard CAS it depends on into a cross-shard write. Binding both sides
// to one var is what makes a typo a compile error instead.
func shaleKey(prefix []byte, parts ...string) []byte {
	n := len(prefix)
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	out = append(out, prefix...)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func shaleKeyPaste(slug domain.Slug) []byte { return shaleKey(prefixPastes, slug.String()) }

func shaleKeyVersion(slug domain.Slug, verNum int) []byte {
	return fmt.Appendf(shalePrefixVersions(slug), "%04d", verNum)
}

func shalePrefixVersions(slug domain.Slug) []byte {
	return shaleKey(prefixVersionsAll, slug.String(), "/")
}

func shaleKeySlugOwner(slug domain.Slug) []byte { return shaleKey(prefixSlugOwner, slug.String()) }

func shaleKeyIdentityPaste(identity, slug string) []byte {
	return shaleKey(prefixIdentityPastesAll, identity, "/", slug)
}

func shalePrefixIdentityPastes(identity string) []byte {
	return shaleKey(prefixIdentityPastesAll, identity, "/")
}

func shaleKeyIdentityFirstSeen(identity string) []byte {
	return shaleKey(prefixIdentityFirstSeenAll, identity)
}

func shaleKeyKeygate(subnet, identity string) []byte {
	return shaleKey(prefixKeygateAll, subnet, "/", identity)
}

func shalePrefixKeygateSubnet(subnet string) []byte {
	return shaleKey(prefixKeygateAll, subnet, "/")
}

// Prefix form of the identity-leading index key. It lives in this build-tagged
// file because only the tagged scan paths use it.
func shalePrefixKeygateIdentity(identity string) []byte {
	return shaleKey(prefixKeygateIdentityAll, identity, "/")
}

// --- JSON projections ------------------------------------------------------

// toDomain renders a listing row from the cached entry alone - the whole point
// of the entry being value-bearing. Fields the listing does not show (the
// content sha, the blob id) stay zero: a caller needing those reads the
// authoritative row.
// renderable reports whether the entry carries everything a list row needs, so
// the listing can skip reading the authoritative row. A zero Kind or ServedSize
// marks an entry written before that field existed; those take the upgrade path
// once and are rewritten complete. An empty paste is not creatable, so a zero
// ServedSize is unambiguous.
func (e identityPasteRow) renderable() bool {
	return e.Kind != "" && e.ServedSize > 0 && !e.Placeholder
}

func (e identityPasteRow) toDomain(slug domain.Slug, owner string) domain.Paste {
	return domain.Paste{
		Slug:          slug,
		Identity:      domain.Identity(owner),
		Status:        domain.PasteStatusReady,
		Kind:          domain.ContentKind(e.Kind),
		Size:          e.ServedSize,
		StoredBytes:   e.Size,
		Name:          e.Name,
		PinnedVersion: e.PinnedVersion,
		LatestVersion: e.LatestVersion,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
	}
}

// identityPasteRow is the value-bearing projection stored at
// identity_pastes/<id>/<slug>. Size caches the paste's LIVE byte sum (its
// non-deleted version sizes), so the quota scan sums exactly that cached field
// in one prefix scan with zero per-entry fan-out (docs/SPEC.md "Scan-derived
// quota"). The entry is derived
// and eventually consistent: every size-changing write path maintains it and the
// reconciler rebuilds it from the authoritative pastes/* + versions/* rows, so
// cached-value error is bounded by the owner's next read.
type identityPasteRow struct {
	Name      string    `json:"name"`
	Size      int       `json:"size"`
	CreatedAt time.Time `json:"created_at"`

	// Kind, LatestVersion and PinnedVersion are the rest of what a `list` row
	// renders. With them here the listing needs NO per-item read, so its
	// latency is flat in the number of pastes an owner holds rather than one
	// routed round-trip per paste (docs/SPEC.md "Listing is O(1) reads").
	//
	// UpdatedAt is the list's sort key, cached for the same reason.
	//
	// A zero Kind marks an entry written before these fields existed; the
	// listing falls back to reading that one row, so an old entry keeps
	// rendering correctly until its next write refreshes it.
	Kind          string `json:"kind,omitempty"`
	LatestVersion int    `json:"latest_version,omitempty"`

	// ServedSize is the HEAD version's own size, distinct from Size (the live
	// sum across every non-deleted version). The list shows both: the served
	// bytes, and the larger total the quota actually charges. Deriving one
	// from the other is impossible, so both are cached.
	ServedSize    int       `json:"served_size,omitempty"`
	PinnedVersion int       `json:"pinned_version,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`

	// Placeholder marks a fail-closed entry for a slug whose authoritative
	// record (head or any version row) cannot be decoded:
	// the live sum is uncomputable, so rather than project a partial number (a
	// silent under-count) the entry carries this marker and the quota scan
	// HARD-FAILS on it (docs/SPEC.md "Decode tolerance of the quota scan"). It
	// clears on the owner's next list once the record decodes again; real
	// corruption therefore needs an operator repair or raw-key delete, since
	// Delete/DeleteVersion/the sweep all decode that same row. omitempty keeps
	// ordinary entries byte-shaped.
	Placeholder bool `json:"placeholder,omitempty"`
}

// --- generic helpers -------------------------------------------------------

// getJSON reads and JSON-decodes the value at key via routed Get, translating
// shale's backend.ErrNotFound to the storage.ErrNotFound sentinel callers and
// the conformance suite expect.
func (r *ShaleRepo) getJSON(key []byte, out any) error {
	var raw []byte
	err := retryAcquiring(readRetry, r.repoLog(), "get", func() error {
		var gerr error
		raw, gerr = r.cluster.Get(key)
		return gerr
	})
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("get %s: %w", key, err)
	}
	// cluster.Get only unwraps the LWW envelope on the getReplicated (R>1,
	// non-multi) path; the R=1 backend read and the multi-backend read hand
	// back RAW stored bytes, so a record enveloped by an R>1 write arrives here
	// still wrapped. Stripping is idempotent for unwrapped values.
	payload, serr := stripEnvelope(raw)
	if serr != nil {
		return fmt.Errorf("strip %s: %w", key, serr)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	return nil
}

// getRaw reads a value via routed Get, returning (nil, nil) when absent. Like
// getJSON it strips the LWW envelope cluster.Get leaves on at R=1 / multi, so
// callers see the payload rather than the wrapper.
func (r *ShaleRepo) getRaw(key []byte) ([]byte, error) {
	var raw []byte
	err := retryAcquiring(readRetry, r.repoLog(), "get", func() error {
		var gerr error
		raw, gerr = r.cluster.Get(key)
		return gerr
	})
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	payload, serr := stripEnvelope(raw)
	if serr != nil {
		return nil, fmt.Errorf("strip %s: %w", key, serr)
	}
	return payload, nil
}

// scanPrefix collects every (key, value) under prefix from the OWNING shard via
// the routed cluster.ScanPrefix. Used for single-shard list/prefix queries (a
// paste's versions, an owner's pastes, a subnet's keygate rows); cross-shard
// scans go through the aggregate helpers.
//
// The retry unit is the WHOLE scan, not just the ScanPrefix call: a unit can go
// mid-handoff partway through iteration, so an acquiring refusal surfaces from
// it.Next() as readily as from the open. A half-consumed iterator is not a
// usable answer, so a retry restarts from the beginning.
func (r *ShaleRepo) scanPrefix(prefix []byte) ([]scanItem, error) {
	var out []scanItem
	err := retryAcquiring(readRetry, r.repoLog(), "scan-prefix", func() error {
		var serr error
		out, serr = r.scanPrefixOnce(prefix)
		return serr
	})
	return out, err
}

func (r *ShaleRepo) scanPrefixOnce(prefix []byte) ([]scanItem, error) {
	it, err := r.cluster.ScanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("scan prefix %s: %w", prefix, err)
	}
	defer it.Close() //nolint:errcheck
	var out []scanItem
	for {
		k, v, err := it.Next()
		if err != nil {
			return nil, fmt.Errorf("scan next %s: %w", prefix, err)
		}
		if k == nil && v == nil {
			break
		}
		// cluster.ScanPrefix returns the RAW stored value, an LWW envelope at
		// R>1. Stripping lets consumers (keygate timestamps, version rows,
		// owner pastes) decode the payload exactly as cluster.Get hands it to
		// them. Idempotent for unenveloped values; a truncated envelope is
		// corruption and surfaces as an error.
		env, derr := cluster.Decode(v)
		if derr != nil {
			return nil, fmt.Errorf("scan strip %s: %w", prefix, derr)
		}
		if isTombstoneEnvelope(env) {
			// A DELETED key, not a row. Skipping is what makes this scan agree
			// with cluster.Get, which reports not-found for a winning
			// tombstone; without it every delete leaves a permanent phantom
			// that decodes as corrupt. A bare empty marker is NOT a tombstone
			// and is deliberately kept: see isTombstoneEnvelope.
			continue
		}
		out = append(out, scanItem{
			Key:   append([]byte(nil), k...),
			Value: append([]byte(nil), env.Payload...),
		})
	}
	return out, nil
}

// --- version-set helpers ---------------------------------------------------

// scanVersions returns a slug's decoded version rows, scanned from the {slug}
// shard OUTSIDE any transaction.
func (r *ShaleRepo) scanVersions(slug domain.Slug) ([]versionRow, error) {
	items, err := r.scanPrefix(shalePrefixVersions(slug))
	if err != nil {
		return nil, err
	}
	out := make([]versionRow, 0, len(items))
	for _, item := range items {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// maxVerNum returns the highest version number across the supplied rows
// INCLUDING tombstones (version numbers are never reused). 0 for none.
func maxVerNum(versions []versionRow) int {
	max := 0
	for _, v := range versions {
		if v.VerNum > max {
			max = v.VerNum
		}
	}
	return max
}

// latestActiveVerNum returns the highest NON-deleted version number, or 1
// when none are live (matches sqlite COALESCE(..., 1)).
func latestActiveVerNum(versions []versionRow) int {
	latest := 0
	for _, v := range versions {
		if v.Deleted {
			continue
		}
		if v.VerNum > latest {
			latest = v.VerNum
		}
	}
	if latest == 0 {
		latest = 1
	}
	return latest
}

// --- counter helpers -------------------------------------------------------

// stripEnvelope returns the LWW-envelope payload of a RAW backend value. At R>1
// every stored value is wrapped (magic + Stamp + payload); the cluster layer
// unwraps on cluster.Get/Delete, but the low-level paths (CAS tx.Get and the
// backend ScanPrefix under the aggregate) hand back the raw stored bytes. Any
// caller that DECODES such a read (parseCounter, json.Unmarshal, room-value
// length) must strip first or choke on the binary envelope header. Idempotent
// for already-stripped values: bytes with no magic prefix pass through
// unchanged. A truncated envelope is corruption and surfaces as an error.
func stripEnvelope(raw []byte) ([]byte, error) {
	env, err := cluster.Decode(raw)
	if err != nil {
		return nil, err
	}
	return env.Payload, nil
}

func parseCounter(raw []byte) (int64, error) {
	payload, err := stripEnvelope(raw)
	if err != nil {
		return 0, fmt.Errorf("decode counter envelope: %w", err)
	}
	if len(payload) == 0 {
		return 0, nil // absent or an empty-payload (tombstone) envelope
	}
	n, err := strconv.ParseInt(string(payload), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode counter: %w", err)
	}
	return n, nil
}

func formatCounter(n int64) []byte {
	if n < 0 {
		n = 0 // the counter never goes negative; clamp defensively
	}
	return []byte(strconv.FormatInt(n, 10))
}

// txGetCounter reads an integer counter inside a CAS tx, recording the
// read-check. Absent reads as 0 (and an ExpectAbsent check). The per-app room
// byte counter is the only counter the shale backend keeps, since a room write
// is a strict single-shard CAS (see shale_room_repo.go); the per-identity
// paste/site quota is scan-derived instead.
func txGetCounter(tx shaleKVTx, key []byte) (int64, error) {
	raw, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("tx get counter: %w", err)
	}
	return parseCounter(raw)
}

// shaleTxGetJSON reads and decodes inside a CAS tx, returning storage.ErrNotFound
// when absent so the closure can branch on existence.
func shaleTxGetJSON(tx shaleKVTx, key []byte, out any) error {
	raw, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("tx get %s: %w", key, err)
	}
	payload, err := stripEnvelope(raw)
	if err != nil {
		return fmt.Errorf("tx strip %s: %w", key, err)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("tx decode %s: %w", key, err)
	}
	return nil
}

// shaleTxPutJSON encodes + buffers a Put inside a CAS tx.
func shaleTxPutJSON(tx shaleKVTx, key []byte, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	return tx.Put(key, body)
}
