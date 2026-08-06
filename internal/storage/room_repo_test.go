package storage

// Sqlite-direct room tests, pinning behavior the backend-agnostic conformance
// suite (conformance_rooms_test.go) does not: reading back an overwritten
// value, the clock after a PUT and a DELETE, the sqlite-specific empty
// IndexRef, and SumActiveRoomBytes.
//
// newRoomTestRepo / mkRoom are shared with room_isolation_test.go.

import (
	"bytes"
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
	}
	if err := repo.CreateRoom(room, "10.0.0.0/24", 0, now); err != nil {
		t.Fatalf("create room: %v", err)
	}
	return room
}

// An overwrite replaces the value and consumes no extra key slot.
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

// A PUT and a DELETE each move the room's UpdatedAt.
func TestRoom_WriteMovesUpdatedAt(t *testing.T) {
	repo := newRoomTestRepo(t)
	created := time.Now().UTC().Truncate(time.Second)
	room := mkRoom(repo, t, "app12345", created)

	later := created.Add(5 * 24 * time.Hour)
	if _, err := repo.PutValue(room.AppSlug, room.ID, "k", []byte("v"), 0, later); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, _ := repo.GetRoom(room.AppSlug, room.ID)
	if !got.UpdatedAt.Equal(later) {
		t.Fatalf("put did not move UpdatedAt: got %v, want %v", got.UpdatedAt, later)
	}
	evenLater := later.Add(24 * time.Hour)
	if _, err := repo.DeleteValue(room.AppSlug, room.ID, "k", evenLater); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = repo.GetRoom(room.AppSlug, room.ID)
	if !got.UpdatedAt.Equal(evenLater) {
		t.Fatalf("delete did not move UpdatedAt: got %v", got.UpdatedAt)
	}
}

// SumActiveRoomBytes totals live value bytes across every app.
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
