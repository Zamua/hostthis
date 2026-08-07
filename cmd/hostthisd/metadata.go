// metadata.go - pluggable metadata backend selector.
//
// HOSTTHIS_METADATA_BACKEND picks one of:
//
//	sqlite   PasteRepo, the default, no build tag required
//	slatedb  SlateRepo, requires -tags slatedb
//	shale    ShaleRepo, requires -tags slatedb
//
// See docs/SPEC.md "Metadata storage backends".

package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
)

// relayPeerTransport is the multi-pod relay peer transport a metadata backend
// supplies when it runs a peer-reachable gRPC server (multi-node shale). A
// single-pod backend leaves the bundle field nil, which keeps the relay's nil
// publisher: the zero-peer degenerate case, byte-for-byte the single-pod
// relay. The fields are relay-port types only, so the untagged build stays
// free of the transport's gRPC dependency. See docs/SPEC.md "Multi-pod relay:
// the peer transport".
type relayPeerTransport struct {
	// Publisher is the outbound per-peer fan-out main wires into the relay
	// via SetPeerPublisher.
	Publisher relay.PeerPublisher
	// Bind late-binds the receive path's local-delivery hook (the relay's
	// DeliverFromPeer) once the relay exists. The relay is constructed after
	// the repo, so the receiver starts unbound and drops early frames: a boot
	// race, correct by design.
	Bind func(func(relay.RoomKey, relay.Frame))
	// Close stops the publisher's sender goroutines and closes its client
	// connections; main calls it during shutdown, after CloseAll.
	Close func()
}

// metadataBundle is everything the rest of the binary needs from a metadata
// backend.
type metadataBundle struct {
	Repo    metadataRepo
	KeyGate service.KeyGateRepo
	// Sites is the static-site repo. nil disables static-site archive uploads
	// for that backend, and is nil-safe throughout. Held as the siteStore
	// interface so any backend's impl can be assigned.
	Sites siteStore
	// Rooms is the room-KV repo (the no-auth app-persistence tier). nil
	// disables the /api/rooms surface for that backend, and is nil-safe
	// throughout. Held as the roomStore interface so any backend's impl can be
	// assigned.
	Rooms roomStore
	// BlobUnit is an OPTIONAL backend-supplied blob seam. The shale backend
	// with a blob store configured supplies the transactional shaleblob.Unit,
	// which co-commits the blob pointer with the metadata. nil falls back to
	// the standalone detached-store blob unit, keeping the blob plane
	// decoupled from the metadata plane. See docs/SPEC.md "Shale-collocated
	// blobs".
	BlobUnit service.BlobUnit
	// BlobOrphanSweeper is an OPTIONAL backend-supplied orphan-bytes
	// reclaimer. On the shale-blob path it is SweepOrphans (staged-but-unbound
	// objects, age-gated, mounted-unit-local); main schedules it in the sweep
	// loop. nil elsewhere, where the global content-addressed sweep is the GC.
	BlobOrphanSweeper service.BlobOrphanSweeper
	// IntentSweeper is an OPTIONAL backend-supplied boot sweep that settles
	// durable intents left by a process death mid-write. Supplied only by a
	// backend whose writes span shards; a single-transaction backend has no
	// half-finished state to settle. Run ONCE after the server is listening -
	// never before, because settling an intent reads a shard that may not be
	// mounted anywhere during a cold start (docs/SPEC.md "Durable intent").
	IntentSweeper interface {
		SweepIntents(ctx context.Context, now time.Time) (int, error)
	}
	// RelayPeer is the OPTIONAL multi-pod relay peer transport (multi-node
	// shale only). nil keeps the relay pod-local.
	RelayPeer *relayPeerTransport
	// Readiness is the OPTIONAL readiness predicate behind /readyz
	// (docs/SPEC.md "Readiness vs liveness"). The shale backend supplies its
	// mount-floor prober; a backend with no mount concept leaves it nil, which
	// the http server reads as always-ready.
	Readiness httpapi.ReadinessProber
	Close     func() error
}

// metadataRepo is the union of every service-layer / http-layer interface the
// metadata backend has to satisfy. Defined here rather than in
// internal/service because it is a wiring-layer concern.
type metadataRepo interface {
	service.PasteRepo
	service.PasteAdmin
	httpapi.PasteReader
}

// siteStore is the union of every site-side interface the service / http
// layers consume: the deploy view (service.SiteRepo) and the read view
// (httpapi.SiteReader).
type siteStore interface {
	service.SiteRepo
	httpapi.SiteReader
}

// roomStore is the room write/read view the service layer consumes.
type roomStore interface {
	service.RoomRepo
}

// buildMetadata reads HOSTTHIS_METADATA_BACKEND and returns the configured
// bundle, defaulting to the local engine. Every branch errors with a clear
// message when this build cannot serve it: `local` needs a build WITHOUT
// -tags slatedb, `slatedb` and `shale` need one WITH it.
func buildMetadata(dataDir string, logger *log.Logger) (*metadataBundle, error) {
	backend := strings.ToLower(envOr("HOSTTHIS_METADATA_BACKEND", "local"))
	switch backend {
	case "local":
		return buildMetadataLocal(dataDir, logger)
	case "slatedb":
		return buildMetadataSlate(logger)
	case "shale":
		return buildMetadataShale(logger)
	default:
		return nil, fmt.Errorf("unknown HOSTTHIS_METADATA_BACKEND %q (want local|slatedb|shale)", backend)
	}
}
