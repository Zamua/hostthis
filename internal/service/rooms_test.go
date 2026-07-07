package service

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// newRoomsSvc wires a Rooms service over the real sqlite-backed repo so
// the rate-limit + cap + isolation behavior is exercised end-to-end (an
// integration test, the preferred boundary per CLAUDE.md).
func newRoomsSvc(t *testing.T) (*Rooms, *fixedClock) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := &fixedClock{t: time.Now().UTC().Truncate(time.Second)}
	svc := NewRooms(storage.NewRoomKVRepo(db))
	svc.Now = clk.now
	return svc, clk
}

type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time          { return c.t }
func (c *fixedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestRoomsSvc_CreateAndKVRoundTrip(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	room, err := svc.Create("app12345", "10.0.0.0/24")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := domain.ParseRoomID(room.ID.String()); err != nil {
		t.Fatalf("created id not a valid v4: %v", err)
	}

	if _, err := svc.Put(room.AppSlug, room.ID, "name", []byte("alice")); err != nil {
		t.Fatalf("put: %v", err)
	}
	v, err := svc.Get(room.AppSlug, room.ID, "name")
	if err != nil || !bytes.Equal(v, []byte("alice")) {
		t.Fatalf("get = %q, %v", v, err)
	}
	kv, err := svc.Scan(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if kv.KeyCount() != 1 {
		t.Fatalf("scan count = %d", kv.KeyCount())
	}
}

func TestRoomsSvc_NonexistentRoomIs404Shape(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	// A well-formed but nonexistent room: read, scan, put, delete all
	// surface ErrRoomNotFound (no existence leak).
	id := domain.NewRoomID()
	if _, err := svc.Get("app12345", id, "k"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("get missing room = %v", err)
	}
	if _, err := svc.Scan("app12345", id); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("scan missing room = %v", err)
	}
	if _, err := svc.Put("app12345", id, "k", []byte("v")); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("put missing room = %v", err)
	}
	if _, err := svc.Delete("app12345", id, "k"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("delete missing room = %v", err)
	}
}

func TestRoomsSvc_MissingKeyIs404(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	room, _ := svc.Create("app12345", "10.0.0.0/24")
	if _, err := svc.Get(room.AppSlug, room.ID, "absent"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("missing key = %v, want ErrRoomNotFound", err)
	}
}

func TestRoomsSvc_DeleteAbsentKeyIsSuccess(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	room, _ := svc.Create("app12345", "10.0.0.0/24")
	// Deleting a key that never existed in a REAL room is a success.
	if _, err := svc.Delete(room.AppSlug, room.ID, "never"); err != nil {
		t.Fatalf("delete absent key in real room = %v, want nil", err)
	}
}

func TestRoomsSvc_PerIPRateLimit(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	svc.MaxRoomsPerIP = 3
	svc.MaxRoomsPerApp = 1000 // keep the app cap out of the way
	const subnet = "203.0.113.0/24"
	for i := range 3 {
		if _, err := svc.Create("app12345", subnet); err != nil {
			t.Fatalf("create %d under IP limit: %v", i, err)
		}
	}
	// 4th from the same subnet is rate-limited with scope "ip".
	_, err := svc.Create("app12345", subnet)
	var rl *RoomRateLimit
	if !errors.As(err, &rl) {
		t.Fatalf("4th create = %v, want *RoomRateLimit", err)
	}
	if rl.Scope != "ip" {
		t.Fatalf("rate-limit scope = %q, want ip", rl.Scope)
	}
	if !errors.Is(err, ErrRoomCreateRateLimited) {
		t.Fatalf("errors.Is(ErrRoomCreateRateLimited) false")
	}
	// A DIFFERENT subnet is not limited.
	if _, err := svc.Create("app12345", "198.51.100.0/24"); err != nil {
		t.Fatalf("other subnet create = %v", err)
	}
}

func TestRoomsSvc_PerAppRateLimit(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	svc.MaxRoomsPerIP = 1000 // keep IP cap out of the way
	svc.MaxRoomsPerApp = 2
	// Two distinct subnets so the IP cap can't be what trips.
	if _, err := svc.Create("app12345", "203.0.113.0/24"); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if _, err := svc.Create("app12345", "198.51.100.0/24"); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	_, err := svc.Create("app12345", "192.0.2.0/24")
	var rl *RoomRateLimit
	if !errors.As(err, &rl) || rl.Scope != "app" {
		t.Fatalf("3rd app create = %v, want *RoomRateLimit scope app", err)
	}
	// A DIFFERENT app is independent.
	if _, err := svc.Create("app99999", "203.0.113.0/24"); err != nil {
		t.Fatalf("other app create = %v", err)
	}
}

func TestRoomsSvc_RateLimitWindowSlides(t *testing.T) {
	svc, clk := newRoomsSvc(t)
	svc.MaxRoomsPerIP = 1
	svc.MaxRoomsPerApp = 1000
	const subnet = "203.0.113.0/24"
	if _, err := svc.Create("app12345", subnet); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := svc.Create("app12345", subnet); !errors.Is(err, ErrRoomCreateRateLimited) {
		t.Fatalf("immediate second = %v, want rate-limited", err)
	}
	// Advance past the window: the slot frees.
	clk.advance(svc.CreateWindow + time.Minute)
	if _, err := svc.Create("app12345", subnet); err != nil {
		t.Fatalf("create after window = %v, want nil", err)
	}
}

func TestRoomsSvc_PerRoomDataCap(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	room, _ := svc.Create("app12345", "10.0.0.0/24")
	atCap := make([]byte, domain.MaxRoomBytes)
	if _, err := svc.Put(room.AppSlug, room.ID, "big", atCap); err != nil {
		t.Fatalf("at-cap put: %v", err)
	}
	if _, err := svc.Put(room.AppSlug, room.ID, "more", []byte("x")); !errors.Is(err, ErrRoomDataCap) {
		t.Fatalf("over-cap put = %v, want ErrRoomDataCap", err)
	}
}

func TestRoomsSvc_PerAppAggregateCap(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	svc.PerAppByteCap = 10
	r1, _ := svc.Create("app12345", "10.0.0.0/24")
	r2, _ := svc.Create("app12345", "10.0.1.0/24")
	if _, err := svc.Put(r1.AppSlug, r1.ID, "k", []byte("123456")); err != nil {
		t.Fatalf("first write under app cap: %v", err)
	}
	if _, err := svc.Put(r2.AppSlug, r2.ID, "k", []byte("78901")); !errors.Is(err, ErrAppRoomsCap) {
		t.Fatalf("over-app-cap write = %v, want ErrAppRoomsCap", err)
	}
}

func TestRoomsSvc_CrossRoomIsolation(t *testing.T) {
	svc, _ := newRoomsSvc(t)
	a, _ := svc.Create("app12345", "10.0.0.0/24")
	b, _ := svc.Create("app12345", "10.0.0.0/24")
	if _, err := svc.Put(a.AppSlug, a.ID, "secret", []byte("A")); err != nil {
		t.Fatalf("put A: %v", err)
	}
	// B cannot read A's key.
	if _, err := svc.Get(b.AppSlug, b.ID, "secret"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("B read A's key: %v", err)
	}
}
