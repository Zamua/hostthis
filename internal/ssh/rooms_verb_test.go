package ssh_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"
)

// roomCall sends one request to the public rooms API and returns its status
// and body.
func roomCall(t *testing.T, c *http.Client, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close() //nolint:errcheck
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// createRoom creates a room in the app the rooms API at api serves and
// returns its id.
func createRoom(t *testing.T, c *http.Client, api string) string {
	t.Helper()
	code, body := roomCall(t, c, http.MethodPost, api, "")
	var room struct {
		ID string `json:"id"`
	}
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &room) != nil || room.ID == "" {
		t.Fatalf("create room: %d %s", code, body)
	}
	return room.ID
}

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
	// The app's users reach its rooms through the Worker's public rooms API.
	api := g.httpURL + "/p/" + slug + "/api/rooms"
	var ids []string
	for range 3 {
		ids = append(ids, createRoom(t, g.client, api))
	}
	greeting := api + "/" + ids[0] + "/greeting"
	if code, body := roomCall(t, g.client, http.MethodPut, greeting, `"hello"`); code != http.StatusNoContent {
		t.Fatalf("put: %d %s", code, body)
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
	if code, body := roomCall(t, g.client, http.MethodGet, greeting, ""); code != http.StatusNotFound {
		t.Fatalf("read of a deleted room = %d %s, want 404", code, body)
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
	createRoom(t, g.client, api)
	for _, cmd := range []string{"rooms", "rooms " + slug + " rm", "rooms " + slug + " rm not-a-room", "rooms " + slug + " purge"} {
		if _, _, code := g.run(cmd, nil); code != 2 {
			t.Fatalf("%q: exit %d, want 2", cmd, code)
		}
	}
}
