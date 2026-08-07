package http

// Multi-pod relay acceptance harness, the gate the spec names in "Multi-pod
// relay -> Acceptance criteria".
//
// Stands up N in-process pods (independent Server + relay.Relay instances over
// ONE shared storage backend, the production shape) bridged by an in-memory
// PeerPublisher, and drives real WebSocket clients through each pod's real
// upgrade, hub and snapshot paths. The correctness core needs no network
// transport; the gRPC adapter sits behind the same port and is gated by
// multipod_grpc_seam_test.go.
//
// The three acceptance criteria pinned here:
//
//  1. Clients on DIFFERENT pods receive every put/delete mirror, whichever
//     pod's HTTP surface handled the durable write.
//  2. A late join during concurrent cross-pod writes has no gap and no dup.
//  3. A killed pod's clients reconnect to a surviving pod and resync via
//     snapshot + splice, again with no gap and no dup.
//
// Plus a characterization: with NO peer publisher wired, live mirrors are
// pod-local and cross-pod subscribers observe silence.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	nethttp "net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// --- the in-process multi-pod fixture ---------------------------------------

// pod is one in-process "hostthisd pod": its own relay (per-process hub
// state, the thing that splits across pods) + its own HTTP surface, over
// the SHARED storage the fixture owns.
type pod struct {
	srv   *Server
	relay *relay.Relay
	rooms *service.Rooms
	ts    *httptest.Server
}

// multiPod is N pods over one shared metadata store, bridged by memBridge.
type multiPod struct {
	t    *testing.T
	pods []*pod

	mu    sync.RWMutex
	alive []bool // bridge fan-out skips dead pods (a killed pod's transport is gone)
}

// memBridge is the in-memory PeerPublisher for ONE pod: Publish delivers the
// frame synchronously to every OTHER live pod's DeliverFromPeer. Test double
// for the gRPC transport: same port, no network. Delivery is synchronous
// (strongest-ordering transport), yet the cross-pod races the splice contract
// exists for still occur, because two pods' commit->publish sections interleave
// freely.
type memBridge struct {
	h    *multiPod
	self int
}

func (b *memBridge) Publish(key relay.RoomKey, f relay.Frame) {
	b.h.mu.RLock()
	defer b.h.mu.RUnlock()
	for i, p := range b.h.pods {
		if i == b.self || !b.h.alive[i] {
			continue
		}
		p.relay.DeliverFromPeer(key, f)
	}
}

// buildMultiPod stands up n pods over one fresh metadata store. bridged selects
// the peer wiring: true wires the memBridge into every pod's relay (the
// multi-pod deploy), false leaves every relay with a nil publisher (the
// zero-peer relay, which on a multi-pod deploy is the bug).
func buildMultiPod(t *testing.T, n int, bridged bool) *multiPod {
	t.Helper()
	repo := storage.NewShaleRoomRepo(storagetest.NewRepo(t))

	h := &multiPod{t: t, alive: make([]bool, n)}
	for i := range n {
		rooms := service.NewRooms(repo)
		rl := relay.NewRelay(rooms, relay.NewLimits())
		if bridged {
			rl.SetPeerPublisher(&memBridge{h: h, self: i})
		}
		srv := &Server{
			ApexDomain: wsTestApex,
			Rooms:      rooms,
			Relay:      rl,
			Sites:      liveAppSiteReader{},
		}
		ts := httptest.NewServer(srv.Handler())
		h.pods = append(h.pods, &pod{srv: srv, relay: rl, rooms: rooms, ts: ts})
		h.alive[i] = true
	}
	t.Cleanup(func() {
		for i := range h.pods {
			h.kill(i)
		}
	})
	return h
}

// kill takes pod i down: its relay closes every live connection, the bridge
// stops delivering to it, and its HTTP surface stops. Idempotent.
func (h *multiPod) kill(i int) {
	h.mu.Lock()
	dead := !h.alive[i]
	h.alive[i] = false
	h.mu.Unlock()
	if dead {
		return
	}
	h.pods[i].relay.Registry().CloseAll()
	h.pods[i].ts.CloseClientConnections()
	h.pods[i].ts.Close()
}

// put commits a durable PUT through pod i's real HTTP handler stack. Fails the
// test on a non-204.
func (h *multiPod) put(i int, slug, id, key string, val []byte) {
	h.t.Helper()
	if w := reqFrom(h.t, h.pods[i].srv, nethttp.MethodPut, slug, "/api/rooms/"+id+"/"+key, "203.0.113.9:40000", val); w.Code != nethttp.StatusNoContent {
		h.t.Fatalf("pod%d put %s: code %d body %q", i, key, w.Code, w.Body.String())
	}
}

// del commits a durable DELETE through pod i's real HTTP handler stack.
func (h *multiPod) del(i int, slug, id, key string) {
	h.t.Helper()
	if w := reqFrom(h.t, h.pods[i].srv, nethttp.MethodDelete, slug, "/api/rooms/"+id+"/"+key, "203.0.113.9:40000", nil); w.Code != nethttp.StatusNoContent {
		h.t.Fatalf("pod%d delete %s: code %d body %q", i, key, w.Code, w.Body.String())
	}
}

// scanTruth reads the durable ground truth (state + exact seq) from the shared
// storage through the service layer.
func (h *multiPod) scanTruth(slug, id string) domain.RoomKV {
	h.t.Helper()
	kv, err := h.pods[0].rooms.Scan(domain.Slug(slug), domain.RoomID(id))
	if err != nil {
		h.t.Fatalf("scan ground truth: %v", err)
	}
	return kv
}

// --- the splice client -------------------------------------------------------
//
// spliceClient implements the SPEC'd client splice contract ("The client
// splice contract") on top of the harness wsClient:
//
//   - on snapshot: replace state, lastSeq = S
//   - on a durable frame with seq n:
//       n <= lastSeq          -> discard (the no-dup rule)
//       n == lastSeq+1        -> apply, advance
//       n >  lastSeq+1        -> hold in the pending set (out-of-order
//                                arrival is NORMAL: two pods' fan-outs race),
//                                apply the run when the hole fills
//   - a hole that never fills within the wait = a lost frame -> the test
//     FAILS (this transport is lossless, so a stall is a bug; production
//     resyncs by reconnect)
//
// Records every applied seq (exactly-once assertions) and every discarded frame
// (dup deliveries are LEGAL on the wire; applying one is not).

type durableFrame struct {
	Type  string                     `json:"type"`
	Seq   uint64                     `json:"seq"`
	Key   string                     `json:"key"`
	Value json.RawMessage            `json:"value"`
	State map[string]json.RawMessage `json:"state"`
}

type spliceClient struct {
	t    *testing.T
	name string
	c    *wsClient

	lastSeq   uint64
	snapSeq   uint64
	state     map[string]json.RawMessage
	pending   map[uint64]durableFrame
	applied   map[uint64]int
	discarded int
}

// newSpliceClient dials a pod's HTTP surface and consumes the join snapshot
// (always the first frame), initializing lastSeq to its exact seq S. Takes the
// httptest server directly so both the in-memory bridge fixture and the
// real-gRPC seam test share it.
func newSpliceClient(t *testing.T, ctx context.Context, ts *httptest.Server, name, slug, id string) *spliceClient {
	t.Helper()
	sc := &spliceClient{
		t:       t,
		name:    name,
		c:       newWSClient(t, ctx, ts, name, slug, id),
		pending: make(map[uint64]durableFrame),
		applied: make(map[uint64]int),
	}
	snap := sc.c.expectSnapshotFrame(ctx)
	sc.ingest(snap)
	return sc
}

func (sc *spliceClient) close() { sc.c.close() }

// ingest runs one frame through the splice contract. A frame that is not
// a server envelope (an ephemeral peer frame) is ignored: ephemeral
// payloads carry no seq and no contract.
func (sc *spliceClient) ingest(data []byte) {
	var f durableFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return // opaque ephemeral payload
	}
	switch f.Type {
	case relay.TypeSnapshot:
		sc.state = make(map[string]json.RawMessage, len(f.State))
		maps.Copy(sc.state, f.State)
		sc.lastSeq = f.Seq
		sc.snapSeq = f.Seq
		// Frames buffered from before the snapshot splice onto the new base
		// (only possible when the snapshot is not the connection's first frame).
		sc.drainPending()
	case relay.TypePut, relay.TypeDelete:
		if f.Seq <= sc.lastSeq {
			sc.discarded++ // already reflected (snapshot or earlier frame)
			return
		}
		sc.pending[f.Seq] = f
		sc.drainPending()
	}
}

func (sc *spliceClient) drainPending() {
	for {
		f, ok := sc.pending[sc.lastSeq+1]
		if !ok {
			return
		}
		delete(sc.pending, sc.lastSeq+1)
		sc.apply(f)
		sc.lastSeq = f.Seq
	}
}

func (sc *spliceClient) apply(f durableFrame) {
	sc.applied[f.Seq]++
	switch f.Type {
	case relay.TypePut:
		sc.state[f.Key] = f.Value
	case relay.TypeDelete:
		delete(sc.state, f.Key)
	}
}

// awaitSeq pumps inbound frames through the splice until lastSeq reaches
// target, failing the test if the stream stalls past the quiet window: on a
// lossless transport a stalled splice means a lost or missing frame, the bug
// this harness gates.
func (sc *spliceClient) awaitSeq(ctx context.Context, target uint64, quiet time.Duration) {
	sc.t.Helper()
	for sc.lastSeq < target {
		data := sc.c.nextFrame(ctx, quiet)
		if data == nil {
			sc.t.Fatalf("%s: splice stalled at seq %d waiting for %d (pending holes: %v)",
				sc.name, sc.lastSeq, target, sc.pendingSeqs())
		}
		sc.ingest(data)
	}
}

func (sc *spliceClient) pendingSeqs() []uint64 {
	out := make([]uint64, 0, len(sc.pending))
	for s := range sc.pending {
		out = append(out, s)
	}
	return out
}

// assertExactlyOnceSince asserts the applied set is EXACTLY the dense range
// (since, through]: every seq in it applied exactly once (no gap, no dup),
// and nothing at or below since was ever applied (the discard rule held).
func (sc *spliceClient) assertExactlyOnceSince(since, through uint64) {
	sc.t.Helper()
	for s := since + 1; s <= through; s++ {
		switch n := sc.applied[s]; n {
		case 1:
		case 0:
			sc.t.Fatalf("%s: seq %d never applied (GAP)", sc.name, s)
		default:
			sc.t.Fatalf("%s: seq %d applied %d times (DUP)", sc.name, s, n)
		}
	}
	for s, n := range sc.applied {
		if s <= since && n > 0 {
			sc.t.Fatalf("%s: seq %d (<= snapshot seq %d) applied %d times - the discard rule failed", sc.name, s, since, n)
		}
		if s > through {
			sc.t.Fatalf("%s: unexpected applied seq %d past %d", sc.name, s, through)
		}
	}
}

// assertStateEquals compares the spliced client state against the durable
// ground truth, value-encoded exactly as the wire encodes it.
func (sc *spliceClient) assertStateEquals(truth domain.RoomKV) {
	sc.t.Helper()
	if len(sc.state) != truth.KeyCount() {
		sc.t.Fatalf("%s: spliced state holds %d keys, ground truth %d (state=%v)",
			sc.name, len(sc.state), truth.KeyCount(), rawKeysOf(sc.state))
	}
	for k, v := range truth.Values {
		got, ok := sc.state[k]
		if !ok {
			sc.t.Fatalf("%s: spliced state missing key %q", sc.name, k)
		}
		if want := domain.RoomWireValue(v); string(got) != string(want) {
			sc.t.Fatalf("%s: key %q spliced to %s, ground truth %s", sc.name, k, got, want)
		}
	}
}

func rawKeysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// THE REPRO: a zero-peer relay on a multi-pod deploy is pod-local. Two clients
// whose sockets live on pods 0 and 1: a durable PUT handled by pod 2 reaches
// NEITHER, and a PUT handled by pod 0 reaches only pod 0's client, the (N-1)/N
// live-mirror loss the peer fan-out exists to fix. The durable KV stays correct
// throughout; only the LIVE delta splits.
//
// This test characterizes the bug and passes with or without the peer wiring.
// The gate that FAILS without it is TestMultiPod_BroadcastReachesAllPods.
// ---------------------------------------------------------------------------

func TestMultiPod_ZeroPeerRelayIsPodLocal(t *testing.T) {
	h := buildMultiPod(t, 3, false /* NO peer bridge: the pre-branch relay */)
	const slug = "appz2345"
	id := mkRoom(t, h.pods[0].rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	clientA := newWSClient(t, ctx, h.pods[0].ts, "clientA", slug, id)
	defer clientA.close()
	clientB := newWSClient(t, ctx, h.pods[1].ts, "clientB", slug, id)
	defer clientB.close()
	clientA.expectSnapshotFrame(ctx)
	clientB.expectSnapshotFrame(ctx)

	// A PUT handled by pod 2 (no local subscribers): the durable write succeeds
	// and is mirrored only into pod 2's own empty hub, so both remote clients
	// observe SILENCE.
	h.put(2, slug, id, "routed-to-2", []byte(`"lost-live"`))
	clientA.expectSilence(ctx, 700*time.Millisecond, "pod-local mirror: pod2's PUT must not reach pod0 without peer fan-out")
	clientB.expectSilence(ctx, 700*time.Millisecond, "pod-local mirror: pod2's PUT must not reach pod1 without peer fan-out")

	// A PUT handled by pod 0 reaches pod 0's client and ONLY pod 0's client.
	h.put(0, slug, id, "routed-to-0", []byte(`"pod0-only"`))
	clientA.expectFrame(ctx, 3*time.Second, "pod0's own mirror", func(b []byte) bool {
		return hasType(b, relay.TypePut) && containsSub(b, `"routed-to-0"`)
	})
	clientB.expectSilence(ctx, 700*time.Millisecond, "pod-local mirror: pod0's PUT must not reach pod1 without peer fan-out")

	// The durable KV was never wrong: both writes are in the shared store.
	truth := h.scanTruth(slug, id)
	if truth.KeyCount() != 2 || truth.Seq != 2 {
		t.Fatalf("ground truth keys=%d seq=%d, want 2/2 (the durable tier is not what splits)", truth.KeyCount(), truth.Seq)
	}
}

// ---------------------------------------------------------------------------
// ACCEPTANCE 1: two clients on different pods receive EVERY put/delete mirror,
// including writes routed through a third pod, through each client's own pod,
// and a delete of an absent key (which still commits and still assigns a seq).
// Ordering, de-duplication and completeness ride the per-room seq: the splice
// client asserts the dense range applied exactly once and the final state
// byte-equals the durable truth. Ephemeral (payload-opaque) frames cross pods
// on the same path.
// ---------------------------------------------------------------------------

func TestMultiPod_BroadcastReachesAllPods(t *testing.T) {
	h := buildMultiPod(t, 3, true)
	const slug = "appz2345"
	id := mkRoom(t, h.pods[0].rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	clientA := newSpliceClient(t, ctx, h.pods[0].ts, "clientA", slug, id)
	defer clientA.close()
	clientB := newSpliceClient(t, ctx, h.pods[1].ts, "clientB", slug, id)
	defer clientB.close()
	if clientA.snapSeq != 0 || clientB.snapSeq != 0 {
		t.Fatalf("fresh room snapshots at seq %d/%d, want 0/0", clientA.snapSeq, clientB.snapSeq)
	}

	// Ten durable mutations spread across every pod's HTTP surface (any pod can
	// commit; ordering is a property of the data, not the topology), covering
	// both verbs plus the absent-key delete.
	h.put(2, slug, id, "board/1", []byte(`{"cell":"x"}`))       // seq 1, via the no-subscriber pod
	h.put(2, slug, id, "board/2", []byte(`{"cell":"o"}`))       // seq 2
	h.put(0, slug, id, "cursor/a", []byte(`"a1"`))              // seq 3, via A's own pod
	h.put(1, slug, id, "cursor/b", []byte(`"b1"`))              // seq 4, via B's own pod
	h.del(2, slug, id, "board/2")                               // seq 5, delete via third pod
	h.put(2, slug, id, "board/1", []byte(`{"cell":"x","n":2}`)) // seq 6, overwrite
	h.del(0, slug, id, "never-existed")                         // seq 7, absent-key delete still mirrors
	h.put(1, slug, id, "title", []byte(`"our board"`))          // seq 8
	h.del(1, slug, id, "cursor/a")                              // seq 9
	h.put(0, slug, id, "done", []byte("true"))                  // seq 10

	const total = 10
	clientA.awaitSeq(ctx, total, 3*time.Second)
	clientB.awaitSeq(ctx, total, 3*time.Second)

	truth := h.scanTruth(slug, id)
	if truth.Seq != total {
		t.Fatalf("ground truth seq = %d, want %d", truth.Seq, total)
	}
	for _, sc := range []*spliceClient{clientA, clientB} {
		sc.assertExactlyOnceSince(0, total)
		sc.assertStateEquals(truth)
	}

	// Ephemeral cross-pod fan-out rides the same path, payload verbatim.
	clientA.c.send(ctx, "ephemeral-from-A")
	if got := clientB.c.expectFrame(ctx, 3*time.Second, "cross-pod ephemeral frame", func(b []byte) bool {
		return string(b) == "ephemeral-from-A"
	}); got == nil {
		t.Fatalf("clientB: expected cross-pod ephemeral frame")
	}
	clientB.c.send(ctx, "ephemeral-from-B")
	clientA.c.expectFrame(ctx, 3*time.Second, "cross-pod ephemeral frame", func(b []byte) bool {
		return string(b) == "ephemeral-from-B"
	})
}

// ---------------------------------------------------------------------------
// ACCEPTANCE 2: late join during concurrent cross-pod writes, no gap and no
// dup, with the splice at the snapshot's exact seq S. Two writers hammer the
// same room through DIFFERENT pods (their commit->fanout sections interleave
// freely, so the joiner's stream races both pods' mirrors and the snapshot
// read); the joiner must reconstruct exactly the durable state: nothing at or
// below its snapshot S applied, the dense range (S, total] applied exactly
// once, spliced state byte-equal to the shared store. Run under -count=20 to
// sample the interleavings.
// ---------------------------------------------------------------------------

func TestMultiPod_LateJoinDuringConcurrentCrossPodWrites(t *testing.T) {
	h := buildMultiPod(t, 3, true)
	const slug = "appz2345"
	id := mkRoom(t, h.pods[0].rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two concurrent writers on different pods, each PUTting its own keys and
	// deleting a few again so the stream carries both verbs.
	const perWriter = 22 // 18 puts + 4 deletes each
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 18 {
				h.put(w, slug, id, fmt.Sprintf("w%d/k%02d", w, i), fmt.Appendf(nil, `{"writer":%d,"i":%d}`, w, i))
			}
			for i := range 4 {
				h.del(w, slug, id, fmt.Sprintf("w%d/k%02d", w, i))
			}
		}(w)
	}

	// Join mid-stream: wait until at least a third of the writes committed (the
	// durable seq is the ground truth for "mid"), then connect through pod 2, a
	// pod NEITHER writer routes through, so every mirror the joiner sees crossed
	// pods.
	for h.scanTruth(slug, id).Seq < 15 {
		time.Sleep(2 * time.Millisecond)
	}
	joiner := newSpliceClient(t, ctx, h.pods[2].ts, "late-joiner", slug, id)
	defer joiner.close()
	if joiner.snapSeq < 15 {
		t.Fatalf("joiner snapshot seq %d, want >= 15 (the join gate polls the durable seq first)", joiner.snapSeq)
	}

	wg.Wait()
	// A post-join tail through both writer pods guarantees live spliced
	// frames exist even if the join landed after the concurrent phase.
	const tail = 6
	for i := range tail {
		h.put(i%2, slug, id, fmt.Sprintf("tail/k%d", i), []byte(`"t"`))
	}

	const total = 2*perWriter + tail
	joiner.awaitSeq(ctx, total, 3*time.Second)

	truth := h.scanTruth(slug, id)
	if truth.Seq != total {
		t.Fatalf("ground truth seq = %d, want %d", truth.Seq, total)
	}
	joiner.assertExactlyOnceSince(joiner.snapSeq, total)
	joiner.assertStateEquals(truth)
}

// ---------------------------------------------------------------------------
// ACCEPTANCE 3: a relay pod dies mid-stream. Its client's socket dies with it
// (accepted: WebSockets die with their pod), the client reconnects to a
// SURVIVING pod through the normal join, and snapshot + splice resync it: the
// fresh snapshot's S2 covers everything missed during the outage, the discard
// rule drops any frame at or below S2, and the stream from S2 on is dense and
// exactly-once. Writes never stop flowing during the kill: they route through
// surviving pods and the durable tier is unaffected.
// ---------------------------------------------------------------------------

func TestMultiPod_PodKillMidStreamReconnectResync(t *testing.T) {
	h := buildMultiPod(t, 3, true)
	const slug = "appz2345"
	id := mkRoom(t, h.pods[0].rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	victim := newSpliceClient(t, ctx, h.pods[0].ts, "victim", slug, id)
	defer victim.close()

	// A steady writer through pod 2, paced so the kill lands mid-stream.
	const total = 30
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := range total {
			h.put(2, slug, id, fmt.Sprintf("k%02d", i), fmt.Appendf(nil, `{"i":%d}`, i))
			time.Sleep(3 * time.Millisecond)
		}
	}()

	// Let the victim splice part of the stream, then kill its pod.
	victim.awaitSeq(ctx, 8, 3*time.Second)
	epoch1Last := victim.lastSeq
	h.kill(0)

	// The victim observes its socket die with the pod.
	victim.c.expectClosed(ctx, 5*time.Second, "its pod was killed")
	victim.assertExactlyOnceSince(victim.snapSeq, epoch1Last) // epoch 1 was clean up to where it spliced

	// Reconnect to a SURVIVING pod: normal join, fresh snapshot, fresh splice
	// base. S2 can only be at or past everything epoch 1 applied (the durable
	// seq never regresses).
	revived := newSpliceClient(t, ctx, h.pods[1].ts, "revived", slug, id)
	defer revived.close()
	if revived.snapSeq < epoch1Last {
		t.Fatalf("reconnect snapshot seq %d regressed below the %d the victim had already applied", revived.snapSeq, epoch1Last)
	}

	<-writerDone
	revived.awaitSeq(ctx, total, 3*time.Second)

	truth := h.scanTruth(slug, id)
	if truth.Seq != total {
		t.Fatalf("ground truth seq = %d, want %d", truth.Seq, total)
	}
	revived.assertExactlyOnceSince(revived.snapSeq, total)
	revived.assertStateEquals(truth)
}
