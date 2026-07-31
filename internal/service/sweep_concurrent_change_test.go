package service_test

import (
	"log"
	"sort"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// contendedSweepRepo fails exactly one slug's cascade with ErrConcurrentChange,
// the way a repo does when an append lands mid-delete, and drains the rest.
type contendedSweepRepo struct {
	slugs     []string
	contended string
	processed []string
}

func (r *contendedSweepRepo) ExpiredPastes(_ time.Time) ([]domain.ExpiredPaste, error) {
	out := make([]domain.ExpiredPaste, 0, len(r.slugs))
	for _, s := range r.slugs {
		out = append(out, domain.ExpiredPaste{Slug: domain.Slug(s), IndexRef: "expiry/" + s})
	}
	return out, nil
}

func (r *contendedSweepRepo) DeleteExpired(ref domain.ExpiredPaste) (bool, error) {
	if ref.Slug.String() == r.contended {
		// Nothing applied, and the expiry entry still stands.
		return false, domain.ErrConcurrentChange
	}
	r.processed = append(r.processed, ref.Slug.String())
	return true, nil
}

func (r *contendedSweepRepo) ReferencedBlobSHAs() ([]string, error) {
	return []string{"sha-live"}, nil
}

// One contended record must not strand the rest of the batch. The scan order is
// map-derived, so the contended slug can land anywhere in it; the property is
// that every other ref is processed no matter where it falls.
func TestSweep_ConcurrentChangeSkipsRefAndContinuesBatch(t *testing.T) {
	repo := &contendedSweepRepo{
		slugs:     []string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd"},
		contended: "bbbbbbbb",
	}
	sweep := service.NewSweep(repo, nil, log.New(&testWriter{t}, "", 0))

	deleted, _, err := sweep.Once(time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("a contended ref must not fail the pass: %v", err)
	}

	sort.Strings(repo.processed)
	want := []string{"aaaaaaaa", "cccccccc", "dddddddd"}
	if len(repo.processed) != len(want) {
		t.Fatalf("processed %v, want every uncontended ref %v: one contended record "+
			"stranded %d other expired paste(s) behind it",
			repo.processed, want, len(want)-len(repo.processed))
	}
	for i := range want {
		if repo.processed[i] != want[i] {
			t.Fatalf("processed %v, want %v", repo.processed, want)
		}
	}
	// The contended ref applied nothing, so counting it deleted would report a
	// reclaim that did not happen.
	if deleted != len(want) {
		t.Fatalf("deleted = %d, want %d (the contended ref must not count)", deleted, len(want))
	}
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) { return len(p), nil }
