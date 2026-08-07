// Package storage's shale-cluster-backed room (app-persistence) KV store.
//
// Key names and JSON schema match the slatedb room repo, so the on-wire record
// is identical across both slatedb-tagged backends. Co-location is by
// shaleShardKey, not by renaming keys.
//
// # Key layout (all sharded on {app-slug})
//
//	rooms/<app-slug>/<uuid>                     JSON record, carries byte_total + key_count
//	roomkv/<app-slug>/<uuid>/<key>              raw value bytes, sentinel-prefixed
//	roomcreate/<app-slug>/<subnet>/<ts>/<uuid>  creation ledger marker
//	roombytes/<app-slug>                        per-app byte counter
//
// Everything an app owns co-locates on one shard, so loading a room, counting
// its creations and checking the per-app cap are all single-shard. The room
// repo never fans out on the hot path.
//
// # Both caps are strict
//
// A CAS read-set is discrete key checks and ScanPrefix is not allowed inside a
// transaction, so a cap whose magnitude is "the sum over a key range" needs a
// discrete counter the read-set can carry. The two caps use different members:
//
//   - Per-APP: roombytes/<app> is read-checked and incremented in the same CAS,
//     so two concurrent same-app writers cannot both pass a stale sum.
//   - Per-ROOM: byte_total + key_count live ON the room record and are validated
//     inside the CAS. That record is already in the read-set and is rewritten on
//     every put, so two writers to DISTINCT keys of one room still conflict on
//     it and the loser retries against updated totals.
//
// Every room key co-shards, so a put is a single-shard
// read-check-validate-write with no reserve/confirm split.
package storage

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

// ShaleRoomRepo is the service.RoomRepo + service.SweepRooms adapter over a
// ShaleRepo, delegating to its `...Room` methods so the room repo shares one
// cluster handle (and shard routing) with the paste + site repos.
type ShaleRoomRepo struct {
	repo *ShaleRepo
}

// NewShaleRoomRepo wraps a ShaleRepo so the no-auth room-persistence tier runs
// on the shale backend.
func NewShaleRoomRepo(repo *ShaleRepo) *ShaleRoomRepo { return &ShaleRoomRepo{repo: repo} }

// service.RoomRepo
func (s *ShaleRoomRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	return s.repo.CreateRoom(room, subnet, appCap, now)
}
func (s *ShaleRoomRepo) GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error) {
	return s.repo.GetRoom(appSlug, id)
}
func (s *ShaleRoomRepo) GetValue(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	return s.repo.GetRoomValue(appSlug, id, key)
}
func (s *ShaleRoomRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	return s.repo.ScanRoom(appSlug, id)
}
func (s *ShaleRoomRepo) PutValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap int64, now time.Time) (uint64, error) {
	return s.repo.PutRoomValue(appSlug, id, key, val, appCap, now)
}
func (s *ShaleRoomRepo) DeleteValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	return s.repo.DeleteRoomValue(appSlug, id, key, now)
}
func (s *ShaleRoomRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	return s.repo.CountRoomCreates(appSlug, subnet, now, window)
}

// --- key builders (mirror the slatedb room layout; sharded on {app-slug}) ---

func shaleKeyRoom(appSlug domain.Slug, id domain.RoomID) []byte {
	return shaleKey(prefixRooms, appSlug.String(), "/", id.String())
}

func shaleKeyRoomValue(appSlug domain.Slug, id domain.RoomID, key string) []byte {
	return shaleKey(prefixRoomKV, appSlug.String(), "/", id.String(), "/", key)
}

func shalePrefixRoomValues(appSlug domain.Slug, id domain.RoomID) []byte {
	return shaleKey(prefixRoomKV, appSlug.String(), "/", id.String(), "/")
}

// shaleKeyRoomCreate is the creation-ledger marker. The room id trails so two
// rooms created under the SAME (app, subnet) at the SAME timestamp get DISTINCT
// keys; a shared key would overwrite and undercount the rate limit. The <ts> is
// fixed-width and a windowed compare reads it as the second-to-last segment.
func shaleKeyRoomCreate(appSlug domain.Slug, subnet string, id domain.RoomID, t time.Time) []byte {
	return shaleKey(prefixRoomCreate, appSlug.String(), "/", subnet, "/",
		t.UTC().Format(sortableTimeFormat), "/", id.String())
}

func shalePrefixAppRoomCreates(appSlug domain.Slug) []byte {
	return shaleKey(prefixRoomCreate, appSlug.String(), "/")
}

// shaleKeyRoomBytes is the per-app room-byte counter. It co-shards with every
// room family on {app-slug}, so a value write and the counter update are one
// single-shard CAS.
func shaleKeyRoomBytes(appSlug domain.Slug) []byte {
	return shaleKey(prefixRoomBytes, appSlug.String())
}

// --- room value encoding (shale rejects empty Put values) ------------------
//
// shale reserves the empty payload for Delete tombstones, so Put rejects it,
// but an app PUTting "" is valid room state on the other backends. A stored
// room value is therefore shaleRoomValuePrefix + the app's exact bytes: the
// prefix keeps the stored value non-empty, and the decode strips it, so the
// verbatim-round-trip contract is identical across all three backends. All room
// BYTE accounting charges the DECODED length, so an empty value costs 0.
const shaleRoomValuePrefix = 'v'

func shaleEncodeRoomValue(val []byte) []byte {
	out := make([]byte, 0, len(val)+1)
	out = append(out, shaleRoomValuePrefix)
	return append(out, val...)
}

// shaleDecodeRoomValue strips the sentinel prefix, returning the app's exact
// bytes. A malformed stored value carrying no prefix is returned as-is, so a
// read never fails on one.
func shaleDecodeRoomValue(stored []byte) []byte {
	// Raw CAS reads at R>1 are LWW-wrapped, so strip the envelope first.
	// Idempotent at R=1 and for already-stripped values.
	if payload, err := stripEnvelope(stored); err == nil {
		stored = payload
	}
	if len(stored) > 0 && stored[0] == shaleRoomValuePrefix {
		return append([]byte(nil), stored[1:]...)
	}
	return append([]byte(nil), stored...)
}

// shaleDecodedRoomValueLen is the app-visible byte length of a stored room
// value: the figure all room byte accounting charges.
func shaleDecodedRoomValueLen(stored []byte) int {
	// Only cluster.Get unwraps, and the CAS tx.Get callers hand back RAW
	// stored bytes, so strip the R>1 LWW envelope here. Idempotent at R=1.
	if payload, err := stripEnvelope(stored); err == nil {
		stored = payload
	}
	if len(stored) > 0 && stored[0] == shaleRoomValuePrefix {
		return len(stored) - 1
	}
	return len(stored)
}

// --- Room operations (on ShaleRepo) ----------------------------------------

// CreateRoom mints an empty room, records the creation-accounting marker, and
// enforces the per-app aggregate cap in ONE {app-slug}-shard CAS: the room
// record, the ledger marker and the per-app counter all co-shard. The creation rate-limit DECISION runs in the service via
// CountRoomCreates OUTSIDE this call, so that gate stays a SOFT bound. Returns
// ErrSlugTaken on an (app, id) collision, ErrAppRoomsFull past the byte cap.
func (r *ShaleRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	roomKey := shaleKeyRoom(room.AppSlug, room.ID)
	counterKey := shaleKeyRoomBytes(room.AppSlug)
	return r.cluster.Transact(roomKey, func(tx backend.Transaction) error {
		// The collision read joins the CAS read-set, so a racing create
		// conflicts rather than overwriting.
		if _, err := tx.Get(roomKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("room collision check: %w", err)
		}
		// A brand-new room is empty, but refusing creation at the byte cap
		// keeps a full app from accumulating unbounded EMPTY rooms.
		if appCap > 0 {
			cur, err := txGetCounter(tx, counterKey)
			if err != nil {
				return err
			}
			if cur >= appCap {
				return ErrAppRoomsFull
			}
		}
		if err := shaleTxPutJSON(tx, roomKey, roomRowFromDomain(room)); err != nil {
			return err
		}
		return tx.Put(shaleKeyRoomCreate(room.AppSlug, subnet, room.ID, now), markerValue)
	})
}

// GetRoom returns the room record for (appSlug, id) or ErrNotFound.
func (r *ShaleRepo) GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error) {
	var row roomRow
	if err := r.getJSON(shaleKeyRoom(appSlug, id), &row); err != nil {
		return domain.Room{}, err
	}
	return row.toDomain(appSlug, id), nil
}

// GetRoomValue returns one value scoped by (appSlug, id, key), VERBATIM. A
// missing key and a missing room both return ErrNotFound, so this layer leaks
// no per-key existence.
func (r *ShaleRepo) GetRoomValue(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	raw, err := r.getRaw(shaleKeyRoomValue(appSlug, id, key))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, ErrNotFound
	}
	return shaleDecodeRoomValue(raw), nil
}

// ScanRoom returns the whole namespace for (appSlug, id), stamped with the
// EXACT per-room sequence the snapshot reflects. Single-shard ScanPrefix over
// roomkv/<app>/<uuid>/, so it never fans out. An existing room with no values
// returns an empty (non-nil) RoomKV; a nonexistent room scans empty at seq 0,
// and the service layer's prior GetRoom is what turns that into a 404.
//
// Shale cannot put a prefix scan inside a CAS (no phantom protection), so
// exactness runs a SEQ FENCE: read the room record's seq, scan, re-read the
// seq. Every mutation bumps that seq in its own CAS, so equal fence values mean
// the scan is exactly the state at S; a change means a commit interleaved and
// the scan retries. On retry exhaustion the scan FAILS rather than returning a
// state whose S is a lie - the join fails and the client reconnects.
//
// # Replication bar (R <= 2)
//
// The fence assumes every read it issues observes every mutation the equal
// fence values bracket. That holds at R=1, and at R=2 because the write bar is
// 2/2, so any member a read lands on is complete.
//
// At R >= 3 a write acks on a quorum, so a read-one union scan served
// mid-handoff by a member outside some write's ack set could miss a mutation
// both fence reads agree is committed. S would be stamped high with the
// mutation absent, a hole the splice contract CANNOT detect because the
// snapshot claims to cover it.
//
// Raising the room tier past R=2 requires revisiting this fence, e.g. a quorum
// union scan, or bracketing the scan and fence reads on the same member set.
// Pinned by TestShaleRoomSeqFence_ReplicationBar.
func (r *ShaleRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	prefix := string(shalePrefixRoomValues(appSlug, id))
	for attempt := range maxFenceRetries {
		if attempt > 0 {
			fenceBackoff(attempt - 1)
		}
		before, err := r.roomSeq(appSlug, id)
		if err != nil {
			return domain.RoomKV{}, err
		}
		items, err := r.scanPrefix(shalePrefixRoomValues(appSlug, id))
		if err != nil {
			return domain.RoomKV{}, err
		}
		after, err := r.roomSeq(appSlug, id)
		if err != nil {
			return domain.RoomKV{}, err
		}
		if before != after {
			continue // a mutation interleaved; the scan's S would be a lie
		}
		kv := domain.NewRoomKV()
		kv.Seq = before
		for _, item := range items {
			// A key may itself contain '/', so recover it by stripping the
			// fixed namespace prefix, never by splitting.
			k := strings.TrimPrefix(string(item.Key), prefix)
			kv.Values[k] = shaleDecodeRoomValue(item.Value)
		}
		return kv, nil
	}
	return domain.RoomKV{}, fmt.Errorf("scan room %s/%s: seq fence exhausted after %d retries", appSlug, id, maxFenceRetries)
}

// roomSeq reads the room record's current seq. A nonexistent room reads as
// seq 0, matching the empty scan the caller pairs it with.
func (r *ShaleRepo) roomSeq(appSlug domain.Slug, id domain.RoomID) (uint64, error) {
	var row roomRow
	if err := r.getJSON(shaleKeyRoom(appSlug, id), &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return row.Seq, nil
}

// PutRoomValue writes val under key, enforcing the per-room and per-app caps,
// in ONE {app-slug}-shard CAS: re-read the value key for the byte delta,
// validate both caps from values the read-set carries (the per-ROOM byte total
// + key count on the room record, and the per-APP roombytes/<app> counter),
// write the value, touch the room. Value, counter and record all co-shard, so
// no reserve/confirm split is needed.
//
// The per-room cap is strict because the room record is in the read-set (read
// for the room-exists check, rewritten on the clock touch): two writers to
// DISTINCT keys of one room do not conflict on their disjoint value keys, but
// DO conflict on the shared record, so the loser retries against updated
// totals. That in-record figure exists because ScanPrefix is not allowed inside
// a CAS, so the cap needs a discrete value the read-set can carry.
//
// Returns the assigned per-room sequence: each commit observes the prior seq
// and writes exactly prior + 1, so density and uniqueness fall out of the same
// conflict that makes the per-room cap strict (SPEC "The per-room sequence:
// assignment at commit").
//
// Returns ErrNotFound if the room is gone, ErrRoomDataFull (413) on the
// per-room cap, ErrAppRoomsFull (507) on the per-app aggregate. Rooms hold no
// blobs, so this path touches no object-store quota and has no service-wide
// byte cap (SPEC "Rooms -> Quota and abuse -> Durable total-bytes ceiling").
func (r *ShaleRepo) PutRoomValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap int64, now time.Time) (uint64, error) {
	valueKey := shaleKeyRoomValue(appSlug, id, key)
	roomKey := shaleKeyRoom(appSlug, id)
	counterKey := shaleKeyRoomBytes(appSlug)

	// Race-free because it depends only on len(val). The byte-total and
	// key-count caps cannot be checked out here - they would race a stale
	// scan - so they live inside the CAS below.
	if len(val) > domain.MaxRoomValueBytes {
		return 0, ErrRoomDataFull
	}

	// Captured from the closure's LAST (committed) run: a CAS retry re-reads
	// the record and recomputes, so what is left here is what the winning
	// commit wrote.
	var assigned uint64

	err := retryContended(func() error {
		return r.cluster.Transact(valueKey, func(tx backend.Transaction) error {
			// Re-checking existence INSIDE the write boundary is what stops a
			// concurrent delete removing the room between the service's GetRoom and
			// this write. It also puts the record in the read-set,
			// which is what serializes two DISTINCT-key writers to the same room.
			var row roomRow
			if err := shaleTxGetJSON(tx, roomKey, &row); err != nil {
				return err // ErrNotFound if the room is gone
			}

			// The delta is against the value this CAS will overwrite, and on
			// DECODED lengths, so accounting matches the other backends exactly.
			prior := 0
			isNewKey := true
			if cur, err := tx.Get(valueKey); err == nil {
				prior = shaleDecodedRoomValueLen(cur)
				isNewKey = false
			} else if !errors.Is(err, backend.ErrNotFound) {
				return fmt.Errorf("re-read room value: %w", err)
			}
			delta := int64(len(val) - prior)

			// STRICT per-room cap: the same byte + key-count checks as
			// domain.RoomKV.CanPut, but against the record's running totals (in
			// the read-set) rather than a materialized namespace, so the CAS
			// makes them race-safe.
			newByteTotal := row.ByteTotal + delta
			if newByteTotal > int64(domain.MaxRoomBytes) {
				return ErrRoomDataFull
			}
			newKeyCount := row.KeyCount
			if isNewKey {
				newKeyCount++
			}
			if newKeyCount > domain.MaxRoomKeys {
				return ErrRoomDataFull
			}

			// STRICT per-app aggregate: read-check-increment the counter in the
			// same CAS, charging only a positive delta. Skipped entirely on a
			// zero delta, where no cap decision and no counter write can follow:
			// reading it would only widen the read set and buy spurious retries.
			if delta != 0 {
				cur, err := txGetCounter(tx, counterKey)
				if err != nil {
					return err
				}
				if appCap > 0 && delta > 0 && cur+delta > appCap {
					return ErrAppRoomsFull
				}
				if newTotal := cur + delta; newTotal != cur {
					if err := tx.Put(counterKey, formatCounter(newTotal)); err != nil {
						return err
					}
				}
			}

			// Sentinel-prefixed so an empty app value still stores a non-empty
			// payload, which is what shale's Put requires.
			if err := tx.Put(valueKey, shaleEncodeRoomValue(val)); err != nil {
				return fmt.Errorf("put room value: %w", err)
			}

			// The touch bumps the clock, commits the running totals, and assigns
			// the seq, all inside this CAS.
			row.ByteTotal = newByteTotal
			row.KeyCount = newKeyCount
			if err := shaleTxTouchRoom(tx, appSlug, id, &row, now); err != nil {
				return err
			}
			assigned = row.Seq
			return nil
		})
	})
	if err != nil {
		// AMBIGUOUS-COMMIT CAVEAT (SPEC "Multi-pod relay -> Delivery
		// semantics"): an error here does not always mean the write failed.
		// A CAS that landed but lost its ack consumed the room's seq DURABLY
		// while this caller, which mirrors only on success, broadcasts no
		// frame for it. Subscribers then hold a hole at that seq which only
		// the NEXT durable frame exposes, so a quiet room stays visually
		// stale until one arrives. The durable KV is never wrong, and a
		// re-snapshot heals any subscriber.
		return 0, err
	}
	return assigned, nil
}

// DeleteRoomValue removes key and bumps the room's clock (a delete is a
// write), in one {app-slug}-shard CAS: re-check the room exists, delete the
// value, credit the freed bytes back to the per-app counter and the record's
// running totals, touch the room. Returns the assigned per-room sequence, and
// ErrNotFound only when the ROOM does not exist.
//
// Idempotent: deleting an absent key succeeds and still assigns a seq, because
// a seq bump rides every commit and no relay subscriber may see a bump with no
// frame.
func (r *ShaleRepo) DeleteRoomValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	valueKey := shaleKeyRoomValue(appSlug, id, key)
	roomKey := shaleKeyRoom(appSlug, id)
	counterKey := shaleKeyRoomBytes(appSlug)
	var assigned uint64
	err := retryContended(func() error {
		return r.cluster.Transact(valueKey, func(tx backend.Transaction) error {
			var row roomRow
			if err := shaleTxGetJSON(tx, roomKey, &row); err != nil {
				return err // ErrNotFound if the room is gone
			}
			// Freed bytes are the DECODED length, so the decrement matches the
			// increment PutValue made.
			freed := int64(0)
			deletedKey := false
			if cur, err := tx.Get(valueKey); err == nil {
				freed = int64(shaleDecodedRoomValueLen(cur))
				deletedKey = true
				if err := tx.Delete(valueKey); err != nil {
					return fmt.Errorf("delete room value: %w", err)
				}
			} else if !errors.Is(err, backend.ErrNotFound) {
				return fmt.Errorf("read room value for delete: %w", err)
			}
			if freed > 0 {
				cur, err := txGetCounter(tx, counterKey)
				if err != nil {
					return err
				}
				if err := tx.Put(counterKey, formatCounter(cur-freed)); err != nil {
					return err
				}
			}
			// Only a present key that was actually removed changes the totals.
			if deletedKey {
				row.ByteTotal -= freed
				if row.ByteTotal < 0 {
					row.ByteTotal = 0
				}
				if row.KeyCount > 0 {
					row.KeyCount--
				}
			}
			if err := shaleTxTouchRoom(tx, appSlug, id, &row, now); err != nil {
				return err
			}
			assigned = row.Seq
			return nil
		})
	})
	if err != nil {
		// Same ambiguous-commit caveat as PutRoomValue: an error here may
		// still have consumed a seq durably with no mirror frame anywhere.
		return 0, err
	}
	return assigned, nil
}

// shaleTxTouchRoom bumps UpdatedAt and assigns the mutation's per-room sequence
// (row.Seq = prior + 1) inside the caller's CAS. Mutates *row in place.
//
// The record is in the CAS read-set, so a concurrent same-room mutation
// conflicts and the loser retries against the updated seq: seq density and
// uniqueness ride the same conflict that makes the per-room cap strict. Every
// key touched co-shards with the value on {app-slug}, so the whole touch stays
// in the one CAS.
func shaleTxTouchRoom(tx backend.Transaction, appSlug domain.Slug, id domain.RoomID, row *roomRow, now time.Time) error {
	row.UpdatedAt = now
	row.Seq++
	return shaleTxPutJSON(tx, shaleKeyRoom(appSlug, id), *row)
}

// CountRoomCreates returns the in-window (perSubnet, perApp) creation counts
// via a single-shard ScanPrefix on roomcreate/<app>/: the per-app count is
// every in-window row, the per-subnet count the subset whose <subnet> segment
// matches. The fixed-width <ts> on each marker is what the window compares.
func (r *ShaleRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	items, err := r.scanPrefix(shalePrefixAppRoomCreates(appSlug))
	if err != nil {
		return 0, 0, err
	}
	cutoff := now.Add(-window).UTC().Format(sortableTimeFormat)
	prefix := string(shalePrefixAppRoomCreates(appSlug))
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), prefix)
		// rest is "<subnet>/<ts>/<uuid>"; splitRoomCreateRest strips the
		// trailing uuid and recovers (subnet, ts).
		rowSubnet, ts, ok := splitRoomCreateRest(rest)
		if !ok {
			continue
		}
		if ts <= cutoff {
			// Out of window: it can no longer change any decision, so the scan
			// that already walked past it drops it. That is what keeps the
			// family bounded with no background pass and no fan-out
			// (docs/SPEC.md "Room creation rate limit").
			if err := r.cluster.Delete(item.Key); err != nil {
				r.repoLog().Printf("roomcreate: drop expired marker %s: %v (it stops counting regardless)", item.Key, err)
			}
			continue
		}
		perApp++
		if rowSubnet == subnet {
			perSubnet++
		}
	}
	return perSubnet, perApp, nil
}

// --- SweepRooms ------------------------------------------------------------

// Fence retry budget. A fence attempt is only satisfied by a window with NO
// interleaved commit, so retrying is a race against the writers - and retrying
// IMMEDIATELY re-enters the same contention that just lost. The backoff is what
// creates a gap wide enough for a burst to drain; without it a fence under
// sustained concurrent writes cannot converge however many attempts it is given.
//
// Jittered so N concurrent scanners do not retry in lockstep and keep colliding.
const (
	maxFenceRetries = 8
	fenceBackoffMin = 500 * time.Microsecond
	fenceBackoffMax = 40 * time.Millisecond
)

// fenceBackoff sleeps before fence attempt n (0-based), doubling to a cap with
// full jitter.
func fenceBackoff(n int) {
	d := min(fenceBackoffMin<<min(n, 16), fenceBackoffMax)
	time.Sleep(fenceBackoffMin + time.Duration(rand.Int64N(int64(d))))
}

// retryContended re-runs a room mutation whose transaction exhausted its own
// CAS budget.
//
// Concurrent writers to one room contend on the SAME room record: every
// mutation bumps its seq, so the record is in every writer's read set by
// design (that is what serializes them). Under a burst the cluster's internal
// OCC budget can run out, which surfaces as backend.ErrCASConflict rather than
// as a completed write. That is contention, not a fault, and the caller's write
// is still valid - so it is retried after a backoff wide enough for the burst
// to drain, instead of being reported to a client as a failure.
//
// Matches ONLY ErrCASConflict: any other error is a real failure and is
// returned untouched.
func retryContended(fn func() error) error {
	var err error
	for attempt := range maxFenceRetries {
		if attempt > 0 {
			fenceBackoff(attempt - 1)
		}
		if err = fn(); !errors.Is(err, backend.ErrCASConflict) {
			return err
		}
	}
	return err
}

// errRoomDeleteFenceStale reports that a mutation landed between DeleteRoom's
// value enumeration and its commit, so the enumeration no longer describes the
// room. Never escapes DeleteRoom's retry loop.
var errRoomDeleteFenceStale = errors.New("room mutated during delete enumeration")

// DeleteRoom removes a room record and EVERY value in its namespace, then
// decrements the per-app counter by the freed bytes, all on the one {app-slug}
// shard. Idempotent: a missing room is a no-op.
//
// The value keys are enumerated OUTSIDE the CAS, since ScanPrefix is not
// allowed inside, so the enumeration is fenced on the room record's seq exactly
// as ScanRoom fences its scan: read the seq, scan, then re-read the record
// inside the CAS and require the same seq. Every mutation bumps that seq in its
// own CAS, so a put committing in the window is detected and the whole pass
// retries. Without the fence a value written in that window survives as a row
// no scan can reach (its room is gone) and its bytes stay charged to the
// per-app counter forever, which nothing repairs.
//
// Given equal fence values, every delete and the counter decrement land in ONE
// CAS, so the room and all its values vanish atomically (the shale analogue of
// the sqlite FK cascade).
func (r *ShaleRepo) DeleteRoom(appSlug domain.Slug, id domain.RoomID) error {
	roomKey := shaleKeyRoom(appSlug, id)
	counterKey := shaleKeyRoomBytes(appSlug)
	for attempt := range maxFenceRetries {
		if attempt > 0 {
			fenceBackoff(attempt - 1)
		}
		var row roomRow
		if err := r.getJSON(roomKey, &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		// The value keys and their total freed bytes, outside the tx.
		values, err := r.scanPrefix(shalePrefixRoomValues(appSlug, id))
		if err != nil {
			return err
		}
		var freed int64
		for _, item := range values {
			freed += int64(shaleDecodedRoomValueLen(item.Value))
		}
		err = r.cluster.Transact(roomKey, func(tx backend.Transaction) error {
			// Re-reading the record joins it to the read set AND closes the
			// enumeration window: an interleaved put bumped its seq.
			var inTx roomRow
			if err := shaleTxGetJSON(tx, roomKey, &inTx); err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil // a concurrent delete already removed it
				}
				return err
			}
			if inTx.Seq != row.Seq {
				return errRoomDeleteFenceStale
			}
			if err := tx.Delete(roomKey); err != nil {
				return fmt.Errorf("delete room record: %w", err)
			}
			for _, item := range values {
				if err := tx.Delete(item.Key); err != nil {
					return fmt.Errorf("delete room value %s: %w", item.Key, err)
				}
			}
			if freed > 0 {
				cur, err := txGetCounter(tx, counterKey)
				if err != nil {
					return err
				}
				if err := tx.Put(counterKey, formatCounter(cur-freed)); err != nil {
					return err
				}
			}
			return nil
		})
		if errors.Is(err, errRoomDeleteFenceStale) {
			continue
		}
		return err
	}
	return fmt.Errorf("delete room %s/%s: seq fence exhausted after %d retries", appSlug, id, maxFenceRetries)
}
