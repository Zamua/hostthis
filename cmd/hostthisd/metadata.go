// The metadata plane: the celld adapters at HOSTTHIS_CELLD_ENDPOINT, for
// example http://celld:8080 (docs/SPEC.md "Metadata storage backends").

package main

import (
	"errors"
	"fmt"
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
	rt, err := celldTransport(base, http.DefaultTransport)
	if err != nil {
		return nil, err
	}
	// One client across every adapter, so connections to the fleet are pooled
	// rather than each surface opening its own.
	client := &http.Client{Timeout: 30 * time.Second, Transport: rt}
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
		RoomRelay: celld.NewRoomProxy(base, client),
	}, nil
}

// celldTransport is rt sending HOSTTHIS_CELLD_TOKEN, when set, to the Worker
// at base. Every client that calls the Worker is built over it.
func celldTransport(base string, rt http.RoundTripper) (http.RoundTripper, error) {
	authed, err := celld.Authorize(rt, base, envOr("HOSTTHIS_CELLD_TOKEN", ""))
	if err != nil {
		return nil, fmt.Errorf("HOSTTHIS_CELLD_TOKEN: %w", err)
	}
	return authed, nil
}
