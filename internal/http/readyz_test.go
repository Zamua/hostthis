package http

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// fakeProber is the test double behind the /readyz pins. A REAL
// shale-backed pin (a cluster reporting 0 mounted units) needs the slatedb
// build tag plus a live MinIO, so the readiness split is pinned here via
// the same narrow port the composition root wires; the shale predicate's
// own edge contract is pinned upstream in the shale repo.
type fakeProber struct {
	ready bool
	stats ReadinessStats
}

func (f fakeProber) Ready() bool                    { return f.ready }
func (f fakeProber) ReadinessStats() ReadinessStats { return f.stats }

func decodeReadyz(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("readyz body is not JSON: %v (body %q)", err, body)
	}
	return m
}

// TestReadyz_NotReady503_HealthzStays200 pins the liveness/readiness
// split for the uniform-failure class (docs/SPEC.md "Readiness vs
// liveness"): a pod whose cluster reports 0 mounted of N desired units
// fails /readyz with the diagnosable counts body, while /healthz on the
// SAME server stays 200 (a restart cannot fix an unmountable store, so
// liveness must not gate on it).
func TestReadyz_NotReady503_HealthzStays200(t *testing.T) {
	srv := &Server{
		ApexDomain: "paste.test",
		Readiness: fakeProber{
			ready: false,
			stats: ReadinessStats{
				Desired:          8,
				Mounted:          0,
				Pending:          8,
				FailedOpen:       8,
				LastAcquireError: "open unit 0: access denied",
			},
		},
	}
	h := srv.Handler()

	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("/readyz status: got %d, want 503", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type: got %q, want application/json; charset=utf-8", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control: got %q, want no-store", cc)
	}
	m := decodeReadyz(t, w.Body.Bytes())
	if m["ready"] != false {
		t.Errorf("ready: got %v, want false", m["ready"])
	}
	if m["desired"] != float64(8) || m["mounted"] != float64(0) ||
		m["pending"] != float64(8) || m["failedOpen"] != float64(8) {
		t.Errorf("counts: got %v, want desired=8 mounted=0 pending=8 failedOpen=8", m)
	}
	if m["lastAcquireError"] != "open unit 0: access denied" {
		t.Errorf("lastAcquireError: got %v", m["lastAcquireError"])
	}

	// Liveness is unaffected on the same server: /healthz stays 200.
	r = httptest.NewRequest("GET", "/healthz", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("/healthz status with not-ready prober: got %d, want 200", w.Code)
	}
}

// TestReadyz_Ready200WithCounts pins the healthy direction: 200 with the
// same counts JSON, so the body is curl-diagnosable in both directions.
func TestReadyz_Ready200WithCounts(t *testing.T) {
	srv := &Server{
		ApexDomain: "paste.test",
		Readiness: fakeProber{
			ready: true,
			stats: ReadinessStats{Desired: 8, Mounted: 8},
		},
	}
	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("/readyz status: got %d, want 200", w.Code)
	}
	m := decodeReadyz(t, w.Body.Bytes())
	if m["ready"] != true {
		t.Errorf("ready: got %v, want true", m["ready"])
	}
	if m["desired"] != float64(8) || m["mounted"] != float64(8) {
		t.Errorf("counts: got %v, want desired=8 mounted=8", m)
	}
	if _, present := m["lastAcquireError"]; present {
		t.Errorf("lastAcquireError should be omitted when empty, got %v", m["lastAcquireError"])
	}
}

// TestReadyz_NilProberAlwaysReady pins the non-shale path (sqlite /
// single-node slatedb, and every test fixture that never wires a prober):
// no ReadinessProber means process-up IS ready, zero counts in the body.
func TestReadyz_NilProberAlwaysReady(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test"}
	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("/readyz status with nil prober: got %d, want 200", w.Code)
	}
	m := decodeReadyz(t, w.Body.Bytes())
	if m["ready"] != true {
		t.Errorf("ready: got %v, want true", m["ready"])
	}
	if m["desired"] != float64(0) || m["mounted"] != float64(0) {
		t.Errorf("counts: got %v, want all zero", m)
	}
}
