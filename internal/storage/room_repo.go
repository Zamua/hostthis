package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// ErrRoomDataFull is returned when a write would push one room past its
// per-room byte or key-count cap; the prior value is left intact. The service
// layer maps it to a 413. Alias of the domain-owned sentinel.
var ErrRoomDataFull = domain.ErrRoomDataFull

// ErrAppRoomsFull is returned when a room creation or write would push an
// APP's aggregate room bytes past the per-app cap. The service layer maps it
// to a 507. Alias of the domain-owned sentinel.
var ErrAppRoomsFull = domain.ErrAppRoomsFull

// RoomKVRepo is the sqlite-backed room persistence tier. Namespacing is
// STRICT: every read and write is scoped by the (app_slug, room_id) pair, so a
// request carrying one room's UUID can only address keys under that room, and
// a forged or guessed id addresses an empty keyspace.
type RoomKVRepo struct {
	db *sql.DB
}

func NewRoomKVRepo(db *sql.DB) *RoomKVRepo { return &RoomKVRepo{db: db} }

// CreateRoom inserts an empty room owned by appSlug, enforcing the per-app
// aggregate cap (ErrAppRoomsFull) and recording the creation-accounting row,
// all in one serializable tx.
//
// The rate-limit DECISION runs in the service layer via CountRoomCreates,
// OUTSIDE this tx, which makes the creation rate limit a SOFT bound: N
// concurrent creators can all read the same in-window count and pass before any
// accounting row commits, so the cap can be slightly overshot. Accepted - the
// creation gate is a coarse abuse bound, and the hard structural limit on what
// an app can store is the per-app byte cap enforced here plus the per-room cap
// enforced in PutValue.
//
// Returns ErrSlugTaken when the (app, id) pair exists; the service retries with
// a fresh id (astronomically unlikely for a v4).
func (r *RoomKVRepo) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// A brand-new room is empty, but refusing creation at the byte cap keeps
	// a full app from accumulating unbounded EMPTY rooms.
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
		INSERT INTO rooms (app_slug, room_id, created_at, updated_at)
		VALUES (?, ?, ?, ?)
	`, room.AppSlug.String(), room.ID.String(),
		formatTime(room.CreatedAt), formatTime(room.UpdatedAt)); err != nil {
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
func (r *RoomKVRepo) GetRoom(appSlug domain.Slug, id domain.RoomID) (domain.Room, error) {
	row := r.db.QueryRow(`
		SELECT app_slug, room_id, created_at, updated_at
		FROM rooms WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String())
	var appStr, idStr, created, updated string
	if err := row.Scan(&appStr, &idStr, &created, &updated); err != nil {
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
	}, nil
}

// GetValue returns one value, scoped by (appSlug, id, key). A missing key and
// a missing room both return ErrNotFound, so this layer leaks no per-key
// existence; a caller that needs the distinction checks the room separately.
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

// ScanRoom returns the whole namespace for (appSlug, id), stamped with the
// EXACT per-room sequence the snapshot reflects (RoomKV.Seq). The seq read and
// the namespace scan share ONE serializable transaction, so every mutation with
// seq <= Seq is in the state and none with seq > Seq is: the fence the relay's
// late-join splice contract rides.
//
// An existing room with no values returns an empty (non-nil) RoomKV, and a
// nonexistent room scans empty at seq 0 - indistinguishable here, so the
// service layer's prior GetRoom is what makes a missing room a 404.
func (r *RoomKVRepo) ScanRoom(appSlug domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return domain.RoomKV{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	kv := domain.NewRoomKV()

	// Read inside the same tx as the scan, so the stamped Seq is exactly the
	// state the scan returns.
	var seq sql.NullInt64
	err = tx.QueryRow(`
		SELECT seq FROM rooms WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String()).Scan(&seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.RoomKV{}, fmt.Errorf("scan room seq: %w", err)
	}
	kv.Seq = uint64(seq.Int64)

	rows, err := tx.Query(`
		SELECT key, val FROM room_kv WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String())
	if err != nil {
		return domain.RoomKV{}, fmt.Errorf("scan room: %w", err)
	}
	defer rows.Close() //nolint:errcheck
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
	if err := rows.Err(); err != nil {
		return domain.RoomKV{}, err
	}
	// database/sql requires no open rows on the tx at Commit time, and the
	// deferred Close would run too late.
	if err := rows.Close(); err != nil {
		return domain.RoomKV{}, err
	}
	return kv, tx.Commit()
}

// PutValue writes val under key in room (appSlug, id), in ONE serializable tx:
// re-check the room exists (ErrNotFound), enforce the per-room byte + key-count
// caps via the pure domain CanPut (ErrRoomDataFull) and the per-app aggregate
// on the delta (ErrAppRoomsFull), upsert the value, bump the room's clock.
//
// The single tx is what stops two concurrent writes to the same room both
// passing a stale cap check and both committing. The room-exists re-check must
// be inside it too, so a concurrent delete cannot remove the room between the
// service's GetRoom and this write.
//
// Rooms hold no blobs, so this path touches no object-store quota and has no
// service-wide byte cap (SPEC "Rooms -> Quota and abuse -> Durable total-bytes
// ceiling").
//
// Returns the assigned per-room sequence: a dense counter incremented by
// exactly one inside the tx that commits the value (SPEC "The per-room
// sequence: assignment at commit"). The relay stamps it onto the mirror frame.
func (r *RoomKVRepo) PutValue(appSlug domain.Slug, id domain.RoomID, key string, val []byte, appCap int64, now time.Time) (uint64, error) {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := roomExistsTx(tx, appSlug, id); err != nil {
		return 0, err
	}

	// Materializing the namespace for the pure cap check is cheap: a room is
	// capped at 256 keys / 256 KiB.
	kv, err := scanRoomTx(tx, appSlug, id)
	if err != nil {
		return 0, err
	}
	if err := kv.CanPut(key, val); err != nil {
		// Every domain cap error collapses to the one storage sentinel the
		// service keys on: from the storage contract's view a value-too-large
		// is also "room full" (write refused, state intact).
		return 0, ErrRoomDataFull
	}

	// Replacing an existing key frees its old bytes.
	prior := 0
	if existing, ok := kv.Values[key]; ok {
		prior = len(existing)
	}
	delta := int64(len(val) - prior)

	// Per-app aggregate charges only the delta.
	if appCap > 0 && delta > 0 {
		total, err := appRoomBytesTx(tx, appSlug.String())
		if err != nil {
			return 0, err
		}
		if total+delta > appCap {
			return 0, ErrAppRoomsFull
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO room_kv (app_slug, room_id, key, val, val_size)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (app_slug, room_id, key)
		DO UPDATE SET val = excluded.val, val_size = excluded.val_size
	`, appSlug.String(), id.String(), key, val, len(val)); err != nil {
		return 0, fmt.Errorf("put room value: %w", err)
	}

	seq, err := touchRoomTx(tx, appSlug, id, now)
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// DeleteValue removes key and bumps the room's clock (a delete is a write). Idempotent: deleting an absent key succeeds. Returns ErrNotFound only
// when the ROOM does not exist.
//
// Returns the assigned per-room sequence, like PutValue. The absent-key delete
// commits and assigns a seq too, because a seq bump with no mirror frame reads
// as a permanent hole to a relay subscriber (SPEC "The per-room sequence:
// assignment at commit").
func (r *RoomKVRepo) DeleteValue(appSlug domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	tx, err := r.db.BeginTx(context.Background(), &txSerializable)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := roomExistsTx(tx, appSlug, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`
		DELETE FROM room_kv WHERE app_slug = ? AND room_id = ? AND key = ?
	`, appSlug.String(), id.String(), key); err != nil {
		return 0, fmt.Errorf("delete room value: %w", err)
	}
	seq, err := touchRoomTx(tx, appSlug, id, now)
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// CountRoomCreates returns the in-window (perSubnet, perApp) creation counts
// the service layer's room-creation rate limit reads before minting a new id.
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

// AppRoomBytes returns the total stored value bytes across all of an app's
// rooms: the figure the per-app aggregate cap bounds.
func (r *RoomKVRepo) AppRoomBytes(appSlug domain.Slug) (int64, error) {
	var n sql.NullInt64
	if err := r.db.QueryRow(`
		SELECT COALESCE(SUM(val_size), 0) FROM room_kv WHERE app_slug = ?
	`, appSlug.String()).Scan(&n); err != nil {
		return 0, fmt.Errorf("app room bytes: %w", err)
	}
	return n.Int64, nil
}

// DeleteRoom removes a room and, via the FK cascade, every value in its
// namespace. Idempotent.
func (r *RoomKVRepo) DeleteRoom(appSlug domain.Slug, id domain.RoomID) error {
	if _, err := r.db.Exec(`
		DELETE FROM rooms WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String()); err != nil {
		return fmt.Errorf("delete room: %w", err)
	}
	return nil
}

// PruneOldRoomCreates deletes room_creates rows older than cutoff: past the
// rate-limit window they can never affect a future decision. The sweep calls it
// each tick so the table stays bounded.
func (r *RoomKVRepo) PruneOldRoomCreates(cutoff time.Time) (int, error) {
	res, err := r.db.Exec(`DELETE FROM room_creates WHERE created_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune room creates: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SumActiveRoomBytes returns the total stored value bytes across every app's
// rooms. Observability only: the durable total-bytes ceiling lives
// at the object store (the blob bucket quota), not in a sum of room bytes.
func (r *RoomKVRepo) SumActiveRoomBytes(_ time.Time) (int64, error) {
	tx, err := r.db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	total, err := activeRoomBytesTx(tx)
	if err != nil {
		return 0, err
	}
	return total, tx.Commit()
}

// activeRoomBytesTx sums stored value bytes across every app's rooms inside the
// caller's tx.
func activeRoomBytesTx(tx *sql.Tx) (int64, error) {
	var n sql.NullInt64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(kv.val_size), 0)
		FROM room_kv kv
		JOIN rooms ro ON ro.app_slug = kv.app_slug AND ro.room_id = kv.room_id
	`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sum active room bytes (tx): %w", err)
	}
	return n.Int64, nil
}

// --- tx-scoped helpers (run inside a caller's serializable tx) ---

// roomExistsTx returns ErrNotFound when (appSlug, id) names no room. Called
// inside PutValue/DeleteValue so existence is re-checked within the same
// serializable boundary as the write, closing the race with a concurrent delete.
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
	defer rows.Close() //nolint:errcheck
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

// appRoomBytesTx sums an app's stored value bytes inside the caller's tx: every
// room_kv row counts, and bytes are freed only by an explicit delete.
func appRoomBytesTx(tx *sql.Tx, appSlug string) (int64, error) {
	var n sql.NullInt64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(val_size), 0) FROM room_kv WHERE app_slug = ?
	`, appSlug).Scan(&n); err != nil {
		return 0, fmt.Errorf("app room bytes (tx): %w", err)
	}
	return n.Int64, nil
}

// touchRoomTx bumps updated_at and assigns the mutation's per-room sequence
// (seq = prior + 1, in the same UPDATE), returning it. Every write calls it; the
// serializable tx is what makes the increment dense and unique under concurrent
// same-room writers.
func touchRoomTx(tx *sql.Tx, appSlug domain.Slug, id domain.RoomID, now time.Time) (uint64, error) {
	if _, err := tx.Exec(`
		UPDATE rooms SET updated_at = ?, seq = seq + 1
		WHERE app_slug = ? AND room_id = ?
	`, formatTime(now), appSlug.String(), id.String()); err != nil {
		return 0, fmt.Errorf("touch room: %w", err)
	}
	var seq uint64
	if err := tx.QueryRow(`
		SELECT seq FROM rooms WHERE app_slug = ? AND room_id = ?
	`, appSlug.String(), id.String()).Scan(&seq); err != nil {
		return 0, fmt.Errorf("read assigned room seq: %w", err)
	}
	return seq, nil
}
