// Command hostthis-metadata-migrate copies every paste / version /
// keygate row from the sqlite metadata store to a SlateDB-backed
// object-storage store. One-way, idempotent.
//
// Reads the same env vars hostthisd does:
//
//	HOSTTHIS_DATA_DIR                 source: <data-dir>/hostthis.db
//	HOSTTHIS_METADATA_S3_ENDPOINT     destination object store
//	HOSTTHIS_METADATA_S3_BUCKET       (required)
//	HOSTTHIS_METADATA_S3_REGION       (default us-east-1)
//	HOSTTHIS_METADATA_S3_ACCESS_KEY   (required)
//	HOSTTHIS_METADATA_S3_SECRET_KEY   (required)
//	HOSTTHIS_METADATA_S3_USE_SSL      (default true)
//	HOSTTHIS_METADATA_DB_NAME         (default hostthis-metadata)
//
// Idempotent: re-running overwrites destination rows with the same
// data. Safe during a staged cutover. Does NOT delete source rows —
// flip HOSTTHIS_METADATA_BACKEND=slatedb to switch reads/writes, then
// remove the source DB when confident.
//
// REQUIRES the slatedb build tag + libslatedb_uniffi on the loader
// path (same as hostthisd-with-slatedb).

//go:build slatedb

package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func main() {
	dataDir := flag.String("data-dir", envOr("HOSTTHIS_DATA_DIR", "./data"), "source sqlite directory")
	flag.Parse()
	logger := log.New(os.Stderr, "meta-migrate ", log.LstdFlags|log.LUTC)

	// Source: sqlite.
	dbPath := filepath.Join(*dataDir, "hostthis.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		logger.Fatalf("open sqlite %s: %v", dbPath, err)
	}
	defer db.Close()
	src := storage.NewPasteRepo(db)
	logger.Printf("source: sqlite at %s", dbPath)

	// Debug: total row count direct from sqlite (bypassing the
	// repo). Helps diagnose silent empty-source bugs.
	var npastes, nversions int
	if err := db.QueryRow("SELECT COUNT(*) FROM pastes").Scan(&npastes); err != nil {
		logger.Printf("warn: pastes count: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM versions").Scan(&nversions); err != nil {
		logger.Printf("warn: versions count: %v", err)
	}
	logger.Printf("source counts: pastes=%d versions=%d", npastes, nversions)

	// Destination: slatedb.
	bucket := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_BUCKET"))
	if bucket == "" {
		logger.Fatalf("HOSTTHIS_METADATA_S3_BUCKET is required")
	}
	endpoint := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ENDPOINT"))
	region := envOr("HOSTTHIS_METADATA_S3_REGION", "us-east-1")
	accessKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("HOSTTHIS_METADATA_S3_SECRET_KEY"))
	useSSL := strings.EqualFold(envOr("HOSTTHIS_METADATA_S3_USE_SSL", "true"), "true")
	dbName := envOr("HOSTTHIS_METADATA_DB_NAME", "hostthis-metadata")
	if accessKey == "" || secretKey == "" {
		logger.Fatalf("HOSTTHIS_METADATA_S3_ACCESS_KEY + HOSTTHIS_METADATA_S3_SECRET_KEY are required")
	}
	dst, err := storage.NewSlateRepo(storage.SlateConfig{
		Endpoint:  endpoint,
		Region:    region,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
		UseSSL:    useSSL,
		DbName:    dbName,
	})
	if err != nil {
		logger.Fatalf("open slatedb: %v", err)
	}
	defer dst.Close()
	logger.Printf("destination: slatedb bucket=%s db=%s endpoint=%s", bucket, dbName, endpoint)

	identities, err := allIdentities(db)
	if err != nil {
		logger.Fatalf("list identities: %v", err)
	}
	logger.Printf("found %d identities in source", len(identities))

	var pasteCount, versionCount int
	for _, identity := range identities {
		pastes, err := src.ListByOwner(identity)
		if err != nil {
			logger.Fatalf("list pastes for %s: %v", identity, err)
		}
		for _, p := range pastes {
			if err := insertPasteIdempotent(dst, p); err != nil {
				logger.Fatalf("write paste %s: %v", p.Slug, err)
			}
			pasteCount++

			versions, err := src.ListVersions(p.Slug)
			if err != nil {
				logger.Fatalf("list versions for %s: %v", p.Slug, err)
			}
			// Replay oldest-first so destination MAX(ver_num) grows
			// monotonically. ListVersions returns DESC; reverse.
			for i := len(versions) - 1; i >= 0; i-- {
				v := versions[i]
				if v.VerNum == 1 {
					if v.Deleted {
						if err := dst.DeleteVersion(p.Slug, 1); err != nil {
							logger.Fatalf("tombstone v1 of %s: %v", p.Slug, err)
						}
					}
					versionCount++
					continue
				}
				if _, err := dst.AppendVersionWithQuotaCheck(p.Slug, v.Kind, v.ContentSHA, v.Size, 0, 0, v.CreatedAt); err != nil {
					logger.Fatalf("append v%d of %s: %v", v.VerNum, p.Slug, err)
				}
				if v.Deleted {
					if err := dst.DeleteVersion(p.Slug, v.VerNum); err != nil {
						logger.Fatalf("tombstone v%d of %s: %v", v.VerNum, p.Slug, err)
					}
				}
				versionCount++
			}

			// Re-apply pin if set.
			if p.PinnedVersion != 0 {
				ver, err := src.GetVersion(p.Slug, p.PinnedVersion)
				if err != nil {
					logger.Printf("warn: paste %s pinned to v%d but version missing: %v", p.Slug, p.PinnedVersion, err)
					continue
				}
				if err := dst.SetPinnedVersion(p.Slug, ver); err != nil {
					logger.Fatalf("re-apply pin for %s: %v", p.Slug, err)
				}
			}
		}
	}

	keygateRows, err := allKeygateRows(db)
	if err != nil {
		logger.Fatalf("read keygate: %v", err)
	}
	keygateCount := 0
	for _, k := range keygateRows {
		// Massive limits so replay never trips rate-limiting.
		_, err := dst.AdmitNewKey(k.identity, k.subnet, k.firstSeen, 1<<30, 365*24*time.Hour)
		if err != nil {
			logger.Fatalf("replay keygate %s/%s: %v", k.subnet, k.identity, err)
		}
		keygateCount++
	}

	logger.Printf("migration complete: pastes=%d versions=%d keygate=%d", pasteCount, versionCount, keygateCount)
}

func insertPasteIdempotent(dst *storage.SlateRepo, p domain.Paste) error {
	err := dst.InsertWithQuotaCheck(p, 0, 0, p.CreatedAt)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "slug already taken") {
		return nil
	}
	return err
}

func allIdentities(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT identity FROM pastes ORDER BY identity`)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type keygateRow struct {
	identity  string
	subnet    string
	firstSeen time.Time
}

func allKeygateRows(db *sql.DB) ([]keygateRow, error) {
	rows, err := db.Query(`SELECT identity, ip_subnet, first_seen_at FROM key_first_seen`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []keygateRow
	for rows.Next() {
		var id, sub, ts string
		if err := rows.Scan(&id, &sub, &ts); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, err
		}
		out = append(out, keygateRow{identity: id, subnet: sub, firstSeen: t})
	}
	return out, rows.Err()
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
