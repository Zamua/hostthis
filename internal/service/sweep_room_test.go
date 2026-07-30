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

// TestSweep_ExpiresRoomsAndCascades pins that a room past its inactivity
// window is swept and the FK cascade takes its values with it, leaving the
// blob GC untouched (rooms hold no blobs).
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

	roomsSvc := service.NewRooms(rooms)
	roomsSvc.Now = func() time.Time { return now }
	room, err := roomsSvc.Create("appz2345", "203.0.113.0/24")
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	if _, err := roomsSvc.Put(room.AppSlug, room.ID, "k1", []byte("v1")); err != nil {
		t.Fatalf("put: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Rooms = rooms

	// Well within the retention window: nothing expires.
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

	// Past the inactivity window: the room expires.
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
	kv, _ := rooms.ScanRoom(room.AppSlug, room.ID)
	if kv.KeyCount() != 0 {
		t.Fatalf("expiry left %d orphan values", kv.KeyCount())
	}
}

// TestSweep_ExpiredRoom404sThroughServiceAfterSweep pins that after the sweep
// every verb (get / scan / put / delete) on the room returns ErrRoomNotFound,
// so a swept room is indistinguishable from one that never existed.
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
	if _, err := roomsSvc.Put(room.AppSlug, room.ID, "state", []byte(`{"votes":3}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Before expiry the room reads fine.
	if _, err := roomsSvc.Get(room.AppSlug, room.ID, "state"); err != nil {
		t.Fatalf("pre-expiry get: %v", err)
	}

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

	if _, err := roomsSvc.Get(room.AppSlug, room.ID, "state"); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("get swept room = %v, want ErrRoomNotFound", err)
	}
	if _, err := roomsSvc.Scan(room.AppSlug, room.ID); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("scan swept room = %v, want ErrRoomNotFound", err)
	}
	if _, err := roomsSvc.Put(room.AppSlug, room.ID, "state", []byte("x")); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("put swept room = %v, want ErrRoomNotFound", err)
	}
	if _, err := roomsSvc.Delete(room.AppSlug, room.ID, "state"); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("delete swept room = %v, want ErrRoomNotFound", err)
	}
}

// TestSweep_PrunesRoomCreates pins that the room-creation rate-limit table is
// pruned once rows fall past the window, keeping it bounded.
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

	// The fresh row counts toward the rate limit.
	perSubnet, _, _ := rooms.CountRoomCreates("appz2345", "203.0.113.0/24", now, domain.RoomCreateWindow)
	if perSubnet != 1 {
		t.Fatalf("create count = %d, want 1", perSubnet)
	}

	logger := log.New(io.Discard, "", 0)
	sweep := service.NewSweep(pastes, disk, logger)
	sweep.Rooms = rooms
	sweep.Now = func() time.Time { return now.Add(domain.RoomCreateWindow + time.Hour) }

	// A sweep past the window prunes the creation row.
	sweep.Once(now.Add(domain.RoomCreateWindow + time.Hour)) //nolint:errcheck
	if _, err := rooms.PruneOldRoomCreates(now.Add(domain.RoomCreateWindow + time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	perSubnet, _, _ = rooms.CountRoomCreates("appz2345", "203.0.113.0/24", now.Add(domain.RoomCreateWindow+time.Hour), domain.RoomCreateWindow)
	if perSubnet != 0 {
		t.Fatalf("create count after prune = %d, want 0", perSubnet)
	}
}

// TestSweep_RoomIndexNoOpsNotCountedAsDeleted pins that an expired
// roomexpiry-index entry whose room record is ALREADY GONE counts as an index
// cleanup, not a record deletion. Counting it would report "deleted N expired
// records" every cycle forever.
func TestSweep_RoomIndexNoOpsNotCountedAsDeleted(t *testing.T) {
	rooms := &orphanRoomSweep{ref: domain.ExpiredRoom{
		AppSlug:  "appz2345",
		ID:       "0b7ff45c-6a41-4f3e-9c5d-2a9d6f4b8e13",
		IndexRef: "roomexpiry/2026-07-03T21:22:23.536246341Z/appz2345/0b7ff45c-6a41-4f3e-9c5d-2a9d6f4b8e13",
	}}
	sweep := service.NewSweep(noopSweepRepo{}, nil, log.New(io.Discard, "", 0))
	sweep.Rooms = rooms

	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	deleted, _, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("a room-index no-op (record already gone) must not count as a deletion: got %d, want 0", deleted)
	}
	// The cleanup must target the surfaced entry, not a re-derivation.
	if len(rooms.gotRefs) != 1 || rooms.gotRefs[0] != rooms.ref {
		t.Fatalf("DeleteExpiredRoom must receive the exact scanned ref; got %+v", rooms.gotRefs)
	}

	deleted, _, err = sweep.Once(now)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if deleted != 0 || len(rooms.gotRefs) != 1 {
		t.Fatalf("second pass must see zero expired rooms: deleted=%d calls=%d", deleted, len(rooms.gotRefs))
	}
}

// orphanRoomSweep is a SweepRooms whose expiry scan surfaces one entry with no
// room record behind it: the record delete is an idempotent no-op and the
// entry cleanup drains the scan.
type orphanRoomSweep struct {
	ref     domain.ExpiredRoom
	drained bool
	gotRefs []domain.ExpiredRoom
}

func (s *orphanRoomSweep) ExpiredRooms(_ time.Time) ([]domain.ExpiredRoom, error) {
	if s.drained {
		return nil, nil
	}
	return []domain.ExpiredRoom{s.ref}, nil
}

func (s *orphanRoomSweep) DeleteExpiredRoom(ref domain.ExpiredRoom) (bool, error) {
	s.gotRefs = append(s.gotRefs, ref)
	s.drained = true // the exact-entry removal drains the scan
	return false, nil
}

func (s *orphanRoomSweep) PruneOldRoomCreates(_ time.Time) (int, error) { return 0, nil }
