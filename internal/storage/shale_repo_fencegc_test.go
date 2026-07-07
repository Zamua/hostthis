//go:build slatedb

package storage

import (
	"encoding/json"
	"testing"

	slatedb "slatedb.io/slatedb-go/uniffi"
)

// TestNewFenceWALGCSettings pins the permanent fix for the fence-WAL bloat (the
// 0-byte WAL objects slatedb writes one-per-open to claim the writer epoch).
// slatedb ships its fence-WAL garbage collector in DRY-RUN by default, so those
// objects are never deleted and accumulate without bound, making a unit's
// cold-start open crawl (it re-reads every WAL). The fix is to flip ONLY that
// one flag (garbage_collector_options.wal_fence_options.dry_run) to false; this
// test asserts both that the slatedb default is the bug and that our Settings
// corrects it without touching the (already-active) data-WAL collector.
func TestNewFenceWALGCSettings(t *testing.T) {
	type gcView struct {
		GC struct {
			WALFence struct {
				DryRun bool `json:"dry_run"`
			} `json:"wal_fence_options"`
			WAL struct {
				DryRun bool `json:"dry_run"`
			} `json:"wal_options"`
		} `json:"garbage_collector_options"`
	}
	parse := func(s *slatedb.Settings) gcView {
		js, err := s.ToJsonString()
		if err != nil {
			t.Fatalf("ToJsonString: %v", err)
		}
		var g gcView
		if err := json.Unmarshal([]byte(js), &g); err != nil {
			t.Fatalf("unmarshal settings json: %v\n%s", err, js)
		}
		return g
	}

	// Precondition: slatedb's default ships fence-WAL GC in dry-run - that IS
	// the leak. If this ever flips upstream the fix may be redundant; fail loud.
	def := slatedb.SettingsDefault()
	defer def.Destroy()
	if d := parse(def); !d.GC.WALFence.DryRun {
		t.Fatalf("precondition: expected slatedb default wal_fence_options.dry_run=true (the bug); got false - did the upstream default change?")
	}

	// The fix: fence-WAL GC active (dry_run=false), data-WAL GC unchanged.
	fixed, err := newFenceWALGCSettings()
	if err != nil {
		t.Fatalf("newFenceWALGCSettings: %v", err)
	}
	defer fixed.Destroy()
	f := parse(fixed)
	if f.GC.WALFence.DryRun {
		t.Error("wal_fence_options.dry_run = true, want false (fence WALs would never be reaped)")
	}
	if f.GC.WAL.DryRun {
		t.Error("wal_options.dry_run = true, want false (slatedb default; must stay unchanged)")
	}
}
