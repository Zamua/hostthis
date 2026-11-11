package ssh_test

import (
	"context"
	"errors"
	"fmt"
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
		if _, err := hostssh.NewUploadAdmission(tc.limit, tc.limit, tc.wait); err == nil {
			t.Errorf("NewUploadAdmission(%d, %s) = nil error, want refusal", tc.limit, tc.wait)
		}
	}
}

// A slot frees for the next waiter on release; a wait past the limit is busy.
func TestUploadAdmission_WaitLimitIsStoreBusy(t *testing.T) {
	a, err := hostssh.NewUploadAdmission(1, 1, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	release, err := a.Acquire(context.Background(), testOwner(t, 1))
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := a.Acquire(context.Background(), testOwner(t, 2)); !errors.Is(err, domain.ErrStoreBusy) {
		t.Fatalf("acquire while full = %v, want ErrStoreBusy", err)
	}
	release()
	release() // idempotent: a double release must not free a second slot
	r2, err := a.Acquire(context.Background(), testOwner(t, 3))
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if _, err := a.Acquire(context.Background(), testOwner(t, 4)); !errors.Is(err, domain.ErrStoreBusy) {
		t.Fatalf("double release freed an extra slot: %v", err)
	}
	r2()
}

// A cancelled caller leaves the queue with its context error, not busy.
func TestUploadAdmission_CancelledWaiterLeaves(t *testing.T) {
	a, err := hostssh.NewUploadAdmission(1, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	release, _ := a.Acquire(context.Background(), testOwner(t, 5))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Acquire(ctx, testOwner(t, 6))
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
	r, err := a.Acquire(context.Background(), testOwner(t, 7))
	if err != nil {
		t.Fatalf("acquire after cancelled waiter: %v", err)
	}
	r()
}

// Waiters are admitted in arrival order.
func TestUploadAdmission_FIFO(t *testing.T) {
	a, err := hostssh.NewUploadAdmission(1, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	release, _ := a.Acquire(context.Background(), testOwner(t, 8))
	order := make(chan int, 3)
	for i := range 3 {
		go func() {
			r, err := a.Acquire(context.Background(), testOwner(t, 100+i))
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

// testOwner is a distinct owner per n.
func testOwner(t *testing.T, n int) domain.Owner {
	t.Helper()
	owner, err := domain.OwnerFromKeyFingerprint(fmt.Sprintf("SHA256:%064x", n))
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

// One owner holds or waits for at most its limit; the next is refused at
// once, other owners are unaffected, and a release frees the owner's place.
func TestUploadAdmission_PerOwnerLimit(t *testing.T) {
	if _, err := hostssh.NewUploadAdmission(2, 3, time.Second); err == nil {
		t.Fatal("per-owner limit above the limit accepted")
	}
	if _, err := hostssh.NewUploadAdmission(2, 0, time.Second); err == nil {
		t.Fatal("per-owner limit 0 accepted")
	}
	a, err := hostssh.NewUploadAdmission(3, 2, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	alice, bob := testOwner(t, 1001), testOwner(t, 1002)
	r1, err := a.Acquire(context.Background(), alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Acquire(context.Background(), alice); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var busy *hostssh.OwnerUploadsError
	if _, err := a.Acquire(context.Background(), alice); !errors.As(err, &busy) || busy.Limit != 2 {
		t.Fatalf("third upload for one owner: %v, want OwnerUploadsError{2}", err)
	}
	if time.Since(start) > 40*time.Millisecond {
		t.Fatal("the per-owner refusal waited instead of answering at once")
	}
	rb, err := a.Acquire(context.Background(), bob)
	if err != nil {
		t.Fatalf("another owner beside a capped one: %v", err)
	}
	// Bob waiting for a full gate counts against bob, and is given back.
	if _, err := a.Acquire(context.Background(), bob); !errors.Is(err, domain.ErrStoreBusy) {
		t.Fatalf("bob's second upload with the gate full: %v, want ErrStoreBusy", err)
	}
	rb()
	r1()
	r1()
	if _, err := a.Acquire(context.Background(), alice); err != nil {
		t.Fatalf("after a release: %v", err)
	}
	if _, err := a.Acquire(context.Background(), bob); err != nil {
		t.Fatalf("bob after his release and failed wait: %v", err)
	}
}
