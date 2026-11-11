package ssh_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/worker"

	xssh "golang.org/x/crypto/ssh"
)

// The owner of an app lists its rooms and deletes one, several or all of them;
// a deleted room is gone for its users, and nobody else can see or touch the
// app's rooms.
func TestRooms_OwnerListsAndDeletes(t *testing.T) {
	g := startStack(t)
	url, stderr, code := g.run("", []byte("<!doctype html><p>app</p>"))
	if code != 0 {
		t.Fatalf("upload: exit %d %q", code, stderr)
	}
	slug := extractSlug(url)
	app, err := domain.ParseSlug(slug)
	if err != nil {
		t.Fatal(err)
	}
	rooms := service.NewRooms(worker.NewRoomRepo(g.endpoint, nil))
	var ids []string
	for range 3 {
		room, err := rooms.Create(app, "203.0.113.0/24")
		if err != nil {
			t.Fatalf("create room: %v", err)
		}
		ids = append(ids, room.ID.String())
	}
	firstID, err := domain.ParseRoomID(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rooms.Put(app, firstID, "greeting", []byte(`"hello"`)); err != nil {
		t.Fatalf("put: %v", err)
	}

	listed := func() []string {
		t.Helper()
		out, stderr, code := g.run("rooms "+slug+" -o json", nil)
		if code != 0 {
			t.Fatalf("rooms: exit %d %q", code, stderr)
		}
		var views []struct {
			Room      string `json:"room"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal([]byte(out), &views); err != nil {
			t.Fatalf("rooms json %q: %v", out, err)
		}
		got := make([]string, len(views))
		for i, v := range views {
			if v.CreatedAt == "" {
				t.Fatalf("room %s has no created_at", v.Room)
			}
			got[i] = v.Room
		}
		return got
	}
	if got := listed(); len(got) != 3 {
		t.Fatalf("listed %d rooms, want 3: %v", len(got), got)
	}
	table, stderr, code := g.run("rooms "+slug, nil)
	if code != 0 || !strings.Contains(table, ids[0]) || !strings.Contains(stderr, "3 of 500 rooms") {
		t.Fatalf("rooms table: exit %d stdout %q stderr %q", code, table, stderr)
	}

	// Another key learns nothing and removes nothing.
	_, otherCfg := freshKeyConfig(t)
	other, err := xssh.Dial("tcp", g.sshAddr, otherCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	for _, cmd := range []string{"rooms " + slug, "rooms " + slug + " rm " + ids[0], "rooms " + slug + " rm --all"} {
		if _, stderr, code := g.runOn(other, cmd, nil); code != 4 || !strings.Contains(stderr, "not found") {
			t.Fatalf("%q as another key: exit %d %q, want 4 not found", cmd, code, stderr)
		}
	}
	if got := listed(); len(got) != 3 {
		t.Fatalf("after another key's attempts: %d rooms, want 3", len(got))
	}

	// One room, with its stored value, plus an id the app never had.
	absent := "00000000-0000-4000-8000-000000000000"
	_, stderr, code = g.run("rooms "+slug+" rm "+ids[0]+" "+absent, nil)
	if code != 0 || !strings.Contains(stderr, "removed 1 room(s).") || !strings.Contains(stderr, absent+": no such room") {
		t.Fatalf("rm one: exit %d %q", code, stderr)
	}
	if _, err := rooms.Get(app, firstID, "greeting"); !errors.Is(err, service.ErrRoomNotFound) {
		t.Fatalf("read of a deleted room = %v, want ErrRoomNotFound", err)
	}
	if got := listed(); len(got) != 2 {
		t.Fatalf("after rm one: %d rooms, want 2: %v", len(got), got)
	}

	out, stderr, code := g.run("rooms "+slug+" rm --all -o json", nil)
	if code != 0 {
		t.Fatalf("rm --all: exit %d %q", code, stderr)
	}
	var removed struct {
		Removed []string `json:"removed"`
		Absent  []string `json:"absent"`
	}
	if err := json.Unmarshal([]byte(out), &removed); err != nil || len(removed.Removed) != 2 || len(removed.Absent) != 0 {
		t.Fatalf("rm --all json %q: %v", out, err)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("after rm --all: %v, want none", got)
	}
	if _, stderr, code := g.run("rooms "+slug, nil); code != 0 || !strings.Contains(stderr, "no rooms") {
		t.Fatalf("empty list: exit %d %q", code, stderr)
	}

	// A new room can be created in the freed app, and bad arguments are usage errors.
	if _, err := rooms.Create(app, "203.0.113.0/24"); err != nil {
		t.Fatalf("create after deleting all: %v", err)
	}
	for _, cmd := range []string{"rooms", "rooms " + slug + " rm", "rooms " + slug + " rm not-a-room", "rooms " + slug + " purge"} {
		if _, _, code := g.run(cmd, nil); code != 2 {
			t.Fatalf("%q: exit %d, want 2", cmd, code)
		}
	}
}
