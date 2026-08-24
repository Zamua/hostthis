package celld_test

// The proxy against a FAKE cell: a local WS server standing where the room
// cell would. What these pin is the PROXY's own contract - dial, pipe
// verbatim, caps, release - not the cell's, which the celld room conformance
// and the staging cross-pod battery own.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/roomwire"
)

// fakeCell accepts the upstream socket and immediately sends one snapshot
// frame, as the room cell does in its join event.
func fakeCell(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/room/join") {
			http.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"snapshot","seq":0,"state":{}}`))
		// Hold the socket open until the client goes away.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
}

func testKey() roomwire.RoomKey {
	return roomwire.RoomKey{App: "app12345", ID: domain.NewRoomID()}
}

// The snapshot the cell sends arrives at the client byte-for-byte: the proxy
// pipes, never parses.
func TestRoomProxyPipesTheSnapshotVerbatim(t *testing.T) {
	cell := fakeCell(t)
	defer cell.Close()
	p := celld.NewRoomProxy(cell.URL)

	key := testKey()
	id, err := p.Admit(key)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}

	// A client socket via a local echo of the production path: an httptest
	// server whose handler runs Serve, exactly as the upgrade handler does.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if aerr != nil {
			return
		}
		p.Serve(r.Context(), key, id, c)
	}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close(websocket.StatusNormalClosure, "") //nolint:errcheck

	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if want := `{"type":"snapshot","seq":0,"state":{}}`; string(data) != want {
		t.Fatalf("snapshot arrived as %q; the proxy must pipe frames verbatim", data)
	}
}

// The per-room cap refuses with the shared sentinel, and Release gives the
// slot back - the admission accounting the HTTP status mapping depends on.
func TestRoomProxyCapsAndRelease(t *testing.T) {
	p := celld.NewRoomProxy("http://127.0.0.1:1")
	key := testKey()
	ids := make([]uint64, 0, roomwire.DefaultMaxConnsPerRoom)
	for range roomwire.DefaultMaxConnsPerRoom {
		id, err := p.Admit(key)
		if err != nil {
			t.Fatalf("admit under the cap: %v", err)
		}
		ids = append(ids, id)
	}
	if _, err := p.Admit(key); err != roomwire.ErrRoomFull {
		t.Fatalf("admit past the cap = %v; want roomwire.ErrRoomFull, the sentinel the HTTP layer maps to 429", err)
	}
	p.Release(key, ids[0])
	if _, err := p.Admit(key); err != nil {
		t.Fatalf("admit after a release: %v; the slot did not come back", err)
	}
}

// An unreachable cell closes the client cleanly rather than hanging the
// handler: the proxy's dial failure is a connection outcome, not a stall.
func TestRoomProxyUnreachableCellClosesTheClient(t *testing.T) {
	p := celld.NewRoomProxy("http://127.0.0.1:1")
	key := testKey()
	id, err := p.Admit(key)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if aerr != nil {
			return
		}
		p.Serve(r.Context(), key, id, c)
	}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("read succeeded against an unreachable cell; want the proxy to close the socket")
	}
}
