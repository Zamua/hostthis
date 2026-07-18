// Package main wires the hostthis daemon: SSH server + HTTP server +
// storage + the periodic expiry sweep. Reads flags / env for the
// runtime config (apex domain, URL mode, scheme, ports, data dir).
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
	)
	flag.Parse()

	logger := log.New(os.Stderr, "hostthis ", log.LstdFlags|log.LUTC)

	if *apexDomain == "" {
		logger.Fatalf("--apex-domain is required (or set HOSTTHIS_APEX_DOMAIN). Pass the public domain hostthis serves on, e.g. paste.example.com.")
	}

	// Content TTL policy (HOSTTHIS_RETENTION): how long a paste/site lives after
	// its last update before the sweep evicts it. Default 30 days; "off" never
	// expires. Injected into the metadata backends + the upload/site services.
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
	// supplies a transactional shaleblob.Unit (the metadata co-commits the blob
	// pointer); every other backend uses the standalone adapter over the
	// detached content-addressed store. Either way the services run one shape.
	var blobUnit service.BlobUnit = service.NewStandaloneBlobUnit(blobs)
	if metadata.BlobUnit != nil {
		blobUnit = metadata.BlobUnit
		logger.Printf("blobs: transactional shale-collocated blob plane (pointer co-commits with metadata)")
	}

	siteRepo := metadata.Sites
	roomRepo := metadata.Rooms

	// Per-identity create admission (docs/SPEC.md "Same-identity create
	// admission: a width-2 gate"): same-identity creates beyond the width
	// queue BEFORE the metadata commit, so a one-owner create storm cannot
	// amplify in the storage tier's CAS layer; other identities pass
	// independently. Wired here as a repo decorator so the upload service
	// stays admission-unaware.
	admissionWidth := envOrInt("HOSTTHIS_CREATE_ADMISSION_WIDTH", service.DefaultCreateAdmissionWidth)
	if admissionWidth < 1 {
		logger.Fatalf("HOSTTHIS_CREATE_ADMISSION_WIDTH must be >= 1, got %d", admissionWidth)
	}
	createGate := service.NewCreateAdmission(admissionWidth)
	uploadSvc := service.NewUpload(service.GateCreates(pasteRepo, createGate), blobUnit)
	uploadSvc.Retention = retention
	uploadSvc.Logger = logger // record background blob-finalize outcomes
	// HOSTTHIS_BLOB_SYNC is a BENCHMARK toggle (sync vs async A/B on one
	// binary): when true, Create writes the blob inline on the ack path
	// (the pre-async shape) instead of finalizing in the background.
	if strings.EqualFold(os.Getenv("HOSTTHIS_BLOB_SYNC"), "true") {
		uploadSvc.SyncBlob = true
		logger.Printf("upload: HOSTTHIS_BLOB_SYNC=true (inline blob write; benchmark mode)")
	}
	manageSvc := service.NewManage(pasteRepo, blobUnit)

	// Static-site archive deploys. Reuses the same blob store + the same
	// per-identity quota as pastes; nil-safe if the metadata backend
	// doesn't expose a site repo.
	var deploySvc *service.DeploySite
	if siteRepo != nil {
		deploySvc = service.NewDeploySite(siteRepo, pasteRepo, blobUnit)
		deploySvc.Retention = retention
		// So whoami's used_bytes includes static-site bytes (the quota cap sums
		// paste + site; without this the reported total under-counts sites).
		manageSvc.SiteBytes = siteRepo
	}

	// Rooms: the no-auth, capability-based app-persistence tier
	// (POST/GET/PUT/DELETE under /api/rooms on an app subdomain). Reuses
	// the same metadata backend; nil-safe if the backend has no room repo.
	var roomsSvc *service.Rooms
	if roomRepo != nil {
		roomsSvc = service.NewRooms(roomRepo)
	}

	// Relay: the real-time per-room WebSocket relay layered on the rooms
	// tier (see SPEC.md "Real-time room relay (WebSocket)"). It depends on
	// the rooms service only for the late-join snapshot (the Scan verb) and
	// reuses the durable KV for persistence via the HTTP PUT/DELETE mirror.
	// Single-node, in-memory per-room hubs; nil-safe when rooms are not
	// wired (no relay surface on a backend without a room repo).
	var roomRelay *relay.Relay
	if roomsSvc != nil {
		roomRelay = relay.NewRelay(roomsSvc, relay.NewLimits())
	}

	// Multi-pod relay peer fan-out (SPEC "Multi-pod relay"). A multi-node
	// shale backend supplies the transport: the outbound publisher (frames
	// fan out to every peer pod over the cluster gRPC tier) and the
	// late-bound receive hook (a peer's frames broadcast into THIS pod's
	// local hubs). Every single-pod backend leaves RelayPeer nil and the
	// relay keeps its nil publisher - the zero-peer degenerate case, the
	// single-pod relay unchanged.
	if roomRelay != nil && metadata.RelayPeer != nil {
		roomRelay.SetPeerPublisher(metadata.RelayPeer.Publisher)
		metadata.RelayPeer.Bind(roomRelay.DeliverFromPeer)
		logger.Printf("relay: multi-pod peer fan-out wired (publish + receive on the cluster gRPC tier)")
	}

	keyGate := service.NewKeyGate(keyGateRepo)
	keyGate.MaxFreshKeysPerSubnet = *freshKeysLimit
	keyGate.Window = *freshKeysWindow
	// Whoami uses the keygate for per-session subnet/budget info.
	manageSvc.KeyGate = keyGate
	sweepSvc := service.NewSweep(pasteRepo, blobsSweep, logger)
	sweepSvc.KeyGate = keyGate
	// On the transactional shale-blob path the cluster owns the blobs: a delete
	// unbinds the pointer in the metadata-delete transaction, so the global
	// content-addressed GC over the detached store is skipped (Blobs=nil) and
	// orphan BYTES are reclaimed by SweepOrphans instead. The detached store is
	// not the blob backend on that path.
	if metadata.BlobOrphanSweeper != nil {
		sweepSvc.Blobs = nil
		sweepSvc.BlobOrphans = metadata.BlobOrphanSweeper
		logger.Printf("sweep: shale-blob path - global content-addressed blob GC disabled; SweepOrphans reclaims staged-but-unbound objects (grace %s)", service.DefaultOrphanGrace)
	}
	if siteRepo != nil {
		// Wire site expiry + site-blob GC protection into the sweep.
		sweepSvc.Sites = siteRepo
	}
	if roomRepo != nil {
		// Wire room expiry (30-day inactivity TTL) + the room-create
		// rate-limit prune into the sweep.
		sweepSvc.Rooms = roomRepo
	}

	// HOSTTHIS_SWEEP_DISABLED toggles the sweep between DRY-RUN and LIVE -
	// it is NOT an on/off switch and a "disabled" sweep is never a no-op.
	// true (the default-safe operator handle for a cutover/nervous window):
	// the sweep still runs every interval, computing + LOGGING what it WOULD
	// expire/GC, but mutating nothing. false: live cleanup. So an operator
	// can deploy a risky change, watch the dry-run log confirm the sweep
	// would clean only what's expected, then flip to false. See docs/SPEC.md
	// "Dry-run (observability)".
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
	// Substitute the apex placeholder so the landing page tells visitors
	// to ssh to the actual configured domain. The template ships with
	// `{{APEX}}` everywhere a hostname appears (e.g. `ssh {{APEX}} list`).
	if len(landing) > 0 {
		s := strings.ReplaceAll(string(landing), "{{APEX}}", *apexDomain)
		// {{RETENTION}} tracks HOSTTHIS_RETENTION so the landing never advertises
		// the wrong expiry: "for 30 days" / "for 12 hours" / "with no expiry".
		retPhrase := "with no expiry"
		if retention.Enabled() {
			retPhrase = "for " + retention.Describe()
		}
		landing = []byte(strings.ReplaceAll(s, "{{RETENTION}}", retPhrase))
	}

	// URL builder picks based on mode. Subdomain mode is required for
	// production; path mode is the dev-friendly alternative documented
	// in SPEC.md "Dev-only path mode".
	build := buildURL(*scheme, *apexDomain, *urlMode, logger)

	// CDN cache purger (noop unless a CDN is configured). The decorator
	// wraps the verb service so a mutation transparently invalidates the
	// edge cache for the affected slug; the verb service itself stays
	// cache-unaware (see SPEC.md "Active invalidation: CachePurger").
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
		// shale mount floor); nil on backends with no mount concept, which
		// the server reads as always-ready. /healthz stays pure liveness.
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
		// Bound the four axes a slow / hostile client could try to
		// hold open. Reads are tiny (we only do GETs for content +
		// headers), writes are at most MaxPasteBytes (1 MiB), so the
		// values below are generous but not unbounded.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10, // 8 KiB
	}

	// Run both servers + the sweep goroutine; whichever signaling
	// event hits first wins. Signals tear them all down cleanly.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errs := make(chan error, 2)
	go func() { errs <- sshServer.ListenAndServe() }()
	go func() {
		logger.Printf("http: listening on %s", *httpAddr)
		errs <- httpSrv.ListenAndServe()
	}()
	// Always run the sweep loop; HOSTTHIS_SWEEP_DISABLED selects dry-run vs
	// live (sweepSvc.DryRun), it does not gate the goroutine - a dry-run
	// sweep must still run to log what it would clean.
	go sweepSvc.Run(ctx)

	select {
	case <-ctx.Done():
		logger.Printf("signal received; shutting down")
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("server error: %v", err)
		}
	}

	// Drain hint, then the grace window, then the close (SPEC "Drain hint:
	// reconnect-before-shutdown"). The hint fires at drain start - BEFORE the
	// HTTP server stops accepting - telling every live relay client to
	// re-home; the process then KEEPS SERVING through HOSTTHIS_DRAIN_GRACE
	// (default 3s, 0 disables) so the hint flushes and hint-acting clients
	// reconnect make-before-break through the ingress onto a surviving pod.
	if roomRelay != nil {
		roomRelay.AnnounceDrain()
		if grace := envOrDuration("HOSTTHIS_DRAIN_GRACE", 3*time.Second); grace > 0 {
			logger.Printf("relay: drain hint broadcast; serving through %s grace before close", grace)
			time.Sleep(grace)
		}
	}

	// Close all live relay connections: a hijacked WebSocket is not
	// tracked by http.Server.Shutdown, so closing them here (with a normal
	// closure) unblocks their request goroutines and lets clients reconnect
	// on their backoff schedule rather than hammering instantly.
	if roomRelay != nil {
		roomRelay.Registry().CloseAll()
	}

	// Stop the peer publisher's senders (multi-pod only): local fan-out is
	// done, so drop the outbound peer queues and their connections cleanly.
	if metadata.RelayPeer != nil {
		metadata.RelayPeer.Close()
	}

	shutdownCtx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// buildBlobStore reads HOSTTHIS_BLOB_BACKEND and returns the configured
// BlobStore + SweepBlobs (same type, narrowed via two interfaces). The disk
// backend is the only standalone backend (dev/test); production runs the shale
// metadata backend, whose ShaleRepo owns its OWN shale-managed MinIO blob plane
// (cluster.BlobKV) constructed in NewShaleRepo - it does NOT go through this
// detached store. The detached S3 standalone backend was retired with the
// shale-collocated blob work (the shale path made it redundant; dev uses disk).
func buildBlobStore(dataDir string, logger *log.Logger) (*storage.CompressedBlobStore, service.SweepBlobs, func(), error) {
	backend := strings.ToLower(envOr("HOSTTHIS_BLOB_BACKEND", "disk"))
	switch backend {
	case "", "disk":
		bs, err := storage.NewBlobStore(filepath.Join(dataDir, "blobs"))
		if err != nil {
			return nil, nil, nil, err
		}
		logger.Printf("blobs: disk backend at %s/blobs (zstd-compressed at rest)", dataDir)
		// Wrap with the compression layer for Put/Get used by upload + manage.
		// Sweep uses WalkBlobs + Remove which are sha-only (no body access),
		// so it bypasses the wrapper and talks to the raw backend.
		inner, sweep, cleanup, err := maybeWrapWriteBack(bs, dataDir, logger)
		if err != nil {
			return nil, nil, nil, err
		}
		return storage.NewCompressedBlobStore(inner), sweep, cleanup, nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown HOSTTHIS_BLOB_BACKEND %q (only 'disk' is supported as a standalone backend; production uses the shale-collocated blob plane)", backend)
	}
}

// writeBackInner is the contract maybeWrapWriteBack needs of a durable
// backend: the inner Put/Get/GetReader the compression layer wraps, plus
// the WalkBlobs/Remove the sweep uses. *storage.BlobStore (the disk store)
// satisfies it.
type writeBackInner interface {
	Put(sha string, r io.Reader, size int64) error
	Get(sha string) ([]byte, error)
	GetReader(sha string) (io.ReadCloser, int64, error)
	WalkBlobs(fn func(sha string) error) error
	Remove(sha string) error
}

// maybeWrapWriteBack optionally fronts the durable backend with the
// local-disk write-back cache when HOSTTHIS_BLOB_WRITEBACK=true. When
// disabled (the default), the durable backend is returned unchanged so
// today's strict durable-before-ack behavior is preserved. Returns the
// store to wrap with compression, the sweep interface, and a cleanup
// func (stops the uploaders; no-op when disabled).
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

// buildCachePurger reads HOSTTHIS_CACHE_BACKEND and returns the
// configured CachePurger. Defaults to Noop (no CDN in front). The
// scheme/apex/mode let the cloudflare adapter build a slug's public URL
// variants (the page plus the markdown shell's "?raw=1" content fetch)
// so a purge invalidates every cache key the paste is reachable at.
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

// buildURL returns the URL emitter for a given scheme + mode + apex.
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

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envOrInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func envOrDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// parseRetention reads the HOSTTHIS_RETENTION operator knob into a retention
// policy. Accepted forms (case-insensitive):
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
