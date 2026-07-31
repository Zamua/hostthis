package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// parkingDurable fails every Put and parks the Put of one chosen sha until
// released, so a single uploader worker can be pinned while the queue fills.
type parkingDurable struct {
	mu      sync.Mutex
	calls   map[string]int
	parkSha string
	park    chan struct{}
	started chan struct{}
	once    sync.Once
}

func newParkingDurable(parkSha string) *parkingDurable {
	return &parkingDurable{
		calls:   map[string]int{},
		parkSha: parkSha,
		park:    make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (p *parkingDurable) Put(sha string, r io.Reader, _ int64) error {
	_, _ = io.Copy(io.Discard, r)
	p.mu.Lock()
	p.calls[sha]++
	p.mu.Unlock()
	if sha == p.parkSha {
		p.once.Do(func() { close(p.started) })
		<-p.park
	}
	return errors.New("durable down")
}

func (p *parkingDurable) Get(string) ([]byte, error) { return nil, ErrNotFound }

func (p *parkingDurable) GetReader(string) (io.ReadCloser, int64, error) {
	return nil, 0, ErrNotFound
}

func (p *parkingDurable) WalkBlobs(func(sha string) error) error { return nil }

func (p *parkingDurable) Remove(string) error { return nil }

func (p *parkingDurable) count(sha string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[sha]
}

// A Put that meets a full queue and a failing backend retries a bounded number
// of times, and no retry outlives Close.
func TestWriteBack_QueueFullRetryIsBoundedAndStopsAtClose(t *testing.T) {
	parked := strings.Repeat("a", 64)
	dur := newParkingDurable(parked)
	release := sync.OnceFunc(func() { close(dur.park) })
	defer release()

	wb := newTestWriteBack(t, dur, WriteBackConfig{Workers: 1})

	// Pin the only worker inside a durable Put so nothing drains the queue.
	if err := wb.writeLocal(parked, []byte("parked")); err != nil {
		t.Fatalf("writeLocal parked: %v", err)
	}
	wb.enqueue(parked)
	select {
	case <-dur.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never entered the parked upload")
	}

	// Fill the queue behind the parked worker. These shas have no local file,
	// so any that do get drained complete instantly.
	for i := range cap(wb.queue) {
		wb.enqueue(fmt.Sprintf("%064x", i))
	}

	// This one meets the full queue; its synchronous attempt fails.
	target := strings.Repeat("b", 64)
	if err := wb.writeLocal(target, []byte("target")); err != nil {
		t.Fatalf("writeLocal target: %v", err)
	}
	wb.enqueue(target)

	time.Sleep(300 * time.Millisecond)
	const maxAttempts = 20 // one synchronous attempt plus the workers' bounded retry
	if n := dur.count(target); n > maxAttempts {
		t.Fatalf("queue-full retry is unbounded: %d durable Put attempts for one blob in 300ms", n)
	}

	release() // let the parked worker go so Close can finish
	wb.Close()
	after := dur.count(target)
	time.Sleep(150 * time.Millisecond)
	if n := dur.count(target); n != after {
		t.Fatalf("retry goroutines survived Close: attempts went %d -> %d", after, n)
	}
}

// Names too short to carry a shard prefix are skipped rather than sliced, on
// every path that derives a cache path from a caller- or disk-supplied name.
func TestWriteBack_ShortNamesDoNotPanic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("junk"), 0o640); err != nil {
		t.Fatal(err)
	}

	// Construction rescans the cache dir and must survive the stray file.
	wb := newTestWriteBack(t, newFakeDurable(), WriteBackConfig{Dir: dir})

	if _, _, err := wb.scanEntries(); err != nil {
		t.Fatalf("scanEntries: %v", err)
	}
	wb.evictIfNeeded()
	if err := wb.Remove("x"); err != nil {
		t.Fatalf("Remove(short sha): %v", err)
	}
}

// A Put well under the byte cap does not walk the whole cache directory.
func TestWriteBack_UnderCapPutSkipsEvictionWalk(t *testing.T) {
	dir := t.TempDir()
	// An unreadable subdirectory makes any full walk fail observably. "zz" is
	// not hex, so it can never collide with a real shard prefix.
	blocked := filepath.Join(dir, "zz")
	if err := os.Mkdir(blocked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o750) })

	var logs bytes.Buffer
	wb := newTestWriteBack(t, newFakeDurable(), WriteBackConfig{Dir: dir, Logger: log.New(&logs, "", 0)})

	if _, _, err := wb.scanEntries(); err == nil {
		t.Skip("the unreadable directory is still walkable (running as root?); a skipped walk is not observable here")
	}
	logs.Reset()

	body := []byte("well under the cap")
	if err := wb.PutPrecompressed(wbShaOf(body), body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if strings.Contains(logs.String(), "eviction scan error") {
		t.Fatalf("Put walked the whole cache dir while far under the cap: %q", logs.String())
	}
}
