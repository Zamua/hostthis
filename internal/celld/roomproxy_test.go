package celld_test

// The proxy against a FAKE cell: a local WS server standing where the room
// cell would. What these pin is the PROXY's own contract - dial, pipe
// verbatim, caps, release - not the cell's, which the celld room conformance
// and the staging cross-pod battery own.

import (
	"bytes"
	"context"
	"errors"
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

// fakeRoomCell accepts the upstream socket and immediately sends one snapshot
// frame, as the room cell does in its join event, then runs after; nil holds
// the socket open until the client goes away.
func fakeRoomCell(t *testing.T, payload []byte, after func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/room/join") {
			http.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		if err := c.Write(r.Context(), websocket.MessageText, payload); err != nil {
			return
		}
		if after != nil {
			after(c)
			return
		}
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testKey() roomwire.RoomKey {
	return roomwire.RoomKey{App: "app12345", ID: domain.NewRoomID()}
}

// proxied admits one connection to a proxy at base and dials it through a
// front server whose handler runs Serve, exactly as the upgrade handler does.
func proxied(t *testing.T, base string) (*celld.RoomProxy, *websocket.Conn, context.Context) {
	t.Helper()
	p := celld.NewRoomProxy(base)
	key := testKey()
	id, err := p.Admit(key)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err == nil {
			p.Serve(r.Context(), key, id, c)
		}
	}))
	t.Cleanup(front.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.CloseNow() }) //nolint:errcheck
	return p, client, ctx
}

// The snapshot the cell sends arrives at the client byte-for-byte: the proxy
// pipes, never parses.
func TestRoomProxyPipesTheSnapshotVerbatim(t *testing.T) {
	cell := fakeRoomCell(t, []byte(`{"type":"snapshot","seq":0,"state":{}}`), nil)
	_, client, ctx := proxied(t, cell.URL)
	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if want := `{"type":"snapshot","seq":0,"state":{}}`; string(data) != want {
		t.Fatalf("snapshot arrived as %q; the proxy must pipe frames verbatim", data)
	}
}

func TestRoomProxyPipesSnapshotAboveLibraryDefault(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 64<<10)
	cell := fakeRoomCell(t, payload, nil)
	_, client, ctx := proxied(t, cell.URL)
	client.SetReadLimit(roomwire.MaxServerFrameBytes)
	_, got, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read large snapshot: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("large snapshot changed in proxy: got %d bytes, want %d", len(got), len(payload))
	}
}

// How the cell ends the upstream socket is how the client sees its socket
// end: a close frame keeps its code and reason, and an abrupt loss (no close
// frame, status -1) stays abrupt.
func TestRoomProxyPreservesUpstreamClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   websocket.StatusCode
		reason string
	}{
		{name: "service restart", code: 1012, reason: "cell restarting"},
		{name: "policy refusal", code: 1008, reason: "reserved control frame"},
		{name: "abrupt loss", code: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cell := fakeRoomCell(t, []byte(`{"type":"snapshot"}`), func(c *websocket.Conn) {
				if tc.code < 0 {
					c.CloseNow() //nolint:errcheck
					return
				}
				_ = c.Close(tc.code, tc.reason)
			})
			_, client, ctx := proxied(t, cell.URL)
			if _, _, err := client.Read(ctx); err != nil {
				t.Fatalf("read snapshot: %v", err)
			}
			_, _, err := client.Read(ctx)
			if got := websocket.CloseStatus(err); got != tc.code {
				t.Fatalf("close status = %d, want %d (error %v)", got, tc.code, err)
			}
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Reason != tc.reason {
				t.Fatalf("close reason = %q, want %q", closeErr.Reason, tc.reason)
			}
		})
	}
}

func TestRoomProxyStopsAdmission(t *testing.T) {
	p := celld.NewRoomProxy("http://127.0.0.1:1")
	p.StopAdmission()
	p.StopAdmission()
	if _, err := p.Admit(testKey()); !errors.Is(err, roomwire.ErrRelayDraining) {
		t.Fatalf("admit after stop = %v, want ErrRelayDraining", err)
	}
}

func TestRoomProxyShutdownWaitsForAdmittedConnection(t *testing.T) {
	p := celld.NewRoomProxy("http://127.0.0.1:1")
	key := testKey()
	id, err := p.Admit(key)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Shutdown(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("shutdown returned across Admit/Serve gap: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	p.Release(key, id)
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestRoomProxyShutdownClosesActiveClientWithServiceRestart(t *testing.T) {
	cell := fakeRoomCell(t, []byte(`{"type":"snapshot"}`), nil)
	p, client, ctx := proxied(t, cell.URL)
	if _, _, err := client.Read(ctx); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- p.Shutdown(ctx) }()
	_, _, err := client.Read(ctx)
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("close error = %v, want websocket.CloseError", err)
	}
	if closeErr.Code != websocket.StatusServiceRestart || closeErr.Reason != "service restart" {
		t.Fatalf("close = %d %q, want 1012 %q", closeErr.Code, closeErr.Reason, "service restart")
	}
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
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
	p.Release(key, ids[0])
	if _, err := p.Admit(key); err != nil {
		t.Fatalf("admit after a release: %v; the slot did not come back", err)
	}
	if _, err := p.Admit(key); !errors.Is(err, roomwire.ErrRoomFull) {
		t.Fatalf("admit after duplicate release = %v, want ErrRoomFull", err)
	}
}

// An unreachable cell closes the client cleanly rather than hanging the
// handler: the proxy's dial failure is a connection outcome, not a stall.
func TestRoomProxyUnreachableCellClosesTheClient(t *testing.T) {
	_, client, ctx := proxied(t, "http://127.0.0.1:1")
	_, _, err := client.Read(ctx)
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read error = %v, want websocket.CloseError", err)
	}
	if closeErr.Code != websocket.StatusInternalError || closeErr.Reason != "upstream unavailable" {
		t.Fatalf("close = %d %q, want 1011 %q", closeErr.Code, closeErr.Reason, "upstream unavailable")
	}
}
