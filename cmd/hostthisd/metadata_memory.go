// The in-memory metadata bundle: MemRepo behind every port. Zero
// configuration, which is what `make run` and the e2e suite want.
//
// EPHEMERAL: metadata lives for the process. Blobs still go to the configured
// blob backend, so a restart leaves orphans the sweep reclaims. The startup
// line says so.

package main

import (
	"log"

	"github.com/Zamua/hostthis/internal/storage"
)

func buildMetadataMemory(_ string, logger *log.Logger) (*metadataBundle, error) {
	repo := storage.NewMemRepo()
	logger.Printf("metadata backend: memory (EPHEMERAL - metadata lives for the process; dev/test only)")
	return &metadataBundle{
		Repo:    repo,
		KeyGate: repo,
		Sites:   storage.NewSites(repo),
		Rooms:   storage.NewMemRoomRepo(repo),
		// RoomRelay nil: the realtime layer is the room cell's, and the memory
		// backend has no cell to fan out from.
	}, nil
}
