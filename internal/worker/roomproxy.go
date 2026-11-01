package worker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/drain"
	"github.com/Zamua/hostthis/internal/roomwire"
)

// relayPhase is a reservation's progress through Serve.
type relayPhase int

const (
	// relayReserved: admitted, not yet picked up by Serve.
	relayReserved relayPhase = iota
	// relayDialing: Serve owns the client and is dialing the cell. StopAdmission
	// cancels the dial.
	relayDialing
	// relayDialed: the dial has returned. StopAdmission signals stop instead of
	// cancelling, so the client gets a close frame.
	relayDialed
)

type relayReservation struct {
	key   roomwire.RoomKey
	phase relayPhase
	stop  chan struct{}
	// cancel and client are set on entering relayDialing.
	cancel context.CancelFunc
	client *websocket.Conn
}

// RoomProxy is the Worker backend's real-time layer: a 1:1 WebSocket proxy from
// each client socket to the room's cell, which is the broadcast point. It holds
// no room state: the cell sends the snapshot in the event that attaches the
// upstream socket and mirrors every durable write in the event that commits
// it, so there is no pod-to-pod hop to lose a frame on.
//
// What stays here belongs to the pod: the handshake and origin policy
// (enforced by the caller before Serve), the connection caps, and the
// client-facing heartbeat. The cell's sockets are hibernatable and are never
// pinged from this side; waking a dormant cell every 20s per connection would
// defeat the hibernation.
type RoomProxy struct {
	base   string
	client *http.Client

	// inflight counts reservations; it changes only under mu, alongside
	// reservations.
	inflight drain.Gate

	mu           sync.Mutex
	nextID       uint64
	perRoom      map[roomwire.RoomKey]int
	perApp       map[domain.Slug]int
	reservations map[uint64]*relayReservation
}

// NewRoomProxy relays to the Worker at base, dialing through client; nil
// means http.DefaultClient.
func NewRoomProxy(base string, client *http.Client) *RoomProxy {
	return &RoomProxy{
		base:         base,
		client:       client,
		perRoom:      make(map[roomwire.RoomKey]int),
		perApp:       make(map[domain.Slug]int),
		reservations: make(map[uint64]*relayReservation),
	}
}

// Admit reserves a connection slot. The caps are PER POD: they bound this
// pod's resource use, not the room's global audience.
func (p *RoomProxy) Admit(key roomwire.RoomKey) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.inflight.Admit() {
		return 0, roomwire.ErrRelayDraining
	}
	if p.perRoom[key] >= roomwire.DefaultMaxConnsPerRoom {
		p.inflight.Release()
		return 0, roomwire.ErrRoomFull
	}
	if p.perApp[key.App] >= roomwire.DefaultMaxConnsPerApp {
		p.inflight.Release()
		return 0, roomwire.ErrAppFull
	}
	p.perRoom[key]++
	p.perApp[key.App]++
	p.nextID++
	p.reservations[p.nextID] = &relayReservation{key: key, stop: make(chan struct{})}
	return p.nextID, nil
}

func (p *RoomProxy) Release(_ roomwire.RoomKey, id uint64) {
	p.mu.Lock()
	reservation, ok := p.reservations[id]
	if !ok {
		p.mu.Unlock()
		return
	}
	delete(p.reservations, id)
	key := reservation.key
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
	p.inflight.Release()
	p.mu.Unlock()
	if reservation.cancel != nil {
		reservation.cancel()
	}
}

func (p *RoomProxy) StopAdmission() {
	p.mu.Lock()
	if !p.inflight.Stop() {
		p.mu.Unlock()
		return
	}
	cancels := make([]context.CancelFunc, 0, len(p.reservations))
	for _, reservation := range p.reservations {
		close(reservation.stop)
		if reservation.phase == relayDialing {
			cancels = append(cancels, reservation.cancel)
		}
	}
	p.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (p *RoomProxy) Shutdown(ctx context.Context) error {
	p.StopAdmission()
	err := p.inflight.Wait(ctx)
	if err == nil {
		return nil
	}
	p.mu.Lock()
	clients := make([]*websocket.Conn, 0, len(p.reservations))
	cancels := make([]context.CancelFunc, 0, len(p.reservations))
	for _, reservation := range p.reservations {
		if reservation.phase != relayReserved {
			clients = append(clients, reservation.client)
			cancels = append(cancels, reservation.cancel)
		}
	}
	p.mu.Unlock()
	for _, client := range clients {
		go client.CloseNow() //nolint:errcheck
	}
	for _, cancel := range cancels {
		cancel()
	}
	return err
}

// Serve dials the room's cell and pipes frames until either side ends.
func (p *RoomProxy) Serve(ctx context.Context, key roomwire.RoomKey, id uint64, client *websocket.Conn) {
	defer p.Release(key, id)
	ctx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()
	p.mu.Lock()
	reservation, admitted := p.reservations[id]
	if admitted {
		reservation.phase = relayDialing
		reservation.cancel = relayCancel
		reservation.client = client
	}
	stopping := p.inflight.Stopped()
	p.mu.Unlock()
	if !admitted || stopping {
		client.Close(websocket.StatusServiceRestart, "service restart") //nolint:errcheck
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "") //nolint:errcheck

	wsBase := strings.Replace(p.base, "http", "ws", 1)
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	upstream, _, err := websocket.Dial(dialCtx, wsBase+opRoomJoin.target(roomKey(key.App, key.ID)),
		&websocket.DialOptions{HTTPClient: p.client})
	dialCancel()
	p.mu.Lock()
	if current := p.reservations[id]; current == reservation {
		current.phase = relayDialed
	}
	p.mu.Unlock()
	if err != nil {
		if p.inflight.Stopped() {
			client.Close(websocket.StatusServiceRestart, "service restart") //nolint:errcheck
		} else {
			client.Close(websocket.StatusInternalError, "upstream unavailable") //nolint:errcheck
		}
		return
	}
	defer upstream.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	client.SetReadLimit(roomwire.MaxClientFrameBytes)
	upstream.SetReadLimit(roomwire.MaxServerFrameBytes)

	// The frames are opaque here: ordering, seq and content are the cell's
	// business, and this loop must stay dumb enough to never reorder or drop.
	type relayEnd struct {
		upstream bool
		err      error
	}
	done := make(chan relayEnd, 2)

	// cell -> client: the substantive direction.
	go func() {
		for {
			typ, data, err := upstream.Read(ctx)
			if err != nil {
				done <- relayEnd{upstream: true, err: err}
				return
			}
			if err := client.Write(ctx, typ, data); err != nil {
				done <- relayEnd{err: err}
				return
			}
		}
	}()

	// client -> cell: ephemeral frames are relayed byte-identically. Durable
	// mutations still use the HTTP KV path, which owns caps and sequencing.
	go func() {
		for {
			typ, data, err := client.Read(ctx)
			if err != nil {
				done <- relayEnd{err: err}
				return
			}
			if err := upstream.Write(ctx, typ, data); err != nil {
				done <- relayEnd{upstream: true, err: err}
				return
			}
		}
	}()

	// The client-facing heartbeat: ping under the proxy idle timeouts, reap on
	// a missed pong. The upstream socket is never pinged (see the type comment).
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
					relayCancel()
					return
				}
			}
		}
	}()

	var end relayEnd
	select {
	case end = <-done:
		if p.inflight.Stopped() {
			client.Close(websocket.StatusServiceRestart, "service restart") //nolint:errcheck
			relayCancel()
			return
		}
	case <-reservation.stop:
		client.Close(websocket.StatusServiceRestart, "service restart") //nolint:errcheck
		relayCancel()
		return
	}
	// The close frame must reach the wire before relayCancel: canceling a
	// blocked Read's context tears the connection down abruptly, racing the
	// graceful close into an EOF. The deferred relayCancel runs after the
	// deferred closes.
	if ctx.Err() != nil {
		return
	}
	var closeErr websocket.CloseError
	if end.upstream {
		if errors.As(end.err, &closeErr) {
			client.Close(closeErr.Code, closeErr.Reason) //nolint:errcheck
		} else {
			client.CloseNow() //nolint:errcheck
		}
		return
	}
	if errors.As(end.err, &closeErr) {
		upstream.Close(closeErr.Code, closeErr.Reason) //nolint:errcheck
	} else {
		upstream.CloseNow() //nolint:errcheck
	}
}
