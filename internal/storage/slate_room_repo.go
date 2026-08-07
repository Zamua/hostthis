// SlateDB-backed room (app-persistence) KV store.
//
// The room key families live in the SAME SlateDB instance as the paste and
// site keys, so a room create/write commits in one transaction. The room
// methods live on SlateRepo; SlateRoomRepo adapts them to the
// service.RoomRepo + service.SweepRooms names.
//
// Layout: docs/SPEC.md "Room storage on the slatedb (and shale) backend".
//
// # Key layout
//
//	rooms/<app-slug>/<uuid>                     JSON room record
//	roomkv/<app-slug>/<uuid>/<key>              raw value bytes, NOT JSON
//	roomcreate/<app-slug>/<subnet>/<ts>/<uuid>  empty value, one per create
//
// Room values are stored verbatim: hostthis never parses one. Marker families
// carry empty values, the slatedb index-key convention.
//
// Byte and key totals are not stored on the room record; they are computed by
// prefix-scanning roomkv/<app>/<uuid>/ at put time.
//
// # Fixed-width ledger timestamp
//
// The <ts> segments use sortableTimeFormat (zero-padded 9-digit nanos), so a
// string compare on the key is byte order == time order exactly, including
// within a shared second. time.RFC3339Nano is variable-width and sorts wrong
// within a second.

//go:build slatedb

package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	slatedb "slatedb.io/slatedb-go/uniffi"

	"github.com/Zamua/hostthis/internal/domain"
)

// SlateRoomRepo is the service.RoomRepo + service.SweepRooms adapter over a
// SlateRepo, so the slatedb backend's room repo shares one SlateDB instance
// (and quota accounting) with its paste + site repos.
type SlateRoomRepo struct {
	repo *SlateRepo
}

// NewSlateRoomRepo wraps a SlateRepo. The returned adapter satisfies
// service.RoomRepo and service.SweepRooms.
func NewSlateRoomRepo(repo *SlateRepo) *SlateRoomRepo { return &SlateRoomRepo{repo: repo} }

// service.RoomRepo
func (s *SlateRoomRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	return s.repo.CreateRoom(room, subnet, appCap, now)
}
func (s *SlateRoomRepo) GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error) {
	return s.repo.GetRoom(appSlug, id)
}
func (s *SlateRoomRepo) GetValue(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	return s.repo.GetRoomValue(appSlug, id, key)
}
func (s *SlateRoomRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	return s.repo.ScanRoom(appSlug, id)
}
func (s *SlateRoomRepo) PutValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap int64, now time.Time) (uint64, error) {
	return s.repo.PutRoomValue(appSlug, id, key, val, appCap, now)
}
func (s *SlateRoomRepo) DeleteValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	return s.repo.DeleteRoomValue(appSlug, id, key, now)
}
func (s *SlateRoomRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	return s.repo.CountRoomCreates(appSlug, subnet, now, window)
}

// --- JSON row schema -------------------------------------------------------

// roomRow is the persisted shape of a Room record. ByteTotal + KeyCount are
// maintained ONLY by the shale backend: shale validates the per-room cap
// inside a single-shard CAS, and a CAS read-set cannot include a ScanPrefix
// (no phantom protection), so it needs a discrete in-record total the read-set
// can carry. slatedb leaves both at zero and computes the per-room cap
// by materializing the namespace under a serialized writer (slatedb's per-room
// lockQuota stripe), so it never reads these
// fields. A room is only ever written by one backend's store, so the
// shale-only totals are inert on the others; omitempty keeps them out of the
// slatedb-written JSON entirely.
// --- Key builders ----------------------------------------------------------

func keyRoom(appSlug domain.Slug, id domain.RoomID) []byte {
	return []byte("rooms/" + appSlug.String() + "/" + id.String())
}

// keyRoomValue is the namespacing triple (app-slug, room-uuid, key) in path
// order. Cross-room isolation falls out of this shape: see docs/SPEC.md
// "Strict isolation is structural in the key shape".
func keyRoomValue(appSlug domain.Slug, id domain.RoomID, key string) []byte {
	return []byte("roomkv/" + appSlug.String() + "/" + id.String() + "/" + key)
}

// prefixRoomValues bounds a whole-room scan to exactly one room's subtree.
func prefixRoomValues(appSlug domain.Slug, id domain.RoomID) []byte {
	return []byte("roomkv/" + appSlug.String() + "/" + id.String() + "/")
}

// keyRoomCreate is the creation-ledger marker. The room id is the trailing
// segment so two rooms created under the SAME (app, subnet) at the SAME
// timestamp get distinct keys; without it they would collide on one key and
// undercount the rate limit. The <ts> is fixed-width and the <uuid> is
// slash-free, so a windowed compare reads the ts as the second-to-last
// segment.
func keyRoomCreate(appSlug domain.Slug, subnet string, id domain.RoomID, t time.Time) []byte {
	return []byte("roomcreate/" + appSlug.String() + "/" + subnet + "/" + t.UTC().Format(sortableTimeFormat) + "/" + id.String())
}

func prefixAppRoomCreates(appSlug domain.Slug) []byte {
	return []byte("roomcreate/" + appSlug.String() + "/")
}

// --- Room operations (on SlateRepo) ----------------------------------------

// CreateRoom mints an empty room, records its creation-accounting marker, and
// enforces the per-app aggregate cap in one transaction. It holds the per-APP
// quota stripe (lockQuota on the app slug, since the room aggregate cap is
// per-app) across the pre-check and the write so two concurrent same-app
// creates cannot both pass a stale cap.
//
// The rate-limit DECISION is made by the service via CountRoomCreates OUTSIDE
// this transaction, so the creation gate is a SOFT bound; CreateRoom only
// records the marker. Returns ErrSlugTaken on an (app, id) collision (the
// service retries), ErrAppRoomsFull past the per-app byte cap.
func (r *SlateRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	defer r.lockQuota(room.AppSlug.String())()

	// A brand-new room is empty, but refusing creation once the app is at its
	// byte cap keeps a full app from accumulating unbounded empty rooms.
	if appCap > 0 {
		total, err := r.sumAppRoomBytes(room.AppSlug)
		if err != nil {
			return fmt.Errorf("app room bytes: %w", err)
		}
		if total >= appCap {
			return ErrAppRoomsFull
		}
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	existing, err := tx.Get(keyRoom(room.AppSlug, room.ID))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tx room collision check: %w", err)
	}
	if existing != nil {
		_ = tx.Rollback()
		return ErrSlugTaken
	}
	if err := txPutJSON(tx, keyRoom(room.AppSlug, room.ID), roomRowFromDomain(room)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Put(keyRoomCreate(room.AppSlug, subnet, room.ID, now), []byte{}); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put room-create marker: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create room %s/%s: %w", room.AppSlug, room.ID, err)
	}
	return nil
}

// GetRoom returns the room record for (appSlug, id) or ErrNotFound. Like the
// paste/site reads it returns stale rows; the HTTP layer 404s
// them and the sweep deletes them.
func (r *SlateRepo) GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error) {
	var row roomRow
	if err := r.getJSON(keyRoom(appSlug, id), &row); err != nil {
		return domain.Room{}, err
	}
	return row.toDomain(appSlug, id), nil
}

// GetRoomValue returns one value scoped by (appSlug, id, key), verbatim, or
// ErrNotFound. A missing key in a real room and a key under a nonexistent room
// are indistinguishable here; the service layer does a GetRoom existence check
// where it needs the distinction.
func (r *SlateRepo) GetRoomValue(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	raw, err := r.db.Get(keyRoomValue(appSlug, id, key))
	if err != nil {
		return nil, fmt.Errorf("get room value: %w", err)
	}
	if raw == nil {
		return nil, ErrNotFound
	}
	// Copy out of the slatedb-owned buffer.
	out := append([]byte(nil), *raw...)
	return out, nil
}

// ScanRoom returns the whole namespace for (appSlug, id), stamped with the
// EXACT per-room sequence the snapshot reflects (RoomKV.Seq). Exactness rides
// the per-ROOM quota stripe: every same-room mutation holds it across its
// record touch + value write, so holding it here across the record read (the
// seq) and the namespace scan means no commit can interleave between them.
// Valid because SlateDB is single-writer: only in-process goroutines can race.
//
// An existing room with no values returns an empty (non-nil) RoomKV; a
// nonexistent room scans empty at seq 0, and the service layer's GetRoom turns
// that into a 404 rather than an empty 200.
func (r *SlateRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	defer r.lockQuota(appSlug.String() + "/" + id.String())()

	var row roomRow
	seq := uint64(0)
	if err := r.getJSON(keyRoom(appSlug, id), &row); err == nil {
		seq = row.Seq
	} else if !errors.Is(err, ErrNotFound) {
		return domain.RoomKV{}, err
	}
	kv, err := r.scanRoomValues(appSlug, id)
	if err != nil {
		return domain.RoomKV{}, err
	}
	kv.Seq = seq
	return kv, nil
}

// scanRoomValues materializes the room's namespace WITHOUT taking the
// per-room stripe and without the seq stamp. Callers that already hold the
// stripe must use this: the public ScanRoom takes the stripe itself and would
// self-deadlock.
func (r *SlateRepo) scanRoomValues(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	items, err := r.scanPrefix(prefixRoomValues(appSlug, id))
	if err != nil {
		return domain.RoomKV{}, err
	}
	kv := domain.NewRoomKV()
	prefix := string(prefixRoomValues(appSlug, id))
	for _, item := range items {
		// A key may itself contain '/' (app keys like "card/<id>"), so recover
		// it by stripping the fixed prefix, not by a last-segment split.
		k := strings.TrimPrefix(string(item.Key), prefix)
		kv.Values[k] = append([]byte(nil), item.Value...)
	}
	return kv, nil
}

// PutRoomValue writes val under key in room (appSlug, id), enforcing the
// per-room and per-app caps. It holds the
// per-ROOM quota stripe across the scan and the write so two concurrent writes
// to the SAME room cannot both pass a stale cap and both commit. Valid because
// SlateDB is single-writer: only in-process goroutines can race. The cap
// checks, the upsert, and the clock bump are one snapshot-isolation
// transaction.
//
// Rooms hold no blobs, so a room write touches no object-store quota and has
// no service-wide byte cap on this path (SPEC "Rooms -> Quota and abuse ->
// Durable total-bytes ceiling").
//
// Returns the assigned per-room sequence. ErrNotFound if the room is gone,
// ErrRoomDataFull (413) on the per-room cap, ErrAppRoomsFull (507) on the
// per-app aggregate.
func (r *SlateRepo) PutRoomValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap int64, now time.Time) (uint64, error) {
	defer r.lockQuota(appSlug.String() + "/" + id.String())()

	// Materialize the namespace for the pure cap math (a room is capped at 256
	// keys / 256 KiB, so this is a small in-memory map). Outside the tx
	// because SlateDB has no SUM operator; the stripe held above is what keeps
	// it from going stale before commit.
	kv, err := r.scanRoomValues(appSlug, id)
	if err != nil {
		return 0, err
	}
	if err := kv.CanPut(key, val); err != nil {
		// A value-too-large is also "room full" in the storage contract: the
		// write is refused, prior state intact.
		return 0, ErrRoomDataFull
	}

	// Byte delta this write adds (replacing a key frees its old bytes).
	prior := 0
	if existing, ok := kv.Values[key]; ok {
		prior = len(existing)
	}
	delta := int64(len(val) - prior)

	// The per-app aggregate charges only the positive delta.
	if appCap > 0 && delta > 0 {
		total, err := r.sumAppRoomBytes(appSlug)
		if err != nil {
			return 0, fmt.Errorf("app room bytes: %w", err)
		}
		if total+delta > appCap {
			return 0, ErrAppRoomsFull
		}
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}

	// Room-exists re-check INSIDE the write boundary so a concurrent delete
	// cannot remove the room between the service's GetRoom and here.
	var row roomRow
	if err := txGetJSON(tx, keyRoom(appSlug, id), &row); err != nil {
		_ = tx.Rollback()
		return 0, err // ErrNotFound if the room is gone
	}

	// slatedb Put accepts arbitrary bytes (no empty-value restriction), so the
	// app's exact bytes round-trip, including an empty value.
	if err := tx.Put(keyRoomValue(appSlug, id, key), val); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("put room value: %w", err)
	}

	if err := r.txTouchRoom(tx, appSlug, id, &row, now); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if _, err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit put room value %s/%s: %w", appSlug, id, err)
	}
	return row.Seq, nil
}

// DeleteRoomValue removes key from room (appSlug, id) and bumps the room's
// clock (a delete is a write). Idempotent: deleting an absent key succeeds and
// still assigns a seq, because it commits a touch and a seq bump rides every
// commit. Returns ErrNotFound only when the ROOM itself does not exist.
func (r *SlateRepo) DeleteRoomValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	defer r.lockQuota(appSlug.String() + "/" + id.String())()
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	var row roomRow
	if err := txGetJSON(tx, keyRoom(appSlug, id), &row); err != nil {
		_ = tx.Rollback()
		return 0, err // ErrNotFound if the room is gone
	}
	if err := tx.Delete(keyRoomValue(appSlug, id, key)); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("delete room value: %w", err)
	}
	if err := r.txTouchRoom(tx, appSlug, id, &row, now); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if _, err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit delete room value %s/%s: %w", appSlug, id, err)
	}
	return row.Seq, nil
}

// txTouchRoom bumps the room's UpdatedAt inside the caller's tx and assigns the
// mutation's per-room sequence. The per-room stripe every caller holds is what
// makes the seq increment dense and unique. *row is mutated in place so the
// caller sees the new clock and assigned seq.
func (r *SlateRepo) txTouchRoom(tx *slatedb.DbTransaction, appSlug domain.Slug, id domain.RoomID, row *roomRow, now time.Time) error {
	row.UpdatedAt = now
	row.Seq++
	return txPutJSON(tx, keyRoom(appSlug, id), *row)
}

// CountRoomCreates returns how many rooms were created from subnet and under
// appSlug within the window ending at now, feeding the service's creation rate
// limit. Both counts walk roomcreate/<app>/ only: the per-subnet figure gates
// creation WITHIN one app, so scanning that app's marker family is sufficient.
func (r *SlateRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	items, err := r.scanPrefix(prefixAppRoomCreates(appSlug))
	if err != nil {
		return 0, 0, err
	}
	cutoff := now.Add(-window).UTC().Format(sortableTimeFormat)
	prefix := string(prefixAppRoomCreates(appSlug))
	var expired [][]byte
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), prefix)
		rowSubnet, ts, ok := splitRoomCreateRest(rest)
		if !ok {
			continue
		}
		if ts <= cutoff {
			// Outside the window: it can no longer change any decision, so the
			// scan that already walked past it drops it (docs/SPEC.md "Room
			// creation rate limit").
			expired = append(expired, item.Key)
			continue
		}
		perApp++
		if rowSubnet == subnet {
			perSubnet++
		}
	}
	if len(expired) > 0 {
		tx, terr := r.db.Begin(slatedb.IsolationLevelSnapshot)
		if terr != nil {
			return 0, 0, fmt.Errorf("begin prune tx: %w", terr)
		}
		for _, k := range expired {
			if derr := tx.Delete(k); derr != nil {
				_ = tx.Rollback()
				return 0, 0, fmt.Errorf("drop expired create marker %s: %w", k, derr)
			}
		}
		if _, cerr := tx.Commit(); cerr != nil {
			return 0, 0, fmt.Errorf("commit create-marker prune: %w", cerr)
		}
	}
	return perSubnet, perApp, nil
}

// SumActiveRoomBytes returns the total stored value bytes across every app's
// rooms. Observability and tests only: the durable total-bytes
// ceiling lives at the object store's blob-bucket quota, not here.
func (r *SlateRepo) SumActiveRoomBytes(_ time.Time) (int64, error) {
	rooms, err := r.scanPrefix([]byte("rooms/"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range rooms {
		var row roomRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		appSlug, id, ok := parseRoomRecordKey(item.Key)
		if !ok {
			continue
		}
		bytes, err := r.sumOneRoomBytes(appSlug, id)
		if err != nil {
			return 0, err
		}
		total += bytes
	}
	return total, nil
}

// sumAppRoomBytes sums the value bytes across ALL of one app's rooms; bytes are
// freed only by an explicit delete.
func (r *SlateRepo) sumAppRoomBytes(appSlug domain.Slug) (int64, error) {
	items, err := r.scanPrefix([]byte("roomkv/" + appSlug.String() + "/"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range items {
		total += int64(len(item.Value))
	}
	return total, nil
}

// sumOneRoomBytes sums the value-byte lengths under one room's subtree.
func (r *SlateRepo) sumOneRoomBytes(appSlug domain.Slug, id domain.RoomID) (int64, error) {
	items, err := r.scanPrefix(prefixRoomValues(appSlug, id))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range items {
		total += int64(len(item.Value))
	}
	return total, nil
}

// --- SweepRooms ------------------------------------------------------------

// DeleteRoom removes a room record and EVERY value in
// its namespace: the slatedb analogue of a relational FK cascade, enumerating the
// value subtree and deleting each in the tx. Idempotent; a missing room is a
// no-op.
func (r *SlateRepo) DeleteRoom(appSlug domain.Slug, id domain.RoomID) error {
	var row roomRow
	if err := r.getJSON(keyRoom(appSlug, id), &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	values, err := r.scanPrefix(prefixRoomValues(appSlug, id))
	if err != nil {
		return err
	}
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := tx.Delete(keyRoom(appSlug, id)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete room record: %w", err)
	}
	for _, item := range values {
		if err := tx.Delete(item.Key); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete room value %s: %w", item.Key, err)
		}
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete room %s/%s: %w", appSlug, id, err)
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

// parseRoomRecordKey extracts (app-slug, room-uuid) from a
// rooms/<app-slug>/<uuid> key; both segments are slash-free.
func parseRoomRecordKey(key []byte) (domain.Slug, domain.RoomID, bool) {
	rest := strings.TrimPrefix(string(key), "rooms/")
	before, after, ok := strings.Cut(rest, "/")
	if !ok {
		return "", "", false
	}
	return domain.Slug(before), domain.RoomID(after), true
}
