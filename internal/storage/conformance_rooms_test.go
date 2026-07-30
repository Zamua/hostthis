package storage_test

// Room operations in the backend-agnostic conformance suite.
//
// These subtests pin the observable room-persistence contract every metadata
// backend supporting the rooms tier must hold IDENTICALLY. They run only when
// the backend's factory supplies a non-nil room repo.
//
// The room, paste, and site repos from one factory call MUST share the same
// backing store, so the cross-kind service-wide cap subtest exercises the real
// interaction rather than three independent stores.

import (
	"bytes"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// conformanceRoomRepo is the union of the two room-side service interfaces a
// backend supporting the rooms tier must satisfy.
type conformanceRoomRepo interface {
	service.RoomRepo
	service.SweepRooms
}

// roomConformanceStores bundles the three repos a backend's room factory
// returns, all sharing one backing store.
type roomConformanceStores struct {
	Rooms conformanceRoomRepo
	Paste conformanceRepo
	Site  conformanceSiteRepo
}

// mkConformRoom creates an empty room under app with a fresh UUIDv4, the
// standard retention window, and no caps.
func mkConformRoom(t *testing.T, rr conformanceRoomRepo, app string, now time.Time) domain.Room {
	t.Helper()
	room := domain.Room{
		AppSlug:   domain.Slug(app),
		ID:        domain.NewRoomID(),
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RoomRetentionWindow),
	}
	if err := rr.CreateRoom(room, "10.0.0.0/24", 0, now); err != nil {
		t.Fatalf("create room under %q: %v", app, err)
	}
	return room
}

// runRoomConformance runs the room contract subtests. newRooms must produce a
// FRESH store bundle per subtest, since the empty-store assertions depend on
// it. caps declares the backend's by-design behavior exceptions.
func runRoomConformance(t *testing.T, name string, caps conformCaps, newRooms func(t *testing.T) roomConformanceStores) {
	t.Helper()
	t.Run(name+"/Rooms/RoundTrip", func(t *testing.T) { conformRoomRoundTrip(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/CrossRoomIsolation", func(t *testing.T) { conformRoomCrossRoomIsolation(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/CrossAppIsolation", func(t *testing.T) { conformRoomCrossAppIsolation(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/NonexistentRoom404", func(t *testing.T) { conformRoomNonexistent404(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/PerRoomByteCap", func(t *testing.T) { conformRoomPerRoomByteCap(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/PerRoomKeyCap", func(t *testing.T) { conformRoomPerRoomKeyCap(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/PerRoomCapConcurrentCeiling", func(t *testing.T) { conformRoomPerRoomCapConcurrentCeiling(t, newRooms(t).Rooms, caps) })
	t.Run(name+"/Rooms/PerAppAggregateCap", func(t *testing.T) { conformRoomPerAppAggregateCap(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/DeleteFreesCap", func(t *testing.T) { conformRoomDeleteFreesCap(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/CreationRateLimitCounts", func(t *testing.T) { conformRoomCreationRateLimitCounts(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/CreationLedgerPrune", func(t *testing.T) { conformRoomCreationLedgerPrune(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/AppExistenceNotRepoGated", func(t *testing.T) { conformRoomAppExistenceNotRepoGated(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/ExpiryAndSweep", func(t *testing.T) { conformRoomExpiryAndSweep(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/DeleteExpiredRoom", func(t *testing.T) { conformDeleteExpiredRoom(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/ExpirySubSecondOrdering", func(t *testing.T) { conformRoomExpirySubSecondOrdering(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/SeqDenseAssignment", func(t *testing.T) { conformRoomSeqDenseAssignment(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/SeqConcurrentWritersUniqueDense", func(t *testing.T) { conformRoomSeqConcurrentWritersUniqueDense(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/SeqScanExactUnderConcurrentWrites", func(t *testing.T) { conformRoomSeqScanExactUnderConcurrentWrites(t, newRooms(t).Rooms) })
}

// conformRoomRoundTrip: values PUT into a room come back byte-identically from
// GET and from a whole-namespace SCAN, and a delete removes exactly one key.
func conformRoomRoundTrip(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	pairs := map[string][]byte{
		"participants":    []byte(`["alice","bob"]`),
		"card/1":          []byte("first card"),
		"slot/2026-06-11": []byte{0x00, 0x01, 0x02, 0xff}, // opaque (non-JSON) bytes
		"empty":           {},                             // empty value must round-trip
	}
	for k, v := range pairs {
		if _, err := rr.PutValue(room.AppSlug, room.ID, k, v, 0, fixedNow); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}
	for k, want := range pairs {
		got, err := rr.GetValue(room.AppSlug, room.ID, k)
		if err != nil {
			t.Fatalf("get %q: %v", k, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("value round-trip mismatch for %q: got %v, want %v", k, got, want)
		}
	}
	got, err := rr.GetRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("get room: %v", err)
	}
	if got.AppSlug != room.AppSlug || got.ID != room.ID {
		t.Fatalf("room record mismatch: got app=%q id=%q", got.AppSlug, got.ID)
	}
	kv, err := rr.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if kv.KeyCount() != len(pairs) {
		t.Fatalf("scan key count: got %d, want %d", kv.KeyCount(), len(pairs))
	}
	for k, want := range pairs {
		got, ok := kv.Get(k)
		if !ok {
			t.Fatalf("scan missing key %q", k)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("scan value mismatch for %q: got %v, want %v", k, got, want)
		}
	}
	if _, err := rr.DeleteValue(room.AppSlug, room.ID, "card/1", fixedNow); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := rr.GetValue(room.AppSlug, room.ID, "card/1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted key should be gone: %v", err)
	}
	// Deleting an absent key is idempotent while the room exists.
	if _, err := rr.DeleteValue(room.AppSlug, room.ID, "card/1", fixedNow); err != nil {
		t.Fatalf("re-delete absent key should be a no-op, got %v", err)
	}
}

// conformRoomCrossRoomIsolation: a second room's UUID under the SAME app
// cannot read, write, or scan the first room's data. Fails if the key builder
// drops the room segment.
func conformRoomCrossRoomIsolation(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	roomA := mkConformRoom(t, rr, app, fixedNow)
	roomB := mkConformRoom(t, rr, app, fixedNow)

	if _, err := rr.PutValue(roomA.AppSlug, roomA.ID, "secret", []byte("A-only"), 0, fixedNow); err != nil {
		t.Fatalf("put in A: %v", err)
	}
	if _, err := rr.GetValue(app, roomB.ID, "secret"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("room B read room A's key (isolation broken): %v", err)
	}
	kvB, err := rr.ScanRoom(app, roomB.ID)
	if err != nil {
		t.Fatalf("scan B: %v", err)
	}
	if kvB.KeyCount() != 0 {
		t.Fatalf("room B scan leaked %d keys from room A (isolation broken): %v", kvB.KeyCount(), kvB.Values)
	}
	if _, err := rr.PutValue(app, roomB.ID, "secret", []byte("B-only"), 0, fixedNow); err != nil {
		t.Fatalf("put in B: %v", err)
	}
	gotA, err := rr.GetValue(app, roomA.ID, "secret")
	if err != nil {
		t.Fatalf("get A after B write: %v", err)
	}
	if !bytes.Equal(gotA, []byte("A-only")) {
		t.Fatalf("room A's value was clobbered by room B's write (isolation broken): got %q", gotA)
	}
}

// conformRoomCrossAppIsolation: a byte-identical room UUID under a SECOND app
// addresses a different keyspace. Fails if the key builder drops the app
// segment.
func conformRoomCrossAppIsolation(t *testing.T, rr conformanceRoomRepo) {
	id := domain.NewRoomID()
	now := fixedNow
	roomA := domain.Room{AppSlug: "app1aaaa", ID: id, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RoomRetentionWindow)}
	roomB := domain.Room{AppSlug: "app2bbbb", ID: id, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RoomRetentionWindow)}
	if err := rr.CreateRoom(roomA, "10.0.0.0/24", 0, now); err != nil {
		t.Fatalf("create room under app1: %v", err)
	}
	if err := rr.CreateRoom(roomB, "10.0.0.0/24", 0, now); err != nil {
		t.Fatalf("create room under app2 (same uuid): %v", err)
	}

	if _, err := rr.PutValue(roomA.AppSlug, id, "k", []byte("app1-data"), 0, now); err != nil {
		t.Fatalf("put under app1: %v", err)
	}
	if _, err := rr.GetValue(roomB.AppSlug, id, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("app2 read app1's room key under the same UUID (cross-app isolation broken): %v", err)
	}
	kvB, err := rr.ScanRoom(roomB.AppSlug, id)
	if err != nil {
		t.Fatalf("scan app2 room: %v", err)
	}
	if kvB.KeyCount() != 0 {
		t.Fatalf("app2's same-UUID room leaked app1's data (cross-app isolation broken): %v", kvB.Values)
	}
}

// conformRoomNonexistent404: a per-key GET / PUT / DELETE on a well-formed but
// nonexistent room returns ErrNotFound, the same shape as a missing key in a
// real room, so per-key existence never leaks.
func conformRoomNonexistent404(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	ghost := domain.NewRoomID()
	if _, err := rr.GetRoom(app, ghost); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetRoom on nonexistent room: got %v, want ErrNotFound", err)
	}
	if _, err := rr.GetValue(app, ghost, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetValue on nonexistent room: got %v, want ErrNotFound", err)
	}
	// Room existence is re-checked inside the write boundary.
	if _, err := rr.PutValue(app, ghost, "k", []byte("x"), 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("PutValue on nonexistent room: got %v, want ErrNotFound", err)
	}
	// Only the ROOM-missing case errors; an absent key in a REAL room is a
	// success, covered in RoundTrip.
	if _, err := rr.DeleteValue(app, ghost, "k", fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("DeleteValue on nonexistent room: got %v, want ErrNotFound", err)
	}
	room := mkConformRoom(t, rr, app, fixedNow)
	if _, err := rr.GetValue(room.AppSlug, room.ID, "absent"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing key in a real room: got %v, want ErrNotFound (same as nonexistent room)", err)
	}
}

// conformRoomPerRoomByteCap: a PUT past MaxRoomBytes is rejected with the
// prior state intact. Fails if the per-room cap check is removed.
func conformRoomPerRoomByteCap(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	full := make([]byte, domain.MaxRoomBytes)
	if _, err := rr.PutValue(room.AppSlug, room.ID, "big", full, 0, fixedNow); err != nil {
		t.Fatalf("put at byte cap: %v", err)
	}
	if _, err := rr.PutValue(room.AppSlug, room.ID, "more", []byte("x"), 0, fixedNow); !errors.Is(err, storage.ErrRoomDataFull) {
		t.Fatalf("over-byte-cap put: got %v, want ErrRoomDataFull", err)
	}
	if _, err := rr.GetValue(room.AppSlug, room.ID, "more"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("rejected key was written anyway: %v", err)
	}
	// A value larger than the whole-room budget is rejected up front.
	if _, err := rr.PutValue(room.AppSlug, room.ID, "huge", make([]byte, domain.MaxRoomValueBytes+1), 0, fixedNow); !errors.Is(err, storage.ErrRoomDataFull) {
		t.Fatalf("over-value-cap put: got %v, want ErrRoomDataFull", err)
	}
}

// conformRoomPerRoomKeyCap: a PUT past MaxRoomKeys is rejected even when the
// bytes are tiny.
func conformRoomPerRoomKeyCap(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	for i := range domain.MaxRoomKeys {
		k := keyN(i)
		if _, err := rr.PutValue(room.AppSlug, room.ID, k, []byte("x"), 0, fixedNow); err != nil {
			t.Fatalf("put key %d: %v", i, err)
		}
	}
	if _, err := rr.PutValue(room.AppSlug, room.ID, "overflow", []byte("x"), 0, fixedNow); !errors.Is(err, storage.ErrRoomDataFull) {
		t.Fatalf("over-key-cap put: got %v, want ErrRoomDataFull", err)
	}
	// Overwriting an EXISTING key adds no key slot, so it stays allowed.
	if _, err := rr.PutValue(room.AppSlug, room.ID, keyN(0), []byte("y"), 0, fixedNow); err != nil {
		t.Fatalf("overwrite at key cap should be allowed (no new slot): %v", err)
	}
}

// conformRoomPerRoomCapConcurrentCeiling pins the per-room cap CEILING under
// concurrency: n writers race for the k slots MaxRoomBytes admits, and the
// bytes that land never exceed the cap however the writes interleave. Fails if
// a backend declaring StrictQuotaUnderConcurrency drops the per-room
// serialization, letting two writers read a stale namespace, both pass CanPut,
// and both commit.
func conformRoomPerRoomCapConcurrentCeiling(t *testing.T, rr conformanceRoomRepo, caps conformCaps) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	// body chosen so exactly k values fit under MaxRoomBytes and the (k+1)-th
	// would breach it; n > k writers race for the k slots.
	const (
		k = 3
		n = 8
	)
	body := domain.MaxRoomBytes / k // floor: k*body <= MaxRoomBytes < (k+1)*body
	var landed int64
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct key per writer, so the cap is exercised by the byte
			// total across keys rather than by overwriting one key, and no
			// per-app cap, isolating the per-room one. Any error means the
			// value did not land; only the ceiling is asserted, so the error
			// kind is moot.
			if _, err := rr.PutValue(room.AppSlug, room.ID, keyN(i), make([]byte, body), 0, fixedNow); err == nil {
				atomic.AddInt64(&landed, 1)
			}
		}(i)
	}
	wg.Wait()

	if !caps.StrictQuotaUnderConcurrency {
		t.Logf("backend does not guarantee strict per-room cap under concurrency: %d values x %dB = %dB landed, cap %dB",
			landed, body, landed*int64(body), int64(domain.MaxRoomBytes))
		return
	}
	if landed*int64(body) > int64(domain.MaxRoomBytes) {
		t.Fatalf("per-room cap ceiling breached under concurrency: %d values x %dB = %dB landed, cap %dB",
			landed, body, landed*int64(body), int64(domain.MaxRoomBytes))
	}
	// The ceiling must hold against what is PERSISTED, not just the success
	// count.
	kv, err := rr.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("scan after race: %v", err)
	}
	if kv.TotalBytes() > domain.MaxRoomBytes {
		t.Fatalf("persisted room bytes exceed cap after race: %d > %d", kv.TotalBytes(), domain.MaxRoomBytes)
	}
}

// conformRoomPerAppAggregateCap: the per-app byte cap sums across ALL of an
// app's rooms, and a different app has its own budget.
func conformRoomPerAppAggregateCap(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	const appCap = 100
	roomA := mkConformRoom(t, rr, app, fixedNow)
	// Seed 90 of the 100 app-cap bytes via room A.
	if _, err := rr.PutValue(roomA.AppSlug, roomA.ID, "k", make([]byte, 90), 0 /*per-room cap unused; appCap is the axis under test*/, fixedNow); err != nil {
		t.Fatalf("seed 90 app bytes: %v", err)
	}
	// A second room under the SAME app: 90 + 20 > 100, so the write is
	// rejected only if the sum counts both rooms.
	roomB := mkConformRoom(t, rr, app, fixedNow)
	if _, err := rr.PutValue(roomB.AppSlug, roomB.ID, "k", make([]byte, 20), appCap, fixedNow); !errors.Is(err, storage.ErrAppRoomsFull) {
		t.Fatalf("over-app-cap write (must count both rooms): got %v, want ErrAppRoomsFull", err)
	}
	if _, err := rr.PutValue(roomB.AppSlug, roomB.ID, "k", make([]byte, 10), appCap, fixedNow); err != nil {
		t.Fatalf("write within app cap (90+10=100): %v", err)
	}
	roomC := mkConformRoom(t, rr, "app99999", fixedNow)
	if _, err := rr.PutValue(roomC.AppSlug, roomC.ID, "k", make([]byte, 90), appCap, fixedNow); err != nil {
		t.Fatalf("different app should have its own budget: %v", err)
	}
}

// conformRoomDeleteFreesCap: a DeleteValue credits BOTH the per-room total and
// the per-app counter, so a re-PUT of the freed size succeeds. Fails if a
// delete credits neither (the re-PUT still 413s) or only one of them (still
// 507s).
func conformRoomDeleteFreesCap(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	room := mkConformRoom(t, rr, app, fixedNow)

	// Two values fill the room to EXACTLY MaxRoomBytes, with appCap set to the
	// same figure so both caps sit at their ceiling.
	const doomed = 1000
	anchor := domain.MaxRoomBytes - doomed
	appCap := int64(domain.MaxRoomBytes)
	if _, err := rr.PutValue(room.AppSlug, room.ID, "anchor", make([]byte, anchor), appCap, fixedNow); err != nil {
		t.Fatalf("seed anchor (%d bytes): %v", anchor, err)
	}
	if _, err := rr.PutValue(room.AppSlug, room.ID, "doomed", make([]byte, doomed), appCap, fixedNow); err != nil {
		t.Fatalf("seed doomed (%d bytes): %v", doomed, err)
	}

	// Full on BOTH axes: a new key of `doomed` bytes overflows the per-room
	// byte cap and the per-app counter alike.
	if _, err := rr.PutValue(room.AppSlug, room.ID, "extra", make([]byte, doomed), appCap, fixedNow); err == nil {
		t.Fatalf("write into a full room should be rejected (per-room + per-app both at cap), got nil")
	} else if !errors.Is(err, storage.ErrRoomDataFull) && !errors.Is(err, storage.ErrAppRoomsFull) {
		t.Fatalf("full-room write err = %v, want ErrRoomDataFull or ErrAppRoomsFull", err)
	}

	if _, err := rr.DeleteValue(room.AppSlug, room.ID, "doomed", fixedNow); err != nil {
		t.Fatalf("delete doomed: %v", err)
	}

	// A NEW key of exactly the freed size: it adds a key slot and `doomed`
	// bytes, putting both budgets back at-but-not-over their ceiling.
	if _, err := rr.PutValue(room.AppSlug, room.ID, "reclaimed", make([]byte, doomed), appCap, fixedNow); err != nil {
		t.Fatalf("re-PUT of the freed size should succeed after a delete frees capacity: %v", err)
	}
	kv, err := rr.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("scan after reclaim: %v", err)
	}
	if _, ok := kv.Values["doomed"]; ok {
		t.Fatalf("deleted key 'doomed' is still present after delete + reclaim")
	}
	if kv.TotalBytes() != domain.MaxRoomBytes {
		t.Fatalf("room bytes after reclaim = %d, want %d (anchor + reclaimed)", kv.TotalBytes(), domain.MaxRoomBytes)
	}
}

// conformRoomCreationRateLimitCounts: the per-subnet and per-app in-window
// counts the service gates on are accurate, and creations outside the window
// do not count.
func conformRoomCreationRateLimitCounts(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	const window = time.Hour
	subnetA := "1.2.3.0/24"
	subnetB := "9.9.9.0/24"

	// 3 rooms from subnet A and 2 from subnet B, all under one app.
	for i := range 3 {
		room := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RoomRetentionWindow)}
		if err := rr.CreateRoom(room, subnetA, 0, fixedNow); err != nil {
			t.Fatalf("create A%d: %v", i, err)
		}
	}
	for i := range 2 {
		room := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RoomRetentionWindow)}
		if err := rr.CreateRoom(room, subnetB, 0, fixedNow); err != nil {
			t.Fatalf("create B%d: %v", i, err)
		}
	}

	perSubnet, perApp, err := rr.CountRoomCreates(app, subnetA, fixedNow, window)
	if err != nil {
		t.Fatalf("count creates (A): %v", err)
	}
	if perSubnet != 3 {
		t.Fatalf("per-subnet count for A: got %d, want 3", perSubnet)
	}
	if perApp != 5 {
		t.Fatalf("per-app count: got %d, want 5", perApp)
	}
	perSubnetB, _, err := rr.CountRoomCreates(app, subnetB, fixedNow, window)
	if err != nil {
		t.Fatalf("count creates (B): %v", err)
	}
	if perSubnetB != 2 {
		t.Fatalf("per-subnet count for B: got %d, want 2", perSubnetB)
	}
	old := fixedNow.Add(-2 * window)
	oldRoom := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: old, UpdatedAt: old, ExpiresAt: old.Add(domain.RoomRetentionWindow)}
	if err := rr.CreateRoom(oldRoom, subnetA, 0, old); err != nil {
		t.Fatalf("create old: %v", err)
	}
	perSubnet, _, err = rr.CountRoomCreates(app, subnetA, fixedNow, window)
	if err != nil {
		t.Fatalf("count creates after old: %v", err)
	}
	if perSubnet != 3 {
		t.Fatalf("aged-out creation should not count: per-subnet got %d, want 3", perSubnet)
	}
}

// conformRoomCreationLedgerPrune: a windowed prune drops past-window ledger
// rows so the family stays bounded, and the in-window count survives it.
func conformRoomCreationLedgerPrune(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	const window = time.Hour
	subnet := "1.2.3.0/24"

	old := fixedNow.Add(-2 * window)
	for i := range 2 {
		room := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: old, UpdatedAt: old, ExpiresAt: old.Add(domain.RoomRetentionWindow)}
		if err := rr.CreateRoom(room, subnet, 0, old); err != nil {
			t.Fatalf("create old %d: %v", i, err)
		}
	}
	fresh := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RoomRetentionWindow)}
	if err := rr.CreateRoom(fresh, subnet, 0, fixedNow); err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	n, err := rr.PruneOldRoomCreates(fixedNow.Add(-window))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("prune should remove the 2 old ledger rows, got %d", n)
	}
	perSubnet, _, err := rr.CountRoomCreates(app, subnet, fixedNow, window)
	if err != nil {
		t.Fatalf("count after prune: %v", err)
	}
	if perSubnet != 1 {
		t.Fatalf("post-prune per-subnet count: got %d, want 1", perSubnet)
	}
}

// conformRoomAppExistenceNotRepoGated: the repo creates a room under a slug
// naming no live site or paste. The slug-names-a-live-app rule is an
// HTTP-layer concern, not a repo one.
func conformRoomAppExistenceNotRepoGated(t *testing.T, rr conformanceRoomRepo) {
	room := domain.Room{AppSlug: "noappxyz", ID: domain.NewRoomID(), CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RoomRetentionWindow)}
	if err := rr.CreateRoom(room, "10.0.0.0/24", 0, fixedNow); err != nil {
		t.Fatalf("repo CreateRoom should not be app-existence-gated: %v", err)
	}
	if _, err := rr.GetRoom(room.AppSlug, room.ID); err != nil {
		t.Fatalf("created room should be readable: %v", err)
	}
}

// conformRoomExpiryAndSweep: ExpiredRooms returns one reference per room
// whose ExpiresAt <= now (inclusive boundary), DeleteExpiredRoom removes the
// room and cascades to its values, and an unexpired room is left alone.
func conformRoomExpiryAndSweep(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	// A PUT resets the retention clock to its write time + window, which is
	// how each room below gets a known ExpiresAt.
	soon := mkConformRoom(t, rr, app, fixedNow)
	writeAt := fixedNow.Add(-domain.RoomRetentionWindow).Add(time.Hour)
	if _, err := rr.PutValue(soon.AppSlug, soon.ID, "k", []byte("v"), 0, writeAt); err != nil {
		t.Fatalf("put to set soon expiry: %v", err)
	}
	far := mkConformRoom(t, rr, app, fixedNow)
	if _, err := rr.PutValue(far.AppSlug, far.ID, "k", []byte("v"), 0, fixedNow); err != nil {
		t.Fatalf("put to set far expiry: %v", err)
	}

	at := fixedNow.Add(2 * time.Hour)
	expired, err := rr.ExpiredRooms(at)
	if err != nil {
		t.Fatalf("expired rooms: %v", err)
	}
	if !refsHas(expired, soon.AppSlug, soon.ID) {
		t.Fatalf("soon room should be expired at %v, got %v", at, expired)
	}
	if refsHas(expired, far.AppSlug, far.ID) {
		t.Fatalf("far room should NOT be expired at %v, got %v", at, expired)
	}

	// Inclusive boundary: ExpiresAt == now counts as expired.
	atBoundary := writeAt.Add(domain.RoomRetentionWindow)
	expired, err = rr.ExpiredRooms(atBoundary)
	if err != nil {
		t.Fatalf("expired rooms at boundary: %v", err)
	}
	soonRef, ok := refFor(expired, soon.AppSlug, soon.ID)
	if !ok {
		t.Fatalf("ExpiresAt == now should be inclusive-expired, got %v", expired)
	}

	// Processing the surfaced reference cascades to the room's values.
	deleted, err := rr.DeleteExpiredRoom(soonRef)
	if err != nil {
		t.Fatalf("delete expired room: %v", err)
	}
	if !deleted {
		t.Fatalf("DeleteExpiredRoom must report true for a live room record")
	}
	if _, err := rr.GetRoom(soon.AppSlug, soon.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted room should be gone: %v", err)
	}
	if _, err := rr.GetValue(soon.AppSlug, soon.ID, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted room's value should be cascaded away: %v", err)
	}
	// The sweep may re-process a reference a prior tick already handled.
	deleted, err = rr.DeleteExpiredRoom(soonRef)
	if err != nil {
		t.Fatalf("re-processed room reference must no-op, got %v", err)
	}
	if deleted {
		t.Fatalf("DeleteExpiredRoom must report false when the room record was already gone")
	}
	if _, err := rr.GetRoom(far.AppSlug, far.ID); err != nil {
		t.Fatalf("far room should survive the sweep of soon: %v", err)
	}
}

// conformDeleteExpiredRoom pins the room half of the expiry-pass delete
// contract (docs/SPEC.md "Room storage on the slatedb (and shale) backend"):
// processing a scanned reference deletes the room record and reports true,
// leaves unexpired rooms alone, and DRAINS the scan, so a re-scan sees zero
// references and a re-processed reference no-ops reporting false.
func conformDeleteExpiredRoom(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	dead := mkConformRoom(t, rr, app, fixedNow)
	// Write at (fixedNow - window + hour) so ExpiresAt = fixedNow + 1h.
	writeAt := fixedNow.Add(-domain.RoomRetentionWindow).Add(time.Hour)
	if _, err := rr.PutValue(dead.AppSlug, dead.ID, "k", []byte("v"), 0, writeAt); err != nil {
		t.Fatalf("put to set dead expiry: %v", err)
	}
	alive := mkConformRoom(t, rr, app, fixedNow)
	if _, err := rr.PutValue(alive.AppSlug, alive.ID, "k", []byte("v"), 0, fixedNow); err != nil {
		t.Fatalf("put to set alive expiry: %v", err)
	}

	at := fixedNow.Add(2 * time.Hour)
	refs, err := rr.ExpiredRooms(at)
	if err != nil {
		t.Fatalf("expired rooms: %v", err)
	}
	if len(refs) != 1 || refs[0].AppSlug != dead.AppSlug || refs[0].ID != dead.ID {
		t.Fatalf("only the dead room should be expired at %v, got %v", at, refs)
	}

	deleted, err := rr.DeleteExpiredRoom(refs[0])
	if err != nil {
		t.Fatalf("delete expired room: %v", err)
	}
	if !deleted {
		t.Fatalf("DeleteExpiredRoom must report true for a live room record")
	}
	if _, err := rr.GetRoom(dead.AppSlug, dead.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired room should be gone after DeleteExpiredRoom: %v", err)
	}

	again, err := rr.ExpiredRooms(at)
	if err != nil {
		t.Fatalf("expired rooms (re-scan): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("re-scan after the pass must see zero expired room references, got %v", again)
	}

	// The sweep's deleted-count must reflect only real record deletions.
	deleted, err = rr.DeleteExpiredRoom(refs[0])
	if err != nil {
		t.Fatalf("re-processed room reference must no-op, got: %v", err)
	}
	if deleted {
		t.Fatalf("DeleteExpiredRoom must report false when the room record was already gone")
	}

	if _, err := rr.GetRoom(alive.AppSlug, alive.ID); err != nil {
		t.Fatalf("active room must survive the expiry pass: %v", err)
	}
}

// conformRoomExpirySubSecondOrdering pins that the room expiry index orders by
// TIME within a shared whole second. Under a variable-width timestamp a room
// expiring at "...00.5Z" sorts BEFORE a whole-second cutoff "...00Z" and is
// swept up to ~1s early; the fixed-width format makes byte order == time order.
func conformRoomExpirySubSecondOrdering(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	base := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Expires half a second into the whole second.
	late := mkConformRoom(t, rr, app, base)
	lateWriteAt := base.Add(-domain.RoomRetentionWindow).Add(500 * time.Millisecond)
	if _, err := rr.PutValue(late.AppSlug, late.ID, "k", []byte("v"), 0, lateWriteAt); err != nil {
		t.Fatalf("put to set late (.5s) expiry: %v", err)
	}
	// Expires at the START of the same whole second.
	early := mkConformRoom(t, rr, app, base)
	earlyWriteAt := base.Add(-domain.RoomRetentionWindow)
	if _, err := rr.PutValue(early.AppSlug, early.ID, "k", []byte("v"), 0, earlyWriteAt); err != nil {
		t.Fatalf("put to set early (.0s) expiry: %v", err)
	}

	atStart := base
	expired, err := rr.ExpiredRooms(atStart)
	if err != nil {
		t.Fatalf("expired at .0s: %v", err)
	}
	if refsHas(expired, late.AppSlug, late.ID) {
		t.Fatalf("room expiring at .5s must NOT be expired at a .0s cutoff (sub-second ordering bug), got %v", expired)
	}
	if !refsHas(expired, early.AppSlug, early.ID) {
		t.Fatalf("room expiring at .0s should be inclusive-expired at a .0s cutoff, got %v", expired)
	}

	// A .4s cutoff proves the boundary is real sub-second time rather than
	// whole-second rounding.
	atBelow := base.Add(400 * time.Millisecond)
	expired, err = rr.ExpiredRooms(atBelow)
	if err != nil {
		t.Fatalf("expired at .4s: %v", err)
	}
	if refsHas(expired, late.AppSlug, late.ID) {
		t.Fatalf("room expiring at .5s must NOT be expired at a .4s cutoff, got %v", expired)
	}
}

// --- small helpers ---------------------------------------------------------

func refsHas(refs []domain.ExpiredRoom, app domain.Slug, id domain.RoomID) bool {
	_, ok := refFor(refs, app, id)
	return ok
}

func refFor(refs []domain.ExpiredRoom, app domain.Slug, id domain.RoomID) (domain.ExpiredRoom, bool) {
	for _, ref := range refs {
		if ref.AppSlug == app && ref.ID == id {
			return ref, true
		}
	}
	return domain.ExpiredRoom{}, false
}

// keyN builds a distinct key for the i-th value, base-36 and within
// MaxRoomKeyLen.
func keyN(i int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if i == 0 {
		return "k0"
	}
	out := []byte{'k'}
	for i > 0 {
		out = append(out, digits[i%36])
		i /= 36
	}
	return string(out)
}

// --- Per-room sequence conformance (SPEC "The per-room sequence:
// assignment at commit") -----------------------------------------------------
//
// The relay's multi-pod correctness rides these invariants, so every backend
// must hold them:
//
//   - every committed mutation (PUT or DELETE, including the idempotent
//     DELETE of an absent key) assigns exactly one seq, dense +1 from 0
//   - PutValue / DeleteValue return the assigned seq
//   - concurrent same-room writers never share or skip a seq
//   - ScanRoom's stamped Seq is EXACT: every mutation with seq <= S is in the
//     state and none with seq > S is, even under concurrent writes

// conformRoomSeqDenseAssignment: sequential mutations of every flavor assign
// 1, 2, 3, ... with no holes, and ScanRoom reports the last assigned seq.
func conformRoomSeqDenseAssignment(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)

	kv, err := rr.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("scan fresh: %v", err)
	}
	if kv.Seq != 0 {
		t.Fatalf("fresh room scan seq = %d, want 0", kv.Seq)
	}

	steps := []struct {
		name string
		run  func() (uint64, error)
	}{
		{"put k1", func() (uint64, error) { return rr.PutValue(room.AppSlug, room.ID, "k1", []byte("v1"), 0, fixedNow) }},
		{"put k2", func() (uint64, error) { return rr.PutValue(room.AppSlug, room.ID, "k2", []byte("v2"), 0, fixedNow) }},
		{"overwrite k1", func() (uint64, error) { return rr.PutValue(room.AppSlug, room.ID, "k1", []byte("v1b"), 0, fixedNow) }},
		{"delete k2", func() (uint64, error) { return rr.DeleteValue(room.AppSlug, room.ID, "k2", fixedNow) }},
		// The idempotent DELETE of an ABSENT key still commits, touching the
		// retention clock, so it must still assign a seq: a bump with no
		// frame reads as a permanent hole to a relay subscriber, and a commit
		// with no bump breaks density.
		{"delete absent", func() (uint64, error) { return rr.DeleteValue(room.AppSlug, room.ID, "never-existed", fixedNow) }},
	}
	for i, step := range steps {
		seq, err := step.run()
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if want := uint64(i + 1); seq != want {
			t.Fatalf("%s: assigned seq = %d, want %d (dense +1 per committed mutation)", step.name, seq, want)
		}
	}

	kv, err = rr.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("scan after mutations: %v", err)
	}
	if want := uint64(len(steps)); kv.Seq != want {
		t.Fatalf("scan seq = %d, want %d (the exact seq the snapshot reflects)", kv.Seq, want)
	}
}

// conformRoomSeqConcurrentWritersUniqueDense: concurrent same-room writers on
// distinct keys receive seqs that are all unique and together form the dense
// range 1..N. This is what makes a hole in the relay's live stream MEAN a lost
// frame rather than a storage-side numbering artifact.
func conformRoomSeqConcurrentWritersUniqueDense(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	const writers = 4
	const putsEach = 20

	var mu sync.Mutex
	seen := make(map[uint64]string, writers*putsEach)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range putsEach {
				key := keyN(w*putsEach + i)
				seq, err := rr.PutValue(room.AppSlug, room.ID, key, []byte("v"), 0, fixedNow)
				if err != nil {
					t.Errorf("writer %d put %d: %v", w, i, err)
					return
				}
				mu.Lock()
				if prev, dup := seen[seq]; dup {
					t.Errorf("seq %d assigned twice (to %s and %s)", seq, prev, key)
				}
				seen[seq] = key
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	const total = writers * putsEach
	if len(seen) != total {
		t.Fatalf("assigned %d distinct seqs, want %d", len(seen), total)
	}
	for s := uint64(1); s <= total; s++ {
		if _, ok := seen[s]; !ok {
			t.Fatalf("seq %d never assigned: the range 1..%d must be dense (no skip)", s, total)
		}
	}
	kv, err := rr.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("final scan: %v", err)
	}
	if kv.Seq != uint64(total) {
		t.Fatalf("final scan seq = %d, want %d", kv.Seq, total)
	}
}

// conformRoomSeqScanExactUnderConcurrentWrites: while writers add one NEW key
// per mutation, every concurrent ScanRoom must satisfy key-count == Seq
// exactly. A snapshot claiming seq S with more or fewer than S keys has a
// broken fence and would hand a relay late-joiner a state that does not match
// its splice point. Sequential scans must also observe a nondecreasing Seq.
func conformRoomSeqScanExactUnderConcurrentWrites(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	const writers = 3
	const putsEach = 25

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range putsEach {
				if _, err := rr.PutValue(room.AppSlug, room.ID, keyN(w*putsEach+i), []byte("v"), 0, fixedNow); err != nil {
					t.Errorf("writer %d put %d: %v", w, i, err)
					return
				}
			}
		}(w)
	}

	scanErr := make(chan error, 1)
	go func() {
		defer close(scanErr)
		var lastSeq uint64
		for {
			select {
			case <-stop:
				return
			default:
			}
			kv, err := rr.ScanRoom(room.AppSlug, room.ID)
			if err != nil {
				scanErr <- err
				return
			}
			if kv.Seq < lastSeq {
				scanErr <- errSeqRegressed(lastSeq, kv.Seq)
				return
			}
			lastSeq = kv.Seq
			if got, want := uint64(kv.KeyCount()), kv.Seq; got != want {
				scanErr <- errScanInexact(want, got)
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	if err := <-scanErr; err != nil {
		t.Fatal(err)
	}
	if t.Failed() {
		return
	}
	kv, err := rr.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("final scan: %v", err)
	}
	const total = writers * putsEach
	if kv.Seq != uint64(total) || kv.KeyCount() != total {
		t.Fatalf("final scan seq=%d keys=%d, want both %d", kv.Seq, kv.KeyCount(), total)
	}
}

func errSeqRegressed(prev, cur uint64) error {
	return errors.New("scan seq regressed from " +
		strconv.FormatUint(prev, 10) + " to " + strconv.FormatUint(cur, 10))
}

func errScanInexact(seq, keys uint64) error {
	return errors.New("scan fence inexact: snapshot claims seq " +
		strconv.FormatUint(seq, 10) + " but holds " + strconv.FormatUint(keys, 10) +
		" keys (each committed mutation added exactly one key, so they must match)")
}
