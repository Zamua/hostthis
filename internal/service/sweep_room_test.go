package service_test

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

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
