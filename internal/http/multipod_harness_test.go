package http

// This file is the MULTI-POD relay acceptance harness: the gate the spec
// names in "Multi-pod relay -> Acceptance criteria". It stands up N
// in-process "pods" - N independent Server + relay.Relay instances over
// ONE shared storage backend (exactly the production shape: pods share
// the storage cluster, nothing else) - bridged by an in-memory
// implementation of the relay's PeerPublisher port, and drives real
// WebSocket clients through each pod's real upgrade + hub + snapshot
// paths. No real network transport is needed for the correctness core;
// the gRPC adapter is a seam behind the same port, gated separately by
// multipod_grpc_seam_test.go over the real client/server transport.
//
// The three observable acceptance criteria pinned here:
//
//   1. Two clients on DIFFERENT pods receive every put/delete mirror,
//      no matter which pod's HTTP surface handled the durable write.
//   2. A late join during concurrent cross-pod writes has no gap and no
//      dup: the snapshot's exact seq S plus the client splice contract
//      (discard <= S, buffer out-of-order, apply dense runs) reconstructs
//      exactly the durable state.
//   3. A killed pod's clients reconnect to a surviving pod and resync
//      via snapshot + splice, again with no gap and no dup.
//
// Plus the REPRO pin: with NO peer publisher wired (the zero-peer relay,
// byte-for-byte the pre-multi-pod behavior), live mirrors are POD-LOCAL
// and cross-pod subscribers observe silence - the measured (N-1)/N live
// update loss this design exists to fix. That test documents the failure
// shape permanently; the fan-out test above is the one that goes RED if
// the peer wiring is dropped.
//
// WEAKEN DEMO (run, observed, restored - the fan-out gate fails closed):
// in relay/relay.go, delete the `rl.publishToPeers(key, mirror)` line from
// CommitAndMirror. Demonstrated RED: TestMultiPod_BroadcastReachesAllPods
// fails with "clientB: splice stalled at seq 0 waiting for 10" (the
// cross-pod clients never receive another pod's mirrors - exactly the
// pre-branch pod-local relay). Deleting the readLoop's publishToPeers
// instead fails the same test's ephemeral leg ("clientB: expected
// cross-pod ephemeral frame"). Restoring both is green.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	nethttp "net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
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

// multiPod is N pods over one shared sqlite store, bridged by memBridge.
type multiPod struct {
	t    *testing.T
	pods []*pod

	mu    sync.RWMutex
	alive []bool // bridge fan-out skips dead pods (a killed pod's transport is gone)
}

// memBridge is the in-memory PeerPublisher for ONE pod: Publish delivers
// the frame synchronously to every OTHER live pod's DeliverFromPeer. It is
// the test double for the gRPC transport - same port, no network. Delivery
// is synchronous (strongest-ordering transport); the cross-pod races the
// splice contract exists for still occur naturally, because two pods'
// commit->publish sections interleave freely.
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

// buildMultiPod stands up n pods over one fresh sqlite store. bridged
// selects the peer wiring: true wires the memBridge into every pod's
// relay (the multi-pod deploy), false leaves every relay with a nil
// publisher (the zero-peer relay - byte-for-byte the single-pod behavior,
// which on a multi-pod deploy IS the repro'd bug).
func buildMultiPod(t *testing.T, n int, bridged bool) *multiPod {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "multipod.db"))
	if err != nil {
		t.Fatalf("open shared store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := storage.NewRoomKVRepo(db)

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

// kill takes pod i down: its relay closes every live connection (the
// sockets die with the pod), the bridge stops delivering to it, and its
// HTTP surface stops. Idempotent.
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

// put commits a durable PUT through pod i's real HTTP handler stack (the
// "a PUT handled by a third pod" path). Fails the test on a non-204.
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

// scanTruth reads the durable ground truth (state + exact seq) directly
// from the shared storage through the service layer.
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
//     FAILS (production resyncs by reconnect; the harness's transport is
//     lossless so a stall is a bug, not weather)
//
// It records every applied seq (for exactly-once assertions) and every
// discarded frame (dup deliveries are LEGAL on the wire; applying one is
// not).

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

// newSpliceClient dials a pod's HTTP surface and consumes the join
// snapshot (always the first frame), initializing lastSeq to its exact
// seq S. It takes the httptest server directly so both the in-memory
// bridge fixture and the real-gRPC seam test share it.
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
		// Frames buffered from before the snapshot (possible only for a
		// snapshot that is not the connection's first frame; kept for
		// contract completeness) splice onto the new base.
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
// target, failing the test if the stream stalls for more than the quiet
// window (a stalled splice on a lossless transport = a lost or missing
// frame = the bug this harness gates).
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
// ground truth, value-encoded exactly as the wire encodes them.
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
// THE REPRO: the zero-peer relay on a multi-pod deploy is pod-local. Two
// clients whose sockets live on pods 0 and 1; a durable PUT handled by pod
// 2 reaches NEITHER, and a PUT handled by pod 0 reaches only pod 0's
// client. This is byte-for-byte the pre-multi-pod relay (no publisher
// wired is exactly what main's relay does on every pod) and pins the
// measured (N-1)/N live-mirror loss that motivated the design. The durable
// KV stays correct throughout - only the LIVE delta splits.
//
// This test PASSES against the pre-branch relay too - it characterizes the
// bug. The gate that FAILS without the fix is
// TestMultiPod_BroadcastReachesAllPods below (see the WEAKEN DEMO at the
// top of the file: deleting the publishToPeers wiring reproduces the
// pre-branch behavior and that test goes red).
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

	// A PUT handled by pod 2 (no local subscribers): the durable write
	// succeeds and is mirrored only into pod 2's own (empty) hub. Both
	// remote clients observe SILENCE - the live update is lost.
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
// ACCEPTANCE 1: two clients on different pods receive EVERY put/delete
// mirror - durable writes routed through a third pod, through each
// client's own pod, and a delete of an absent key (which still commits and
// still assigns a seq) all included. Ordering, de-duplication, and
// completeness ride the per-room seq; the splice client asserts the dense
// range applied exactly once and the final state byte-equals the durable
// truth. Ephemeral (payload-opaque) frames cross pods on the same path.
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

	// Ten durable mutations, deliberately spread across every pod's HTTP
	// surface (any pod can commit; ordering is a property of the data, not
	// the topology) and covering both verbs plus the absent-key delete.
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

	// Ephemeral cross-pod fan-out rides the same path: a raw peer frame
	// from A (pod 0) reaches B (pod 1) verbatim, and vice versa.
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
// ACCEPTANCE 2: late join during concurrent cross-pod writes - no gap, no
// dup, and the splice at the snapshot's exact seq S holds. Two writers
// hammer the same room through DIFFERENT pods (their commit->fanout
// sections interleave freely, so the joiner's stream races both pods'
// mirrors and the snapshot read); a client then joins mid-stream and must
// reconstruct exactly the durable state: nothing at or below its snapshot
// S applied, the dense range (S, total] applied exactly once, spliced
// state byte-equal to the shared store. Run under -count=20 to sample the
// interleavings (the phase-2 gate did; see the workflow notes).
// ---------------------------------------------------------------------------

func TestMultiPod_LateJoinDuringConcurrentCrossPodWrites(t *testing.T) {
	h := buildMultiPod(t, 3, true)
	const slug = "appz2345"
	id := mkRoom(t, h.pods[0].rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two concurrent writers on different pods. Each PUTs its own keys and
	// deletes a few of them again, so the stream carries both verbs. 22
	// mutations each, 44 in the concurrent phase.
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

	// Join mid-stream: wait until at least a third of the writes committed
	// (the durable seq is the ground truth for "mid"), then connect through
	// pod 2 - a pod NEITHER writer routes through, so every mirror the
	// joiner sees crossed pods.
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
// ACCEPTANCE 3: a relay pod dies mid-stream. Its client's socket dies with
// it (accepted; WebSockets die with their pod), the client reconnects to a
// SURVIVING pod through the normal join, and the snapshot + splice resync
// it: the fresh snapshot's S2 covers everything missed during the outage,
// the discard rule drops any frame at or below S2, and the stream from S2
// on is dense and exactly-once. Writes never stop flowing during the kill
// (they route through surviving pods; the durable tier is unaffected).
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

	// Reconnect to a SURVIVING pod: the normal join, a fresh snapshot, a
	// fresh splice base. S2 can only be at or past everything epoch 1
	// applied (the durable seq never regresses).
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
