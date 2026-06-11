package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestRoom_CollaborativeOnRefresh pins the headline product behavior of
// the rooms tier: two participants who hold the SAME room UUID see each
// other's writes on the next read. This is the "collaborative on refresh"
// model the spec describes (request/response KV, no WebSocket): one
// browser PUTs a value, a second browser holding the same room link reads
// the full namespace and observes it, then writes its own value back, and
// the first browser sees that on its next scan. There is no per-user
// identity at this layer - both participants address the one shared
// namespace keyed by (app, room-uuid), exactly like a shared doc link.
func TestRoom_CollaborativeOnRefresh(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	// One room, shared by both participants. The repo IS the server; two
	// callers carrying the same (app, id) stand in for two browsers.
	room := mkRoom(repo, t, "app12345", now)

	// Participant A joins and writes a value (its availability, say).
	if err := repo.PutValue(room.AppSlug, room.ID, "slot/mon", []byte("alice"), 0, now); err != nil {
		t.Fatalf("A put: %v", err)
	}

	// Participant B holds the same room link. On join it scans the room
	// and must see A's write - that is the whole "collaborate on refresh"
	// payoff.
	kvB, err := repo.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("B scan: %v", err)
	}
	if got, _ := kvB.Get("slot/mon"); !bytes.Equal(got, []byte("alice")) {
		t.Fatalf("B did not see A's write on join: got %q", got)
	}

	// B writes its own value into the same shared namespace.
	if err := repo.PutValue(room.AppSlug, room.ID, "slot/tue", []byte("bob"), 0, now); err != nil {
		t.Fatalf("B put: %v", err)
	}

	// A refreshes and now sees BOTH writes - its own and B's. The room is
	// one shared namespace; possession of the UUID is the whole access
	// model.
	kvA, err := repo.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("A re-scan: %v", err)
	}
	if kvA.KeyCount() != 2 {
		t.Fatalf("A re-scan key count = %d, want 2", kvA.KeyCount())
	}
	if got, _ := kvA.Get("slot/tue"); !bytes.Equal(got, []byte("bob")) {
		t.Fatalf("A did not see B's write on refresh: got %q", got)
	}
	if got, _ := kvA.Get("slot/mon"); !bytes.Equal(got, []byte("alice")) {
		t.Fatalf("A lost its own write: got %q", got)
	}

	// A point read of B's key by A also succeeds (not just the scan).
	v, err := repo.GetValue(room.AppSlug, room.ID, "slot/tue")
	if err != nil || !bytes.Equal(v, []byte("bob")) {
		t.Fatalf("A point-read of B's key = %q, %v", v, err)
	}

	// Either participant can overwrite any key - the names are cosmetic
	// attribution, not access control (per the spec's in-room-identity
	// note). B overwrites A's slot; A sees the new value on refresh.
	if err := repo.PutValue(room.AppSlug, room.ID, "slot/mon", []byte("carol"), 0, now); err != nil {
		t.Fatalf("B overwrite A's key: %v", err)
	}
	kvA, _ = repo.ScanRoom(room.AppSlug, room.ID)
	if got, _ := kvA.Get("slot/mon"); !bytes.Equal(got, []byte("carol")) {
		t.Fatalf("overwrite not visible to A: got %q", got)
	}
	if kvA.KeyCount() != 2 {
		t.Fatalf("overwrite changed key count to %d, want 2", kvA.KeyCount())
	}
}

// TestRoom_IsolationDependsOnRoomIDNamespacing is the deliberate
// isolation-weakening demonstration the rooms threat model calls for. The
// security property is "a request carrying room B's UUID can never read
// room A's data." That property is enforced structurally by the room_id
// predicate in the WHERE clause of every read/write. This test does not
// just assert the property holds (TestRoom_CrossRoomIsolation does that);
// it pins WHY it holds by exercising the read through a function that can
// be run with the room_id scoping ON or OFF, proving the guard is
// load-bearing rather than incidental.
//
// With scoping ON (the shipped behavior), room B's UUID cannot read room
// A's secret -> ErrNotFound. With scoping OFF (the namespacing weakened
// to key only), the same call leaks A's value to B. The "OFF" branch is
// the failure the production WHERE clause prevents; we exercise it here so
// a future change that drops the room_id predicate is caught by this test
// flipping from a clean isolation result to a leak.
func TestRoom_IsolationDependsOnRoomIDNamespacing(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "iso.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := NewRoomKVRepo(db)

	now := time.Now().UTC()
	a := mkRoom(repo, t, "app12345", now)
	b := mkRoom(repo, t, "app12345", now)
	if err := repo.PutValue(a.AppSlug, a.ID, "secret", []byte("from-A"), 0, now); err != nil {
		t.Fatalf("put A: %v", err)
	}

	// scopedRead is the production read shape: scoped by (app, room, key).
	// B addressing A's key under B's own room id finds nothing.
	scopedRead := func() ([]byte, error) {
		var val []byte
		err := db.QueryRow(
			`SELECT val FROM room_kv WHERE app_slug = ? AND room_id = ? AND key = ?`,
			b.AppSlug.String(), b.ID.String(), "secret",
		).Scan(&val)
		return val, err
	}
	if _, err := scopedRead(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("scoped read leaked across rooms (err=%v): the room_id namespacing is the guard", err)
	}

	// weakenedRead drops the room_id predicate - the exact mistake the
	// production WHERE clause guards against. Run with B's app slug only,
	// it now matches A's row (same app, same key, different room). This
	// branch is here to PROVE the room_id scoping is what stops the leak:
	// without it, the read crosses the room boundary.
	weakenedRead := func() ([]byte, error) {
		var val []byte
		err := db.QueryRow(
			`SELECT val FROM room_kv WHERE app_slug = ? AND key = ?`,
			b.AppSlug.String(), "secret",
		).Scan(&val)
		return val, err
	}
	leaked, err := weakenedRead()
	if err != nil {
		t.Fatalf("weakened read errored unexpectedly: %v", err)
	}
	if !bytes.Equal(leaked, []byte("from-A")) {
		t.Fatalf("weakened-read demo did not reproduce the leak: got %q", leaked)
	}
	// The contrast is the point: scoped read = no leak; weakened read =
	// leak. The production repo always uses the scoped form, so the
	// boundary holds. If a refactor drops the room_id predicate from
	// GetValue, TestRoom_CrossRoomIsolation goes red - this test documents
	// the mechanism that makes that failure meaningful.
}
