// Package main wires the hostthis daemon: SSH server, HTTP server and storage,
// configured from flags and HOSTTHIS_* env.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	httppprof "net/http/pprof"
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
		freshKeysLimit  = flag.Int("fresh-keys-per-subnet", envParse("HOSTTHIS_FRESH_KEYS_PER_SUBNET", 20, strconv.Atoi, "an integer"), "max distinct new key fingerprints admitted per IP subnet per window")
		freshKeysWindow = flag.Duration("fresh-keys-window", envParse("HOSTTHIS_FRESH_KEYS_WINDOW", 24*time.Hour, time.ParseDuration, "a duration"), "rolling window for the Sybil rate limit on fresh keys")
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

	metadata, err := buildMetadata(*dataDir, *apexDomain, logger)
	if err != nil {
		logger.Fatalf("metadata backend: %v", err)
	}
	pasteRepo := metadata.Repo
	keyGateRepo := metadata.KeyGate
	blobs, blobsCleanup, err := buildBlobStore(*dataDir, logger)
	if err != nil {
		logger.Fatalf("blob store: %v", err)
	}

	blobUnit := service.NewStandaloneBlobUnit(blobs)

	siteRepo := metadata.Sites
	roomRepo := metadata.Rooms

	// Per-identity create admission (docs/SPEC.md "Same-identity create
	// admission"): same-identity creates beyond the width queue BEFORE the
	// metadata commit, so a one-owner create storm cannot amplify in the
	// storage tier's CAS layer, while other identities pass independently.
	// A repo decorator, so the upload service stays admission-unaware.
	admissionWidth := envParse("HOSTTHIS_CREATE_ADMISSION_WIDTH", service.DefaultCreateAdmissionWidth, strconv.Atoi, "an integer")
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
		// One entry point: Create now dispatches the multi-file shape itself,
		// so no transport forks on content (docs/SPEC.md "One paste, not two
		// aggregates").
		uploadSvc.Archive = service.ArchiveAdapter{Deployer: deploySvc}
	}

	// Rooms: the no-auth, capability-based app-persistence tier under
	// /api/rooms. Nil when the metadata backend has no room repo.
	var roomsSvc *service.Rooms
	var roomPushSvc *service.RoomPush
	if roomRepo != nil {
		roomsSvc = service.NewRooms(roomRepo)
		roomPushSvc = service.NewRoomPush(roomRepo)
	}

	// Relay: the real-time per-room WebSocket layer over the rooms tier (SPEC
	// "Real-time room relay (WebSocket)"). It depends on the rooms service only
	// for the late-join snapshot; persistence goes through the HTTP PUT/DELETE
	// mirror. Per-room hubs are in-memory and per-pod. Nil without a room repo.
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
		Logf:        logger.Printf,
	}
	if siteRepo != nil {
		httpServer.Sites = siteRepo
	}
	if roomsSvc != nil {
		httpServer.Rooms = roomsSvc
		httpServer.RoomPush = roomPushSvc
	}
	var relayDrain relayShutdowner = idleRelay{}
	if metadata.RoomRelay != nil {
		httpServer.Relay = metadata.RoomRelay
		lifecycle, ok := metadata.RoomRelay.(relayShutdowner)
		if !ok {
			logger.Fatalf("cell relay does not implement the shutdown lifecycle")
		}
		relayDrain = lifecycle
		logger.Printf("relay: cell proxy (the room cell is the broadcast point)")
	}
	// Metrics listen on their OWN port, never the public one. The public mux
	// answers /healthz on any Host without auth, so adding /metrics there
	// would publish request rates, verb mix and failure counts to anyone who
	// asked. A separate listener is not routed by the ingress at all.
	// pprof rides the same private listener. A goroutine dump taken during a
	// slow command names the call it is blocked in, which counters and CPU
	// profiles cannot; and pprof exposes stacks and heap contents, so it must
	// never appear on the public mux. Explicit routes rather than importing
	// net/http/pprof for its side effect on DefaultServeMux, which this
	// process never serves.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(metricsReg, promhttp.HandlerOpts{}))
	metricsMux.HandleFunc("/debug/pprof/", httppprof.Index)
	metricsMux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
	metricsMux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
	metricsMux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
	metricsMux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
	metricsSrv := &http.Server{
		Addr:    *metricsAddr,
		Handler: metricsMux,
		// Generous where the public server is strict: a 30s CPU profile or an
		// execution trace legitimately holds the response open far longer than
		// any public request may.
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

	// The servers run concurrently; the first signalling event wins and tears
	// them all down.
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
	select {
	case <-ctx.Done():
		logger.Printf("signal received; shutting down")
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("server error: %v", err)
		}
	}

	shutdownCtx, scancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer scancel()
	if err := shutdownDaemon(
		shutdownCtx,
		20*time.Second,
		httpSrv,
		metricsSrv,
		relayDrain,
		sshServer,
		uploadSvc.WaitFinalize,
		blobsCleanup,
	); err != nil {
		logger.Printf("shutdown: %v", err)
	}
}

// buildBlobStore reads HOSTTHIS_BLOB_BACKEND and returns the configured store:
// the raw backend, optionally fronted by the write-back cache, under the
// compression layer.
func buildBlobStore(dataDir string, logger *log.Logger) (*storage.CompressedBlobStore, func(), error) {
	var raw storage.InnerBlobStore
	backend := strings.ToLower(envOr("HOSTTHIS_BLOB_BACKEND", "disk"))
	switch backend {
	case "", "disk":
		bs, err := storage.NewBlobStore(filepath.Join(dataDir, "blobs"))
		if err != nil {
			return nil, nil, err
		}
		logger.Printf("blobs: disk backend at %s/blobs (zstd-compressed at rest)", dataDir)
		raw = bs
	case "s3":
		// The celld backend holds no bytes, so the byte plane needs a durable
		// home of its own. Content-addressed, exactly like disk: interchangeable
		// layouts mean moving between them is a key rename, never a re-encode.
		bs, err := storage.NewS3BlobStore(storage.S3BlobConfig{
			Endpoint:  envOr("HOSTTHIS_S3_ENDPOINT", ""),
			Bucket:    envOr("HOSTTHIS_S3_BUCKET", ""),
			Region:    envOr("HOSTTHIS_S3_REGION", "us-east-1"),
			AccessKey: envOr("HOSTTHIS_S3_ACCESS_KEY", ""),
			SecretKey: envOr("HOSTTHIS_S3_SECRET_KEY", ""),
			UseSSL:    strings.EqualFold(envOr("HOSTTHIS_S3_USE_SSL", "false"), "true"),
			Prefix:    envOr("HOSTTHIS_S3_BLOB_PREFIX", "blob"),
		})
		if err != nil {
			return nil, nil, err
		}
		logger.Printf("blobs: s3 backend at %s/%s (zstd-compressed at rest)",
			envOr("HOSTTHIS_S3_BUCKET", ""), envOr("HOSTTHIS_S3_BLOB_PREFIX", "blob"))
		raw = bs
	default:
		return nil, nil, fmt.Errorf("unknown HOSTTHIS_BLOB_BACKEND %q (want disk|s3)", backend)
	}
	inner, cleanup, err := maybeWrapWriteBack(raw, dataDir, logger)
	if err != nil {
		return nil, nil, err
	}
	return storage.NewCompressedBlobStore(inner), cleanup, nil
}

// maybeWrapWriteBack fronts the durable backend with the local-disk write-back
// cache when HOSTTHIS_BLOB_WRITEBACK=true. Disabled, it returns the durable
// backend unchanged, preserving strict durable-before-ack. The cleanup func
// stops the uploaders and is a no-op when disabled.
func maybeWrapWriteBack(durable storage.InnerBlobStore, dataDir string, logger *log.Logger) (storage.InnerBlobStore, func(), error) {
	if strings.ToLower(envOr("HOSTTHIS_BLOB_WRITEBACK", "false")) != "true" {
		return durable, func() {}, nil
	}
	cfg := storage.WriteBackConfig{
		Dir:      envOr("HOSTTHIS_BLOB_WRITEBACK_DIR", filepath.Join(dataDir, "blob-cache")),
		MaxBytes: envParse("HOSTTHIS_BLOB_WRITEBACK_MAX_BYTES", int64(1<<30), parseInt64, "an integer"),
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

// envParse reads key through parse, or returns fallback when it is unset.
//
// A malformed value is a configuration ERROR, not a reason to fall back. The
// operator set the variable deliberately, and silently substituting the default
// leaves the startup log confirming a value they never got. Exits rather than
// returning an error because these are read during flag setup, before there is
// anywhere to return one to.
func envParse[T any](key string, fallback T, parse func(string) (T, error), want string) T {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := parse(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostthisd: %s=%q is not %s\n", key, v, want)
		os.Exit(2)
	}
	return n
}

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
