# hostthis blobs through shale (*BlobKV) - phase 3 design

Status: design / no code. The contract to poke holes in before implementation.

This is PHASE 3 of the shale streaming-blob effort (shale's
`docs/design/blob-values.md` section 9 phasing; phase 2 = the cluster `*BlobKV`
surface, committed at shale `886690a` on `feat/blob-values`). It supersedes the
slug-scoped detached-blob model on the `feat/slug-scoped-blobs` branch.

The goal (blob-values.md section 11.10, the "phase 2 defers to phase 3" list):
hostthis's blobs go THROUGH shale - collocated with the metadata on the owning
shard, transactionally co-committed, streamed end to end - replacing the
detached S3/disk blob store, the slug-scoped crash-orphan reconcile, and the
site reservation.

## 0. What shale gives us (the phase-2 surface, as built)

From `github.com/Zamua/shale/pkg/cluster/kv.go` + `pkg/blob`:

```go
// pkg/cluster
type BlobKV struct{ /* embeds *KV; holds blob.Store */ }
func NewBlobKV(cfg Config) (*BlobKV, error)               // requires cfg.BlobStore != nil

func (b *BlobKV) StageBlob(ctx, routeKey []byte, r io.Reader, size int64) (BlobRef, error)
func (b *BlobKV) GetBlob(ctx, routeKey []byte, blobid string) (io.ReadCloser, int64, error)
func (b *BlobKV) SweepOrphans(ctx, now time.Time, grace time.Duration) error
func (b *BlobKV) Transact(pinKey []byte, fn func(*BlobTx) error) error  // shadows *KV.Transact

// *BlobTx embeds *Tx (Get/Put/Delete) and adds:
func (bt *BlobTx) BindBlob(ref BlobRef) error
func (bt *BlobTx) UnbindBlob(ref BlobRef) error

// BlobRef is opaque to the app: {Unit, RouteShard []byte, BlobID, Size, ContentHash}
```

Properties phase 2 already guarantees (we build ON these, do not re-derive
them):

- **Reader-atomic create.** `StageBlob` streams the bytes to the FINAL,
  unit-keyed object key (`blob/<unit>/<blobid>`) OUTSIDE any transaction. Those
  bytes are invisible to every reader until a `BindBlob` writes the pointer
  (`bref/{<routeShard>}/<unit>/<blobid>`) in a routed transaction. The bind
  co-commits with the app's metadata Put in ONE single-shard CAS.
- **Atomic delete.** `UnbindBlob` (or a plain `tx.Delete(brefKey)`) removes the
  pointer in the SAME transaction that removes the app's metadata. The bytes go
  unreferenced and are reclaimed by `SweepOrphans` (age-gated, mounted-unit-local).
- **Bytes never cross gRPC.** `blob.Store` (the MinIO adapter) is a plain
  object-store client; `StageBlob`/`GetBlob` go node -> object store directly.
  Only the small pointer routes through the ring.
- **Co-routing requires the app's ShardKeyFn to honor hash tags for `bref/`.**
  `brefKey` puts the route shard key in a `{...}` Redis hash tag. The default
  `ring.ShardKey` extracts it; a CUSTOM ShardKeyFn (hostthis has one) MUST add a
  `bref/` case (blob-values.md 11.5). This is THE one wiring requirement on us.

## 1. Architecture: where *BlobKV lands in hostthis

Today (`feat/slug-scoped-blobs`) hostthis has TWO independent stores:

- **metadata**: `ShaleRepo` wraps a `*cluster.Cluster` (slate backend, sharded
  by `shaleShardKey`). Three backends total: sqlite (`PasteRepo`), slatedb
  (`SlateRepo`), shale (`ShaleRepo`), all satisfying the same service interfaces.
- **blobs**: a detached `CompressedBlobStore` over a slug-scoped disk or S3
  store, poked directly by the services. Coordinated with metadata by
  ordered-writes (metadata-first) + the crash-orphan reconcile + the site
  reservation.

Phase 3 fuses these for the shale backend only: `ShaleRepo` holds a
`*cluster.BlobKV` instead of a bare `*cluster.Cluster`, and the blob byte plane
is the SAME MinIO the metadata's object store already runs on (a separate blob
bucket). The detached store, the reconcile, and the site reservation are deleted
from the shale path.

```
            hostthis services (upload / manage / deploy_site / http read / sweep)
                          |                                  |
              metadata interfaces                   blob interface (the SEAM, section 3)
              (PasteRepo, PasteAdmin, ...)           (ShaleBlobStore | StandaloneBlobStore)
                          |                                  |
   +----------------------+----------------+        +--------+----------------------------+
   | sqlite | slatedb | shale (ShaleRepo)  |        | shale path: ShaleRepo's *BlobKV     |
   +--------+---------+--------------------+        | standalone path: CompressedBlobStore|
                          |                         +-------------------------------------+
                  *cluster.BlobKV  ---- StageBlob/Bind/Get/Sweep ----> MinIO blob bucket
                          |
                  routed pointer (bref/) co-commits with pastes/<slug>, sites/<slug>
```

## 2. Backend divergence: shale uses *BlobKV, sqlite/slatedb keep BlobStore

**Decision: pragmatic split, NOT unification.**

- **Prod (shale backend)**: blobs go through `*BlobKV`. Transactional
  collocation: the pointer co-commits with the metadata on the owning shard.
- **Dev (sqlite, slatedb backends)**: keep the existing detached
  `CompressedBlobStore` (disk or S3) with ordered-writes (metadata-first) + the
  crash-orphan reconcile.

**Why not unify:**

1. The `*BlobKV` transactional collocation is intrinsically a CLUSTER property:
   the pointer is a routed slatedb value on the shard the metadata routes to.
   sqlite has no shard, no routed transaction, no `bref/` keyspace. slatedb
   (the `SlateRepo` direct path) talks to one SlateDB instance with no cluster
   layer above it - there is no `cluster.Transact`, hence no co-commit vehicle.
   You cannot give sqlite/slatedb the co-committed-pointer guarantee without
   building a cluster under them, which is exactly what the shale backend IS.
2. sqlite/slatedb are dev/test backends (`make run`, `make test`, single-node
   local). Their ordered-writes + reconcile model is already built, tested, and
   adequate for a single process with a local disk. Porting them to a
   pointer-and-sweep model with no cluster buys nothing and adds a fake
   "unit" concept they have no use for.
3. The whole point of phase 2's capability-in-the-type (`*KV` vs `*BlobKV`) is
   that a backend WITHOUT a configured blob store cannot reach the blob ops at
   compile time. sqlite/slatedb deliberately do not configure one.

So: prod gets the transactional guarantee; dev keeps the simpler model that is
correct for a single process. This mirrors how hostthis already tolerates
backend divergence (shale has the reservation-pattern quota + derived indexes;
sqlite has FK cascades + a real `WHERE`).

### 2.1 The service seam (what lets upload/manage/deploy work against either)

The services must call blob operations without knowing whether they are on the
transactional shale path or the standalone path. Today they hold a
`service.BlobStore` (`Put`/`PutPrecompressed`/`Get`) + a `service.BlobDeleter`
(`RemoveOwner`/`Remove`). Those interfaces bake in the slug-scoped,
detached-store model (a `Put` with no transaction, a `RemoveOwner` of a whole
prefix). The shale path has no whole-prefix delete and needs the blob bound
inside the metadata transaction.

**Introduce a new seam: `service.BlobUnit`** - a per-record blob lifecycle
abstraction that BOTH paths satisfy. It is small, names the OPERATION the
service performs (not the storage mechanism), and keeps the domain pure (the
domain never sees it; only the application services do).

```go
// service package - the seam. Names the lifecycle, hides the mechanism.
//
// The "record" is a paste or a site, identified by its slug (the route key).
// A blob is identified within a record by its content sha (the app's existing
// content-addressing); the seam maps (slug, sha) -> an opaque handle the commit
// uses. The shale path's handle wraps a cluster.BlobRef; the standalone path's
// handle is just (slug, sha).
type BlobUnit interface {
    // Stage durably writes the (already magic+zstd-encoded) bytes for this
    // record's blob and returns an opaque handle to bind. On the shale path it
    // is StageBlob -> the final unit-keyed object (reader-invisible until
    // Commit). On the standalone path it is a Put to the detached store
    // (immediately readable; the ordered-writes model relies on metadata-first
    // ordering, preserved by the service calling Stage before the metadata
    // write, exactly as today).
    Stage(ctx context.Context, slug, sha string, body []byte) (BlobHandle, error)

    // Commit persists the record's metadata AND binds every staged handle in
    // ONE atomic unit. fn buffers the metadata writes (the repo's existing
    // per-record tx body); the seam adds the blob binds. On the shale path this
    // is BlobKV.Transact(slug, ...) with BindBlob(handle) + the metadata Puts
    // co-committed. On the standalone path fn runs against the metadata repo's
    // own transaction (or its reservation sequence) and Stage already wrote the
    // bytes, so Commit is just the metadata write - the binds are no-ops.
    //
    // NB: the metadata writes inside fn are the SAME repo operations the repo
    // exposes today (InsertWithQuotaCheck etc.); see section 2.2 for how the
    // reservation pattern reconciles with a single co-commit.

    // Read streams a record's blob bytes (decompressed by the caller's existing
    // compression layer, or raw if the seam decompresses - see section 4 read).
    Read(ctx context.Context, slug, sha string) (io.ReadCloser, int64, error)

    // UnbindOnDelete removes a record's blob references as part of the metadata
    // delete. Shale path: UnbindBlob in the metadata-delete transaction.
    // Standalone path: RemoveOwner(slug) after the metadata delete (today's
    // ordered model). Exposed so the service deletes uniformly.
}

type BlobHandle struct {
    // Opaque. Shale path: holds a cluster.BlobRef. Standalone path: holds
    // (slug, sha). The service never inspects it; it threads it into Commit.
}
```

This is the MINIMAL seam: `Stage` (slow streaming), `Commit` (atomic
metadata+bind), `Read`, `UnbindOnDelete`. It is the application-layer mirror of
shale's own StageBlob/BindBlob split (which exists for the same reason: the slow
write must be outside the transaction). The two concrete implementations:

- **`ShaleBlobUnit`** (new, in `internal/storage`, `-tags slatedb`): wraps the
  `*cluster.BlobKV`. `Stage` = `StageBlob`; `Commit` = `BlobKV.Transact` with
  `BindBlob` + the metadata writes co-committed; `Read` = `GetBlob`;
  `UnbindOnDelete` = `UnbindBlob` inside the delete transaction.
- **`StandaloneBlobUnit`** (new, thin adapter in `internal/storage`): wraps the
  existing `CompressedBlobStore` + `BlobDeleter` + the metadata repo. `Stage` =
  `PutPrecompressed`; `Commit` = the metadata repo's existing
  Insert/Append/Replace (Stage already wrote the bytes; metadata-first ordering
  is preserved by the service's Stage-before-Commit order, see section 4);
  `Read` = `GetReader`; `UnbindOnDelete` = `RemoveOwner`.

The service code path is then ONE shape for both backends.

### 2.2 The reservation pattern vs the single co-commit (the load-bearing wrinkle)

There is a real tension to resolve here, grounded in the code. hostthis's shale
metadata writes are NOT a single transaction today: Insert/Append/Delete span
the `{id}` counter shard and the `{slug}` authoritative shard, which "cannot be
one transaction" (`shale_repo.go` "Cross-family writes: the reservation
pattern"). They are a SEQUENCE: reserve on `{id}`, authoritative write on
`{slug}`, confirm on `{id}`.

`BindBlob` co-commits with a SINGLE-shard transaction (the `{slug}` shard - the
pointer's hash tag routes there). So the blob can co-commit ONLY with the
AUTHORITATIVE `{slug}` write (step 2), NOT with the whole reserve/confirm
sequence.

**Decision: the blob binds in the authoritative `{slug}` transaction (step 2).**
That is the correct shard (the pointer co-routes there) and the correct moment
(the paste/site row becomes visible exactly when the blob does). The reserve
(step 1, `{id}` quota CAS) stays a separate prior transaction; the confirm
(step 3, derived index) stays a separate later transaction. So:

```
reserveBytes({id})            // unchanged: strict quota CAS, BEFORE staging or in parallel
StageBlob(slug, body)         // slow bytes -> final object (reader-invisible)
Transact(slug):               // step 2, now a *BlobTx:
    tx.Put(pastes/<slug>, meta)
    tx.BindBlob(ref)          // <-- the blob becomes referenced HERE, atomically with the row
deferredConfirmInsert({id})   // unchanged: derived index + first-seen
```

This means `insertAuthoritative` (the step-2 body in `shale_repo.go`) becomes
the place the bind lands. The seam's `Commit` on the shale path therefore does
NOT own the whole reservation sequence; it owns the step-2 transaction and takes
the metadata-write closure plus the staged handle. The reserve/confirm stay
inside `InsertWithQuotaCheck` exactly as now. (Concretely: `ShaleRepo` grows a
variant of `insertAuthoritative` that takes a `BlobRef` and adds `BindBlob` to
the same `cluster.Transact` closure; `InsertWithQuotaCheck` calls it when a ref
is present.)

A crash between reserve and the step-2 bind leaves: a reserved-but-uncommitted
`{id}` over-count (the reconciler releases it, unchanged) AND a staged-but-
unbound blob object (shale's `SweepOrphans` age-gates and reclaims it). Both are
already-handled leak-only states. No new failure mode.

The domain stays pure: `BlobUnit`/`BlobHandle` live in `service`, the
implementations in `storage`. The domain types (`Paste`, `Site`, `Manifest`)
never reference a blob handle.

## 3. Wiring: constructing the *BlobKV and adding the bref/ ShardKeyFn case

### 3.1 Cluster construction (cmd/hostthisd/metadata_shale.go + storage.ShaleRepo)

`NewShaleRepo` (`shale_repo.go`) today calls `cluster.Open(clusterCfg)` and
stores the `*cluster.Cluster`. Phase 3:

1. Build a `blob.Store` adapter. Reuse shale's MinIO adapter:
   `github.com/Zamua/shale/backends/slate/blobstore.New(blobstore.Config{...})`
   pointed at the SAME MinIO endpoint/creds the metadata uses, a DISTINCT blob
   bucket (`HOSTTHIS_SHALE_BLOB_BUCKET`, e.g. `hostthis-blobs`). This is the
   `*blobstore.MinioBlobStore`, which satisfies `blob.Store`.
2. Set `clusterCfg.BlobStore = thatStore`.
3. Call `cluster.NewBlobKV(clusterCfg)` instead of `cluster.Open(clusterCfg)`.
   `NewBlobKV` requires `cfg.BlobStore != nil` (compile/runtime gate) and
   returns a `*cluster.BlobKV` whose embedded `*KV` wraps the cluster.
4. `ShaleRepo.cluster` becomes `ShaleRepo.kv *cluster.BlobKV`. Every existing
   `r.cluster.Get/Put/Delete/Transact/ScanPrefix/...` call routes through the
   embedded `*KV` (which exposes Get/Put/Delete/Close) OR the underlying
   `*Cluster` via `kv.Cluster()` for the cluster-only methods the wrappers do
   not re-expose (`ScanPrefix`, `LocalScanPrefix`, `Aggregate`, `MountedUnits`,
   membership, the rpc server registration). `kv.Cluster()` is the documented
   escape hatch (kv.go:60).

   Concretely the existing `r.cluster.ScanPrefix(...)`, `r.cluster.Aggregate(...)`,
   `r.cluster.LocalScanPrefix(...)`, and `rpc.NewServer(cl)` calls become
   `r.kv.Cluster().ScanPrefix(...)` etc.; the `Get`/`Transact` calls that the
   `*KV`/`*BlobKV` wrappers DO re-expose stay as `r.kv.Get(...)`. NB the existing
   transaction body uses `cluster.Transact(pinKey, func(tx backend.Transaction))`
   directly (the low-level form); the reservation-pattern CAS closures keep using
   `r.kv.Cluster().Transact(...)` (the raw `backend.Transaction`), and ONLY the
   step-2 authoritative-write path that needs a `BindBlob` switches to
   `r.kv.Transact(slug, func(tx *cluster.BlobTx))`.

5. `ShaleRepo` exposes the blob lifecycle to the service via a `ShaleBlobUnit`
   built over `r.kv`. The cmd wiring constructs `ShaleBlobUnit{kv: repo.kv}` and
   passes it to the services as the `service.BlobUnit` (section 2.1).

### 3.2 The bref/ ShardKeyFn case (the one required change to shaleShardKey)

`shaleShardKey` (`shale_shardkey.go`) is hostthis's custom `ShardKeyFn`. Phase 2
REQUIRES (blob-values.md 11.5): an app with a custom ShardKeyFn must honor hash
tags for `bref/` keys, so the pointer co-routes with the metadata.

The bref key is `bref/{<routeShard>}/<unit>/<blobid>` where `<routeShard> =
shaleShardKey(routeKey)`. For a paste, `routeKey = pastes/<slug>` so
`<routeShard> = <slug>`; the bref key is `bref/{<slug>}/<unit>/<blobid>`. We
need `shaleShardKey(brefKey) == <slug>` so the pointer routes to the same shard
as `pastes/<slug>`.

Add ONE leading case to `shaleShardKey`:

```go
func shaleShardKey(key []byte) []byte {
    // Blob-pointer keys (shale-internal): the route shard key lives in the
    // Redis-style hash tag {<routeShard>}, exactly as the metadata key shards.
    // Defer to the default hash-tag extractor so the pointer co-routes with the
    // app metadata under the same unit. (shale's brefKey writes the tag; we
    // only have to read it. blob-values.md section 11.5.)
    if bytes.HasPrefix(key, prefixBref) {
        return ring.ShardKey(key)
    }
    // ... existing pastes/ versions/ ... cases unchanged ...
}

var prefixBref = []byte("bref/")
```

`ring.ShardKey` (`github.com/Zamua/shale/pkg/ring`) is exported and importable;
it returns the bytes between the first `{` and `}` (the route shard). For
`bref/{<slug>}/...` that is `<slug>`, which hashes into the same unit
`pastes/<slug>` does. Co-routed by construction.

**Build note:** `shaleShardKey` is currently in an UNTAGGED, import-free file
(`shale_shardkey.go`) so it is unit-testable on the default build. Importing
`pkg/ring` adds a shale dependency to that file. `pkg/ring` is a pure, light
core package (the consistent-hash ring helper, no cgo, no slatedb), so importing
it does NOT pull in the slatedb/cgo build constraint. The file can stay untagged
(it still builds with plain `go build`); only the value of having it import-free
is lost. Acceptable - the alternative (re-implementing the 20-line hash-tag
extractor locally to avoid the import) duplicates shale's contract and risks
drift if `ring.ShardKey`'s tag semantics ever change. Import `pkg/ring`.

## 4. Operation mapping (the create/read/delete/version/site flows)

Throughout, the route key is the metadata key (`pastes/<slug>` for a paste,
`sites/<slug>` for a site); the seam takes the slug and builds the route key.

### 4.1 Upload (paste create) - and the pending-status decision

Today (`upload.go`): stream stdin -> `streamUpload` tees sha256 + zstd + count
into an in-memory magic-prefixed staged body; write the paste row as PENDING
(synchronous, quota-reserved); hand back the URL; a background finalizer
`PutPrecompressed`s the body and flips PENDING -> READY (or FAILED).

The pending/finalizer model exists for ONE reason (SPEC "Paste lifecycle
status"): the blob write was the ~250ms-to-2s bottleneck, so it was moved off
the ack path - the row commits pending, the URL returns immediately, the bytes
flush in the background.

**Phase 3 shale flow:**

```
staged := streamUpload(stdin)                 // unchanged: sha256 + zstd + caps, in-memory body
reserveBytes({id}, staged.CompressedSize)     // unchanged: strict quota CAS
handle := blobUnit.Stage(slug, sha, staged.Body)  // StageBlob -> final object (reader-invisible)
blobUnit.Commit(slug, func(tx) {              // step-2 *BlobTx transaction:
    tx.Put(pastes/<slug>, meta=READY)         //   metadata committed as READY (not pending)
    tx.BindBlob(handle)                        //   blob bound atomically with the row
})
deferredConfirmInsert({id})                   // unchanged: derived index
```

**Pending-status decision: COLLAPSE the pending/finalizer model on the shale
path. Commit the paste as READY directly.**

Justification, grounded in the code:

- The pending model's job was to hide the slow blob write behind a fast-acked
  pending row. With `*BlobKV`, the blob is DURABLE (StageBlob completed) BEFORE
  the metadata commit. There is no window where the row exists but the bytes do
  not - the bind makes them visible together. A reader never sees a row without
  its blob, so there is nothing to show a loading page FOR.
- The async finalizer's failure modes - a pod crash losing the in-memory body, a
  blob write failing after the row committed pending - both vanish: if StageBlob
  fails, we never commit the row (return the error to the SSH client, release
  the reservation); if it succeeds, the bind+row co-commit or neither lands.
- This DELETES: `domain.PasteStatusPending`/`Failed` handling on the shale read
  path, `MarkReady`/`MarkFailed`, the `startFinalize`/`finalize` goroutine, the
  `finalizeWG`, the reconciler's pending-age-out, the loading + failed pages for
  shale-backed pastes. Big simplification, and it is the model blob-values.md
  section 3 describes ("commit a FAST transaction that writes {metadata + the
  blob pointer} together").

**Cost / open consideration (flagged, not blocking):** StageBlob is now ON the
ack path. The SSH client waits for the full blob upload (the ~2s prod blob PUT,
per the team's measured async-blob justification) before the URL returns. The
pending model traded that latency for a loading page. Two ways to keep the fast
ack IF the latency proves unacceptable:

  1. Keep StageBlob synchronous but COMMIT pending first, bind later: commit a
     pending row WITHOUT a bound blob, StageBlob+bind in the background, flip to
     ready. This re-introduces the finalizer but keeps the transactional bind.
     It loses the "row and blob commit together" simplicity (a pending row has
     no blob), so the read path keeps the loading page. This is the FALLBACK,
     not the default.
  2. Accept the synchronous latency. Pastes are a human ssh-pipe action; a 1-2s
     wait for the URL is tolerable and the simplification is large.

**Recommendation: ship the collapsed READY-direct model (default). Keep the
pending machinery alive on the standalone (sqlite/slatedb) path** (it is correct
there and costs nothing to leave), and keep option 1 documented as the escape
hatch behind a flag (mirroring the existing `HOSTTHIS_BLOB_SYNC` benchmark
toggle, which becomes moot on the shale path - the shale path IS sync-by-design,
so `SyncBlob` is ignored / removed for shale). Decide for real after measuring
the synchronous StageBlob latency on staging against the loading-page UX.

The standalone (sqlite/slatedb) path KEEPS the pending/finalizer model unchanged
- its `Stage` is a detached `Put` with no transaction, so the metadata-first
ordering still wants the pending row.

### 4.2 Read (paste serve)

Today (`http/server.go`): `Pastes.Get(slug)` -> the paste row carries
`ContentSHA`; `Blobs.GetReader(slug, sha)` streams the (decompressed) bytes.

Phase 3 shale: `Get(slug)` -> the paste row (unchanged: it is a routed metadata
read). The row carries the blobid (see "What the metadata stores" below). Then
`blobUnit.Read(slug, sha)` -> on the shale path `GetBlob(routeKey=pastes/<slug>,
blobid)` returns the COMPRESSED stream (the staged object is the magic+zstd
body); the read path decompresses with the existing `CompressedBlobStore` /
`zstdReadCloser` decode logic. So the decompression layer MOVES from "inside the
blob store" to "inside the seam's Read on the shale path" (or the http layer
decompresses, same as today's `GetReader` which already returns decompressed
bytes via the compression wrapper). Keep `Read`'s contract identical to today's
`GetReader`: it returns DECOMPRESSED bytes + a Close. The shale `Read` wraps
`GetBlob`'s stream in the streaming zstd decoder (the magic-header peek +
`zstd.NewReader` logic from `blob_compressed.go`, reused).

`ctx` lifetime: `GetBlob`'s reader streams lazily and `ctx` MUST outlive the
reader (kv.go:249 LIFETIME note). The http serve path must scope the request
ctx to the whole `io.Copy(w, rc)`, not just "resolve the blob". The existing
`ctxReadCloser` pattern (`blob_s3.go`) is the model; the shale `Read` returns a
ReadCloser whose Close cancels the GetBlob ctx.

**What the metadata stores.** Today the paste/version row carries `ContentSHA`,
and the blob is keyed `(slug, sha)`. With `*BlobKV`, a blob is identified by a
shale `blobid` (the `BlobRef.BlobID` minted by `StageBlob`), NOT by the app's
sha. So the row must additionally carry the `blobid` (a new field on
`pasteRow`/`versionRow`/the manifest file entry). `ContentSHA` stays (it is the
ETag, the content-addressing for within-record dedup, the integrity hash carried
into `Pointer.ContentHash`); `blobid` is what `GetBlob` needs. `Read(slug, sha)`
on the seam therefore becomes `Read(slug, blobid)` - the service reads the row,
pulls the blobid, and calls the seam. (The standalone path ignores blobid and
keys by sha, or stores blobid == sha; simplest is the seam takes BOTH and each
impl uses what it needs.)

### 4.3 Delete / expire

Today: metadata delete FIRST, then `RemoveOwner(slug)` of the whole prefix; a
crash between them leaves an orphan the reconcile reclaims.

Phase 3 shale: the metadata delete and the unbind co-commit.

```
Transact(slug):                    // the existing {slug} authoritative delete tx, now *BlobTx
    tx.Delete(pastes/<slug>)        // + versions, slug_owner, expiry index (existing)
    tx.UnbindBlob(ref)              // for the served blob; or tx.Delete(brefKey) per blobid
```

The `{id}` counter decrement (`decrementBytes`) stays its own `{id}` transaction
(it always was). The bytes go unreferenced at the unbind commit and
`SweepOrphans` reclaims them (age-gated). No `RemoveOwner`, no per-prefix walk.

**Expiry (TTL) sweep stays hostthis's concern** but co-deletes the bref. The
sweep's expiry pass (`service/sweep.go` `Once`) already deletes the metadata via
`Repo.Delete(slug)`; on the shale path `ShaleRepo.Delete` now ALSO unbinds the
blobs in the same transaction (it knows the row's blobids). So `Sweep` no longer
calls `Blobs.RemoveOwner(slug)` on the shale path - the unbind is folded into
`Repo.Delete`. The `SweepBlobs.RemoveOwner` call is removed from the shale wiring.

**Orphan-bytes reclamation is shale's `SweepOrphans`, scheduled by hostthis.**
The crash-orphan reconcile (`reconcileOrphans` in `sweep.go`, `WalkOwners` +
`SlugExists`) is DELETED on the shale path. In its place hostthis schedules
`BlobKV.SweepOrphans(ctx, now, grace)` on a cadence (the same `Sweep.Run` loop,
or a dedicated ticker). Phase 2 ships `SweepOrphans` as a single callable pass;
phase 3 owns the loop (blob-values.md 11.10 "phase 2 defers"). Each node sweeps
its OWN mounted units (mounted-unit-local, no cross-node coordination), so it
slots into the existing per-node sweep model. The `grace` is a generous window
(shale default ~1h) so a just-staged-not-yet-bound blob is never swept.

**The slug-scoped cross-store reconcile + the site reservation are DELETED** -
they were the two-store coordination. See section 5.

### 4.4 Versions + sites

- **Versions** (`manage.go` `Update`, `DeleteVersion`): each version is its own
  blob. Today `Update` `PutPrecompressed(slug, sha, body)` then
  `AppendVersionWithQuotaCheck`. Phase 3 shale: `Stage(slug, sha, body)` ->
  handle; the `AppendVersion` authoritative `{slug}` transaction binds the
  handle (`versionRow` carries the new blobid). `DeleteVersion`'s tombstone +
  within-record reclaim: instead of `shaStillReferenced` + `Remove(slug, sha)`,
  the tombstone transaction `UnbindBlob`s the version's blob IF no live version
  of the same paste still references that blobid (the existing same-paste,
  same-shard reference check stays - it is local to the slug's shard). Each
  version's blob is its own `blobid` bound under the slug's shard.

- **Sites** (`deploy_site.go`): each file is its own blob, bound under the
  site's slug shard. Today the deploy RESERVES the slug (an empty-manifest row)
  BEFORE the untar so the metadata exists before any blob (the crash-orphan
  reconcile safety). Phase 3 shale: NO reservation. The flow becomes:

  ```
  for each file in safe-untar(archive):
      handle[i] := Stage(slug, fileSha, fileBody)   // StageBlob each file (reader-invisible)
  Transact(slug):                                    // ONE authoritative site write:
      tx.Put(sites/<slug>, manifest)                 //   the real manifest, first time
      for each handle: tx.BindBlob(handle)           //   every file bound atomically
  ```

  The files are staged (reader-invisible) during the untar; the single site
  transaction binds them all WITH the manifest. There is no window where the
  manifest exists without its files, so no reservation is needed to "protect"
  in-flight blobs from the reconcile (there is no reconcile). A crash mid-untar
  leaves staged-but-unbound objects that `SweepOrphans` reclaims. A slug
  collision is resolved by the authoritative `Transact`'s existence read-check
  (the existing collision retry), now WITHOUT the pre-untar reservation - but
  note the collision is only detectable at commit, AFTER the untar consumed the
  stream. **Open question (section 7): collision handling without the
  pre-untar reservation.** The reservation today also resolved the collision
  cheaply before the stream was consumed; removing it means a (vanishingly rare)
  collision is detected only at commit, after the untar. Options: keep a
  metadata-only `slug_owner/<slug>` reservation read-check before the untar (NOT
  a blob reservation - just a cheap metadata existence claim, which costs
  nothing and is not a two-store coordination), OR accept re-untar on collision
  (32^8 slugs makes it effectively never). Recommend the cheap metadata-only
  slug claim: it keeps collision-cheap without re-introducing the blob
  reservation. Redeploy (`DeployToSlug`) drops files via UnbindBlob of the
  shas the new manifest no longer references (the existing `reclaimDroppedFiles`
  set-difference, now unbind-in-transaction).

### 4.5 Within-record dedup

Today: identical `(slug, sha)` Put is idempotent (same object). With `*BlobKV`,
StageBlob mints a fresh random blobid each time, so naive staging would store
duplicate bytes for an unchanged file on redeploy / a paste reverting to old
content. blob-values.md section 6.2 + 11.10 leave content-keyed dedup
(`StageBlob` skipped when `Has(content-keyed objkey)`) to a follow-up. Phase-2
`StageBlob` is NewBlobID-keyed only.

**Decision: phase 3 keeps the existing same-record reference check at the
METADATA layer** (DeleteVersion's `shaStillReferenced`, redeploy's
`reclaimDroppedFiles` set-difference) and accepts that an unchanged file
re-staged on redeploy gets a NEW blobid + a new object (the old object is
unbound + swept). This is correct (no leak, the sweep reclaims the orphan) but
re-uploads unchanged bytes. True within-record byte dedup (skip StageBlob when
the content sha is already bound for this record) is a phase-3.1 follow-up that
needs the app to key the blob by content sha and shale's `Has`. Flag, don't
build, in phase 3. (The metadata-layer ref check still prevents unbinding a
blobid a live sibling version shares.)

## 5. What's deleted from hostthis (shale path)

Specific files / methods. Some are deleted outright (shale-only constructs);
some have their shale BRANCH removed but stay for sqlite/slatedb.

DELETED OUTRIGHT (were the two-store coordination; the shale path no longer has
two stores):

- The crash-orphan reconcile, shale branch:
  `service/sweep.go` `reconcileOrphans` + `recordExists` are removed from the
  shale wiring; `SweepBlobs.WalkOwners` is no longer called on shale.
  (`reconcileOrphans` stays for sqlite/slatedb.)
- `S3BlobStore.WalkOwners` (`blob_s3.go`) - the reconcile's enumerator - is
  unused on the shale path (kept for the standalone S3 path if that survives;
  see below).
- The site reservation: `deploy_site.go` `reserveSlug` + `cleanupReservation`
  are removed from the shale path (replaced by the single bind-all transaction,
  section 4.4). The `SiteRepo` reservation methods used only by it on shale
  (the empty-manifest `InsertWithQuotaCheck` reservation call) drop their shale
  use.
- `domain.PasteStatusPending` / `PasteStatusFailed` handling, shale path:
  `upload.go` `startFinalize`/`finalize`/`finalizeWG`/`WaitFinalize`,
  `PasteRepo.MarkReady`/`MarkFailed`, the reconciler's pending-age-out
  (`PendingPasteTimeout`), and `http/server.go`'s `servePending`/`serveFailed`
  branches are bypassed on the shale path (the row commits READY). They stay for
  the standalone path. (If sqlite/slatedb also drop pending later, these go
  entirely - out of phase-3 scope.)

DELETED for hostthis as a whole IF the standalone S3 path is also retired (an
operator decision - prod is shale; dev uses disk):

- The detached `S3BlobStore` direct client (`blob_s3.go`) and its slug-scoped
  `s3Key`, `RemoveOwner`, `Remove`, `WalkOwners`, `WalkBlobs`. Prod no longer
  uses a detached S3 blob store (the blob bytes are shale-managed objects via the
  MinIO `blob.Store`). If dev keeps the disk standalone path, `blob_s3.go` can be
  deleted; if dev wants S3-standalone too, it stays. **Recommend deleting the
  S3-standalone path** (prod is the only S3 user and prod moves to `*BlobKV`),
  leaving disk for dev. That removes `blob_s3.go`, the `HOSTTHIS_BLOB_BACKEND=s3`
  branch in `cmd/hostthisd/main.go` `buildBlobStore`, the `s3` migration tooling
  references in `CLAUDE.md`, and the `minio-go` dependency IF nothing else uses
  it (the shale MinIO `blob.Store` lives in shale's backend module, not
  hostthis's, so hostthis may drop `minio-go` entirely - verify no other import).
- The slug-scoped blob keying as the PRIMARY model: `blobs/<slug>/<sha>` is
  replaced by shale's `blob/<unit>/<blobid>` + `bref/{<slug>}/<unit>/<blobid>`.
  The disk standalone path keeps `<root>/<slug>/<sha>` for dev.

NOT deleted (carries forward): `streamUpload` (the tee pipeline - still produces
the magic+zstd staged body), the `CompressedBlobStore` encode/decode logic
(reused: the shale `Read` decompresses the staged object the same way; the
standalone path still wraps with it), `shaleShardKey` (gains the `bref/` case),
the reservation-pattern quota on `{id}` (unchanged - it is metadata quota, not
blob coordination), the `{slug}`/`{id}` shard families.

## 6. Dependency / build wiring (go.mod)

`hostthis/go.mod` pins shale `v0.7.1-0.20260619005503-a07654c3c18d` (commit
`a07654c`) for the core module and `v0.7.2-...a07654c` for `backends/slate`.
The `*BlobKV` API is on `feat/blob-values` (commit `886690a`), UNRELEASED. So:

1. **Local co-development replace -> the shale-optionb worktree.** hostthis
   already uses a replace block for slatedb co-development (go.mod line 24+:
   "Local co-development replaces so the slatedb-tagged ShaleRepo builds against
   the working tree of shale on disk"). Phase 3 points those replaces at the
   `feat/blob-values` worktree:

   ```
   replace github.com/Zamua/shale => /abs/path/to/shale-optionb
   replace github.com/Zamua/shale/backends/slate => /abs/path/to/shale-optionb/backends/slate
   ```

   (the worktree on disk, at the prod lineage path noted in MEMORY - shale
   prod runs `feat/overlap-handoff`; phase 3's blob work is on
   `feat/blob-values`, which must MERGE INTO the prod lineage before a tagged
   build). The existing genproto pin replace stays. Both the core module AND the
   slate backend module must be redirected (Go only applies the main module's
   replaces, per the existing go.mod comment), PLUS now potentially the blob
   adapter lives in `backends/slate/blobstore` - same module as `backends/slate`,
   so the existing slate replace covers it (no third replace needed; `blobstore`
   is a package IN the slate backend module).

2. **The tagged build / shale #431 interaction.** The release build drops the
   replaces and pins a published pseudo-version (go.mod comment). For phase 3 to
   ship a release image, `feat/blob-values` must be merged into the shale prod
   lineage and a new core+slate pair tagged; hostthis then bumps the pins to
   that tag and drops the worktree replaces. Until then, hostthis builds the
   shale-blob path ONLY via the local replace (the `Dockerfile.slatedb.local`
   image, repo-root context + `go.work`, per shale's CLAUDE.md - the only way to
   ship unreleased core changes). The staging-gate (dev -> Hetzner staging ->
   prod) validates the local-replace image before the tag. #431 (the tagged-build
   coordination tracked in shale) must close first.

3. **Tests use the blobmem fake, no MinIO.** `github.com/Zamua/shale/pkg/blob/blobmem`
   is the exported in-memory `blob.Store` (kv.go 11.12: "exported, not internal,
   so an app's tests (hostthis, phase 3) can use it"). hostthis's
   `ShaleBlobUnit` tests construct a `*cluster.BlobKV` with
   `Config.BlobStore = blobmem.New()` and an in-memory/temp slate backend (the
   existing shale-test harness pattern), so the upload/read/delete/site flows are
   exercised end-to-end with NO MinIO and NO cgo for the blob plane (blobmem is
   pure Go; the metadata backend still needs `-tags slatedb` + the dylib for the
   slate path - the existing constraint). The fake's settable per-object ModTime
   drives the `SweepOrphans` age-gate deterministically in tests.

## 7. Migration shape (feeds phase 4, detail deferred)

Existing prod blobs live in the detached `hostthis-blobs` MinIO bucket, keyed
`<slug>/<sha>` (slug-scoped) - the magic+zstd bodies. The new model stores the
SAME bytes as shale-managed objects `blob/<unit>/<blobid>` with a pointer
`bref/{<slug>}/<unit>/<blobid>` in slatedb, co-committed with each
`pastes/<slug>` / `versions/<slug>/<NNNN>` / `sites/<slug>` row.

So the one-time migration must, per existing record (paste version / site file):

1. Read the metadata row + its `ContentSHA`, fetch `<slug>/<sha>` bytes from the
   old bucket.
2. Compute the routed `unit = RoutedUnitToken(pastes/<slug>)`, mint a `blobid`,
   PUT the bytes at `blob/<unit>/<blobid>` in the new blob bucket.
3. In a `Transact(slug)`: write the row WITH the new `blobid` field +
   `BindBlob(ref)`.
4. After verification, the old bucket is decommissioned.

This is a re-key + re-bind under a brief write-freeze (blob-values.md section 9
phase 4). It lives in `infra/tools/` (an operator/migration tool talking to the
real cluster, NOT in the hostthis repo per the operator-code separation in
CLAUDE.md), reusing shale's `RoutedUnitToken` + the `*BlobKV` bind path. Detail
(freeze window, idempotency, batch size, the verify pass) is phase 4. Shape only
here.

## 8. Phased implementation plan (within phase 3)

Spec-first per step (hostthis CLAUDE.md). Each step is its own change + tests.

1. **The service seam (no behavior change).** Introduce `service.BlobUnit` +
   `BlobHandle`. Implement `StandaloneBlobUnit` over the existing
   `CompressedBlobStore` + `BlobDeleter` + metadata repo (a pure refactor: the
   services call the seam, the seam calls today's code; sqlite/slatedb/disk
   behavior is byte-identical). Characterization tests pin the current
   upload/read/delete/version/site behavior through the seam. GREEN before any
   shale-blob code exists. (DDD: seam in `service`, impls in `storage`, domain
   untouched.)

2. **The shale blob path.** Wire `Config.BlobStore` (MinIO `blob.Store`) +
   `NewBlobKV` in `NewShaleRepo`; add the `bref/` case to `shaleShardKey`;
   implement `ShaleBlobUnit` (`StageBlob`/`Transact`+`BindBlob`/`GetBlob`/
   `UnbindBlob`); add the `blobid` field to the row schemas; route the step-2
   authoritative writes through `*BlobTx`. Tests against a `blobmem`-backed
   `*BlobKV` (no MinIO): reader-atomic create (staged-not-bound -> read 404;
   post-commit -> serves), atomic delete (unbind + metadata delete one tx),
   crash-injection (stage then no commit -> SweepOrphans reclaims after grace,
   not before), the `bref/` co-routing (the pointer lands on the metadata's
   shard), versions + sites bind-all, redeploy dropped-file unbind. The
   pending-collapse (READY-direct) is part of this step for the shale path;
   demonstrate the loading-page path is gone on shale and present on standalone.

3. **Drop the old store (shale path) + schedule SweepOrphans.** Remove the
   reconcile / site-reservation / pending-finalizer from the shale wiring; fold
   the unbind into `ShaleRepo.Delete`; schedule `BlobKV.SweepOrphans` in the
   sweep loop (per-node, mounted-unit-local). Decide + execute the
   S3-standalone retirement (recommend: delete `blob_s3.go` + the `s3` backend
   branch + `minio-go` dep if unused). Tests: the shale sweep loop reclaims a
   stage-without-bind orphan; the shale delete co-removes the bref; no
   `WalkOwners`/`RemoveOwner` on shale.

4. **Integration + e2e + staging.** Multi-node shale integration test (stage on
   one node, pointer routes to another, read from a third - proving the byte
   plane is off the RPC path and co-routing holds); a streaming peak-memory
   assertion (a large paste/site never buffers the whole blob - bounded by the
   staging buffer, the StageBlob stream, and the GetBlob decode); the full
   ssh-upload -> http-read -> delete e2e on the shale backend. Then the staging
   gate: build the `Dockerfile.slatedb.local` image with the worktree replace,
   deploy to the Hetzner staging cluster, verify, THEN prod (per the staging-gate
   memory). The phase-4 migration is a SEPARATE change (in `infra/tools/`).

## 9. Open questions (could not resolve from the code)

1. **Site-deploy slug collision without the pre-untar reservation (section
   4.4).** Removing the blob reservation means a collision is detected only at
   the authoritative `Transact`, AFTER the untar consumed the (one-shot) stream.
   Recommend a cheap metadata-only `slug_owner/<slug>` existence claim before the
   untar (NOT a blob reservation - it is single-shard metadata, no two-store
   coordination), so a collision is still resolved cheaply pre-stream. Needs
   confirming this does not re-introduce the in-flight-blob-protection coupling
   the reservation had (it should not: there is no reconcile to protect from
   anymore). Pin with a test.

2. **Synchronous StageBlob latency vs the loading-page UX (section 4.1).** The
   collapsed READY-direct model puts the ~2s prod blob PUT on the ack path. Is
   the ssh-pipe latency acceptable, or is the pending fallback (option 1) needed?
   Cannot decide from code; needs a staging latency measurement against the
   loading-page UX. Recommend shipping READY-direct and measuring.

3. **Whether the standalone S3 path is retired (section 5).** Prod moves to
   `*BlobKV`; dev can use disk. Deleting `blob_s3.go` + the `s3` backend removes
   `minio-go` from hostthis (cleaner) but forecloses S3-standalone dev. Operator
   call; recommend retire-S3-standalone, keep-disk-dev.

4. **`blobid` placement in the row schema.** The version/manifest entries carry
   `ContentSHA` today; adding `blobid` is a row-schema change (a new JSON field,
   forward-compatible since JSON tolerates unknown fields, but the read path must
   handle a row with no blobid during migration - it falls back to the old
   `(slug, sha)` keying until re-bound). The exact dual-read window is a
   phase-4/migration concern; flagged so the schema change in step 2 reserves the
   field.

5. **Does hostthis still import `minio-go` after the S3-standalone retirement?**
   The shale MinIO `blob.Store` adapter lives in shale's `backends/slate/blobstore`
   module, so hostthis does NOT import `minio-go` for the blob plane. Verify no
   OTHER hostthis code imports it (the migration tool that does lives in
   `infra/`, not the repo). If clean, drop `github.com/minio/minio-go/v7` from
   `go.mod`. Cannot fully verify without the retire-S3 decision (Q3).
