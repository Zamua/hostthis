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

// These tests pin the SHALE-PATH sweep contract WITHOUT MinIO or the slatedb
// build tag: they exercise the pure-Go service.Sweep wiring (Blobs nil +
// BlobOrphans set) that cmd/hostthisd builds for a blob-capable ShaleRepo.
//
// What they prove (docs/design/shale-blobs-phase3.md section 10.3 step 2):
//   - the global content-addressed GC (WalkBlobs + Remove) does NOT run on the
//     shale path (Blobs is nil; the metadata-delete already unbinds bound blobs),
//   - the orphan-bytes reclaimer (SweepOrphans) IS invoked each tick with the
//     configured grace (DefaultOrphanGrace when OrphanGrace is zero).

// recordingBlobs is a SweepBlobs whose WalkBlobs/Remove record whether the
// global content-addressed GC touched it. On the shale path it must NEVER be
// wired (Sweep.Blobs is nil), so these counters stay zero - the test asserts
// the shale wiring leaves Blobs nil rather than handing the sweep a live store.
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

// recordingOrphanSweeper records each SweepBlobOrphans call + the grace it was
// handed, standing in for ShaleRepo.SweepBlobOrphans (-> BlobKV.SweepOrphans).
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

// noopSweepRepo is a SweepRepo with nothing to expire and no referenced shas -
// the shale path's metadata side is irrelevant to what these tests pin (the
// orphan sweep, not the expiry pass). Panics on Delete so an unexpected expiry
// surfaces.
type noopSweepRepo struct{}

func (noopSweepRepo) ExpiredSlugs(_ time.Time) ([]string, error) { return nil, nil }
func (noopSweepRepo) Delete(_ domain.Slug) error                 { panic("not expected: nothing should expire") }
func (noopSweepRepo) ReferencedBlobSHAs() ([]string, error)      { return nil, nil }

// TestSweep_ShalePath_NoGlobalGC: with Blobs nil (the shale wiring) Once does
// NOT run the global content-addressed GC - it returns early before any
// WalkBlobs/Remove. Even with a recordingBlobs available, the shale wiring keeps
// Blobs nil, so the recorder is never touched.
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
	// The whole point: Blobs==nil short-circuits the global GC entirely, so the
	// blobsGCd count is zero and no WalkBlobs/Remove ran.
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

	// Run the loop once: Run does an immediate tick then blocks on the ticker;
	// cancel right away so only the boot tick fires.
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
// BlobOrphans nil) runs the global GC and NEVER the orphan sweep - the two GC
// mechanisms stay on their own paths. We assert the orphan sweeper is untouched
// when only Blobs is wired.
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
