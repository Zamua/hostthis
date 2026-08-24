// metadata_memory.go - the in-memory metadata bundle: MemRepo behind every
// port. Zero configuration, nothing installed, which is what `make run`, a
// fresh clone and the e2e suite want.
//
// EPHEMERAL BY DESIGN: metadata lives for the process. Blobs still go to the
// configured blob backend, so a restart leaves orphaned blobs behind - which
// the sweep reclaims, and which no dev flow cares about. The startup line says
// so, because an operator who reaches for this in production should learn it
// from the log, not from an empty listing after a restart.

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
		// RoomRelay nil: no /ws surface on the memory backend. The realtime
		// layer is the room cell's; an in-process fan-out here would be the hub
		// relay reborn for a backend whose whole point is having no server.
	}, nil
}
