package storage_test

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// gateProbe is an inner store whose Put records the weight in flight and
// blocks until released.
type gateProbe struct {
	mu        sync.Mutex
	inFlight  int64
	highWater int64
	release   chan struct{}
	entered   chan int64
	gets      atomic.Int32
	deletes   atomic.Int32
}

func newGateProbe() *gateProbe {
	return &gateProbe{release: make(chan struct{}), entered: make(chan int64, 64)}
}

func (p *gateProbe) Put(_ string, r io.Reader, size int64) error {
	p.mu.Lock()
	p.inFlight += size
	p.highWater = max(p.highWater, p.inFlight)
	p.mu.Unlock()
	p.entered <- size
	<-p.release
	_, err := io.Copy(io.Discard, r)
	p.mu.Lock()
	p.inFlight -= size
	p.mu.Unlock()
	return err
}

func (p *gateProbe) GetReader(string) (io.ReadCloser, int64, error) {
	p.gets.Add(1)
	return io.NopCloser(strings.NewReader("x")), 1, nil
}

func (p *gateProbe) DeletePrefix(string) error {
	p.deletes.Add(1)
	return nil
}

// readSpy records whether anything read from it.
type readSpy struct{ reads atomic.Int32 }

func (s *readSpy) Read([]byte) (int, error) {
	s.reads.Add(1)
	return 0, io.EOF
}

func newGate(t *testing.T, inner storage.InnerBlobStore, budget int64, wait time.Duration) *storage.GatedBlobStore {
	t.Helper()
	g, err := storage.NewGatedBlobStore(inner, budget, wait)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}
	return g
}

func putAsync(g *storage.GatedBlobStore, size int64) <-chan error {
	done := make(chan error, 1)
	go func() { done <- g.Put("uploads/x/0", strings.NewReader("body"), size) }()
	return done
}

func waitEntered(t *testing.T, p *gateProbe) int64 {
	t.Helper()
	select {
	case n := <-p.entered:
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("no put reached the inner store")
		return 0
	}
}

func assertNotEntered(t *testing.T, p *gateProbe, within time.Duration) {
	t.Helper()
	select {
	case n := <-p.entered:
		t.Fatalf("put of %d reached the inner store past the budget", n)
	case <-time.After(within):
	}
}

// Admitted Puts never weigh more than the budget together.
func TestGatedBlobStore_InFlightNeverExceedsBudget(t *testing.T) {
	p := newGateProbe()
	g := newGate(t, p, 100, 5*time.Second)
	var done []<-chan error
	for range 8 {
		done = append(done, putAsync(g, 40))
	}
	// 100/40: two fit, the rest wait.
	waitEntered(t, p)
	waitEntered(t, p)
	assertNotEntered(t, p, 50*time.Millisecond)
	close(p.release)
	for _, d := range done {
		if err := <-d; err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	if p.highWater > 100 {
		t.Fatalf("high-water in flight = %d, budget 100", p.highWater)
	}
}

// A waiting Put is admitted when an admitted one finishes.
func TestGatedBlobStore_WaiterAdmittedWhenSlotFrees(t *testing.T) {
	p := newGateProbe()
	g := newGate(t, p, 10, 5*time.Second)
	first := putAsync(g, 10)
	waitEntered(t, p)
	second := putAsync(g, 10)
	assertNotEntered(t, p, 30*time.Millisecond)

	p.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("first put: %v", err)
	}
	waitEntered(t, p)
	p.release <- struct{}{}
	if err := <-second; err != nil {
		t.Fatalf("second put: %v", err)
	}
}

// A Put not admitted within the wait fails with the busy sentinel and never
// reaches the inner store.
func TestGatedBlobStore_WaitExpiryIsBusy(t *testing.T) {
	p := newGateProbe()
	g := newGate(t, p, 10, 50*time.Millisecond)
	first := putAsync(g, 10)
	waitEntered(t, p)

	t.Cleanup(func() { close(p.release) })
	select {
	case err := <-putAsync(g, 1):
		if !errors.Is(err, domain.ErrStoreBusy) {
			t.Fatalf("put = %v, want ErrStoreBusy", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("put neither admitted nor refused")
	}
	assertNotEntered(t, p, 20*time.Millisecond)
	p.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("first put: %v", err)
	}
}

// A body larger than the budget, or of unknown size, is admitted alone.
func TestGatedBlobStore_OversizedAdmittedAlone(t *testing.T) {
	for _, size := range []int64{1000, -1} {
		p := newGateProbe()
		g := newGate(t, p, 10, 5*time.Second)
		big := putAsync(g, size)
		waitEntered(t, p)
		small := putAsync(g, 1)
		assertNotEntered(t, p, 30*time.Millisecond)
		close(p.release)
		if err := <-big; err != nil {
			t.Fatalf("size %d: put: %v", size, err)
		}
		if err := <-small; err != nil {
			t.Fatalf("size %d: small put: %v", size, err)
		}
	}
}

// Reads and deletes proceed while the budget is exhausted.
func TestGatedBlobStore_ReadsAndDeletesUngated(t *testing.T) {
	p := newGateProbe()
	g := newGate(t, p, 10, 5*time.Second)
	held := putAsync(g, 10)
	waitEntered(t, p)

	rc, _, err := g.GetReader("uploads/x/0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = rc.Close()
	if err := g.DeletePrefix("uploads/x/"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if p.gets.Load() != 1 || p.deletes.Load() != 1 {
		t.Fatalf("gets=%d deletes=%d, want 1 and 1", p.gets.Load(), p.deletes.Load())
	}
	close(p.release)
	if err := <-held; err != nil {
		t.Fatalf("held put: %v", err)
	}
}

// A waiting Put reads none of its body.
func TestGatedBlobStore_NoReadBeforeAdmission(t *testing.T) {
	p := newGateProbe()
	g := newGate(t, p, 10, 5*time.Second)
	held := putAsync(g, 10)
	waitEntered(t, p)

	spy := &readSpy{}
	waiting := make(chan error, 1)
	go func() { waiting <- g.Put("uploads/z/0", spy, 5) }()
	assertNotEntered(t, p, 30*time.Millisecond)
	if n := spy.reads.Load(); n != 0 {
		t.Fatalf("body read %d times before admission", n)
	}
	close(p.release)
	if err := <-held; err != nil {
		t.Fatalf("held put: %v", err)
	}
	if err := <-waiting; err != nil {
		t.Fatalf("waiting put: %v", err)
	}
	if spy.reads.Load() == 0 {
		t.Fatal("body never streamed after admission")
	}
}

// Construction refuses a budget below 1 or a non-positive wait.
func TestGatedBlobStore_RejectsBadConfig(t *testing.T) {
	if _, err := storage.NewGatedBlobStore(newGateProbe(), 0, time.Second); err == nil {
		t.Fatal("budget 0 accepted")
	}
	if _, err := storage.NewGatedBlobStore(newGateProbe(), 1, 0); err == nil {
		t.Fatal("wait 0 accepted")
	}
}
