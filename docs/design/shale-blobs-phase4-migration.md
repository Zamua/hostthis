# hostthis shale-collocated blobs - phase 4: the one-time PROD blob migration

Status: design / no code. The contract to poke holes in before building the
phase-4 migrator.

This is PHASE 4 of the shale streaming-blob effort. Phase 3 (branch
`feat/shale-blobs`) built the transactional shale-collocated blob plane: the
`*cluster.BlobKV` wiring in `ShaleRepo`, the `bref/` `ShardKeyFn` case, the
`service.BlobUnit` seam + the `shaleblob.Unit` adapter, the `blobid` row-schema
field, and the READY-direct upload collapse. Phase 3 ships a blob-ENABLED
`hostthisd` that, for a FRESH paste, stages the bytes into the collocated bucket
and co-commits the pointer with the metadata.

Phase 4 is the data move: every EXISTING prod blob lives in the detached
`hostthis-blobs` MinIO bucket keyed at bare `<sha>` (229 objects today); the
metadata rows carry a `ContentSHA` but NO `blobid`. After the blob-enabled image
is deployed, the read path resolves a blob via `ShaleRepo.ResolveBlobID` ->
`BlobKV.GetBlob`, which needs a `blobid` on the row and a
`bref/{<slug>}/<unit>/<blobid>` pointer in slatedb. Neither exists for legacy
rows. **The migrator must, for every existing record, copy its bytes into the
collocated bucket under a freshly-minted `blobid`, set that `blobid` on the
metadata row, and write the `bref` pointer - all routed to the correct shard and
replicated R=2, so the new image can serve the existing pastes/sites after
cutover.**

This doc supersedes the phase-3 doc section 7 ("Migration shape") AND revises the
earlier phase-4 draft (which chose an in-process "Option C" inside hostthis and
proposed new app code). Two operator constraints reshape it:

1. **The migrator is a FULLY SEPARATE infra tool.** It is a standalone Go module
   under `infra/tools/hostthis-blob-migrate/`, importing ONLY shale's PUBLIC API
   (`pkg/cluster`, `pkg/blob`, `backends/slate`) + `minio-go`. It imports
   **nothing** from `hostthis/internal/*` (Go forbids the cross-module internal
   import anyway) and adds **zero** migration logic to the hostthis or shale
   codebases. This mirrors the EXISTING precedent: the legacy->sharded metadata
   migrator already lives at `infra/hostthis/migrate/main.go` and uses only
   `shale/pkg/{cluster,rpc}` + `shale/backends/slate`, never a hostthis import.
2. **The cutover is READ-DOWN.** "Take the service down -> run the migration ->
   deploy the new image." There is no zero-downtime requirement, so the prior
   draft's `HOSTTHIS_READ_ONLY` flag + the legacy dual-read fallback are
   DELETED - they only existed to keep reads up during a write-freeze. Read-down
   means ZERO new hostthis app code.

It grounds in:

- the phase-3 implementation on `feat/shale-blobs`
  (`internal/storage/shale_repo.go`, `internal/storage/shale_site_repo.go`,
  the shared row schema in `internal/storage/slate_repo.go` +
  `slate_site_repo.go`),
- the shale `feat/blob-values` `*BlobKV` surface
  (`pkg/cluster/kv.go`, `pkg/cluster/blob_units.go`),
- the shale replicated multi-backend write path
  (`pkg/cluster/multibackend_replicated.go`, `pkg/cluster/cas.go`,
  `pkg/cluster/apply_batch.go`, `backends/slate/factory.go`,
  `backends/slate/dbname.go`),
- the prod sharded deploy (`infra/hostthis/k8s/sharded/`, R=2 / UnitCount=16,
  3 pods),
- the existing operator migrator (`infra/hostthis/migrate/main.go`) + its Job
  (`infra/hostthis/k8s/sharded/30-migrate-job.yaml`).

---

## 1. The shape of the data, before and after

### 1.1 Old (current prod)

- **Blob bytes**: the detached `hostthis-blobs` MinIO bucket
  (`HOSTTHIS_S3_BUCKET`), keyed at **bare `<sha>`** (content-addressed, NO
  `<slug>/` prefix). Each object is a `magic + zstd(bytes)` body (`HZ\0\x01`
  prefix, `storage.magicV1`). A blob is shared across every record (paste /
  version / site file) that references the same `<sha>`: content-addressed
  dedup, ONE object per distinct sha. 229 objects today.
- **Metadata rows** (sharded shale cluster, R=2, UnitCount=16): `pastes/<slug>`,
  `versions/<slug>/<NNNN>`, `sites/<slug>`, each carrying `ContentSHA`. On
  `feat/shale-blobs` the shared row structs ALREADY carry `BlobID string
  json:"blob_id,omitempty"` (`slate_repo.go:180` paste, `:193` version) and the
  site row carries `FileBlobs map[string]string json:"file_blobs,omitempty"`
  (sha -> blob-id side-table, `slate_site_repo.go:116`). For legacy rows these
  are EMPTY (`omitempty` keeps them absent on the wire).

### 1.2 New (post-phase-4)

- **Blob bytes**: the collocated `HOSTTHIS_SHALE_BLOB_BUCKET`, keyed at
  `blob/<unit>/<blobid>` (shale-managed, `blob.FinalKey`). The body is the SAME
  `magic + zstd(...)` bytes, copied verbatim (no re-encode - `StageBlob` streams
  what it is given and the old object is already in the at-rest format).
- **Metadata rows**: the SAME rows, now with `BlobID` set (paste head + each
  version) / `FileBlobs[sha] = blobid` populated (site files), and a
  `bref/{<slug>}/<unit>/<blobid>` pointer co-located on the `{slug}` shard.

A record's `<sha>` is unchanged (it is the integrity hash + the ETag); only the
addressing of the BYTES changes (bare `<sha>` object -> `blob/<unit>/<blobid>`
object + the pointer + the row's `blobid`).

### 1.3 Per-record-blob, not per-sha (dedup granularity)

The old bucket has ONE object per distinct `<sha>` (global content dedup); the
new model keys by a minted `blobid` per `StageBlob` call. Phase 3 deliberately
does NOT do cross-record content dedup. The migrator stages the bytes ONCE PER
RECORD-BLOB (the paste head, each version, each site file), minting a distinct
blobid each time:

- **per-record-blob (CHOSEN)**: every object ends up referenced by EXACTLY ONE
  pointer on EXACTLY ONE shard, so the mounted-unit-local `SweepOrphans`
  referenced-set is always complete for every object. The storage cost of
  re-copying shared bytes is bounded by prod's actual sha-sharing (low; 229
  objects, most pastes unique).
- **per-sha (REJECTED)**: would preserve the old bucket's dedup but a shared
  sha's object is referenced by pointers on DIFFERENT units, which the
  mounted-unit-local `SweepOrphans` (`blob_units.go:92` MountedUnits +
  `kv.go:309` SweepOrphans) can mis-classify as orphan on a node that mounts one
  shard but not the other. That re-introduces the exact cross-shard-orphan-sweep
  hazard phase-3 spent effort avoiding.

Concretely: the migrator enumerates record-blobs `(slug, kind, verNum, sha)` -
the paste head, each version row, each site file - and for each, copies `<sha>`
bytes -> a fresh `blob/<unit>/<blobid>`, sets that row's blobid, binds the
pointer.

---

## 2. THE CRUX: how does a SEPARATE infra tool perform routed, R=2-replicated blobid+bref writes against quiescent prod buckets?

Two facts collide:

- **The bind is an in-process `*BlobKV.Transact`, NOT a gRPC RPC.** The blobid+bref
  write is a co-commit: the row's `blobid` Put and the `bref` Put ship in ONE
  routed single-shard transaction (`kv.go:144` `BlobKV.Transact` -> `BlobTx.Put`
  + `BlobTx.BindBlob`, `kv.go:174`). The gRPC server (`pkg/rpc/server.go`)
  exposes single-key `Put`/`Get`/`Delete` + the cluster-INTERNAL
  `CommitCAS`/`ApplyBatch` fan-out RPCs, but NO public "route this multi-key
  transaction" surface. So the legacy metadata migrator's trick - a plain
  `rpc.NewClient(addr).Put(key, value)` that routes + R=2-replicates server-side
  (`infra/hostthis/migrate/main.go:132`) - CANNOT express the bind. The bind is
  only reachable by a process that holds a `*cluster.BlobKV` in-process.

- **R=2 placement is the ISSUER's live ring, not "the replica I own".** A
  replicated write fans out to ALL R replicas of the key's unit, resolved from
  the issuing node's ring:
  `putReplicatedUnitAttempt` (`multibackend_replicated.go:535`) calls
  `routedReplicasWithUnit(key)` -> `unitReplicas(gu)` ->
  `ring.LocateKeyN(genUnitBytes(gu), R)` (`multibackend_replicated.go:72-77`),
  builds the R-member replica set, and dispatches to each member: local-self
  applies into its mounted unit, remote-self forwards via gRPC
  (`dispatchReplicaPutUnit`, `:572`). The CAS/bind path is identical:
  `CommitCASApply` commits owner-local then `replicateCASBatch` ->
  `replicateCASBatchAttempt` re-resolves `c.replicasForKey(pinKey)` and fans the
  write-set to the unit's R replicas (`apply_batch.go:201`, `cas.go:239`).
  **Crucially `unitReplicas` returns the local node only when the ring is
  nil/empty (`multibackend_replicated.go:73-74`).** So:

  > **A single standalone shale process - configured exactly like prod
  > (UnitCount=16, R=2, the ShardKeyFn, the bucket) but with a ONE-MEMBER ring -
  > does NOT produce R=2-consistent writes. Its ring is itself, so
  > `LocateKeyN(unit, 2)` clamps to one member, and every blobid+bref lands on
  > exactly ONE replica position. When the real 3-node R=2 cluster restarts, each
  > migrated unit has one fresh replica and one STALE replica (the position the
  > single node never wrote).** This is crux-(a): a single node at R=2 writes
  > only the replica(s) its own ring places, leaving the other replica
  > un-updated. (Worse: a single node desires EVERY position - it would mount and
  > write whichever replica LocateKeyN ranks first for each unit, a different
  > position per unit, so the result is not even a clean "all replica-0" set.)

  Does the restarted cluster self-heal the stale replica? **No.** shale's R>1
  multi-backend topology is STATIC per generation (no backfill/anti-entropy that
  copies a whole unit between replica positions); the only convergence is
  per-key read-repair (`getReplicatedUnit` -> `scheduleReadRepairUnit`,
  `multibackend_replicated.go:736`), which fires ONLY when a quorum READ observes
  a disagreement and pushes the winner to the lagger. With R=2 and
  `WriteQuorum` (W=2/2... actually `requiredWriteAcks(WriteQuorum, 2) = 2/2+1 =
  2`, see `replicate.go:214`), but `ReadConsistency=ReadQuorum` reads
  `2/2+1 = 2` of 2. A read does eventually repair the stale replica IF the read
  reaches both. But relying on lazy read-repair to back-fill an entire migrated
  corpus's second replica is NOT a migration guarantee: an un-read blob's second
  replica stays stale indefinitely, and a node loss before that key is ever read
  loses the only fresh replica. **Read-repair is not a substitute for writing
  both replicas at migrate time.** So a single-node migrator is rejected for
  correctness, not just tidiness.

### 2.1 The resolution (crux-(b)): a TRANSIENT N-node migrate cluster that IS the real R=2 topology over the quiescent buckets

**The migrator stands up the SAME 3-node R=2 topology the app runs, over the
SAME quiescent prod buckets, forms a real ring, and runs the blobid+bref re-key
in-process on ONE of those nodes. Because the ring has all 3 members, every
`Transact`+`BindBlob` fans out to BOTH replicas of the owning unit through the
cluster's own routing - exactly as a live app write would. The migrate cluster
then tears down, and the real app StatefulSet restarts on those buckets seeing
complete, R=2-consistent replicas.**

This is option (b)(i) from the crux ("run the migrator as the FULL N-node
topology"), chosen over (b)(ii) single-node-plus-self-heal (rejected above: shale
does not backfill a whole replica position; read-repair is lazy + per-key) and
over (b)(iii) join-the-live-cluster-as-a-transient-member (rejected: a 4th node
joining the 3-node ring triggers a lease-handoff rebalance on the 2 GB boxes -
there is NO observer/non-owning join in shale; any `BindAddr` node is an owner
candidate, `cluster.go` reconcile).

Concretely the migrate cluster is a **3-pod Kubernetes Job/StatefulSet** built
from the infra tool image, configured byte-identically to the prod app's shale
cluster:

- `UnitCount=16`, `ReplicationFactor=2`, `ShardKeyFn = the replicated
  shaleShardKey` (section 3.2), `KeyPrefix/DbName = hostthis-sharded` (the prod
  metadata key-prefix), the prod `hostthis-metadata` bucket + creds, the
  collocated `HOSTTHIS_SHALE_BLOB_BUCKET` as the `BlobStore`.
- 3 members, one-per-node (anti-affinity), founder + 2 joiners seeded off the
  founder's stable peer DNS - the SAME `BindAddr=$(POD_IP):7946` /
  `GRPCAddr=$(POD_IP):7947` / seed shape the prod
  `infra/hostthis/k8s/sharded/base/20-statefulsets.yaml` uses, but a SEPARATE
  set of pods (a `migrate-` name prefix) so it is its own ring.
- It opens each unit at its replica position via the slate `Handle`
  (`backends/slate/factory.go`), reading/writing the SAME `u/g<gen>/u<id>/r<n>`
  per-replica slatedb databases the app uses (`dbname.go`
  `dbNameReplicaFor`). The migrate cluster's nodes ARE the app's nodes as far as
  the buckets are concerned: same unit DBs, same replica prefixes, same fence
  epochs.

**Why this is safe with the app DOWN (read-down):** with the prod app
StatefulSet scaled to 0, no live writer holds any unit's slatedb open. The
migrate cluster opens each unit, fencing nothing live (the durable manifest
writer-epoch has no contender), does its re-key writes at R=2, flushes on
`CloseUnit` (the slate factory forces a durable flush before release,
`factory.go` `CloseUnit` / `Close`), and exits. When the real app StatefulSet
scales back up it opens each unit at a strictly-higher epoch (fenceEpoch =
max(intended, durable+1)) and recovers the durable tail the migrate cluster
flushed. No two writers ever hold a unit at once (the app is down throughout the
migrate window), so there is no split-brain, no fence race.

**The migrate cluster ONLY needs to be R=2-correct, not a long-lived app.** It
runs one batch loop on ONE of its nodes (the founder, by convention) and exits.
The other two nodes exist purely to be the ring members `LocateKeyN` needs so
the founder's writes replicate to the right two positions per unit. They serve no
requests; they hold their owned units open + apply the founder's forwarded
replica writes (`dispatchReplicaPutUnit` remote branch -> `PutAtReplica`), and
flush on shutdown.

### 2.2 What the migrator must replicate about the cluster's write semantics (crux-(c))

Beyond constructing the `*BlobKV` and calling `Transact`+`BindBlob`, the migrate
cluster must match prod's shale config so the writes are placed + stamped
identically and the restarted app reads them as its own:

- **Same `ShardKeyFn`.** Routing depends on it. hostthis opens the cluster with
  the custom `shaleShardKey` (`internal/storage/shale_shardkey.go`), which has a
  `bref/` case deferring to `ring.ShardKey` (the hash-tag extractor) plus the
  pastes/versions/sites/... family cases. The infra tool REPLICATES this ~30-line
  function (section 3.2). A WRONG shard key mis-routes every object + pointer.
- **Same `UnitCount=16` and `ReplicationFactor=2`.** `genUnitForKey` hashes into
  16 units; a different count re-keys everything. R drives the fan-out width.
- **Same `KeyPrefix`/`DbName=hostthis-sharded`.** The per-unit slatedb DBs live
  at `<DbName>u/g<gen>/u<id>/r<n>` (`dbname.go`); a different prefix opens a
  DIFFERENT, empty set of unit DBs and the app never sees the writes.
- **Same generation.** `RoutedUnitToken` + routing resolve the unit at the LIVE
  generation (`genUnitForKey`). The migrate cluster, opening the same buckets
  with the same UnitCount and NOT running any reshard, sees generation 0 (prod
  has never resharded), identical to the app. (If prod ever reshards before this
  migrate, re-derive: the migrate cluster reads the same `PresentUnits` the app
  does, so it converges to the same generation - but for the 229-object,
  never-resharded prod today, gen 0 is a given.)
- **The serving marker is NOT something the migrator writes for the bind.** The
  serving marker (`dbname.go:76` `servingMarkerKeyFor`, written by
  `acquireReplicaUnit`/the overlap acquire, `multibackend_replicated.go:424`) is
  the Option-B overlap-handoff RELEASE signal between a draining and a gaining
  owner. The migrate cluster's nodes DO write a serving marker when they mount
  each owned position (the clean-cut acquire writes it), which is harmless: it
  records the migrate node's open epoch. When the real app restarts it opens at a
  strictly-higher epoch and writes its own marker; markers are monotonic
  (`factory.go:316` `writeServingMarker` never lowers). So the migrate cluster's
  markers do not interfere with the app's later mounts - they are just stale
  lower-epoch records the app's higher-epoch open supersedes. **The bind itself
  needs NO marker/lease/fence beyond what `Transact`+`BindBlob` already does:** the
  co-commit is an ordinary CAS write-set, R=2-replicated by the cluster, with the
  shared LWW stamp `CommitCASApply` assigns (`cas.go:180`). The restarted app
  reads it via the same envelope-decode path, so it sees a normal committed row +
  pointer.
- **Durability.** The migrate cluster should open with the SAME durability mode
  the app uses or stricter. Prod runs `HOSTTHIS_METADATA_AWAIT_DURABLE=false`
  (relaxed durability, safe at R=2 because a peer holds the write through the
  flush window, `factory.go:104` `RelaxedReplicaDurability`). The migrate cluster
  runs at R=2 too, so relaxed durability is equally safe; BUT to be conservative
  for a one-time migrate the tool should set `AwaitDurable=true` (strict
  per-write object-store flush) so every blobid+bref is durable in the bucket
  before the loop advances - no reliance on a background flush that a fast
  shutdown might truncate. The slate `CloseUnit`/`Close` flush-before-release
  (`factory.go:691`/`:882`) backstops this regardless, but strict durability +
  the explicit flush is the belt-and-suspenders a non-reversible data move wants.

So crux-(c)'s answer: the migrator must replicate **the ShardKeyFn, UnitCount,
R, KeyPrefix, and (implicitly) the generation**; it must NOT hand-roll any
serving-marker/lease/fence for the bind (the cluster's own
`Transact`+`BindBlob`+R=2-fan-out is the entire write semantics, and the markers
its nodes write on mount are harmless monotonic records the app's later opens
supersede).

### 2.3 Bytes staging is direct-to-object-store, but the UNIT TOKEN comes from the live ring

`StageBlob` PUTs the bytes node->object-store directly (`kv.go:229`
`b.blobs.PutStream`) and mints the blobid; the object key `blob/<unit>/<blobid>`
needs the unit token, which `RoutedUnitToken(routeKey)` derives PURELY from the
routing state (`blob_units.go:64`). Because the migrate cluster is configured
identically (UnitCount/ShardKeyFn/gen), its `RoutedUnitToken` yields the SAME
token the app's `RoutedUnitToken` will - so the object lands under the unit the
app routes to, and the bref's hash tag `{<slug>}` co-routes the pointer to the
same unit. Staging through the cluster (not a bare MinIO PUT) is what guarantees
the token matches: do NOT compute the object key offline.

---

## 3. The infra tool

### 3.1 Module + dependency boundary

```
infra/tools/hostthis-blob-migrate/      (its OWN Go module, own go.mod)
  go.mod                  module github.com/Zamua/infra-tools/hostthis-blob-migrate
                          requires github.com/Zamua/shale + .../shale/backends/slate
                          + github.com/minio/minio-go/v7   (NO hostthis dep)
  main.go                 -tags slatedb; the migrate cluster bootstrap + the
                          enumerate->stage->rebind loop + the verify pass.
  main_stub.go            !slatedb stub (so a no-cgo build compiles + fails loudly),
                          mirroring infra/hostthis/migrate's pattern.
  rows.go                 the REPLICATED row structs (pasteRow/versionRow/siteRow
                          minimal projections, section 3.3) + the manifest decode.
  shardkey.go             the REPLICATED shaleShardKey (section 3.2).
  Dockerfile.slatedb.local  3-stage (Rust .so -> Go+cgo -tags slatedb -> distroless),
                          same shape as infra/hostthis/migrate/Dockerfile (copies
                          libslatedb_uniffi.so from the hostthis image).

infra/hostthis/k8s/sharded/
  35-blob-migrate-job.yaml   the 3-pod migrate cluster (founder + 2 joiners),
                             env from the hostthis-env Secret, args
                             [-dry-run | <copy> | -verify], the OLD bucket +
                             NEW collocated bucket wiring.
  BLOB-MIGRATION.md          the operator runbook (down->migrate->verify->deploy
                             ->verify->keep-old-bucket), staging validation,
                             rollback.
```

**It imports ONLY the shale public surface:** `cluster.NewBlobKV`,
`cluster.Config{ShardKeyFn, BlobStore, UnitCount, ReplicationFactor, NodeID,
BindAddr, GRPCAddr, Seeds, ...}`, `*BlobKV.StageBlob`, `*BlobKV.Transact(pinKey,
func(*BlobTx))`, `*BlobTx.Put` / `*BlobTx.Delete` / `*BlobTx.BindBlob`,
`*BlobKV.Get` (the routed metadata read), `*BlobKV.GetBlob` (verify),
`cluster.RoutedUnitToken`, `cluster.BlobRef`, `blob.Store` (the MinIO adapter -
the same `blobstore.MinioBlobStore` the app wires, which is in shale's PUBLIC
`pkg/blobstore`), and `slate.NewBacking` / `Handle` for the `BackendFactory`.
**Confirmed sufficient:** the bind is `Transact`+`BindBlob` (in-process, present
on the public `*BlobKV`), staging is `StageBlob` (public), routing is the
`Config.ShardKeyFn` the tool supplies, the row read is `BlobKV.Get(key)` +
JSON-decode into the tool's own struct. Nothing the tool needs is in
`hostthis/internal/*` or unexported in shale. (The one shale gap to confirm at
build time: `blobstore.MinioBlobStore` must be in an exported package; the app
wires it from the cmd binary, so it is reachable. If it is NOT exported, the tool
constructs its own tiny `blob.Store` over `minio-go` - a thin PutStream/GetStream/
List/Delete adapter, ~80 lines - since `blob.Store` is a public interface.)

### 3.2 The replicated ShardKeyFn

The tool replicates hostthis's `shaleShardKey` (`internal/storage/shale_shardkey.go`)
verbatim into `infra/tools/hostthis-blob-migrate/shardkey.go`. It is ~30 lines of
pure byte-slice prefix matching with ONE shale dependency, `pkg/ring.ShardKey`
(for the leading `bref/` hash-tag case), which IS public. The families the
migrate touches are `pastes/`, `versions/`, `sites/`, and (it writes) `bref/`;
the function must classify ALL families correctly because the cluster routes
every key through it, but only those four are exercised by the re-key. Replicating
it (rather than asking hostthis to export it) keeps hostthis import-free per
constraint 1.

**Drift risk + mitigation.** A replicated ShardKeyFn can drift from hostthis's if
hostthis ever changes its routing. Mitigation: the tool's `shardkey.go` carries a
header comment pinning it to the hostthis source path + commit, and the VERIFY
pass (section 5) reads every blob back THROUGH a freshly-opened migrate cluster
using the SAME replicated ShardKeyFn - so a self-consistent tool passes verify;
the real guard against drift-vs-hostthis is the staging run (section 6), where
the ACTUAL hostthis blob image reads the migrated rows. If hostthis's routing and
the tool's replica disagreed, staging reads would 404. (If the operator prefers,
hostthis could instead expose `shaleShardKey` as a tiny PUBLIC package
`hostthis/pkg/shaleroute` that both the app and the tool import - that eliminates
the drift entirely at the cost of one new public package in hostthis. This doc
defaults to REPLICATE per constraint 1's "or note if hostthis should expose it";
the operator decides, section 11.)

### 3.3 The replicated row projections

The tool decodes each metadata row into its OWN minimal struct (only the fields
the re-key needs), with json tags copied from the hostthis shared schema
(`slate_repo.go:175` / `:189`, `slate_site_repo.go:101`):

```go
type pasteRow struct {
    ContentSHA string `json:"content_sha"`
    BlobID     string `json:"blob_id,omitempty"`
    Kind       string `json:"kind"`   // (optional, for logging)
}
type versionRow struct {
    VerNum     int    `json:"ver_num"`
    ContentSHA string `json:"content_sha"`
    BlobID     string `json:"blob_id,omitempty"`
    Deleted    bool   `json:"deleted"`
}
type siteRow struct {
    Manifest  string            `json:"manifest"`   // path->{sha,size,ctype} JSON
    FileBlobs map[string]string `json:"file_blobs,omitempty"`
}
```

The site's `Manifest` is an opaque JSON string (`encodeManifest`'s output)
mapping each file path to its sha; the tool decodes it generically to enumerate
the distinct file shas, then targets `FileBlobs[sha]`. It does NOT need the full
hostthis manifest type - just "what shas does this site reference" + "set
`FileBlobs[sha]`".

**Reading + writing the row via the public surface.** The tool reads a row with
`kv.Get(key)` (e.g. `kv.Get([]byte("pastes/"+slug))`), which returns the
LWW-decoded PAYLOAD (the cluster strips the envelope on the read path), so the
tool JSON-unmarshals the payload directly into `pasteRow`. It writes the re-key
via `kv.Transact(routeKey, func(tx *BlobTx) { tx.Put(rowKey, updatedRowJSON);
tx.BindBlob(ref) })`. Because `Transact` pins on `routeKey` (`pastes/<slug>`, the
`{slug}` shard) and the bref hash tag is `{<slug>}`, the row Put and the bref Put
co-route to the SAME unit and commit in ONE single-shard CAS, R=2-replicated by
the cluster (`cas.go:97` `CommitCASApply` -> `replicateCASBatch`). This is the
identical guarantee a fresh hostthis paste gets. No `RebindLegacyBlob` method is
added to hostthis - the prior draft's new repo method is DELETED; the tool
expresses the rebind directly as `Put`+`BindBlob` over the public `*BlobTx`.

### 3.4 Enumeration

The tool enumerates record-blobs by scanning the metadata families. Since
`*BlobKV` (via the embedded `*KV` -> `*Cluster`) does not expose a cross-shard
`ScanPrefix` on the public wrapper, the tool uses
`kv.Cluster().Aggregate(...)` (`cluster.go:1776`, PUBLIC) or
`kv.Cluster().ScanPrefix(prefix)` per family routed to the owning shard. The
robust path is `Aggregate` with a per-node `LocalScanPrefix("pastes/")` /
`"versions/"` / `"sites/"` fn, which walks every shard's local rows once and
returns `(key, payload)` pairs the tool decodes. (The legacy metadata migrator
proves the Aggregate/scan pattern is reachable from a pure shale-public consumer.)
For each row it extracts `(slug, kind, verNum, sha, current_blobid)`.

---

## 4. The migrator's loop (enumerate -> stage -> rebind -> verify)

Given a constructed blob-enabled migrate-cluster `kv *cluster.BlobKV` and a
read-only MinIO client for the OLD `hostthis-blobs` bucket (`oldBlobs`):

```
for each record-blob (slug, kind, verNum, sha) enumerated from the metadata:
    if the row already carries a non-empty blobid for this sha:   # idempotency pre-check
        skip  (counted already-migrated)
    body := oldBlobs.Get(bareKey(sha))     # the magic+zstd object, verbatim
    if not found:
        record + FAIL  (a row referencing a missing blob is real metadata/blob drift)
    routeKey := []byte("pastes/" + slug)   # pastes/ and sites/ both shard on {slug}
    ref := kv.StageBlob(ctx, routeKey, bytes.NewReader(body), len(body))
         # mints blobid, PUTs blob/<unit>/<blobid> into the NEW collocated bucket
    ref.ContentHash = sha                  # carried into the pointer
    kv.Transact(routeKey, func(tx *cluster.BlobTx) error {
        # re-read the row inside the tx (CAS-safe), set blobid, bind:
        row := decode(tx.Get(rowKey))
        if row.BlobID == ref.BlobID && tx.Get(brefKey) present: return nil  # in-CAS idempotency
        row.BlobID = ref.BlobID            # (or FileBlobs[sha]=ref.BlobID for a site)
        tx.Put(rowKey, encode(row))
        return tx.BindBlob(ref)            # writes bref/{<slug>}/<unit>/<blobid>
    })
    progress.record(slug, sha, ref.BlobID) # checkpoint for resume
```

- **`StageBlob` reuses the bytes verbatim**: the old object is ALREADY
  `magic + zstd(...)` (written by the standalone `CompressedBlobStore`), and
  `StageBlob` streams what it is given without re-encoding, so the migrator passes
  the old bytes straight through. The read path's decode peels the magic + zstd on
  the way out, exactly as for a fresh upload. Do NOT re-compress.
- **Bounded concurrency across DISTINCT slugs** (a worker pool of K, K ~ 4-8):
  distinct slugs route to (mostly) distinct shards, so parallel writes do not
  self-contend one owner's CAS. Do NOT parallelize within one slug.
- **The loop runs on ONE migrate node (the founder).** Its `Transact` routes +
  R=2-replicates through the 3-member migrate ring; the other two nodes apply the
  forwarded replica writes. Single writer, no app contention (app is down).

---

## 5. The cutover runbook (read-down)

The migrator writes blobs + binds while the OLD `hostthis-blobs` bucket stays
RETAINED + untouched (rollback safety). The exact sequence:

**Preconditions.** The legacy->sharded metadata migration is DONE (the sharded
R=2 cluster holds all rows, with empty `blobid` fields). The blob-enabled image
(`feat/shale-blobs`, the staging-validated `shale-blobs-*` tag) is BUILT + pushed
but NOT yet the prod serving image. The collocated blob bucket
`HOSTTHIS_SHALE_BLOB_BUCKET` exists in MinIO (created, empty).

```
# 0. PRE-FLIGHT: dry-run the migrate cluster against prod buckets (READ-ONLY scan,
#    no writes) to confirm enumeration count + that every referenced sha exists
#    in hostthis-blobs.
kubectl -n hostthis apply -f infra/hostthis/k8s/sharded/35-blob-migrate-job.yaml   # args: [-dry-run]
kubectl -n hostthis logs job/hostthis-blob-migrate -f      # expect ~229 record-blobs, 0 missing
kubectl -n hostthis delete job hostthis-blob-migrate

# 1. TAKE THE APP DOWN (read-down). Scale the prod app StatefulSets to 0 so NO
#    node holds any unit's slatedb open. (Both the seed and the worker set.)
kubectl -n hostthis scale statefulset hostthis-shard-seed --replicas=0
kubectl -n hostthis scale statefulset hostthis-shard      --replicas=0
kubectl -n hostthis rollout status ...      # wait until 0/0, pods gone
#    The public ingress now serves nothing (read-down maintenance window).

# 2. RUN THE 3-NODE MIGRATE CLUSTER (the copy). The Job's 3 pods form their OWN
#    R=2 ring over the quiescent hostthis-metadata + hostthis-blobs + collocated
#    buckets, enumerate, stage, rebind. Idempotent + resumable.
kubectl -n hostthis apply -f .../35-blob-migrate-job.yaml      # args: [] (real copy)
kubectl -n hostthis logs ... -f       # "migration complete: N record-blobs rebound"
#    On a mid-flight crash, re-apply the Job: the per-record skip + in-CAS
#    idempotency make a re-run correct (it skips already-rebound rows).

# 3. VERIFY (the zero-loss gate). A fresh 3-node migrate cluster re-reads EVERY
#    record-blob's NEW blobid, GetBlob from the collocated bucket THROUGH the
#    cluster (proving the bref routed + R=2-replicated), decompresses, recomputes
#    sha, and byte-compares to the OLD hostthis-blobs <sha> object. Exit non-zero
#    on ANY miss/mismatch.
kubectl -n hostthis apply -f .../35-blob-migrate-job.yaml      # args: [-verify]
kubectl -n hostthis logs ... -f       # "VERIFY PASSED: all N present + byte-identical"
#    DO NOT PROCEED until verify is GREEN.
kubectl -n hostthis delete job hostthis-blob-migrate

# 4. DEPLOY THE BLOB-ENABLED IMAGE. Point the prod overlay at the shale-blobs
#    image tag + set HOSTTHIS_SHALE_BLOB_BUCKET in the env. Scale the app back up.
#    The new pods open the SAME unit DBs at a strictly-higher epoch (fenceEpoch =
#    durable+1) and recover the migrate cluster's flushed tail.
#    (edit overlays/prod: image -> shale-blobs-<tag>; add HOSTTHIS_SHALE_BLOB_BUCKET)
kubectl -n hostthis apply -k infra/hostthis/k8s/sharded/overlays/prod
kubectl -n hostthis scale statefulset hostthis-shard-seed --replicas=1
kubectl -n hostthis scale statefulset hostthis-shard      --replicas=2
kubectl -n hostthis rollout status ...      # wait healthy, ring formed, /healthz 200

# 5. SMOKE (live verify). Lift is automatic once pods are Ready.
#    - read an OLD (pre-migration) paste: exercises migrated blobid -> GetBlob -> 200
#    - read a migrated SITE: exercises FileBlobs -> GetBlob -> 200
#    - upload a NEW paste, read it back: exercises the READY-direct collocated path
make -C <hostthis-repo> smoke   # or the infra-side post-deploy smoke

# 6. KEEP THE OLD BUCKET. hostthis-blobs stays UNTOUCHED for a rollback window
#    (days). It is the rollback anchor. GC it only after confidence (step 7).
```

7. **Decommission (after grace).** Once confident, delete `hostthis-blobs` (drop
   the `HOSTTHIS_S3_BUCKET` wiring). Until then it is the rollback anchor.

**Rollback** (any step before 7): the old bucket is untouched + byte-complete,
and the pre-phase-4 image read every blob from it by sha. Roll the image back to
the pre-blob (detached-store) tag + restore the old config; the migrated blobids
+ brefs are simply ignored by the old image (it reads by sha from the old
bucket). No data was mutated in place - the rebind only ADDED a `blob_id` field
(`omitempty`; the old image's row decode tolerates an unknown-but-present field)
+ a `bref/` key (the old image never reads bref). So rollback is "redeploy old
image + old config." Note: the migrate cluster's mounts bumped each unit's fence
epoch; the rolled-back app simply opens at the next-higher epoch (fence is always
"strictly above durable"), so a higher epoch is never a problem for a re-open.

**The freeze is just "app scaled to 0."** With read-down there is no
`HOSTTHIS_READ_ONLY` flag, no dual-read fallback, no app change. The migrate
cluster is the ONLY thing touching the buckets between step 1 and step 4, so
there is no concurrent-writer race at all (not merely a frozen one).

---

## 6. Idempotency, resume, and the verify pass

### 6.1 Idempotency + resume

- **Per-record skip** (primary): the cheap pre-check (the row already carries a
  non-empty blobid for this sha -> skip BEFORE staging) makes a re-run nearly free
  and, crucially, keeps a re-run from minting a fresh blobid + leaking the
  newly-staged object. This is the main idempotency guard.
- **In-CAS idempotency** (backstop): inside the `Transact`, if the row already
  carries `ref.BlobID` AND the bref exists, return nil (no-op). Makes even a
  partially-applied record safe.
- **Resume**: an append-only progress manifest (`(slug, kind, verNum, sha,
  blobid, status)`) to a MinIO object or a PVC file lets a restart skip completed
  records. Even without it, the per-record skip makes a cold re-run correct (just
  slower). The manifest is an optimization + audit trail, not a correctness
  requirement.

### 6.2 The verify pass

A `--verify` mode (mirroring `infra/hostthis/migrate`'s `verifyNewCluster`): the
INDEPENDENT zero-loss reconciliation, run on a FRESH 3-node migrate cluster (so
it proves the writes survive a full close+reopen, exactly as the app's restart
will). For every record-blob:

1. Re-read the row from the cluster (`kv.Get(rowKey)`), pull its NEW blobid.
2. `kv.GetBlob(ctx, routeKey, blobid)` from the collocated bucket THROUGH the
   cluster (so it proves the bref routed + R=2-replicated, not just object
   existence).
3. Decompress, recompute sha256 of the decompressed bytes, assert it equals the
   record's `ContentSHA`.
4. ALSO fetch the OLD bucket's `<sha>` object, decompress, assert byte-identical
   to the new (a direct old-vs-new body compare).

Exit non-zero on any missing blobid, missing bref, GetBlob failure, sha mismatch,
or body mismatch. The verify does NOT trust the migrate's self-reported counts;
it re-derives the keyset from the metadata and re-reads every blob. This is the
gate that authorizes step 4+ of the runbook.

**R=2-consistency note in the verify.** Because the verify cluster is itself a
3-node R=2 ring with `ReadConsistency=ReadQuorum` (the prod default, set in
`NewShaleRepo` and matched by the tool), each `GetBlob`'s bref read is a quorum
read across BOTH replicas of the unit. So verify GREEN proves the pointer is
present on a quorum (both, at R=2), i.e. the bind landed on both replicas - the
exact property a single-node migrator would have FAILED. This is how the runbook
catches the crux-(a) failure mode if it ever crept in.

---

## 7. R=2 consistency: does the restarted cluster see complete replicas?

**Yes - BECAUSE the migrator runs as the full 3-node R=2 topology, not a single
node.** Each blobid+bref write fans out, through the migrate cluster's 3-member
ring, to BOTH replica positions of the owning unit
(`putReplicatedUnitAttempt`/`replicateCASBatchAttempt` over
`ring.LocateKeyN(unit, 2)`), and each position is an INDEPENDENT durable slatedb
database at `u/g<gen>/u<id>/r0` and `.../r1` (`dbname.go` `dbNameReplicaFor`,
the prefix-disjointness guarantee). The migrate cluster flushes both on
`CloseUnit`. When the real app restarts, it opens BOTH positions (its own ring
also places the unit on the same two nodes - same UnitCount, same ring hash) at a
higher epoch and reads the flushed tail of EACH. So every migrated unit has both
replicas carrying the blobid + bref; a quorum read finds the pointer; a
single-node loss after restart still leaves the surviving replica complete. **No
unit is left with a stale/missing replica.**

Had the migrator been a single standalone node (the rejected shape), exactly the
opposite: its 1-member ring writes one position per unit, the app restarts seeing
one fresh + one stale replica per unit, and only lazy per-key read-repair would
(eventually, if ever read) fix the stale one - an un-read blob's second replica
stays stale and a node loss loses the only fresh copy. The verify pass (quorum
read at R=2) is the gate that would catch this.

---

## 8. Top data-loss risks + mitigations

**Risk 1 - the single-node-migrator R=2 trap (the crux danger).** A standalone
migrate process configured "like prod" but with a one-member ring writes only one
replica position per unit (`unitReplicas` clamps to the local node on a 1-member
ring), leaving the second replica stale; shale does NOT backfill a whole replica
position, so the restarted cluster carries half-stale units. **Mitigation: the
migrator runs as the FULL 3-node R=2 topology (section 2.1), so every write fans
to both positions through the cluster's own routing. The verify pass (section
6.2) reads every bref back via a quorum read at R=2, which can only pass if both
replicas carry the pointer - so a regression to the single-node shape is caught
before deploy.**

**Risk 2 - mis-routing via a wrong/drifted ShardKeyFn, UnitCount, or KeyPrefix.**
The tool replicates `shaleShardKey` + sets UnitCount=16 / KeyPrefix=hostthis-sharded;
a divergence from hostthis mis-keys every object + pointer and post-cutover reads
404. **Mitigation: the verify pass reads every blob back THROUGH the migrate
cluster (catches a tool-internal inconsistency); the STAGING run (section 9) has
the ACTUAL hostthis blob image read the migrated rows (catches a
tool-vs-hostthis drift). The replicated functions carry a pin-comment to their
hostthis source. Optionally hostthis exports `shaleShardKey` as a public package
both import, eliminating drift (open question 11.2).**

**Risk 3 - the old bucket is mutated/deleted before verify passes.** **Mitigation:
the tool opens `hostthis-blobs` READ-ONLY (Get only), the runbook RETAINS it
untouched through step 6, decommission (step 7) is a separate gated post-grace
action, and the verify compares old-vs-new bodies directly so it cannot pass
unless the old bytes still exist + match.** Rollback = redeploy old image + old
config; old bytes always intact.

**Risk 4 - a writer races the migrate.** With read-down (app scaled to 0) the
migrate cluster is the ONLY process touching the buckets, so there is no
concurrent app write and no same-slug CAS contention from the app. The migrator's
own concurrency is bounded + spread across distinct slugs (distinct shards), so it
never self-contends one owner's CAS.

**Risk 5 - two writers hold a unit at once (split-brain during the migrate
window).** Could happen if the app were NOT fully down (a straggler pod) while the
migrate cluster opens the same units. **Mitigation: step 1 scales BOTH app
StatefulSets to 0 and waits for the pods to be GONE before step 2. slatedb's
writer-epoch fence is the backstop even if a straggler lingered (the migrate
open fences it), but the runbook does not rely on the fence - it relies on the
app being verifiably down.** The migrate cluster's own 3 nodes never double-open a
position (each owns distinct positions per the ring; the fan-out forwards to the
peer, it does not co-open).

---

## 9. Testing on a prod-data COPY (staging-first)

The staging sharded cluster (`infra/hostthis/k8s/sharded/overlays/staging`, 3
pods, R=2, UnitCount=16) runs the blob image (`shale-blobs-*` tag,
`HOSTTHIS_SHALE_BLOB_BUCKET` set). The `SHARD-MIGRATION.md` prod-copy-into-staging
pattern (`hostthis-prodcopy/*` prefix mirror) extends to phase 4:

1. **Seed staging with old-shaped rows + old blobs.** Mirror a representative
   slice of prod into the staging MinIO: the prod `hostthis-metadata` sharded
   unit DBs (under a `hostthis-prodcopy` key-prefix) AND the prod `hostthis-blobs`
   bucket (into a `hostthis-prodcopy-blobs` bucket), read-only on the prod side
   (`mc mirror` / a copy Job). A SAMPLE is fine (a few hundred pastes + a couple
   of sites + their blobs); the migrate is per-record, so a sample exercises every
   path: paste head, versions, pinned versions, site files, AND a deliberately
   planted row referencing a MISSING blob (to test the FAIL path). The rows have
   EMPTY `blob_id` (the pre-phase-4 shape) - so seeding is just copying the
   already-emptyblobid prod rows.
2. **Run the FULL runbook against staging**: scale the staging app to 0, run the
   3-node migrate Job (pointed at the `hostthis-prodcopy` metadata prefix + the
   `hostthis-prodcopy-blobs` old bucket + a fresh staging collocated bucket), run
   the verify Job, then deploy the staging blob image + smoke. Assert: every
   sampled record-blob ends with a blobid + a routed+replicated bref at R=2,
   GetBlob returns byte-identical bytes, verify GREEN, the ACTUAL hostthis blob
   image serves old + new + sites after restart (the tool-vs-hostthis routing
   cross-check), and rollback works (redeploy the pre-phase-4 image, confirm it
   reads the sampled pastes from the old prodcopy bucket).
3. **Prove R=2 on the restarted cluster** (the crux gate): after the migrate + a
   staging app restart, kill ONE staging node and confirm reads of migrated blobs
   still 200 (the surviving replica carries the pointer). A single-node-migrator
   bug would fail this for the units whose only-written replica was on the killed
   node.
4. **Measure the migrate window** on a 2 GB box (per-blob PUT latency * blob count
   / write concurrency) to size the prod read-down window before scheduling. With
   229 prod objects and ~2 s/PUT measured, serial is ~8 min; K=6 concurrency
   across distinct slugs cuts it to ~1.5 min.
5. Only then schedule the prod cutover (operator go).

Because staging shares the box class (2 GB CPX11) + the MinIO topology with prod,
the staging run de-risks the CAS contention, the window estimate, the routing
correctness, AND the R=2-completeness against real-shaped data before prod.

---

## 10. Why a separate infra tool is correct here (reconciling with the module boundary)

The earlier phase-4 draft argued the migrator MUST live in `hostthis/cmd/`
because it called `storage.ShaleRepo.RebindLegacyBlob` (an `internal/` method),
and Go forbids cross-module internal imports. **Both constraints in this revision
dissolve that argument:**

- The rebind is NOT a hostthis method - it is `*cluster.BlobKV.Transact` +
  `*BlobTx.Put` + `*BlobTx.BindBlob`, ALL on shale's PUBLIC surface. The tool
  expresses the row-set + bind directly over the public `*BlobTx`, so it needs no
  hostthis import. The only hostthis-specific knowledge is the ShardKeyFn + the
  row json tags, both of which are small, stable, and replicable (sections 3.2,
  3.3).
- The precedent already exists: `infra/hostthis/migrate/main.go` is a separate
  infra binary using only `shale/pkg/{cluster,rpc}` + `shale/backends/slate`. The
  blob migrator is its sibling, one rung richer (it forms a 3-node cluster + uses
  the `*BlobKV` surface instead of a bare gRPC `Put`).

So the tool lives in `infra/tools/hostthis-blob-migrate/` as its own module,
honoring the CLAUDE.md "ops/migration code goes in infra/, app repos stay clean"
rule AND the Go module boundary - with NO new code in hostthis or shale.
**Confirmed: the migration needs ZERO hostthis/shale code changes** (the only
"if" is whether the operator prefers hostthis to export `shaleShardKey` as a
public package rather than replicate it - section 11.2 - which would be ONE small
public-export in hostthis, not migration logic).

---

## 11. Open questions (need the operator's decision)

1. **Sha-sharing count (informational).** The per-record-blob decision (1.3)
   re-copies bytes shared across records. With 229 objects this is negligible;
   counting distinct-shas vs total-record-blobs during the staging copy confirms
   it. Recommend: ship per-record-blob regardless for the one-time migrate.

2. **Replicate `shaleShardKey` vs export it from hostthis.** This doc defaults to
   REPLICATE (constraint 1: hostthis import-free, the function is ~30 lines + one
   public `ring.ShardKey` dep). The alternative is hostthis exposing it as a
   public `hostthis/pkg/shaleroute` package both the app and the tool import,
   eliminating drift at the cost of one new public package + a one-line app change
   (the app's `cluster.Config.ShardKeyFn` points at the public func). The staging
   run (the actual hostthis image reading migrated rows) catches drift either way,
   so REPLICATE is safe; EXPORT is cleaner-long-term. **Operator's call.** Recommend
   REPLICATE for the one-time migrate (no hostthis change at all), revisit EXPORT
   only if a second tool ever needs the routing.

3. **`blobstore.MinioBlobStore` export check (build-time).** The tool needs a
   `blob.Store` over MinIO. If shale's `blobstore.MinioBlobStore` is in an
   exported package, reuse it; if not, the tool ships its own ~80-line
   `blob.Store` adapter over `minio-go` (the interface is public). Confirm at
   build; not a blocker either way.

4. **Migrate-cluster image dylib source.** The migrate tool is `-tags slatedb`
   (cgo + `libslatedb_uniffi.so`), so its image copies the `.so` from a hostthis
   (or shale) image exactly as `infra/hostthis/migrate/Dockerfile` does
   (`ARG DYLIB_IMAGE`). Pin it to the SAME slatedb version the prod app links, so
   the on-disk slatedb format matches byte-for-byte. (A version skew is a
   correctness risk: the migrate cluster and the app must agree on the slatedb
   manifest/WAL format. Recommend: copy the `.so` from the exact prod app image
   tag.)
