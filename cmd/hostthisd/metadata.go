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

	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

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
	// Rooms is the room-KV repo (the no-auth app-persistence tier). Only
	// the sqlite backend supplies it today; the slatedb / shale backends
	// leave it nil and the /api/rooms surface is disabled there. The spec
	// documents the shale shard-family shape for a future shale-backed
	// implementation. nil-safe throughout.
	Rooms *storage.RoomKVRepo
	Close func() error
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

// buildMetadata reads HOSTTHIS_METADATA_BACKEND and returns the
// configured bundle. Default is sqlite (no behavior change vs prior).
// "slatedb" requires the binary to be built with `-tags slatedb`;
// otherwise the slatedb branch errors with a clear message.
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

// buildMetadataSqlite is the always-compiled default. Opens
// <data-dir>/hostthis.db and wires PasteRepo + KeyGateRepo.
func buildMetadataSqlite(dataDir string, logger *log.Logger) (*metadataBundle, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir data-dir: %w", err)
	}
	db, err := storage.Open(filepath.Join(dataDir, "hostthis.db"))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	logger.Printf("metadata: sqlite at %s", filepath.Join(dataDir, "hostthis.db"))
	return &metadataBundle{
		Repo:    storage.NewPasteRepo(db),
		KeyGate: storage.NewKeyGateRepo(db),
		Sites:   storage.NewSiteRepo(db),
		Rooms:   storage.NewRoomKVRepo(db),
		Close:   db.Close,
	}, nil
}
