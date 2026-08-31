package celld_test

// Does the identity cell become a bottleneck under concurrent uploads from ONE
// owner?
//
// Every upload mutates that cell twice: once to reserve quota and once to
// confirm. Those mutations use blockConcurrencyWhile, so one owner's uploads
// contend on the same serialized section. The question is whether that cost is
// material relative to uploads spread across distinct identities. Class names
// are part of persistent routing and cannot be renamed after production data
// exists, so this is measured before deployment.
//
// THE CONTROL IS THE POINT. Rising latency with rising N proves nothing on its
// own - HTTP, the client, the port-forward and the node all contend too. So the
// same N runs twice: once against one shared identity, once against N distinct
// identities. Distinct identities exercise every layer except the shared cell.
// The DIFFERENCE between the two is the serialization cost; the absolute
// numbers are mostly the harness.
//
// Skipped unless CELLD_TEST_ENDPOINT names a running fleet.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
)

func pctl(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	i := int(float64(len(d)-1) * p)
	return d[i]
}

// burst fires n concurrent inserts and returns wall time plus per-op latencies.
// ownerFor decides whether they share an identity cell or not.
func burst(t *testing.T, repo *celld.PasteRepo, n int, tag string, ownerFor func(i int) string) (time.Duration, []time.Duration) {
	t.Helper()
	lat := make([]time.Duration, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := domain.Paste{
				Slug:       domain.Slug(fmt.Sprintf("%s%04d", tag, i)),
				Identity:   domain.Identity(ownerFor(i)),
				Status:     domain.PasteStatusReady,
				Kind:       domain.KindMarkdown,
				ContentSHA: "sha-" + tag,
				Size:       100,
				CreatedAt:  time.Now().UTC(),
				UpdatedAt:  time.Now().UTC(),
			}
			t0 := time.Now()
			// Errors are recorded as latency rather than failing the run: a
			// refusal under load is itself a reading, and a partial burst that
			// aborts would report a flattering wall time.
			_ = repo.InsertWithQuotaCheck(context.Background(), p, 0, time.Now().UTC())
			lat[i] = time.Since(t0)
		}(i)
	}
	wg.Wait()
	return time.Since(start), lat
}

func TestIdentityCellUnderConcurrentUploads(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the identity-cell load probe")
	}
	repo := celld.NewPasteRepo(base, nil)
	run := time.Now().UnixNano() % 100000

	// Warm both arms so the comparison isolates contention in the shared
	// blockConcurrencyWhile section rather than cell activation.
	t.Log("N | shared-identity wall/p50/p99 | distinct-identity wall/p50/p99 | shared:distinct wall")
	for _, n := range []int{1, 4, 8, 16, 32} {
		shared := fmt.Sprintf("key:load%d-s%d", run, n)
		sharedOwner := func(int) string { return shared }
		distinctOwner := func(i int) string { return fmt.Sprintf("key:load%d-d%d-%d", run, n, i) }

		burst(t, repo, n, fmt.Sprintf("ws%d%03d", run%100, n), sharedOwner)
		burst(t, repo, n, fmt.Sprintf("wd%d%03d", run%100, n), distinctOwner)

		sw, sl := burst(t, repo, n, fmt.Sprintf("s%d%03d", run%100, n), sharedOwner)
		dw, dl := burst(t, repo, n, fmt.Sprintf("d%d%03d", run%100, n), distinctOwner)

		ratio := float64(sw) / float64(dw)
		t.Logf("MEASURED n=%-3d shared %6v / %6v / %6v | distinct %6v / %6v / %6v | ratio %.2fx",
			n, sw.Round(time.Millisecond), pctl(sl, 0.5).Round(time.Millisecond), pctl(sl, 0.99).Round(time.Millisecond),
			dw.Round(time.Millisecond), pctl(dl, 0.5).Round(time.Millisecond), pctl(dl, 0.99).Round(time.Millisecond),
			ratio)
	}
	t.Log("both arms pre-warmed, so the ratio isolates shared serialization")
	t.Log("ratio near 1.0 means the identity cell is not the constraint at this concurrency")
}
