package service_test

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/service"
)

// The shale-path sweep contract, over the pure-Go service.Sweep wiring
// (Blobs nil + BlobOrphans set) cmd/hostthisd builds for a blob-capable
// ShaleRepo, so no MinIO or slatedb build tag is needed:
//   - the global content-addressed GC (WalkBlobs + Remove) does NOT run on the
//     shale path (the metadata delete already unbinds bound blobs),
//   - the orphan-bytes reclaimer IS invoked each tick with the configured
//     grace (DefaultOrphanGrace when OrphanGrace is zero).

// recordingBlobs records whether the global content-addressed GC touched it.
type recordingBlobs struct {
	walkCalls   int
	removeCalls int
}

func (r *recordingBlobs) WalkBlobs(fn func(sha string) error) error {
	r.walkCalls++
	return nil
}

func (r *recordingBlobs) Remove(sha string) error {
	r.removeCalls++
	return nil
}

// recordingOrphanSweeper stands in for ShaleRepo.SweepBlobOrphans, recording
// each call and the grace it was handed.
type recordingOrphanSweeper struct {
	calls     int
	lastGrace time.Duration
	lastNow   time.Time
}

func (r *recordingOrphanSweeper) SweepBlobOrphans(_ context.Context, now time.Time, grace time.Duration) error {
	r.calls++
	r.lastGrace = grace
	r.lastNow = now
	return nil
}

// noopSweepRepo reports no referenced shas.
type noopSweepRepo struct{}

func (noopSweepRepo) ReferencedBlobSHAs() ([]string, error) { return nil, nil }

// The collocated plane reports no global GC: the cluster owns reachability,
// so there is nothing for a content-addressed walk to reclaim.
func TestSweep_ShalePath_NoGlobalGC(t *testing.T) {
	orphans := &recordingOrphanSweeper{}
	sweep := &service.Sweep{
		Repo:     noopSweepRepo{},
		Blobs:    service.CollocatedReclaimer{Sweeper: orphans},
		Interval: time.Hour,
		Logger:   log.New(io.Discard, "", 0),
		Now:      func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) },
	}

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	blobsGCd, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if blobsGCd != 0 {
		t.Fatalf("shale path must not GC via the global content-addressed sweep: blobsGCd=%d", blobsGCd)
	}
}

// TestSweep_ShalePath_TickRunsOrphanSweep: each tick invokes SweepBlobOrphans
// with DefaultOrphanGrace (OrphanGrace zero), and never the global GC.
func TestSweep_ShalePath_TickRunsOrphanSweep(t *testing.T) {
	orphans := &recordingOrphanSweeper{}
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	sweep := &service.Sweep{
		Repo:     noopSweepRepo{},
		Blobs:    service.CollocatedReclaimer{Sweeper: orphans},
		Interval: time.Hour,
		Logger:   log.New(io.Discard, "", 0),
		Now:      func() time.Time { return now },
	}

	// Run does an immediate tick then blocks on the ticker; cancelling first
	// leaves only the boot tick.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweep.Run(ctx)

	if orphans.calls != 1 {
		t.Fatalf("orphan sweep should run once per tick: calls=%d", orphans.calls)
	}
	if orphans.lastGrace != service.DefaultOrphanGrace {
		t.Fatalf("orphan grace = %s, want DefaultOrphanGrace (%s)", orphans.lastGrace, service.DefaultOrphanGrace)
	}
	if !orphans.lastNow.Equal(now.UTC()) {
		t.Fatalf("orphan sweep now = %s, want %s", orphans.lastNow, now.UTC())
	}
}

// A non-zero Grace is threaded to SweepBlobOrphans verbatim, not overridden by
// the default.
func TestSweep_ShalePath_HonorsConfiguredGrace(t *testing.T) {
	orphans := &recordingOrphanSweeper{}
	sweep := &service.Sweep{
		Repo:     noopSweepRepo{},
		Blobs:    service.CollocatedReclaimer{Sweeper: orphans, Grace: 3 * time.Hour},
		Interval: time.Hour,
		Logger:   log.New(io.Discard, "", 0),
		Now:      func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweep.Run(ctx)

	if orphans.calls != 1 {
		t.Fatalf("orphan sweep should run once: calls=%d", orphans.calls)
	}
	if orphans.lastGrace != 3*time.Hour {
		t.Fatalf("orphan grace = %s, want configured 3h", orphans.lastGrace)
	}
}

// Selecting the detached plane cannot also select the collocated one: one
// field holds the reclaimer, so the orphan sweep is unreachable from here.
func TestSweep_StandalonePath_NoOrphanSweep(t *testing.T) {
	orphans := &recordingOrphanSweeper{}
	blobs := &recordingBlobs{}
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	sweep := &service.Sweep{
		Repo:     noopSweepRepo{},
		Blobs:    service.DetachedStoreReclaimer{Blobs: blobs},
		Interval: time.Hour,
		Logger:   log.New(io.Discard, "", 0),
		Now:      func() time.Time { return now },
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweep.Run(ctx)

	if orphans.calls != 0 {
		t.Fatalf("orphan sweep must not run on the standalone path: calls=%d", orphans.calls)
	}
}
