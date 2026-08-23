// metadata_celld.go - celld-backed metadataBundle.
//
// Needs no build tag and no cgo: celld is reached over HTTP, so this backend
// builds in the plain toolchain where `shale` needs -tags slatedb plus the
// slatedb cdylib on the loader path.
//
//	HOSTTHIS_CELLD_ENDPOINT  (required, e.g. http://celld:8080)

package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/storage"
)

func buildMetadataCelld(logger *log.Logger) (*metadataBundle, error) {
	base := envOr("HOSTTHIS_CELLD_ENDPOINT", "")
	if base == "" {
		return nil, errors.New("HOSTTHIS_CELLD_ENDPOINT is required for the celld backend")
	}
	// One client across every adapter, so connections to the fleet are pooled
	// rather than each surface opening its own.
	client := &http.Client{Timeout: 30 * time.Second}
	repo := celld.NewPasteRepo(base, client)

	logger.Printf("metadata backend: celld at %s", base)
	return &metadataBundle{
		Repo:    repo,
		KeyGate: celld.NewKeyGateRepo(base, client),
		// The SAME storage.Sites the shale backend uses. A site is a paste with
		// Kind=site, so the translation is pure vocabulary and belongs to
		// neither backend.
		Sites: storage.NewSites(repo),
		Rooms: celld.NewRoomRepo(base, client),

		// BlobUnit stays nil: celld holds no bytes, so the blob plane keeps its
		// standalone detached store rather than pretending to co-commit.
		//
		// IntentSweeper is nil because the intent log lives IN the identity
		// cell, and a cell settles its own half-finished writes on the next
		// touch. There is no cross-shard state for a boot sweep to find.
		//
		// RelayPeer is nil: a room is owned by one cell, so there is no
		// second pod holding the same room to fan out to.
		//
		// Readiness is nil - always ready. celld has no mount floor to reach;
		// a cell activates on demand.
	}, nil
}
