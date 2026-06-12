package storage

import (
	"bytes"
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
	if err := repo.PutValue(room.AppSlug, room.ID, "slot/mon", []byte("alice"), 0, 0, now); err != nil {
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
	if err := repo.PutValue(room.AppSlug, room.ID, "slot/tue", []byte("bob"), 0, 0, now); err != nil {
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
	if err := repo.PutValue(room.AppSlug, room.ID, "slot/mon", []byte("carol"), 0, 0, now); err != nil {
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

// TestRoom_IsolationDependsOnRoomIDNamespacing pins the cross-room
// isolation property THROUGH the production repo methods, so it is a real
// regression guard rather than a re-implementation of the WHERE clause.
//
// The security property is "a request carrying room B's UUID can never
// read room A's data, even within the same app." It is enforced
// structurally by the room_id predicate in the WHERE clause of every
// production read (GetValue, ScanRoom). This test exercises EXACTLY those
// methods - it does NOT run its own ad-hoc SQL - so that if a refactor
// drops the room_id predicate from the real GetValue or ScanRoom, this
// test goes red (the same change makes TestRoom_CrossRoomIsolation go red
// too; the two together pin the namespacing from both the point-read and
// the scan side).
//
// Concretely: two rooms A and B exist under the SAME app. They share a key
// name ("secret") with DIFFERENT values, AND each holds a key the other
// does not ("a-only" / "b-only"). That is the adversarial setup - if the
// room_id scoping were dropped, (app, key) alone is ambiguous across the
// two rooms, so B's point-read would surface A's "secret" value and B's
// scan would surface A's "a-only" key (an extra key that should not exist
// in B). We assert the production methods keep them separate from both the
// point-read and the scan side, so dropping the predicate from EITHER
// GetValue or ScanRoom makes this test fail.
func TestRoom_IsolationDependsOnRoomIDNamespacing(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	a := mkRoom(repo, t, "app12345", now)
	b := mkRoom(repo, t, "app12345", now)

	// Shared key name, different values (catches a GetValue predicate drop).
	if err := repo.PutValue(a.AppSlug, a.ID, "secret", []byte("from-A"), 0, 0, now); err != nil {
		t.Fatalf("put A secret: %v", err)
	}
	if err := repo.PutValue(b.AppSlug, b.ID, "secret", []byte("from-B"), 0, 0, now); err != nil {
		t.Fatalf("put B secret: %v", err)
	}
	// Per-room unique keys (catches a ScanRoom predicate drop: a leak shows
	// up as an EXTRA key in the other room's scan, not just an overwrite of
	// a shared key name).
	if err := repo.PutValue(a.AppSlug, a.ID, "a-only", []byte("A-private"), 0, 0, now); err != nil {
		t.Fatalf("put A a-only: %v", err)
	}
	if err := repo.PutValue(b.AppSlug, b.ID, "b-only", []byte("B-private"), 0, 0, now); err != nil {
		t.Fatalf("put B b-only: %v", err)
	}

	// Production point-read: B's UUID reads B's own value, never A's. If the
	// room_id predicate is dropped from GetValue, this surfaces "from-A"
	// (or an ambiguous row) and the assertion fails.
	gotB, err := repo.GetValue(b.AppSlug, b.ID, "secret")
	if err != nil {
		t.Fatalf("B GetValue errored: %v", err)
	}
	if !bytes.Equal(gotB, []byte("from-B")) {
		t.Fatalf("GetValue crossed the room boundary: B read %q, want its own \"from-B\"", gotB)
	}
	gotA, err := repo.GetValue(a.AppSlug, a.ID, "secret")
	if err != nil {
		t.Fatalf("A GetValue errored: %v", err)
	}
	if !bytes.Equal(gotA, []byte("from-A")) {
		t.Fatalf("GetValue crossed the room boundary: A read %q, want its own \"from-A\"", gotA)
	}

	// Production scan: B's scan returns ONLY B's keys ("secret"=from-B,
	// "b-only"), never A's "a-only". If the room_id predicate is dropped
	// from ScanRoom, B's scan surfaces A's "a-only" key too -> 3 keys, and
	// the count + contents assertions fail.
	kvB, err := repo.ScanRoom(b.AppSlug, b.ID)
	if err != nil {
		t.Fatalf("B ScanRoom errored: %v", err)
	}
	if kvB.KeyCount() != 2 {
		t.Fatalf("B scan surfaced %d keys, want exactly 2 (its own): a leak means the room_id scoping was dropped", kvB.KeyCount())
	}
	if _, ok := kvB.Get("a-only"); ok {
		t.Fatalf("B scan leaked A's private key \"a-only\" - the room_id predicate is the guard")
	}
	if got, _ := kvB.Get("secret"); !bytes.Equal(got, []byte("from-B")) {
		t.Fatalf("B scan returned %q for \"secret\", want its own \"from-B\"", got)
	}
}
