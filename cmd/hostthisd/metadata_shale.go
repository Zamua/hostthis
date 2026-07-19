// metadata_shale.go - shale-cluster-backed metadataBundle. Active only
// when the binary is built with -tags slatedb (the shale backend wraps
// slatedb as its per-node KV engine, so it carries the same build/runtime
// requirements as the direct slatedb backend).
//
// Reads the same S3 connection config as the slatedb backend:
//   HOSTTHIS_METADATA_S3_ENDPOINT   (e.g. http://minio:9000)
//   HOSTTHIS_METADATA_S3_BUCKET     (required)
//   HOSTTHIS_METADATA_S3_REGION     (default us-east-1)
//   HOSTTHIS_METADATA_S3_ACCESS_KEY (required)
//   HOSTTHIS_METADATA_S3_SECRET_KEY (required)
//   HOSTTHIS_METADATA_S3_USE_SSL    (true|false; default true)
//   HOSTTHIS_METADATA_DB_NAME       (default "hostthis-metadata")
//
// Plus the cluster-layer additions:
//   HOSTTHIS_NODE_ID                  (default os.Hostname(), or "hostthis-1")
//   HOSTTHIS_SHALE_REPLICATION_FACTOR (default 1)
//   HOSTTHIS_SHALE_BIND_ADDR          (host:port; NON-EMPTY enables multi-node mode)
//   HOSTTHIS_SHALE_GRPC_ADDR          (host:port; required when BIND_ADDR is set)
//   HOSTTHIS_SHALE_SEEDS              (comma-separated peer BIND_ADDRs; empty = seed node)
//   HOSTTHIS_METADATA_AWAIT_DURABLE   (true|false; default true; false = relaxed
//                                      durability/fast-ack, only safe at RF>=2)
//   HOSTTHIS_SHALE_READ_TIMEOUT       (Go duration, e.g. "8s"; unset/empty =
//                                      shale's 5s default; malformed fails startup)
//   HOSTTHIS_SHALE_WRITE_TIMEOUT      (same, for the per-dispatch write deadline)
//
// With HOSTTHIS_SHALE_BIND_ADDR unset (the default) the node runs the
// single-node path exactly as before: no gossip, no ring routing, every
// op local. Setting it brings up memberlist + gRPC forwarding and joins
// the ring (docs/SPEC.md "Multi-node shale").
//
// ReadConsistency is fixed at cluster.ReadQuorum inside
// storage.NewShaleRepo (ShaleConfig exposes no knob for it), so there is
// no env var for it here. Quorum is required at R>1 (ReadNearest could
// return a still-backfilling replica's NotFound for live data); at R=1 a
// quorum is the single replica, so it is behavior-identical there.

//go:build slatedb

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/backends/slate/blobstore"
	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/storageunit"
	"google.golang.org/grpc"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/relay/relaygrpc"
	"github.com/Zamua/hostthis/internal/shaleblob"
	"github.com/Zamua/hostthis/internal/storage"
	slatedb "slatedb.io/slatedb-go/uniffi"
)

// slatedbLogLevel maps HOSTTHIS_SLATEDB_LOG_LEVEL to a slatedb LogLevel.
// Empty / "off" => no slatedb tracing (the default). Diagnostic only:
// enabling debug/trace is verbose and meant for short-lived investigation.
func slatedbLogLevel(s string) (slatedb.LogLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off":
		return 0, false
	case "error":
		return slatedb.LogLevelError, true
	case "warn":
		return slatedb.LogLevelWarn, true
	case "info":
		return slatedb.LogLevelInfo, true
	case "debug":
		return slatedb.LogLevelDebug, true
	case "trace":
		return slatedb.LogLevelTrace, true
	default:
		return slatedb.LogLevelInfo, true
	}
}

// openShaleRepoFromEnv builds the shale ShaleRepo from the HOSTTHIS_* env and
// nothing else: no background reconcile loop, no blob-unit wiring, no debug
// server - just the opened repo with its retention set. buildMetadataShale
// calls it and then layers the reconcile loop / blob unit / debug server on
// top, keeping the bare open path separate from the daemon wiring. The caller
// owns repo.Close().
//
// registerGRPC (optional, nil for callers that want the bare repo) is the
// opaque hook threaded into ShaleConfig.RegisterGRPC: in multi-node mode
// NewShaleRepo calls it with the cluster gRPC server before serving, so
// composition-root services (the relay's peer fan-out) ride the same
// listener + advertised address shale forwarding uses.
func openShaleRepoFromEnv(retention domain.Retention, logger *log.Logger, registerGRPC func(*grpc.Server)) (*storage.ShaleRepo, error) {
	// Optional slatedb tracing (to stderr) for diagnosing the SST-read
	// pattern. Off unless HOSTTHIS_SLATEDB_LOG_LEVEL is set.
	if lvl, on := slatedbLogLevel(os.Getenv("HOSTTHIS_SLATEDB_LOG_LEVEL")); on {
		if err := slatedb.InitLogging(lvl, nil); err != nil {
			logger.Printf("metadata: slatedb InitLogging failed: %v", err)
		} else {
			logger.Printf("metadata: slatedb tracing enabled at %v", os.Getenv("HOSTTHIS_SLATEDB_LOG_LEVEL"))
		}
	}
	endpoint := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_BUCKET"))
	region := envOr("HOSTTHIS_METADATA_S3_REGION", "us-east-1")
	accessKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_SECRET_KEY"))
	useSSL := strings.EqualFold(envOr("HOSTTHIS_METADATA_S3_USE_SSL", "true"), "true")
	dbName := envOr("HOSTTHIS_METADATA_DB_NAME", "hostthis-metadata")

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
	// Backend shape: 0 (default) = single slatedb DB per node (today). A power
	// of two = MULTI-BACKEND sharded mode: that many slatedb units distributed
	// across the ring, routed per key. Each unit is a full slatedb instance per
	// owning replica, so keep small on the RAM-tight boxes. See docs/SPEC.md
	// "Sharded metadata (multi-backend mode)".
	unitCount := envOrInt("HOSTTHIS_SHALE_UNIT_COUNT", 0)
	if unitCount < 0 {
		unitCount = 0
	}
	// Durability mode: default true (ack after the durable object-store
	// flush). false = relaxed durability (fast-ack at the memtable), the big
	// write-throughput win; only safe at RF>=2 on separate nodes. See
	// docs/SPEC.md "Relaxed durability: fast-ack at the memtable".
	awaitDurable := strings.EqualFold(envOr("HOSTTHIS_METADATA_AWAIT_DURABLE", "true"), "true")
	// Block cache for the slatedb metadata layer (Moka in-memory). Without it
	// slatedb has NO block cache and re-fetches SST blocks from MinIO on every
	// read - a steady self-inflicted read storm against the object store.
	// Default 128 MiB; set 0 to disable. Tunable on the RAM-tight boxes.
	cacheBytes := envOrInt("HOSTTHIS_METADATA_CACHE_BYTES", 128<<20)
	if cacheBytes < 0 {
		cacheBytes = 0
	}
	// Fence-WAL GC: enable slatedb's fence-WAL garbage collector so the
	// per-open fence WAL objects don't accumulate unboundedly (slatedb ships
	// that GC category in dry-run by default; see storage.ShaleConfig.ReapFenceWALs).
	// Default ON for this long-lived server; HOSTTHIS_SLATEDB_FENCE_GC=false is
	// the kill-switch (fall back to slatedb's untouched defaults).
	reapFenceWALs := !strings.EqualFold(strings.TrimSpace(os.Getenv("HOSTTHIS_SLATEDB_FENCE_GC")), "false")
	// Per-dispatch cluster deadlines (shale defaults each to 5s; zero keeps
	// them). The read budget is the one deploys raise (e.g. "8s"): during a
	// rollout a read that lands in a shard's sub-second handoff window
	// re-polls within ReadTimeout, so a bigger budget turns the rare
	// window-exceeding read into latency instead of a client error. Malformed
	// values are a config error and fail startup - never a silent default.
	// See docs/SPEC.md "Dispatch deadlines: the read/write timeout knobs".
	readTimeout, writeTimeout, err := shaleTimeoutsFromEnv()
	if err != nil {
		return nil, err
	}

	// Multi-node peer-discovery config. A non-empty bind addr is the
	// switch that takes the cluster out of the single-node path.
	bindAddr := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_BIND_ADDR"))
	grpcAddr := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_GRPC_ADDR"))
	seeds := parseSeeds(os.Getenv("HOSTTHIS_SHALE_SEEDS"))

	if bucket == "" {
		return nil, fmt.Errorf("HOSTTHIS_METADATA_S3_BUCKET is required for shale backend")
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("HOSTTHIS_METADATA_S3_ACCESS_KEY + HOSTTHIS_METADATA_S3_SECRET_KEY are required")
	}
	// Checked with the other required-env checks, i.e. BEFORE anything is
	// allocated, so a refusal needs no cleanup path. Deliberately validated
	// here rather than left to the cluster layer: the operator gets an error
	// naming the env var they set, not one about internal config fields.
	if err := checkUnitCountForMode(unitCount, bindAddr); err != nil {
		return nil, err
	}

	// Optional transactional shale-blob plane. When HOSTTHIS_SHALE_BLOB_BUCKET
	// is set, blobs go THROUGH shale (the pointer co-commits with the metadata
	// on the owning shard) over a MinIO blob.Store pointed at that DISTINCT blob
	// bucket on the SAME object store the metadata uses. Unset keeps the
	// metadata-only path (the detached content-addressed store via
	// buildBlobStore stays the blob backend).
	var blobStore blob.Store
	blobBucket := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_BLOB_BUCKET"))
	if blobBucket != "" {
		bs, bsErr := blobstore.New(blobstore.Config{
			EndpointHost: stripScheme(endpoint),
			AccessKey:    accessKey,
			SecretKey:    secretKey,
			UseSSL:       useSSL,
			Bucket:       blobBucket,
		})
		if bsErr != nil {
			return nil, fmt.Errorf("open shale blob store: %w", bsErr)
		}
		blobStore = bs
	}

	// Optional HOMOGENEOUS bootstrap. When HOSTTHIS_SHALE_HOMOGENEOUS=true, build
	// a MinIO-backed ConditionalStore over the SAME metadata bucket, namespaced by
	// the DB name (so the __cluster/init marker is one shared object for every
	// pod), and hand it to the cluster. cluster.Open then decides form-vs-join at
	// runtime against the marker (try-join-else-form) instead of the founder/
	// joiner seed asymmetry; every pod runs identical config. Requires multi-
	// backend (sharded) + multi-node mode - the marker's durable {gen, count}
	// records the unit count, which is meaningless without sharding, and the
	// solo-start/form race only arises across gossiping pods. See docs/SPEC.md
	// "Homogeneous bootstrap (optional)". Unset keeps the seed-based bootstrap.
	var condStore storageunit.ConditionalStore
	homogeneous := strings.EqualFold(strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_HOMOGENEOUS")), "true")
	if homogeneous {
		if unitCount <= 0 {
			return nil, fmt.Errorf("HOSTTHIS_SHALE_HOMOGENEOUS=true requires multi-backend mode (HOSTTHIS_SHALE_UNIT_COUNT > 0)")
		}
		if bindAddr == "" {
			return nil, fmt.Errorf("HOSTTHIS_SHALE_HOMOGENEOUS=true requires multi-node mode (HOSTTHIS_SHALE_BIND_ADDR set)")
		}
		cs, csErr := slate.NewMinioConditionalStore(slate.MinioConditionalStoreConfig{
			EndpointHost: stripScheme(endpoint),
			AccessKey:    accessKey,
			SecretKey:    secretKey,
			UseSSL:       useSSL,
			Bucket:       bucket,
			KeyPrefix:    dbName,
		})
		if csErr != nil {
			return nil, fmt.Errorf("open shale conditional store: %w", csErr)
		}
		condStore = cs
	}

	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
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
		ReapFenceWALs:     reapFenceWALs,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		Logger:            logger,
		BlobStore:         blobStore,
		ConditionalStore:  condStore,
		RegisterGRPC:      registerGRPC,
	})
	if err != nil {
		return nil, fmt.Errorf("open shale: %w", err)
	}
	repo.Retention = retention
	if bindAddr == "" {
		logger.Printf("metadata: shale (single-node) node=%s bucket=%s db=%s rf=%d shards=%d awaitDurable=%t fenceGC=%t blobBucket=%q endpoint=%s",
			nodeID, bucket, dbName, replicationFactor, unitCount, awaitDurable, reapFenceWALs, blobBucket, endpoint)
	} else {
		logger.Printf("metadata: shale (multi-node) node=%s bind=%s grpc=%s seeds=%d rf=%d shards=%d awaitDurable=%t fenceGC=%t homogeneous=%t blobBucket=%q bucket=%s db=%s endpoint=%s",
			nodeID, bindAddr, grpcAddr, len(seeds), replicationFactor, unitCount, awaitDurable, reapFenceWALs, homogeneous, blobBucket, bucket, dbName, endpoint)
	}
	return repo, nil
}

// buildMetadataShale opens the shale repo (openShaleRepoFromEnv) and wires the
// full daemon bundle around it: the site + room repos, the optional
// transactional blob unit, the optional debug endpoint, and the periodic
// Reconcile loop. The audit subcommand deliberately does NOT go through here -
// it wants the bare repo with no background loops.
func buildMetadataShale(retention domain.Retention, logger *log.Logger) (*metadataBundle, error) {
	// Multi-pod relay peer transport (SPEC "Multi-pod relay: the peer
	// transport"): the RECEIVER must exist before the repo opens - its
	// Register method is the opaque func(*grpc.Server) hook NewShaleRepo
	// calls when it stands up the cluster gRPC server in multi-node mode -
	// while its local-delivery target is late-bound by main once the relay
	// exists (frames arriving before that bind are dropped; no client can
	// be connected before the HTTP server is up). Single-node mode never
	// invokes the hook and wires no transport: the relay keeps its nil
	// publisher, the zero-peer degenerate case.
	// The receiver's cap must admit the LARGEST legal frame on this
	// channel: a durable mirror carrying a committed room value verbatim
	// (domain.MaxRoomValueBytes, set by the HTTP PUT path - several times
	// the client-socket frame cap) with worst-case JSON-string inflation.
	// Sizing this to the client-socket cap silently severs cross-pod
	// mirrors for every legal value above it (SPEC "Trust boundary").
	relayRecv := relaygrpc.NewReceiver(relay.MaxDurableFrameBytes(domain.MaxRoomValueBytes))

	// /readyz mount floor (docs/SPEC.md "Readiness vs liveness"). Parsed
	// BEFORE the (heavy) cluster open: a malformed or out-of-range fraction
	// is a configuration error that must refuse startup, same fail-loud
	// posture as the dispatch timeouts.
	minMountedFraction, err := readyMinMountedFractionFromEnv()
	if err != nil {
		return nil, err
	}

	repo, err := openShaleRepoFromEnv(retention, logger, relayRecv.Register)
	if err != nil {
		return nil, err
	}
	bundle := &metadataBundle{
		Repo:    repo,
		KeyGate: repo,
		// Static-site hosting on shale: the ShaleSiteRepo adapter shares the
		// same shale cluster (shard routing + per-shard CAS) as the paste
		// repo, so a non-nil Sites lights up archive hosting on shale, the
		// same way it does on slatedb.
		Sites: storage.NewShaleSiteRepo(repo),
		// Room persistence on shale: the ShaleRoomRepo adapter shares the same
		// shale cluster, co-locating every room family on the {app-slug} shard
		// so the room tier runs on shale clusters too.
		Rooms: storage.NewShaleRoomRepo(repo),
		// Readiness: gate /readyz on the cluster's mount floor so a rollout
		// stalls on a pod that cannot mount its storage instead of surging
		// past it. The fraction semantics (0 = no floor, desired == 0
		// vacuously ready) live in the shale predicate.
		Readiness: shaleReadinessProber{repo: repo, minMountedFraction: minMountedFraction},
		Close:     repo.Close,
	}
	logger.Printf("readiness: /readyz mount floor minMountedFraction=%g (0 disables the floor)", minMountedFraction)
	// Multi-node: supply the relay peer transport. The publisher fans every
	// frame out to the CURRENT peer set, discovered per publish from the
	// ring membership the cluster gossips (self excluded) - the same
	// advertised gRPC addresses shale forwarding dials, adapted onto the
	// relay's narrow Peers port so the relay stays storage-agnostic. main
	// wires Publisher + Bind into the relay and Closes the publisher at
	// shutdown.
	if repo.GRPCAddr() != "" {
		pub := relaygrpc.NewPublisher(shalePeers{repo: repo}, relaygrpc.PublisherConfig{Logf: logger.Printf})
		bundle.RelayPeer = &relayPeerTransport{
			Publisher: pub,
			Bind:      func(deliver func(relay.RoomKey, relay.Frame)) { relayRecv.Bind(deliver) },
			Close:     pub.Close,
		}
		logger.Printf("relay: multi-pod peer transport ready (peer service on the cluster gRPC server at %s; discovery via ring membership)", repo.GRPCAddr())
	}

	// Transactional shale-blob seam: when the repo opened a blob plane, supply
	// the shaleblob.Unit (pointer co-commits with metadata) + schedule
	// SweepOrphans for orphan-bytes reclamation. main picks these over the
	// standalone unit + the global content-addressed sweep.
	if repo.HasBlobPlane() {
		unit, uErr := shaleblob.New(repo)
		if uErr != nil {
			_ = repo.Close()
			return nil, fmt.Errorf("build shale blob unit: %w", uErr)
		}
		bundle.BlobUnit = unit
		bundle.BlobOrphanSweeper = repo
	}

	// OPTIONAL live-diagnosis endpoint. When HOSTTHIS_SHALE_DEBUG_ADDR is set
	// (e.g. ":6060"), serve the embedded cluster's per-position handoff dump at
	// /debug/shale/state - the production-safe equivalent of the shaled binary's
	// SHALE_DEBUG_ADDR endpoint, for diagnosing a stuck position (desired-but-
	// unmounted, a parked handoff phase, the last swallowed acquire error). OFF
	// unless the env var is set, so production is unaffected.
	if dbgAddr := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_DEBUG_ADDR")); dbgAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/shale/state", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, repo.DebugClusterState())
		})
		srv := &http.Server{Addr: dbgAddr, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Printf("metadata: shale debug server on %s exited: %v", dbgAddr, err)
			}
		}()
		logger.Printf("metadata: shale debug endpoint serving %s/debug/shale/state", dbgAddr)
	}

	// PERIODIC RECONCILE. Reconcile is the metadata backend's maintenance pass,
	// and it is the SOLE quota-healing mechanism now that the per-identity quota
	// is DERIVED by scanning the enumeration indexes rather than kept in a stored
	// counter (docs/SPEC.md "Scan-derived quota" / "Derived indexes and
	// repair-on-read"). Each pass:
	//   - REPROJECTS the identity_pastes + identity_sites enumeration indexes
	//     from the authoritative pastes/ + sites/ rows (add missing entries,
	//     refresh stale projections, drop orphans). This closes the
	//     crash-between-row-and-index gap: a crash between the authoritative row
	//     write and the {id} enumeration-index write leaves a live row the index
	//     does not list, transiently UNDER-counting that owner until this pass
	//     re-adds the entry. It backfills sites deployed before the
	//     identity_sites index existed too, so it REPLACES the old one-shot
	//     BackfillSiteIndexes goroutine.
	//   - AGES OUT crashed pending pastes (the pod-death backstop).
	// The SET part of the heal (add/drop entries) is idempotent, but the entries
	// carry cached NUMBERS (each paste's live version sum) computed from the
	// pass's point-in-time snapshot, so a pass CAN race a live write's fresher
	// refresh. Every reprojection write is therefore guarded - it commits only
	// if the entry still holds the pass's snapshot value, skipping (guarded to
	// LOSE, never to clobber) when a live write landed mid-pass - which is what
	// makes the pass safe under live traffic on any cadence and from every pod
	// concurrently; residual staleness from a skip is healed by the next pass
	// (docs/SPEC.md "Periodic reconcile"). Best-effort: a pass that hits the
	// post-boot convergence window (the aggregate scan fails while units are
	// still handing off), or that had per-entry write failures (each skipped +
	// logged, Policy 1), just logs and the next tick retries. First pass after a
	// short settle delay, then every 10 min.
	go func() {
		const reconcileInterval = 10 * time.Minute
		timer := time.NewTimer(90 * time.Second) // let the cluster converge past boot first
		defer timer.Stop()
		for {
			<-timer.C
			if err := repo.Reconcile(time.Now().UTC()); err != nil {
				logger.Printf("metadata: periodic reconcile: %v (retrying next tick)", err)
			} else {
				logger.Printf("metadata: periodic reconcile complete")
			}
			timer.Reset(reconcileInterval)
		}
	}()

	return bundle, nil
}

// shalePeers adapts the ShaleRepo's gossiped ring-membership view onto
// the relay's Peers port (the current peer gRPC addresses, self
// excluded). Defined here at the composition root so storage stays
// relay-agnostic and the relay stays storage-agnostic.
type shalePeers struct{ repo *storage.ShaleRepo }

func (p shalePeers) Addresses() []string { return p.repo.PeerGRPCAddrs() }

// stripScheme removes a leading http:// or https:// from a metadata S3 endpoint
// so the blobstore.Config gets the bare host:port it wants (the metadata config
// carries the full URL; the blobstore adapter takes EndpointHost + a UseSSL
// flag separately). A scheme-less endpoint is returned unchanged.
func stripScheme(endpoint string) string {
	s := strings.TrimSpace(endpoint)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimSuffix(s, "/")
}

// parseSeeds splits a comma-separated HOSTTHIS_SHALE_SEEDS value into peer
// bind addresses, trimming whitespace around each entry and dropping empty
// ones (so a trailing comma or an all-whitespace value yields nil, which
// cluster.Open reads as "this node is the seed"). Returns nil for an empty
// input so the single-node path sees a nil Seeds slice unchanged.
func parseSeeds(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}
