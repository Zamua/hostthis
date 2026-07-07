package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// ErrRoomDataFull is returned when accepting a write would push a single
// room past its per-room byte or key-count cap. The prior value is left
// intact. The service layer maps it to a 413.
var ErrRoomDataFull = errors.New("storage: room is at its data cap")

// ErrAppRoomsFull is returned when accepting a room creation or a write
// would push an APP's aggregate room bytes past the per-app cap. The
// service layer maps it to a 507.
var ErrAppRoomsFull = errors.New("storage: app room storage is at capacity")

// RoomKVRepo is the sqlite-backed implementation of the room
// persistence tier. It stores rooms + their key-value namespaces with
// STRICT namespacing: every read and write is scoped by the
// (app_slug, room_id) pair, so a request carrying one room's UUID can
// only ever address keys under that room, and one app's rooms are
// structurally separate from another app's. A forged or guessed id
// addresses an empty keyspace.
//
// It lives alongside PasteRepo / SiteRepo and shares the same db.
type RoomKVRepo struct {
	db *sql.DB
}

func NewRoomKVRepo(db *sql.DB) *RoomKVRepo { return &RoomKVRepo{db: db} }

// CreateRoom inserts a new empty room owned by appSlug with the minted
// id, recording a creation row for the per-IP / per-app rate limit and
// (atomically, under the serializable tx):
//  1. enforces the per-app aggregate cap (room creation is refused once
//     the app is already at its byte aggregate) -> ErrAppRoomsFull
//  2. inserts the room record (created_at = updated_at = now,
//     expires_at = now + RoomRetentionWindow)
//  3. inserts the creation-accounting row (app, subnet, now)
//
// The rate-limit DECISION (count in-window creations) is made by the
// service layer via CountRoomCreates BEFORE this call, OUTSIDE this
// transaction; CreateRoom only records the accounting row so the count is
// accurate for the next caller. Because the count runs outside the tx, the
// creation rate limit is a SOFT bound, not a strictly-serialized one: N
// concurrent creators can each read the same in-window count and all pass
// before any of their accounting rows commit, so the cap can be slightly
// overshot under concurrency. This is an accepted trade - the creation
// gate is a coarse abuse bound (60/IP/hr, 300/app/hr), not a precise
// quota, and the per-app aggregate BYTE cap below (which IS enforced
// inside this serializable tx) is the hard structural bound on how much an
// app can actually store. The per-room byte cap on PutValue is likewise
// hard-enforced inside its own serializable tx.
//
// Returns ErrSlugTaken if the (app, id) pair already exists - the
// service retries with a fresh id (astronomically unlikely for a v4).
func (r *RoomKVRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// 1. Per-app aggregate: refuse a new room once the app is already at
	// its byte cap. A brand-new room is empty, but bounding creation here
	// keeps a single app from accumulating unbounded EMPTY rooms once it
	// is full of data, and pairs with the per-write check below.
	if appCap > 0 {
		total, err := appRoomBytesTx(tx, room.AppSlug.String())
		if err != nil {
			return err
		}
		if total >= appCap {
			return ErrAppRoomsFull
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO rooms (app_slug, room_id, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
	`, room.AppSlug.String(), room.ID.String(),
		formatTime(room.CreatedAt), formatTime(room.UpdatedAt), formatSiteExpiry(room.ExpiresAt)); err != nil {
		if isUniqueViolation(err) {
			return ErrSlugTaken
		}
		return fmt.Errorf("insert room: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO room_creates (app_slug, ip_subnet, created_at)
		VALUES (?, ?, ?)
	`, room.AppSlug.String(), subnet, formatTime(now)); err != nil {
		return fmt.Errorf("record room create: %w", err)
	}
	return tx.Commit()
}

// GetRoom returns the room record for (appSlug, id), or ErrNotFound.
// Like the paste/site reads it returns expired-but-not-yet-swept rows;
// the HTTP layer 404s them and the sweep deletes them. A well-formed id
// that names no room returns ErrNotFound (the existence-not-leaked 404).
func (r *RoomKVRepo) GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error) {
	row := r.db.QueryRow(`
		SELECT app_slug, room_id, created_at, updated_at, expires_at
		FROM rooms WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String())
	var appStr, idStr, created, updated, expires string
	if err := row.Scan(&appStr, &idStr, &created, &updated, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Room{}, ErrNotFound
		}
		return domain.Room{}, fmt.Errorf("scan room: %w", err)
	}
	return domain.Room{
		AppSlug:   domain.Slug(appStr),
		ID:        domain.RoomID(idStr),
		CreatedAt: parseTime(created),
		UpdatedAt: parseTime(updated),
		ExpiresAt: parseSiteExpiry(expires), // fixed-width column
	}, nil
}

// GetValue returns one value, scoped by (appSlug, id, key). Returns
// ErrNotFound when the key (or the room) does not exist - the same
// not-found shape either way, so a caller cannot distinguish "missing
// key in a real room" from "no such room" at this layer (the service /
// HTTP layer checks the room separately when it needs to).
func (r *RoomKVRepo) GetValue(appSlug domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	var val []byte
	err := r.db.QueryRow(`
		SELECT val FROM room_kv WHERE app_slug = ? AND room_id = ? AND key = ?
	`, appSlug.String(), id.String(), key).Scan(&val)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get room value: %w", err)
	}
	return val, nil
}

// ScanRoom returns the whole namespace for (appSlug, id) as a domain
// RoomKV (every key -> value), so an app can load full room state in one
// request. An existing room with no values returns an empty (non-nil)
// RoomKV. Caller verifies the room exists first (a scan of a nonexistent
// room returns an empty namespace, indistinguishable here - the service
// layer does the GetRoom existence check before calling this so a
// missing room is a 404, not an empty 200).
func (r *RoomKVRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	rows, err := r.db.Query(`
		SELECT key, val FROM room_kv WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String())
	if err != nil {
		return domain.RoomKV{}, fmt.Errorf("scan room: %w", err)
	}
	defer rows.Close()
	kv := domain.NewRoomKV()
	for rows.Next() {
		var key string
		var val []byte
		if err := rows.Scan(&key, &val); err != nil {
			return domain.RoomKV{}, err
		}
		// Copy: the driver may reuse the val backing array across Next().
		cp := make([]byte, len(val))
		copy(cp, val)
		kv.Values[key] = cp
	}
	return kv, rows.Err()
}

// PutValue writes val under key in room (appSlug, id), atomically (under
// the serializable tx):
//  1. verifies the room exists (ErrNotFound otherwise - existence is the
//     caller's gate; we re-check inside the tx so a concurrent expiry
//     sweep cannot delete the room between the service's GetRoom and this
//     write)
//  2. enforces the per-room byte + key-count caps using the PURE domain
//     CanPut math against the room's current state -> ErrRoomDataFull
//  3. enforces the per-app aggregate byte cap on the post-write delta ->
//     ErrAppRoomsFull
//  4. upserts the value row
//  5. resets the room's retention clock (updated_at + expires_at)
//
// Rooms hold no blobs, so a room write touches no object-store quota and
// there is no service-wide byte cap on this path (see SPEC "Rooms ->
// Quota and abuse -> Durable total-bytes ceiling"). The whole thing is one
// serializable transaction so two concurrent writes to the same room
// cannot both pass a stale cap check and both commit.
func (r *RoomKVRepo) PutValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := roomExistsTx(tx, appSlug, id); err != nil {
		return err
	}

	// Materialize the current namespace for the pure cap check. A room is
	// capped at 256 keys / 256 KiB, so this is a small in-memory map.
	kv, err := scanRoomTx(tx, appSlug, id)
	if err != nil {
		return err
	}
	if err := kv.CanPut(key, val); err != nil {
		// Map the domain cap errors onto the storage sentinel the service
		// layer keys on. A value-too-large is also "room full" from the
		// storage contract's view (the write is refused, state intact).
		return ErrRoomDataFull
	}

	// The byte DELTA this write adds (replacing an existing key frees its
	// old bytes). Used by the per-app aggregate check.
	prior := 0
	if existing, ok := kv.Values[key]; ok {
		prior = len(existing)
	}
	delta := int64(len(val) - prior)

	// Per-app aggregate: charge only the delta.
	if appCap > 0 && delta > 0 {
		total, err := appRoomBytesTx(tx, appSlug.String())
		if err != nil {
			return err
		}
		if total+delta > appCap {
			return ErrAppRoomsFull
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO room_kv (app_slug, room_id, key, val, val_size)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (app_slug, room_id, key)
		DO UPDATE SET val = excluded.val, val_size = excluded.val_size
	`, appSlug.String(), id.String(), key, val, len(val)); err != nil {
		return fmt.Errorf("put room value: %w", err)
	}

	if err := touchRoomTx(tx, appSlug, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteValue removes key from room (appSlug, id) and resets the room's
// retention clock (a delete is a write). Idempotent: deleting an absent
// key succeeds (the post-condition "the key is gone" holds either way).
// Returns ErrNotFound only when the ROOM itself does not exist.
func (r *RoomKVRepo) DeleteValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := roomExistsTx(tx, appSlug, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		DELETE FROM room_kv WHERE app_slug = ? AND room_id = ? AND key = ?
	`, appSlug.String(), id.String(), key); err != nil {
		return fmt.Errorf("delete room value: %w", err)
	}
	if err := touchRoomTx(tx, appSlug, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// CountRoomCreates returns how many rooms have been created from subnet
// AND under appSlug within the window ending at now. The service layer
// uses the two counts (per-IP and per-app) for the room-creation rate
// limit before minting a new id. Returned as (perSubnet, perApp).
func (r *RoomKVRepo) CountRoomCreates(appSlug domain.Slug, subnet string, now time.Time, window time.Duration) (perSubnet, perApp int, err error) {
	windowStart := formatTime(now.Add(-window))
	if err := r.db.QueryRow(`
		SELECT COUNT(*) FROM room_creates WHERE ip_subnet = ? AND created_at > ?
	`, subnet, windowStart).Scan(&perSubnet); err != nil {
		return 0, 0, fmt.Errorf("count creates by subnet: %w", err)
	}
	if err := r.db.QueryRow(`
		SELECT COUNT(*) FROM room_creates WHERE app_slug = ? AND created_at > ?
	`, appSlug.String(), windowStart).Scan(&perApp); err != nil {
		return 0, 0, fmt.Errorf("count creates by app: %w", err)
	}
	return perSubnet, perApp, nil
}

// AppRoomBytes returns the total stored value bytes across ALL of an
// app's rooms - the figure the per-app aggregate cap bounds. Exposed for
// the service layer + tests.
func (r *RoomKVRepo) AppRoomBytes(appSlug domain.Slug) (int64, error) {
	var n sql.NullInt64
	if err := r.db.QueryRow(`
		SELECT COALESCE(SUM(val_size), 0) FROM room_kv WHERE app_slug = ?
	`, appSlug.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("app room bytes: %w", err)
	}
	return n.Int64, nil
}

// ExpiredRooms returns one reference per room whose expires_at is at or
// before now. The sweep processes each (deleting the record, which cascades
// to its values); the HTTP read path 404s expired-but-not-yet-deleted rooms
// inline. The sqlite scan reads the rooms table itself (no standalone expiry
// index to fall out of sync with the records), so IndexRef is always empty
// and a returned reference always names a live row at scan time.
func (r *RoomKVRepo) ExpiredRooms(now time.Time) ([]domain.ExpiredRoom, error) {
	rows, err := r.db.Query(`
		SELECT app_slug, room_id FROM rooms WHERE expires_at <= ?
	`, formatSiteExpiry(now))
	if err != nil {
		return nil, fmt.Errorf("expired rooms: %w", err)
	}
	defer rows.Close()
	var out []domain.ExpiredRoom
	for rows.Next() {
		var ref domain.ExpiredRoom
		var appStr, idStr string
		if err := rows.Scan(&appStr, &idStr); err != nil {
			return nil, err
		}
		ref.AppSlug = domain.Slug(appStr)
		ref.ID = domain.RoomID(idStr)
		out = append(out, ref)
	}
	return out, rows.Err()
}

// DeleteExpiredRoom processes one expired reference: the same full-cascade
// delete as DeleteRoom, reporting whether a room row was actually removed.
// On sqlite there is no standalone expiry-index entry to clean (the scan IS
// the rooms table), so a missing row is simply a no-op that returns false.
func (r *RoomKVRepo) DeleteExpiredRoom(ref domain.ExpiredRoom) (bool, error) {
	res, err := r.db.Exec(`
		DELETE FROM rooms WHERE app_slug = ? AND room_id = ?
	`, ref.AppSlug.String(), ref.ID.String())
	if err != nil {
		return false, fmt.Errorf("delete expired room %s/%s: %w", ref.AppSlug, ref.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete expired room %s/%s: rows affected: %w", ref.AppSlug, ref.ID, err)
	}
	return n > 0, nil
}

// DeleteRoom removes a room and (via the FK cascade) every value in its
// namespace. Idempotent. The internal cascade the expiry pass reuses
// (through DeleteExpiredRoom); not called by the sweep directly.
func (r *RoomKVRepo) DeleteRoom(appSlug domain.Slug, id domain.RoomID) error {
	if _, err := r.db.Exec(`
		DELETE FROM rooms WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String()); err != nil {
		return fmt.Errorf("delete room: %w", err)
	}
	return nil
}

// PruneOldRoomCreates deletes room_creates rows older than cutoff - past
// the rate-limit window they can never affect a future decision. The
// sweep calls this each tick so the table stays bounded. Returns the
// number of rows deleted.
func (r *RoomKVRepo) PruneOldRoomCreates(cutoff time.Time) (int, error) {
	res, err := r.db.Exec(`DELETE FROM room_creates WHERE created_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune room creates: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SumActiveRoomBytes returns the total stored value bytes across every
// app's non-expired rooms. Exposed for observability and tests; the
// durable total-bytes ceiling lives at the object store (the blob bucket
// quota), not in an app-level sum of room bytes.
func (r *RoomKVRepo) SumActiveRoomBytes(now time.Time) (int64, error) {
	tx, err := r.db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	total, err := activeRoomBytesTx(tx, formatSiteExpiry(now))
	if err != nil {
		return 0, err
	}
	return total, tx.Commit()
}

// activeRoomBytesTx sums stored value bytes across every app's non-expired
// rooms inside the caller's tx. Used by SumActiveRoomBytes. The room
// expires_at column is fixed-width (formatSiteExpiry), so nowStr must be
// the fixed-width form, matching the stored column's byte-order ==
// time-order layout.
func activeRoomBytesTx(tx *sql.Tx, nowStr string) (int64, error) {
	var n sql.NullInt64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(kv.val_size), 0)
		FROM room_kv kv
		JOIN rooms ro ON ro.app_slug = kv.app_slug AND ro.room_id = kv.room_id
		WHERE ro.expires_at > ?
	`, nowStr).Scan(&n); err != nil {
		return 0, fmt.Errorf("sum active room bytes (tx): %w", err)
	}
	return n.Int64, nil
}

// --- tx-scoped helpers (run inside a caller's serializable tx) ---

// roomExistsTx returns ErrNotFound when (appSlug, id) names no room. Used
// inside PutValue/DeleteValue so a write re-checks existence within the
// same serializable boundary that does the write - closing the race with
// a concurrent expiry sweep.
func roomExistsTx(tx *sql.Tx, appSlug domain.Slug, id domain.RoomID) error {
	var one int
	err := tx.QueryRow(`
		SELECT 1 FROM rooms WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("room exists check: %w", err)
	}
	return nil
}

// scanRoomTx materializes a room's namespace inside the caller's tx, for
// the pure cap math.
func scanRoomTx(tx *sql.Tx, appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	rows, err := tx.Query(`
		SELECT key, val FROM room_kv WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String())
	if err != nil {
		return domain.RoomKV{}, fmt.Errorf("scan room (tx): %w", err)
	}
	defer rows.Close()
	kv := domain.NewRoomKV()
	for rows.Next() {
		var key string
		var val []byte
		if err := rows.Scan(&key, &val); err != nil {
			return domain.RoomKV{}, err
		}
		cp := make([]byte, len(val))
		copy(cp, val)
		kv.Values[key] = cp
	}
	return kv, rows.Err()
}

// appRoomBytesTx sums an app's stored value bytes inside the caller's tx,
// with NO expiry predicate: every room_kv row under the app counts, including
// rows of an expired-but-not-yet-swept room. This is DELIBERATE and consistent
// across all three backends (the slatedb sumAppRoomBytes and the shale per-app
// roombytes/<app> counter do the same) - the per-app room aggregate is
// sweep-time, distinct from the per-identity paste/site quota that sqlite +
// slatedb free at read time. The effect is fail-safe: an expired-unswept room's
// bytes are counted until the sweep deletes the room (DeleteRoom cascades its
// values), so the cap can transiently OVER-count (reject a write slightly
// early) but never UNDER-counts (admit past the real cap). The sweep runs on a
// short cadence, so the over-count window is small and self-correcting.
func appRoomBytesTx(tx *sql.Tx, appSlug string) (int64, error) {
	var n sql.NullInt64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(val_size), 0) FROM room_kv WHERE app_slug = ?
	`, appSlug).Scan(&n); err != nil {
		return 0, fmt.Errorf("app room bytes (tx): %w", err)
	}
	return n.Int64, nil
}

// touchRoomTx resets the room's retention clock to now + the room
// retention window. Called by every write (PUT / DELETE).
func touchRoomTx(tx *sql.Tx, appSlug domain.Slug, id domain.RoomID, now time.Time) error {
	expires := now.Add(domain.RoomRetentionWindow)
	if _, err := tx.Exec(`
		UPDATE rooms SET updated_at = ?, expires_at = ?
		WHERE app_slug = ? AND room_id = ?
	`, formatTime(now), formatSiteExpiry(expires), appSlug.String(), id.String()); err != nil {
		return fmt.Errorf("touch room: %w", err)
	}
	return nil
}
