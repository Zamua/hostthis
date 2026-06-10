// Command hostthis-shale-migrate transforms a slatedb-direct metadata
// bucket into the shape the shale backend (ShaleRepo) expects. One-time,
// idempotent, read-only on the authoritative rows.
//
// It is a thin wrapper: it opens the slate backend from the same env vars
// the metadata backends read, then hands the backend to internal/migrate,
// which owns the entire transform. ShaleRepo itself assumes the data is
// already shale-shaped and contains no lazy-seed or self-heal-on-read
// logic (docs/SPEC.md "Migrating an existing slatedb-direct bucket to
// shale").
//
// Reads the same env vars the metadata backends read:
//
//	HOSTTHIS_METADATA_S3_ENDPOINT     object store (e.g. http://minio:9000)
//	HOSTTHIS_METADATA_S3_BUCKET       (required)
//	HOSTTHIS_METADATA_S3_REGION       (default us-east-1)
//	HOSTTHIS_METADATA_S3_ACCESS_KEY   (required)
//	HOSTTHIS_METADATA_S3_SECRET_KEY   (required)
//	HOSTTHIS_METADATA_S3_USE_SSL      (true|false; default true)
//	HOSTTHIS_METADATA_DB_NAME         (default hostthis-metadata)
//
// Flags:
//
//	--dry-run    compute + print the plan WITHOUT writing anything.
//
// Idempotent: counters and projections are recomputed from the
// authoritative rows and overwritten (never blindly incremented), so a
// re-run produces the same bytes. Read-only on pastes/*, versions/*,
// slug_owner/*, expiry/*. Run against a quiescent bucket: a copy first,
// then production at cutover.
//
// REQUIRES the slatedb build tag + libslatedb_uniffi on the loader path
// (the shale slate backend wraps slatedb), same as hostthisd-with-shale.

//go:build slatedb

package main

import (
	"flag"
	"log"
	"os"
	"strings"

	"github.com/Zamua/shale/backends/slate"

	"github.com/Zamua/hostthis/internal/migrate"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "compute + print the plan without writing anything")
	flag.Parse()
	logger := log.New(os.Stderr, "shale-migrate ", log.LstdFlags|log.LUTC)

	bucket := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_BUCKET"))
	if bucket == "" {
		logger.Fatalf("HOSTTHIS_METADATA_S3_BUCKET is required")
	}
	accessKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_SECRET_KEY"))
	if accessKey == "" || secretKey == "" {
		logger.Fatalf("HOSTTHIS_METADATA_S3_ACCESS_KEY + HOSTTHIS_METADATA_S3_SECRET_KEY are required")
	}
	endpoint := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ENDPOINT"))
	region := envOr("HOSTTHIS_METADATA_S3_REGION", "us-east-1")
	useSSL := strings.EqualFold(envOr("HOSTTHIS_METADATA_S3_USE_SSL", "true"), "true")
	dbName := envOr("HOSTTHIS_METADATA_DB_NAME", "hostthis-metadata")

	// Open the slate backend directly: raw KV access. ShaleRepo at
	// ReplicationFactor=1 wraps this same backend with no LWW envelope, so
	// the raw values the transform Puts here are exactly what ShaleRepo
	// reads back.
	be, err := slate.New(slate.Config{
		Bucket:    bucket,
		DbName:    dbName,
		Endpoint:  endpoint,
		Region:    region,
		AccessKey: accessKey,
		SecretKey: secretKey,
		UseSSL:    useSSL,
	})
	if err != nil {
		logger.Fatalf("open slate backend: %v", err)
	}
	defer be.Close() //nolint:errcheck

	mode := "WRITE"
	if *dryRun {
		mode = "DRY-RUN (no writes)"
	}
	logger.Printf("metadata bucket=%s db=%s endpoint=%s mode=%s", bucket, dbName, endpoint, mode)

	stats, err := migrate.Run(be, *dryRun, logger)
	if err != nil {
		logger.Fatalf("migrate: %v", err)
	}

	if *dryRun {
		logger.Printf("dry-run complete: would write counters=%d projections=%d (no changes made)",
			stats.Identities, stats.Pastes)
		return
	}
	logger.Printf("migration complete: identities=%d counters=%d projections=%d",
		stats.Identities, stats.CountersWritten, stats.ProjectionsWritten)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
