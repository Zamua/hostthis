// Package storage's SlateDB-backed metadata implementation.
//
// SlateDB-backed twin of paste_repo.go + sweep.go + keygate_repo.go.
// Both backends implement the same service-layer interfaces, so
// cmd/hostthisd picks one via HOSTTHIS_METADATA_BACKEND env var and
// the rest of the app is unaware of the choice. Canonical spec in
// docs/SPEC.md "Metadata storage backends".
//
// Build/runtime requirement: cgo + libslatedb_uniffi on the platform
// loader path. See Dockerfile + Makefile.
//
// # Key layout
//
// All keys are UTF-8 strings cast to []byte. Values are JSON unless
// noted. The layout is designed so every operation we need maps to
// either a single Get, a single Put, an atomic transaction
// (Db.Begin + DbTransaction.Commit), or a prefix Scan.
//
//	pastes/<slug>                      JSON {Identity, Kind, ContentSHA, Size, Name, PinnedVersion, CreatedAt, UpdatedAt, ExpiresAt}
//	versions/<slug>/<NNNN>             JSON {VerNum, Kind, ContentSHA, Size, CreatedAt, Deleted}
//	                                   NNNN is 4-digit zero-padded so prefix-scan + decode keeps numeric order
//	slug_owner/<slug>                  raw identity string (small; for visitor-side lookup)
//	identity_pastes/<identity>/<slug>  empty value (for "list by identity" prefix scan)
//	identity_first_seen/<identity>     RFC3339 timestamp (cached MIN(pastes.created_at))
//	expiry/<rfc3339>/<slug>            empty value (for sweep prefix scan to find pastes whose expires_at <= now)
//	keygate/<subnet>/<identity>        RFC3339 first-seen timestamp (Sybil rate limit)
//
// # Atomicity
//
// Every multi-key write opens a SlateDB transaction (SnapshotIsolation)
// and commits all puts/deletes atomically. SlateDB's writer_epoch
// fencing ensures only one writer is alive at once across processes -
// matches our single-replica rolling-deploy model.
//
// # Quota math
//
// SumActiveBytesByOwner scans versions/* for every paste in
// identity_pastes/<owner>/* and sums sizes of non-deleted rows.
// O(versions-owned-by-identity) per call. For low identity activity
// (the common case) this is fast enough to inline in the hot path;
// for heavy identities we'd want a cached counter (out of scope for
// this initial implementation; the sqlite backend isn't smarter
// either).

//go:build slatedb

package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	slatedb "slatedb.io/slatedb-go/uniffi"

	"github.com/Zamua/hostthis/internal/domain"
)

// SlateConfig captures the connection parameters for the SlateDB
// metadata store. NewSlateRepo writes these to the AWS_* process env
// vars before calling ObjectStoreResolve - that's how the underlying
// OpenDAL/object_store crate picks up S3 configuration (passing the
// same fields via URL query params does NOT work; the crate ignores
// them).
type SlateConfig struct {
	Endpoint  string // e.g. "http://minio:9000"; empty for AWS
	Region    string // e.g. "us-east-1"
	Bucket    string // bucket name (required)
	AccessKey string
	SecretKey string
	UseSSL    bool   // false → set AWS_ALLOW_HTTP=true (MinIO dev)
	DbName    string // logical db name within the bucket; key prefix for SlateDB files
}

// SlateRepo is the SlateDB-backed metadata store. Satisfies the
// same service-layer interfaces as PasteRepo (sqlite). Concurrent
// access from a single Go process is safe; multi-process writers are
// fenced via SlateDB's writer_epoch protocol.
type SlateRepo struct {
	db    *slatedb.Db
	store *slatedb.ObjectStore
}

// NewSlateRepo opens a SlateDB instance backed by the configured
// object store. Caller must Close() to flush + shut down cleanly.
// Sets process-global AWS_* env vars from cfg - the OpenDAL crate
// SlateDB uses internally reads them. Don't run two SlateRepo
// instances pointing at different buckets within the same process
// (the env-var write would collide).
func NewSlateRepo(cfg SlateConfig) (*SlateRepo, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("SlateConfig.Bucket required")
	}
	if cfg.DbName == "" {
		cfg.DbName = "hostthis-metadata"
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Endpoint != "" {
		os.Setenv("AWS_ENDPOINT_URL", cfg.Endpoint)
	}
	os.Setenv("AWS_REGION", cfg.Region)
	os.Setenv("AWS_ACCESS_KEY_ID", cfg.AccessKey)
	os.Setenv("AWS_SECRET_ACCESS_KEY", cfg.SecretKey)
	if !cfg.UseSSL {
		os.Setenv("AWS_ALLOW_HTTP", "true")
	}
	// Path-style addressing - MinIO + most non-AWS S3-compatibles
	// don't support virtual-hosted-style (bucket.host) without
	// custom DNS. Harmless on AWS proper too.
	os.Setenv("AWS_VIRTUAL_HOSTED_STYLE_REQUEST", "false")

	url := "s3://" + cfg.Bucket + "/"
	store, err := slatedb.ObjectStoreResolve(url)
	if err != nil {
		return nil, fmt.Errorf("resolve object store %q: %w", url, err)
	}
	builder := slatedb.NewDbBuilder(cfg.DbName, store)
	db, err := builder.Build()
	if err != nil {
		store.Destroy()
		return nil, fmt.Errorf("open slatedb: %w", err)
	}
	return &SlateRepo{db: db, store: store}, nil
}

// Close flushes pending writes + shuts down the underlying SlateDB.
func (r *SlateRepo) Close() error {
	if r.db != nil {
		if err := r.db.Shutdown(); err != nil {
			return fmt.Errorf("shutdown slatedb: %w", err)
		}
	}
	if r.store != nil {
		r.store.Destroy()
	}
	return nil
}

// --- JSON row schemas ------------------------------------------------------

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

func (p pasteRow) toDomain(slug domain.Slug) domain.Paste {
	return domain.Paste{
		Slug:          slug,
		Identity:      domain.Identity(p.Identity),
		Kind:          domain.ContentKind(p.Kind),
		ContentSHA:    p.ContentSHA,
		Size:          p.Size,
		Name:          p.Name,
		PinnedVersion: p.PinnedVersion,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		ExpiresAt:     p.ExpiresAt,
	}
}

func pasteFromDomain(p domain.Paste) pasteRow {
	return pasteRow{
		Identity:      p.Identity.String(),
		Kind:          string(p.Kind),
		ContentSHA:    p.ContentSHA,
		Size:          p.Size,
		Name:          p.Name,
		PinnedVersion: p.PinnedVersion,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		ExpiresAt:     p.ExpiresAt,
	}
}

func (v versionRow) toDomain(slug domain.Slug) domain.Version {
	return domain.Version{
		Slug:       slug,
		VerNum:     v.VerNum,
		Kind:       domain.ContentKind(v.Kind),
		ContentSHA: v.ContentSHA,
		Size:       v.Size,
		CreatedAt:  v.CreatedAt,
		Deleted:    v.Deleted,
	}
}

// --- Key builders ----------------------------------------------------------

func keyPaste(slug domain.Slug) []byte { return []byte("pastes/" + slug.String()) }

func keyVersion(slug domain.Slug, verNum int) []byte {
	return fmt.Appendf(nil, "versions/%s/%04d", slug.String(), verNum)
}

func prefixVersions(slug domain.Slug) []byte { return []byte("versions/" + slug.String() + "/") }

func keySlugOwner(slug domain.Slug) []byte { return []byte("slug_owner/" + slug.String()) }

func keyIdentityPaste(identity, slug string) []byte {
	return []byte("identity_pastes/" + identity + "/" + slug)
}

func prefixIdentityPastes(identity string) []byte {
	return []byte("identity_pastes/" + identity + "/")
}

func keyIdentityFirstSeen(identity string) []byte {
	return []byte("identity_first_seen/" + identity)
}

func keyExpiry(t time.Time, slug domain.Slug) []byte {
	return []byte("expiry/" + t.UTC().Format(time.RFC3339Nano) + "/" + slug.String())
}

func prefixExpiry() []byte { return []byte("expiry/") }

func keyKeygate(subnet, identity string) []byte {
	return []byte("keygate/" + subnet + "/" + identity)
}

func prefixKeygateSubnet(subnet string) []byte { return []byte("keygate/" + subnet + "/") }

// extractSlug pulls the trailing slug out of a key like
// "identity_pastes/<identity>/<slug>" or "expiry/<rfc3339>/<slug>".
func extractSlug(key []byte) string {
	s := string(key)
	idx := strings.LastIndex(s, "/")
	if idx < 0 {
		return s
	}
	return s[idx+1:]
}

// --- Generic helpers -------------------------------------------------------

// getJSON reads + JSON-decodes a value at key. Returns ErrNotFound
// when the key doesn't exist.
func (r *SlateRepo) getJSON(key []byte, out any) error {
	raw, err := r.db.Get(key)
	if err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	if raw == nil {
		return ErrNotFound
	}
	if err := json.Unmarshal(*raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	return nil
}

// txGetJSON reads + JSON-decodes inside a transaction.
func txGetJSON(tx *slatedb.DbTransaction, key []byte, out any) error {
	raw, err := tx.Get(key)
	if err != nil {
		return fmt.Errorf("tx.get %s: %w", key, err)
	}
	if raw == nil {
		return ErrNotFound
	}
	if err := json.Unmarshal(*raw, out); err != nil {
		return fmt.Errorf("tx decode %s: %w", key, err)
	}
	return nil
}

// txPutJSON encodes + puts inside a transaction.
func txPutJSON(tx *slatedb.DbTransaction, key []byte, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	if err := tx.Put(key, body); err != nil {
		return fmt.Errorf("tx.put %s: %w", key, err)
	}
	return nil
}

// scanPrefix collects all (key, value) pairs whose key starts with
// prefix. Used for list/prefix queries (versions of a paste, pastes
// of an identity, expired pastes, keygate-by-subnet).
func (r *SlateRepo) scanPrefix(prefix []byte) ([]scanItem, error) {
	it, err := r.db.ScanPrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("scan prefix %s: %w", prefix, err)
	}
	defer it.Destroy()
	var out []scanItem
	for {
		kv, err := it.Next()
		if err != nil {
			return nil, fmt.Errorf("scan next %s: %w", prefix, err)
		}
		if kv == nil {
			break
		}
		k := append([]byte(nil), kv.Key...)
		v := append([]byte(nil), kv.Value...)
		out = append(out, scanItem{Key: k, Value: v})
	}
	return out, nil
}

type scanItem struct {
	Key   []byte
	Value []byte
}

// --- PasteReader / PasteAdmin reads ----------------------------------------

func (r *SlateRepo) Get(slug domain.Slug) (domain.Paste, error) {
	var row pasteRow
	if err := r.getJSON(keyPaste(slug), &row); err != nil {
		return domain.Paste{}, err
	}
	return row.toDomain(slug), nil
}

func (r *SlateRepo) ListByOwner(owner string) ([]domain.Paste, error) {
	if owner == "" {
		return nil, nil
	}
	idx, err := r.scanPrefix(prefixIdentityPastes(owner))
	if err != nil {
		return nil, err
	}
	out := make([]domain.Paste, 0, len(idx))
	for _, item := range idx {
		slugStr := extractSlug(item.Key)
		slug := domain.Slug(slugStr)
		var row pasteRow
		if err := r.getJSON(keyPaste(slug), &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				// Index leaked past the paste; skip silently. A future
				// compaction-style fixup could clean these up.
				continue
			}
			return nil, err
		}
		p := row.toDomain(slug)
		// LatestVersion = max(ver_num) across non-deleted versions of this slug.
		latest, err := r.latestActiveVersion(slug)
		if err != nil {
			return nil, err
		}
		p.LatestVersion = latest
		out = append(out, p)
	}
	// Match sqlite ORDER BY expires_at ASC.
	sortByExpiresAt(out)
	return out, nil
}

func (r *SlateRepo) latestActiveVersion(slug domain.Slug) (int, error) {
	items, err := r.scanPrefix(prefixVersions(slug))
	if err != nil {
		return 0, err
	}
	latest := 0
	for _, it := range items {
		var v versionRow
		if err := json.Unmarshal(it.Value, &v); err != nil {
			return 0, fmt.Errorf("decode %s: %w", it.Key, err)
		}
		if v.Deleted {
			continue
		}
		if v.VerNum > latest {
			latest = v.VerNum
		}
	}
	if latest == 0 {
		latest = 1 // matches sqlite COALESCE(..., 1)
	}
	return latest, nil
}

func (r *SlateRepo) CountByOwner(owner string) (int, error) {
	if owner == "" {
		return 0, nil
	}
	idx, err := r.scanPrefix(prefixIdentityPastes(owner))
	if err != nil {
		return 0, err
	}
	return len(idx), nil
}

func (r *SlateRepo) SumActiveBytesByOwner(owner string, now time.Time) (int, error) {
	if owner == "" {
		return 0, nil
	}
	total, err := r.sumActiveBytesForOwner(owner, now)
	if err != nil {
		return 0, err
	}
	return int(total), nil
}

// sumActiveBytesForOwner walks every paste indexed under
// identity_pastes/<owner>/ and sums the sizes of non-deleted version
// rows attached to pastes whose expires_at > now. Used both by the
// public SumActiveBytesByOwner and by the quota check inside
// InsertWithQuotaCheck / AppendVersionWithQuotaCheck.
func (r *SlateRepo) sumActiveBytesForOwner(owner string, now time.Time) (int64, error) {
	idx, err := r.scanPrefix(prefixIdentityPastes(owner))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range idx {
		slugStr := extractSlug(item.Key)
		slug := domain.Slug(slugStr)
		var p pasteRow
		if err := r.getJSON(keyPaste(slug), &p); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // stale index entry
			}
			return 0, err
		}
		if !p.ExpiresAt.After(now) {
			continue // expired pastes don't count toward quota
		}
		versions, err := r.scanPrefix(prefixVersions(slug))
		if err != nil {
			return 0, err
		}
		for _, vit := range versions {
			var v versionRow
			if err := json.Unmarshal(vit.Value, &v); err != nil {
				return 0, fmt.Errorf("decode %s: %w", vit.Key, err)
			}
			if v.Deleted {
				continue
			}
			total += int64(v.Size)
		}
	}
	return total, nil
}

// sumServiceWideActiveBytes is the service-cap equivalent - sum over
// EVERY paste, not just one identity. Used inside the quota check.
// Implementation walks pastes/ then versions/<slug>/; O(active pastes)
// + O(versions per paste). For low-volume hostthis (today: <100 active
// pastes, <500 versions total) this is sub-millisecond.
func (r *SlateRepo) sumServiceWideActiveBytes(now time.Time) (int64, error) {
	pastes, err := r.scanPrefix([]byte("pastes/"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range pastes {
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if !p.ExpiresAt.After(now) {
			continue
		}
		slugStr := strings.TrimPrefix(string(item.Key), "pastes/")
		versions, err := r.scanPrefix(prefixVersions(domain.Slug(slugStr)))
		if err != nil {
			return 0, err
		}
		for _, vit := range versions {
			var v versionRow
			if err := json.Unmarshal(vit.Value, &v); err != nil {
				return 0, fmt.Errorf("decode %s: %w", vit.Key, err)
			}
			if v.Deleted {
				continue
			}
			total += int64(v.Size)
		}
	}
	return total, nil
}

func (r *SlateRepo) ListVersions(slug domain.Slug) ([]domain.Version, error) {
	items, err := r.scanPrefix(prefixVersions(slug))
	if err != nil {
		return nil, err
	}
	out := make([]domain.Version, 0, len(items))
	for _, item := range items {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		out = append(out, v.toDomain(slug))
	}
	// Match sqlite ORDER BY ver_num DESC.
	sortVersionsDesc(out)
	return out, nil
}

func (r *SlateRepo) GetVersion(slug domain.Slug, ver int) (domain.Version, error) {
	var row versionRow
	if err := r.getJSON(keyVersion(slug, ver), &row); err != nil {
		return domain.Version{}, err
	}
	return row.toDomain(slug), nil
}

func (r *SlateRepo) OwnerFirstSeen(owner string) (time.Time, error) {
	if owner == "" {
		return time.Time{}, nil
	}
	raw, err := r.db.Get(keyIdentityFirstSeen(owner))
	if err != nil {
		return time.Time{}, fmt.Errorf("owner first seen: %w", err)
	}
	if raw == nil {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, string(*raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("decode first seen: %w", err)
	}
	return t, nil
}

// --- Writes (each opens a SlateDB transaction) -----------------------------

func (r *SlateRepo) InsertWithQuotaCheck(p domain.Paste, serviceCap, userCap int64, now time.Time) error {
	// Service-wide cap + per-identity cap pre-checks happen OUTSIDE
	// the transaction window because SlateDB has no SUM operator;
	// scanning every key during a transaction would hold tx state
	// across many round-trips. Single-writer fencing means no other
	// process can sneak a write in between the check and the commit;
	// concurrent goroutines within this process are serialized by the
	// SI transaction at commit time.
	body := int64(p.Size)
	if serviceCap > 0 {
		total, err := r.sumServiceWideActiveBytes(now)
		if err != nil {
			return fmt.Errorf("service-wide sum: %w", err)
		}
		if total+body > serviceCap {
			return ErrServiceFull
		}
	}
	if userCap > 0 {
		ownerTotal, err := r.sumActiveBytesForOwner(p.Identity.String(), now)
		if err != nil {
			return fmt.Errorf("identity sum: %w", err)
		}
		if ownerTotal+body > userCap {
			return ErrOverUserQuota
		}
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Slug-collision: read; if a row exists, surface ErrSlugTaken so
	// the caller picks a different slug + retries. Read participates
	// in SI conflict detection.
	existing, err := tx.Get(keyPaste(p.Slug))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tx slug check: %w", err)
	}
	if existing != nil {
		_ = tx.Rollback()
		return ErrSlugTaken
	}

	if err := txPutJSON(tx, keyPaste(p.Slug), pasteFromDomain(p)); err != nil {
		_ = tx.Rollback()
		return err
	}
	v1 := versionRow{
		VerNum:     1,
		Kind:       string(p.Kind),
		ContentSHA: p.ContentSHA,
		Size:       p.Size,
		CreatedAt:  p.CreatedAt,
	}
	if err := txPutJSON(tx, keyVersion(p.Slug, 1), v1); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Put(keySlugOwner(p.Slug), []byte(p.Identity.String())); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put slug owner: %w", err)
	}
	if err := tx.Put(keyIdentityPaste(p.Identity.String(), p.Slug.String()), []byte{}); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put identity-paste index: %w", err)
	}
	if err := tx.Put(keyExpiry(p.ExpiresAt, p.Slug), []byte{}); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("put expiry index: %w", err)
	}

	// identity_first_seen: write only if absent (sqlite uses MIN(created_at)
	// across paste rows; here we cache it explicitly on first paste).
	fsKey := keyIdentityFirstSeen(p.Identity.String())
	fs, err := tx.Get(fsKey)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("first-seen check: %w", err)
	}
	if fs == nil {
		if err := tx.Put(fsKey, []byte(p.CreatedAt.UTC().Format(time.RFC3339Nano))); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("put first-seen: %w", err)
		}
	}

	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit insert %q: %w", p.Slug, err)
	}
	return nil
}

func (r *SlateRepo) Delete(slug domain.Slug) error {
	// Read the paste first to learn its identity + expires_at (need
	// both to clean up secondary indexes).
	var p pasteRow
	if err := r.getJSON(keyPaste(slug), &p); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // idempotent - sqlite's DELETE is also a no-op on missing rows
		}
		return err
	}
	// Enumerate version keys to delete.
	versions, err := r.scanPrefix(prefixVersions(slug))
	if err != nil {
		return err
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := tx.Delete(keyPaste(slug)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete paste: %w", err)
	}
	for _, v := range versions {
		if err := tx.Delete(v.Key); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete version key %s: %w", v.Key, err)
		}
	}
	if err := tx.Delete(keySlugOwner(slug)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete slug owner: %w", err)
	}
	if err := tx.Delete(keyIdentityPaste(p.Identity, slug.String())); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete identity-paste index: %w", err)
	}
	if err := tx.Delete(keyExpiry(p.ExpiresAt, slug)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete expiry index: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete %q: %w", slug, err)
	}
	return nil
}

func (r *SlateRepo) SetName(slug domain.Slug, name string) error {
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	var p pasteRow
	if err := txGetJSON(tx, keyPaste(slug), &p); err != nil {
		_ = tx.Rollback()
		return err
	}
	p.Name = name
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set name %q: %w", slug, err)
	}
	return nil
}

func (r *SlateRepo) SetPinnedVersion(slug domain.Slug, ver domain.Version) error {
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	var p pasteRow
	if err := txGetJSON(tx, keyPaste(slug), &p); err != nil {
		_ = tx.Rollback()
		return err
	}
	p.PinnedVersion = ver.VerNum
	p.ContentSHA = ver.ContentSHA
	p.Size = ver.Size
	p.Kind = string(ver.Kind)
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set pinned %q: %w", slug, err)
	}
	return nil
}

func (r *SlateRepo) Unpin(slug domain.Slug) error {
	// Need latest non-deleted version's head fields. Scan outside tx
	// is fine; commit detects conflicting writes.
	versions, err := r.scanPrefix(prefixVersions(slug))
	if err != nil {
		return err
	}
	var latest *versionRow
	for _, item := range versions {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if latest == nil || v.VerNum > latest.VerNum {
			vCopy := v
			latest = &vCopy
		}
	}
	if latest == nil {
		return ErrNotFound
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	var p pasteRow
	if err := txGetJSON(tx, keyPaste(slug), &p); err != nil {
		_ = tx.Rollback()
		return err
	}
	p.PinnedVersion = 0
	p.Kind = latest.Kind
	p.ContentSHA = latest.ContentSHA
	p.Size = latest.Size
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unpin %q: %w", slug, err)
	}
	return nil
}

func (r *SlateRepo) AppendVersionWithQuotaCheck(slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, serviceCap, userCap int64, now time.Time) (AppendResult, error) {
	// Need owner identity to do the per-user check.
	var existing pasteRow
	if err := r.getJSON(keyPaste(slug), &existing); err != nil {
		return AppendResult{}, err
	}
	body := int64(size)
	if serviceCap > 0 {
		total, err := r.sumServiceWideActiveBytes(now)
		if err != nil {
			return AppendResult{}, fmt.Errorf("service-wide sum: %w", err)
		}
		if total+body > serviceCap {
			return AppendResult{}, ErrServiceFull
		}
	}
	if userCap > 0 {
		ownerTotal, err := r.sumActiveBytesForOwner(existing.Identity, now)
		if err != nil {
			return AppendResult{}, fmt.Errorf("identity sum: %w", err)
		}
		if ownerTotal+body > userCap {
			return AppendResult{}, ErrOverUserQuota
		}
	}
	// MAX(ver_num) INCLUDING deleted rows - version numbers are not
	// reused (matches sqlite behavior).
	versions, err := r.scanPrefix(prefixVersions(slug))
	if err != nil {
		return AppendResult{}, err
	}
	maxVer := 0
	for _, item := range versions {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return AppendResult{}, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if v.VerNum > maxVer {
			maxVer = v.VerNum
		}
	}
	newVer := maxVer + 1
	expires := now.Add(domain.RetentionWindow)

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return AppendResult{}, fmt.Errorf("begin tx: %w", err)
	}
	var p pasteRow
	if err := txGetJSON(tx, keyPaste(slug), &p); err != nil {
		_ = tx.Rollback()
		return AppendResult{}, err
	}
	newV := versionRow{
		VerNum:     newVer,
		Kind:       string(kind),
		ContentSHA: contentSHA,
		Size:       size,
		CreatedAt:  now,
	}
	if err := txPutJSON(tx, keyVersion(slug, newVer), newV); err != nil {
		_ = tx.Rollback()
		return AppendResult{}, err
	}

	// Remove + re-add expiry index entry (the date in the key changes).
	if err := tx.Delete(keyExpiry(p.ExpiresAt, slug)); err != nil {
		_ = tx.Rollback()
		return AppendResult{}, fmt.Errorf("delete old expiry idx: %w", err)
	}
	p.UpdatedAt = now
	p.ExpiresAt = expires
	if p.PinnedVersion == 0 {
		p.Kind = string(kind)
		p.ContentSHA = contentSHA
		p.Size = size
	}
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return AppendResult{}, err
	}
	if err := tx.Put(keyExpiry(p.ExpiresAt, slug), []byte{}); err != nil {
		_ = tx.Rollback()
		return AppendResult{}, fmt.Errorf("put new expiry idx: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return AppendResult{}, fmt.Errorf("commit append %q: %w", slug, err)
	}
	return AppendResult{NewVer: newVer, WasPinned: existing.PinnedVersion != 0}, nil
}

func (r *SlateRepo) DeleteVersion(slug domain.Slug, ver int) error {
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	var v versionRow
	if err := txGetJSON(tx, keyVersion(slug, ver), &v); err != nil {
		_ = tx.Rollback()
		return err
	}
	v.Deleted = true
	if err := txPutJSON(tx, keyVersion(slug, ver), v); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete version %q v%d: %w", slug, ver, err)
	}
	return nil
}

// --- SweepRepo -------------------------------------------------------------

func (r *SlateRepo) ExpiredSlugs(now time.Time) ([]string, error) {
	items, err := r.scanPrefix(prefixExpiry())
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Format(time.RFC3339Nano)
	var out []string
	for _, item := range items {
		// key shape: expiry/<rfc3339>/<slug>. Compare timestamp lex (sortable).
		k := string(item.Key)
		rest := strings.TrimPrefix(k, "expiry/")
		idx := strings.LastIndex(rest, "/")
		if idx < 0 {
			continue
		}
		ts := rest[:idx]
		slug := rest[idx+1:]
		if ts <= cutoff {
			out = append(out, slug)
		}
	}
	return out, nil
}

// UnreferencedBlobSHAs returns the set of REFERENCED content SHAs -
// note the misleading name (inherited from the sqlite impl). The
// sweep treats the returned slice as the allow-list: any blob whose
// sha is NOT in this slice is deleted as orphan. Returning an empty
// slice while the bucket has blobs would tell the sweep "everything
// is orphan" and wipe the bucket - never stub this method to nil.
//
// A sha is "referenced" if it's the head sha of an active paste OR
// the content_sha of a non-deleted version row.
func (r *SlateRepo) UnreferencedBlobSHAs() ([]string, error) {
	pastes, err := r.scanPrefix([]byte("pastes/"))
	if err != nil {
		return nil, err
	}
	versions, err := r.scanPrefix([]byte("versions/"))
	if err != nil {
		return nil, err
	}
	referenced := make(map[string]struct{}, len(pastes)+len(versions))
	for _, item := range pastes {
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if p.ContentSHA != "" {
			referenced[p.ContentSHA] = struct{}{}
		}
	}
	for _, item := range versions {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if v.ContentSHA != "" && !v.Deleted {
			referenced[v.ContentSHA] = struct{}{}
		}
	}
	out := make([]string, 0, len(referenced))
	for sha := range referenced {
		out = append(out, sha)
	}
	return out, nil
}

// --- KeyGateRepo (Sybil rate limit) ----------------------------------------

func (r *SlateRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	if identity == "" || subnet == "" {
		return false, errors.New("identity + subnet required")
	}
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}

	// Fast path: already known?
	if raw, err := tx.Get(keyKeygate(subnet, identity)); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("keygate get: %w", err)
	} else if raw != nil {
		if _, err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit known: %w", err)
		}
		return true, nil
	}

	// New (identity, subnet) - count fresh keys in this subnet within window.
	items, err := r.scanPrefix(prefixKeygateSubnet(subnet))
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	cutoff := now.Add(-window)
	freshCount := 0
	for _, item := range items {
		t, err := time.Parse(time.RFC3339Nano, string(item.Value))
		if err != nil {
			continue
		}
		if t.After(cutoff) {
			freshCount++
		}
	}
	if freshCount >= limitPerSubnet {
		_ = tx.Rollback()
		return false, ErrTooManyNewKeys
	}
	if err := tx.Put(keyKeygate(subnet, identity), []byte(now.UTC().Format(time.RFC3339Nano))); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("keygate put: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit fresh: %w", err)
	}
	return false, nil
}

// SubnetSnapshot counts in-window rows for a subnet + finds the
// oldest first_seen value among them. Used by the rich Sybil refusal
// + by whoami's per-session budget block.
func (r *SlateRepo) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error) {
	items, err := r.scanPrefix(prefixKeygateSubnet(subnet))
	if err != nil {
		return 0, time.Time{}, err
	}
	cutoff := now.Add(-window)
	count := 0
	var oldest time.Time
	for _, item := range items {
		t, err := time.Parse(time.RFC3339Nano, string(item.Value))
		if err != nil {
			continue
		}
		if !t.After(cutoff) {
			continue
		}
		count++
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return count, oldest, nil
}

// SubnetsForIdentity counts distinct in-window subnets for an identity.
// Walks the global keygate prefix; cost is O(total keygate rows) which
// is bounded by the per-subnet cap × number of active subnets.
func (r *SlateRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	items, err := r.scanPrefix([]byte("keygate/"))
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-window)
	seen := make(map[string]struct{})
	for _, item := range items {
		// key shape: keygate/<subnet>/<identity>
		k := string(item.Key)
		rest := strings.TrimPrefix(k, "keygate/")
		idx := strings.LastIndex(rest, "/")
		if idx < 0 {
			continue
		}
		subnet := rest[:idx]
		id := rest[idx+1:]
		if id != identity {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, string(item.Value))
		if err != nil {
			continue
		}
		if !t.After(cutoff) {
			continue
		}
		seen[subnet] = struct{}{}
	}
	return len(seen), nil
}

func (r *SlateRepo) DeleteFirstSeenOlderThan(cutoff time.Time) (int, error) {
	items, err := r.scanPrefix([]byte("keygate/"))
	if err != nil {
		return 0, err
	}
	var toDelete [][]byte
	for _, item := range items {
		t, err := time.Parse(time.RFC3339Nano, string(item.Value))
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			toDelete = append(toDelete, item.Key)
		}
	}
	if len(toDelete) == 0 {
		return 0, nil
	}
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	for _, k := range toDelete {
		if err := tx.Delete(k); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("delete %s: %w", k, err)
		}
	}
	if _, err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit keygate prune: %w", err)
	}
	return len(toDelete), nil
}

// --- Sort helpers (avoid pulling sort.Slice into hot paths) ----------------

func sortByExpiresAt(ps []domain.Paste) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].ExpiresAt.Before(ps[j-1].ExpiresAt); j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

func sortVersionsDesc(vs []domain.Version) {
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && vs[j].VerNum > vs[j-1].VerNum; j-- {
			vs[j], vs[j-1] = vs[j-1], vs[j]
		}
	}
}

// --- Misc helpers ----------------------------------------------------------

// parseVerNumFromKey extracts the NNNN ver_num suffix from a key like
// "versions/<slug>/<NNNN>". Returns 0 + error on malformed keys.
func parseVerNumFromKey(key []byte) (int, error) {
	s := string(key)
	if !bytes.HasPrefix(key, []byte("versions/")) {
		return 0, fmt.Errorf("not a version key: %s", s)
	}
	idx := bytes.LastIndexByte(key, '/')
	if idx < 0 {
		return 0, fmt.Errorf("malformed version key: %s", s)
	}
	return strconv.Atoi(string(key[idx+1:]))
}
