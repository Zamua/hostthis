// blobbench - sync-vs-async blob-write A/B harness for Upload.Create.
//
// Drives Upload.Create directly against a local slatedb/shale metadata
// store + an on-disk standalone blob store, so it measures the exact
// sync-vs-async variable with no SSH-handshake noise and no Sybil keygate in
// the way. (Use -blobms to simulate a slow blob store's write tail.)
//
// It reports two throughputs on purpose:
//   - ack throughput  = ops / (time for all Create calls to RETURN). In
//     async mode this is inflated: Create returns before the blob lands.
//   - drained throughput = ops / (time until all background finalizers
//     have also completed). This is the HONEST sustained rate - the same
//     blobs get written either way, so async should be ~equal or slightly
//     WORSE here (it does one extra metadata write per op).
//
// Build (needs the slatedb cdylib on the loader path):
//   CGO_LDFLAGS="-L$HOME/.local/lib" DYLD_LIBRARY_PATH="$HOME/.local/lib" \
//     go build -tags slatedb -o /tmp/blobbench ./cmd/blobbench
//
// Run (local MinIO at :9000 for the metadata bucket hostthis-metadata; the
// standalone blob store is on disk under a temp dir):
//   DYLD_LIBRARY_PATH="$HOME/.local/lib" /tmp/blobbench \
//     -sync=false -conc=8 -ops=2000 -owners=20 -size=4096

//go:build slatedb

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// delayBlob wraps a blob store and sleeps before each write to simulate a
// slow/variable blob store (e.g. the prod distributed-MinIO PUT tail,
// ~110ms-1.5s). The write path is what the async work moved off the ack,
// so this is the knob that decides whether async earns its keep. It holds
// the concrete compressed store so it satisfies the full read+write surface
// the StandaloneBlobUnit seam needs (Get/GetReader as well as the writes),
// delaying only the writes.
type delayBlob struct {
	inner *storage.CompressedBlobStore
	d     time.Duration
}

func (b delayBlob) Put(sha string, r io.Reader, size int64) error {
	time.Sleep(b.d)
	return b.inner.Put(sha, r, size)
}
func (b delayBlob) PutPrecompressed(sha string, body []byte) error {
	time.Sleep(b.d)
	return b.inner.PutPrecompressed(sha, body)
}
func (b delayBlob) Get(sha string) ([]byte, error) { return b.inner.Get(sha) }
func (b delayBlob) GetReader(sha string) (io.ReadCloser, int64, error) {
	return b.inner.GetReader(sha)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		syncMode = flag.Bool("sync", false, "true = inline blob (pre-async); false = async background blob")
		conc     = flag.Int("conc", 8, "concurrent workers")
		ops      = flag.Int("ops", 2000, "total Create calls")
		owners   = flag.Int("owners", 20, "distinct identities to spread across (quota is per-owner)")
		size     = flag.Int("size", 4096, "raw payload bytes per op (random, ~incompressible)")
		relaxed  = flag.Bool("relaxed", true, "relaxed durability (fast-ack at memtable) - matches prod R>=2")
		blobMs   = flag.Int("blobms", 0, "artificial ms of latency per blob write (simulate slow blob store)")
		label    = flag.String("label", "", "optional label for the report line")
	)
	flag.Parse()

	endpoint := env("MINIO_TEST_ENDPOINT", "http://localhost:9000")
	access := env("MINIO_TEST_ACCESS_KEY", "admin")
	secret := env("MINIO_TEST_SECRET_KEY", "supersecret")
	metaBucket := env("MINIO_TEST_METADATA_BUCKET", "hostthis-metadata")

	// Fresh logical db per run so each run's quota starts empty.
	dbName := fmt.Sprintf("blobbench-%d", time.Now().UnixNano())
	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            "blobbench",
		Endpoint:          endpoint,
		Region:            "us-east-1",
		Bucket:            metaBucket,
		AccessKey:         access,
		SecretKey:         secret,
		UseSSL:            false,
		DbName:            dbName,
		ReplicationFactor: 1,
		RelaxedDurability: *relaxed,
	})
	if err != nil {
		log.Fatalf("NewShaleRepo: %v", err)
	}
	defer repo.Close()

	// Standalone blob store for the A/B: a fresh on-disk store under a temp
	// dir. The detached S3 standalone backend was retired with the
	// shale-collocated blob work; this bench measures sync-vs-async on the
	// StandaloneBlobUnit seam, which the disk store exercises identically (and
	// the -blobms knob simulates a slow blob store's write tail directly).
	blobDir, err := os.MkdirTemp("", "blobbench-blobs-")
	if err != nil {
		log.Fatalf("temp blob dir: %v", err)
	}
	defer os.RemoveAll(blobDir) //nolint:errcheck
	disk, err := storage.NewBlobStore(blobDir)
	if err != nil {
		log.Fatalf("NewBlobStore: %v", err)
	}
	compressed := storage.NewCompressedBlobStore(disk)
	// blobUnit is the read+write surface the StandaloneBlobUnit seam needs.
	// When a write delay is configured, wrap the compressed store so the
	// writes (the ack-path bottleneck the benchmark exercises) sleep first.
	blobUnit := service.NewStandaloneBlobUnit(compressed)
	if *blobMs > 0 {
		blobUnit = service.NewStandaloneBlobUnit(delayBlob{inner: compressed, d: time.Duration(*blobMs) * time.Millisecond})
	}

	up := &service.Upload{Repo: repo, Blob: blobUnit, Now: time.Now, SyncBlob: *syncMode}

	// Pre-generate distinct, incompressible payloads (one per op) so no
	// blob dedup makes a write free. Each op's owner is round-robined.
	// Pastes are text. Generate random printable ASCII (lightly
	// compressible, like real text) so DetectKind accepts it and the blob
	// write reflects a realistic compressed size. A per-payload unique
	// header guarantees distinct SHAs (no blob dedup).
	const alpha = "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ 0123456789\n"
	payloads := make([][]byte, *ops)
	rng := rand.New(rand.NewSource(1)) // fixed seed -> same payload set both modes
	for i := range payloads {
		b := make([]byte, *size)
		for j := range b {
			b[j] = alpha[rng.Intn(len(alpha))]
		}
		copy(b, []byte(fmt.Sprintf("blobbench op %d\n", i)))
		payloads[i] = b
	}
	ownerIDs := make([]string, *owners)
	for i := range ownerIDs {
		ownerIDs[i] = fmt.Sprintf("key:blobbench-%s-owner-%d", dbName, i)
	}

	var (
		lats     = make([]time.Duration, *ops)
		errCount atomic.Int64
		firstErr atomic.Value
		jobs     = make(chan int, *ops)
	)
	for i := 0; i < *ops; i++ {
		jobs <- i
	}
	close(jobs)

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				owner := ownerIDs[i%*owners]
				t0 := time.Now()
				_, err := up.Create(bytes.NewReader(payloads[i]), owner, "", "markdown")
				lats[i] = time.Since(t0)
				if err != nil {
					errCount.Add(1)
					lats[i] = -1
					firstErr.CompareAndSwap(nil, err.Error())
				}
			}
		}()
	}
	wg.Wait()
	ackWall := time.Since(start)

	// Drain the background finalizers (async mode). In sync mode this is a
	// no-op. The time to here is the HONEST sustained-throughput clock.
	up.WaitFinalize()
	drainWall := time.Since(start)

	// Latency percentiles over successful ops only.
	ok := lats[:0]
	for _, d := range lats {
		if d >= 0 {
			ok = append(ok, d)
		}
	}
	sort.Slice(ok, func(a, b int) bool { return ok[a] < ok[b] })
	pct := func(q float64) float64 {
		if len(ok) == 0 {
			return 0
		}
		idx := int(q * float64(len(ok)-1))
		return float64(ok[idx].Microseconds()) / 1000.0
	}
	nOK := len(ok)
	mode := "async"
	if *syncMode {
		mode = "sync "
	}
	lbl := *label
	if lbl != "" {
		lbl = " " + lbl
	}
	dur := "relaxed"
	if !*relaxed {
		dur = "durable"
	}
	fmt.Printf("mode=%s%s dur=%s blobms=%-4d conc=%-3d ops=%-5d owners=%-3d size=%-7d | ack p50=%7.1fms p90=%7.1fms p99=%7.1fms | ackTput=%8.1f op/s | drainTput=%8.1f op/s | err=%d\n",
		mode, lbl, dur, *blobMs, *conc, *ops, *owners, *size,
		pct(0.50), pct(0.90), pct(0.99),
		float64(nOK)/ackWall.Seconds(),
		float64(nOK)/drainWall.Seconds(),
		errCount.Load(),
	)
	if e := firstErr.Load(); e != nil {
		fmt.Printf("  firstErr: %v\n", e)
	}
}
