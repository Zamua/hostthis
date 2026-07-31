package service_test

import (
	"context"
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

// TestSweep_PrunesRoomCreates pins that a SWEEP PASS prunes the room-creation
// rate-limit table once rows fall past the window, keeping it bounded.
//
// Driven through Run with an already-canceled context: Run's boot tick runs
// before the loop's first select, so that is exactly one full pass, and the
// prune lives on the pass (tick), not in Once. Calling Once and then pruning by
// hand asserts the test's own setup line and would pass with the sweep's prune
// deleted entirely.
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
	future := now.Add(domain.RoomCreateWindow + time.Hour)
	sweep.Now = func() time.Time { return future }

	// The observation window must still REACH the row, or the count answers
	// "outside the rate-limit window" and reads 0 whether or not anything was
	// pruned - the boundedness claim would then hold with the prune deleted.
	wide := domain.RoomCreateWindow + 2*time.Hour
	if n, _, _ := rooms.CountRoomCreates("appz2345", "203.0.113.0/24", future, wide); n != 1 {
		t.Fatalf("pre-sweep row count over the wide window = %d, want 1; the fixture cannot observe a prune", n)
	}

	// One sweep pass past the window prunes the creation row.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweep.Run(ctx)

	if n, _, _ := rooms.CountRoomCreates("appz2345", "203.0.113.0/24", future, wide); n != 0 {
		t.Fatalf("row count after the sweep pass = %d, want 0; the pass did not prune the room-create table", n)
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
