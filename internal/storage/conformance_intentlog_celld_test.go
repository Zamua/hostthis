package storage_test

// The celld entry into the durable.Log conformance suite.
//
// Skipped unless CELLD_TEST_ENDPOINT names a running fleet, the same shape the
// slatedb-backed tests use for MinIO: the default build must stay runnable with
// nothing installed. When it IS set, this runs the identical assertions the
// in-memory and shale implementations run, unmodified, which is the whole point
// of the experiment (docs/design/celld-migration.md section 7).
//
// Each subtest gets a fresh scope so runs do not observe each other's intents.
// A celld cell is durable and there is no teardown hook, so isolation comes
// from naming rather than from cleaning up.

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/durable"
)

var celldScopeSeq atomic.Int64

// scopedLog gives each subtest its own scope prefix, so "owner-1" in one
// subtest is a different cell from "owner-1" in the next.
type scopedLog struct {
	inner  durable.Log
	prefix string
}

func (s scopedLog) scope(in durable.Scope) durable.Scope {
	return durable.Scope(s.prefix + string(in))
}

func (s scopedLog) Begin(ctx context.Context, in durable.Intent) error {
	in.Scope = s.scope(in.Scope)
	return s.inner.Begin(ctx, in)
}

func (s scopedLog) Advance(ctx context.Context, id durable.ID, sc durable.Scope, step durable.StepName) error {
	return s.inner.Advance(ctx, id, s.scope(sc), step)
}

func (s scopedLog) Complete(ctx context.Context, id durable.ID, sc durable.Scope) error {
	return s.inner.Complete(ctx, id, s.scope(sc))
}

func (s scopedLog) Outstanding(ctx context.Context, sc durable.Scope) ([]durable.Intent, error) {
	got, err := s.inner.Outstanding(ctx, s.scope(sc))
	if err != nil {
		return nil, err
	}
	// Hand back the scope the caller asked for, not the prefixed one, or the
	// suite's field assertions would see the harness rather than the adapter.
	for i := range got {
		got[i].Scope = sc
	}
	return got, nil
}

func TestIntentLogConformance_Celld(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld intent-log conformance")
	}
	runIntentLogConformance(t, "celld", func(t *testing.T) durable.Log {
		return scopedLog{
			inner:  celld.NewIntentLog(base, nil),
			prefix: fmt.Sprintf("t%d-", celldScopeSeq.Add(1)),
		}
	})
}
