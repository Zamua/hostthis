// The slate backing: shale units as slatedb databases on an S3-compatible
// object store. The production engine. Needs cgo + libslatedb_uniffi.

//go:build slatedb

package storage

import (
	"fmt"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/pkg/storageunit"
	slatedb "slatedb.io/slatedb-go/uniffi"
)

// openBacking opens the slate engine in whichever mode cfg asks for: UnitCount
// units under one Backing, or a single database.
//
// The native handles it allocates (block cache, GC settings) outlive the open
// call and are Destroyed by the returned release, so every failure path below
// tears down what it already built rather than leaking a uniffi object.
func openBacking(cfg ShaleConfig) (*backing, error) {
	sc := slateConfigFromShale(cfg)

	// Without a block cache slatedb re-fetches SST blocks from the object store
	// on every read: on a distributed-MinIO backend that is a self-inflicted
	// read storm, the same hot SSTs fetched hundreds of times a second.
	var cache *slatedb.DbCache
	if cfg.CacheBytes > 0 {
		c, err := slatedb.DbCacheNewMokaCache(slatedb.MokaCacheOptions{MaxCapacity: cfg.CacheBytes})
		if err != nil {
			return nil, fmt.Errorf("shale: build slatedb block cache: %w", err)
		}
		sc.Cache = c
		cache = c
	}

	// Forwarded verbatim to every unit's DbBuilder, in both modes.
	var fenceGCSettings *slatedb.Settings
	if cfg.ReapFenceWALs {
		s, err := newFenceWALGCSettings()
		if err != nil {
			if cache != nil {
				cache.Destroy()
			}
			return nil, fmt.Errorf("shale: build fence-WAL GC settings: %w", err)
		}
		sc.Settings = s
		fenceGCSettings = s
	}

	destroyHandles := func() {
		if cache != nil {
			cache.Destroy()
		}
		if fenceGCSettings != nil {
			fenceGCSettings.Destroy()
		}
	}

	if cfg.UnitCount > 0 {
		// MULTI-BACKEND (sharded): UnitCount independent slatedb unit databases
		// under cfg.DbName as the shared key-prefix, distributed across the ring
		// and routed per key by ShardKeyFn (docs/SPEC.md "Sharded metadata").
		uc, err := storageunit.NewUnitCount(cfg.UnitCount)
		if err != nil {
			destroyHandles()
			return nil, fmt.Errorf("shale: invalid UnitCount %d (must be a power of two): %w", cfg.UnitCount, err)
		}
		b, err := slate.NewBacking(slate.BackingConfig{
			Bucket:                   cfg.Bucket,
			Endpoint:                 cfg.Endpoint,
			Region:                   cfg.Region,
			AccessKey:                cfg.AccessKey,
			SecretKey:                cfg.SecretKey,
			UseSSL:                   cfg.UseSSL,
			KeyPrefix:                cfg.DbName,
			Cache:                    cache,
			Settings:                 fenceGCSettings,
			RelaxedReplicaDurability: cfg.RelaxedDurability,
		})
		if err != nil {
			destroyHandles()
			return nil, fmt.Errorf("shale: open slate backing: %w", err)
		}
		handle := b.Handle()
		return &backing{
			factory:   handle,
			unitCount: uc,
			release: func() error {
				// Handle first: the units captured these handles at Build time,
				// so nothing may reference them until the backing has shut down.
				err := handle.Close()
				destroyHandles()
				return err
			},
		}, nil
	}

	// SINGLE-BACKEND: one slatedb database, owned by the cluster.
	be, err := slate.New(sc)
	if err != nil {
		destroyHandles()
		return nil, fmt.Errorf("shale: open slate backend: %w", err)
	}
	return &backing{
		backend: be,
		release: func() error {
			destroyHandles()
			return nil
		},
	}, nil
}

// slateConfigFromShale maps a ShaleConfig to the slate.Config used to open the
// per-node backend. Pure, so the WriteOptions wiring is unit-testable without a
// live object store. The S3 fields copy straight through; the only logic is the
// durability knob, and nil WriteOptions is slate's AwaitDurable=true.
func slateConfigFromShale(cfg ShaleConfig) slate.Config {
	sc := slate.Config{
		Bucket:    cfg.Bucket,
		DbName:    cfg.DbName,
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		UseSSL:    cfg.UseSSL,
	}
	if cfg.RelaxedDurability {
		sc.WriteOptions = &slatedb.WriteOptions{AwaitDurable: false}
	}
	return sc
}

// newFenceWALGCSettings builds a slatedb Settings with the fence-WAL garbage
// collector enabled. ONLY dry_run is flipped: every other GC category and the
// conservative min_age (slatedb's 300s default) stay untouched, so the GC reaps
// superseded fence WAL objects and never a data WAL or a still-live fence. See
// ShaleConfig.ReapFenceWALs. The returned handle is operator-owned: the caller
// forwards it to the slate backend and Destroys it on shutdown.
func newFenceWALGCSettings() (*slatedb.Settings, error) {
	s := slatedb.SettingsDefault()
	if err := s.Set("garbage_collector_options.wal_fence_options.dry_run", "false"); err != nil {
		s.Destroy()
		return nil, err
	}
	return s, nil
}
