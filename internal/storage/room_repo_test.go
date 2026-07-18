package storage

// Sqlite-direct room tests. Most of the original TestRoom_* suite was
// folded into the backend-agnostic conformance suite, which runs the same
// assertions against sqlite on every default `go test` run (the
// conformRoom* subtests in conformance_rooms_test.go). The tests below
// stay because they pin behavior the conformance suite does NOT:
//
//   - TestRoom_Overwrite: reads BACK an overwritten value (and that the
//     overwrite added no key slot). Conformance admits an overwrite at the
//     key cap but never re-reads the replaced value.
//   - TestRoom_WriteResetsRetentionClock: asserts GetRoom().ExpiresAt
//     directly after a PUT and after a DELETE. Conformance pins the PUT
//     reset only indirectly (expiry-boundary math) and never pins that a
//     DELETE resets the clock.
//   - TestRoom_ExpiryAndCascade: pins the sqlite-specific contract that
//     ExpiredRooms returns an EMPTY IndexRef (the scan reads the rooms
//     table itself; no standalone index). Conformance is backend-agnostic
//     and treats IndexRef as opaque.
//   - TestRoom_SumActiveRoomBytes: SumActiveRoomBytes (the service-wide
//     cap input) has no conformance counterpart.
//
// The newRoomTestRepo / mkRoom helpers are shared with
// room_isolation_test.go.

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func newRoomTestRepo(t *testing.T) *RoomKVRepo {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewRoomKVRepo(db)
}

func mkRoom(repo *RoomKVRepo, t *testing.T, app string, now time.Time) domain.Room {
	t.Helper()
	room := domain.Room{
		AppSlug:   domain.Slug(app),
		ID:        domain.NewRoomID(),
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RoomRetentionWindow),
	}
	if err := repo.CreateRoom(room, "10.0.0.0/24", 0, now); err != nil {
		t.Fatalf("create room: %v", err)
	}
	return room
}

func TestRoom_Overwrite(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	room := mkRoom(repo, t, "app12345", now)
	if _, err := repo.PutValue(room.AppSlug, room.ID, "k", []byte("v1"), 0, now); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := repo.PutValue(room.AppSlug, room.ID, "k", []byte("v2-longer"), 0, now); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	v, _ := repo.GetValue(room.AppSlug, room.ID, "k")
	if !bytes.Equal(v, []byte("v2-longer")) {
		t.Fatalf("after overwrite = %q", v)
	}
	kv, _ := repo.ScanRoom(room.AppSlug, room.ID)
	if kv.KeyCount() != 1 {
		t.Fatalf("overwrite added a key: count = %d", kv.KeyCount())
	}
}

func TestRoom_WriteResetsRetentionClock(t *testing.T) {
	repo := newRoomTestRepo(t)
	created := time.Now().UTC().Truncate(time.Second)
	room := mkRoom(repo, t, "app12345", created)

	later := created.Add(5 * 24 * time.Hour)
	if _, err := repo.PutValue(room.AppSlug, room.ID, "k", []byte("v"), 0, later); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, _ := repo.GetRoom(room.AppSlug, room.ID)
	wantExpiry := later.Add(domain.RoomRetentionWindow)
	if !got.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("put did not reset clock: expires %v, want %v", got.ExpiresAt, wantExpiry)
	}
	// A DELETE also resets the clock.
	evenLater := later.Add(24 * time.Hour)
	if _, err := repo.DeleteValue(room.AppSlug, room.ID, "k", evenLater); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = repo.GetRoom(room.AppSlug, room.ID)
	if !got.ExpiresAt.Equal(evenLater.Add(domain.RoomRetentionWindow)) {
		t.Fatalf("delete did not reset clock: expires %v", got.ExpiresAt)
	}
}

func TestRoom_ExpiryAndCascade(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	room := mkRoom(repo, t, "app12345", now)
	_, _ = repo.PutValue(room.AppSlug, room.ID, "k", []byte("v"), 0, now)

	// Nothing expired yet.
	expired, err := repo.ExpiredRooms(now)
	if err != nil {
		t.Fatalf("expired rooms: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("room expired prematurely: %d", len(expired))
	}
	// Past the window, the room is expired (IndexRef empty on sqlite: the
	// scan reads the rooms table itself, no standalone index).
	future := now.Add(domain.RoomRetentionWindow + time.Hour)
	expired, _ = repo.ExpiredRooms(future)
	if len(expired) != 1 || expired[0].ID != room.ID || expired[0].IndexRef != "" {
		t.Fatalf("expired set = %+v", expired)
	}
	// Processing the reference deletes the room, cascading to its values.
	deleted, err := repo.DeleteExpiredRoom(expired[0])
	if err != nil {
		t.Fatalf("delete expired room: %v", err)
	}
	if !deleted {
		t.Fatalf("DeleteExpiredRoom must report true for a live room row")
	}
	if _, err := repo.GetRoom(room.AppSlug, room.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("room survived delete: %v", err)
	}
	// The cascade left no orphan values.
	kv, _ := repo.ScanRoom(room.AppSlug, room.ID)
	if kv.KeyCount() != 0 {
		t.Fatalf("FK cascade left %d orphan values", kv.KeyCount())
	}
}

func TestRoom_SumActiveRoomBytes(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	a := mkRoom(repo, t, "app11111", now)
	b := mkRoom(repo, t, "app22222", now)
	_, _ = repo.PutValue(a.AppSlug, a.ID, "k", []byte("12345"), 0, now)
	_, _ = repo.PutValue(b.AppSlug, b.ID, "k", []byte("678"), 0, now)
	total, err := repo.SumActiveRoomBytes(now)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if total != 8 {
		t.Fatalf("sum active room bytes = %d, want 8", total)
	}
}
