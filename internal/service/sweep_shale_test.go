package service_test

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
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

// noopSweepRepo has nothing to expire and no referenced shas. It panics on
// DeleteExpired so an unexpected expiry surfaces rather than passing silently.
type noopSweepRepo struct{}

func (noopSweepRepo) ExpiredPastes(_ time.Time) ([]domain.ExpiredPaste, error) { return nil, nil }
func (noopSweepRepo) DeleteExpired(_ domain.ExpiredPaste) (bool, error) {
	panic("not expected: nothing should expire")
}
func (noopSweepRepo) ReferencedBlobSHAs() ([]string, error) { return nil, nil }

// TestSweep_ShalePath_NoGlobalGC: with Blobs nil, Once returns before any
// WalkBlobs/Remove.
func TestSweep_ShalePath_NoGlobalGC(t *testing.T) {
	orphans := &recordingOrphanSweeper{}
	sweep := &service.Sweep{
		Repo:        noopSweepRepo{},
		Blobs:       nil, // shale path: the cluster owns the blobs; no global GC
		BlobOrphans: orphans,
		Interval:    time.Hour,
		Logger:      log.New(io.Discard, "", 0),
		Now:         func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) },
	}

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	pastes, blobsGCd, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if pastes != 0 {
		t.Fatalf("nothing should expire: pastes=%d", pastes)
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
		Repo:        noopSweepRepo{},
		Blobs:       nil,
		BlobOrphans: orphans,
		Interval:    time.Hour,
		Logger:      log.New(io.Discard, "", 0),
		Now:         func() time.Time { return now },
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

// TestSweep_ShalePath_HonorsConfiguredGrace: a non-zero OrphanGrace is threaded
// to SweepBlobOrphans verbatim (not overridden by the default).
func TestSweep_ShalePath_HonorsConfiguredGrace(t *testing.T) {
	orphans := &recordingOrphanSweeper{}
	sweep := &service.Sweep{
		Repo:        noopSweepRepo{},
		Blobs:       nil,
		BlobOrphans: orphans,
		OrphanGrace: 3 * time.Hour,
		Interval:    time.Hour,
		Logger:      log.New(io.Discard, "", 0),
		Now:         func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) },
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

// TestSweep_StandalonePath_NoOrphanSweep: the standalone path (Blobs set,
// BlobOrphans nil) runs the global GC and never the orphan sweep. The two GC
// mechanisms stay on their own paths.
func TestSweep_StandalonePath_NoOrphanSweep(t *testing.T) {
	orphans := &recordingOrphanSweeper{}
	blobs := &recordingBlobs{}
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	sweep := &service.Sweep{
		Repo:        noopSweepRepo{},
		Blobs:       blobs, // standalone path: the global content-addressed GC
		BlobOrphans: nil,   // no orphan sweep on the standalone path
		Interval:    time.Hour,
		Logger:      log.New(io.Discard, "", 0),
		Now:         func() time.Time { return now },
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sweep.Run(ctx)

	if orphans.calls != 0 {
		t.Fatalf("orphan sweep must not run on the standalone path: calls=%d", orphans.calls)
	}
}
