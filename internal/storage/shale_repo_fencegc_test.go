//go:build slatedb

package storage

import (
	"encoding/json"
	"testing"

	slatedb "slatedb.io/slatedb-go/uniffi"
)

// TestNewFenceWALGCSettings pins that newFenceWALGCSettings activates slatedb's
// fence-WAL collector (dry-run by default, so the 0-byte fence WAL written per
// open accumulates without bound and slows cold-start) and leaves the data-WAL
// collector alone.
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

	// Precondition: the upstream default is dry-run. If it ever flips, the
	// override below is redundant; fail loud rather than pass silently.
	def := slatedb.SettingsDefault()
	defer def.Destroy()
	if d := parse(def); !d.GC.WALFence.DryRun {
		t.Fatalf("precondition: expected slatedb default wal_fence_options.dry_run=true (the bug); got false - did the upstream default change?")
	}

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
