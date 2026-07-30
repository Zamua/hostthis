// readyz.go - the /readyz rollout gate (docs/SPEC.md "Readiness vs
// liveness"): 200 iff the metadata backend's readiness predicate passes, 503
// otherwise, with the mount counts as JSON in both directions so a stalled
// rollout is diagnosable with curl. /healthz stays pure liveness and never
// gates on storage.
package http

import (
	"encoding/json"
	"net/http"
)

// ReadinessProber is the narrow port /readyz gates on. The composition root
// wires the shale backend's mount-floor predicate (mounted >= ceil(fraction *
// desired)) here; backends with no mount concept leave it nil, which means the
// process being up IS ready since their open failures already fail startup.
type ReadinessProber interface {
	// Ready reports whether this replica should receive traffic and a rollout
	// may proceed past it.
	Ready() bool
	// ReadinessStats returns the counts behind the verdict, echoed in the
	// /readyz body for probe-side diagnosis.
	ReadinessStats() ReadinessStats
}

// ReadinessStats mirrors the metadata backend's mount counters in the wire
// shape /readyz emits. All zero on backends with no mount concept.
type ReadinessStats struct {
	// Desired is the number of storage-unit positions this pod owns.
	Desired int `json:"desired"`
	// Mounted is the number of desired positions currently mounted.
	Mounted int `json:"mounted"`
	// Pending positions are in flight or failing.
	Pending int `json:"pending"`
	// FailedOpen is the subset of pending whose last open attempt errored.
	FailedOpen int `json:"failedOpen"`
	// LastAcquireError is one representative open error; omitted when none.
	LastAcquireError string `json:"lastAcquireError,omitempty"`
}

type readyzBody struct {
	Ready bool `json:"ready"`
	ReadinessStats
}

// serveReadyz answers the orchestrator's readiness probe: 200 when ready (a
// nil prober is always ready), 503 when the predicate fails. no-store because
// a probe result must never be cached.
func (s *Server) serveReadyz(w http.ResponseWriter, _ *http.Request) {
	body := readyzBody{Ready: true}
	if s.Readiness != nil {
		body.Ready = s.Readiness.Ready()
		body.ReadinessStats = s.Readiness.ReadinessStats()
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if !body.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(body)
}
