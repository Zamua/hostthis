// hostthis-migrate copies a shale-backed metadata plane onto celld cells,
// re-staging every blob through the destination's own content-addressed store.
//
// It streams bytes through the application's OWN read and write paths rather
// than renaming objects in the bucket. That costs a download and an upload per
// blob, which inside one cluster is cheap, and buys two things worth more than
// the bandwidth: the destination computes each sha itself, so a corrupted or
// mis-keyed byte cannot survive the copy unnoticed; and there is no key
// arithmetic to get wrong, because neither side is told how the other lays out
// its bucket.
//
// IDEMPOTENT, keyed by slug. A slug already present in the destination is
// skipped, so an interrupted run is resumed by running it again - which is what
// makes the cutover window safe to retry rather than a single attempt that must
// not fail.
//
// Built only under -tags slatedb: reading the source means mounting shale.
//
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
	"time"

	"strconv"
	"strings"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/backends/slate/blobstore"
	"github.com/Zamua/shale/pkg/blob"
	"github.com/Zamua/shale/pkg/storageunit"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/shaleblob"
	"github.com/Zamua/hostthis/internal/storage"
)

// srcBlobStore opens the SOURCE's shale-collocated blob bucket. Required: a
// cluster opened without it cannot read a single paste's bytes, and the
// migration would copy metadata pointing at nothing.
func srcBlobStore(logger *log.Logger) blob.Store {
	bs, err := blobstore.New(blobstore.Config{
		EndpointHost: stripScheme(env("SRC_S3_ENDPOINT", "")),
		AccessKey:    env("SRC_S3_ACCESS_KEY", ""),
		SecretKey:    env("SRC_S3_SECRET_KEY", ""),
		Bucket:       env("SRC_BLOB_BUCKET", ""),
	})
	if err != nil {
		logger.Fatalf("open source blob bucket: %v", err)
	}
	return bs
}

// srcCondStore is the homogeneous bootstrap marker store. The source cluster
// was formed with it, so opening without it would try to FORM a new cluster
// over the same bucket rather than joining the existing one.
func srcCondStore(logger *log.Logger) storageunit.ConditionalStore {
	cs, err := slate.NewMinioConditionalStore(slate.MinioConditionalStoreConfig{
		EndpointHost: stripScheme(env("SRC_S3_ENDPOINT", "")),
		AccessKey:    env("SRC_S3_ACCESS_KEY", ""),
		SecretKey:    env("SRC_S3_SECRET_KEY", ""),
		Bucket:       env("SRC_METADATA_BUCKET", ""),
		KeyPrefix:    env("SRC_METADATA_DB", ""),
	})
	if err != nil {
		logger.Fatalf("open source conditional store: %v", err)
	}
	return cs
}

func stripScheme(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "http://"), "https://")
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

var (
	errNoLiveVersions   = errors.New("no live versions")
	errSourceUnreadable = errors.New("source cannot read the bytes its metadata names")
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	dry := flag.Bool("dry-run", false, "report what would move, write nothing")
	verify := flag.Bool("verify", false, "compare the destination against the source, write nothing")
	flag.Parse()
	logger := log.New(os.Stderr, "migrate ", log.LstdFlags)

	src, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:    env("MIGRATE_NODE_ID", "migrate-1"),
		Endpoint:  env("SRC_S3_ENDPOINT", ""),
		Region:    env("SRC_S3_REGION", "us-east-1"),
		Bucket:    env("SRC_METADATA_BUCKET", ""),
		AccessKey: env("SRC_S3_ACCESS_KEY", ""),
		SecretKey: env("SRC_S3_SECRET_KEY", ""),
		DbName:    env("SRC_METADATA_DB", ""),
		BlobStore: srcBlobStore(logger),
		// These MUST match the deployment that wrote the data. A cluster opened
		// with a different unit count or replication factor routes keys
		// elsewhere and reads an empty store rather than failing loudly, which
		// is the failure mode this comment exists to prevent someone rediscovering.
		Coordinator: env("SRC_COORDINATOR", "cas"),
		// A cas cluster is multi-node by definition and demands an advertised
		// address even when this process is the only member left. Nothing dials
		// it during a migration - every other node is stopped - so any free
		// local port satisfies the requirement.
		GRPCAddr:          env("SRC_GRPC_ADDR", "127.0.0.1:17947"),
		ReplicationFactor: envInt("SRC_REPLICATION_FACTOR", 2),
		UnitCount:         envInt("SRC_UNIT_COUNT", 16),
		ConditionalStore:  srcCondStore(logger),
		Logger:            logger,
	})
	if err != nil {
		logger.Fatalf("open source cluster: %v", err)
	}
	defer src.Close() //nolint:errcheck
	srcBlobs, err := shaleblob.New(src)
	if err != nil {
		logger.Fatalf("open source blob plane: %v", err)
	}

	dstRepo := celld.NewPasteRepo(env("DST_CELLD_ENDPOINT", ""), nil)
	dstBlobStore, err := storage.NewS3BlobStore(storage.S3BlobConfig{
		Endpoint:  env("DST_S3_ENDPOINT", ""),
		Bucket:    env("DST_BLOB_BUCKET", ""),
		Region:    env("DST_S3_REGION", "us-east-1"),
		AccessKey: env("DST_S3_ACCESS_KEY", ""),
		SecretKey: env("DST_S3_SECRET_KEY", ""),
		Prefix:    env("DST_BLOB_PREFIX", "blob"),
	})
	if err != nil {
		logger.Fatalf("open destination blobs: %v", err)
	}
	dstBlobs := service.NewStandaloneBlobUnit(storage.NewCompressedBlobStore(dstBlobStore))

	// A freshly opened node MOUNTS its units asynchronously, and a scan issued
	// before they settle fails with a hand-off error rather than returning a
	// short answer. Retry until the whole keyspace is readable: an enumeration
	// that silently saw only the mounted fraction would migrate a SUBSET and
	// report success, which is the worst outcome available here.
	if err := awaitMounted(src, logger); err != nil {
		logger.Fatalf("source never became fully readable: %v", err)
	}

	// Outcomes are classified rather than lumped into pass/fail, because an
	// operator has to treat them differently. A row the source cannot read is DATA
	// THE MIGRATION LOST, and it must be named individually; a tombstoned or
	// unpublished row is nothing to move; only an unexpected error is a defect in
	// this tool.
	var moved, skipped, degraded, failed int
	var degradedSlugs []string
	walkErr := src.WalkPastes(func(p domain.Paste) error {
		// The walk sees every ROW, which is more than the live set: a failed
		// upload and a paste inside its tombstone grace both still have one.
		// movePaste commits READY, so migrating those would RESURRECT content
		// the owner never published or explicitly deleted.
		if p.Status != domain.PasteStatusReady {
			skipped++
			return nil
		}
		if !*verify {
			if _, err := dstRepo.Get(p.Slug); err == nil {
				skipped++
				return nil
			} else if !errors.Is(err, domain.ErrNotFound) {
				logger.Printf("%s: probe destination: %v", p.Slug, err)
				failed++
				return nil
			}
		}
		if *verify {
			if err := verifyPaste(src, srcBlobs, dstRepo, dstBlobs, p); err != nil {
				logger.Printf("VERIFY FAIL %s: %v", p.Slug, err)
				failed++
				return nil
			}
			fmt.Printf("verified %s\n", p.Slug)
			moved++
			return nil
		}
		if *dry {
			moved++
			return nil
		}
		switch err := movePaste(src, srcBlobs, dstRepo, dstBlobs, p); {
		case err == nil:
		case errors.Is(err, errNoLiveVersions):
			// Every version tombstoned but the row still READY. Nothing to
			// carry across, and inventing content would be worse than omitting.
			skipped++
			return nil
		case errors.Is(err, errSourceUnreadable):
			// The source cannot produce the bytes its own metadata points at, so
			// this paste was ALREADY broken before the migration touched it.
			// Named individually: it is the one category a human must look at.
			logger.Printf("DEGRADED %s: %v", p.Slug, err)
			degraded++
			degradedSlugs = append(degradedSlugs, p.Slug.String())
			return nil
		default:
			// One bad record must not end the run: the rest are still movable,
			// and a partial migration that names its failures beats one that
			// stops at the first.
			logger.Printf("FAILED %s: %v", p.Slug, err)
			failed++
			return nil
		}
		moved++
		return nil
	})
	if walkErr != nil {
		logger.Fatalf("walk source: %v", walkErr)
	}
	fmt.Printf("moved=%d skipped=%d degraded=%d failed=%d\n", moved, skipped, degraded, failed)
	for _, s := range degradedSlugs {
		fmt.Printf("degraded: %s (source could not read its bytes; it was already broken)\n", s)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// verifyPaste re-reads one migrated paste from the DESTINATION and compares it
// against the source: the row's user-visible fields, the live version count,
// and the sha256 of every blob as the destination actually serves it.
//
// The comparison hashes bytes read back through the destination's own read
// path, not bucket objects: what matters is what a user would receive, and a
// correct object behind a broken read path must fail this.
func verifyPaste(src *storage.ShaleRepo, srcBlobs service.BlobUnit,
	dst *celld.PasteRepo, dstBlobs service.BlobUnit, p domain.Paste,
) error {
	srcVersAll, err := src.ListVersions(p.Slug)
	if err != nil {
		return fmt.Errorf("list source versions: %w", err)
	}
	srcLive := 0
	for _, v := range srcVersAll {
		if !v.Deleted {
			srcLive++
		}
	}

	got, err := dst.Get(p.Slug)
	if err != nil {
		// The migrator skips a paste whose every version is tombstoned - there
		// is nothing to carry - so for THAT paste absence is the correct
		// outcome, and reporting it as a failure would teach the operator to
		// ignore the one line that matters.
		if srcLive == 0 && errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("absent in destination: %w", err)
	}
	if got.Kind != p.Kind && len(p.Manifest.Files) == 0 {
		return fmt.Errorf("kind %q, source %q", got.Kind, p.Kind)
	}
	if got.Name != p.Name {
		return fmt.Errorf("name %q, source %q", got.Name, p.Name)
	}
	if got.PinnedVersion != p.PinnedVersion {
		return fmt.Errorf("pin v%d, source v%d", got.PinnedVersion, p.PinnedVersion)
	}

	srcVers, live := srcVersAll, srcLive
	dstVers, err := dst.ListVersions(p.Slug)
	if err != nil {
		return fmt.Errorf("list destination versions: %w", err)
	}
	dstLive := 0
	for _, v := range dstVers {
		if !v.Deleted {
			dstLive++
		}
	}
	if dstLive != live {
		return fmt.Errorf("%d live versions, source %d", dstLive, live)
	}

	ctx := context.Background()
	for _, v := range srcVers {
		if v.Deleted {
			continue
		}
		for _, want := range versionSHAs(v) {
			if want == "" {
				continue
			}
			body, _, err := dstBlobs.Read(ctx, p.Slug.String(), want)
			if err != nil {
				return fmt.Errorf("v%d blob %s unreadable in destination: %w", v.VerNum, want, err)
			}
			h := sha256.New()
			_, cerr := io.Copy(h, body)
			_ = body.Close()
			if cerr != nil {
				return fmt.Errorf("v%d blob %s read: %w", v.VerNum, want, cerr)
			}
			if got := hex.EncodeToString(h.Sum(nil)); got != want {
				return fmt.Errorf("v%d blob sha %s, want %s", v.VerNum, got, want)
			}
		}
	}
	_ = srcBlobs
	return nil
}

// versionSHAs lists every content sha a version references: one per manifest
// entry for a directory, or the single row sha for a document.
//
// Deduplicated, because a site can serve the same bytes at several paths and
// staging one blob twice would charge for it twice.
func versionSHAs(v domain.Version) []string {
	if len(v.Manifest.Files) == 0 {
		return []string{v.ContentSHA}
	}
	seen := make(map[string]struct{}, len(v.Manifest.Files))
	out := make([]string, 0, len(v.Manifest.Files))
	for _, e := range v.Manifest.Files {
		if e.SHA == "" {
			continue
		}
		if _, dup := seen[e.SHA]; dup {
			continue
		}
		seen[e.SHA] = struct{}{}
		out = append(out, e.SHA)
	}
	return out
}

// awaitMounted blocks until EVERY storage unit is mounted on this node.
//
// The obvious version of this - retry the scan until it stops erroring - is
// WRONG, and dangerously so. A scan against a partially mounted keyspace
// SUCCEEDS and returns only the units already up: a rehearsal against 38 pastes
// enumerated 3 and reported no error. A migration built on that would copy a
// subset and declare victory.
//
// Ready(1.0) is the real signal, because it asks how many of the node's DESIRED
// unit positions are actually serving rather than whether one query happened to
// find a home.
func awaitMounted(src *storage.ShaleRepo, logger *log.Logger) error {
	deadline := time.Now().Add(5 * time.Minute)
	for attempt := 1; ; attempt++ {
		if src.Ready(1.0) {
			// Mounted is necessary but not sufficient: a unit can be serving and
			// still refuse a scan mid-handoff, so the first clean full scan is
			// the confirmation.
			if err := src.WalkPastes(func(domain.Paste) error { return nil }); err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("not every storage unit mounted before the deadline")
		}
		logger.Printf("waiting for all units to mount (attempt %d)", attempt)
		time.Sleep(3 * time.Second)
	}
}

// movePaste copies one paste and every live version of it.
//
// Versions go in ASCENDING order so the destination assigns the same numbers
// the source used: the destination mints v1 on create and increments on each
// append, so replaying out of order would renumber the history and break every
// pinned URL pointing into it.
func movePaste(src *storage.ShaleRepo, srcBlobs service.BlobUnit,
	dst *celld.PasteRepo, dstBlobs service.BlobUnit, p domain.Paste,
) error {
	ctx := context.Background()
	versions, err := src.ListVersions(p.Slug)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	// ListVersions is newest-first; replay oldest-first.
	for i, j := 0, len(versions)-1; i < j; i, j = i+1, j-1 {
		versions[i], versions[j] = versions[j], versions[i]
	}

	first := true
	for _, v := range versions {
		if v.Deleted {
			// A tombstone holds a version NUMBER, not bytes. Replaying it as a
			// live version would resurrect content the owner deleted.
			continue
		}
		// EVERY blob the version references, not just the root. A directory's
		// manifest names one blob per file, and staging only the root migrates a
		// site that serves its index and 500s on every other page - which is
		// what this did until a rehearsal opened the second page.
		shas := versionSHAs(v)
		handles := make([]service.BlobHandle, 0, len(shas))
		var rootSHA string
		var rootSize int
		for _, want := range shas {
			body, _, err := srcBlobs.Read(ctx, p.Slug.String(), want)
			if err != nil {
				return fmt.Errorf("%w: v%d %s: %v", errSourceUnreadable, v.VerNum, want, err)
			}
			staged, sha, size, err := dstBlobs.StageEncoding(ctx, p.Slug.String(), body)
			_ = body.Close()
			if err != nil {
				return fmt.Errorf("stage v%d %s: %w", v.VerNum, want, err)
			}
			// The destination hashes what it actually received. A mismatch means
			// the bytes changed in flight, and binding metadata to them would
			// record a paste whose content is not what its owner uploaded.
			if want != "" && sha != want {
				return fmt.Errorf("v%d sha mismatch: source %s, restaged %s", v.VerNum, want, sha)
			}
			handles = append(handles, staged)
			if want == v.ContentSHA || rootSHA == "" {
				rootSHA, rootSize = sha, size
			}
		}
		sha, size := rootSHA, rootSize
		staged := handles

		if first {
			row := p
			row.ContentSHA = sha
			row.Size = size
			row.Kind = v.Kind
			row.Manifest = v.Manifest
			row.Status = domain.PasteStatusReady
			row.PinnedVersion = 0 // set once, after every version exists
			if err := dstBlobs.Commit(ctx, staged, func(c context.Context) error {
				return dst.InsertWithQuotaCheck(c, row, 0, row.CreatedAt)
			}); err != nil {
				return fmt.Errorf("insert: %w", err)
			}
			first = false
			continue
		}
		if err := dstBlobs.Commit(ctx, staged, func(c context.Context) error {
			if len(v.Manifest.Files) > 0 {
				root, _ := v.Manifest.Lookup("/")
				_, aerr := dst.AppendManifestVersion(c, p.Slug, v.Manifest, root, size, 0, v.CreatedAt)
				return aerr
			}
			_, aerr := dst.AppendVersionWithQuotaCheck(c, p.Slug, v.Kind, sha, size, 0, v.CreatedAt)
			return aerr
		}); err != nil {
			return fmt.Errorf("append v%d: %w", v.VerNum, err)
		}
	}
	if first {
		return errNoLiveVersions
	}

	// The pin and the label go LAST, once the versions they refer to exist.
	if p.Name != "" {
		if err := dst.SetName(p.Slug, p.Name, p.Identity, p.CreatedAt); err != nil {
			return fmt.Errorf("set name: %w", err)
		}
	}
	if p.PinnedVersion != 0 {
		if err := dst.SetPinnedVersion(p.Slug, domain.Version{VerNum: p.PinnedVersion}); err != nil {
			return fmt.Errorf("pin v%d: %w", p.PinnedVersion, err)
		}
	}
	return nil
}
