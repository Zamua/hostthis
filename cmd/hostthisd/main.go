// Package main wires the hostthis daemon: SSH server + HTTP server +
// storage + the periodic expiry sweep. Reads flags / env for the
// runtime config (apex domain, URL mode, scheme, ports, data dir).
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

func main() {
	var (
		dataDir        = flag.String("data-dir", envOr("HOSTTHIS_DATA_DIR", "./data"), "where sqlite + blobs live")
		sshAddr        = flag.String("ssh-addr", envOr("HOSTTHIS_SSH_ADDR", ":2222"), "ssh listen address")
		httpAddr       = flag.String("http-addr", envOr("HOSTTHIS_HTTP_ADDR", ":8080"), "http listen address")
		apexDomain     = flag.String("apex-domain", envOr("HOSTTHIS_APEX_DOMAIN", "hostthis.dev"), "public apex (for URL emission)")
		urlMode        = flag.String("mode", envOr("HOSTTHIS_URL_MODE", "path"), "url mode: subdomain (prod) | path (dev)")
		scheme         = flag.String("scheme", envOr("HOSTTHIS_PUBLIC_SCHEME", "https"), "public URL scheme (https for prod, http for local dev)")
		landingPath    = flag.String("landing", envOr("HOSTTHIS_LANDING", "web/landing.html"), "path to apex landing HTML")
		storageCap     = flag.Int64("storage-cap-bytes", envOrInt64("HOSTTHIS_STORAGE_CAP_BYTES", 5<<30), "service-wide cap on total active bytes (0 = unlimited)")
		freshKeysLimit = flag.Int("fresh-keys-per-subnet", envOrInt("HOSTTHIS_FRESH_KEYS_PER_SUBNET", 20), "max distinct new key fingerprints admitted per IP subnet per window")
		freshKeysWindow = flag.Duration("fresh-keys-window", envOrDuration("HOSTTHIS_FRESH_KEYS_WINDOW", 24*time.Hour), "rolling window for the Sybil rate limit on fresh keys")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "hostthis ", log.LstdFlags|log.LUTC)

	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		logger.Fatalf("mkdir data-dir: %v", err)
	}

	db, err := storage.Open(filepath.Join(*dataDir, "hostthis.db"))
	if err != nil {
		logger.Fatalf("open db: %v", err)
	}
	defer db.Close()

	pasteRepo := storage.NewPasteRepo(db)
	keyGateRepo := storage.NewKeyGateRepo(db)
	blobs, err := storage.NewBlobStore(filepath.Join(*dataDir, "blobs"))
	if err != nil {
		logger.Fatalf("blob store: %v", err)
	}

	uploadSvc := service.NewUpload(pasteRepo, blobs)
	uploadSvc.ServiceCapBytes = *storageCap
	manageSvc := service.NewManage(pasteRepo, blobs)
	manageSvc.ServiceCapBytes = *storageCap
	keyGate := service.NewKeyGate(keyGateRepo)
	keyGate.MaxFreshKeysPerSubnet = *freshKeysLimit
	keyGate.Window = *freshKeysWindow
	sweepSvc := service.NewSweep(pasteRepo, blobs, logger)
	sweepSvc.KeyGate = keyGate

	logger.Printf("config: storage_cap=%d bytes, fresh_keys/subnet=%d per %s",
		*storageCap, *freshKeysLimit, *freshKeysWindow)

	landing, err := os.ReadFile(*landingPath)
	if err != nil {
		logger.Printf("warn: landing not loaded from %q: %v (apex will serve a stub)", *landingPath, err)
	}

	// URL builder picks based on mode. Subdomain mode is required for
	// production; path mode is the dev-friendly alternative documented
	// in SPEC.md "Dev-only path mode".
	build := buildURL(*scheme, *apexDomain, *urlMode, logger)

	sshServer := &hostssh.Server{
		Addr:        *sshAddr,
		HostKeyPath: filepath.Join(*dataDir, "ssh_host_ed25519_key"),
		Upload:      uploadSvc,
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
	go sweepSvc.Run(ctx)

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

// buildURL returns the URL emitter for a given scheme + mode + apex.
func buildURL(scheme, apex, mode string, logger *log.Logger) hostssh.URLBuilder {
	switch strings.ToLower(mode) {
	case "subdomain":
		return func(slug domain.Slug) string {
			return scheme + "://" + slug.String() + "." + apex
		}
	case "path":
		logger.Printf("WARN running in path mode — origin isolation is dev-only. " +
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
