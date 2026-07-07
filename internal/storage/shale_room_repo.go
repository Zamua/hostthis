// Package storage's shale-cluster-backed room (app-persistence) KV store.
//
// Shale-cluster twin of slate_room_repo.go (the SlateDB-direct room repo)
// and room_repo.go (the sqlite RoomKVRepo). It reuses the SAME room key
// names + JSON record schema the slatedb backend uses (see
// slate_room_repo.go and docs/SPEC.md "Shale reuses the layout"); co-location
// across shards is by the ShardKeyFn (shaleShardKey in shale_shardkey.go),
// not by renaming keys. The roomRow JSON shape is shared package-level with
// the slatedb backend, so the on-wire room record is identical across both
// slatedb-tagged backends.
//
// # Key layout (reuses the slatedb room layout; all sharded on {app-slug})
//
//	rooms/<app-slug>/<uuid>                     -> {app-slug} shard  (JSON record; carries the per-room byte_total + key_count for the strict per-room cap)
//	roomkv/<app-slug>/<uuid>/<key>              -> {app-slug} shard  (raw value bytes, sentinel-prefixed)
//	roomcreate/<app-slug>/<subnet>/<ts>/<uuid>  -> {app-slug} shard  (creation ledger marker; ts fixed-width, uuid disambiguates)
//	roomexpiry/<ts>/<app-slug>/<uuid>           -> {app-slug} shard  (sweep index marker; ts fixed-width)
//	roombytes/<app-slug>                        -> {app-slug} shard  (the per-app room-byte counter)
//
// All four room families AND the per-app byte counter shard on <app-slug>,
// so an app's rooms, every value, its creation ledger, its expiry entries,
// and its byte counter co-locate on ONE shard. That makes "write one key,"
// "load the whole room," "count this app's creations," and "check the per-app
// cap" all single-shard operations - the room repo never fans out across
// shards on the hot path.
//
// # Both caps are STRICT (single-shard CAS, two different read-set members)
//
// A CAS read-set is a set of discrete key checks - ScanPrefix is not allowed
// inside a transaction (a scanned range has no cheap phantom protection) - so a
// cap whose magnitude is "the sum over a key range" must be backed by a
// discrete COUNTER the read-set can carry, not by an in-CAS scan. The two room
// caps use two different such read-set members:
//
//   - Per-APP aggregate: the per-app counter roombytes/<app> is read-checked +
//     incremented in the {app-slug} value-write CAS, so two concurrent same-app
//     writers cannot both pass a stale per-app sum and overshoot appCap.
//   - Per-ROOM byte + key total: stored ON the room record (the roomRow
//     byte_total + key_count fields, shale-only) and validated against
//     MaxRoomBytes / MaxRoomKeys INSIDE the CAS. The room record is already in
//     the read-set (the room-exists re-check) and rewritten on every PUT/DELETE
//     (the clock touch), so two concurrent writers to DISTINCT keys of the same
//     room - which target disjoint value keys and so would NOT conflict on the
//     value-key check alone - DO conflict on the shared room record; the loser
//     retries against the now-updated totals. So the per-room ceiling holds no
//     matter how the writes interleave (conformCaps.StrictQuotaUnderConcurrency
//     = true; the Rooms/PerRoomCapConcurrentCeiling conformance subtest pins it).
//
// Unlike the paste / site insert (which spans the {id} counter shard and the
// {slug} authoritative shard and needs the three-step reserve/authoritative/
// confirm reservation), every room key co-shards on {app-slug}, so a room PUT
// is a single-shard read-check-validate-write - no reserve/confirm split.
//
// # Sweep-time expiry (ExpiryFreesQuotaAtReadTime = false)
//
// The per-app room-byte counter (roombytes/<app>) is monotonic the way the
// paste / site byte counters are: an expired-but-unswept room's bytes leave
// the counter when the sweep deletes the room (DeleteRoom), not at read time.
// So the counter over-counts an expired-unswept room transiently but never
// under-counts (the fail-safe direction), matching the shale paste + site
// counters.

//go:build slatedb

package storage

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

// ShaleRoomRepo is the service.RoomRepo + service.SweepRooms adapter over a
// ShaleRepo. It delegates the interface-named methods to the ShaleRepo
// `...Room` methods, so the shale backend's room repo shares the same cluster
// handle (and shard routing) as its paste + site repos. NewShaleRoomRepo(repo)
// builds it. Mirrors ShaleSiteRepo / SlateRoomRepo exactly.
type ShaleRoomRepo struct {
	repo *ShaleRepo
}

// NewShaleRoomRepo wraps a ShaleRepo so the no-auth room-persistence tier
// runs on the shale backend. The returned adapter satisfies service.RoomRepo
// and service.SweepRooms (and so the cmd/hostthisd roomStore union).
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

// service.SweepRooms
func (s *ShaleRoomRepo) ExpiredRooms(now time.Time) ([]domain.ExpiredRoom, error) {
	return s.repo.ExpiredRooms(now)
}
func (s *ShaleRoomRepo) DeleteExpiredRoom(ref domain.ExpiredRoom) (bool, error) {
	return s.repo.DeleteExpiredRoom(ref)
}
func (s *ShaleRoomRepo) PruneOldRoomCreates(cutoff time.Time) (int, error) {
	return s.repo.PruneOldRoomCreates(cutoff)
}

// --- key builders (mirror the slatedb room layout; sharded on {app-slug}) --

func shaleKeyRoom(appSlug domain.Slug, id domain.RoomID) []byte {
	return []byte("rooms/" + appSlug.String() + "/" + id.String())
}

func shalePrefixAppRooms(appSlug domain.Slug) []byte {
	return []byte("rooms/" + appSlug.String() + "/")
}

func shaleKeyRoomValue(appSlug domain.Slug, id domain.RoomID, key string) []byte {
	return []byte("roomkv/" + appSlug.String() + "/" + id.String() + "/" + key)
}

func shalePrefixRoomValues(appSlug domain.Slug, id domain.RoomID) []byte {
	return []byte("roomkv/" + appSlug.String() + "/" + id.String() + "/")
}

// shaleKeyRoomCreate is the creation-ledger marker. The room id is the
// trailing segment so two rooms created under the SAME (app, subnet) at the
// SAME timestamp get DISTINCT keys (a shared key would overwrite and
// undercount the rate limit). The <ts> is fixed-width; a windowed compare
// reads it as the second-to-last segment.
func shaleKeyRoomCreate(appSlug domain.Slug, subnet string, id domain.RoomID, t time.Time) []byte {
	return []byte("roomcreate/" + appSlug.String() + "/" + subnet + "/" + t.UTC().Format(expirySiteTimeFormat) + "/" + id.String())
}

func shalePrefixAppRoomCreates(appSlug domain.Slug) []byte {
	return []byte("roomcreate/" + appSlug.String() + "/")
}

func shaleKeyRoomExpiry(t time.Time, appSlug domain.Slug, id domain.RoomID) []byte {
	return []byte("roomexpiry/" + t.UTC().Format(expirySiteTimeFormat) + "/" + appSlug.String() + "/" + id.String())
}

// shaleKeyRoomBytes is the per-app room-byte counter (the per-app analogue of
// identity_bytes/<id>). Co-shards with every room family on {app-slug}, so the
// value write + the counter update are one single-shard CAS.
func shaleKeyRoomBytes(appSlug domain.Slug) []byte {
	return []byte("roombytes/" + appSlug.String())
}

// --- room value encoding (shale rejects empty Put values) ------------------
//
// shale's Put rejects empty values (the empty payload is reserved for Delete
// tombstones), but a room value may legitimately be the empty byte string (an
// app PUTting "" is valid app state on the sqlite + slatedb backends). To keep
// the observable verbatim-round-trip contract IDENTICAL across all three
// backends, a stored room value is prefixed with one sentinel byte:
// shaleRoomValuePrefix + the app's exact bytes. The prefix guarantees the
// stored value is never empty (so Put accepts an empty room value), and the
// decode strips it to return the app's exact bytes. All room BYTE accounting
// (the per-app counter and the per-room cap) operates on
// the DECODED length, so the byte totals match slate exactly (an empty value
// counts as 0 bytes, a 10-byte value as 10).
const shaleRoomValuePrefix = 'v'

func shaleEncodeRoomValue(val []byte) []byte {
	out := make([]byte, 0, len(val)+1)
	out = append(out, shaleRoomValuePrefix)
	return append(out, val...)
}

// shaleDecodeRoomValue strips the sentinel prefix, returning the app's exact
// bytes. A legacy / malformed stored value with no prefix is returned as-is so
// a read never fails on it (defensive; the encoder always writes the prefix).
func shaleDecodeRoomValue(stored []byte) []byte {
	// Strip the LWW envelope first (see shaleDecodedRoomValueLen): raw CAS
	// reads at R>1 are wrapped. Idempotent for R=1 / already-stripped values.
	if payload, err := stripEnvelope(stored); err == nil {
		stored = payload
	}
	if len(stored) > 0 && stored[0] == shaleRoomValuePrefix {
		return append([]byte(nil), stored[1:]...)
	}
	return append([]byte(nil), stored...)
}

// shaleDecodedRoomValueLen is the app-visible byte length of a stored room
// value (the figure all room byte accounting charges), accounting for the
// sentinel prefix the encoder adds.
func shaleDecodedRoomValueLen(stored []byte) int {
	// Strip the LWW envelope first: R>1 writes are wrapped, and the CAS
	// tx.Get sites that call this hand back the RAW stored bytes (only
	// cluster.Get unwraps). Idempotent for R=1 / already-stripped values.
	if payload, err := stripEnvelope(stored); err == nil {
		stored = payload
	}
	if len(stored) > 0 && stored[0] == shaleRoomValuePrefix {
		return len(stored) - 1
	}
	return len(stored)
}

// --- Room operations (on ShaleRepo) ----------------------------------------

// CreateRoom mints an empty room, records the creation-accounting marker,
// and enforces the per-app aggregate cap, in ONE {app-slug}-shard CAS (the
// room record, the create ledger marker, and the expiry index all co-shard
// on {app-slug}, and the per-app byte counter is read in the same CAS). The
// creation rate-limit DECISION is read by the service via CountRoomCreates
// OUTSIDE this call (the gate stays a SOFT bound). Returns ErrSlugTaken on
// the astronomically-unlikely (app, id) collision, ErrAppRoomsFull past the
// per-app byte cap.
func (r *ShaleRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	roomKey := shaleKeyRoom(room.AppSlug, room.ID)
	counterKey := shaleKeyRoomBytes(room.AppSlug)
	return r.cluster.Transact(roomKey, func(tx backend.Transaction) error {
		// Collision check: a found record means the minted id is taken. The
		// read participates in the CAS read-set so a racing create conflicts.
		if _, err := tx.Get(roomKey); err == nil {
			return ErrSlugTaken
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("room collision check: %w", err)
		}
		// Per-app aggregate pre-check: refuse a new room once the app is
		// already at its byte cap (a brand-new room is empty, but bounding
		// creation here keeps a full app from accumulating unbounded empty
		// rooms, mirroring sqlite + slatedb CreateRoom). The counter co-shards
		// on {app-slug}, so this is a same-shard read inside the CAS.
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
		if err := tx.Put(shaleKeyRoomCreate(room.AppSlug, subnet, room.ID, now), markerValue); err != nil {
			return err
		}
		return tx.Put(shaleKeyRoomExpiry(room.ExpiresAt, room.AppSlug, room.ID), markerValue)
	})
}

// GetRoom returns the room record for (appSlug, id) or ErrNotFound. Like the
// sqlite + slate room Get it returns expired-but-unswept rows too (the HTTP
// layer 404s them, the sweep deletes them).
func (r *ShaleRepo) GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error) {
	var row roomRow
	if err := r.getJSON(shaleKeyRoom(appSlug, id), &row); err != nil {
		return domain.Room{}, err
	}
	return row.toDomain(appSlug, id), nil
}

// GetRoomValue returns one value scoped by (appSlug, id, key), or ErrNotFound.
// A missing key in a real room and a key under a nonexistent room both return
// ErrNotFound (no per-key existence leak). The value is returned VERBATIM.
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

// ScanRoom returns the whole namespace for (appSlug, id) as a domain.RoomKV,
// stamped with the EXACT per-room sequence the snapshot reflects
// (RoomKV.Seq). Shale cannot put a prefix scan inside a CAS (no phantom
// protection), so exactness runs a SEQ FENCE (see SPEC "The per-room
// sequence: assignment at commit"): read the room record's seq, scan the
// namespace, re-read the seq - equal means no mutation committed between
// the two reads (every mutation bumps the record's seq in its CAS), so the
// scan is exactly the state at S; changed means a commit interleaved and
// the scan retries. Bounded retries; on exhaustion the scan FAILS rather
// than returning a state whose S is a lie (the relay join then fails and
// the client reconnects - correctness is never traded for a stale fence).
//
// REPLICATION BAR (R <= 2): the fence's exactness additionally assumes
// every read it issues - the two seq reads AND the union scan between them
// - observes every mutation the equal fence values bracket. That holds at
// R=1 (a single owner serves everything) and at R=2 (the write bar is 2/2:
// a write acks only once EVERY replica holds it, so any member a read
// lands on is complete). At R >= 3 a write's ack set is a quorum, so a
// read-one union scan served mid-handoff by a member OUTSIDE some write's
// ack set could miss a mutation that both fence reads - served by
// up-to-date members - agree is committed: S would be stamped high with
// the mutation absent from the state, a hole the splice contract CANNOT
// detect (the snapshot claims to cover it). Before raising the room
// tier's replication factor past 2, this fence must be revisited (e.g. a
// quorum union scan, or bracketing scan and fence reads on the same
// member set). The current bar is pinned by
// TestShaleRoomSeqFence_ReplicationBar.
//
// Single-shard ScanPrefix over roomkv/<app>/<uuid>/ (every room family
// co-shards on {app-slug}, so this never fans out). An existing room with no
// values returns an empty (non-nil) RoomKV; a nonexistent room scans empty
// at seq 0 (the service layer's prior GetRoom is what turns it into a 404).
func (r *ShaleRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	const maxFenceRetries = 5
	prefix := string(shalePrefixRoomValues(appSlug, id))
	for range maxFenceRetries {
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
			// Recover the app-chosen <key> by stripping the namespace prefix (a
			// key may itself contain '/', so strip the fixed prefix, not split).
			k := strings.TrimPrefix(string(item.Key), prefix)
			kv.Values[k] = shaleDecodeRoomValue(item.Value)
		}
		return kv, nil
	}
	return domain.RoomKV{}, fmt.Errorf("scan room %s/%s: seq fence exhausted after %d retries", appSlug, id, maxFenceRetries)
}

// roomSeq reads the room record's current seq; a nonexistent room reads as
// seq 0 (matching the empty scan the caller pairs it with).
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

// PutRoomValue writes val under key, enforcing the per-room and per-app caps
// and resetting the retention clock. The authoritative write
// is ONE {app-slug}-shard CAS that re-reads the specific value key for the
// byte delta, validates BOTH caps from values the CAS read-set carries - the
// per-ROOM byte total + key count stored ON the room record, and the per-APP
// counter (roombytes/<app>) - then writes the value and touches the room. The
// per-room cap is strict because the room record is in the read-set (read for
// the room-exists check, rewritten on the clock touch): two concurrent writers
// to DISTINCT keys of the same room - which do not conflict on their disjoint
// value keys - DO conflict on the shared room record, so the loser retries
// against the now-updated totals. ScanPrefix is not allowed inside a CAS (no
// phantom protection), which is why the per-room total is a discrete in-record
// figure the read-set can carry rather than an in-CAS scan. Because the value,
// the per-app counter, the room record, and the expiry index all co-shard on
// {app-slug}, this is single-shard - no reserve/confirm split needed.
//
// Returns the assigned per-room sequence: the room record is in every
// mutation's CAS read-set, so each commit observes the prior seq and
// writes exactly prior + 1 - density and uniqueness fall out of the same
// conflict that makes the per-room cap strict (see SPEC "The per-room
// sequence: assignment at commit").
//
// Returns ErrNotFound if the room is gone, ErrRoomDataFull (413) on the
// per-room cap, ErrAppRoomsFull (507) on the per-app aggregate. Rooms hold
// no blobs, so a room write touches no object-store quota and has no
// service-wide byte cap on this path (see SPEC "Rooms -> Quota and abuse ->
// Durable total-bytes ceiling").
func (r *ShaleRepo) PutRoomValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap int64, now time.Time) (uint64, error) {
	valueKey := shaleKeyRoomValue(appSlug, id, key)
	roomKey := shaleKeyRoom(appSlug, id)
	counterKey := shaleKeyRoomBytes(appSlug)

	// Stateless value-size guard (no race: depends only on len(val)). The
	// byte-total + key-count caps are NOT checked here - they are race-prone
	// against a stale scan, so they move INSIDE the CAS below, validated against
	// the room record's running totals (which the CAS read-set carries).
	if len(val) > domain.MaxRoomValueBytes {
		return 0, ErrRoomDataFull
	}

	// The assigned seq, captured from the closure's LAST (committed) run: a
	// CAS retry re-reads the record and recomputes, so the value left here
	// is the one the winning commit actually wrote.
	var assigned uint64

	// Authoritative single-shard CAS on {app-slug}: re-check the room exists,
	// re-read the value key (so the byte delta is computed against the value
	// the CAS will overwrite, race-safe even if a concurrent writer changed it),
	// enforce the STRICT per-room cap (from the room record's running totals,
	// which are in the read-set) and the STRICT per-app cap (the per-app
	// counter, also in the read-set), write the value, and touch the room (move
	// the expiry index + update the running totals + assign the seq).
	err := r.cluster.Transact(valueKey, func(tx backend.Transaction) error {
		// Room-exists re-check INSIDE the write boundary so a concurrent
		// expiry sweep cannot delete the room between the service's GetRoom
		// and this write. This Get also puts the room record in the read-set,
		// which is what serializes two concurrent DISTINCT-key writers to the
		// SAME room (they would not conflict on their disjoint value keys, but
		// they both read + rewrite the shared room record, so the second one's
		// CAS conflicts after the first commits and re-runs against the updated
		// per-room totals - the strict-per-room-cap mechanism).
		var row roomRow
		if err := shaleTxGetJSON(tx, roomKey, &row); err != nil {
			return err // ErrNotFound if the room is gone
		}

		// Re-read the value key to compute the authoritative byte delta. The
		// delta is computed on DECODED lengths (the sentinel prefix is not
		// charged), so all byte accounting matches the slate backend exactly.
		prior := 0
		isNewKey := true
		if cur, err := tx.Get(valueKey); err == nil {
			prior = shaleDecodedRoomValueLen(cur)
			isNewKey = false
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("re-read room value: %w", err)
		}
		delta := int64(len(val) - prior)

		// STRICT per-room cap: validate the new byte total + key count against
		// the structural caps, computed from the room record's running totals
		// (in the read-set) + this write's delta. Mirrors domain.RoomKV.CanPut's
		// byte + key-count checks, but against the stored totals rather than a
		// materialized namespace, so it is race-safe under the CAS read-set.
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

		// STRICT per-app aggregate: read-check-increment the per-app counter in
		// the same CAS. Charge only a positive delta.
		if delta != 0 || appCap > 0 {
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

		// Upsert the value, sentinel-prefixed so an empty app value still
		// stores a non-empty payload (shale Put rejects empty values). The
		// decode on read strips the prefix, so the app's exact bytes
		// round-trip - including the empty string.
		if err := tx.Put(valueKey, shaleEncodeRoomValue(val)); err != nil {
			return fmt.Errorf("put room value: %w", err)
		}

		// Touch the room: reset the clock, move the expiry index entry,
		// commit the new per-room running totals onto the record, AND assign
		// the mutation's seq (row.Seq+1, inside this CAS).
		row.ByteTotal = newByteTotal
		row.KeyCount = newKeyCount
		if err := shaleTxTouchRoom(tx, appSlug, id, &row, now); err != nil {
			return err
		}
		assigned = row.Seq
		return nil
	})
	if err != nil {
		// AMBIGUOUS-COMMIT CAVEAT (documented limitation; see SPEC "Multi-pod
		// relay -> Delivery semantics"): an error here does NOT always mean
		// the write failed. If the CAS landed but its ack was lost (a timeout
		// racing the object-store round trip), the room's seq was consumed
		// DURABLY while this caller - which mirrors only on success -
		// broadcasts no frame for it, locally or to any peer. Every
		// subscriber then sees a hole at that seq that only the NEXT durable
		// frame exposes, so a then-quiet room stays visually stale until one
		// arrives; the durable KV itself is never wrong (a re-snapshot heals
		// any subscriber). Accepted for now; the periodic room-seq beacon the
		// spec names is the future fix.
		return 0, err
	}
	return assigned, nil
}

// DeleteRoomValue removes key and resets the retention clock (a delete is a
// write). Idempotent: deleting an absent key succeeds - and still assigns a
// seq (it commits a touch; a seq bump rides every commit, so no relay
// subscriber ever sees a bump with no frame). One {app-slug}-shard
// CAS: re-check the room exists, delete the value, decrement the per-app
// counter by the freed bytes, drop the per-room byte_total + key_count on the
// record by the same freed bytes / one key, and touch the room (which
// assigns the seq). Returns the assigned per-room sequence. Returns
// ErrNotFound only when the ROOM does not exist.
func (r *ShaleRepo) DeleteRoomValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	valueKey := shaleKeyRoomValue(appSlug, id, key)
	roomKey := shaleKeyRoom(appSlug, id)
	counterKey := shaleKeyRoomBytes(appSlug)
	var assigned uint64
	err := r.cluster.Transact(valueKey, func(tx backend.Transaction) error {
		var row roomRow
		if err := shaleTxGetJSON(tx, roomKey, &row); err != nil {
			return err // ErrNotFound if the room is gone
		}
		// Read the value to learn its byte count (to credit the counter + the
		// per-room running totals). The freed bytes are the DECODED length (the
		// sentinel prefix was never charged), so the decrement matches the
		// increment PutValue made. A delete of a present key also drops the
		// per-room key count by one.
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
		// Maintain the per-room running totals (only when a present key was
		// actually removed; deleting an absent key changes nothing).
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
	if err != nil {
		// Same ambiguous-commit caveat as PutRoomValue: an error here can
		// still have consumed a seq durably with no mirror frame anywhere.
		return 0, err
	}
	return assigned, nil
}

// shaleTxTouchRoom resets the room's retention clock to now + the window
// inside the caller's CAS, ASSIGNS the mutation's per-room sequence
// (row.Seq = prior + 1; the record is in the CAS read-set, so a concurrent
// same-room mutation conflicts and the loser retries against the updated
// seq - density and uniqueness ride the same conflict that makes the
// per-room cap strict), and moves the roomexpiry index entry from the old
// ExpiresAt to the new one (delete-then-add). Every key it touches (the room
// record, both expiry index entries) co-shards with the value on {app-slug},
// so the whole touch stays in the one CAS. Mutates *row in place.
func shaleTxTouchRoom(tx backend.Transaction, appSlug domain.Slug, id domain.RoomID, row *roomRow, now time.Time) error {
	oldExpiry := row.ExpiresAt
	row.UpdatedAt = now
	row.ExpiresAt = now.Add(domain.RoomRetentionWindow)
	row.Seq++
	if err := shaleTxPutJSON(tx, shaleKeyRoom(appSlug, id), *row); err != nil {
		return err
	}
	if err := tx.Delete(shaleKeyRoomExpiry(oldExpiry, appSlug, id)); err != nil {
		return fmt.Errorf("delete old room expiry idx: %w", err)
	}
	return tx.Put(shaleKeyRoomExpiry(row.ExpiresAt, appSlug, id), markerValue)
}

// CountRoomCreates returns the in-window (perSubnet, perApp) creation counts.
// Single-shard ScanPrefix on roomcreate/<app>/ (the ledger co-shards on
// {app-slug}); the per-app count is every in-window row, the per-subnet count
// is the subset whose <subnet> segment matches. Markers carry the fixed-width
// <ts>; a row counts when its ts is within the window.
func (r *ShaleRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	items, err := r.scanPrefix(shalePrefixAppRoomCreates(appSlug))
	if err != nil {
		return 0, 0, err
	}
	cutoff := now.Add(-window).UTC().Format(expirySiteTimeFormat)
	prefix := string(shalePrefixAppRoomCreates(appSlug))
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), prefix)
		// rest is "<subnet>/<ts>/<uuid>"; splitRoomCreateRest (slate_room_repo.go)
		// strips the trailing uuid + recovers (subnet, ts).
		rowSubnet, ts, ok := splitRoomCreateRest(rest)
		if !ok {
			continue
		}
		if ts <= cutoff {
			continue
		}
		perApp++
		if rowSubnet == subnet {
			perSubnet++
		}
	}
	return perSubnet, perApp, nil
}

// SumActiveRoomBytes returns the total room value bytes across EVERY app,
// served from the per-app roombytes/ counters via a cross-shard aggregate
// (one read per app counter). Exposed for observability and tests; the
// durable total-bytes ceiling lives at the object store (the blob bucket
// quota), not in an app-level sum of room bytes. Like the shale paste + site
// counters it has NO read-time expiry awareness: an expired-unswept room's
// bytes leave the counter at sweep time (DeleteRoom), not read time.
func (r *ShaleRepo) SumActiveRoomBytes() (int64, error) {
	counters, err := r.aggregatePrefix(prefixRoomBytes)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range counters {
		n, err := parseCounter(item.Value)
		if err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		total += n
	}
	return total, nil
}

// --- SweepRooms ------------------------------------------------------------

// ExpiredRooms fans out across all {app-slug} shards over roomexpiry/ and
// returns one reference per entry whose <ts> is <= now (inclusive
// boundary): the (app-slug, room-id) pair plus the entry's full key as the
// opaque IndexRef, so DeleteExpiredRoom can remove the EXACT entry the scan
// surfaced even when the room record is already gone. The expiry keys use
// the fixed-width expirySiteTimeFormat, so the timestamp segment's byte
// order is time order EXACTLY (correct within a shared whole second).
// Matches the slate ExpiredRooms.
func (r *ShaleRepo) ExpiredRooms(now time.Time) ([]domain.ExpiredRoom, error) {
	items, err := r.aggregatePrefix(prefixRoomExpiryAll)
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(expirySiteTimeFormat)
	var out []domain.ExpiredRoom
	for _, item := range items {
		// key shape: roomexpiry/<ts>/<app-slug>/<uuid>
		k := string(item.Key)
		rest := strings.TrimPrefix(k, "roomexpiry/")
		before, after, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		ts := before
		appAndID := after
		before, after, ok = strings.Cut(appAndID, "/")
		if !ok {
			continue
		}
		if ts > cutoff {
			continue
		}
		out = append(out, domain.ExpiredRoom{
			AppSlug:  domain.Slug(before),
			ID:       domain.RoomID(after),
			IndexRef: k,
		})
	}
	return out, nil
}

// DeleteExpiredRoom processes one expired reference: the same full-cascade
// delete as DeleteRoom when the room record still exists, and then - in
// every case - removal of the exact expiry-index entry the scan surfaced
// (a single {app-slug}-shard CAS: the entry's shard key is its app-slug
// segment). The cascade removes the DERIVED key; this removes the OBSERVED
// one. Idempotent: a missing record and a missing entry are both no-ops.
// Returns whether a room record was actually deleted. Mirrors the paste
// DeleteExpired and site DeleteExpiredSite; see docs/SPEC.md "Room storage
// on the slatedb (and shale) backend" (sweep path).
func (r *ShaleRepo) DeleteExpiredRoom(ref domain.ExpiredRoom) (bool, error) {
	entryKey, err := expiryRoomIndexKey(ref)
	if err != nil {
		return false, err
	}
	deleted := false
	var row roomRow
	switch err := r.getJSON(shaleKeyRoom(ref.AppSlug, ref.ID), &row); {
	case errors.Is(err, ErrNotFound):
		// Orphaned entry: nothing to cascade, just clean the entry below.
	case err != nil:
		return false, err
	default:
		if err := r.DeleteRoom(ref.AppSlug, ref.ID); err != nil {
			return false, err
		}
		deleted = true
	}
	if entryKey != nil {
		if err := r.cluster.Transact(entryKey, func(tx backend.Transaction) error {
			return tx.Delete(entryKey)
		}); err != nil {
			return deleted, fmt.Errorf("delete room expiry entry %s: %w", entryKey, err)
		}
	}
	return deleted, nil
}

// DeleteRoom removes a room record, its expiry index entry, and EVERY value
// in its namespace, then decrements the per-app counter by the freed bytes -
// all on the one {app-slug} shard. The value keys are enumerated OUTSIDE the
// CAS (ScanPrefix is not allowed inside) but every delete + the counter
// decrement land in ONE CAS, so the room and all its values vanish atomically
// (the shale analogue of the sqlite FK cascade). The internal cascade the
// expiry pass reuses (through DeleteExpiredRoom); not called by the sweep
// directly. Idempotent: a missing room is a no-op.
func (r *ShaleRepo) DeleteRoom(appSlug domain.Slug, id domain.RoomID) error {
	var row roomRow
	if err := r.getJSON(shaleKeyRoom(appSlug, id), &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	// Enumerate the value keys (outside the tx) + their total freed bytes.
	values, err := r.scanPrefix(shalePrefixRoomValues(appSlug, id))
	if err != nil {
		return err
	}
	var freed int64
	for _, item := range values {
		freed += int64(shaleDecodedRoomValueLen(item.Value))
	}
	roomKey := shaleKeyRoom(appSlug, id)
	counterKey := shaleKeyRoomBytes(appSlug)
	return r.cluster.Transact(roomKey, func(tx backend.Transaction) error {
		if err := tx.Delete(roomKey); err != nil {
			return fmt.Errorf("delete room record: %w", err)
		}
		if err := tx.Delete(shaleKeyRoomExpiry(row.ExpiresAt, appSlug, id)); err != nil {
			return fmt.Errorf("delete room expiry idx: %w", err)
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
}

// PruneOldRoomCreates deletes roomcreate/ markers whose <ts> is before cutoff
// (now - RoomCreateWindow), via a cross-shard aggregate to find them + a
// per-shard delete each. Past the window a ledger marker can never change a
// future rate-limit decision, so the sweep drops it each tick to keep the
// family bounded - the same discipline the keygate prune + the sqlite
// room_creates prune use. Returns the number of markers deleted.
func (r *ShaleRepo) PruneOldRoomCreates(cutoff time.Time) (int, error) {
	items, err := r.aggregatePrefix(prefixRoomCreate)
	if err != nil {
		return 0, err
	}
	cutoffStr := cutoff.UTC().Format(expirySiteTimeFormat)
	deleted := 0
	for _, item := range items {
		// key shape: roomcreate/<app-slug>/<subnet>/<ts>/<uuid>; the <ts> is
		// the SECOND-to-last segment (the trailing <uuid> disambiguates
		// same-ms creates). Strip the trailing uuid, then ts is the last.
		k := string(item.Key)
		uuidSlash := strings.LastIndex(k, "/")
		if uuidSlash < 0 {
			continue
		}
		beforeUUID := k[:uuidSlash]
		tsSlash := strings.LastIndex(beforeUUID, "/")
		if tsSlash < 0 {
			continue
		}
		ts := beforeUUID[tsSlash+1:]
		if ts < cutoffStr {
			if err := r.cluster.Delete(item.Key); err != nil {
				return deleted, fmt.Errorf("delete room-create marker %s: %w", k, err)
			}
			deleted++
		}
	}
	return deleted, nil
}
