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
//	rooms/<app-slug>/<uuid>                     -> {app-slug} shard  (authoritative JSON record)
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
// # Per-app cap is STRICT (single-shard CAS, no reserve/confirm split)
//
// Because the authoritative value write (roomkv/<app>/<uuid>/<key>) and the
// per-app counter (roombytes/<app>) co-shard on {app-slug}, the value write
// and the counter update are ONE CAS transaction. So unlike the paste / site
// insert (which span the {id} counter shard and the {slug} authoritative
// shard and need the three-step reserve/authoritative/confirm reservation),
// a room PUT is a single-shard read-check-increment-write. The per-app cap is
// strict under concurrency (conformCaps.StrictQuotaUnderConcurrency = true):
// two concurrent same-app writers serialize through the one {app-slug} shard
// CAS, so the per-app byte ceiling cannot be overshot. The per-ROOM cap (byte
// + key totals) is materialized OUTSIDE the tx (ScanPrefix is not allowed
// inside a CAS) and the decision is made race-safe by re-reading the specific
// value key inside the CAS, the same discipline AppendVersion uses for the
// next version number.
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
func (s *ShaleRoomRepo) PutValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap, serviceCap int64, now time.Time) error {
	return s.repo.PutRoomValue(appSlug, id, key, val, appCap, serviceCap, now)
}
func (s *ShaleRoomRepo) DeleteValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) error {
	return s.repo.DeleteRoomValue(appSlug, id, key, now)
}
func (s *ShaleRoomRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	return s.repo.CountRoomCreates(appSlug, subnet, now, window)
}

// service.SweepRooms
func (s *ShaleRoomRepo) ExpiredRoomKeys(now time.Time) ([]domain.RoomRef, error) {
	return s.repo.ExpiredRoomKeys(now)
}
func (s *ShaleRoomRepo) DeleteRoom(appSlug domain.Slug, id domain.RoomID) error {
	return s.repo.DeleteRoom(appSlug, id)
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
// (the per-app counter, the per-room cap, the service-wide sum) operates on
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
	if len(stored) > 0 && stored[0] == shaleRoomValuePrefix {
		return append([]byte(nil), stored[1:]...)
	}
	return append([]byte(nil), stored...)
}

// shaleDecodedRoomValueLen is the app-visible byte length of a stored room
// value (the figure all room byte accounting charges), accounting for the
// sentinel prefix the encoder adds.
func shaleDecodedRoomValueLen(stored []byte) int {
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

// ScanRoom returns the whole namespace for (appSlug, id) as a domain.RoomKV.
// Single-shard ScanPrefix over roomkv/<app>/<uuid>/ (every room family
// co-shards on {app-slug}, so this never fans out). An existing room with no
// values returns an empty (non-nil) RoomKV; the service layer's prior GetRoom
// is what turns a nonexistent room into a 404.
func (r *ShaleRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	items, err := r.scanPrefix(shalePrefixRoomValues(appSlug, id))
	if err != nil {
		return domain.RoomKV{}, err
	}
	kv := domain.NewRoomKV()
	prefix := string(shalePrefixRoomValues(appSlug, id))
	for _, item := range items {
		// Recover the app-chosen <key> by stripping the namespace prefix (a
		// key may itself contain '/', so strip the fixed prefix, not split).
		k := strings.TrimPrefix(string(item.Key), prefix)
		kv.Values[k] = shaleDecodeRoomValue(item.Value)
	}
	return kv, nil
}

// PutRoomValue writes val under key, enforcing the per-room, per-app, and
// service-wide caps and resetting the retention clock. The per-room cap math
// runs on a namespace materialized OUTSIDE the tx (ScanPrefix is not allowed
// in a CAS); the authoritative write is ONE {app-slug}-shard CAS that
// re-reads the specific value key for race-safety, checks + increments the
// per-app counter (strict ceiling), writes the value, and moves the expiry
// index. Because the value, the counter, and the expiry index all co-shard
// on {app-slug}, this is single-shard - no reserve/confirm split needed.
//
// Returns ErrNotFound if the room is gone, ErrRoomDataFull (413) on the
// per-room cap, ErrAppRoomsFull (507) on the per-app aggregate, ErrServiceFull
// on the service-wide cap.
func (r *ShaleRepo) PutRoomValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap, serviceCap int64, now time.Time) error {
	valueKey := shaleKeyRoomValue(appSlug, id, key)
	roomKey := shaleKeyRoom(appSlug, id)
	counterKey := shaleKeyRoomBytes(appSlug)

	// Materialize the namespace for the pure per-room cap math (outside the
	// tx). A room is capped at 256 keys / 256 KiB, so this is a small map.
	kv, err := r.ScanRoom(appSlug, id)
	if err != nil {
		return err
	}
	if err := kv.CanPut(key, val); err != nil {
		return ErrRoomDataFull // value-too-large is also "room full"
	}
	priorScan := 0
	if existing, ok := kv.Values[key]; ok {
		priorScan = len(existing)
	}

	// Service-wide cap: SOFT cross-shard pre-check (the same posture as the
	// paste / site paths). The shared sumServiceWideActiveBytes counts version
	// bytes + site bytes + room bytes, so a room PUT sees paste + site bytes.
	if serviceCap > 0 && int64(len(val)-priorScan) > 0 {
		total, err := r.sumServiceWideActiveBytes()
		if err != nil {
			return fmt.Errorf("service-wide sum: %w", err)
		}
		if total+int64(len(val)-priorScan) > serviceCap {
			return ErrServiceFull
		}
	}

	// Authoritative single-shard CAS on {app-slug}: re-check the room exists,
	// re-read the value key (so the byte delta is computed against the value
	// the CAS will overwrite, race-safe even if a concurrent writer changed
	// it since the scan), enforce the strict per-app cap, write the value,
	// and touch the room (move the expiry index).
	return r.cluster.Transact(valueKey, func(tx backend.Transaction) error {
		// Room-exists re-check INSIDE the write boundary so a concurrent
		// expiry sweep cannot delete the room between the service's GetRoom
		// and this write.
		var row roomRow
		if err := shaleTxGetJSON(tx, roomKey, &row); err != nil {
			return err // ErrNotFound if the room is gone
		}

		// Re-read the value key to compute the authoritative byte delta. This
		// is the race-safety re-read: the CAS read-check makes a concurrent
		// write of the same key conflict (the closure retries). The delta is
		// computed on DECODED lengths (the sentinel prefix is not charged), so
		// the per-app counter matches the slate byte accounting exactly.
		prior := 0
		if cur, err := tx.Get(valueKey); err == nil {
			prior = shaleDecodedRoomValueLen(cur)
		} else if !errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("re-read room value: %w", err)
		}
		delta := int64(len(val) - prior)

		// Per-app aggregate: read-check-increment the counter in the same CAS
		// (strict ceiling). Charge only a positive delta.
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

		// Touch the room: reset the clock + move the expiry index entry.
		return shaleTxTouchRoom(tx, appSlug, id, &row, now)
	})
}

// DeleteRoomValue removes key and resets the retention clock (a delete is a
// write). Idempotent: deleting an absent key succeeds. One {app-slug}-shard
// CAS: re-check the room exists, delete the value, decrement the per-app
// counter by the freed bytes, and touch the room. Returns ErrNotFound only
// when the ROOM does not exist.
func (r *ShaleRepo) DeleteRoomValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) error {
	valueKey := shaleKeyRoomValue(appSlug, id, key)
	roomKey := shaleKeyRoom(appSlug, id)
	counterKey := shaleKeyRoomBytes(appSlug)
	return r.cluster.Transact(valueKey, func(tx backend.Transaction) error {
		var row roomRow
		if err := shaleTxGetJSON(tx, roomKey, &row); err != nil {
			return err // ErrNotFound if the room is gone
		}
		// Read the value to learn its byte count (to credit the counter). The
		// freed bytes are the DECODED length (the sentinel prefix was never
		// charged), so the counter decrement matches the increment PutValue
		// made.
		freed := int64(0)
		if cur, err := tx.Get(valueKey); err == nil {
			freed = int64(shaleDecodedRoomValueLen(cur))
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
		return shaleTxTouchRoom(tx, appSlug, id, &row, now)
	})
}

// shaleTxTouchRoom resets the room's retention clock to now + the window
// inside the caller's CAS, moving the roomexpiry index entry from the old
// ExpiresAt to the new one (delete-then-add). Every key it touches (the room
// record, both expiry index entries) co-shards with the value on {app-slug},
// so the whole touch stays in the one CAS. Mutates *row in place.
func shaleTxTouchRoom(tx backend.Transaction, appSlug domain.Slug, id domain.RoomID, row *roomRow, now time.Time) error {
	oldExpiry := row.ExpiresAt
	row.UpdatedAt = now
	row.ExpiresAt = now.Add(domain.RoomRetentionWindow)
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
// (one read per app counter). Best-effort, used for the SOFT service-wide cap
// pre-check. Like the shale paste + site counters it has NO read-time expiry
// awareness: an expired-unswept room's bytes leave the counter at sweep time
// (DeleteRoom), not read time, so this over-counts transiently but never
// under-counts (conformCaps.ExpiryFreesQuotaAtReadTime = false on shale).
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

// ExpiredRoomKeys fans out across all {app-slug} shards over roomexpiry/ and
// returns the RoomRefs whose <ts> is <= now (inclusive boundary). The expiry
// keys use the fixed-width expirySiteTimeFormat, so the timestamp segment's
// byte order is time order EXACTLY (correct within a shared whole second).
func (r *ShaleRepo) ExpiredRoomKeys(now time.Time) ([]domain.RoomRef, error) {
	items, err := r.aggregatePrefix(prefixRoomExpiryAll)
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(expirySiteTimeFormat)
	var out []domain.RoomRef
	for _, item := range items {
		// key shape: roomexpiry/<ts>/<app-slug>/<uuid>
		rest := strings.TrimPrefix(string(item.Key), "roomexpiry/")
		tsEnd := strings.IndexByte(rest, '/')
		if tsEnd < 0 {
			continue
		}
		ts := rest[:tsEnd]
		appAndID := rest[tsEnd+1:]
		sep := strings.IndexByte(appAndID, '/')
		if sep < 0 {
			continue
		}
		if ts > cutoff {
			continue
		}
		out = append(out, domain.RoomRef{
			AppSlug: domain.Slug(appAndID[:sep]),
			ID:      domain.RoomID(appAndID[sep+1:]),
		})
	}
	return out, nil
}

// DeleteRoom removes a room record, its expiry index entry, and EVERY value
// in its namespace, then decrements the per-app counter by the freed bytes -
// all on the one {app-slug} shard. The value keys are enumerated OUTSIDE the
// CAS (ScanPrefix is not allowed inside) but every delete + the counter
// decrement land in ONE CAS, so the room and all its values vanish atomically
// (the shale analogue of the sqlite FK cascade). Idempotent: a missing room
// is a no-op. The sweep calls this for each expired RoomRef.
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
