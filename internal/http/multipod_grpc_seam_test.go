package http

// The real-gRPC seam test the spec's acceptance criteria call for
// ("plus a real-gRPC seam test for the transport adapter"): two full
// in-process pods - each a Server + relay.Relay over ONE shared sqlite
// store - bridged by the REAL relaygrpc client/server transport over
// loopback listeners, NOT the in-process memBridge. It proves the
// adapter end to end in the production shape:
//
//   - each pod runs a real grpc.Server with the relaygrpc.Receiver
//     registered (the same Register hook the shale storage config invokes
//     in multi-node mode) and Bind'd to its relay's DeliverFromPeer;
//   - each pod's relay publishes through a real relaygrpc.Publisher whose
//     Peers port names the OTHER pod's listener address (a static stand-in
//     for the ring-membership adapter);
//   - a durable PUT/DELETE through either pod's HTTP surface reaches the
//     other pod's WebSocket client as a seq'd mirror over the wire, the
//     splice holds exactly-once, and an ephemeral frame crosses on the
//     same path.
//
// The correctness CORE (ordering, splice, pod-kill) is gated by the
// in-memory-bridge harness in multipod_harness_test.go; this file gates
// the TRANSPORT: proto round-trip, client/server plumbing, queue + sender
// delivery, receive-side broadcast.

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/relay/relaygrpc"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// grpcPod is one pod of the seam fixture: the HTTP surface + relay plus
// its real gRPC receiver listener.
type grpcPod struct {
	rooms *service.Rooms
	relay *relay.Relay
	ts    *httptest.Server
	addr  string // the pod's RoomRelay listener address (what peers dial)
	pub   *relaygrpc.Publisher
}

// fixedPeers is the static Peers stand-in for the membership adapter.
type fixedPeers []string

func (p fixedPeers) Addresses() []string { return p }

// buildGRPCPair stands up two pods over one shared sqlite store, bridged
// by the real gRPC transport in both directions.
func buildGRPCPair(t *testing.T) [2]*grpcPod {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(dir + "/seam.db")
	if err != nil {
		t.Fatalf("open shared store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := storage.NewRoomKVRepo(db)

	var pods [2]*grpcPod
	for i := range pods {
		rooms := service.NewRooms(repo)
		rl := relay.NewRelay(rooms, relay.NewLimits())
		srv := &Server{
			ApexDomain: wsTestApex,
			Rooms:      rooms,
			Relay:      rl,
			Sites:      liveAppSiteReader{},
		}
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		// The pod's REAL peer-facing gRPC server: receiver registered via
		// the same hook the storage config calls, delivery late-bound to
		// this pod's relay (the composition-root order main uses).
		lis, lerr := net.Listen("tcp", "127.0.0.1:0")
		if lerr != nil {
			t.Fatalf("pod%d listen: %v", i, lerr)
		}
		g := grpc.NewServer()
		recv := relaygrpc.NewReceiver(relay.MaxDurableFrameBytes(domain.MaxRoomValueBytes))
		recv.Register(g)
		go func() { _ = g.Serve(lis) }()
		t.Cleanup(g.Stop)
		recv.Bind(rl.DeliverFromPeer)

		pods[i] = &grpcPod{rooms: rooms, relay: rl, ts: ts, addr: lis.Addr().String()}
	}

	// Cross-wire the publishers: each pod's peer set is the OTHER pod.
	for i := range pods {
		other := pods[1-i].addr
		pub := relaygrpc.NewPublisher(fixedPeers{other}, relaygrpc.PublisherConfig{})
		pods[i].pub = pub
		pods[i].relay.SetPeerPublisher(pub)
		t.Cleanup(pub.Close)
	}
	return pods
}

func TestMultiPod_RealGRPCSeam_CrossPodDelivery(t *testing.T) {
	pods := buildGRPCPair(t)
	const slug = "appz2345"
	id := mkRoom(t, pods[0].rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	clientA := newSpliceClient(t, ctx, pods[0].ts, "clientA", slug, id)
	defer clientA.close()
	clientB := newSpliceClient(t, ctx, pods[1].ts, "clientB", slug, id)
	defer clientB.close()

	// Durable mutations through BOTH pods' HTTP surfaces: every mirror must
	// cross the real wire to the other pod's client, splice exactly-once.
	httpPut(t, pods[0].ts, slug, id, "board/1", []byte(`{"cell":"x"}`)) // seq 1, via A's pod
	httpPut(t, pods[1].ts, slug, id, "board/2", []byte(`{"cell":"o"}`)) // seq 2, via B's pod
	httpDelete(t, pods[0].ts, slug, id, "board/2")                      // seq 3
	httpPut(t, pods[1].ts, slug, id, "title", []byte(`"seam"`))         // seq 4

	const total = 4
	clientA.awaitSeq(ctx, total, 5*time.Second)
	clientB.awaitSeq(ctx, total, 5*time.Second)

	truth, err := pods[0].rooms.Scan(domainSlug(slug), domain.RoomID(id))
	if err != nil {
		t.Fatalf("scan ground truth: %v", err)
	}
	if truth.Seq != total {
		t.Fatalf("ground truth seq = %d, want %d", truth.Seq, total)
	}
	for _, sc := range []*spliceClient{clientA, clientB} {
		sc.assertExactlyOnceSince(0, total)
		sc.assertStateEquals(truth)
	}

	// Ephemeral frames cross the same real transport, verbatim, both ways.
	clientA.c.send(ctx, "ephemeral-A-over-grpc")
	clientB.c.expectFrame(ctx, 5*time.Second, "cross-pod ephemeral frame over real gRPC", func(b []byte) bool {
		return string(b) == "ephemeral-A-over-grpc"
	})
	clientB.c.send(ctx, "ephemeral-B-over-grpc")
	clientA.c.expectFrame(ctx, 5*time.Second, "cross-pod ephemeral frame over real gRPC", func(b []byte) bool {
		return string(b) == "ephemeral-B-over-grpc"
	})

	// Nothing was silently dropped on the way: the transport is healthy in
	// this fixture, so a nonzero drop count means the adapter shed frames
	// it had every opportunity to deliver.
	for i, p := range pods {
		if d := p.pub.Drops(); d != 0 {
			t.Fatalf("pod%d publisher dropped %d frames on a healthy loopback transport", i, d)
		}
	}
}

// TestMultiPod_RealGRPCSeam_LargeDurableMirrorCrossesPods pins the peer
// receiver's frame cap to the durable-mirror class (SPEC "Trust boundary"):
// a legal room value is far larger than the client-socket frame cap, and a
// non-JSON value inflates further under JSON-string escaping. With the
// receiver capped at the client-socket size, this mirror is refused on
// arrival and pod B's client silently never sees the write (the exact
// defect a delta review proved live); sized to MaxDurableFrameBytes it
// crosses. Red observed with relay.DefaultMaxMessageBytes in the fixture.
func TestMultiPod_RealGRPCSeam_LargeDurableMirrorCrossesPods(t *testing.T) {
	pods := buildGRPCPair(t)
	const slug = "appz2345"
	id := mkRoom(t, pods[0].rooms, slug)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	clientB := newSpliceClient(t, ctx, pods[1].ts, "clientB", slug, id)
	defer clientB.close()

	// 64 KiB of non-JSON bytes with control characters: over the
	// client-socket cap on its own, and escaping-inflated in the envelope.
	big := make([]byte, 64<<10)
	for i := range big {
		big[i] = byte(i % 32) // control range: worst-case \u00XX escaping
	}
	httpPut(t, pods[0].ts, slug, id, "blob/1", big) // seq 1, via pod A

	clientB.awaitSeq(ctx, 1, 5*time.Second)
	if clientB.applied[1] == 0 {
		t.Fatalf("pod B's client never received the large durable mirror: receiver cap severed the cross-pod frame")
	}
	if _, ok := clientB.state["blob/1"]; !ok {
		t.Fatalf("pod B's client applied seq 1 but blob/1 missing from state")
	}
}
