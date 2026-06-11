// Package main wires the hostthis daemon: SSH server + HTTP server +
// storage + the periodic expiry sweep. Reads flags / env for the
// runtime config (apex domain, URL mode, scheme, ports, data dir).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
		storageCap      = flag.Int64("storage-cap-bytes", envOrInt64("HOSTTHIS_STORAGE_CAP_BYTES", 5<<30), "service-wide cap on total active bytes (0 = unlimited)")
		freshKeysLimit  = flag.Int("fresh-keys-per-subnet", envOrInt("HOSTTHIS_FRESH_KEYS_PER_SUBNET", 20), "max distinct new key fingerprints admitted per IP subnet per window")
		freshKeysWindow = flag.Duration("fresh-keys-window", envOrDuration("HOSTTHIS_FRESH_KEYS_WINDOW", 24*time.Hour), "rolling window for the Sybil rate limit on fresh keys")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "hostthis ", log.LstdFlags|log.LUTC)

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
	blobs, blobsSweep, err := buildBlobStore(*dataDir, logger)
	if err != nil {
		logger.Fatalf("blob store: %v", err)
	}

	siteRepo := metadata.Sites
	roomRepo := metadata.Rooms

	uploadSvc := service.NewUpload(pasteRepo, blobs)
	uploadSvc.ServiceCapBytes = *storageCap
	manageSvc := service.NewManage(pasteRepo, blobs)
	manageSvc.ServiceCapBytes = *storageCap

	// Static-site archive deploys. Reuses the same blob store + the same
	// per-identity quota as pastes; nil-safe if the metadata backend
	// doesn't expose a site repo.
	var deploySvc *service.DeploySite
	if siteRepo != nil {
		deploySvc = service.NewDeploySite(siteRepo, pasteRepo, blobs)
		deploySvc.ServiceCapBytes = *storageCap
	}

	// Rooms: the no-auth, capability-based app-persistence tier
	// (POST/GET/PUT/DELETE under /api/rooms on an app subdomain). Reuses
	// the same metadata backend; nil-safe if the backend has no room repo.
	var roomsSvc *service.Rooms
	if roomRepo != nil {
		roomsSvc = service.NewRooms(roomRepo)
		roomsSvc.ServiceCapBytes = *storageCap
	}

	// CDN cache purger. Default is noop (no CDN in front); when CF is
	// configured we wire it up so Update/Delete invalidate the edge
	// cache entries for the affected slugs.
	manageSvc.Cache = buildCachePurger(logger)
	keyGate := service.NewKeyGate(keyGateRepo)
	keyGate.MaxFreshKeysPerSubnet = *freshKeysLimit
	keyGate.Window = *freshKeysWindow
	// Whoami uses the keygate for per-session subnet/budget info.
	manageSvc.KeyGate = keyGate
	sweepSvc := service.NewSweep(pasteRepo, blobsSweep, logger)
	sweepSvc.KeyGate = keyGate
	if siteRepo != nil {
		// Wire site expiry + site-blob GC protection into the sweep.
		sweepSvc.Sites = siteRepo
	}
	if roomRepo != nil {
		// Wire room expiry (30-day inactivity TTL) + the room-create
		// rate-limit prune into the sweep.
		sweepSvc.Rooms = roomRepo
	}

	// HOSTTHIS_SWEEP_DISABLED=true skips the periodic sweep entirely
	// for the lifetime of the process. Operator handle for cutover
	// windows where blob GC must NOT run (e.g. switching the
	// metadata backend before the new backend's view of "referenced
	// shas" has caught up with the bucket). Unset / "false" leaves
	// the default 10-min sweep on.
	sweepDisabled := strings.EqualFold(envOr("HOSTTHIS_SWEEP_DISABLED", "false"), "true")
	if sweepDisabled {
		logger.Printf("sweep: DISABLED via HOSTTHIS_SWEEP_DISABLED=true (no periodic expiry / blob GC / key-gate prune this process lifetime)")
	}

	logger.Printf("config: storage_cap=%d bytes, fresh_keys/subnet=%d per %s",
		*storageCap, *freshKeysLimit, *freshKeysWindow)

	landing, err := os.ReadFile(*landingPath)
	if err != nil {
		logger.Printf("warn: landing not loaded from %q: %v (apex will serve a stub)", *landingPath, err)
	}
	// Substitute the apex placeholder so the landing page tells visitors
	// to ssh to the actual configured domain. The template ships with
	// `{{APEX}}` everywhere a hostname appears (e.g. `ssh {{APEX}} list`).
	if len(landing) > 0 {
		landing = []byte(strings.ReplaceAll(string(landing), "{{APEX}}", *apexDomain))
	}

	// URL builder picks based on mode. Subdomain mode is required for
	// production; path mode is the dev-friendly alternative documented
	// in SPEC.md "Dev-only path mode".
	build := buildURL(*scheme, *apexDomain, *urlMode, logger)
	manageSvc.PublicURL = service.URLBuilder(build)

	sshServer := &hostssh.Server{
		Addr:        *sshAddr,
		HostKeyPath: filepath.Join(*dataDir, "ssh_host_ed25519_key"),
		ApexDomain:  *apexDomain,
		Upload:      uploadSvc,
		Deploy:      deploySvc, // nil when the backend has no site repo
		Manage:      manageSvc,
		KeyGate:     keyGate,
		BuildURL:    build,
		Logger:      logger,
	}

	httpServer := &httpapi.Server{
		Pastes:      pasteRepo,
		Blobs:       blobs,
		LandingHTML: landing,
		ApexDomain:  *apexDomain,
		Color:       envOr("HOSTTHIS_BACKEND_COLOR", ""),
	}
	if siteRepo != nil {
		httpServer.Sites = siteRepo
	}
	if roomsSvc != nil {
		httpServer.Rooms = roomsSvc
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
	if !sweepDisabled {
		go sweepSvc.Run(ctx)
	}

	select {
	case <-ctx.Done():
		logger.Printf("signal received; shutting down")
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("server error: %v", err)
		}
	}

	shutdownCtx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// buildBlobStore reads HOSTTHIS_BLOB_BACKEND and returns the
// configured BlobStore + SweepBlobs (same type, narrowed via two
// interfaces). The disk backend is the default; "s3" picks up an
// S3-compatible endpoint via HOSTTHIS_S3_* env vars.
func buildBlobStore(dataDir string, logger *log.Logger) (service.BlobStore, service.SweepBlobs, error) {
	backend := strings.ToLower(envOr("HOSTTHIS_BLOB_BACKEND", "disk"))
	switch backend {
	case "", "disk":
		bs, err := storage.NewBlobStore(filepath.Join(dataDir, "blobs"))
		if err != nil {
			return nil, nil, err
		}
		logger.Printf("blobs: disk backend at %s/blobs (zstd-compressed at rest)", dataDir)
		// Wrap with the compression layer for Put/Get used by upload + manage.
		// Sweep uses WalkBlobs + Remove which are sha-only (no body access),
		// so it bypasses the wrapper and talks to the raw backend.
		return storage.NewCompressedBlobStore(bs), bs, nil
	case "s3":
		cfg := storage.S3Config{
			EndpointURL: os.Getenv("HOSTTHIS_S3_ENDPOINT"),
			Bucket:      os.Getenv("HOSTTHIS_S3_BUCKET"),
			Region:      envOr("HOSTTHIS_S3_REGION", "us-east-1"),
			AccessKey:   os.Getenv("HOSTTHIS_S3_ACCESS_KEY"),
			SecretKey:   os.Getenv("HOSTTHIS_S3_SECRET_KEY"),
			UseSSL:      strings.ToLower(envOr("HOSTTHIS_S3_USE_SSL", "true")) != "false",
		}
		bs, err := storage.NewS3BlobStore(cfg)
		if err != nil {
			return nil, nil, err
		}
		logger.Printf("blobs: s3 backend at %s bucket=%s (zstd-compressed at rest)", cfg.EndpointURL, cfg.Bucket)
		return storage.NewCompressedBlobStore(bs), bs, nil
	default:
		return nil, nil, fmt.Errorf("unknown HOSTTHIS_BLOB_BACKEND %q (want disk|s3)", backend)
	}
}

// buildCachePurger reads HOSTTHIS_CACHE_BACKEND and returns the
// configured CachePurger. Defaults to Noop (no CDN in front).
func buildCachePurger(logger *log.Logger) service.CachePurger {
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
		return &cache.Cloudflare{ZoneID: zone, Token: token, Logger: logger}
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

func envOrInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
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

func envOrDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
