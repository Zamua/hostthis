// metadata_celld.go - celld-backed metadataBundle.
//
// HOSTTHIS_CELLD_ENDPOINT is required, for example http://celld:8080.

package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/storage"
)

func buildMetadataCelld(apex string, logger *log.Logger) (*metadataBundle, error) {
	base := envOr("HOSTTHIS_CELLD_ENDPOINT", "")
	if base == "" {
		return nil, errors.New("HOSTTHIS_CELLD_ENDPOINT is required for the celld backend")
	}
	// One client across every adapter, so connections to the fleet are pooled
	// rather than each surface opening its own.
	client := &http.Client{Timeout: 30 * time.Second}
	repo := celld.NewPasteRepo(base, client)
	rooms := celld.NewRoomRepo(base, client)
	rooms.PushSubject = "https://" + apex

	logger.Printf("metadata backend: celld at %s", base)
	return &metadataBundle{
		Repo:    repo,
		KeyGate: celld.NewKeyGateRepo(base, client),
		Sites:   storage.NewSites(repo),
		Rooms:   rooms,
		// Every socket terminates at its Room cell, which owns broadcast order.
		RoomRelay: celld.NewRoomProxy(base),
	}, nil
}
