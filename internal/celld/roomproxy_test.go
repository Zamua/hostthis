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

// fakeCell accepts the upstream socket and immediately sends one snapshot
// frame, as the room cell does in its join event.
func fakeCell(t *testing.T, payload []byte) *httptest.Server {
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
		_ = c.Write(r.Context(), websocket.MessageText, payload)
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
	cell := fakeCell(t, []byte(`{"type":"snapshot","seq":0,"state":{}}`))
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

func TestRoomProxyPipesSnapshotAboveLibraryDefault(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 64<<10)
	cell := fakeCell(t, payload)
	defer cell.Close()
	p := celld.NewRoomProxy(cell.URL)
	key := testKey()
	id, err := p.Admit(key)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if aerr == nil {
			p.Serve(r.Context(), key, id, c)
		}
	}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	client.SetReadLimit(roomwire.MaxServerFrameBytes)
	_, got, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read large snapshot: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("large snapshot changed in proxy: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestRoomProxyPreservesUpstreamClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   websocket.StatusCode
		reason string
	}{
		{name: "service restart", code: 1012, reason: "cell restarting"},
		{name: "policy refusal", code: 1008, reason: "reserved control frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					return
				}
				if err := c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"snapshot"}`)); err != nil {
					return
				}
				_ = c.Close(tc.code, tc.reason)
			}))
			defer cell.Close()
			p := celld.NewRoomProxy(cell.URL)
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
			defer front.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http"), nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.CloseNow() //nolint:errcheck
			if _, _, err := client.Read(ctx); err != nil {
				t.Fatalf("read snapshot: %v", err)
			}
			_, _, err = client.Read(ctx)
			var closeErr websocket.CloseError
			if !errors.As(err, &closeErr) {
				t.Fatalf("close error = %v, want websocket.CloseError", err)
			}
			if closeErr.Code != tc.code || closeErr.Reason != tc.reason {
				t.Fatalf("close = %d %q, want %d %q", closeErr.Code, closeErr.Reason, tc.code, tc.reason)
			}
		})
	}
}

func TestRoomProxyPreservesAbruptUpstreamLoss(t *testing.T) {
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		if err := c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"snapshot"}`)); err != nil {
			return
		}
		c.CloseNow() //nolint:errcheck
	}))
	defer cell.Close()
	p := celld.NewRoomProxy(cell.URL)
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
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.CloseNow() //nolint:errcheck
	if _, _, err := client.Read(ctx); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	_, _, err = client.Read(ctx)
	if status := websocket.CloseStatus(err); status != -1 {
		t.Fatalf("close status = %d, want -1 after abrupt upstream loss (error %v)", status, err)
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
	cell := fakeCell(t, []byte(`{"type":"snapshot"}`))
	defer cell.Close()
	p := celld.NewRoomProxy(cell.URL)
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
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.CloseNow() //nolint:errcheck
	if _, _, err := client.Read(ctx); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- p.Shutdown(ctx) }()
	_, _, err = client.Read(ctx)
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
	_, _, err = client.Read(ctx)
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read error = %v, want websocket.CloseError", err)
	}
	if closeErr.Code != websocket.StatusInternalError || closeErr.Reason != "upstream unavailable" {
		t.Fatalf("close = %d %q, want 1011 %q", closeErr.Code, closeErr.Reason, "upstream unavailable")
	}
}
