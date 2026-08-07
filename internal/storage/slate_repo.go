// SlateDB-backed metadata implementation, satisfying the same service-layer
// interfaces as the other backends used; cmd/hostthisd picks one via
// HOSTTHIS_METADATA_BACKEND and the rest of the app is unaware. Spec:
// docs/SPEC.md "Metadata storage backends". Needs cgo + libslatedb_uniffi on
// the loader path.
//
// # Key layout
//
// UTF-8 keys, JSON values unless noted. The layout is chosen so every operation
// maps to a single Get, a single Put, one transaction, or a prefix scan.
//
//	pastes/<slug>                      JSON paste row
//	versions/<slug>/<NNNN>             JSON version row; NNNN zero-padded so a
//	                                   prefix scan keeps numeric order
//	slug_owner/<slug>                  raw identity, for visitor-side lookup
//	identity_pastes/<identity>/<slug>  empty value, for list-by-identity
//	identity_first_seen/<identity>     RFC3339, cached MIN(created_at)
//	keygate/<subnet>/<identity>        RFC3339 first-seen, Sybil rate limit
//
// # Atomicity
//
// Every multi-key write commits in one SnapshotIsolation transaction. SlateDB's
// writer_epoch fencing keeps exactly one writer alive across processes, which
// matches the single-replica rolling-deploy model.
//
// # Quota math
//
// SumActiveBytesByOwner scans versions/* for every paste in
// identity_pastes/<owner>/* and sums non-deleted rows: O(versions owned). Fast
// enough inline for low per-identity activity; a heavy identity would want a
// cached counter.

//go:build slatedb

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"sync"
	"time"

	slatedb "slatedb.io/slatedb-go/uniffi"

	"github.com/Zamua/hostthis/internal/domain"
)

// SlateConfig captures the connection parameters for the SlateDB metadata
// store. NewSlateRepo writes these to the AWS_* process env vars before calling
// ObjectStoreResolve: that is the only way the underlying OpenDAL/object_store
// crate picks up S3 configuration, since it ignores the same fields passed as
// URL query params.
type SlateConfig struct {
	Endpoint  string // e.g. "http://minio:9000"; empty for AWS
	Region    string // e.g. "us-east-1"
	Bucket    string // bucket name (required)
	AccessKey string
	SecretKey string
	UseSSL    bool   // false sets AWS_ALLOW_HTTP=true (MinIO dev)
	DbName    string // logical db name within the bucket; key prefix for SlateDB files
}

// SlateRepo is the SlateDB-backed metadata store. Concurrent access from a
// single Go process is safe; multi-process writers are fenced via SlateDB's
// writer_epoch protocol.
type SlateRepo struct {
	db    *slatedb.Db
	store *slatedb.ObjectStore

	// quotaLocks serializes the per-identity quota-check-and-write so two
	// concurrent same-identity uploads cannot both read the pre-upload sum,
	// both pass the cap, and both commit. Snapshot isolation does NOT
	// serialize them: they write DIFFERENT keys (distinct slugs), so SI sees
	// no write-write conflict, while the shared per-identity SUM is read
	// outside any key the transaction conflicts on. SlateDB is single-writer,
	// so no other process can interleave and an in-process lock suffices.
	// Striped by identity hash so different identities do not contend. The
	// service-wide cap stays best-effort per the spec.
	quotaLocks [256]sync.Mutex

	// keygateLocks serializes the per-subnet Sybil count-and-admit for the same
	// reason quotaLocks exists, and it is NOT redundant with the transaction:
	// two first-sight identities in one subnet write DIFFERENT keygate keys, so
	// SI sees no write-write conflict, while the budget count is a prefix scan
	// no key in the read-set covers. Without the stripe N simultaneous new keys
	// from one subnet each read count = limit-1 and all N are admitted.
	// Striped by subnet hash so different subnets do not contend.
	keygateLocks [256]sync.Mutex
}

// lockQuota acquires the per-identity quota stripe and returns the unlock. Hold
// it across the quota SUM and the write transaction so the check and the commit
// are atomic against other same-identity uploads.
func (r *SlateRepo) lockQuota(identity string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(identity))
	m := &r.quotaLocks[h.Sum32()%uint32(len(r.quotaLocks))]
	m.Lock()
	return m.Unlock
}

// lockKeygate acquires the per-subnet Sybil stripe and returns the unlock. Hold
// it across the in-window count and the admitting commit so the check and the
// write are atomic against other admits for the same subnet.
func (r *SlateRepo) lockKeygate(subnet string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(subnet))
	m := &r.keygateLocks[h.Sum32()%uint32(len(r.keygateLocks))]
	m.Lock()
	return m.Unlock
}

// NewSlateRepo opens a SlateDB instance backed by the configured object store.
// The caller must Close to flush and shut down cleanly. cfg is applied as
// process-global AWS_* env vars, so two SlateRepo instances pointing at
// different buckets cannot coexist in one process.
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
	// Path-style addressing: MinIO and most non-AWS S3-compatibles need
	// custom DNS for virtual-hosted-style (bucket.host). Harmless on AWS.
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

// Close flushes pending writes and shuts down the underlying SlateDB.
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

// --- Key builders ----------------------------------------------------------

func keyPaste(slug domain.Slug) []byte { return shaleKey(prefixPastes, slug.String()) }

func keyVersion(slug domain.Slug, verNum int) []byte {
	return fmt.Appendf(nil, "versions/%s/%04d", slug.String(), verNum)
}

func prefixVersions(slug domain.Slug) []byte { return []byte("versions/" + slug.String() + "/") }

func keySlugOwner(slug domain.Slug) []byte { return shaleKey(prefixSlugOwner, slug.String()) }

func keyIdentityPaste(identity, slug string) []byte {
	return []byte("identity_pastes/" + identity + "/" + slug)
}

func prefixIdentityPastes(identity string) []byte {
	return []byte("identity_pastes/" + identity + "/")
}

func keyIdentityFirstSeen(identity string) []byte {
	return []byte("identity_first_seen/" + identity)
}

func keyKeygate(subnet, identity string) []byte {
	return []byte("keygate/" + subnet + "/" + identity)
}

func prefixKeygateSubnet(subnet string) []byte { return []byte("keygate/" + subnet + "/") }

// --- Generic helpers -------------------------------------------------------

// getJSON decodes the value at key, returning ErrNotFound when it is absent.
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

// txGetJSON is getJSON inside a transaction, so the read joins SI conflict
// detection.
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

// scanPrefix collects every (key, value) pair under prefix, copying both out of
// the iterator's buffers.
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
				continue // index entry outlived its paste; skip the orphan
			}
			return nil, err
		}
		p := row.toDomain(slug)
		latest, err := r.latestActiveVersion(slug)
		if err != nil {
			return nil, err
		}
		p.LatestVersion = latest
		out = append(out, p)
	}
	sortByUpdatedAtDesc(out) // newest first
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
		latest = 1 // no live version: the head still names v1
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
	// Resolve each derived-index entry to its authoritative row and skip
	// orphans, mirroring ListByOwner. A raw len(idx) counts orphans too, and
	// whoami then disagrees with list.
	live := 0
	for _, item := range idx {
		slug := domain.Slug(extractSlug(item.Key))
		var row pasteRow
		if gerr := r.getJSON(keyPaste(slug), &row); gerr != nil {
			if errors.Is(gerr, ErrNotFound) {
				continue
			}
			return 0, gerr
		}
		live++
	}
	return live, nil
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
// identity_pastes/<owner>/ and sums the sizes of their non-deleted version
// rows.
func (r *SlateRepo) sumActiveBytesForOwner(owner string, _ time.Time) (int64, error) {
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

// sumLiveVersionBytesForSlug sums a paste's non-deleted version rows.
func (r *SlateRepo) sumLiveVersionBytesForSlug(slug domain.Slug) (int64, error) {
	items, err := r.scanPrefix(prefixVersions(slug))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, item := range items {
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return 0, fmt.Errorf("decode %s: %w", item.Key, err)
		}
		if v.Deleted {
			continue
		}
		total += int64(v.Size)
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
	sortVersionsDesc(out) // newest version first
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

// ctx satisfies the service.PasteRepo interface (the shale backend carries
// staged blob refs on it); the direct slate path has no shale-blob plane.
func (r *SlateRepo) InsertWithQuotaCheck(_ context.Context, p domain.Paste, userCap int64, now time.Time) error {
	// The per-identity cap pre-check runs OUTSIDE the transaction: SlateDB
	// has no SUM operator, so scanning every key inside one would hold tx
	// state across many round-trips. Single-writer fencing keeps another
	// PROCESS from interleaving, but concurrent goroutines here write
	// different keys and so raise no SI conflict; the quota stripe is what
	// stops two same-identity uploads both passing the cap and both landing.
	// The durable total-bytes ceiling is the object-store bucket quota,
	// enforced when a blob Put is rejected, not here (SPEC "Limits").
	defer r.lockQuota(p.Identity.String())()
	body := int64(p.Size)
	if userCap > 0 {
		// The per-owner cap counts BOTH paste and site bytes, symmetric with
		// the site deploy path and with the per-identity sum.
		// Without the site term an 800-byte site plus a 300-byte paste would
		// pass a 1000-byte cap.
		ownerPaste, err := r.sumActiveBytesForOwner(p.Identity.String(), now)
		if err != nil {
			return fmt.Errorf("identity paste sum: %w", err)
		}
		ownerSite, err := r.sumActiveSiteBytesForOwner(p.Identity.String(), now)
		if err != nil {
			return fmt.Errorf("identity site sum: %w", err)
		}
		if err := (domain.Allowance{Cap: userCap, Used: ownerPaste + ownerSite}).Admit(body); err != nil {
			return err
		}
	}

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Slug collision: ErrSlugTaken tells the caller to pick another slug and
	// retry. The read participates in SI conflict detection.
	existing, err := tx.Get(keyPaste(p.Slug))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tx slug check: %w", err)
	}
	if existing != nil {
		_ = tx.Rollback()
		return ErrSlugTaken
	}
	// A slug must be unique across sites too, since a read resolves a slug in
	// either family. The site insert makes the mirror-image check.
	existingSite, err := tx.Get(keySite(p.Slug))
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tx site slug check: %w", err)
	}
	if existingSite != nil {
		_ = tx.Rollback()
		return ErrSlugTaken
	}

	v1 := newVersionRow(1, contentRefFromDomain(p), p.CreatedAt)
	pr := pasteFromDomain(p)
	// The head serves v1, so it takes v1's descriptor WHOLE - the same roll an
	// append and a pin perform.
	pr.contentRef = v1.contentRef
	if err := txPutJSON(tx, keyPaste(p.Slug), pr); err != nil {
		_ = tx.Rollback()
		return err
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

	// identity_first_seen is write-once: it is the MIN(created_at)
	// across paste rows, so overwriting here would move it forward.
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
	// Identity is a secondary-index key, so the row must be read before it can
	// be removed.
	var p pasteRow
	if err := r.getJSON(keyPaste(slug), &p); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // idempotent: deleting a missing row is not an error
		}
		return err
	}
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

// MarkReady flips a paste's status pending -> ready. Only a still-pending
// paste transitions, so a late finalizer cannot resurrect a failed one; any
// other state, or a missing paste, is a no-op. docs/SPEC.md "Paste lifecycle
// status (async blob write)".
func (r *SlateRepo) MarkReady(slug domain.Slug) error {
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	var p pasteRow
	if err := txGetJSON(tx, keyPaste(slug), &p); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if domain.NormalizeStatus(p.Status) != domain.PasteStatusPending {
		_ = tx.Rollback()
		return nil
	}
	p.Status = string(domain.PasteStatusReady)
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mark ready %q: %w", slug, err)
	}
	return nil
}

// MarkFailed flips a paste's status pending -> failed and releases its quota by
// deleting the identity_pastes index entry, which is what the quota SUM walks.
// The row itself stays, flipped to failed, so a read can serve an error page.
// Only a still-pending paste transitions, so a ready paste is never un-counted,
// and a second call is a no-op. docs/SPEC.md "Paste lifecycle status (async
// blob write)".
func (r *SlateRepo) MarkFailed(slug domain.Slug) error {
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	var p pasteRow
	if err := txGetJSON(tx, keyPaste(slug), &p); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if domain.NormalizeStatus(p.Status) != domain.PasteStatusPending {
		_ = tx.Rollback()
		return nil
	}
	p.Status = string(domain.PasteStatusFailed)
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Delete(keyIdentityPaste(p.Identity, slug.String())); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete identity-paste index: %w", err)
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mark failed %q: %w", slug, err)
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
	// Repoint the head's served descriptor as ONE value. The version ROW
	// carries the full contentRef including BlobID, which domain.Version does
	// not, so the head cannot drift a field out of sync.
	var vr versionRow
	if err := txGetJSON(tx, keyVersion(slug, ver.VerNum), &vr); err != nil {
		_ = tx.Rollback()
		return err
	}
	p.PinnedVersion = ver.VerNum
	p.contentRef = vr.contentRef
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set pinned %q: %w", slug, err)
	}
	return nil
}

// Unpin clears the pin and rolls the head to the latest LIVE version, the same
// rule latestActiveVersion and the read path apply. Tombstoned versions are
// skipped: pointing the head at one would serve bytes the owner deleted, or
// 404 once the GC reclaims them. ErrNotFound when no live version remains.
func (r *SlateRepo) Unpin(slug domain.Slug) error {
	// Scanning for the latest version outside the tx is safe: the commit
	// still detects a conflicting write.
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
		if v.Deleted {
			continue
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
	p.contentRef = latest.contentRef // the whole served descriptor rolls, never one field
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unpin %q: %w", slug, err)
	}
	return nil
}

// ctx satisfies the service.PasteAdmin interface; the direct slate path has no
// shale-blob plane.
func (r *SlateRepo) AppendVersionWithQuotaCheck(_ context.Context, slug domain.Slug, kind domain.ContentKind, contentSHA string, size int, userCap int64, now time.Time) (AppendResult, error) {
	// The owner identity is needed to pick the quota stripe, so it is read
	// first, unlocked; the stripe then spans the sum and the append so two
	// concurrent same-identity writes cannot both pass the cap (see
	// quotaLocks). The durable total-bytes ceiling is the object-store bucket
	// quota, not checked here.
	var existing pasteRow
	if err := r.getJSON(keyPaste(slug), &existing); err != nil {
		return AppendResult{}, err
	}
	defer r.lockQuota(existing.Identity)()
	body := int64(size)
	if userCap > 0 {
		// The per-owner cap counts BOTH paste and site bytes, so an append
		// cannot ignore the owner's existing site bytes.
		ownerPaste, err := r.sumActiveBytesForOwner(existing.Identity, now)
		if err != nil {
			return AppendResult{}, fmt.Errorf("identity paste sum: %w", err)
		}
		ownerSite, err := r.sumActiveSiteBytesForOwner(existing.Identity, now)
		if err != nil {
			return AppendResult{}, fmt.Errorf("identity site sum: %w", err)
		}
		if err := (domain.Allowance{Cap: userCap, Used: ownerPaste + ownerSite}).Admit(body); err != nil {
			return AppendResult{}, err
		}
	}
	// MAX(ver_num) INCLUDING deleted rows: version numbers are never reused.
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

	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return AppendResult{}, fmt.Errorf("begin tx: %w", err)
	}
	var p pasteRow
	if err := txGetJSON(tx, keyPaste(slug), &p); err != nil {
		_ = tx.Rollback()
		return AppendResult{}, err
	}
	newV := newVersionRow(newVer,
		contentRef{Kind: string(kind), ContentSHA: contentSHA, Size: size},
		now)
	if err := txPutJSON(tx, keyVersion(slug, newVer), newV); err != nil {
		_ = tx.Rollback()
		return AppendResult{}, err
	}

	p.UpdatedAt = now
	if p.PinnedVersion == 0 {
		p.contentRef = newV.contentRef // an unpinned head rolls whole, never one field
	}
	if err := txPutJSON(tx, keyPaste(slug), p); err != nil {
		_ = tx.Rollback()
		return AppendResult{}, err
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

// --- KeyGateRepo (Sybil rate limit) ----------------------------------------

// AdmitNewKey checks the subnet's in-window budget and admits the pair, both
// under the per-subnet stripe: the budget is a prefix scan, which snapshot
// isolation cannot serialize (concurrent admits write different keys and so
// raise no conflict), and SlateDB is single-writer, so an in-process lock is
// the whole boundary.
//
// The same transaction drops this subnet's out-of-window rows: they cannot
// change an admission decision, so the scan that walks past them removes them
// and the family stays bounded with no background pass (docs/SPEC.md "Sybil
// rate limit").
func (r *SlateRepo) AdmitNewKey(identity, subnet string, now time.Time, limitPerSubnet int, window time.Duration) (knownAlready bool, err error) {
	if identity == "" || subnet == "" {
		return false, errors.New("identity + subnet required")
	}
	defer r.lockKeygate(subnet)()
	tx, err := r.db.Begin(slatedb.IsolationLevelSnapshot)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}

	// A known (identity, subnet) never counts against the budget again.
	if raw, err := tx.Get(keyKeygate(subnet, identity)); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("keygate get: %w", err)
	} else if raw != nil {
		if _, err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit known: %w", err)
		}
		return true, nil
	}

	// New pair: admit only while the subnet's in-window key count is under
	// the limit.
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
			// Undecodable: cannot be shown to be out of window, so it is left
			// alone rather than deleted.
			continue
		}
		if t.After(cutoff) {
			freshCount++
			continue
		}
		if err := tx.Delete(item.Key); err != nil {
			_ = tx.Rollback()
			return false, fmt.Errorf("drop expired keygate row %s: %w", item.Key, err)
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

// SubnetSnapshot counts a subnet's in-window rows and the oldest first_seen
// among them, which together tell a refused user when budget frees up.
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

// SubnetsForIdentity counts distinct in-window subnets for an identity. There
// is no identity-first index, so it walks the whole keygate prefix: O(total
// keygate rows), bounded by the per-subnet cap times the active subnets.
func (r *SlateRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	items, err := r.scanPrefix([]byte("keygate/"))
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-window)
	seen := make(map[string]struct{})
	for _, item := range items {
		// keygate/<subnet>/<identity>; identity is the slash-free suffix.
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

// --- Sort helpers -----------------------------------------------------------

// Insertion sorts: these slices are one owner's pastes or one paste's versions,
// small enough that sort.Slice's reflection costs more than it saves.
