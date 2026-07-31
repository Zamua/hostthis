package service_test

import (
	"bytes"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// A ref whose delete ERRORED is retried on the next pass. The convergence guard
// classifies a resurfaced ref as unreachable, which is only sound when the
// processing that preceded it reported success; a transient backend error must
// not suppress that record's expiry for the process lifetime.
func TestSweep_FailedDeleteRetriedNextPass(t *testing.T) {
	repo := &flakySweepRepo{
		ref:      domain.ExpiredPaste{Slug: "8ajitdpm", IndexRef: "expiry/2026-07-01T03:20:59Z/8ajitdpm"},
		failNext: true,
	}

	var logbuf bytes.Buffer
	sweep := service.NewSweep(repo, nil, log.New(&logbuf, "", 0))
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	// Pass 1: the delete fails, so the pass aborts with the error.
	if _, _, err := sweep.Once(now); err == nil {
		t.Fatal("pass 1 should surface the delete error")
	}
	if repo.deleteCalls != 1 {
		t.Fatalf("pass 1 should attempt the ref once, got %d calls", repo.deleteCalls)
	}

	// Pass 2: the backend recovered, so the ref must be retried and drain.
	logbuf.Reset()
	deleted, _, err := sweep.Once(now)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if repo.deleteCalls != 2 {
		t.Fatalf("a ref whose delete ERRORED must be retried: want 2 total calls, got %d", repo.deleteCalls)
	}
	if deleted != 1 {
		t.Fatalf("the retried delete should count as a deletion, got %d", deleted)
	}
	if bytes.Contains(logbuf.Bytes(), []byte("unreachable")) {
		t.Fatalf("a failed delete is not an unreachable ref; got:\n%s", logbuf.String())
	}

	// Pass 3: it really drained.
	if _, _, err := sweep.Once(now); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if repo.deleteCalls != 2 {
		t.Fatalf("drained ref must not resurface: want 2 total calls, got %d", repo.deleteCalls)
	}
}

// flakySweepRepo fails its first DeleteExpired with a transient error, then
// succeeds and drains the entry.
type flakySweepRepo struct {
	ref         domain.ExpiredPaste
	gone        bool
	failNext    bool
	deleteCalls int
}

func (r *flakySweepRepo) ExpiredPastes(_ time.Time) ([]domain.ExpiredPaste, error) {
	if r.gone {
		return nil, nil
	}
	return []domain.ExpiredPaste{r.ref}, nil
}

func (r *flakySweepRepo) DeleteExpired(_ domain.ExpiredPaste) (bool, error) {
	r.deleteCalls++
	if r.failNext {
		r.failNext = false
		return false, errors.New("transient backend error")
	}
	r.gone = true
	return true, nil
}

func (r *flakySweepRepo) ReferencedBlobSHAs() ([]string, error) { return []string{"sha-x"}, nil }
