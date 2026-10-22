// The metadata plane: the worker adapters at HOSTTHIS_WORKER_ENDPOINT, for
// example http://worker:8787 (docs/SPEC.md "Metadata storage backends").

package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/worker"
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
	// Solid forwards the apex /_solid/ paths to the Worker.
	Solid http.Handler
}

// metadataRepo is the union of every service-layer / http-layer interface the
// metadata plane has to satisfy: a wiring-layer concern.
type metadataRepo interface {
	service.PasteRepo
	service.PodLinkRepo
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

// buildMetadata wires the worker adapters. apex names the deployment in the
// VAPID tokens the room cells sign.
func buildMetadata(apex string, logger *log.Logger) (*metadataBundle, error) {
	base := envOr("HOSTTHIS_WORKER_ENDPOINT", "")
	if base == "" {
		return nil, errors.New("HOSTTHIS_WORKER_ENDPOINT is required")
	}
	rt, err := workerTransport(base, http.DefaultTransport)
	if err != nil {
		return nil, err
	}
	// One client across every adapter, so connections to the fleet are pooled
	// rather than each surface opening its own.
	client := &http.Client{Timeout: 30 * time.Second, Transport: rt}
	repo := worker.NewPasteRepo(base, client)
	solid, err := worker.NewSolidProxy(base, apex, rt)
	if err != nil {
		return nil, err
	}
	repo.Logger = logger
	rooms := worker.NewRoomRepo(base, client)
	rooms.PushSubject = "https://" + apex

	logger.Printf("metadata: worker at %s", base)
	return &metadataBundle{
		Repo:    repo,
		KeyGate: worker.NewKeyGateRepo(base, client),
		Sites:   storage.NewSites(repo),
		Rooms:   rooms,
		// Every socket terminates at its Room cell, which owns broadcast order.
		RoomRelay: worker.NewRoomProxy(base, client),
		Solid:     solid,
	}, nil
}

// workerTransport is rt sending HOSTTHIS_WORKER_TOKEN, when set, to the Worker
// at base. Every client that calls the Worker is built over it.
func workerTransport(base string, rt http.RoundTripper) (http.RoundTripper, error) {
	authed, err := worker.Authorize(rt, base, envOr("HOSTTHIS_WORKER_TOKEN", ""))
	if err != nil {
		return nil, fmt.Errorf("HOSTTHIS_WORKER_TOKEN: %w", err)
	}
	return authed, nil
}
