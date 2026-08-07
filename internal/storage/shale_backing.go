// The storage-engine seam: what a shale cluster needs mounted underneath it,
// stated without naming an engine.
//
// ShaleRepo is engine-agnostic. Everything above this file - the key layout,
// the durable intents, the quota, the guarded index writes - is the same code
// whether the units are slatedb databases on an object store or local pebble
// ones. openBacking is the single build-time choice, implemented once per
// engine, so nothing else in the package carries a build tag on its account.

package storage

import (
	"github.com/Zamua/shale/pkg/backend"
	"github.com/Zamua/shale/pkg/cluster"
	"github.com/Zamua/shale/pkg/storageunit"
)

// backing is an opened metadata store, ready for a cluster to mount.
//
// Exactly one mode is populated: a single backend, or a factory plus the unit
// count the key space is partitioned into. Setting both is a cluster.Open
// error, which is why applyTo picks rather than assigns.
type backing struct {
	backend   backend.Backend
	factory   storageunit.BackendFactory
	unitCount storageunit.UnitCount

	// release drops what the CLUSTER does not own: the multi-backend handle and
	// any engine-level resources behind it (block caches, native settings
	// objects). Call it after cluster.Close, never before - a mounted unit may
	// still be reading through them. Nil when the engine holds nothing extra.
	//
	// A single backend is owned by the cluster and closed by cluster.Close, so
	// release must NOT touch it. releaseAll is the open-failure path, where
	// that ownership never transferred.
	release func() error
}

// applyTo selects the cluster's backend mode.
func (b *backing) applyTo(cfg *cluster.Config) {
	if b.factory != nil {
		cfg.BackendFactory = b.factory
		cfg.UnitCount = b.unitCount
		return
	}
	cfg.Backend = b.backend
}

// releaseAll drops everything including the single backend, for the path where
// the cluster failed to open and so never took ownership of it.
func (b *backing) releaseAll() error {
	var err error
	if b.backend != nil {
		err = b.backend.Close()
	}
	if rerr := b.releaseUnowned(); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// releaseUnowned is the post-cluster-shutdown release: the handle and engine
// resources, never the cluster-owned backend.
func (b *backing) releaseUnowned() error {
	if b == nil || b.release == nil {
		return nil
	}
	return b.release()
}
