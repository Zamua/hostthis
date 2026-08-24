// Package storagetest opens a metadata repo for tests in other packages: one
// in-memory MemRepo per test, honest because the same conformance suite the
// celld backend passes runs against it on every `go test ./...`.
package storagetest

import (
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

var (
	mu    sync.Mutex
	repos = map[*testing.T]*storage.MemRepo{}
)

// NewRepo returns t's repo, opening it on first call.
//
// ONE store per test, however many times it is called. Pastes and sites share
// a slug namespace and guard it with a cross-family read, so handing a test
// two independent stores would let a collision the real system rejects go
// unnoticed. Wrap it in storage.NewSites / storage.NewMemRoomRepo for the
// site and room views.
func NewRepo(t *testing.T) *storage.MemRepo {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if r, ok := repos[t]; ok {
		return r
	}
	repo := storage.NewMemRepo()
	repos[t] = repo
	t.Cleanup(func() {
		mu.Lock()
		delete(repos, t)
		mu.Unlock()
	})
	return repo
}
