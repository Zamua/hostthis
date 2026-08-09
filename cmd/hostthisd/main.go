// Package main wires the hostthis daemon: SSH server, HTTP server, storage and
// the periodic blob-GC sweep, configured from flags and HOSTTHIS_* env.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Zamua/hostthis/internal/cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/metrics"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

func main() {
	var (
		dataDir         = flag.String("data-dir", envOr("HOSTTHIS_DATA_DIR", "./data"), "where metadata + blobs live")
		sshAddr         = flag.String("ssh-addr", envOr("HOSTTHIS_SSH_ADDR", ":2222"), "ssh listen address")
		httpAddr        = flag.String("http-addr", envOr("HOSTTHIS_HTTP_ADDR", ":8080"), "http listen address")
		metricsAddr     = flag.String("metrics-addr", envOr("HOSTTHIS_METRICS_ADDR", ":9091"), "prometheus metrics listen address (never route this publicly)")
		apexDomain      = flag.String("apex-domain", os.Getenv("HOSTTHIS_APEX_DOMAIN"), "public apex (required; e.g. paste.example.com)")
		urlMode         = flag.String("mode", envOr("HOSTTHIS_URL_MODE", "path"), "url mode: subdomain (prod) | path (dev)")
		scheme          = flag.String("scheme", envOr("HOSTTHIS_PUBLIC_SCHEME", "https"), "public URL scheme (https for prod, http for local dev)")
		landingPath     = flag.String("landing", envOr("HOSTTHIS_LANDING", "web/landing.html"), "path to apex landing HTML")
		freshKeysLimit  = flag.Int("fresh-keys-per-subnet", envOrInt("HOSTTHIS_FRESH_KEYS_PER_SUBNET", 20), "max distinct new key fingerprints admitted per IP subnet per window")
		freshKeysWindow = flag.Duration("fresh-keys-window", envOrDuration("HOSTTHIS_FRESH_KEYS_WINDOW", 24*time.Hour), "rolling window for the Sybil rate limit on fresh keys")
		cpuProfile      = flag.String("cpuprofile", "", "write a CPU profile to this file until shutdown (local file; opens no network surface)")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "hostthis ", log.LstdFlags|log.LUTC)

	// Deliberately a file, not a /debug/pprof endpoint: a profiling handler on
	// a public daemon is a disclosure and denial-of-service surface, and this
	// answers the same question without listening anywhere.
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			logger.Fatalf("cpuprofile: %v", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			logger.Fatalf("cpuprofile: %v", err)
		}
		logger.Printf("cpu profile: writing to %s until shutdown", *cpuProfile)
		defer func() {
			pprof.StopCPUProfile()
			_ = f.Close()
		}()
	}

	if *apexDomain == "" {
		logger.Fatalf("--apex-domain is required (or set HOSTTHIS_APEX_DOMAIN). Pass the public domain hostthis serves on, e.g. paste.example.com.")
	}

	metadata, err := buildMetadata(*dataDir, logger)
	if err != nil {
		logger.Fatalf("metadata backend: %v", err)
	}
	defer func() {
		if err := metadata.Close(); err != nil {
			logger.Printf("metadata close: %v", err)
		}
	}()

	pasteRepo := metadata.Repo
	keyGateRepo := metadata.KeyGate
	blobs, blobsCleanup, err := buildBlobStore(*dataDir, logger)
	if err != nil {
		logger.Fatalf("blob store: %v", err)
	}
	defer blobsCleanup()

	// The per-record blob seam. A shale backend with a blob store configured
	// supplies a transactional shaleblob.Unit that co-commits the blob pointer
	// with the metadata; every other backend uses the standalone adapter over
	// the detached content-addressed store. The services see one shape either
	// way.
	var blobUnit service.BlobUnit = service.NewStandaloneBlobUnit(blobs)
	if metadata.BlobUnit != nil {
		blobUnit = metadata.BlobUnit
		logger.Printf("blobs: transactional shale-collocated blob plane (pointer co-commits with metadata)")
	}

	siteRepo := metadata.Sites
	roomRepo := metadata.Rooms

	// Per-identity create admission (docs/SPEC.md "Same-identity create
	// admission"): same-identity creates beyond the width queue BEFORE the
	// metadata commit, so a one-owner create storm cannot amplify in the
	// storage tier's CAS layer, while other identities pass independently.
	// A repo decorator, so the upload service stays admission-unaware.
	admissionWidth := envOrInt("HOSTTHIS_CREATE_ADMISSION_WIDTH", service.DefaultCreateAdmissionWidth)
	if admissionWidth < 1 {
		logger.Fatalf("HOSTTHIS_CREATE_ADMISSION_WIDTH must be >= 1, got %d", admissionWidth)
	}
	createGate := service.NewCreateAdmission(admissionWidth)
	uploadSvc := service.NewUpload(service.GateCreates(pasteRepo, createGate), blobUnit)
	uploadSvc.Logger = logger // record background blob-finalize outcomes
	// HOSTTHIS_BLOB_SYNC is a benchmark toggle for a sync-vs-async A/B on one
	// binary: Create writes the blob inline on the ack path instead of
	// finalizing in the background.
	if strings.EqualFold(os.Getenv("HOSTTHIS_BLOB_SYNC"), "true") {
		uploadSvc.SyncBlob = true
		logger.Printf("upload: HOSTTHIS_BLOB_SYNC=true (inline blob write; benchmark mode)")
	}
	manageSvc := service.NewManage(pasteRepo, blobUnit)

	// Static-site archive deploys reuse the same blob store and per-identity
	// quota as pastes. Nil when the metadata backend exposes no site repo.
	var deploySvc *service.DeploySite
	if siteRepo != nil {
		deploySvc = service.NewDeploySite(siteRepo, pasteRepo, blobUnit)
		// Without this the compensating slug-claim release fails silently.
		deploySvc.Logger = logger
		// One entry point: Create now dispatches the multi-file shape itself,
		// so no transport forks on content (docs/SPEC.md "One paste, not two
		// aggregates").
		uploadSvc.Archive = service.ArchiveAdapter{Deployer: deploySvc}
		// The quota cap sums paste + site bytes, so whoami's used_bytes
		// under-counts without this.
		manageSvc.SiteBytes = siteRepo
	}

	// Rooms: the no-auth, capability-based app-persistence tier under
	// /api/rooms. Nil when the metadata backend has no room repo.
	var roomsSvc *service.Rooms
	if roomRepo != nil {
		roomsSvc = service.NewRooms(roomRepo)
	}

	// Relay: the real-time per-room WebSocket layer over the rooms tier (SPEC
	// "Real-time room relay (WebSocket)"). It depends on the rooms service only
	// for the late-join snapshot; persistence goes through the HTTP PUT/DELETE
	// mirror. Per-room hubs are in-memory and per-pod. Nil without a room repo.
	var roomRelay *relay.Relay
	if roomsSvc != nil {
		roomRelay = relay.NewRelay(roomsSvc, relay.NewLimits())
	}

	// Multi-pod relay peer fan-out (SPEC "Multi-pod relay"). A multi-node shale
	// backend supplies both directions: an outbound publisher that fans frames
	// to every peer pod over the cluster gRPC tier, and a late-bound receive
	// hook that broadcasts a peer's frames into this pod's local hubs. A
	// single-pod backend leaves RelayPeer nil, the zero-peer degenerate case.
	if roomRelay != nil && metadata.RelayPeer != nil {
		roomRelay.SetPeerPublisher(metadata.RelayPeer.Publisher)
		metadata.RelayPeer.Bind(roomRelay.DeliverFromPeer)
		logger.Printf("relay: multi-pod peer fan-out wired (publish + receive on the cluster gRPC tier)")
	}

	keyGate := service.NewKeyGate(keyGateRepo)
	keyGate.MaxFreshKeysPerSubnet = *freshKeysLimit
	keyGate.Window = *freshKeysWindow
	// Whoami reports per-session subnet and budget info from the keygate.
	manageSvc.KeyGate = keyGate
	logger.Printf("config: fresh_keys/subnet=%d per %s (durable total-bytes ceiling is the object-store bucket quota)",
		*freshKeysLimit, *freshKeysWindow)

	landing, err := os.ReadFile(*landingPath)
	if err != nil {
		logger.Printf("warn: landing not loaded from %q: %v (apex will serve a stub)", *landingPath, err)
	}
	// The landing template carries {{APEX}} everywhere a hostname appears, so
	// the page never advertises a domain this deploy does not serve.
	if len(landing) > 0 {
		landing = []byte(strings.ReplaceAll(string(landing), "{{APEX}}", *apexDomain))
	}

	build := buildURL(*scheme, *apexDomain, *urlMode, logger)

	// The decorator wraps the verb service so a mutation transparently
	// invalidates the edge cache for the affected slug, keeping the verb
	// service cache-unaware (SPEC "Active invalidation: CachePurger"). Noop
	// unless a CDN is configured.
	cachePurger := buildCachePurger(logger, *scheme, *apexDomain, *urlMode)
	pasteMgr := service.NewCacheInvalidating(manageSvc, cachePurger)

	// Own registry rather than the default one: what this process publishes is
	// then exactly what is registered here, with no collectors arriving via a
	// dependency's init().
	metricsReg := prometheus.NewRegistry()
	metricsReg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	appMetrics := metrics.New(metricsReg)

	sshServer := &hostssh.Server{
		Addr:        *sshAddr,
		HostKeyPath: filepath.Join(*dataDir, "ssh_host_ed25519_key"),
		ApexDomain:  *apexDomain,
		Upload:      uploadSvc,
		Deploy:      deploySvc, // nil when the backend has no site repo
		Manage:      pasteMgr,
		Pastes:      pasteRepo,
		Now:         time.Now,
		KeyGate:     keyGate,
		BuildURL:    build,
		Logger:      logger,
		Metrics:     appMetrics,
	}
	if siteRepo != nil {
		sshServer.Sites = siteRepo
	}

	httpServer := &httpapi.Server{
		Pastes:      pasteRepo,
		Blobs:       blobUnit,
		LandingHTML: landing,
		ApexDomain:  *apexDomain,
		Color:       envOr("HOSTTHIS_BACKEND_COLOR", ""),
		// Readiness gates /readyz on the metadata backend's predicate (the
		// shale mount floor); nil on a backend with no mount concept, which the
		// server reads as always-ready. /healthz stays pure liveness.
		Readiness: metadata.Readiness,
		Logf:      logger.Printf,
	}
	if siteRepo != nil {
		httpServer.Sites = siteRepo
	}
	if roomsSvc != nil {
		httpServer.Rooms = roomsSvc
	}
	if roomRelay != nil {
		httpServer.Relay = roomRelay
	}
	// Metrics listen on their OWN port, never the public one. The public mux
	// answers /healthz on any Host without auth, so adding /metrics there
	// would publish request rates, verb mix and failure counts to anyone who
	// asked. A separate listener is not routed by the ingress at all.
	metricsSrv := &http.Server{
		Addr:              *metricsAddr,
		Handler:           promhttp.HandlerFor(metricsReg, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	httpSrv := &http.Server{
		Addr:    *httpAddr,
		Handler: httpServer.Handler(),
		// Bound the four axes a slow or hostile client could hold open. Reads
		// are tiny and writes are at most MaxPasteBytes, so these are generous
		// but never unbounded.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10, // 8 KiB
	}

	// Both servers and the sweep run concurrently; the first signalling event
	// wins and tears them all down.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errs := make(chan error, 3)
	go func() { errs <- sshServer.ListenAndServe() }()
	go func() {
		logger.Printf("http: listening on %s", *httpAddr)
		errs <- httpSrv.ListenAndServe()
	}()
	go func() {
		logger.Printf("metrics: listening on %s", *metricsAddr)
		errs <- metricsSrv.ListenAndServe()
	}()
	// The sweep loop always runs: HOSTTHIS_SWEEP_DISABLED selects dry-run vs
	// live, it does not gate the goroutine, because a dry-run sweep must still
	// run to log what it would clean.

	// Settle durable intents left by a process death mid-write, ONCE, and only
	// now: deciding an intent reads the shard holding its authoritative row,
	// which may not be mounted anywhere until the cluster is up. Gating
	// readiness on it would deadlock a cold cluster - no node could serve until
	// it swept, and none could sweep until one served. Running late costs
	// nothing: the residue it clears was already there (docs/SPEC.md "Durable
	// intent").
	if metadata.IntentSweeper != nil {
		go runIntentSweep(ctx, metadata, logger)
	}

	select {
	case <-ctx.Done():
		logger.Printf("signal received; shutting down")
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("server error: %v", err)
		}
	}

	// Drain hint, grace window, then close (SPEC "Drain hint:
	// reconnect-before-shutdown"). The hint fires BEFORE the HTTP server stops
	// accepting, and the process keeps serving through HOSTTHIS_DRAIN_GRACE (0
	// disables) so the hint flushes and clients acting on it reconnect
	// make-before-break onto a surviving pod.
	if roomRelay != nil {
		roomRelay.AnnounceDrain()
		if grace := envOrDuration("HOSTTHIS_DRAIN_GRACE", 3*time.Second); grace > 0 {
			logger.Printf("relay: drain hint broadcast; serving through %s grace before close", grace)
			time.Sleep(grace)
		}
	}

	// http.Server.Shutdown does not track hijacked WebSockets, so closing them
	// here unblocks their request goroutines and lets clients reconnect on
	// their backoff schedule rather than hammering instantly.
	if roomRelay != nil {
		roomRelay.Registry().CloseAll()
	}

	// Local fan-out is done, so drop the outbound peer queues and connections.
	if metadata.RelayPeer != nil {
		metadata.RelayPeer.Close()
	}

	shutdownCtx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// buildBlobStore reads HOSTTHIS_BLOB_BACKEND and returns the configured store.
// Disk is the only standalone backend. The shale metadata backend does NOT go
// through this detached store: its ShaleRepo owns a shale-managed blob plane of
// its own.
func buildBlobStore(dataDir string, logger *log.Logger) (*storage.CompressedBlobStore, func(), error) {
	backend := strings.ToLower(envOr("HOSTTHIS_BLOB_BACKEND", "disk"))
	switch backend {
	case "", "disk":
		bs, err := storage.NewBlobStore(filepath.Join(dataDir, "blobs"))
		if err != nil {
			return nil, nil, err
		}
		logger.Printf("blobs: disk backend at %s/blobs (zstd-compressed at rest)", dataDir)
		inner, cleanup, err := maybeWrapWriteBack(bs, dataDir, logger)
		if err != nil {
			return nil, nil, err
		}
		return storage.NewCompressedBlobStore(inner), cleanup, nil
	default:
		return nil, nil, fmt.Errorf("unknown HOSTTHIS_BLOB_BACKEND %q (only 'disk' is supported as a standalone backend; production uses the shale-collocated blob plane)", backend)
	}
}

// writeBackInner is what maybeWrapWriteBack needs of a durable backend: the
// Put/Get/GetReader the compression layer wraps, plus the WalkBlobs/Remove the
// write-back cache's own eviction uses.
type writeBackInner interface {
	Put(sha string, r io.Reader, size int64) error
	Get(sha string) ([]byte, error)
	GetReader(sha string) (io.ReadCloser, int64, error)
	WalkBlobs(fn func(sha string) error) error
	Remove(sha string) error
}

// maybeWrapWriteBack fronts the durable backend with the local-disk write-back
// cache when HOSTTHIS_BLOB_WRITEBACK=true. Disabled, it returns the durable
// backend unchanged, preserving strict durable-before-ack. The cleanup func
// stops the uploaders and is a no-op when disabled.
func maybeWrapWriteBack(durable writeBackInner, dataDir string, logger *log.Logger) (storage.InnerBlobStore, func(), error) {
	if strings.ToLower(envOr("HOSTTHIS_BLOB_WRITEBACK", "false")) != "true" {
		return durable, func() {}, nil
	}
	cfg := storage.WriteBackConfig{
		Dir:      envOr("HOSTTHIS_BLOB_WRITEBACK_DIR", filepath.Join(dataDir, "blob-cache")),
		MaxBytes: envOrInt64("HOSTTHIS_BLOB_WRITEBACK_MAX_BYTES", 1<<30),
		Logger:   logger,
	}
	wb, err := storage.NewWriteBackBlobStore(durable, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("blob write-back cache: %w", err)
	}
	logger.Printf("blobs: write-back cache ENABLED at %s (max %d bytes); durability window applies, see SPEC", cfg.Dir, cfg.MaxBytes)
	return wb, wb.Close, nil
}

// buildCachePurger reads HOSTTHIS_CACHE_BACKEND and returns the configured
// CachePurger, defaulting to Noop. scheme/apex/mode let the cloudflare adapter
// build every public URL variant of a slug (the page plus the markdown shell's
// "?raw=1" fetch), so a purge invalidates every cache key it is reachable at.
func buildCachePurger(logger *log.Logger, scheme, apex, mode string) service.CachePurger {
	backend := strings.ToLower(envOr("HOSTTHIS_CACHE_BACKEND", "noop"))
	switch backend {
	case "", "noop":
		return cache.Noop{}
	case "cloudflare":
		zone := os.Getenv("HOSTTHIS_CF_ZONE_ID")
		token := os.Getenv("HOSTTHIS_CF_PURGE_TOKEN")
		if zone == "" || token == "" {
			logger.Fatalf("HOSTTHIS_CACHE_BACKEND=cloudflare requires HOSTTHIS_CF_ZONE_ID and HOSTTHIS_CF_PURGE_TOKEN")
		}
		logger.Printf("cache: cloudflare purger enabled for zone %s", zone)
		return &cache.Cloudflare{ZoneID: zone, Token: token, Scheme: scheme, Apex: apex, Mode: mode, Logger: logger}
	default:
		logger.Fatalf("unknown HOSTTHIS_CACHE_BACKEND %q (want noop|cloudflare)", backend)
		return nil
	}
}

// buildURL returns the URL emitter for a scheme, mode and apex. Subdomain mode
// is required in production; path mode is dev-only (SPEC "Dev-only path mode").
func buildURL(scheme, apex, mode string, logger *log.Logger) hostssh.URLBuilder {
	switch strings.ToLower(mode) {
	case "subdomain":
		return func(slug domain.Slug) string {
			return scheme + "://" + slug.String() + "." + apex
		}
	case "path":
		logger.Printf("WARN running in path mode - origin isolation is dev-only. " +
			"Production deploys MUST use --mode subdomain.")
		return func(slug domain.Slug) string {
			return scheme + "://" + apex + "/p/" + slug.String()
		}
	default:
		logger.Fatalf("unknown --mode %q (want subdomain|path)", mode)
		return nil
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// A malformed value is a configuration ERROR, not a reason to fall back. The
// operator set the variable deliberately, and silently substituting the default
// leaves the startup log confirming a value they never got. Exits rather than
// returning an error because these are read during flag setup, before there is
// anywhere to return one to.
func configFatal(key, val, want string) {
	fmt.Fprintf(os.Stderr, "hostthisd: %s=%q is not %s\n", key, val, want)
	os.Exit(2)
}

func envOrInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		configFatal(key, v, "an integer")
	}
	return n
}

func envOrInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		configFatal(key, v, "an integer")
	}
	return n
}

func envOrDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// runIntentSweep settles durable intents and reclaims abandoned staged bytes
// once, after the node is serving.
//
// It does NOT wait for readiness first. The scan itself retries while the
// node's positions are still acquiring (storage.bootRetry), which is the same
// refusal readiness is waiting on - so a second gate here would only duplicate
// it, and readiness at the mount FLOOR is not the same condition as "this
// node's own units are scannable" anyway.
func runIntentSweep(ctx context.Context, metadata *metadataBundle, logger *log.Logger) {
	now := time.Now().UTC()
	settled, err := metadata.IntentSweeper.SweepIntents(ctx, now)
	if err != nil {
		logger.Printf("intent sweep: %v (the next boot retries; nothing is lost)", err)
	} else if settled > 0 {
		logger.Printf("intent sweep: settled %d half-finished write(s) on this node's units", settled)
	}

	// Runs even when the intent sweep failed: the two settle different things,
	// and bytes nothing points at are not worth withholding over a metadata
	// problem.
	reclaimed, err := metadata.IntentSweeper.SweepStagedBytes(ctx, now)
	if err != nil {
		logger.Printf("staged sweep: %v (the next boot retries; the bytes stay put)", err)
		return
	}
	if reclaimed > 0 {
		logger.Printf("staged sweep: reclaimed the staged bytes of %d abandoned upload(s)", reclaimed)
	}
}
