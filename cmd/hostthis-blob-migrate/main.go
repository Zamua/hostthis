// ============================================================================
// TEMPORARY phase-4 migration code - DELETE THIS DIRECTORY
// (cmd/hostthis-blob-migrate/) after the prod blob migration completes, along
// with internal/storage/shale_blob_migrate_TEMP.go and the one temp Dockerfile
// build+COPY line. See docs/design/shale-blobs-phase4-migration.md.
// ============================================================================
//
// Command hostthis-blob-migrate is the ONE-SHOT legacy-blob re-key: it copies
// every paste / version / site-file blob out of the detached, sha-keyed
// `hostthis-blobs` MinIO bucket INTO the shale-collocated blob plane, stamping
// each metadata row's blob_id and co-committing the pointer.
//
// THE PIVOT (docs/design/shale-blobs-phase4-migration.md section 0/2): it runs
// AS hostthis itself. It builds the cluster via storage.NewShaleRepo using the
// IDENTICAL ShaleConfig cmd/hostthisd/metadata_shale.go's buildMetadataShale
// builds from the HOSTTHIS_* env, with the pod's OWN node identity
// (HOSTTHIS_NODE_ID). So this process IS the prod cluster restarted in migrate
// mode: it reclaims its own serving markers via the proven mass-restart path
// (no foreign-marker boot-defer, no hand-rolled convergence wait) and writes at
// the real R=2 topology through the cluster's own routing. It reuses the
// existing *BlobKV bind path (StageBlobStream -> RebindLegacyBlob), the row
// structs, shaleShardKey, RouteKeyForSlug, and ResolveBlobID/GetBlobStream with
// ZERO copied logic.
//
// Reads the SAME env vars hostthisd (shale backend) does, plus two migrate-only
// vars:
//
//	-- the cluster (mirrors metadata_shale.go exactly) --
//	HOSTTHIS_METADATA_S3_ENDPOINT     object store (e.g. http://minio:9000)
//	HOSTTHIS_METADATA_S3_BUCKET       metadata bucket (required)
//	HOSTTHIS_METADATA_S3_REGION       (default us-east-1)
//	HOSTTHIS_METADATA_S3_ACCESS_KEY   (required)
//	HOSTTHIS_METADATA_S3_SECRET_KEY   (required)
//	HOSTTHIS_METADATA_S3_USE_SSL      (true|false; default true)
//	HOSTTHIS_METADATA_DB_NAME         (default hostthis-metadata)
//	HOSTTHIS_NODE_ID                  this pod's stable node identity (default hostname)
//	HOSTTHIS_SHALE_REPLICATION_FACTOR (default 1; prod = 2)
//	HOSTTHIS_SHALE_UNIT_COUNT         (default 0; prod sharded = 16)
//	HOSTTHIS_SHALE_BIND_ADDR          memberlist host:port (non-empty => multi-node)
//	HOSTTHIS_SHALE_GRPC_ADDR          forwarding host:port (required with BIND_ADDR)
//	HOSTTHIS_SHALE_SEEDS              peer bind addrs (empty => this node is the founder)
//	HOSTTHIS_METADATA_AWAIT_DURABLE   (true|false; default true)
//	HOSTTHIS_METADATA_CACHE_BYTES     (default 128 MiB)
//	HOSTTHIS_SHALE_BLOB_BUCKET        the NEW collocated blob bucket (REQUIRED here:
//	                                  the migrate must light up the *BlobKV plane)
//
//	-- migrate-only --
//	MIGRATE_OLD_BLOB_BUCKET           the OLD detached hostthis-blobs bucket
//	                                  (bare <sha> keys); opened READ-ONLY (Get/Has).
//	MIGRATE_APPLY                     false (default) = DRY-RUN ONLY (the inspect
//	                                  gate); true = founder also migrates + verifies,
//	                                  all in ONE process after ONE convergence.
//	MIGRATE_ROLE                      founder | member (or auto: empty seeds => founder)
//	MIGRATE_CONCURRENCY               distinct-slug worker count (default 4)
//	MIGRATE_EXPECT_MEMBERS            founder waits for this ring size before re-keying
//	                                  (default len(seeds)+1; 0 = skip the wait)
//
// REQUIRES the slatedb build tag + libslatedb_uniffi on the loader path (the
// shale slate backend wraps slatedb), same as hostthisd-with-shale.

//go:build slatedb

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Zamua/shale/backends/slate/blobstore"
	"github.com/Zamua/shale/pkg/blob"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func main() {
	concurrency := flag.Int("concurrency", envOrInt("MIGRATE_CONCURRENCY", 4), "distinct-slug worker count (migrate)")
	flag.Parse()
	logger := log.New(os.Stderr, "blob-migrate ", log.LstdFlags|log.LUTC)

	// MIGRATE_APPLY is the single safety gate. false (default) = DRY-RUN ONLY:
	// converge, scan, report present/missing, hold (the inspect gate). true =
	// the founder ALSO migrates (re-keys) then verifies, all in THIS one process
	// after ONE convergence. Doing every phase in a single process is the robust
	// shape: a mode-switch by pod-restart re-cold-starts the ring asymmetrically
	// (the founder restarts into a ring the joiners still hold at bumped epochs),
	// which fence-storms the scan. So there is no -mode flag; the operator runs
	// the ring once with APPLY=false to inspect, then once with APPLY=true to apply.
	apply := strings.EqualFold(envOr("MIGRATE_APPLY", "false"), "true")

	// 1. Build the FULL app cluster via NewShaleRepo with the app's own env +
	//    node identity (the pivot). openCluster mirrors buildMetadataShale.
	repo, role, err := openCluster(logger)
	if err != nil {
		logger.Fatalf("open cluster: %v", err)
	}
	defer func() { _ = repo.Close() }()

	ctx := context.Background()

	// 2. Wait for the ring's ownership map to SETTLE (ALL pods). A cold migrate
	//    ring boot-defers units whose stale serving markers a prior tenant (the
	//    rehearsal seeder, or in prod the old serving cluster just scaled to 0)
	//    still holds; reconcile then hands them off over the next intervals, so
	//    the mounted set churns for a few seconds after "cluster up".
	if err := waitForConvergence(logger, repo); err != nil {
		logger.Fatalf("ring convergence: %v", err)
	}

	// 3. MEMBERS just HOLD the ring for R=2 - they do NOT scan. A member running
	//    the cross-shard scan reads the SAME units the founder reads (pure
	//    cold-start churn), and a transient scan error would crash-LOOP the
	//    member (re-cold-starting the ring under the founder). So the role gate
	//    is BEFORE the scan: only the founder opens the old bucket + scans.
	if role != roleFounder {
		logger.Printf("role=member: holding the ring for R=2 while the founder drives. Waiting for shutdown signal.")
		waitForSignal(logger)
		return
	}

	// FOUNDER from here. Open the OLD detached hostthis-blobs bucket READ-ONLY
	// (Get/Has) - only the founder reads it.
	oldStore, err := openOldBucket(logger)
	if err != nil {
		logger.Fatalf("open old blob bucket: %v", err)
	}

	// 4. Enumerate the record-blob set once. Retry transient blips (the
	//    mid-handoff slatedb fence AND cross-node gRPC connection drops on loaded
	//    boxes): ownership can micro-shift right at the convergence boundary and a
	//    single fenced/dropped unit fails the whole cross-shard Aggregate. A
	//    bounded retry makes the enumeration robust without masking a durable error.
	records, err := scanWithRetry(logger, repo)
	if err != nil {
		logger.Fatalf("enumerate record-blobs: %v", err)
	}
	logger.Printf("apply=%t role=%s enumerated %d record-blobs", apply, role, len(records))

	// Phase 1 - DRY-RUN (always, read-only, fail-closed on metadata/blob drift).
	if err := runDryRun(ctx, logger, oldStore, records); err != nil {
		logger.Fatalf("dry-run: %v", err)
	}

	if !apply {
		logger.Printf("MIGRATE_APPLY!=true: DRY-RUN ONLY (inspect gate). Re-run the ring with MIGRATE_APPLY=true to migrate+verify. Holding.")
		waitForSignal(logger)
		return
	}

	// Phase 2 - MIGRATE (the write): re-key every record-blob at R=2.
	if err := runMigrate(ctx, logger, repo, oldStore, records, *concurrency); err != nil {
		logger.Fatalf("migrate: %v", err)
	}
	// Phase 3 - VERIFY (the independent zero-loss gate): re-enumerate from the
	// metadata and assert the full read-back + byte-compare for every record.
	if err := runVerify(ctx, logger, repo, oldStore); err != nil {
		logger.Fatalf("verify FAILED (zero-loss gate): %v", err)
	}
	logger.Printf("verify GREEN: every record-blob resolves + reads back + matches the old bucket")

	// HOLD (do not exit) so the pod stays Ready with the result line in its log:
	// the operator/driver reads the outcome, then tears the ring down. Exiting
	// would let k8s restart the pod and re-cold-start the ring, racing the read.
	logger.Printf("migrate+verify complete; holding the ring (SIGTERM / rollout-restart to tear down)")
	waitForSignal(logger)
}

// --- mode: dry-run ----------------------------------------------------------

// runDryRun enumerates every record-blob and confirms each referenced <sha>
// exists in the OLD bucket (a HEAD via Has). NO writes. Reports the count, how
// many already carry a blob id (an already-migrated re-run), and any drift
// (a record whose sha is MISSING from the old bucket).
func runDryRun(ctx context.Context, logger *log.Logger, old blob.Store, records []storage.LegacyBlobRecord) error {
	var present, missing, already int
	for _, rec := range records {
		if rec.HasBlobID {
			already++
		}
		ok, err := old.Has(ctx, rec.ContentSHA)
		if err != nil {
			return fmt.Errorf("HEAD old bucket %s (%s/%s): %w", rec.ContentSHA, rec.Slug, kindName(rec.Kind), err)
		}
		if ok {
			present++
		} else {
			missing++
			logger.Printf("  MISSING old blob: slug=%s kind=%s sha=%s", rec.Slug, kindName(rec.Kind), rec.ContentSHA)
		}
	}
	logger.Printf("dry-run: records=%d present-in-old=%d missing-in-old=%d already-have-blob-id=%d",
		len(records), present, missing, already)
	if missing > 0 {
		return fmt.Errorf("%d record-blob(s) reference a sha that is NOT in the old bucket (metadata/blob drift) - resolve before migrating", missing)
	}
	return nil
}

// --- mode: migrate ----------------------------------------------------------

// runMigrate is the real copy. For each record lacking a blob id: GET the old
// <sha> bytes (verbatim magic+zstd), StageBlobStream them into the collocated
// bucket (mints a blobid), then RebindLegacyBlob (stamps blob_id + binds the
// pointer in one {slug} CAS). Idempotent + resumable: the per-record HasBlobID
// pre-check skips an already-rebound row BEFORE staging, so a re-run never mints
// a fresh blobid or leaks the newly-staged object; the in-method idempotency is
// the backstop.
//
// Concurrency is bounded across DISTINCT slugs only: records are grouped by slug
// and each worker owns a whole slug's records, so no two workers contend on one
// owner's {slug} CAS. Within a slug the records run sequentially.
func runMigrate(ctx context.Context, logger *log.Logger, repo *storage.ShaleRepo, old blob.Store, records []storage.LegacyBlobRecord, concurrency int) error {
	if concurrency < 1 {
		concurrency = 1
	}
	// Group by slug so each worker owns a slug (distinct shards, no CAS self-
	// contention). A slug's records run in-order within the worker.
	bySlug := make(map[domain.Slug][]storage.LegacyBlobRecord)
	var slugs []domain.Slug
	for _, rec := range records {
		if _, ok := bySlug[rec.Slug]; !ok {
			slugs = append(slugs, rec.Slug)
		}
		bySlug[rec.Slug] = append(bySlug[rec.Slug], rec)
	}

	var (
		migrated  atomic.Int64
		skipped   atomic.Int64
		firstErr  atomic.Pointer[migrateErr]
		abortOnce sync.Once
	)
	work := make(chan domain.Slug)
	// abort is closed exactly once on the first worker error. The producer
	// selects on it so a blocked send unblocks when workers stop draining
	// (otherwise the unbuffered send could deadlock once every worker has
	// returned on error).
	abort := make(chan struct{})
	fail := func(rec storage.LegacyBlobRecord, err error) {
		firstErr.CompareAndSwap(nil, &migrateErr{rec: rec, err: err})
		abortOnce.Do(func() { close(abort) })
	}
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for slug := range work {
				if firstErr.Load() != nil {
					return // abort: another worker hit an error
				}
				for _, rec := range bySlug[slug] {
					done, err := migrateOne(ctx, repo, old, rec)
					if err != nil {
						fail(rec, err)
						return
					}
					if done {
						migrated.Add(1)
					} else {
						skipped.Add(1)
					}
				}
			}
		}()
	}
producer:
	for _, slug := range slugs {
		select {
		case <-abort:
			break producer
		case work <- slug:
		}
	}
	close(work)
	wg.Wait()

	logger.Printf("migrate: records=%d migrated=%d skipped(already-had-blob-id)=%d", len(records), migrated.Load(), skipped.Load())
	if me := firstErr.Load(); me != nil {
		return fmt.Errorf("record slug=%s kind=%s sha=%s: %w", me.rec.Slug, kindName(me.rec.Kind), me.rec.ContentSHA, me.err)
	}
	return nil
}

type migrateErr struct {
	rec storage.LegacyBlobRecord
	err error
}

// migrateOne re-keys one record-blob. Returns done=true when it staged+rebound,
// done=false when the per-record idempotency pre-check skipped an already-bound
// row. Errors abort the slug's worker.
func migrateOne(ctx context.Context, repo *storage.ShaleRepo, old blob.Store, rec storage.LegacyBlobRecord) (bool, error) {
	// Idempotency pre-check: a row that already carries a blob id for this sha
	// is done - skip BEFORE staging so a re-run does not mint a fresh blobid and
	// leak the newly-staged object.
	if rec.HasBlobID {
		return false, nil
	}
	// GET the old bytes (verbatim magic+zstd) and their length.
	rc, size, err := old.GetStream(ctx, rec.ContentSHA)
	if err != nil {
		return false, fmt.Errorf("GET old blob %s: %w", rec.ContentSHA, err)
	}
	defer func() { _ = rc.Close() }()

	routeKey := repo.RouteKeyForSlug(rec.Slug.String())
	ref, err := repo.StageBlobStream(ctx, routeKey, rc, size, rec.ContentSHA)
	if err != nil {
		return false, fmt.Errorf("stage into collocated bucket: %w", err)
	}
	if err := repo.RebindLegacyBlob(ctx, rec.Slug, rec.Kind, rec.ContentSHA, ref); err != nil {
		return false, fmt.Errorf("rebind: %w", err)
	}
	return true, nil
}

// --- mode: verify -----------------------------------------------------------

// runVerify is the INDEPENDENT zero-loss gate. It re-derives the record-blob set
// from the metadata (does NOT trust migrate's self-reported counts) and, for
// every record, asserts the FULL read-back chain:
//
//  1. ResolveBlobID returns a NON-EMPTY blob id (the row carries it),
//  2. GetBlobStream reads the bytes back THROUGH the cluster (proves the bref
//     routed + R=2-replicated, a quorum read at R=2),
//  3. decoding (peel magic+zstd) the served body recomputes a sha == the
//     record's ContentSHA,
//  4. the OLD bucket's <sha> object decodes to byte-identical bytes.
//
// Any miss returns an error => the binary exits non-zero. The verify runs on a
// FRESH cluster (a separate Job pod), so a green verify proves the writes
// survive a full close+reopen, exactly as the app's restart will.
func runVerify(ctx context.Context, logger *log.Logger, repo *storage.ShaleRepo, old blob.Store) error {
	// Independent re-enumeration (does NOT trust migrate's counts), retry-guarded
	// against a transient handoff fence the same way the initial scan is.
	records, err := scanWithRetry(logger, repo)
	if err != nil {
		return fmt.Errorf("re-enumerate record-blobs: %w", err)
	}
	logger.Printf("verify: re-derived %d record-blobs from the metadata", len(records))

	var verified, failed int
	for _, rec := range records {
		if err := verifyOne(ctx, repo, old, rec); err != nil {
			failed++
			logger.Printf("  FAIL slug=%s kind=%s sha=%s: %v", rec.Slug, kindName(rec.Kind), rec.ContentSHA, err)
			continue
		}
		verified++
	}
	logger.Printf("verify: records=%d verified=%d failed=%d", len(records), verified, failed)
	if failed > 0 {
		return fmt.Errorf("%d record-blob(s) failed the read-back / byte-compare gate", failed)
	}
	return nil
}

// verifyOne runs the full read-back chain for one record-blob.
func verifyOne(ctx context.Context, repo *storage.ShaleRepo, old blob.Store, rec storage.LegacyBlobRecord) error {
	// 1. The row resolves to a non-empty blob id.
	blobID, err := repo.ResolveBlobID(rec.Slug, rec.ContentSHA)
	if err != nil {
		return fmt.Errorf("ResolveBlobID: %w", err)
	}
	if blobID == "" {
		return errors.New("ResolveBlobID returned an empty blob id (row not re-keyed)")
	}

	routeKey := repo.RouteKeyForSlug(rec.Slug.String())

	// 2 + 3. Read the NEW blob back through the cluster, decode, recompute sha.
	newDecoded, err := readDecodedThroughCluster(ctx, repo, routeKey, blobID)
	if err != nil {
		return fmt.Errorf("read new blob through cluster: %w", err)
	}
	if got := sha256Hex(newDecoded); got != rec.ContentSHA {
		return fmt.Errorf("new blob decoded sha %s != record ContentSHA %s", got, rec.ContentSHA)
	}

	// 4. The OLD bucket object decodes to byte-identical bytes.
	oldDecoded, err := readDecodedFromOld(ctx, old, rec.ContentSHA)
	if err != nil {
		return fmt.Errorf("read old blob: %w", err)
	}
	if !bytesEqual(newDecoded, oldDecoded) {
		return fmt.Errorf("new blob body (%d B) != old blob body (%d B)", len(newDecoded), len(oldDecoded))
	}
	return nil
}

// readDecodedThroughCluster streams blobID back via GetBlobStream (the cluster
// bref path) and decodes the magic+zstd body to the original bytes, buffered.
// Verify-only; record bodies are bounded by the per-file upload cap.
func readDecodedThroughCluster(ctx context.Context, repo *storage.ShaleRepo, routeKey []byte, blobID string) ([]byte, error) {
	rc, _, err := repo.GetBlobStream(ctx, routeKey, blobID)
	if err != nil {
		return nil, fmt.Errorf("GetBlobStream: %w", err)
	}
	dec, err := storage.DecodeCompressedStream(rc, blobID)
	if err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("decode: %w", err)
	}
	defer func() { _ = dec.Close() }()
	return io.ReadAll(dec)
}

// readDecodedFromOld streams the OLD <sha> object and decodes its magic+zstd
// body to the original bytes (the old bucket stores the SAME at-rest format).
func readDecodedFromOld(ctx context.Context, old blob.Store, sha string) ([]byte, error) {
	rc, _, err := old.GetStream(ctx, sha)
	if err != nil {
		return nil, fmt.Errorf("GetStream old %s: %w", sha, err)
	}
	dec, err := storage.DecodeCompressedStream(rc, sha)
	if err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("decode old %s: %w", sha, err)
	}
	defer func() { _ = dec.Close() }()
	return io.ReadAll(dec)
}

// --- cluster construction (mirrors metadata_shale.go's buildMetadataShale) ---

const (
	roleFounder = "founder"
	roleMember  = "member"
)

// openCluster builds the ShaleConfig from the HOSTTHIS_* env EXACTLY as
// cmd/hostthisd/metadata_shale.go's buildMetadataShale does (same env reads,
// same ShaleConfig incl. the blob plane via HOSTTHIS_SHALE_BLOB_BUCKET +
// blobstore.New), calls storage.NewShaleRepo (the FULL app startup: gRPC server,
// serving markers, reconcile), and resolves this pod's migrate role. The blob
// bucket is REQUIRED here (unlike the app, where it is optional): the migrate
// must light up the *BlobKV plane to stage + bind.
func openCluster(logger *log.Logger) (*storage.ShaleRepo, string, error) {
	endpoint := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_BUCKET"))
	region := envOr("HOSTTHIS_METADATA_S3_REGION", "us-east-1")
	accessKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_SECRET_KEY"))
	useSSL := strings.EqualFold(envOr("HOSTTHIS_METADATA_S3_USE_SSL", "true"), "true")
	dbName := envOr("HOSTTHIS_METADATA_DB_NAME", "hostthis-metadata")

	if bucket == "" {
		return nil, "", errors.New("HOSTTHIS_METADATA_S3_BUCKET is required")
	}
	if accessKey == "" || secretKey == "" {
		return nil, "", errors.New("HOSTTHIS_METADATA_S3_ACCESS_KEY + HOSTTHIS_METADATA_S3_SECRET_KEY are required")
	}

	nodeID := strings.TrimSpace(os.Getenv("HOSTTHIS_NODE_ID"))
	if nodeID == "" {
		if h, err := os.Hostname(); err == nil {
			nodeID = strings.TrimSpace(h)
		}
		if nodeID == "" {
			nodeID = "hostthis-1"
		}
	}
	replicationFactor := envOrInt("HOSTTHIS_SHALE_REPLICATION_FACTOR", 1)
	unitCount := envOrInt("HOSTTHIS_SHALE_UNIT_COUNT", 0)
	if unitCount < 0 {
		unitCount = 0
	}
	awaitDurable := strings.EqualFold(envOr("HOSTTHIS_METADATA_AWAIT_DURABLE", "true"), "true")
	cacheBytes := envOrInt("HOSTTHIS_METADATA_CACHE_BYTES", 128<<20)
	if cacheBytes < 0 {
		cacheBytes = 0
	}

	bindAddr := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_BIND_ADDR"))
	grpcAddr := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_GRPC_ADDR"))
	seeds := parseSeeds(os.Getenv("HOSTTHIS_SHALE_SEEDS"))

	// The blob plane (REQUIRED for the migrate): build the blob.Store over the
	// SAME object store the metadata uses (stripScheme + creds + UseSSL), the
	// IDENTICAL shape buildMetadataShale uses, pointed at the NEW collocated
	// blob bucket. Without it r.kv is nil and StageBlobStream/RebindLegacyBlob
	// error out, so the migrate cannot run.
	blobBucket := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_BLOB_BUCKET"))
	if blobBucket == "" {
		return nil, "", errors.New("HOSTTHIS_SHALE_BLOB_BUCKET is required for the blob migrate (the collocated blob plane must be enabled)")
	}
	var blobStore blob.Store
	bs, bsErr := blobstore.New(blobstore.Config{
		EndpointHost: stripScheme(endpoint),
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		UseSSL:       useSSL,
		Bucket:       blobBucket,
	})
	if bsErr != nil {
		return nil, "", fmt.Errorf("open collocated blob store: %w", bsErr)
	}
	blobStore = bs

	cfg := storage.ShaleConfig{
		NodeID:            nodeID,
		Endpoint:          endpoint,
		Region:            region,
		Bucket:            bucket,
		AccessKey:         accessKey,
		SecretKey:         secretKey,
		UseSSL:            useSSL,
		DbName:            dbName,
		BindAddr:          bindAddr,
		GRPCAddr:          grpcAddr,
		Seeds:             seeds,
		ReplicationFactor: replicationFactor,
		RelaxedDurability: !awaitDurable,
		UnitCount:         unitCount,
		CacheBytes:        uint64(cacheBytes),
		Logger:            logger,
		BlobStore:         blobStore,
	}
	// Retry the cluster open IN-PROCESS instead of crashing. A JOINER's
	// NewShaleRepo learns the ring by joining the founder's gossip/gRPC, which is
	// not up until the founder's OWN open returns - so an early joiner sees
	// "membership: join: connection refused". Crashing (exit -> the cmd's
	// log.Fatalf in main) would make k8s restart the pod with EXPONENTIAL
	// CrashLoopBackOff, delaying convergence past the founder's readiness wait and
	// leaving the founder to run its mode on a degraded 2/3 ring (false "missing
	// blobs"). The join fails FAST (before any unit mount), so retrying is cheap
	// and keeps the pod - and thus ring membership - stable. The founder (no
	// seeds) bootstraps locally and rarely needs this.
	const openRetryWindow = 6 * time.Minute
	openDeadline := time.Now().Add(openRetryWindow)
	var repo *storage.ShaleRepo
	for attempt := 1; ; attempt++ {
		var err error
		repo, err = storage.NewShaleRepo(cfg)
		if err == nil {
			if attempt > 1 {
				logger.Printf("cluster open: succeeded on attempt %d", attempt)
			}
			break
		}
		if time.Now().After(openDeadline) {
			return nil, "", fmt.Errorf("NewShaleRepo (after retries over %s): %w", openRetryWindow, err)
		}
		logger.Printf("cluster open: attempt %d failed (transient join race? retry in 3s): %v", attempt, err)
		time.Sleep(3 * time.Second)
	}

	role := resolveRole(seeds)
	logger.Printf("cluster up: node=%s role=%s bind=%s grpc=%s seeds=%d rf=%d units=%d awaitDurable=%t metaBucket=%s blobBucket=%s db=%s endpoint=%s",
		nodeID, role, bindAddr, grpcAddr, len(seeds), replicationFactor, unitCount, awaitDurable, bucket, blobBucket, dbName, endpoint)
	return repo, role, nil
}

// resolveRole picks the migrate role. An explicit MIGRATE_ROLE wins
// (founder|member); otherwise the founder is the seed node (empty Seeds), the
// SAME "founder = the seedless node" convention cluster.Open uses. The founder
// drives the single-writer re-key loop; members hold the ring for R=2.
func resolveRole(seeds []string) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MIGRATE_ROLE"))) {
	case roleFounder:
		return roleFounder
	case roleMember:
		return roleMember
	}
	if len(seeds) == 0 {
		return roleFounder
	}
	return roleMember
}

// waitForConvergence blocks until the migrate ring's ownership map has SETTLED:
// the full member set has joined AND this node's mounted-unit count has held
// steady across several consecutive polls (longer than one reconcile interval).
// A freshly cold-started ring boot-defers units a prior tenant's stale serving
// markers still cover, then reconcile acquires them over the next intervals, so
// the mounted set churns for a few seconds after "cluster up". Running the
// cross-shard scan during that churn races a peer's fence ("detected newer DB
// client"). Single-node (expect<=1) has no handoff to settle and skips the wait.
func waitForConvergence(logger *log.Logger, repo *storage.ShaleRepo) error {
	seeds := parseSeeds(os.Getenv("HOSTTHIS_SHALE_SEEDS"))
	expect := envOrInt("MIGRATE_EXPECT_MEMBERS", len(seeds)+1)
	if expect <= 1 {
		logger.Printf("convergence: expect<=1 (single-node), no handoff to settle; proceeding")
		return nil
	}
	const (
		pollEvery   = 4 * time.Second
		stablePolls = 3 // mounted count unchanged this many polls in a row => settled (> reconcileInterval)
	)
	deadline := time.Now().Add(6 * time.Minute)
	lastCount := -1
	stable := 0
	for {
		members := repo.MemberCount()
		mounted := repo.MountedUnitCount()
		if mounted == lastCount {
			stable++
		} else {
			stable = 0
			lastCount = mounted
		}
		if members >= expect && stable >= stablePolls {
			logger.Printf("convergence: members=%d/%d, mounted-units steady at %d across %d polls; ring settled, scanning", members, expect, mounted, stable)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ring did not settle within 6m (members=%d/%d mounted-units=%d stable=%d); every migrate pod up + reconcile progressing?", members, expect, mounted, stable)
		}
		logger.Printf("convergence: members=%d/%d mounted-units=%d stable=%d/%d; waiting...", members, expect, mounted, stable, stablePolls)
		time.Sleep(pollEvery)
	}
}

// scanWithRetry runs ScanLegacyBlobs, retrying transient blips as a backstop to
// waitForConvergence (ownership can micro-shift right at the convergence boundary,
// and one fenced/dropped unit fails the whole cross-shard Aggregate). The budget
// is generous: on loaded boxes a cross-node gRPC connection can blip for tens of
// seconds while a peer pod re-establishes. Bounded so a genuine, persistent error
// still surfaces.
func scanWithRetry(logger *log.Logger, repo *storage.ShaleRepo) ([]storage.LegacyBlobRecord, error) {
	const (
		maxAttempts = 24
		backoff     = 5 * time.Second
	)
	for attempt := 1; ; attempt++ {
		records, err := repo.ScanLegacyBlobs()
		if err == nil {
			if attempt > 1 {
				logger.Printf("scan: succeeded on attempt %d", attempt)
			}
			return records, nil
		}
		if attempt >= maxAttempts || !isTransientScanErr(err) {
			return nil, err
		}
		logger.Printf("scan: attempt %d hit a transient error (retry in %s): %v", attempt, backoff, err)
		time.Sleep(backoff)
	}
}

// isTransientScanErr reports whether err is a transient cold-start/convergence
// blip the scan should retry rather than crash on - NOT a durable failure.
// Three families, all matched by substring (they propagate as opaque gRPC errors
// wrapping the underlying message):
//   - the slatedb "a peer fenced my open of this unit" fence, which appears while
//     ownership is still handing off ("detected newer DB client" / "Closed error");
//   - a cross-node gRPC connection drop to a peer holding part of the keyspace, on
//     loaded boxes during convergence ("Unavailable", "connection error", "use of
//     closed network connection", "transport is closing", "reading server preface");
//   - a peer whose gRPC server is not yet serving at cold-start (it is still
//     mounting its units) so the scan's waitReady times out ("peer connection not
//     ready" / "TRANSIENT_FAILURE" / "context deadline exceeded") - the next
//     attempt, once that peer is serving, succeeds.
func isTransientScanErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, frag := range []string{
		"detected newer DB client",
		"Closed error",
		"code = Unavailable",
		"connection error",
		"use of closed network connection",
		"transport is closing",
		"error reading server preface",
		"connection refused",
		// Cold-start peer-gRPC not-yet-ready: the cross-shard scan's waitReady
		// (shale snapshotPeer) returns these while a peer is still mounting its
		// units and its gRPC server has not begun serving. On a loaded box a peer
		// can take a while to come up; these are transient (a later attempt, once
		// the peer is serving, succeeds), so RETRY rather than fail the migrate.
		"peer connection not ready",
		"context deadline exceeded",
		"TRANSIENT_FAILURE",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// openOldBucket opens the OLD detached hostthis-blobs bucket READ-ONLY: a
// blob.Store (blobstore.New) over the SAME object store the metadata uses,
// pointed at MIGRATE_OLD_BLOB_BUCKET. The old bucket keys at bare <sha> and a
// blob.Store with no KeyPrefix reads exactly <bucket>/<sha>, so GetStream(sha)
// + Has(sha) hit the old objects directly. The migrate only ever calls
// Get/Has on it; it never writes or deletes the old bucket.
func openOldBucket(logger *log.Logger) (blob.Store, error) {
	oldBucket := strings.TrimSpace(os.Getenv("MIGRATE_OLD_BLOB_BUCKET"))
	if oldBucket == "" {
		return nil, errors.New("MIGRATE_OLD_BLOB_BUCKET is required (the detached hostthis-blobs bucket, bare <sha> keys)")
	}
	endpoint := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ENDPOINT"))
	accessKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_SECRET_KEY"))
	useSSL := strings.EqualFold(envOr("HOSTTHIS_METADATA_S3_USE_SSL", "true"), "true")
	store, err := blobstore.New(blobstore.Config{
		EndpointHost: stripScheme(endpoint),
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		UseSSL:       useSSL,
		Bucket:       oldBucket,
	})
	if err != nil {
		return nil, fmt.Errorf("open old bucket %s: %w", oldBucket, err)
	}
	logger.Printf("old blob bucket (read-only): %s", oldBucket)
	return store, nil
}

// --- small helpers (mirrors of metadata_shale.go's, kept local to the temp dir) ---

// stripScheme removes a leading http:// or https:// (+ trailing slash) so the
// blobstore.Config gets the bare host:port. Mirrors metadata_shale.go.
func stripScheme(endpoint string) string {
	s := strings.TrimSpace(endpoint)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimSuffix(s, "/")
}

// parseSeeds splits a comma-separated HOSTTHIS_SHALE_SEEDS value, trimming and
// dropping empties. Mirrors metadata_shale.go (an empty / whitespace value =>
// nil => "this node is the founder").
func parseSeeds(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func kindName(k storage.LegacyBlobKind) string {
	switch k {
	case storage.LegacyBlobPaste:
		return "paste"
	case storage.LegacyBlobVersion:
		return "version"
	case storage.LegacyBlobSiteFile:
		return "site-file"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// sha256Hex computes the lowercase-hex sha256 of b, the content-sha shape the
// metadata stores (matches domain.HashContent).
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// bytesEqual is a tiny local byte compare so verify avoids importing bytes for
// one call.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// waitForSignal blocks until SIGINT/SIGTERM (a member pod just holds the ring).
func waitForSignal(logger *log.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	s := <-ch
	logger.Printf("received %v, shutting down", s)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envOrInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}
