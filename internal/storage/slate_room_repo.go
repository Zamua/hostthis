// Package storage's SlateDB-backed room (app-persistence) KV store.
//
// SlateDB-backed twin of room_repo.go (the sqlite RoomKVRepo). Adds the
// room key families to the SAME SlateDB instance the paste + site keys
// live in, so a room create/write commits in one transaction and the
// service-wide quota sum (sumServiceWideActiveBytes in slate_repo.go) can
// include room bytes alongside paste + site bytes.
//
// The room interface method names (GetRoom, GetValue, ScanRoom, PutValue,
// DeleteValue, CreateRoom, CountRoomCreates, plus the sweep-side
// ExpiredRoomKeys / DeleteRoom / PruneOldRoomCreates) do not collide with
// the paste / site method names, so they could live directly on SlateRepo.
// They live there as `...Room` operations and a thin SlateRoomRepo adapter
// exposes them under the service.RoomRepo + service.SweepRooms interface
// names, mirroring SlateSiteRepo so the wiring shape is uniform across the
// site and room tiers. NewSlateRoomRepo(repo) builds the adapter.
//
// Canonical layout in docs/SPEC.md "Room storage on the slatedb (and
// shale) backend".
//
// # Key layout
//
//	rooms/<app-slug>/<uuid>                   JSON {CreatedAt, UpdatedAt, ExpiresAt} (the room record)
//	roomkv/<app-slug>/<uuid>/<key>            raw value bytes (verbatim; NOT JSON)
//	roomcreate/<app-slug>/<subnet>/<ts>/<uuid>  empty value (one marker per room created; ts fixed-width, uuid disambiguates same-ms creates)
//	roomexpiry/<ts>/<app-slug>/<uuid>         empty value (sweep prefix scan; ts fixed-width)
//
// The room value is stored VERBATIM (not JSON-wrapped): hostthis never
// parses a room value, so roomkv/... holds the exact bytes the app PUT.
// The two marker families carry an empty value, the slatedb index-key
// convention (mirroring identity_pastes / expiry_sites). The room record's
// ExpiresAt is the retention clock; byte + key totals are NOT stored on it
// - they are computed by prefix-scanning roomkv/<app>/<uuid>/ at PUT time,
// the same way the sqlite backend materializes the namespace for the pure
// RoomKV.CanPut cap math.
//
// # Fixed-width TTL timestamp
//
// The roomexpiry and roomcreate <ts> segments use expirySiteTimeFormat
// (the zero-padded 9-digit-nanos RFC3339, the SAME format the site expiry
// index uses), so a string compare on the key is byte order == time order
// EXACTLY, including within a shared whole second. This is NOT
// time.RFC3339Nano (variable-width, sorts wrong within a second). The room
// expiry index is new with no prod data, so it adopts the fixed-width
// format from the start - the same lesson the site expiry index learned.

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
// SlateRepo. It delegates the interface-named methods to the SlateRepo
// `...Room` methods, so the slatedb backend's room repo shares the same
// SlateDB instance (and quota accounting) as its paste + site repos.
type SlateRoomRepo struct {
	repo *SlateRepo
}

// NewSlateRoomRepo wraps a SlateRepo so the no-auth room-persistence tier
// runs on the slatedb backend. The returned adapter satisfies
// service.RoomRepo and service.SweepRooms (and so the cmd/hostthisd
// roomStore union).
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
func (s *SlateRoomRepo) PutValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap, serviceCap int64, now time.Time) error {
	return s.repo.PutRoomValue(appSlug, id, key, val, appCap, serviceCap, now)
}
func (s *SlateRoomRepo) DeleteValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) error {
	return s.repo.DeleteRoomValue(appSlug, id, key, now)
}
func (s *SlateRoomRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	return s.repo.CountRoomCreates(appSlug, subnet, now, window)
}

// service.SweepRooms
func (s *SlateRoomRepo) ExpiredRoomKeys(now time.Time) ([]domain.RoomRef, error) {
	return s.repo.ExpiredRoomKeys(now)
}
func (s *SlateRoomRepo) DeleteRoom(appSlug domain.Slug, id domain.RoomID) error {
	return s.repo.DeleteRoom(appSlug, id)
}
func (s *SlateRoomRepo) PruneOldRoomCreates(cutoff time.Time) (int, error) {
	return s.repo.PruneOldRoomCreates(cutoff)
}

// --- JSON row schema -------------------------------------------------------

// roomRow is the persisted shape of a Room record. CreatedAt / UpdatedAt /
// ExpiresAt are the retention clock, maintained on every backend. ByteTotal +
// KeyCount are the room's running per-room cap totals, maintained ONLY by the
// SHALE backend: shale validates the per-room cap inside its single-shard CAS
// and a CAS read-set cannot include a ScanPrefix (no phantom protection), so it
// needs a discrete in-record total the read-set can carry. The slatedb + sqlite
// backends leave both at zero and compute the per-room cap by materializing the
// namespace under a serialized writer (slatedb's per-room lockQuota stripe,
// sqlite's serializable tx), so they never read these fields. Since a room is
// only ever written by one backend's store, the shale-only totals are inert on
// the others. They are `omitempty` so the slatedb/sqlite-written record's JSON
// shape is unchanged (the fields simply do not appear).
type roomRow struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at"`
	ByteTotal int64     `json:"byte_total,omitempty"` // shale-only running per-room byte total
	KeyCount  int       `json:"key_count,omitempty"`  // shale-only running per-room key count
}

func roomRowFromDomain(r domain.Room) roomRow {
	return roomRow{CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ExpiresAt: r.ExpiresAt}
}

func (row roomRow) toDomain(appSlug domain.Slug, id domain.RoomID) domain.Room {
	return domain.Room{
		AppSlug:   appSlug,
		ID:        id,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		ExpiresAt: row.ExpiresAt,
	}
}

// --- Key builders ----------------------------------------------------------

func keyRoom(appSlug domain.Slug, id domain.RoomID) []byte {
	return []byte("rooms/" + appSlug.String() + "/" + id.String())
}

// keyRoomValue is the namespacing-triple key (app-slug, room-uuid, key) in
// path order. The structural isolation guarantee falls out of this shape:
// see docs/SPEC.md "Strict isolation is structural in the key shape".
func keyRoomValue(appSlug domain.Slug, id domain.RoomID, key string) []byte {
	return []byte("roomkv/" + appSlug.String() + "/" + id.String() + "/" + key)
}

// prefixRoomValues bounds a whole-room scan to exactly one room's subtree.
func prefixRoomValues(appSlug domain.Slug, id domain.RoomID) []byte {
	return []byte("roomkv/" + appSlug.String() + "/" + id.String() + "/")
}

// keyRoomCreate is the creation-ledger marker. The room id is the trailing
// segment so two rooms created under the SAME (app, subnet) at the SAME
// timestamp get DISTINCT keys (without it, same-ms creations would collide on
// one key and overwrite, undercounting the rate limit). The <ts> is
// fixed-width and the <uuid> is slash-free, so a windowed compare reads the
// ts as the second-to-last segment.
func keyRoomCreate(appSlug domain.Slug, subnet string, id domain.RoomID, t time.Time) []byte {
	return []byte("roomcreate/" + appSlug.String() + "/" + subnet + "/" + t.UTC().Format(expirySiteTimeFormat) + "/" + id.String())
}

func prefixAppRoomCreates(appSlug domain.Slug) []byte {
	return []byte("roomcreate/" + appSlug.String() + "/")
}

func keyRoomExpiry(t time.Time, appSlug domain.Slug, id domain.RoomID) []byte {
	return []byte("roomexpiry/" + t.UTC().Format(expirySiteTimeFormat) + "/" + appSlug.String() + "/" + id.String())
}

func prefixRoomExpiry() []byte { return []byte("roomexpiry/") }

// --- Room operations (on SlateRepo) ----------------------------------------

// CreateRoom mints an empty room, records its creation-accounting marker,
// and enforces the per-app aggregate cap, all in one transaction. Holds the
// per-APP quota stripe (lockQuota on the app slug, the room analogue of the
// paste/site per-identity stripe - here keyed on the app slug because the
// room aggregate cap is per-app) across the pre-check + the write so two
// concurrent same-app creates cannot both pass a stale cap.
//
// The creation rate-limit DECISION is read by the service via
// CountRoomCreates OUTSIDE this transaction (so the creation gate stays a
// SOFT bound); CreateRoom only records the marker so the next caller's count
// is accurate. Returns ErrSlugTaken on the astronomically-unlikely
// (app, id) collision (service retries), ErrAppRoomsFull past the per-app
// byte cap.
func (r *SlateRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	defer r.lockQuota(room.AppSlug.String())()

	// Per-app aggregate pre-check: refuse a new room once the app is already
	// at its byte cap. A brand-new room is empty, but bounding creation here
	// keeps a full app from accumulating unbounded empty rooms (mirrors the
	// sqlite CreateRoom).
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
	// Collision check: an existing record means the minted id is taken.
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
	if err := tx.Put(keyRoomExpiry(room.ExpiresAt, room.AppSlug, room.ID), []byte{}); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put room expiry index: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create room %s/%s: %w", room.AppSlug, room.ID, err)
	}
	return nil
}

// GetRoom returns the room record for (appSlug, id) or ErrNotFound. Like
// the paste/site reads it returns expired-but-unswept rows (the HTTP layer
// 404s them, the sweep deletes them). A well-formed id naming no room
// returns ErrNotFound (the existence-not-leaked 404).
func (r *SlateRepo) GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error) {
	var row roomRow
	if err := r.getJSON(keyRoom(appSlug, id), &row); err != nil {
		return domain.Room{}, err
	}
	return row.toDomain(appSlug, id), nil
}

// GetRoomValue returns one value scoped by (appSlug, id, key), or
// ErrNotFound. A missing key in a real room and a key under a nonexistent
// room both return ErrNotFound (the same not-found shape, so the per-key
// path cannot distinguish "no such room" from "no such key" - the service
// layer does the GetRoom existence check where it needs the distinction).
// The value is returned VERBATIM (the exact bytes the app PUT).
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

// ScanRoom returns the whole namespace for (appSlug, id) as a domain.RoomKV
// (every key -> value), so an app loads full room state in one request. An
// existing room with no values returns an empty (non-nil) RoomKV. The scan
// is a ScanPrefix over roomkv/<app>/<uuid>/ - bounded to exactly one room's
// subtree (the cross-room isolation guarantee). The caller verifies the
// room exists first (the service layer's GetRoom makes a nonexistent room a
// 404, not an empty 200).
func (r *SlateRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	items, err := r.scanPrefix(prefixRoomValues(appSlug, id))
	if err != nil {
		return domain.RoomKV{}, err
	}
	kv := domain.NewRoomKV()
	prefix := string(prefixRoomValues(appSlug, id))
	for _, item := range items {
		// Recover the app-chosen <key> by stripping the namespace prefix.
		// A key may itself contain '/' (app keys like "card/<id>"), so strip
		// only the fixed prefix, not by last-segment split.
		k := strings.TrimPrefix(string(item.Key), prefix)
		kv.Values[k] = append([]byte(nil), item.Value...)
	}
	return kv, nil
}

// PutRoomValue writes val under key in room (appSlug, id), enforcing the
// per-room, per-app, and service-wide caps and resetting the retention
// clock. Holds the per-ROOM quota stripe (lockQuota on app-slug + "/" +
// uuid) across the scan + the write so two concurrent writes to the SAME
// room cannot both pass a stale cap and both commit (valid because SlateDB
// is single-writer: only in-process goroutines can race, and the stripe
// serializes same-room writers). All the cap checks + the upsert + the
// clock reset are one snapshot-isolation transaction.
//
// Returns ErrNotFound if the room is gone, ErrRoomDataFull (413) on the
// per-room cap, ErrAppRoomsFull (507) on the per-app aggregate, ErrServiceFull
// on the service-wide cap.
func (r *SlateRepo) PutRoomValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap, serviceCap int64, now time.Time) error {
	// The per-room stripe serializes same-room writers (so the materialized
	// namespace the CanPut math runs against is not stale by commit time).
	// The per-app + service-wide pre-checks happen under the same stripe.
	defer r.lockQuota(appSlug.String() + "/" + id.String())()

	// Materialize the current namespace for the pure cap math (a room is
	// capped at 256 keys / 256 KiB, so this is a small in-memory map). Done
	// outside the tx because SlateDB has no SUM operator; the per-room stripe
	// already serializes same-room writers and single-writer fencing means no
	// other process can interleave.
	kv, err := r.ScanRoom(appSlug, id)
	if err != nil {
		return err
	}
	if err := kv.CanPut(key, val); err != nil {
		// A value-too-large is also "room full" from the storage contract's
		// view (the write is refused, prior state intact).
		return ErrRoomDataFull
	}

	// Byte DELTA this write adds (replacing a key frees its old bytes).
	prior := 0
	if existing, ok := kv.Values[key]; ok {
		prior = len(existing)
	}
	delta := int64(len(val) - prior)

	// Per-app aggregate: charge only the positive delta.
	if appCap > 0 && delta > 0 {
		total, err := r.sumAppRoomBytes(appSlug)
		if err != nil {
			return fmt.Errorf("app room bytes: %w", err)
		}
		if total+delta > appCap {
			return ErrAppRoomsFull
		}
	}

	// Service-wide cap (pastes + sites + rooms): charge the same delta
	// against the whole-service active-byte total, so a room write
	// participates in --storage-cap-bytes exactly as a paste insert / site
	// deploy does.
	if serviceCap > 0 && delta > 0 {
		total, err := r.sumServiceWideActiveBytes(now)
		if err != nil {
			return fmt.Errorf("service-wide sum: %w", err)
		}
		if total+delta > serviceCap {
			return ErrServiceFull
		}
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Room-exists re-check INSIDE the write boundary so a concurrent expiry
	// sweep cannot delete the room between the service's GetRoom and here.
	var row roomRow
	if err := txGetJSON(tx, keyRoom(appSlug, id), &row); err != nil {
		_ = tx.Rollback()
		return err // ErrNotFound if the room is gone
	}

	// Upsert the value (verbatim). slatedb Put accepts arbitrary bytes
	// (unlike shale, no empty-value restriction), so the app's exact bytes
	// round-trip - including an empty value if the app PUT one.
	if err := tx.Put(keyRoomValue(appSlug, id, key), val); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put room value: %w", err)
	}

	// Touch the room: reset UpdatedAt + ExpiresAt and move the expiry index
	// entry (remove-and-re-add, the same shape the paste expiry index uses).
	if err := r.txTouchRoom(tx, appSlug, id, &row, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit put room value %s/%s: %w", appSlug, id, err)
	}
	return nil
}

// DeleteRoomValue removes key from room (appSlug, id) and resets the
// retention clock (a delete is a write). Idempotent: deleting an absent key
// succeeds (the post-condition "the key is gone" holds either way). Returns
// ErrNotFound only when the ROOM itself does not exist.
func (r *SlateRepo) DeleteRoomValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) error {
	defer r.lockQuota(appSlug.String() + "/" + id.String())()
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	var row roomRow
	if err := txGetJSON(tx, keyRoom(appSlug, id), &row); err != nil {
		_ = tx.Rollback()
		return err // ErrNotFound if the room is gone
	}
	if err := tx.Delete(keyRoomValue(appSlug, id, key)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete room value: %w", err)
	}
	if err := r.txTouchRoom(tx, appSlug, id, &row, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete room value %s/%s: %w", appSlug, id, err)
	}
	return nil
}

// txTouchRoom resets the room's retention clock to now + the room retention
// window inside the caller's tx, moving the roomexpiry index entry from the
// old ExpiresAt to the new one (delete-then-add, like the paste append). It
// mutates *row in place so the caller sees the new clock if it needs it.
func (r *SlateRepo) txTouchRoom(tx *slatedb.DbTransaction, appSlug domain.Slug, id domain.RoomID, row *roomRow, now time.Time) error {
	oldExpiry := row.ExpiresAt
	row.UpdatedAt = now
	row.ExpiresAt = now.Add(domain.RoomRetentionWindow)
	if err := txPutJSON(tx, keyRoom(appSlug, id), *row); err != nil {
		return err
	}
	if err := tx.Delete(keyRoomExpiry(oldExpiry, appSlug, id)); err != nil {
		return fmt.Errorf("delete old room expiry idx: %w", err)
	}
	if err := tx.Put(keyRoomExpiry(row.ExpiresAt, appSlug, id), []byte{}); err != nil {
		return fmt.Errorf("put new room expiry idx: %w", err)
	}
	return nil
}

// CountRoomCreates returns how many rooms have been created from subnet AND
// under appSlug within the window ending at now (perSubnet, perApp). The
// service layer uses the two counts for the room-creation rate limit before
// minting a new id. The per-app count is a ScanPrefix on
// roomcreate/<app>/; the per-subnet count walks the same per-app family and
// matches the <subnet> segment (the per-app marker family already anchors
// the app, so a per-subnet scan within one app is sufficient for the count
// the service gates each app's creation on). Markers carry the fixed-width
// <ts>; a row counts when its ts is within the window.
func (r *SlateRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	items, err := r.scanPrefix(prefixAppRoomCreates(appSlug))
	if err != nil {
		return 0, 0, err
	}
	cutoff := now.Add(-window).UTC().Format(expirySiteTimeFormat)
	prefix := string(prefixAppRoomCreates(appSlug))
	for _, item := range items {
		rest := strings.TrimPrefix(string(item.Key), prefix)
		rowSubnet, ts, ok := splitRoomCreateRest(rest)
		if !ok {
			continue
		}
		if ts <= cutoff {
			continue // outside the window
		}
		perApp++
		if rowSubnet == subnet {
			perSubnet++
		}
	}
	return perSubnet, perApp, nil
}

// splitRoomCreateRest parses "<subnet>/<ts>/<uuid>" (the part of a roomcreate
// key after the "roomcreate/<app-slug>/" prefix) into (subnet, ts). The uuid
// is the last segment and the ts is the second-to-last; the subnet is
// everything before (it itself contains a '/', e.g. "1.2.3.0/24"), so the
// split strips the two trailing slash-free segments from the right.
func splitRoomCreateRest(rest string) (subnet, ts string, ok bool) {
	lastSlash := strings.LastIndex(rest, "/")
	if lastSlash < 0 {
		return "", "", false
	}
	beforeUUID := rest[:lastSlash] // "<subnet>/<ts>"
	tsSlash := strings.LastIndex(beforeUUID, "/")
	if tsSlash < 0 {
		return "", "", false
	}
	return beforeUUID[:tsSlash], beforeUUID[tsSlash+1:], true
}

// SumActiveRoomBytes returns the total stored value bytes across EVERY app's
// non-expired rooms. Called by sumServiceWideActiveBytes (slate_repo.go) so
// room bytes count toward --storage-cap-bytes alongside pastes and sites.
func (r *SlateRepo) SumActiveRoomBytes(now time.Time) (int64, error) {
	// Walk every room record to learn which are non-expired, then sum each
	// live room's value bytes.
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
		if !row.ExpiresAt.After(now) {
			continue // expired-unswept rooms don't count (read-time exclusion)
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

// sumAppRoomBytes sums the value bytes across ALL of one app's rooms
// (roomkv/<app-slug>/), with NO expiry filter - matching the sqlite per-app
// aggregate (appRoomBytesTx, which sums room_kv by app_slug without an expiry
// predicate). So an expired-but-unswept room's bytes still count toward the
// per-app cap until the sweep deletes the room, on EVERY backend: the per-app
// room aggregate is sweep-time on sqlite + slatedb + shale alike (distinct
// from the per-IDENTITY paste/site quota, which sqlite + slatedb free at read
// time). This keeps the per-app cap identical across backends.
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

// ExpiredRoomKeys returns the RoomRefs of rooms whose ExpiresAt is at or
// before now (inclusive boundary). Prefix-scans roomexpiry/; the timestamp
// segment uses the fixed-width expirySiteTimeFormat, so a string compare on
// the ts is byte order == time order EXACTLY (correct even within a shared
// whole second), the same guarantee the site expiry index has.
func (r *SlateRepo) ExpiredRoomKeys(now time.Time) ([]domain.RoomRef, error) {
	items, err := r.scanPrefix(prefixRoomExpiry())
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(expirySiteTimeFormat)
	var out []domain.RoomRef
	for _, item := range items {
		// key shape: roomexpiry/<ts>/<app-slug>/<uuid>
		rest := strings.TrimPrefix(string(item.Key), "roomexpiry/")
		// Split off the leading <ts> (fixed-width, no '/').
		tsEnd := strings.IndexByte(rest, '/')
		if tsEnd < 0 {
			continue
		}
		ts := rest[:tsEnd]
		appAndID := rest[tsEnd+1:]
		// <app-slug>/<uuid>: slug + uuid are both slash-free, so split on '/'.
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
// in its namespace (the slatedb analogue of the sqlite FK cascade:
// enumerate the value subtree, delete each in the tx). Used by the sweep on
// expiry. Idempotent: a missing room is a no-op.
func (r *SlateRepo) DeleteRoom(appSlug domain.Slug, id domain.RoomID) error {
	var row roomRow
	if err := r.getJSON(keyRoom(appSlug, id), &row); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	// Enumerate the value keys to delete (outside the tx).
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
	if err := tx.Delete(keyRoomExpiry(row.ExpiresAt, appSlug, id)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete room expiry idx: %w", err)
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

// PruneOldRoomCreates deletes roomcreate/ markers whose <ts> is before
// cutoff (now - RoomCreateWindow). Past the window a ledger marker can never
// change a future rate-limit decision, so the sweep drops it each tick to
// keep the family bounded - the same discipline the keygate prune and the
// sqlite room_creates prune use. Returns the number of markers deleted.
func (r *SlateRepo) PruneOldRoomCreates(cutoff time.Time) (int, error) {
	items, err := r.scanPrefix([]byte("roomcreate/"))
	if err != nil {
		return 0, err
	}
	cutoffStr := cutoff.UTC().Format(expirySiteTimeFormat)
	var toDelete [][]byte
	for _, item := range items {
		// key shape: roomcreate/<app-slug>/<subnet>/<ts>/<uuid>; the <ts> is
		// the SECOND-to-last segment (the <uuid> is the trailing one, added so
		// same-ms creations get distinct keys; the subnet itself contains a
		// '/'). Strip the trailing uuid, then the ts is the last segment.
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
			toDelete = append(toDelete, item.Key)
		}
	}
	if len(toDelete) == 0 {
		return 0, nil
	}
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	for _, k := range toDelete {
		if err := tx.Delete(k); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("delete room-create marker %s: %w", k, err)
		}
	}
	if _, err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit room-create prune: %w", err)
	}
	return len(toDelete), nil
}

// --- helpers ---------------------------------------------------------------

// parseRoomRecordKey extracts (app-slug, room-uuid) from a
// rooms/<app-slug>/<uuid> key. Both segments are slash-free.
func parseRoomRecordKey(key []byte) (domain.Slug, domain.RoomID, bool) {
	rest := strings.TrimPrefix(string(key), "rooms/")
	idx := strings.IndexByte(rest, '/')
	if idx < 0 {
		return "", "", false
	}
	return domain.Slug(rest[:idx]), domain.RoomID(rest[idx+1:]), true
}
