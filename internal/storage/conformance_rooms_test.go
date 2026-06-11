package storage_test

// Room operations in the backend-agnostic conformance suite.
//
// These subtests pin the OBSERVABLE room-persistence contract every metadata
// backend that supports the rooms tier must hold IDENTICALLY (sqlite,
// slatedb, and shale). They run only when the backend's factory supplies a
// non-nil room repo (newRooms). A room repo is the union of service.RoomRepo
// (create + the KV verbs + the creation-count) and service.SweepRooms (expiry
// keys + delete + the creation-ledger prune).
//
// The room repo, the paste repo, and the site repo from one factory call MUST
// share the same backing store, so the cross-kind service-wide cap subtest (a
// room's bytes affect a paste / site quota check and vice versa) exercises the
// real interaction, not three independent stores.
//
// Each room subtest is constructed so it FAILS on intentionally-weakened code
// (the TDD gate): the isolation subtest fails if a key builder drops the app
// or room segment; the cap subtests fail if a cap check is removed; the
// service-wide subtest fails if the room bytes are dropped from the sum in
// either direction.

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// conformanceRoomRepo is the union of the two room-side service interfaces a
// backend that supports the rooms tier must satisfy. The sqlite
// *storage.RoomKVRepo, the slatedb *storage.SlateRoomRepo, and the shale
// *storage.ShaleRoomRepo all implement it.
type conformanceRoomRepo interface {
	service.RoomRepo
	service.SweepRooms
}

// roomConformanceStores bundles the three repos a backend's room factory
// returns, all sharing one backing store. The cross-kind service-wide cap
// subtest drives all three.
type roomConformanceStores struct {
	Rooms conformanceRoomRepo
	Paste conformanceRepo
	Site  conformanceSiteRepo
}

// mkConformRoom creates an empty room under app with a freshly-minted UUIDv4
// and the standard retention window, with no caps. Returns the created Room.
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

// runRoomConformance runs the room contract subtests. `newRooms` produces a
// fresh room+paste+site store bundle (sharing one backing store) per subtest,
// matching the suite's "fresh empty store per subtest" discipline so the
// empty-store assertions hold. `caps` declares the backend's by-design
// behavior exceptions (rooms honor ExpiryFreesQuotaAtReadTime for the per-app
// aggregate the same way pastes + sites do).
func runRoomConformance(t *testing.T, name string, caps conformCaps, newRooms func(t *testing.T) roomConformanceStores) {
	t.Helper()
	t.Run(name+"/Rooms/RoundTrip", func(t *testing.T) { conformRoomRoundTrip(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/CrossRoomIsolation", func(t *testing.T) { conformRoomCrossRoomIsolation(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/CrossAppIsolation", func(t *testing.T) { conformRoomCrossAppIsolation(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/NonexistentRoom404", func(t *testing.T) { conformRoomNonexistent404(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/PerRoomByteCap", func(t *testing.T) { conformRoomPerRoomByteCap(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/PerRoomKeyCap", func(t *testing.T) { conformRoomPerRoomKeyCap(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/PerAppAggregateCap", func(t *testing.T) { conformRoomPerAppAggregateCap(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/ServiceCapCountsAll", func(t *testing.T) { conformRoomServiceCapCountsAll(t, newRooms(t)) })
	t.Run(name+"/Rooms/CreationRateLimitCounts", func(t *testing.T) { conformRoomCreationRateLimitCounts(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/CreationLedgerPrune", func(t *testing.T) { conformRoomCreationLedgerPrune(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/AppExistenceNotRepoGated", func(t *testing.T) { conformRoomAppExistenceNotRepoGated(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/ExpiryAndSweep", func(t *testing.T) { conformRoomExpiryAndSweep(t, newRooms(t).Rooms) })
	t.Run(name+"/Rooms/ExpirySubSecondOrdering", func(t *testing.T) { conformRoomExpirySubSecondOrdering(t, newRooms(t).Rooms) })
}

// conformRoomRoundTrip: create a room, PUT values under several keys, GET each
// back byte-identically, SCAN the whole namespace and observe every pair.
func conformRoomRoundTrip(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	pairs := map[string][]byte{
		"participants":    []byte(`["alice","bob"]`),
		"card/1":          []byte("first card"),
		"slot/2026-06-11": []byte{0x00, 0x01, 0x02, 0xff}, // opaque (non-JSON) bytes
		"empty":           {},                             // empty value must round-trip
	}
	for k, v := range pairs {
		if err := rr.PutValue(room.AppSlug, room.ID, k, v, 0, 0, fixedNow); err != nil {
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
	// GetRoom returns the room record.
	got, err := rr.GetRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("get room: %v", err)
	}
	if got.AppSlug != room.AppSlug || got.ID != room.ID {
		t.Fatalf("room record mismatch: got app=%q id=%q", got.AppSlug, got.ID)
	}
	// Scan returns every pair.
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
	// Delete one key; it leaves the namespace, the others remain.
	if err := rr.DeleteValue(room.AppSlug, room.ID, "card/1", fixedNow); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := rr.GetValue(room.AppSlug, room.ID, "card/1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted key should be gone: %v", err)
	}
	// Deleting an absent key is idempotent (room exists -> success).
	if err := rr.DeleteValue(room.AppSlug, room.ID, "card/1", fixedNow); err != nil {
		t.Fatalf("re-delete absent key should be a no-op, got %v", err)
	}
}

// conformRoomCrossRoomIsolation: a second room's UUID under the SAME app
// cannot read, write, or scan the first room's data. This FAILS if the key
// builder drops the room segment (both rooms would share one namespace).
func conformRoomCrossRoomIsolation(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	roomA := mkConformRoom(t, rr, app, fixedNow)
	roomB := mkConformRoom(t, rr, app, fixedNow)

	if err := rr.PutValue(roomA.AppSlug, roomA.ID, "secret", []byte("A-only"), 0, 0, fixedNow); err != nil {
		t.Fatalf("put in A: %v", err)
	}
	// B cannot read A's key.
	if _, err := rr.GetValue(app, roomB.ID, "secret"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("room B read room A's key (isolation broken): %v", err)
	}
	// B's scan does not see A's data.
	kvB, err := rr.ScanRoom(app, roomB.ID)
	if err != nil {
		t.Fatalf("scan B: %v", err)
	}
	if kvB.KeyCount() != 0 {
		t.Fatalf("room B scan leaked %d keys from room A (isolation broken): %v", kvB.KeyCount(), kvB.Values)
	}
	// B writes its own key; A does not see it.
	if err := rr.PutValue(app, roomB.ID, "secret", []byte("B-only"), 0, 0, fixedNow); err != nil {
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

// conformRoomCrossAppIsolation: an IDENTICAL room-UUID-shaped string under a
// SECOND app addresses a different keyspace. FAILS if the key builder drops
// the app segment.
func conformRoomCrossAppIsolation(t *testing.T, rr conformanceRoomRepo) {
	// Two apps, but the SAME room UUID under each (constructed by hand so the
	// id is byte-identical across the two apps).
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

	if err := rr.PutValue(roomA.AppSlug, id, "k", []byte("app1-data"), 0, 0, now); err != nil {
		t.Fatalf("put under app1: %v", err)
	}
	// app2's same-UUID room does not see app1's value.
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
// nonexistent room returns ErrNotFound - the same shape as a missing key in a
// real room (no per-key existence leak), and the same as a write to a gone
// room.
func conformRoomNonexistent404(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	// No room created under this app; a fresh well-formed UUID names no room.
	ghost := domain.NewRoomID()
	if _, err := rr.GetRoom(app, ghost); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetRoom on nonexistent room: got %v, want ErrNotFound", err)
	}
	if _, err := rr.GetValue(app, ghost, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetValue on nonexistent room: got %v, want ErrNotFound", err)
	}
	// PUT to a gone room is ErrNotFound (the room existence is re-checked
	// inside the write boundary).
	if err := rr.PutValue(app, ghost, "k", []byte("x"), 0, 0, fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("PutValue on nonexistent room: got %v, want ErrNotFound", err)
	}
	// DELETE to a gone room is ErrNotFound (only the ROOM-missing case errors;
	// an absent key in a REAL room is a success - covered in RoundTrip).
	if err := rr.DeleteValue(app, ghost, "k", fixedNow); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("DeleteValue on nonexistent room: got %v, want ErrNotFound", err)
	}
	// A real room exists, but a missing KEY in it is the SAME ErrNotFound a
	// nonexistent room gives (no existence distinction on the per-key path).
	room := mkConformRoom(t, rr, app, fixedNow)
	if _, err := rr.GetValue(room.AppSlug, room.ID, "absent"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing key in a real room: got %v, want ErrNotFound (same as nonexistent room)", err)
	}
}

// conformRoomPerRoomByteCap: a PUT that would push the room past MaxRoomBytes
// is rejected (ErrRoomDataFull), prior value intact. FAILS if the per-room cap
// check is removed.
func conformRoomPerRoomByteCap(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	// Fill the room to exactly the byte cap with one value.
	full := make([]byte, domain.MaxRoomBytes)
	if err := rr.PutValue(room.AppSlug, room.ID, "big", full, 0, 0, fixedNow); err != nil {
		t.Fatalf("put at byte cap: %v", err)
	}
	// One more byte (a new key) exceeds MaxRoomBytes -> rejected.
	if err := rr.PutValue(room.AppSlug, room.ID, "more", []byte("x"), 0, 0, fixedNow); !errors.Is(err, storage.ErrRoomDataFull) {
		t.Fatalf("over-byte-cap put: got %v, want ErrRoomDataFull", err)
	}
	// The rejected key was NOT written (prior state intact).
	if _, err := rr.GetValue(room.AppSlug, room.ID, "more"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("rejected key was written anyway: %v", err)
	}
	// A value larger than the whole-room budget is rejected up front too.
	if err := rr.PutValue(room.AppSlug, room.ID, "huge", make([]byte, domain.MaxRoomValueBytes+1), 0, 0, fixedNow); !errors.Is(err, storage.ErrRoomDataFull) {
		t.Fatalf("over-value-cap put: got %v, want ErrRoomDataFull", err)
	}
}

// conformRoomPerRoomKeyCap: a PUT that would push the room past MaxRoomKeys is
// rejected, even though the bytes are tiny.
func conformRoomPerRoomKeyCap(t *testing.T, rr conformanceRoomRepo) {
	room := mkConformRoom(t, rr, "app12345", fixedNow)
	// Fill the room to exactly MaxRoomKeys with one-byte values.
	for i := 0; i < domain.MaxRoomKeys; i++ {
		k := keyN(i)
		if err := rr.PutValue(room.AppSlug, room.ID, k, []byte("x"), 0, 0, fixedNow); err != nil {
			t.Fatalf("put key %d: %v", i, err)
		}
	}
	// One more distinct key exceeds MaxRoomKeys -> rejected.
	if err := rr.PutValue(room.AppSlug, room.ID, "overflow", []byte("x"), 0, 0, fixedNow); !errors.Is(err, storage.ErrRoomDataFull) {
		t.Fatalf("over-key-cap put: got %v, want ErrRoomDataFull", err)
	}
	// Overwriting an EXISTING key does NOT add a key slot -> still allowed.
	if err := rr.PutValue(room.AppSlug, room.ID, keyN(0), []byte("y"), 0, 0, fixedNow); err != nil {
		t.Fatalf("overwrite at key cap should be allowed (no new slot): %v", err)
	}
}

// conformRoomPerAppAggregateCap: an app's rooms in aggregate are bounded; past
// the per-app byte cap a new room write (and a new room creation) is rejected.
func conformRoomPerAppAggregateCap(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	const appCap = 100
	roomA := mkConformRoom(t, rr, app, fixedNow)
	// Fill 90 of the 100 app-cap bytes via room A.
	if err := rr.PutValue(roomA.AppSlug, roomA.ID, "k", make([]byte, 90), 0 /*per-room: unused here, appCap below*/, 0, fixedNow); err != nil {
		// per-room cap unused (the appCap is what we test); pass appCap on the
		// next writes.
		t.Fatalf("seed 90 app bytes: %v", err)
	}
	// A second room under the SAME app: a 20-byte write pushes the app
	// aggregate to 110 > 100 -> rejected (the per-app sum counts BOTH rooms).
	roomB := mkConformRoom(t, rr, app, fixedNow)
	if err := rr.PutValue(roomB.AppSlug, roomB.ID, "k", make([]byte, 20), appCap, 0, fixedNow); !errors.Is(err, storage.ErrAppRoomsFull) {
		t.Fatalf("over-app-cap write (must count both rooms): got %v, want ErrAppRoomsFull", err)
	}
	// A 10-byte write fits (90 + 10 = 100).
	if err := rr.PutValue(roomB.AppSlug, roomB.ID, "k", make([]byte, 10), appCap, 0, fixedNow); err != nil {
		t.Fatalf("write within app cap (90+10=100): %v", err)
	}
	// A DIFFERENT app has its own untouched budget.
	roomC := mkConformRoom(t, rr, "app99999", fixedNow)
	if err := rr.PutValue(roomC.AppSlug, roomC.ID, "k", make([]byte, 90), appCap, 0, fixedNow); err != nil {
		t.Fatalf("different app should have its own budget: %v", err)
	}
}

// conformRoomServiceCapCountsAll: room bytes fill the service-wide cap a paste
// / site then sees, AND paste / site bytes fill the cap a room PUT then sees -
// the symmetric directions. FAILS if room bytes are dropped from the sum in
// either direction.
func conformRoomServiceCapCountsAll(t *testing.T, st roomConformanceStores) {
	const svcCap = 1000
	rr, pr, sr := st.Rooms, st.Paste, st.Site
	room := mkConformRoom(t, rr, "app12345", fixedNow)

	// 1. A room PUT sees paste + site bytes already held. A paste (400) + a
	// site (300) hold 700; a room write of 400 would push to 1100 -> reject.
	if err := pr.InsertWithQuotaCheck(pasteOf("rsp12345", "key:a", 400), svcCap, 0, fixedNow); err != nil {
		t.Fatalf("paste 400 under svc cap: %v", err)
	}
	if err := sr.InsertWithQuotaCheck(siteOf("rss12345", "key:b", 300), 300, svcCap, 0, fixedNow); err != nil {
		t.Fatalf("site 300 under svc cap: %v", err)
	}
	if err := rr.PutValue(room.AppSlug, room.ID, "k", make([]byte, 400), 0, svcCap, fixedNow); !errors.Is(err, storage.ErrServiceFull) {
		t.Fatalf("room PUT over service cap (paste+site bytes must count): got %v, want ErrServiceFull", err)
	}
	// A room write of 200 fits (700 + 200 = 900 <= 1000).
	if err := rr.PutValue(room.AppSlug, room.ID, "k", make([]byte, 200), 0, svcCap, fixedNow); err != nil {
		t.Fatalf("room write within service cap (700+200=900): %v", err)
	}

	// 2. Now a paste sees room bytes: total is 400(paste)+300(site)+200(room)
	// = 900. A new paste of 200 would push to 1100 -> reject. This proves the
	// PASTE upload's service-wide sum INCLUDES room bytes.
	if err := pr.InsertWithQuotaCheck(pasteOf("rsp22345", "key:c", 200), svcCap, 0, fixedNow); !errors.Is(err, storage.ErrServiceFull) {
		t.Fatalf("paste over service cap (room bytes must count): got %v, want ErrServiceFull", err)
	}
	// 3. And a site sees room bytes too (the same 900 total; a 200-byte site
	// overflows). This proves the SITE deploy's service-wide sum includes room
	// bytes.
	if err := sr.InsertWithQuotaCheck(siteOf("rss22345", "key:d", 200), 200, svcCap, 0, fixedNow); !errors.Is(err, storage.ErrServiceFull) {
		t.Fatalf("site over service cap (room bytes must count): got %v, want ErrServiceFull", err)
	}
}

// conformRoomCreationRateLimitCounts: the per-subnet and per-app in-window
// counts the service gates on are accurate after N creations.
func conformRoomCreationRateLimitCounts(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	const window = time.Hour
	subnetA := "1.2.3.0/24"
	subnetB := "9.9.9.0/24"

	// Create 3 rooms from subnet A and 2 from subnet B, all under one app.
	for i := 0; i < 3; i++ {
		room := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RoomRetentionWindow)}
		if err := rr.CreateRoom(room, subnetA, 0, fixedNow); err != nil {
			t.Fatalf("create A%d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		room := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RoomRetentionWindow)}
		if err := rr.CreateRoom(room, subnetB, 0, fixedNow); err != nil {
			t.Fatalf("create B%d: %v", i, err)
		}
	}

	// Per-subnet count for A is 3; per-app count is 5 (all under one app).
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
	// Per-subnet count for B is 2.
	perSubnetB, _, err := rr.CountRoomCreates(app, subnetB, fixedNow, window)
	if err != nil {
		t.Fatalf("count creates (B): %v", err)
	}
	if perSubnetB != 2 {
		t.Fatalf("per-subnet count for B: got %d, want 2", perSubnetB)
	}
	// A row OUTSIDE the window does not count.
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
// rows, so the family stays bounded and the count after a prune is correct.
func conformRoomCreationLedgerPrune(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	const window = time.Hour
	subnet := "1.2.3.0/24"

	// Two old creations (outside the window) + one fresh one.
	old := fixedNow.Add(-2 * window)
	for i := 0; i < 2; i++ {
		room := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: old, UpdatedAt: old, ExpiresAt: old.Add(domain.RoomRetentionWindow)}
		if err := rr.CreateRoom(room, subnet, 0, old); err != nil {
			t.Fatalf("create old %d: %v", i, err)
		}
	}
	fresh := domain.Room{AppSlug: app, ID: domain.NewRoomID(), CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RoomRetentionWindow)}
	if err := rr.CreateRoom(fresh, subnet, 0, fixedNow); err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	// Prune everything older than the window cutoff (now - window).
	n, err := rr.PruneOldRoomCreates(fixedNow.Add(-window))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("prune should remove the 2 old ledger rows, got %d", n)
	}
	// After the prune, the in-window count is just the fresh one.
	perSubnet, _, err := rr.CountRoomCreates(app, subnet, fixedNow, window)
	if err != nil {
		t.Fatalf("count after prune: %v", err)
	}
	if perSubnet != 1 {
		t.Fatalf("post-prune per-subnet count: got %d, want 1", perSubnet)
	}
}

// conformRoomAppExistenceNotRepoGated: room creation at the REPO level is NOT
// app-existence-gated (the HTTP layer enforces the slug-names-a-live-app rule;
// the repo creates a room under any slug, the same way the paste repo is not
// owner-gated). Pins that the repo accepts a create under a slug with no
// matching site/paste.
func conformRoomAppExistenceNotRepoGated(t *testing.T, rr conformanceRoomRepo) {
	// A slug that names no live site or paste still creates a room at the repo
	// level - the existence gate is an HTTP-layer concern, not a repo one.
	room := domain.Room{AppSlug: "noappxyz", ID: domain.NewRoomID(), CreatedAt: fixedNow, UpdatedAt: fixedNow, ExpiresAt: fixedNow.Add(domain.RoomRetentionWindow)}
	if err := rr.CreateRoom(room, "10.0.0.0/24", 0, fixedNow); err != nil {
		t.Fatalf("repo CreateRoom should not be app-existence-gated: %v", err)
	}
	if _, err := rr.GetRoom(room.AppSlug, room.ID); err != nil {
		t.Fatalf("created room should be readable: %v", err)
	}
}

// conformRoomExpiryAndSweep: ExpiredRoomKeys returns rooms whose ExpiresAt <=
// now (inclusive boundary), DeleteRoom removes the room and cascades to its
// values, and an unexpired room is left alone.
func conformRoomExpiryAndSweep(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	// One room expiring soon, one far in the future. Create then PUT to set a
	// known ExpiresAt (a PUT resets the clock to now + window).
	soon := mkConformRoom(t, rr, app, fixedNow)
	// Backdate `soon`'s clock by writing at a time whose window lands an hour
	// out: write at (fixedNow - window + hour) so ExpiresAt = fixedNow + hour.
	writeAt := fixedNow.Add(-domain.RoomRetentionWindow).Add(time.Hour)
	if err := rr.PutValue(soon.AppSlug, soon.ID, "k", []byte("v"), 0, 0, writeAt); err != nil {
		t.Fatalf("put to set soon expiry: %v", err)
	}
	far := mkConformRoom(t, rr, app, fixedNow)
	if err := rr.PutValue(far.AppSlug, far.ID, "k", []byte("v"), 0, 0, fixedNow); err != nil {
		t.Fatalf("put to set far expiry: %v", err)
	}

	// At a time past `soon`'s expiry but before `far`'s, only `soon` is
	// expired.
	at := fixedNow.Add(2 * time.Hour)
	expired, err := rr.ExpiredRoomKeys(at)
	if err != nil {
		t.Fatalf("expired room keys: %v", err)
	}
	if !refsHas(expired, soon.AppSlug, soon.ID) {
		t.Fatalf("soon room should be expired at %v, got %v", at, expired)
	}
	if refsHas(expired, far.AppSlug, far.ID) {
		t.Fatalf("far room should NOT be expired at %v, got %v", at, expired)
	}

	// Inclusive boundary: ExpiresAt == now counts as expired.
	atBoundary := writeAt.Add(domain.RoomRetentionWindow)
	expired, err = rr.ExpiredRoomKeys(atBoundary)
	if err != nil {
		t.Fatalf("expired room keys at boundary: %v", err)
	}
	if !refsHas(expired, soon.AppSlug, soon.ID) {
		t.Fatalf("ExpiresAt == now should be inclusive-expired, got %v", expired)
	}

	// DeleteRoom removes the room AND its values (the cascade).
	if err := rr.DeleteRoom(soon.AppSlug, soon.ID); err != nil {
		t.Fatalf("delete room: %v", err)
	}
	if _, err := rr.GetRoom(soon.AppSlug, soon.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted room should be gone: %v", err)
	}
	if _, err := rr.GetValue(soon.AppSlug, soon.ID, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted room's value should be cascaded away: %v", err)
	}
	// DeleteRoom is idempotent (the sweep may re-delete a key a prior tick
	// removed).
	if err := rr.DeleteRoom(soon.AppSlug, soon.ID); err != nil {
		t.Fatalf("re-delete missing room should be a no-op, got %v", err)
	}
	// `far` survives.
	if _, err := rr.GetRoom(far.AppSlug, far.ID); err != nil {
		t.Fatalf("far room should survive the sweep of soon: %v", err)
	}
}

// conformRoomExpirySubSecondOrdering pins that the room expiry index orders by
// TIME within a shared whole second (the fixed-width-timestamp guard). With a
// variable-width format a room expiring at "...00.5Z" would sort BEFORE a
// whole-second cutoff "...00Z" and be swept up to ~1s early; the fixed-width
// format makes byte order == time order, so this FAILS on the unfixed format.
func conformRoomExpirySubSecondOrdering(t *testing.T, rr conformanceRoomRepo) {
	const app = "app12345"
	base := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// A room that expires HALF A SECOND into the whole second: write at
	// (base - window + 0.5s) so ExpiresAt = base + 0.5s.
	late := mkConformRoom(t, rr, app, base)
	lateWriteAt := base.Add(-domain.RoomRetentionWindow).Add(500 * time.Millisecond)
	if err := rr.PutValue(late.AppSlug, late.ID, "k", []byte("v"), 0, 0, lateWriteAt); err != nil {
		t.Fatalf("put to set late (.5s) expiry: %v", err)
	}
	// A room that expires at the START of the same whole second.
	early := mkConformRoom(t, rr, app, base)
	earlyWriteAt := base.Add(-domain.RoomRetentionWindow)
	if err := rr.PutValue(early.AppSlug, early.ID, "k", []byte("v"), 0, 0, earlyWriteAt); err != nil {
		t.Fatalf("put to set early (.0s) expiry: %v", err)
	}

	// Cutoff at .0s: the .5s room has NOT expired, the .0s room has (inclusive).
	atStart := base // 12:00:00.0
	expired, err := rr.ExpiredRoomKeys(atStart)
	if err != nil {
		t.Fatalf("expired at .0s: %v", err)
	}
	if refsHas(expired, late.AppSlug, late.ID) {
		t.Fatalf("room expiring at .5s must NOT be expired at a .0s cutoff (sub-second ordering bug), got %v", expired)
	}
	if !refsHas(expired, early.AppSlug, early.ID) {
		t.Fatalf("room expiring at .0s should be inclusive-expired at a .0s cutoff, got %v", expired)
	}

	// Cutoff at .4s: still below the .5s room -> it remains unexpired (proving
	// the boundary is real sub-second time, not whole-second rounding).
	atBelow := base.Add(400 * time.Millisecond)
	expired, err = rr.ExpiredRoomKeys(atBelow)
	if err != nil {
		t.Fatalf("expired at .4s: %v", err)
	}
	if refsHas(expired, late.AppSlug, late.ID) {
		t.Fatalf("room expiring at .5s must NOT be expired at a .4s cutoff, got %v", expired)
	}
}

// --- small helpers ---------------------------------------------------------

// refsHas reports whether the (app, id) pair is in the RoomRef slice.
func refsHas(refs []domain.RoomRef, app domain.Slug, id domain.RoomID) bool {
	for _, ref := range refs {
		if ref.AppSlug == app && ref.ID == id {
			return true
		}
	}
	return false
}

// keyN builds a deterministic distinct key for the i-th value (for the key-cap
// subtest). Keys are within MaxRoomKeyLen.
func keyN(i int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	// base-36, fixed enough to stay distinct for i < MaxRoomKeys.
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
