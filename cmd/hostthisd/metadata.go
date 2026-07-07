// metadata.go - pluggable metadata backend selector.
//
// Picks between the sqlite-backed PasteRepo (default, no build tag
// required), the SlateDB-backed SlateRepo (opt-in via the `slatedb`
// build tag + HOSTTHIS_METADATA_BACKEND=slatedb), and the shale-cluster
// ShaleRepo (same `slatedb` build tag + HOSTTHIS_METADATA_BACKEND=shale).
// See docs/SPEC.md "Metadata storage backends" for the model.

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// relayPeerTransport is the multi-pod relay peer transport a metadata
// backend supplies when it runs a peer-reachable gRPC server (multi-node
// shale, where the relay's peer service registers on the cluster server
// and peer discovery reads the gossiped ring membership - docs/SPEC.md
// "Multi-pod relay: the peer transport"). Every single-pod backend leaves
// the bundle field nil, which keeps the relay's nil publisher: the
// zero-peer degenerate case, byte-for-byte the single-pod relay. The
// fields are relay-port types only, so the untagged build stays free of
// the transport's gRPC dependency (the adapter lives behind the slatedb
// build tag).
type relayPeerTransport struct {
	// Publisher is the outbound per-peer fan-out main wires into the relay
	// via SetPeerPublisher.
	Publisher relay.PeerPublisher
	// Bind late-binds the receive path's local-delivery hook to the relay
	// (its DeliverFromPeer) once the relay exists - the relay is
	// constructed after the repo, so the receiver starts unbound and drops
	// early frames (a boot race, correct by design).
	Bind func(func(relay.RoomKey, relay.Frame))
	// Close stops the publisher's sender goroutines and closes its client
	// connections; main calls it during shutdown, after CloseAll.
	Close func()
}

// metadataBundle is everything the rest of the binary needs from a
// metadata backend. We hold the concrete repo + keygate as the union
// of all the interfaces the service / http layers consume; both the
// sqlite and slatedb implementations satisfy them implicitly.
type metadataBundle struct {
	Repo    metadataRepo
	KeyGate service.KeyGateRepo
	// Sites is the static-site repo. The sqlite, slatedb, and shale
	// backends each supply an implementation; a backend that leaves it nil
	// disables static-site archive uploads there. nil-safe throughout. The
	// field is the siteStore interface (not the concrete sqlite
	// *storage.SiteRepo) so any backend's site impl can be assigned.
	Sites siteStore
	// Rooms is the room-KV repo (the no-auth app-persistence tier). The
	// sqlite, slatedb, and shale backends each supply an implementation; a
	// backend that leaves it nil disables the /api/rooms surface there.
	// nil-safe throughout. The field is the roomStore interface (not the
	// concrete sqlite *storage.RoomKVRepo) so any backend's room impl can be
	// assigned - mirroring how Sites was widened to siteStore.
	Rooms roomStore
	// BlobUnit is an OPTIONAL backend-supplied blob seam. The shale backend
	// (with a blob store configured) supplies the transactional shaleblob.Unit,
	// which routes blobs through the cluster and co-commits the pointer with the
	// metadata. When nil (sqlite, slatedb, shale-without-a-blob-store), main
	// falls back to the standalone detached-store blob unit, so the blob plane
	// stays decoupled from the metadata plane on those paths. See docs/SPEC.md
	// "Shale-collocated blobs".
	BlobUnit service.BlobUnit
	// BlobOrphanSweeper is an OPTIONAL backend-supplied orphan-bytes reclaimer.
	// On the shale-blob path it is SweepOrphans (staged-but-unbound objects,
	// age-gated, mounted-unit-local); main schedules it in the sweep loop. nil
	// on every other path (the global content-addressed sweep is the GC there).
	BlobOrphanSweeper service.BlobOrphanSweeper
	// RelayPeer is the OPTIONAL multi-pod relay peer transport (multi-node
	// shale only; see the relayPeerTransport doc). nil keeps the relay
	// pod-local (the zero-peer degenerate case).
	RelayPeer *relayPeerTransport
	Close     func() error
}

// metadataRepo is the union of every service-layer / http-layer
// interface the metadata backend has to satisfy. Both PasteRepo and
// SlateRepo implicitly satisfy it. Defined here (not in
// internal/service) because it's a wiring-layer concern.
type metadataRepo interface {
	service.PasteRepo
	service.PasteAdmin
	service.SweepRepo
	httpapi.PasteReader
}

// siteStore is the union of every site-side interface the service / http
// layers consume: the deploy view (service.SiteRepo), the sweep view
// (service.SweepSites), and the read view (httpapi.SiteReader). The sqlite
// *storage.SiteRepo and the slatedb *storage.SlateSiteRepo both satisfy it.
// Holding the bundle field as this interface (rather than the concrete
// sqlite type) is what lets the slatedb / shale backends supply their own
// site impl. nil disables static-site hosting for that backend.
type siteStore interface {
	service.SiteRepo
	service.SweepSites
	httpapi.SiteReader
}

// roomStore is the union of every room-side interface the service / sweep
// layers consume: the room write/read view (service.RoomRepo) and the sweep
// view (service.SweepRooms). The sqlite *storage.RoomKVRepo, the slatedb
// *storage.SlateRoomRepo, and the shale *storage.ShaleRoomRepo all satisfy
// it. Holding the bundle field as this interface (rather than the concrete
// sqlite type) is what lets the slatedb / shale backends supply their own
// room impl, so the /api/rooms surface lights up on the slatedb-direct prod
// deploy. nil disables the room-persistence tier for that backend.
type roomStore interface {
	service.RoomRepo
	service.SweepRooms
}

// buildMetadata reads HOSTTHIS_METADATA_BACKEND and returns the
// configured bundle. Default is sqlite (no behavior change vs prior).
// "slatedb" requires the binary to be built with `-tags slatedb`;
// otherwise the slatedb branch errors with a clear message.
func buildMetadata(dataDir string, retention domain.Retention, logger *log.Logger) (*metadataBundle, error) {
	backend := strings.ToLower(envOr("HOSTTHIS_METADATA_BACKEND", "sqlite"))
	switch backend {
	case "sqlite":
		return buildMetadataSqlite(dataDir, retention, logger)
	case "slatedb":
		return buildMetadataSlate(retention, logger)
	case "shale":
		return buildMetadataShale(retention, logger)
	default:
		return nil, fmt.Errorf("unknown HOSTTHIS_METADATA_BACKEND %q (want sqlite|slatedb|shale)", backend)
	}
}

// buildMetadataSqlite is the always-compiled default. Opens
// <data-dir>/hostthis.db and wires PasteRepo + KeyGateRepo.
func buildMetadataSqlite(dataDir string, retention domain.Retention, logger *log.Logger) (*metadataBundle, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir data-dir: %w", err)
	}
	db, err := storage.Open(filepath.Join(dataDir, "hostthis.db"))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	logger.Printf("metadata: sqlite at %s", filepath.Join(dataDir, "hostthis.db"))
	pasteRepo := storage.NewPasteRepo(db)
	pasteRepo.Retention = retention
	return &metadataBundle{
		Repo:    pasteRepo,
		KeyGate: storage.NewKeyGateRepo(db),
		Sites:   storage.NewSiteRepo(db),
		Rooms:   storage.NewRoomKVRepo(db),
		Close:   db.Close,
	}, nil
}
