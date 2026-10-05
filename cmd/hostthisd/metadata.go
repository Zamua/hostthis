// The metadata plane: the celld adapters at HOSTTHIS_CELLD_ENDPOINT, for
// example http://celld:8080 (docs/SPEC.md "Metadata storage backends").

package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// metadataBundle is everything the rest of the binary needs from the metadata
// plane.
type metadataBundle struct {
	Repo    metadataRepo
	KeyGate service.KeyGateRepo
	Sites   siteStore
	Rooms   roomStore
	// RoomRelay is the cell-backed real-time transport.
	RoomRelay httpapi.RoomRelay
}

// metadataRepo is the union of every service-layer / http-layer interface the
// metadata plane has to satisfy: a wiring-layer concern.
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

// buildMetadata wires the celld adapters. apex names the deployment in the
// VAPID tokens the room cells sign.
func buildMetadata(apex string, logger *log.Logger) (*metadataBundle, error) {
	base := envOr("HOSTTHIS_CELLD_ENDPOINT", "")
	if base == "" {
		return nil, errors.New("HOSTTHIS_CELLD_ENDPOINT is required")
	}
	// One client across every adapter, so connections to the fleet are pooled
	// rather than each surface opening its own.
	client := &http.Client{Timeout: 30 * time.Second}
	repo := celld.NewPasteRepo(base, client)
	repo.Logger = logger
	rooms := celld.NewRoomRepo(base, client)
	rooms.PushSubject = "https://" + apex

	logger.Printf("metadata: celld at %s", base)
	return &metadataBundle{
		Repo:    repo,
		KeyGate: celld.NewKeyGateRepo(base, client),
		Sites:   storage.NewSites(repo),
		Rooms:   rooms,
		// Every socket terminates at its Room cell, which owns broadcast order.
		RoomRelay: celld.NewRoomProxy(base),
	}, nil
}
