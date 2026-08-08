// metadata_local.go - the default bundle: a single-node shale cluster on this
// build's LOCAL storage engine (pebble), persisted under the data dir.
//
// Zero configuration and no external services, which is what `make run` and a
// fresh clone want. Single-node by construction, so a deployment uses the
// shale branch instead (metadata_shale.go).

//go:build !slatedb

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/Zamua/hostthis/internal/storage"
)

func buildMetadataLocal(dataDir string, logger *log.Logger) (*metadataBundle, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir data-dir: %w", err)
	}
	dir := filepath.Join(dataDir, "metadata")
	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            "local",
		DbName:            "hostthis",
		LocalDir:          dir,
		ReplicationFactor: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("open local metadata: %w", err)
	}
	logger.Printf("metadata: local shale at %s", dir)
	sites := storage.NewArtifactSites(repo)
	return &metadataBundle{
		Repo:    repo,
		KeyGate: repo,
		Sites:   sites,
		Rooms:   storage.NewShaleRoomRepo(repo),
		// Writes span shards even on one node, so the same boot sweep that
		// settles a half-finished write in production applies here.
		IntentSweeper: repo,
		Close:         repo.Close,
	}, nil
}
