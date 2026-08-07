// Package storagetest opens a metadata repo for tests in other packages.
//
// The engine follows the build, exactly as it does in production wiring: the
// default build gets a local in-memory one and needs nothing installed, and a
// -tags slatedb build gets the object-store engine and skips when no endpoint
// is configured. Callers name neither.
package storagetest

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

var seq atomic.Int64

var (
	mu    sync.Mutex
	repos = map[*testing.T]*storage.ShaleRepo{}
)

// NewRepo returns t's repo, opening it on first call and closing it via
// t.Cleanup.
//
// The paste, keygate, site and room surfaces all hang off this one object:
// pass it directly where a PasteRepo or KeyGateRepo is wanted, and wrap it in
// storage.NewShaleSiteRepo / storage.NewShaleRoomRepo for the other two.
//
// ONE store per test, however many times it is called. Pastes and sites share
// a slug namespace and guard it with a cross-family read, so handing a test
// two independent stores would let a collision the real system rejects go
// unnoticed.
func NewRepo(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if r, ok := repos[t]; ok {
		return r
	}
	cfg := config(t)
	cfg.NodeID = fmt.Sprintf("test-node-%d", seq.Add(1))
	cfg.ReplicationFactor = 1
	repo, err := storage.NewShaleRepo(cfg)
	if err != nil {
		t.Fatalf("open test metadata repo: %v", err)
	}
	repos[t] = repo
	t.Cleanup(func() {
		mu.Lock()
		delete(repos, t)
		mu.Unlock()
		_ = repo.Close()
	})
	return repo
}
