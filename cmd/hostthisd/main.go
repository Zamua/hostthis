// Package main wires the hostthis daemon: SSH server, HTTP server, storage and
// the periodic expiry sweep, configured from flags and HOSTTHIS_* env.
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
	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/relay"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

func main() {
	var (
		dataDir         = flag.String("data-dir", envOr("HOSTTHIS_DATA_DIR", "./data"), "where sqlite + blobs live")
		sshAddr         = flag.String("ssh-addr", envOr("HOSTTHIS_SSH_ADDR", ":2222"), "ssh listen address")
		httpAddr        = flag.String("http-addr", envOr("HOSTTHIS_HTTP_ADDR", ":8080"), "http listen address")
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

	// HOSTTHIS_RETENTION: how long a paste or site lives past its last update
	// before the sweep evicts it. Injected into the metadata backends and the
	// upload/site services.
	retention, err := parseRetention(os.Getenv("HOSTTHIS_RETENTION"), domain.DefaultRetention())
	if err != nil {
		logger.Fatalf("%v", err)
	}
	logger.Printf("config: retention=%s (paste/site TTL from last update)", retention.Describe())

	metadata, err := buildMetadata(*dataDir, retention, logger)
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
	blobs, blobsSweep, blobsCleanup, err := buildBlobStore(*dataDir, logger)
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
	uploadSvc.Retention = retention
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
		deploySvc.Retention = retention
		// Without this the compensating slug-claim release fails silently.
		deploySvc.Logger = logger
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
	sweepSvc := service.NewSweep(pasteRepo, blobsSweep, logger)
	sweepSvc.KeyGate = keyGate
	// On the transactional shale-blob path the cluster owns the blobs: a delete
	// unbinds the pointer inside the metadata-delete transaction, so the global
	// content-addressed GC over the detached store is disabled (Blobs=nil) and
	// SweepOrphans reclaims orphan bytes instead.
	if metadata.BlobOrphanSweeper != nil {
		sweepSvc.Blobs = nil
		sweepSvc.BlobOrphans = metadata.BlobOrphanSweeper
		logger.Printf("sweep: shale-blob path - global content-addressed blob GC disabled; SweepOrphans reclaims staged-but-unbound objects (grace %s)", service.DefaultOrphanGrace)
	}
	if siteRepo != nil {
		// Site expiry plus site-blob GC protection.
		sweepSvc.Sites = siteRepo
	}
	if roomRepo != nil {
		// Room inactivity expiry plus the room-create rate-limit prune.
		sweepSvc.Rooms = roomRepo
	}

	// HOSTTHIS_SWEEP_DISABLED selects DRY-RUN vs LIVE; it is not an on/off
	// switch and a "disabled" sweep is never a no-op. True means the sweep
	// still runs every interval, computing and LOGGING what it would expire or
	// GC while mutating nothing, so a risky change can be deployed and the
	// dry-run log read before flipping to live. See docs/SPEC.md "Dry-run
	// (observability)".
	sweepSvc.DryRun = strings.EqualFold(envOr("HOSTTHIS_SWEEP_DISABLED", "false"), "true")
	if sweepSvc.DryRun {
		logger.Printf("sweep: DRY-RUN via HOSTTHIS_SWEEP_DISABLED=true - runs every %s, LOGS what it would expire/GC, deletes nothing. Set false to enable live cleanup.", sweepSvc.Interval)
	} else {
		logger.Printf("sweep: LIVE - periodic expiry + blob GC + key-gate prune every %s", sweepSvc.Interval)
	}

	logger.Printf("config: fresh_keys/subnet=%d per %s (durable total-bytes ceiling is the object-store bucket quota)",
		*freshKeysLimit, *freshKeysWindow)

	landing, err := os.ReadFile(*landingPath)
	if err != nil {
		logger.Printf("warn: landing not loaded from %q: %v (apex will serve a stub)", *landingPath, err)
	}
	// The landing template carries {{APEX}} everywhere a hostname appears and
	// {{RETENTION}} where the expiry is described, so the page never advertises
	// a domain or a TTL this deploy does not have.
	if len(landing) > 0 {
		s := strings.ReplaceAll(string(landing), "{{APEX}}", *apexDomain)
		retPhrase := "with no expiry"
		if retention.Enabled() {
			retPhrase = "for " + retention.Describe()
		}
		landing = []byte(strings.ReplaceAll(s, "{{RETENTION}}", retPhrase))
	}

	build := buildURL(*scheme, *apexDomain, *urlMode, logger)

	// The decorator wraps the verb service so a mutation transparently
	// invalidates the edge cache for the affected slug, keeping the verb
	// service cache-unaware (SPEC "Active invalidation: CachePurger"). Noop
	// unless a CDN is configured.
	cachePurger := buildCachePurger(logger, *scheme, *apexDomain, *urlMode)
	pasteMgr := service.NewCacheInvalidating(manageSvc, cachePurger)

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

	errs := make(chan error, 2)
	go func() { errs <- sshServer.ListenAndServe() }()
	go func() {
		logger.Printf("http: listening on %s", *httpAddr)
		errs <- httpSrv.ListenAndServe()
	}()
	// The sweep loop always runs: HOSTTHIS_SWEEP_DISABLED selects dry-run vs
	// live, it does not gate the goroutine, because a dry-run sweep must still
	// run to log what it would clean.
	go sweepSvc.Run(ctx)

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

// buildBlobStore reads HOSTTHIS_BLOB_BACKEND and returns the configured store,
// narrowed through two interfaces (BlobStore and SweepBlobs). Disk is the only
// standalone backend. The shale metadata backend does NOT go through this
// detached store: its ShaleRepo owns a shale-managed blob plane of its own.
func buildBlobStore(dataDir string, logger *log.Logger) (*storage.CompressedBlobStore, service.SweepBlobs, func(), error) {
	backend := strings.ToLower(envOr("HOSTTHIS_BLOB_BACKEND", "disk"))
	switch backend {
	case "", "disk":
		bs, err := storage.NewBlobStore(filepath.Join(dataDir, "blobs"))
		if err != nil {
			return nil, nil, nil, err
		}
		logger.Printf("blobs: disk backend at %s/blobs (zstd-compressed at rest)", dataDir)
		// Only the upload/manage Put/Get path needs the compression wrapper.
		// The sweep uses WalkBlobs + Remove, which are sha-only and never touch
		// a body, so it talks to the raw backend.
		inner, sweep, cleanup, err := maybeWrapWriteBack(bs, dataDir, logger)
		if err != nil {
			return nil, nil, nil, err
		}
		return storage.NewCompressedBlobStore(inner), sweep, cleanup, nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown HOSTTHIS_BLOB_BACKEND %q (only 'disk' is supported as a standalone backend; production uses the shale-collocated blob plane)", backend)
	}
}

// writeBackInner is what maybeWrapWriteBack needs of a durable backend: the
// Put/Get/GetReader the compression layer wraps, plus the WalkBlobs/Remove the
// sweep uses.
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
func maybeWrapWriteBack(durable writeBackInner, dataDir string, logger *log.Logger) (storage.InnerBlobStore, service.SweepBlobs, func(), error) {
	if strings.ToLower(envOr("HOSTTHIS_BLOB_WRITEBACK", "false")) != "true" {
		return durable, durable, func() {}, nil
	}
	cfg := storage.WriteBackConfig{
		Dir:      envOr("HOSTTHIS_BLOB_WRITEBACK_DIR", filepath.Join(dataDir, "blob-cache")),
		MaxBytes: envOrInt64("HOSTTHIS_BLOB_WRITEBACK_MAX_BYTES", 1<<30),
		Logger:   logger,
	}
	wb, err := storage.NewWriteBackBlobStore(durable, cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("blob write-back cache: %w", err)
	}
	logger.Printf("blobs: write-back cache ENABLED at %s (max %d bytes); durability window applies, see SPEC", cfg.Dir, cfg.MaxBytes)
	return wb, wb, wb.Close, nil
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

// parseRetention reads HOSTTHIS_RETENTION into a policy. Accepted forms,
// case-insensitive:
//
//	""                                 -> the supplied default
//	"off" / "never" / "none" / "0"     -> no expiry (content is never swept)
//	"<N>d"                             -> N days (e.g. "30d", "7d")
//	anything time.ParseDuration takes  -> that duration (e.g. "12h", "720h")
func parseRetention(raw string, def domain.Retention) (domain.Retention, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	switch s {
	case "":
		return def, nil
	case "off", "never", "none", "disabled", "0":
		return domain.Retention{Window: 0}, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return domain.Retention{Window: time.Duration(n) * 24 * time.Hour}, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def, fmt.Errorf("HOSTTHIS_RETENTION=%q is not valid (use e.g. 30d, 12h, or off)", raw)
	}
	return domain.Retention{Window: d}, nil
}
