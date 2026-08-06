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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
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

// roomStore is the union of every room-side interface the service / sweep
// layers consume: the room write/read view (service.RoomRepo) and the sweep
// view (service.SweepRooms).
type roomStore interface {
	service.RoomRepo
	service.SweepRooms
}

// buildMetadata reads HOSTTHIS_METADATA_BACKEND and returns the configured
// bundle, defaulting to sqlite. The slatedb and shale branches error with a
// clear message when the binary was built without `-tags slatedb`.
func buildMetadata(dataDir string, logger *log.Logger) (*metadataBundle, error) {
	backend := strings.ToLower(envOr("HOSTTHIS_METADATA_BACKEND", "sqlite"))
	switch backend {
	case "sqlite":
		return buildMetadataSqlite(dataDir, logger)
	case "slatedb":
		return buildMetadataSlate(logger)
	case "shale":
		return buildMetadataShale(logger)
	default:
		return nil, fmt.Errorf("unknown HOSTTHIS_METADATA_BACKEND %q (want sqlite|slatedb|shale)", backend)
	}
}

// buildMetadataSqlite is the always-compiled default, opening
// <data-dir>/hostthis.db.
func buildMetadataSqlite(dataDir string, logger *log.Logger) (*metadataBundle, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir data-dir: %w", err)
	}
	db, err := storage.Open(filepath.Join(dataDir, "hostthis.db"))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	logger.Printf("metadata: sqlite at %s", filepath.Join(dataDir, "hostthis.db"))
	pasteRepo := storage.NewPasteRepo(db)
	return &metadataBundle{
		Repo:    pasteRepo,
		KeyGate: storage.NewKeyGateRepo(db),
		Sites:   storage.NewSiteRepo(db),
		Rooms:   storage.NewRoomKVRepo(db),
		Close:   db.Close,
	}, nil
}
