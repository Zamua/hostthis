// metadata.go - pluggable metadata backend selector.
//
// HOSTTHIS_METADATA_BACKEND selects memory or celld. See docs/SPEC.md
// "Metadata storage backends".

package main

import (
	"fmt"
	"log"
	"strings"

	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
)

// metadataBundle is everything the rest of the binary needs from a metadata
// backend.
type metadataBundle struct {
	Repo    metadataRepo
	KeyGate service.KeyGateRepo
	Sites   siteStore
	Rooms   roomStore
	// RoomRelay is the cell-backed real-time transport. nil disables /ws.
	RoomRelay httpapi.RoomRelay
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

// roomStore is the room write/read view the service layer consumes, the push
// surface included.
type roomStore interface {
	service.RoomRepo
	service.RoomPushRepo
}

// buildMetadata reads HOSTTHIS_METADATA_BACKEND and defaults to memory. apex
// names the deployment in VAPID tokens the celld backend signs.
func buildMetadata(dataDir, apex string, logger *log.Logger) (*metadataBundle, error) {
	backend := strings.ToLower(envOr("HOSTTHIS_METADATA_BACKEND", "memory"))
	switch backend {
	case "celld":
		return buildMetadataCelld(apex, logger)
	case "memory":
		return buildMetadataMemory(dataDir, logger)
	default:
		return nil, fmt.Errorf("unknown HOSTTHIS_METADATA_BACKEND %q (want celld|memory)", backend)
	}
}
