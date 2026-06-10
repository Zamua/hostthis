// Command hostthis-shale-migrate transforms a slatedb-direct metadata
// bucket into the shape the shale backend (ShaleRepo) expects. One-time,
// idempotent, read-only on the authoritative rows.
//
// The slatedb-direct backend and the shale backend share identical key
// names and identical authoritative rows (pastes/*, versions/*,
// slug_owner/*, expiry/*). They differ only in two derived families:
//
//   - identity_bytes/<id>        the per-owner reservation counter. NEW in
//                                shale; slatedb data has no such row. The
//                                tool CREATES it, seeded to the owner's true
//                                live-version byte sum.
//   - identity_pastes/<id>/<slug>  the per-owner list index. slatedb wrote
//                                EMPTY markers here; shale wants a value-
//                                bearing projection. The tool RE-WRITES each
//                                entry with the projection.
//
// All transform logic lives here, in this standalone tool. ShaleRepo
// itself assumes the data is already shale-shaped and contains no
// lazy-seed or self-heal-on-read logic (docs/SPEC.md "Migrating an
// existing slatedb-direct bucket to shale").
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
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Zamua/shale/backends/slate"
	"github.com/Zamua/shale/pkg/backend"
)

// --- JSON row shapes (mirror internal/storage's slatedb/shale rows) --------
//
// These structs carry the SAME JSON field tags the storage package's
// pasteRow / versionRow / identityPasteRow use, so values round-trip
// byte-compatibly. The storage types are unexported + build-tagged, so the
// tool defines its own with matching tags rather than importing them.

type pasteRow struct {
	Identity      string    `json:"identity"`
	Kind          string    `json:"kind"`
	ContentSHA    string    `json:"content_sha"`
	Size          int       `json:"size"`
	Name          string    `json:"name"`
	PinnedVersion int       `json:"pinned_version"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type versionRow struct {
	VerNum     int       `json:"ver_num"`
	Kind       string    `json:"kind"`
	ContentSHA string    `json:"content_sha"`
	Size       int       `json:"size"`
	CreatedAt  time.Time `json:"created_at"`
	Deleted    bool      `json:"deleted"`
}

// identityPasteRow is the value-bearing projection stored at
// identity_pastes/<id>/<slug>. Field set + tags match exactly what
// ShaleRepo's confirm step and reconciler write.
type identityPasteRow struct {
	Name      string    `json:"name"`
	Size      int       `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

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
	// the raw values we Put here are exactly what ShaleRepo reads back.
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

	plan, err := computePlan(be)
	if err != nil {
		logger.Fatalf("compute plan: %v", err)
	}
	logger.Printf("discovered %d pastes across %d identities", plan.pasteCount, len(plan.identities))

	var countersWritten, projectionsWritten int
	for _, id := range plan.identities {
		ip := plan.byIdentity[id]
		logger.Printf("identity %s: identity_bytes=%d pastes=%d", id, ip.bytes, len(ip.projections))
		if *dryRun {
			continue
		}
		// identity_bytes/<id> = the true live-version byte sum.
		if err := be.Put(keyIdentityBytes(id), formatCounter(ip.bytes)); err != nil {
			logger.Fatalf("write identity_bytes/%s: %v", id, err)
		}
		countersWritten++
		// identity_pastes/<id>/<slug> = value-bearing projection.
		for slug, proj := range ip.projections {
			body, err := json.Marshal(proj)
			if err != nil {
				logger.Fatalf("encode projection %s/%s: %v", id, slug, err)
			}
			if err := be.Put(keyIdentityPaste(id, slug), body); err != nil {
				logger.Fatalf("write identity_pastes/%s/%s: %v", id, slug, err)
			}
			projectionsWritten++
		}
	}

	if *dryRun {
		logger.Printf("dry-run complete: would write counters=%d projections=%d (no changes made)",
			len(plan.identities), plan.pasteCount)
		return
	}
	logger.Printf("migration complete: identities=%d counters=%d projections=%d",
		len(plan.identities), countersWritten, projectionsWritten)
}

// --- plan: the recomputed shale-shape, derived from authoritative rows -----

// identityPlan is one owner's recomputed derived state: the reservation
// counter (sum of live-version bytes) + the value-bearing projection for
// each paste the owner holds.
type identityPlan struct {
	bytes       int64
	projections map[string]identityPasteRow // slug -> projection
}

type migrationPlan struct {
	identities []string                 // sorted, for deterministic output
	byIdentity map[string]*identityPlan // identity -> recomputed state
	pasteCount int
}

// computePlan scans the authoritative pastes/* and versions/* rows
// (read-only) and recomputes every identity's shale-shape derived state.
//
// Discovery is from the authoritative pastes/* rows, NOT the
// identity_pastes index: each pastes/<slug> row carries its owner identity,
// giving a complete + safe slug->identity grouping even if the slatedb
// index is sparse or empty-markered.
//
// The counter follows the SHALE counter definition (docs/SPEC.md "One
// intentional behavior change"): the sum of NON-DELETED version sizes for
// every paste the owner holds, with NO read-time expiry exclusion. An
// expired-but-unswept paste's live bytes still count, because the shale
// counter sheds them at sweep time, not read time. This guarantees the
// counter never under-counts (which would let a migrated owner exceed the
// cap); it may transiently over-count an expired-unswept paste, which is
// the fail-safe direction and is reconciled at the next sweep.
func computePlan(be backend.Backend) (*migrationPlan, error) {
	plan := &migrationPlan{byIdentity: make(map[string]*identityPlan)}

	pastes, err := scanPrefix(be, []byte("pastes/"))
	if err != nil {
		return nil, fmt.Errorf("scan pastes: %w", err)
	}
	for _, item := range pastes {
		slug := strings.TrimPrefix(string(item.key), "pastes/")
		var p pasteRow
		if err := json.Unmarshal(item.value, &p); err != nil {
			return nil, fmt.Errorf("decode pastes/%s: %w", slug, err)
		}
		identity := p.Identity
		if identity == "" {
			return nil, fmt.Errorf("paste %s has empty identity", slug)
		}

		liveBytes, err := sumLiveVersionBytes(be, slug)
		if err != nil {
			return nil, fmt.Errorf("sum versions for %s: %w", slug, err)
		}

		ip := plan.byIdentity[identity]
		if ip == nil {
			ip = &identityPlan{projections: make(map[string]identityPasteRow)}
			plan.byIdentity[identity] = ip
		}
		ip.bytes += liveBytes
		// Projection denormalizes the paste HEAD fields, byte-for-byte the
		// same shape ShaleRepo's confirm step + reconciler write.
		ip.projections[slug] = identityPasteRow{
			Name:      p.Name,
			Size:      p.Size,
			CreatedAt: p.CreatedAt,
			ExpiresAt: p.ExpiresAt,
		}
		plan.pasteCount++
	}

	plan.identities = make([]string, 0, len(plan.byIdentity))
	for id := range plan.byIdentity {
		plan.identities = append(plan.identities, id)
	}
	sort.Strings(plan.identities)
	return plan, nil
}

// sumLiveVersionBytes sums the Size of every non-deleted version row of a
// slug, scanned from the authoritative versions/<slug>/ prefix. NO expiry
// filter (see computePlan's doc): the shale counter counts live versions
// regardless of the paste's expiry.
func sumLiveVersionBytes(be backend.Backend, slug string) (int64, error) {
	items, err := scanPrefix(be, []byte("versions/"+slug+"/"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range items {
		var v versionRow
		if err := json.Unmarshal(item.value, &v); err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.key, err)
		}
		if v.Deleted {
			continue
		}
		total += int64(v.Size)
	}
	return total, nil
}

// --- raw KV helpers --------------------------------------------------------

type scanItem struct {
	key   []byte
	value []byte
}

// scanPrefix collects every (key, value) under prefix via the backend's
// raw iterator. At ReplicationFactor=1 the values are stored verbatim (no
// LWW envelope), so the decoded JSON matches what ShaleRepo reads.
func scanPrefix(be backend.Backend, prefix []byte) ([]scanItem, error) {
	it, err := be.ScanPrefix(prefix)
	if err != nil {
		return nil, err
	}
	defer it.Close() //nolint:errcheck
	var out []scanItem
	for {
		k, v, err := it.Next()
		if err != nil {
			return nil, err
		}
		if k == nil && v == nil {
			break
		}
		out = append(out, scanItem{
			key:   append([]byte(nil), k...),
			value: append([]byte(nil), v...),
		})
	}
	return out, nil
}

// --- key builders + counter format (mirror shale_repo.go) ------------------

func keyIdentityBytes(identity string) []byte {
	return []byte("identity_bytes/" + identity)
}

func keyIdentityPaste(identity, slug string) []byte {
	return []byte("identity_pastes/" + identity + "/" + slug)
}

// formatCounter renders the counter the way ShaleRepo's parseCounter
// reads it: a plain base-10 integer, no padding.
func formatCounter(n int64) []byte {
	if n < 0 {
		n = 0
	}
	return []byte(strconv.FormatInt(n, 10))
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
