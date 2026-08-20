package storage

// Pins the storage-boundary translation of shale's cross-shard guard sentinel
// into the domain vocabulary (translateCrossShard in crossshard.go). No
// cluster is needed: the guard fires client-side in the tx buffer before any
// commit, and every layer between it and the deploy service's isCrossShard
// check passes the error through verbatim or %w-wraps it, so error identity is
// what carries. The service-side half of the pin lives in
// internal/service/typed_errors_test.go.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestTranslateCrossShard(t *testing.T) {
	t.Run("guard error gains the domain sentinel, keeps the backend one", func(t *testing.T) {
		// The shape the guard actually surfaces: backend.ErrCrossShard from a
		// buffered tx op, sometimes %w-wrapped by the tx body on its way out.
		for _, in := range []error{
			backend.ErrCrossShard,
			fmt.Errorf("bind blob: %w", backend.ErrCrossShard),
		} {
			out := translateCrossShard(in)
			if !errors.Is(out, domain.ErrCrossShardDeploy) {
				t.Fatalf("translateCrossShard(%v) = %v; want domain.ErrCrossShardDeploy in the chain", in, out)
			}
			if !errors.Is(out, backend.ErrCrossShard) {
				t.Fatalf("translateCrossShard(%v) = %v; the backend sentinel must stay in the chain", in, out)
			}
			// The operator log must show what the backend actually said.
			if want := in.Error(); !strings.Contains(out.Error(), want) {
				t.Fatalf("translateCrossShard(%v) message %q lost the original %q", in, out.Error(), want)
			}
		}
	})
	t.Run("non-guard errors pass through untouched", func(t *testing.T) {
		other := errors.New("some other failure")
		if got := translateCrossShard(other); got != other {
			t.Fatalf("translateCrossShard(other) = %v, want the same value back", got)
		}
		if got := translateCrossShard(nil); got != nil {
			t.Fatalf("translateCrossShard(nil) = %v, want nil", got)
		}
	})
}
