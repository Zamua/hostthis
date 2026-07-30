// ShaleRepo mount-readiness accessors (docs/SPEC.md "Readiness vs
// liveness"): pass-throughs to the embedded cluster so the composition root
// can gate /readyz on mount state without storage exporting the cluster.

//go:build slatedb

package storage

import "github.com/Zamua/shale/pkg/cluster"

// MountReadiness returns the cluster's point-in-time mount-state counts
// (desired / mounted / pending / failed-open positions plus one
// representative acquire error). Cheap enough for a probe polled every few
// seconds; all zero in single-backend mode.
func (r *ShaleRepo) MountReadiness() cluster.MountReadiness {
	return r.cluster.MountReadiness()
}

// Ready reports whether this node has mounted at least
// ceil(minMountedFraction * desired) of its desired storage-unit positions.
// Edge contract is the cluster predicate's: desired == 0 is vacuously ready,
// a fraction <= 0 means no floor.
func (r *ShaleRepo) Ready(minMountedFraction float64) bool {
	return r.cluster.Ready(minMountedFraction)
}
