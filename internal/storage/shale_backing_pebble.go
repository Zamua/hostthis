// The pebble backing: shale units as local pebble databases, in memory.
//
// Pure Go, no cgo, no native library, no object store, so the whole ShaleRepo
// compiles and its behaviour is testable in a plain `go test ./...`. This is
// what the default build and CI get; production selects slate with -tags
// slatedb.

//go:build !slatedb

package storage

import (
	"fmt"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"

	"github.com/Zamua/shale/backends/pebble"
)

// openBacking opens one pebble database, on disk at cfg.LocalDir or in memory
// when that is empty. Local either way: a node's store is its own, so this
// cannot back a multi-node deployment.
//
// Multi-backend mode is not offered. Units are mounted and FENCED through a
// storageunit.BackendFactory, whose epoch contract is a property of a SHARED
// store that survives a node; a process-local engine has nothing to fence
// against. Silently collapsing to one backend would answer a request for
// sharded storage with unsharded storage, so it is an error instead.
func openBacking(cfg ShaleConfig) (*backing, error) {
	if cfg.UnitCount > 0 {
		return nil, fmt.Errorf("shale: UnitCount %d requires a shared backing store; rebuild with -tags slatedb", cfg.UnitCount)
	}

	pc := pebble.Config{Dir: cfg.LocalDir}
	if pc.Dir == "" {
		// DbName is only a label here: an in-memory FS is private to this
		// handle, so two repos in one process cannot collide whatever they are
		// called.
		pc.Dir = cfg.DbName
		if pc.Dir == "" {
			pc.Dir = "hostthis"
		}
		pc.Options = &pebbledb.Options{FS: vfs.NewMem()}
	}
	be, err := pebble.New(pc)
	if err != nil {
		return nil, fmt.Errorf("shale: open pebble backend: %w", err)
	}
	return &backing{backend: be}, nil
}
