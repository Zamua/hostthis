package storage

import (
	"bytes"
	"testing"
	"time"
)

// TestRoom_CollaborativeOnRefresh pins the collaborate-on-refresh model: two
// callers holding the SAME room UUID address one shared namespace keyed by
// (app, room-uuid), so each sees the other's writes on its next read. There is
// no per-user identity at this layer.
func TestRoom_CollaborativeOnRefresh(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	// Two callers carrying the same (app, id) stand in for two browsers.
	room := mkRoom(repo, t, "app12345", now)

	if _, err := repo.PutValue(room.AppSlug, room.ID, "slot/mon", []byte("alice"), 0, now); err != nil {
		t.Fatalf("A put: %v", err)
	}

	kvB, err := repo.ScanRoom(room.AppSlug, room.ID)
	if err != nil {
		t.Fatalf("B scan: %v", err)
	}
	if got, _ := kvB.Get("slot/mon"); !bytes.Equal(got, []byte("alice")) {
		t.Fatalf("B did not see A's write on join: got %q", got)
	}

	if _, err := repo.PutValue(room.AppSlug, room.ID, "slot/tue", []byte("bob"), 0, now); err != nil {
		t.Fatalf("B put: %v", err)
	}

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

	// The point read, not just the scan, crosses between participants.
	v, err := repo.GetValue(room.AppSlug, room.ID, "slot/tue")
	if err != nil || !bytes.Equal(v, []byte("bob")) {
		t.Fatalf("A point-read of B's key = %q, %v", v, err)
	}

	// Either participant can overwrite any key: the names are cosmetic
	// attribution, not access control.
	if _, err := repo.PutValue(room.AppSlug, room.ID, "slot/mon", []byte("carol"), 0, now); err != nil {
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

// TestRoom_IsolationDependsOnRoomIDNamespacing pins that a request carrying
// room B's UUID can never read room A's data, even within the same app. The
// guard is the room_id predicate in every production read; this test drives
// EXACTLY GetValue and ScanRoom rather than its own SQL, so dropping the
// predicate from either one goes red.
//
// The setup is adversarial on both axes: two rooms under one app share a key
// name with different values, and each holds a key the other does not. Without
// room_id scoping, (app, key) is ambiguous, so B's point-read surfaces A's
// value and B's scan surfaces A's private key.
func TestRoom_IsolationDependsOnRoomIDNamespacing(t *testing.T) {
	repo := newRoomTestRepo(t)
	now := time.Now().UTC()
	a := mkRoom(repo, t, "app12345", now)
	b := mkRoom(repo, t, "app12345", now)

	// Shared key name, different values: catches a GetValue predicate drop.
	if _, err := repo.PutValue(a.AppSlug, a.ID, "secret", []byte("from-A"), 0, now); err != nil {
		t.Fatalf("put A secret: %v", err)
	}
	if _, err := repo.PutValue(b.AppSlug, b.ID, "secret", []byte("from-B"), 0, now); err != nil {
		t.Fatalf("put B secret: %v", err)
	}
	// Per-room unique keys: catches a ScanRoom predicate drop, which shows up
	// as an EXTRA key in the other room's scan.
	if _, err := repo.PutValue(a.AppSlug, a.ID, "a-only", []byte("A-private"), 0, now); err != nil {
		t.Fatalf("put A a-only: %v", err)
	}
	if _, err := repo.PutValue(b.AppSlug, b.ID, "b-only", []byte("B-private"), 0, now); err != nil {
		t.Fatalf("put B b-only: %v", err)
	}

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

	// B's scan must return ONLY B's two keys; a leak makes it three.
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
