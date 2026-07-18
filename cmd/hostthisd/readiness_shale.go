// readiness_shale.go - the shale ReadinessProber adapter (docs/SPEC.md
// "Readiness vs liveness"). Composition-root glue: it adapts the
// ShaleRepo's mount-readiness accessors onto the http server's narrow
// ReadinessProber port, so internal/http stays storage-agnostic and
// internal/storage stays http-agnostic. Tagged slatedb because the
// *storage.ShaleRepo type only exists under that tag.

//go:build slatedb

package main

import (
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/storage"
)

// shaleReadinessProber gates /readyz on the shale cluster's mount floor:
// Ready iff mounted >= ceil(minMountedFraction * desired). The fraction
// semantics (0 = no floor, desired == 0 vacuously ready) live in the
// shale predicate, not here.
type shaleReadinessProber struct {
	repo               *storage.ShaleRepo
	minMountedFraction float64
}

func (p shaleReadinessProber) Ready() bool {
	return p.repo.Ready(p.minMountedFraction)
}

func (p shaleReadinessProber) ReadinessStats() httpapi.ReadinessStats {
	mr := p.repo.MountReadiness()
	return httpapi.ReadinessStats{
		Desired:          mr.DesiredUnits,
		Mounted:          mr.MountedUnits,
		Pending:          mr.PendingUnits,
		FailedOpen:       mr.FailedOpenUnits,
		LastAcquireError: mr.LastAcquireError,
	}
}
