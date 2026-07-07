//go:build slatedb

package storage

import (
	"encoding/json"
	"testing"

	slatedb "slatedb.io/slatedb-go/uniffi"
)

// settingsView is the slice of the slatedb Settings JSON these pins read
// back. Asserting on ToJsonString output (not on Set's error) is the point:
// the uniffi Settings.Set deserializes WITHOUT unknown-field rejection, so a
// typo'd key silently no-ops - only the re-read JSON proves a key was
// actually accepted and applied.
type settingsView struct {
	MaxWALFlushesBeforeL0 int `json:"max_wal_flushes_before_l0_flush"`
	GC                    struct {
		WALFence struct {
			DryRun bool `json:"dry_run"`
		} `json:"wal_fence_options"`
		WAL struct {
			DryRun bool `json:"dry_run"`
		} `json:"wal_options"`
	} `json:"garbage_collector_options"`
}

func parseSettings(t *testing.T, s *slatedb.Settings) settingsView {
	t.Helper()
	js, err := s.ToJsonString()
	if err != nil {
		t.Fatalf("ToJsonString: %v", err)
	}
	var v settingsView
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		t.Fatalf("unmarshal settings json: %v\n%s", err, js)
	}
	return v
}

// TestNewSlatedbSettingsFenceGC pins the permanent fix for the fence-WAL
// bloat (the 0-byte WAL objects slatedb writes one-per-open to claim the
// writer epoch). slatedb ships its fence-WAL garbage collector in DRY-RUN by
// default, so those objects are never deleted and accumulate without bound,
// making a unit's cold-start open crawl (it re-reads every WAL). The fix is
// to flip ONLY that one flag (garbage_collector_options.wal_fence_options.
// dry_run) to false; this test asserts both that the slatedb default is the
// bug and that our Settings corrects it without touching the (already-
// active) data-WAL collector.
func TestNewSlatedbSettingsFenceGC(t *testing.T) {
	// Precondition: slatedb's default ships fence-WAL GC in dry-run - that IS
	// the leak. If this ever flips upstream the fix may be redundant; fail loud.
	def := slatedb.SettingsDefault()
	defer def.Destroy()
	if d := parseSettings(t, def); !d.GC.WALFence.DryRun {
		t.Fatalf("precondition: expected slatedb default wal_fence_options.dry_run=true (the bug); got false - did the upstream default change?")
	}

	// The fix: fence-WAL GC active (dry_run=false), data-WAL GC unchanged.
	fixed, err := newSlatedbSettings(true, 0)
	if err != nil {
		t.Fatalf("newSlatedbSettings(true, 0): %v", err)
	}
	defer fixed.Destroy()
	f := parseSettings(t, fixed)
	if f.GC.WALFence.DryRun {
		t.Error("wal_fence_options.dry_run = true, want false (fence WALs would never be reaped)")
	}
	if f.GC.WAL.DryRun {
		t.Error("wal_options.dry_run = true, want false (slatedb default; must stay unchanged)")
	}
	// The fence-only shape leaves the WAL flush backstop at slatedb's default.
	if f.MaxWALFlushesBeforeL0 != 4096 {
		t.Errorf("max_wal_flushes_before_l0_flush = %d, want the untouched slatedb default 4096", f.MaxWALFlushesBeforeL0)
	}
}

// TestNewSlatedbSettingsWALFlushBackstop pins the WAL flush backstop (docs/
// SPEC.md "WAL flush backstop: bounding live WAL accumulation"): slatedb's
// default max_wal_flushes_before_l0_flush (4096) lets each unit accumulate
// ~4096 live WAL objects before a backstop L0 flush advances the GC boundary,
// and at hostthis's tiny-value write profile the 64 MiB memtable trigger
// never fires - so 4096 is the only trigger and WAL residue (including
// fence-storm 0-byte WALs) sits unreapable ~forever at low write rates. The
// fix caps it (default 256 at the daemon).
//
// The assertions read the value back out of the Settings JSON rather than
// trusting Set's nil error: Settings.Set deserializes without unknown-field
// rejection, so a typo'd key would "succeed" while changing nothing.
func TestNewSlatedbSettingsWALFlushBackstop(t *testing.T) {
	// Precondition: slatedb's default IS the accumulation bug. Fail loud if
	// upstream ever changes it (the backstop default may then be redundant).
	def := slatedb.SettingsDefault()
	defer def.Destroy()
	if d := parseSettings(t, def); d.MaxWALFlushesBeforeL0 != 4096 {
		t.Fatalf("precondition: expected slatedb default max_wal_flushes_before_l0_flush=4096, got %d - did the upstream default change?", d.MaxWALFlushesBeforeL0)
	}

	// The cap is APPLIED (key accepted, value present in the re-read JSON),
	// alongside the fence-GC flip, without touching the data-WAL collector.
	s, err := newSlatedbSettings(true, 256)
	if err != nil {
		t.Fatalf("newSlatedbSettings(true, 256): %v", err)
	}
	defer s.Destroy()
	v := parseSettings(t, s)
	if v.MaxWALFlushesBeforeL0 != 256 {
		t.Errorf("max_wal_flushes_before_l0_flush = %d, want 256 (key not applied - typo'd keys no-op silently)", v.MaxWALFlushesBeforeL0)
	}
	if v.GC.WALFence.DryRun {
		t.Error("wal_fence_options.dry_run = true, want false (fence GC must ride the same Settings)")
	}
	if v.GC.WAL.DryRun {
		t.Error("wal_options.dry_run = true, want false (slatedb default; must stay unchanged)")
	}

	// Backstop without the fence GC: the cap applies, the fence collector
	// keeps slatedb's dry-run default (the two knobs are independent).
	capOnly, err := newSlatedbSettings(false, 256)
	if err != nil {
		t.Fatalf("newSlatedbSettings(false, 256): %v", err)
	}
	defer capOnly.Destroy()
	c := parseSettings(t, capOnly)
	if c.MaxWALFlushesBeforeL0 != 256 {
		t.Errorf("cap-only: max_wal_flushes_before_l0_flush = %d, want 256", c.MaxWALFlushesBeforeL0)
	}
	if !c.GC.WALFence.DryRun {
		t.Error("cap-only: wal_fence_options.dry_run = false, want the untouched slatedb default true")
	}

	// Both knobs off: no Settings at all (slatedb defaults, the unchanged
	// path for short-lived callers - tests, the migration tool).
	off, err := newSlatedbSettings(false, 0)
	if err != nil {
		t.Fatalf("newSlatedbSettings(false, 0): %v", err)
	}
	if off != nil {
		off.Destroy()
		t.Fatal("newSlatedbSettings(false, 0) = non-nil Settings, want nil (leave slatedb defaults untouched)")
	}
}
