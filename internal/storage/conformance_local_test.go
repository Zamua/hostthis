// The default build's entry into the backend-agnostic conformance suite.
//
// No build tag, no cgo, no external services, so the contract is exercised on
// every `go test ./...`. The repo underneath is the same ShaleRepo production
// runs; only the storage engine differs (docs/SPEC.md "The storage-engine
// seam").

//go:build !slatedb

package storage_test

import (
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

func TestConformance_Local(t *testing.T) {
	// StrictIdentityQuotaUnderConcurrency is false for the same reason it is on
	// the slate entry: the per-identity quota is a scan over the authoritative
	// rows that is NOT atomic with the write, so the cap admits a bounded
	// overshoot under same-owner concurrency. That is a property of ShaleRepo,
	// not of the engine beneath it.
	caps := conformCaps{StrictQuotaUnderConcurrency: true, StrictIdentityQuotaUnderConcurrency: false}

	newRepo := func(t *testing.T) conformanceRepo { return storagetest.NewRepo(t) }
	// One store per test, so the cross-quota and cross-family-slug subtests
	// exercise the real interaction rather than two isolated keyspaces.
	newSites := func(t *testing.T) (conformanceRepo, conformanceSiteRepo) {
		repo := storagetest.NewRepo(t)
		return repo, storage.NewSites(repo)
	}
	newRooms := func(t *testing.T) roomConformanceStores {
		repo := storagetest.NewRepo(t)
		return roomConformanceStores{
			Rooms: storage.NewShaleRoomRepo(repo),
			Paste: repo,
			Site:  storage.NewSites(repo),
		}
	}
	runConformanceWithSites(t, "local", caps, newRepo, newSites, newRooms)
}
