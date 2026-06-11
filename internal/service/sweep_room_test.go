package service_test

import (
	"errors"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// TestSweep_ExpiresRoomsAndCascades drives the sweep against a real
// sqlite-backed room repo: a room past its 30-day inactivity window is
// deleted by the sweep, and the FK cascade takes its values with it.
// Rooms hold no blobs, so the blob GC is unaffected.
func TestSweep_ExpiresRoomsAndCascades(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disk, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	pastes := storage.NewPasteRepo(db)
	rooms := storage.NewRoomKVRepo(db)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Create a room with a couple of values via the service.
	roomsSvc := service.NewRooms(rooms)
	roomsSvc.Now = func() time.Time { return now }
	room, err := roomsSvc.Create("appz2345", "203.0.113.0/24")
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	if err := roomsSvc.Put(room.AppSlug, room.ID, "k1", []byte("v1")); err != nil {
		t.Fatalf("put: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Rooms = rooms

	// Nothing expired yet (well within the 30-day window).
	deleted, gc, err := sweep.Once(now.Add(time.Hour))
	if err != nil {
		t.Fatalf("early sweep: %v", err)
	}
	if deleted != 0 || gc != 0 {
		t.Fatalf("nothing should sweep early: deleted=%d gc=%d", deleted, gc)
	}
	if _, err := rooms.GetRoom(room.AppSlug, room.ID); err != nil {
		t.Fatalf("room gone too early: %v", err)
	}

	// Past the 30-day inactivity window: the room expires.
	future := now.Add(domain.RoomRetentionWindow + time.Hour)
	deleted, _, err = sweep.Once(future)
	if err != nil {
		t.Fatalf("future sweep: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 room expired, got %d", deleted)
	}
	if _, err := rooms.GetRoom(room.AppSlug, room.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("room survived expiry: %v", err)
	}
	// The cascade left no orphan values.
	kv, _ := rooms.ScanRoom(room.AppSlug, room.ID)
	if kv.KeyCount() != 0 {
		t.Fatalf("expiry left %d orphan values", kv.KeyCount())
	}
}

// TestSweep_ExpiredRoom404sThroughServiceAfterSweep ties the retention
// lifecycle to the read surface: a room past its TTL is swept, and AFTER
// the sweep every service-level verb (get / scan / put / delete) on that
// room returns ErrRoomNotFound - the existence-not-leaked 404. This is the
// end-state a client sees: once the room is gone, addressing it is
// indistinguishable from a never-existed room.
func TestSweep_ExpiredRoom404sThroughServiceAfterSweep(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disk, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	pastes := storage.NewPasteRepo(db)
	rooms := storage.NewRoomKVRepo(db)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	roomsSvc := service.NewRooms(rooms)
	roomsSvc.Now = func() time.Time { return now }
	room, err := roomsSvc.Create("appz2345", "203.0.113.0/24")
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	if err := roomsSvc.Put(room.AppSlug, room.ID, "state", []byte(`{"votes":3}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Before expiry the room reads fine.
	if _, err := roomsSvc.Get(room.AppSlug, room.ID, "state"); err != nil {
		t.Fatalf("pre-expiry get: %v", err)
	}

	// Sweep PAST the 30-day inactivity window: the room is deleted.
	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Rooms = rooms
	future := now.Add(domain.RoomRetentionWindow + time.Hour)
	roomsSvc.Now = func() time.Time { return future }
	deleted, _, err := sweep.Once(future)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 room swept, got %d", deleted)
	}

	// AFTER the sweep, the service 404s the room on every verb - the same
	// not-found shape a never-existed room gets.
	if _, err := roomsSvc.Get(room.AppSlug, room.ID, "state"); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("get swept room = %v, want ErrRoomNotFound", err)
	}
	if _, err := roomsSvc.Scan(room.AppSlug, room.ID); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("scan swept room = %v, want ErrRoomNotFound", err)
	}
	if err := roomsSvc.Put(room.AppSlug, room.ID, "state", []byte("x")); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("put swept room = %v, want ErrRoomNotFound", err)
	}
	if err := roomsSvc.Delete(room.AppSlug, room.ID, "state"); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("delete swept room = %v, want ErrRoomNotFound", err)
	}
}

// TestSweep_PrunesRoomCreates confirms the sweep prunes the bounded
// room-creation rate-limit table once rows fall past the window.
func TestSweep_PrunesRoomCreates(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disk, _ := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	pastes := storage.NewPasteRepo(db)
	rooms := storage.NewRoomKVRepo(db)

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	roomsSvc := service.NewRooms(rooms)
	roomsSvc.Now = func() time.Time { return now }
	if _, err := roomsSvc.Create("appz2345", "203.0.113.0/24"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Immediately after creation the row counts toward the rate limit.
	perSubnet, _, _ := rooms.CountRoomCreates("appz2345", "203.0.113.0/24", now, domain.RoomCreateWindow)
	if perSubnet != 1 {
		t.Fatalf("create count = %d, want 1", perSubnet)
	}

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Rooms = rooms
	sweep.Now = func() time.Time { return now.Add(domain.RoomCreateWindow + time.Hour) }

	// A sweep past the window prunes the creation row (via tick's prune).
	sweep.Once(now.Add(domain.RoomCreateWindow + time.Hour)) //nolint:errcheck
	if _, err := rooms.PruneOldRoomCreates(now.Add(domain.RoomCreateWindow + time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	perSubnet, _, _ = rooms.CountRoomCreates("appz2345", "203.0.113.0/24", now.Add(domain.RoomCreateWindow+time.Hour), domain.RoomCreateWindow)
	if perSubnet != 0 {
		t.Fatalf("create count after prune = %d, want 0", perSubnet)
	}
}
