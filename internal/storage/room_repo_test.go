package storage

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

func TestRoom_CreateGetRoundTrip(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	room := mkRoom(repo, t, "app12345", now)

	got, err := repo.GetRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("get room: %v", err)
	}
	if got.AppSlug != room.AppSlug || got.ID != room.ID {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.ExpiresAt.Equal(room.ExpiresAt) {
		t.Fatalf("expires round-trip: got %v want %v", got.ExpiresAt, room.ExpiresAt)
	}
}

func TestRoom_GetNonexistentIsNotFound(t *testing.T) {
	repo := newRoomTestRepo(t)
	// A well-formed but nonexistent room id is ErrNotFound (the
	// existence-not-leaked 404 at the HTTP layer).
	if _, err := repo.GetRoom("app12345", domain.NewRoomID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing room: got %v, want ErrNotFound", err)
	}
}

func TestRoom_KVRoundTripAndScan(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	room := mkRoom(repo, t, "app12345", now)

	if err := repo.PutValue(room.AppSlug, room.ID, "name", []byte("alice"), 0, now); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := repo.PutValue(room.AppSlug, room.ID, "votes", []byte(`{"a":3}`), 0, now); err != nil {
		t.Fatalf("put 2: %v", err)
	}

	v, err := repo.GetValue(room.AppSlug, room.ID, "name")
	if err != nil || !bytes.Equal(v, []byte("alice")) {
		t.Fatalf("get name = %q, %v", v, err)
	}

	kv, err := repo.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if kv.KeyCount() != 2 {
		t.Fatalf("scan key count = %d, want 2", kv.KeyCount())
	}
	if got, _ := kv.Get("votes"); !bytes.Equal(got, []byte(`{"a":3}`)) {
		t.Fatalf("scanned votes = %q", got)
	}
}

func TestRoom_GetMissingKeyIsNotFound(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	room := mkRoom(repo, t, "app12345", now)
	if _, err := repo.GetValue(room.AppSlug, room.ID, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key = %v, want ErrNotFound", err)
	}
}

func TestRoom_Overwrite(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	room := mkRoom(repo, t, "app12345", now)
	if err := repo.PutValue(room.AppSlug, room.ID, "k", []byte("v1"), 0, now); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := repo.PutValue(room.AppSlug, room.ID, "k", []byte("v2-longer"), 0, now); err != nil {
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

func TestRoom_DeleteIsIdempotent(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	room := mkRoom(repo, t, "app12345", now)
	_ = repo.PutValue(room.AppSlug, room.ID, "k", []byte("v"), 0, now)
	if err := repo.DeleteValue(room.AppSlug, room.ID, "k", now); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Second delete of the now-absent key still succeeds.
	if err := repo.DeleteValue(room.AppSlug, room.ID, "k", now); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, err := repo.GetValue(room.AppSlug, room.ID, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key survived delete: %v", err)
	}
}

func TestRoom_WriteToNonexistentRoomIsNotFound(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	if err := repo.PutValue("app12345", domain.NewRoomID(), "k", []byte("v"), 0, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("put to missing room = %v, want ErrNotFound", err)
	}
	if err := repo.DeleteValue("app12345", domain.NewRoomID(), "k", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete in missing room = %v, want ErrNotFound", err)
	}
}

// TestRoom_CrossRoomIsolation is the security property: one room's UUID
// can never read or write another room's data, even in the same app.
func TestRoom_CrossRoomIsolation(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	a := mkRoom(repo, t, "app12345", now)
	b := mkRoom(repo, t, "app12345", now)

	if err := repo.PutValue(a.AppSlug, a.ID, "secret", []byte("from-A"), 0, now); err != nil {
		t.Fatalf("put A: %v", err)
	}
	// Room B's UUID addresses a DIFFERENT keyspace - the key is absent.
	if _, err := repo.GetValue(b.AppSlug, b.ID, "secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("room B saw room A's key: %v", err)
	}
	// B's scan is empty; A's holds its one key.
	kvB, _ := repo.ScanRoom(b.AppSlug, b.ID)
	if kvB.KeyCount() != 0 {
		t.Fatalf("room B scan leaked A data: %d keys", kvB.KeyCount())
	}
	kvA, _ := repo.ScanRoom(a.AppSlug, a.ID)
	if kvA.KeyCount() != 1 {
		t.Fatalf("room A lost its key: %d keys", kvA.KeyCount())
	}
}

// TestRoom_CrossAppIsolation: the same room-UUID string under a
// different app addresses a different keyspace.
func TestRoom_CrossAppIsolation(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	id := domain.NewRoomID()
	// Create the SAME id under two different apps (structurally allowed -
	// the PK is the (app, id) pair).
	for _, app := range []string{"app11111", "app22222"} {
		room := domain.Room{
			AppSlug: domain.Slug(app), ID: id,
			CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RoomRetentionWindow),
		}
		if err := repo.CreateRoom(room, "10.0.0.0/24", 0, now); err != nil {
			t.Fatalf("create %s: %v", app, err)
		}
	}
	if err := repo.PutValue("app11111", id, "k", []byte("app1-data"), 0, now); err != nil {
		t.Fatalf("put app1: %v", err)
	}
	// app2's same-id room does NOT see app1's value.
	if _, err := repo.GetValue("app22222", id, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("app2 read app1's data: %v", err)
	}
}

func TestRoom_PerRoomByteCap(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	room := mkRoom(repo, t, "app12345", now)
	// One value at the cap fits.
	atCap := make([]byte, domain.MaxRoomBytes)
	if err := repo.PutValue(room.AppSlug, room.ID, "big", atCap, 0, now); err != nil {
		t.Fatalf("value at cap rejected: %v", err)
	}
	// Another byte over the cap is rejected; the prior value stays intact.
	if err := repo.PutValue(room.AppSlug, room.ID, "more", []byte("x"), 0, now); !errors.Is(err, ErrRoomDataFull) {
		t.Fatalf("over-cap put = %v, want ErrRoomDataFull", err)
	}
	v, err := repo.GetValue(room.AppSlug, room.ID, "big")
	if err != nil || len(v) != domain.MaxRoomBytes {
		t.Fatalf("prior value disturbed: len=%d err=%v", len(v), err)
	}
}

func TestRoom_PerRoomKeyCap(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	room := mkRoom(repo, t, "app12345", now)
	for i := 0; i < domain.MaxRoomKeys; i++ {
		k := keyName(i)
		if err := repo.PutValue(room.AppSlug, room.ID, k, []byte("v"), 0, now); err != nil {
			t.Fatalf("key %d under cap rejected: %v", i, err)
		}
	}
	if err := repo.PutValue(room.AppSlug, room.ID, "overflow", []byte("v"), 0, now); !errors.Is(err, ErrRoomDataFull) {
		t.Fatalf("over-key-cap put = %v, want ErrRoomDataFull", err)
	}
}

func TestRoom_PerAppAggregateCap(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	// Tiny per-app cap so two small writes trip it.
	const appCap = 10
	r1 := mkRoom(repo, t, "app12345", now)
	r2 := mkRoom(repo, t, "app12345", now)

	if err := repo.PutValue(r1.AppSlug, r1.ID, "k", []byte("123456"), appCap, now); err != nil {
		t.Fatalf("first write under app cap rejected: %v", err)
	}
	// A second write in a DIFFERENT room of the SAME app pushes the app
	// aggregate over -> ErrAppRoomsFull.
	if err := repo.PutValue(r2.AppSlug, r2.ID, "k", []byte("78901"), appCap, now); !errors.Is(err, ErrAppRoomsFull) {
		t.Fatalf("over-app-cap write = %v, want ErrAppRoomsFull", err)
	}
}

func TestRoom_WriteResetsRetentionClock(t *testing.T) {
	repo := newRoomTestRepo(t)
	created := time.Now().UTC().Truncate(time.Second)
	room := mkRoom(repo, t, "app12345", created)

	later := created.Add(5 * 24 * time.Hour)
	if err := repo.PutValue(room.AppSlug, room.ID, "k", []byte("v"), 0, later); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, _ := repo.GetRoom(room.AppSlug, room.ID)
	wantExpiry := later.Add(domain.RoomRetentionWindow)
	if !got.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("put did not reset clock: expires %v, want %v", got.ExpiresAt, wantExpiry)
	}
	// A DELETE also resets the clock.
	evenLater := later.Add(24 * time.Hour)
	if err := repo.DeleteValue(room.AppSlug, room.ID, "k", evenLater); err != nil {
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
	_ = repo.PutValue(room.AppSlug, room.ID, "k", []byte("v"), 0, now)

	// Nothing expired yet.
	expired, err := repo.ExpiredRoomKeys(now)
	if err != nil {
		t.Fatalf("expired keys: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("room expired prematurely: %d", len(expired))
	}
	// Past the window, the room is expired.
	future := now.Add(domain.RoomRetentionWindow + time.Hour)
	expired, _ = repo.ExpiredRoomKeys(future)
	if len(expired) != 1 || expired[0].ID != room.ID {
		t.Fatalf("expired set = %+v", expired)
	}
	// Deleting the room cascades to its values.
	if err := repo.DeleteRoom(room.AppSlug, room.ID); err != nil {
		t.Fatalf("delete room: %v", err)
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

func TestRoom_CreationRateAccounting(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	// Two creations from one subnet under one app.
	for i := 0; i < 2; i++ {
		room := domain.Room{
			AppSlug: "app12345", ID: domain.NewRoomID(),
			CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RoomRetentionWindow),
		}
		if err := repo.CreateRoom(room, "203.0.113.0/24", 0, now); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	perSubnet, perApp, err := repo.CountRoomCreates("app12345", "203.0.113.0/24", now, time.Hour)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if perSubnet != 2 || perApp != 2 {
		t.Fatalf("counts = subnet %d, app %d, want 2,2", perSubnet, perApp)
	}
	// A different subnet has no in-window creations.
	perSubnet, _, _ = repo.CountRoomCreates("app12345", "198.51.100.0/24", now, time.Hour)
	if perSubnet != 0 {
		t.Fatalf("other subnet count = %d, want 0", perSubnet)
	}
	// Pruning past-window rows clears the count.
	if _, err := repo.PruneOldRoomCreates(now.Add(2 * time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	perSubnet, _, _ = repo.CountRoomCreates("app12345", "203.0.113.0/24", now.Add(2*time.Hour), time.Hour)
	if perSubnet != 0 {
		t.Fatalf("after prune count = %d, want 0", perSubnet)
	}
}

func TestRoom_SumActiveRoomBytes(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	a := mkRoom(repo, t, "app11111", now)
	b := mkRoom(repo, t, "app22222", now)
	_ = repo.PutValue(a.AppSlug, a.ID, "k", []byte("12345"), 0, now)
	_ = repo.PutValue(b.AppSlug, b.ID, "k", []byte("678"), 0, now)
	total, err := repo.SumActiveRoomBytes(now)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if total != 8 {
		t.Fatalf("sum active room bytes = %d, want 8", total)
	}
}

func keyName(i int) string {
	const d = "0123456789"
	return "k" + string(d[i/100%10]) + string(d[i/10%10]) + string(d[i%10])
}
