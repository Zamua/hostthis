package service_test

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// The room-creation rate-limit table is bounded by the COUNT that already reads
// it, not by a background pass: a row past the window can no longer change a
// decision, so the read that would skip it removes it.
//
// The observation window below is deliberately WIDER than the rate-limit
// window. A count taken at the rate-limit width answers "outside the window"
// and reads 0 whether or not anything was pruned, so it would pass with the
// prune deleted entirely.
func TestRoomCreates_CountPrunesPastWindow(t *testing.T) {
	rooms := storage.NewShaleRoomRepo(storagetest.NewRepo(t))

	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	roomsSvc := service.NewRooms(rooms)
	roomsSvc.Now = func() time.Time { return now }
	if _, err := roomsSvc.Create("appz2345", "203.0.113.0/24"); err != nil {
		t.Fatalf("create: %v", err)
	}

	wide := domain.RoomCreateWindow + 2*time.Hour
	if n, _, _ := rooms.CountRoomCreates("appz2345", "203.0.113.0/24", now, wide); n != 1 {
		t.Fatalf("fixture: want the seeded row visible over the wide window, got %d", n)
	}

	// A count taken past the rate-limit window drops the row on the way past.
	future := now.Add(domain.RoomCreateWindow + time.Hour)
	if n, _, _ := rooms.CountRoomCreates("appz2345", "203.0.113.0/24", future, domain.RoomCreateWindow); n != 0 {
		t.Fatalf("an aged-out row must not count: got %d", n)
	}
	if n, _, _ := rooms.CountRoomCreates("appz2345", "203.0.113.0/24", future, wide); n != 0 {
		t.Fatalf("the count must have DELETED the aged-out row, not just skipped it; still visible over the wide window: %d", n)
	}
}
