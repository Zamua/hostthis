package celld

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/roomwire"
)

// RoomProxy is the celld backend's real-time layer: a 1:1 WebSocket proxy from
// each client socket to the room's cell, which is the broadcast point.
//
// It satisfies the same RoomRelay surface the hub relay does, so the HTTP layer
// cannot tell the backends apart - but it holds no room state. The cell sends
// the snapshot in the event that attaches the upstream socket and mirrors every
// durable write to its sockets in the event that commits it, which is what
// makes cross-pod delivery a non-problem rather than a solved one: there is no
// pod-to-pod hop to lose a frame on.
//
// What stays here is exactly what belongs to the pod: the public handshake and
// origin policy (enforced by the caller before Serve), the connection caps, and
// the client-facing heartbeat. The cell's sockets are hibernatable and are
// deliberately never pinged from this side - waking a dormant cell every 20s
// per connection would defeat the hibernation.
type RoomProxy struct {
	base string

	mu      sync.Mutex
	nextID  uint64
	perRoom map[roomwire.RoomKey]int
	perApp  map[domain.Slug]int
}

func NewRoomProxy(base string) *RoomProxy {
	return &RoomProxy{
		base:    base,
		perRoom: make(map[roomwire.RoomKey]int),
		perApp:  make(map[domain.Slug]int),
	}
}

// Admit reserves a connection slot under the same caps and sentinels the hub
// relay enforces, so the HTTP layer's status mapping keeps working unchanged.
// The caps are PER POD, as they were for the hub: they bound this pod's
// resource use, not the room's global audience.
func (p *RoomProxy) Admit(key roomwire.RoomKey) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.perRoom[key] >= roomwire.DefaultMaxConnsPerRoom {
		return 0, roomwire.ErrRoomFull
	}
	if p.perApp[key.App] >= roomwire.DefaultMaxConnsPerApp {
		return 0, roomwire.ErrAppFull
	}
	p.perRoom[key]++
	p.perApp[key.App]++
	p.nextID++
	return p.nextID, nil
}

func (p *RoomProxy) Release(key roomwire.RoomKey, _ uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.perRoom[key] > 0 {
		p.perRoom[key]--
		if p.perRoom[key] == 0 {
			delete(p.perRoom, key)
		}
	}
	if p.perApp[key.App] > 0 {
		p.perApp[key.App]--
		if p.perApp[key.App] == 0 {
			delete(p.perApp, key.App)
		}
	}
}

// Serve dials the room's cell and pipes frames until either side ends.
func (p *RoomProxy) Serve(ctx context.Context, key roomwire.RoomKey, id uint64, client *websocket.Conn) {
	defer p.Release(key, id)
	defer client.Close(websocket.StatusNormalClosure, "") //nolint:errcheck

	wsBase := strings.Replace(p.base, "http", "ws", 1)
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	upstream, _, err := websocket.Dial(dialCtx, wsBase+"/room/join?room="+urlQuery(roomKey(key.App, key.ID)), nil)
	cancel()
	if err != nil {
		client.Close(websocket.StatusInternalError, "upstream unavailable") //nolint:errcheck
		return
	}
	defer upstream.Close(websocket.StatusNormalClosure, "") //nolint:errcheck

	// The frames are opaque here: ordering, seq and content are the cell's
	// business, and this loop must stay dumb enough to never reorder or drop.
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{}, 2)

	// cell -> client: the substantive direction.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			typ, data, err := upstream.Read(ctx)
			if err != nil {
				return
			}
			if err := client.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}()

	// client -> cell: drained and DROPPED. The durable surface is the HTTP KV
	// API; a socket that could mutate would be a second write path with none
	// of the caps. Reading still matters - it is how client pongs and closes
	// are noticed.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			if _, _, err := client.Read(ctx); err != nil {
				return
			}
		}
	}()

	// The client-facing heartbeat, identical to the hub relay's: ping under the
	// proxy idle timeouts, reap on a missed pong. The upstream socket is never
	// pinged - see the type comment.
	go func() {
		t := time.NewTicker(roomwire.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, roomwire.PingTimeout)
				err := client.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	<-done
}

// CommitAndMirror runs the durable write and discards the frame it built: on
// this backend the MIRROR IS THE CELL'S JOB, done inside the committing event,
// and mirroring from the pod as well would deliver every frame twice.
func (p *RoomProxy) CommitAndMirror(_ roomwire.RoomKey, commit func() (roomwire.Frame, error)) error {
	_, err := commit()
	return err
}

// Probe reports whether the cell endpoint answers at all; used only by wiring
// sanity checks, never on a request path.
func (p *RoomProxy) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return errors.New("celld healthz: " + resp.Status)
	}
	return nil
}
