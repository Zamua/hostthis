package storage_test

// The in-memory backend's entry into the backend-agnostic conformance suite.
//
// No build tag, no external services, so the contract runs on every
// `go test ./...`. This is what keeps MemRepo an honest test double: an
// assertion that passes here and fails on celld is a backend bug, never a
// fixture artifact.

import (
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

func TestConformance_Memory(t *testing.T) {
	// Both strict: every check-then-write runs under one mutex, so whatever a
	// concurrent burst lands, no cap is ever exceeded.
	caps := conformCaps{StrictQuotaUnderConcurrency: true, StrictIdentityQuotaUnderConcurrency: true}
	newRepo := func(t *testing.T) conformanceRepo { return storage.NewMemRepo() }
	newSites := func(t *testing.T) (conformanceRepo, conformanceSiteRepo) {
		repo := storage.NewMemRepo()
		return repo, storage.NewSites(repo)
	}
	newRooms := func(t *testing.T) roomConformanceStores {
		repo := storage.NewMemRepo()
		return roomConformanceStores{
			Rooms: storage.NewMemRoomRepo(repo),
			Paste: repo,
			Site:  storage.NewSites(repo),
		}
	}
	runConformanceWithSites(t, "memory", caps, newRepo, newSites, newRooms)
	runKeygateConformance(t, "memory", func(t *testing.T) keygateRepo { return storage.NewMemRepo() })
}
