package storage_test

// Backend-agnostic conformance suite for durable.Log: the same assertions run
// against every implementation, so the port's contract is pinned by test rather
// than by the comments on the interface.
//
// The contract these pin is stated in internal/durable/intent.go: every method
// safe to call twice, Complete on an absent intent a no-op, Advance past a
// recorded step a no-op, Outstanding oldest-first and scope-bounded. Callers
// rely on all of it - the boot sweep and the resolve-on-read path can run
// concurrently against the same intent, and converge only if repetition is
// harmless.
//
// House rules match conformance_test.go:
//   - Reach an implementation ONLY through durable.Log, never a concrete type,
//     so the suite cannot pin a behavior one implementation has and another
//     lacks.
//   - Fixed IDs, scopes and timestamps, so runs are deterministic.
//   - Timestamps are a fixed UTC instant with no sub-second component. A stored
//     implementation round-trips StartedAt through JSON, which drops the
//     monotonic clock, so a time.Now() value would fail comparison for a reason
//     that is not the contract.

import (
	"context"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/durable"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

type intentLogFactory func(t *testing.T) durable.Log

// base is a fixed instant so ordering assertions do not depend on wall clock.
var intentBase = time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

func intent(id, scope string, at time.Time) durable.Intent {
	return durable.Intent{
		ID:        durable.ID(id),
		Kind:      durable.KindCreatePaste,
		Scope:     durable.Scope(scope),
		Subject:   "subject-" + id,
		Guard:     []byte("guard-" + id),
		StartedAt: at,
	}
}

func mustOutstanding(t *testing.T, lg durable.Log, scope string) []durable.Intent {
	t.Helper()
	out, err := lg.Outstanding(context.Background(), durable.Scope(scope))
	if err != nil {
		t.Fatalf("Outstanding(%s): %v", scope, err)
	}
	return out
}

func runIntentLogConformance(t *testing.T, name string, newLog intentLogFactory) {
	ctx := context.Background()

	t.Run(name+"/BeginMakesItOutstanding", func(t *testing.T) {
		lg := newLog(t)
		in := intent("a1", "owner-1", intentBase)
		if err := lg.Begin(ctx, in); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		out := mustOutstanding(t, lg, "owner-1")
		if len(out) != 1 || out[0].ID != in.ID {
			t.Fatalf("Outstanding = %+v; want exactly the begun intent", out)
		}
	})

	// An operation retried under the same ID must resolve to the SAME intent
	// rather than accumulating duplicates - the reason callers assign the ID.
	t.Run(name+"/BeginUnderSameIDDoesNotDuplicate", func(t *testing.T) {
		lg := newLog(t)
		in := intent("a1", "owner-1", intentBase)
		for i := range 3 {
			if err := lg.Begin(ctx, in); err != nil {
				t.Fatalf("Begin #%d: %v", i, err)
			}
		}
		if out := mustOutstanding(t, lg, "owner-1"); len(out) != 1 {
			t.Fatalf("Outstanding = %d intents; want 1 after repeated Begin", len(out))
		}
	})

	t.Run(name+"/AdvanceRecordsTheStep", func(t *testing.T) {
		lg := newLog(t)
		in := intent("a1", "owner-1", intentBase)
		if err := lg.Begin(ctx, in); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := lg.Advance(ctx, in.ID, in.Scope, storage.StepEntryWritten); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		out := mustOutstanding(t, lg, "owner-1")
		if len(out) != 1 || !out[0].HasReached(storage.StepEntryWritten) {
			t.Fatalf("intent %+v; want HasReached(%q)", out, storage.StepEntryWritten)
		}
	})

	// "Advancing past a step already recorded is a no-op" - it must not append
	// the step twice, or a resolver reading Reached sees phantom progress.
	t.Run(name+"/AdvanceIsIdempotent", func(t *testing.T) {
		lg := newLog(t)
		in := intent("a1", "owner-1", intentBase)
		if err := lg.Begin(ctx, in); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		for i := range 3 {
			if err := lg.Advance(ctx, in.ID, in.Scope, storage.StepEntryWritten); err != nil {
				t.Fatalf("Advance #%d: %v", i, err)
			}
		}
		out := mustOutstanding(t, lg, "owner-1")
		if len(out) != 1 {
			t.Fatalf("Outstanding = %d; want 1", len(out))
		}
		var n int
		for _, s := range out[0].Reached {
			if s == storage.StepEntryWritten {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("step recorded %d times; want exactly 1 (Reached=%v)", n, out[0].Reached)
		}
	})

	// A resolver may advance an intent another resolver just completed. That is
	// a race, not a failure.
	t.Run(name+"/AdvanceOnAbsentIntentIsNotAnError", func(t *testing.T) {
		lg := newLog(t)
		if err := lg.Advance(ctx, durable.ID("never-begun"), durable.Scope("owner-1"), storage.StepEntryWritten); err != nil {
			t.Fatalf("Advance on absent intent = %v; want nil", err)
		}
	})

	t.Run(name+"/CompleteForgets", func(t *testing.T) {
		lg := newLog(t)
		in := intent("a1", "owner-1", intentBase)
		if err := lg.Begin(ctx, in); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := lg.Complete(ctx, in.ID, in.Scope); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if out := mustOutstanding(t, lg, "owner-1"); len(out) != 0 {
			t.Fatalf("Outstanding = %+v; want empty after Complete", out)
		}
	})

	// "Forgetting an intent twice is normal when two resolvers race."
	t.Run(name+"/CompleteIsIdempotent", func(t *testing.T) {
		lg := newLog(t)
		in := intent("a1", "owner-1", intentBase)
		if err := lg.Begin(ctx, in); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		for i := range 3 {
			if err := lg.Complete(ctx, in.ID, in.Scope); err != nil {
				t.Fatalf("Complete #%d = %v; want nil", i, err)
			}
		}
		if err := lg.Complete(ctx, durable.ID("never-begun"), in.Scope); err != nil {
			t.Fatalf("Complete on absent intent = %v; want nil", err)
		}
	})

	t.Run(name+"/OutstandingIsOldestFirst", func(t *testing.T) {
		lg := newLog(t)
		// Inserted newest-first so a pass-through of insertion order fails.
		for i, id := range []string{"c3", "b2", "a1"} {
			at := intentBase.Add(time.Duration(2-i) * time.Hour)
			if err := lg.Begin(ctx, intent(id, "owner-1", at)); err != nil {
				t.Fatalf("Begin %s: %v", id, err)
			}
		}
		out := mustOutstanding(t, lg, "owner-1")
		if len(out) != 3 {
			t.Fatalf("Outstanding = %d; want 3", len(out))
		}
		for i := 1; i < len(out); i++ {
			if out[i].StartedAt.Before(out[i-1].StartedAt) {
				t.Fatalf("not oldest-first: %v then %v", out[i-1].StartedAt, out[i].StartedAt)
			}
		}
	})

	// The load-bearing scoping property: Outstanding is the ONLY read and it is
	// scoped deliberately. A global enumeration would force a keyspace-wide scan
	// on a sharded store, so a scope leaking another scope's intents is not a
	// cosmetic bug - it is the cost model breaking.
	t.Run(name+"/OutstandingIsScopeBounded", func(t *testing.T) {
		lg := newLog(t)
		if err := lg.Begin(ctx, intent("a1", "owner-1", intentBase)); err != nil {
			t.Fatalf("Begin owner-1: %v", err)
		}
		if err := lg.Begin(ctx, intent("b2", "owner-2", intentBase)); err != nil {
			t.Fatalf("Begin owner-2: %v", err)
		}
		for _, tc := range []struct{ scope, wantID string }{{"owner-1", "a1"}, {"owner-2", "b2"}} {
			out := mustOutstanding(t, lg, tc.scope)
			if len(out) != 1 || string(out[0].ID) != tc.wantID {
				t.Fatalf("Outstanding(%s) = %+v; want exactly %s", tc.scope, out, tc.wantID)
			}
		}
		if out := mustOutstanding(t, lg, "owner-absent"); len(out) != 0 {
			t.Fatalf("Outstanding(unknown scope) = %+v; want empty", out)
		}
	})

	// Guard is what a compensating delete compares against before removing
	// anything, and StartedAt is what the grace is measured from. Both must
	// survive storage or recovery makes the wrong decision.
	t.Run(name+"/FieldsSurviveStorage", func(t *testing.T) {
		lg := newLog(t)
		in := intent("a1", "owner-1", intentBase)
		if err := lg.Begin(ctx, in); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		out := mustOutstanding(t, lg, "owner-1")
		if len(out) != 1 {
			t.Fatalf("Outstanding = %d; want 1", len(out))
		}
		got := out[0]
		if got.Kind != in.Kind || got.Subject != in.Subject {
			t.Fatalf("Kind/Subject = %q/%q; want %q/%q", got.Kind, got.Subject, in.Kind, in.Subject)
		}
		if string(got.Guard) != string(in.Guard) {
			t.Fatalf("Guard = %q; want %q", got.Guard, in.Guard)
		}
		if !got.StartedAt.Equal(in.StartedAt) {
			t.Fatalf("StartedAt = %v; want %v", got.StartedAt, in.StartedAt)
		}
		if got.Scope != in.Scope {
			t.Fatalf("Scope = %q; want %q", got.Scope, in.Scope)
		}
	})

	// The property the boot sweep and resolve-on-read both depend on, and the
	// one the mount hook makes load-bearing: running the whole lifecycle twice
	// converges instead of accumulating or erroring.
	t.Run(name+"/LifecycleRunTwiceConverges", func(t *testing.T) {
		lg := newLog(t)
		in := intent("a1", "owner-1", intentBase)
		for pass := range 2 {
			if err := lg.Begin(ctx, in); err != nil {
				t.Fatalf("pass %d Begin: %v", pass, err)
			}
			if err := lg.Advance(ctx, in.ID, in.Scope, storage.StepEntryWritten); err != nil {
				t.Fatalf("pass %d Advance: %v", pass, err)
			}
			if err := lg.Complete(ctx, in.ID, in.Scope); err != nil {
				t.Fatalf("pass %d Complete: %v", pass, err)
			}
			if out := mustOutstanding(t, lg, "owner-1"); len(out) != 0 {
				t.Fatalf("pass %d: Outstanding = %+v; want empty", pass, out)
			}
		}
	})
}

func TestIntentLogConformance_Mem(t *testing.T) {
	runIntentLogConformance(t, "mem", func(t *testing.T) durable.Log {
		return durable.NewMemLog()
	})
}

// The stored implementation, on whatever engine the build selects - the same
// seam storagetest.NewRepo uses for the storage conformance suite.
func TestIntentLogConformance_Shale(t *testing.T) {
	runIntentLogConformance(t, "shale", func(t *testing.T) durable.Log {
		repo := storagetest.NewRepo(t)
		return storage.NewShaleIntentLog(repo.ClusterForTest(), nil)
	})
}
