// ShaleRepo's shared low-level helpers: the key builders (mirroring
// SlateRepo's key layout), the identity_pastes JSON projection row, the
// get/scan primitives (single-shard reads, prefix scans, the cross-shard
// aggregate), the version-set helpers, and the counter / JSON-envelope /
// tx codec helpers. Split out of shale_repo.go (which keeps the config,
// lifecycle, and core CRUD); see that file's package comment for the key
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

func shaleKeyPaste(slug domain.Slug) []byte { return []byte("pastes/" + slug.String()) }

func shaleKeyVersion(slug domain.Slug, verNum int) []byte {
	return fmt.Appendf(nil, "versions/%s/%04d", slug.String(), verNum)
}

func shalePrefixVersions(slug domain.Slug) []byte {
	return []byte("versions/" + slug.String() + "/")
}

func shaleKeySlugOwner(slug domain.Slug) []byte { return []byte("slug_owner/" + slug.String()) }

func shaleKeyIdentityPaste(identity, slug string) []byte {
	return []byte("identity_pastes/" + identity + "/" + slug)
}

func shalePrefixIdentityPastes(identity string) []byte {
	return []byte("identity_pastes/" + identity + "/")
}

func shaleKeyIdentityFirstSeen(identity string) []byte {
	return []byte("identity_first_seen/" + identity)
}

func shaleKeyExpiry(t time.Time, slug domain.Slug) []byte {
	return []byte("expiry/" + t.UTC().Format(time.RFC3339Nano) + "/" + slug.String())
}

func shaleKeyKeygate(subnet, identity string) []byte {
	return []byte("keygate/" + subnet + "/" + identity)
}

func shalePrefixKeygateSubnet(subnet string) []byte {
	return []byte("keygate/" + subnet + "/")
}

// markerValue is the non-empty placeholder for index families that
// carry no data of their own (the expiry index). shale's Put rejects
// empty values, so a one-byte marker stands in for SlateRepo's []byte{}.
var markerValue = []byte{'1'}

// PendingPasteTimeout is how old a status=pending paste may be before the
// reconciler ages it to failed (the pod-death backstop: its in-memory
// bytes never reached the blob store). It is comfortably longer than a
// healthy blob write (~250 ms) plus retries, so the reconciler never
// races a live finalizer, while short enough that a lost-bytes paste
// self-heals out of the loading screen within a sweep tick or two. A var,
// not a const, so tests can shrink it.
var PendingPasteTimeout = 2 * time.Minute

// --- JSON projections ------------------------------------------------------

// identityPasteRow is the value-bearing projection stored at
// identity_pastes/<id>/<slug>. Size caches the paste's LIVE byte sum (its
// non-deleted version sizes) and ExpiresAt its retention deadline: the
// quota scan sums exactly these two cached fields, one prefix scan with
// zero per-entry fan-out (docs/SPEC.md "Scan-derived quota"). The entry is
// derived (eventually consistent); every size-changing write path
// maintains it and the reconciler's reprojection rebuilds it from the
// authoritative pastes/* + versions/* rows, so cached-value error is
// bounded by a reconcile cycle.
type identityPasteRow struct {
	Name      string    `json:"name"`
	Size      int       `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	// Placeholder marks a fail-closed entry the reconciler projects for a
	// slug whose authoritative record (head or any version row) cannot be
	// decoded: the live sum is uncomputable, so instead of projecting a
	// partial number (a silent under-count) the entry carries this marker
	// and the quota scan HARD-FAILS on it (docs/SPEC.md "Decode tolerance
	// of the quota scan"). Cleared only when the record decodes again or
	// the row is removed (which for real corruption means an operator
	// repair or raw-key delete - Delete/DeleteVersion/the sweep decode the
	// same row, so no self-service path clears it; see the spec's operator
	// note). omitempty keeps ordinary entries byte-shaped as before the
	// field existed.
	Placeholder bool `json:"placeholder,omitempty"`
}

// --- generic helpers -------------------------------------------------------

// getJSON reads + JSON-decodes a value at key via routed Get. Returns
// storage.ErrNotFound when the key doesn't exist (translating shale's
// backend.ErrNotFound to the storage sentinel the callers + conformance
// suite expect).
func (r *ShaleRepo) getJSON(key []byte, out any) error {
	var raw []byte
	err := retryAcquiring(readRetry, func() error {
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
	// non-multi) path; the single-node R=1 backend read and the multi-backend
	// read hand back the RAW stored bytes. A record left enveloped by an R>1
	// write therefore reaches here wrapped, so strip before decoding.
	// Idempotent for R=1 raw / already-unwrapped values.
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
// getJSON it strips the LWW envelope cluster.Get leaves on at R=1 / multi
// (idempotent for raw values) so callers see the payload, not the wrapper.
func (r *ShaleRepo) getRaw(key []byte) ([]byte, error) {
	var raw []byte
	err := retryAcquiring(readRetry, func() error {
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

// scanPrefix collects all (key, value) pairs whose key starts with prefix
// from the OWNING shard, via the routed cluster.ScanPrefix. Used for
// single-shard list/prefix queries (a paste's versions, an owner's
// pastes, a subnet's keygate rows). For cross-shard scans use aggregate.
// The WHOLE scan is the retry unit, not just the ScanPrefix call: a unit can
// go mid-handoff partway through iteration, so an acquiring refusal surfaces
// from it.Next() as readily as from the open. Retrying the open alone would
// leave the mid-stream case unretried, and a half-consumed iterator is not a
// usable answer, so a retry restarts the scan from the beginning.
func (r *ShaleRepo) scanPrefix(prefix []byte) ([]scanItem, error) {
	var out []scanItem
	err := retryAcquiring(readRetry, func() error {
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
		// cluster.ScanPrefix, like the backend scan under aggregatePrefix,
		// returns the RAW stored value - an LWW envelope at R>1. Strip it so
		// consumers (keygate timestamps, version rows, owner pastes) decode
		// the payload exactly as cluster.Get hands it to them. Idempotent for
		// R=1 / pre-envelope values; a truncated envelope surfaces as an error.
		payload, derr := stripEnvelope(v)
		if derr != nil {
			return nil, fmt.Errorf("scan strip %s: %w", prefix, derr)
		}
		out = append(out, scanItem{
			Key:   append([]byte(nil), k...),
			Value: append([]byte(nil), payload...),
		})
	}
	return out, nil
}

// aggregatePrefix fans out across all shards (cluster.Aggregate) and
// collects every (key, value) whose key starts with prefix, deduplicating
// keys (a key may surface from more than one replica at R>1; the value is
// identical so last-writer-wins on the map is fine). Used for the three
// inherently cross-shard background operations: the expiry scan, the
// referenced-blob set, and keygate prune / counting.
// Retried as a WHOLE CALL, never per-peer: a refused peer's slice is absent
// from every other peer's result, so a partial fan-out is not a usable answer
// (shale guarantees a refusal is raised rather than silently skipped, on both
// the AggregateResult.Err channel and through the scan fn). backgroundRetry
// rather than readRetry because all three consumers - the expiry sweep, the
// referenced-blob set, keygate prune - are background, so no request deadline
// bounds them and they can afford to be patient. That patience is bought for
// the blob-GC consumer specifically: it acts on ABSENCE (deleting any blob NOT
// in the referenced set), so consuming a truncated set deletes live data,
// whereas the other two merely under-report.
func (r *ShaleRepo) aggregatePrefix(prefix []byte) ([]scanItem, error) {
	var out []scanItem
	err := retryAcquiring(backgroundRetry, func() error {
		var aerr error
		out, aerr = r.aggregatePrefixOnce(prefix)
		return aerr
	})
	return out, err
}

func (r *ShaleRepo) aggregatePrefixOnce(prefix []byte) ([]scanItem, error) {
	results := r.cluster.Aggregate(func(b backend.Backend) any {
		it, err := b.ScanPrefix(prefix)
		if err != nil {
			return err
		}
		defer it.Close() //nolint:errcheck
		var local []scanItem
		for {
			k, v, err := it.Next()
			if err != nil {
				return err
			}
			if k == nil && v == nil {
				break
			}
			// ScanPrefix returns the RAW backend value, which for an
			// R>1 write is an LWW Envelope (the cluster layer wraps on
			// Put and unwraps on Get, but the Backend - and therefore
			// this raw scan - sees the envelope). The aggregate
			// consumers (quota sum, blob-ref set, expiry sweep) expect
			// the decoded payload, exactly as cluster.Get hands them.
			// cluster.Decode is the universal strip: a magic-prefixed
			// envelope yields its payload; a pre-envelope / R=1 raw
			// value passes through unchanged (zero-Stamp payload). A
			// decode error means a truncated envelope (corruption) and
			// is surfaced, not swallowed.
			env, derr := cluster.Decode(v)
			if derr != nil {
				return fmt.Errorf("decode envelope for %q: %w", k, derr)
			}
			local = append(local, scanItem{
				Key:   append([]byte(nil), k...),
				Value: append([]byte(nil), env.Payload...),
			})
		}
		return local
	})

	seen := make(map[string]scanItem)
	for _, res := range results {
		if res.Err != nil {
			return nil, fmt.Errorf("aggregate %s: %w", prefix, res.Err)
		}
		switch v := res.Value.(type) {
		case error:
			return nil, fmt.Errorf("aggregate %s: %w", prefix, v)
		case []scanItem:
			for _, item := range v {
				seen[string(item.Key)] = item
			}
		case nil:
			// peer with no matching keys
		}
	}
	out := make([]scanItem, 0, len(seen))
	for _, item := range seen {
		out = append(out, item)
	}
	return out, nil
}

// --- version-set helpers ---------------------------------------------------

// scanVersions returns the version rows for a slug (decoded), scanned
// from the {slug} shard OUTSIDE any transaction.
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

// stripEnvelope returns the LWW-envelope payload of a RAW backend value.
// At R>1 every stored value is wrapped (magic + Stamp + payload); the cluster
// layer unwraps on cluster.Get/Delete, but the low-level paths - CAS tx.Get
// and the backend ScanPrefix used by aggregatePrefix - hand back the raw
// stored bytes. Any caller that DECODES such a raw read (parseCounter,
// json.Unmarshal, room-value length) must strip first or it will choke on the
// binary envelope header. Idempotent for R=1 / already-stripped values: bytes
// with no magic prefix pass through unchanged (cluster.Decode's v0.3-compat
// path). A truncated envelope is corruption and surfaces as an error.
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

// txGetCounter reads an integer counter value inside a CAS tx, recording the
// read-check. Absent reads as 0 (and an ExpectAbsent check). Used by the
// per-app room byte counter (the one counter the shale backend still keeps,
// because a room write is a strict single-shard CAS - see shale_room_repo.go);
// the per-identity paste/site quota is scan-derived, not counted.
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

// shaleTxGetJSON reads + decodes inside a CAS tx, returning storage.ErrNotFound
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
