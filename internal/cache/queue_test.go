package cache

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

type enqueued struct {
	Slugs  []string `json:"slugs"`
	Urgent bool     `json:"urgent"`
}

// worker records each enqueue and answers with the next scripted status, 200
// once the script runs out.
type worker struct {
	mu      sync.Mutex
	script  []int
	got     []enqueued
	release chan struct{} // when set, every answer waits for it
}

func (w *worker) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if w.release != nil {
		<-w.release
	}
	var e enqueued
	_ = json.NewDecoder(r.Body).Decode(&e)
	sort.Strings(e.Slugs)
	w.mu.Lock()
	status := http.StatusOK
	if len(w.script) > 0 {
		status, w.script = w.script[0], w.script[1:]
	}
	if status == http.StatusOK {
		w.got = append(w.got, e)
	}
	w.mu.Unlock()
	if r.URL.Path != "/purge/enqueue" || r.Method != http.MethodPost {
		status = http.StatusNotFound
	}
	rw.WriteHeader(status)
}

func (w *worker) taken() []enqueued {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]enqueued(nil), w.got...)
}

func newQueue(t *testing.T, w *worker) *Queue {
	t.Helper()
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	return &Queue{Endpoint: srv.URL + "/", Backoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond}
}

func flush(t *testing.T, q *Queue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestQueueEnqueuesToTheWorker(t *testing.T) {
	w := &worker{}
	q := newQueue(t, w)
	if err := q.PurgePaste("abcd2345"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	flush(t, q)
	got := w.taken()
	if len(got) != 1 || len(got[0].Slugs) != 1 || got[0].Slugs[0] != "abcd2345" || got[0].Urgent {
		t.Fatalf("enqueued %+v, want one ordinary purge of the slug", got)
	}
}

// A refused or failed enqueue is offered again until the Worker takes it.
func TestQueueRetriesUntilTaken(t *testing.T) {
	w := &worker{script: []int{429, 500, 507, 503}}
	q := newQueue(t, w)
	_ = q.PurgeRemoved("abcd2345")
	flush(t, q)
	got := w.taken()
	if len(got) != 1 || !got[0].Urgent || got[0].Slugs[0] != "abcd2345" {
		t.Fatalf("enqueued %+v, want the removal once, after the failures", got)
	}
}

// Removals go out ahead of changes, and a slug waiting as both goes once, as
// a removal.
func TestQueueSendsRemovalsFirst(t *testing.T) {
	w := &worker{release: make(chan struct{})}
	q := newQueue(t, w)
	_ = q.PurgePaste("aaaa2345") // taken by the loop, held at the worker
	deadline := time.Now().Add(5 * time.Second)
	for q.pending() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first purge was never taken")
		}
		time.Sleep(time.Millisecond)
	}
	_ = q.PurgePaste("bbbb2345")
	_ = q.PurgePaste("cccc2345")
	_ = q.PurgeRemoved("dddd2345")
	_ = q.PurgeRemoved("cccc2345")
	close(w.release)
	flush(t, q)
	got := w.taken()
	want := []enqueued{
		{[]string{"aaaa2345"}, false},
		{[]string{"cccc2345", "dddd2345"}, true},
		{[]string{"bbbb2345"}, false},
	}
	if len(got) != len(want) {
		t.Fatalf("enqueued %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Urgent != want[i].Urgent || len(got[i].Slugs) != len(want[i].Slugs) {
			t.Fatalf("enqueue %d = %+v, want %+v", i, got[i], want[i])
		}
		for j := range want[i].Slugs {
			if got[i].Slugs[j] != want[i].Slugs[j] {
				t.Fatalf("enqueue %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	}
}

func TestQueueSplitsLargeBacklogs(t *testing.T) {
	w := &worker{release: make(chan struct{})}
	q := newQueue(t, w)
	for i := range 250 {
		_ = q.PurgePaste(domain.Slug("s" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "22345"))
	}
	close(w.release)
	flush(t, q)
	total := 0
	for _, e := range w.taken() {
		if len(e.Slugs) > enqueueMax {
			t.Fatalf("one enqueue carried %d slugs", len(e.Slugs))
		}
		total += len(e.Slugs)
	}
	if total != 250 {
		t.Fatalf("enqueued %d slugs, want 250", total)
	}
}

// A request the Worker calls malformed is dropped, not retried forever.
func TestQueueDropsAMalformedEnqueue(t *testing.T) {
	w := &worker{script: []int{400}}
	q := newQueue(t, w)
	_ = q.PurgePaste("abcd2345")
	flush(t, q)
	if got := w.taken(); len(got) != 0 {
		t.Fatalf("enqueued %+v after a 400", got)
	}
}

func TestQueueShutdown(t *testing.T) {
	q := newQueue(t, &worker{})
	flush(t, q) // never used: nothing to stop
	if err := q.PurgePaste("abcd2345"); !errors.Is(err, ErrClosed) {
		t.Fatalf("purge after shutdown = %v, want ErrClosed", err)
	}

	// A Worker that never takes the purge: Shutdown gives up at its deadline.
	down := &worker{script: make([]int, 100000)}
	for i := range down.script {
		down.script[i] = 503
	}
	q = newQueue(t, down)
	_ = q.PurgePaste("abcd2345")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := q.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown = %v, want the deadline", err)
	}
}
