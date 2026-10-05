package ssh_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
)

func TestNewUploadAdmission_RejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		limit int
		wait  time.Duration
	}{{0, time.Second}, {-1, time.Second}, {1, 0}, {1, -time.Second}} {
		if _, err := hostssh.NewUploadAdmission(tc.limit, tc.wait); err == nil {
			t.Errorf("NewUploadAdmission(%d, %s) = nil error, want refusal", tc.limit, tc.wait)
		}
	}
}

// A slot frees for the next waiter on release; a wait past the limit is busy.
func TestUploadAdmission_WaitLimitIsStoreBusy(t *testing.T) {
	a, err := hostssh.NewUploadAdmission(1, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	release, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := a.Acquire(context.Background()); !errors.Is(err, domain.ErrStoreBusy) {
		t.Fatalf("acquire while full = %v, want ErrStoreBusy", err)
	}
	release()
	release() // idempotent: a double release must not free a second slot
	r2, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if _, err := a.Acquire(context.Background()); !errors.Is(err, domain.ErrStoreBusy) {
		t.Fatalf("double release freed an extra slot: %v", err)
	}
	r2()
}

// A cancelled caller leaves the queue with its context error, not busy.
func TestUploadAdmission_CancelledWaiterLeaves(t *testing.T) {
	a, err := hostssh.NewUploadAdmission(1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	release, _ := a.Acquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Acquire(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled acquire = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not leave the queue")
	}
	release()
	r, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after cancelled waiter: %v", err)
	}
	r()
}

// Waiters are admitted in arrival order.
func TestUploadAdmission_FIFO(t *testing.T) {
	a, err := hostssh.NewUploadAdmission(1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	release, _ := a.Acquire(context.Background())
	order := make(chan int, 3)
	for i := range 3 {
		go func() {
			r, err := a.Acquire(context.Background())
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			order <- i
			r()
		}()
		waitFor(t, func() bool { return a.Waiting() == i+1 })
	}
	release()
	for want := range 3 {
		if got := <-order; got != want {
			t.Fatalf("admitted waiter %d, want %d", got, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}
