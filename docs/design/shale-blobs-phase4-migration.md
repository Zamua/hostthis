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
`hostthis-blobs` MinIO bucket keyed at bare `<sha>`; the metadata rows carry a
`ContentSHA` but NO `blobid`. After the blob-enabled image is deployed, the read
path resolves a blob via `ShaleRepo.ResolveBlobID` -> `BlobKV.GetBlob`, which
needs a `blobid` on the row and a `bref/{<slug>}/<unit>/<blobid>` pointer in
slatedb. Neither exists for legacy rows. **The migrator must, for every existing
record, copy its bytes into the collocated bucket under a freshly-minted
`blobid`, set that `blobid` on the metadata row, and write the `bref` pointer -
all routed to the correct shard and replicated R=2, so the new image can serve
the existing pastes/sites after cutover.**

This doc supersedes the phase-3 doc section 7 ("Migration shape"), which was
shape-only and predated the sharded R=2 prod reality. It grounds in:

- the phase-3 implementation on `feat/shale-blobs`
  (`internal/storage/shale_repo.go`, `internal/storage/shale_site_repo.go`,
  `internal/shaleblob/blobunit.go`),
- the shale `feat/blob-values` `*BlobKV` surface
  (`pkg/cluster/kv.go`, `pkg/cluster/blob_units.go`, `pkg/rpc/server.go`),
- the prod sharded deploy (`infra/hostthis/k8s/sharded/`, R=2 / UnitCount=16),
- the existing operator migrators (`cmd/hostthis-shale-migrate`,
  `cmd/hostthis-metadata-migrate`, `infra/hostthis/migrate`).

---

## 1. The shape of the data, before and after

### 1.1 Old (current prod)

- **Blob bytes**: the detached `hostthis-blobs` MinIO bucket
  (`HOSTTHIS_S3_BUCKET`), keyed at **bare `<sha>`** (content-addressed, NO
  `<slug>/` prefix - this is the section-10 re-grounding correction to the
  phase-3 doc; the slug-scoped keying never existed on main). Each object is a
  `magic + zstd(bytes)` body (`HZ\0\x01` prefix, `storage.magicV1`). A blob is
  shared across every record (paste / version / site file) that references the
  same `<sha>`: content-addressed dedup, ONE object per distinct sha.
- **Metadata rows** (sharded shale cluster, R=2, UnitCount=16): `pastes/<slug>`,
  `versions/<slug>/<NNNN>`, `sites/<slug>`, each carrying `ContentSHA`. On
  `feat/shale-blobs` the row structs ALREADY carry `BlobID string
  json:"blob_id,omitempty"` (`slate_repo.go:180`) and the site row carries
  `FileBlobs map[string]string` (sha -> blob-id side-table). For legacy rows
  these are EMPTY (`omitempty` keeps them absent on the wire).

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

### 1.3 The content-dedup subtlety (one blobid per record-blob, not per sha)

The old bucket has ONE object per distinct `<sha>` (global content dedup). The
new model keys by a minted `blobid` per `StageBlob` call. Phase 3 deliberately
does NOT do cross-record content dedup (phase-3 doc 4.5: `StageBlob` mints a
fresh blobid each call; within-record dedup is a phase-3.1 follow-up). So the
migrator must decide its dedup granularity. Two correct options:

- **(per-record-blob)**: stage the bytes once per record that references the sha,
  minting a distinct blobid each time. Two pastes sharing a sha get two objects.
  Re-uploads the shared bytes; matches phase-3's no-cross-dedup model exactly.
- **(per-sha, recommended)**: stage each distinct `<sha>` ONCE (mint one blobid
  per sha), and bind that ONE blobid into every record that references the sha.
  This preserves the old bucket's dedup, copies each object once, and is what a
  human would do. The blobid is identical across records, so each record's
  `bref/{<slug>}/<unit>/<blobid>` still routes to ITS OWN `{slug}` shard (the
  hash tag differs per record), and the same object is referenced by N pointers
  on N shards. `SweepOrphans` keeps the object alive as long as ANY mounted
  pointer references it - **except** a cross-shard sha is referenced by pointers
  on DIFFERENT units, and `SweepOrphans` is mounted-unit-LOCAL: it only sees the
  pointers under units THIS node mounts. **This is the one real hazard of
  per-sha dedup (see section 9, risk + open question).**

**Decision: per-record-blob (option a), one StageBlob per record-blob.** It is
the model phase-3 already encodes, it makes every object referenced by EXACTLY
ONE pointer on EXACTLY ONE shard (so `SweepOrphans`'s mounted-unit-local
referenced-set is always complete for every object), and the storage cost of
re-copying shared bytes is bounded by prod's actual sha-sharing (low; most
pastes are unique). Per-sha dedup is a tempting optimization that re-introduces
the exact cross-shard-orphan-sweep hazard phase-3 spent effort avoiding; reject
it. (If prod has a pathological number of shared shas, revisit as a follow-up
with a dedup-aware sweep, NOT in the one-time migrator.)

Concretely: the migrator enumerates record-blobs `(slug, sha, blobid_field,
kind)` - the paste head, each version row, each site file - and for each, copies
`<sha>` bytes -> a fresh `blob/<unit>/<blobid>`, sets that row's blobid, binds
the pointer.

---

## 2. THE CRUX: how does the migrator perform routed, replicated blobid+bref writes?

Prod is a **sharded R=2 cluster**: 3 hostthisd pods (a seed StatefulSet + a
2-replica worker StatefulSet per `infra/hostthis/k8s/sharded/base/20-statefulsets.yaml`),
UnitCount=16, ReplicationFactor=2, each pod an embedded shale node
(`cluster.NewBlobKV`, `BindAddr=$(POD_IP):7946`, `GRPCAddr=$(POD_IP):7947`,
seeds the seed pod). A blobid+bref write must land:

- on the **correct shard** (the `{slug}` unit, resolved by the live ring), and
- on **both R=2 replicas** of that unit (the cluster's replication), and
- as a **co-commit** (the `blobid` on the row + the `bref` pointer in ONE
  single-shard CAS - or at minimum each consistently).

The offline-raw approach `hostthis-shale-migrate` uses (open the slate backend
directly, single-writer, against a quiescent bucket) does NOT generalize here:
that was ONE slatedb-direct bucket pre-sharding. A sharded R=2 cluster has 16
independent unit databases distributed across 3 nodes with 2 replicas each; an
offline writer would have to replicate the cluster's `shaleShardKey` + ring +
generation resolution + R=2 fan-out + LWW stamping itself. Fragile and
dangerous. Rejected outright (see section 9 risk 1).

### 2.1 Option B (gRPC client) - REJECTED for the bind; usable only for raw bytes

The existing `infra/hostthis/migrate` (Mechanism-B sharded copy) writes through
`rpc.NewClient(addr).Put(key, value)`: the node routes the key to its unit AND
replicates server-side to R=2. This works for the legacy->sharded METADATA copy
because each metadata key is an INDEPENDENT single-key Put.

It does NOT work for the blob BIND, because the bind is NOT a single-key write -
it is a `BindBlob` co-committed with the metadata row inside a routed multi-key
`Transact`. Verified against the wire surface:

- `pkg/rpc/server.go`'s `ShaleNodeServer` exposes `Put / Get / Delete /
  ScanPrefix / LocalScan / ApplyBatch / CommitCAS / Topology / Stats / Ping /
  MigrateRange / ProposeRebalance / ReshardControl / GenState`.
- `cmd/shale` (the CLI) exposes only `put / get / delete / scan / topology /
  stats / ping / bench / rebalance`.
- `CommitCAS` + `ApplyBatch` ARE on the wire, but they are **cluster-INTERNAL**
  (the owner's CAS write-set fan-out to its replicas - `server.go:262`,
  "It is cluster-internal: never called from outside the cluster"). They carry
  pre-validated, pre-stamped envelopes; they are not a public app-facing
  "route this multi-key transaction" surface, and the closure-based
  read-check-then-write semantics of `insertAuthoritative` (the collision
  `tx.Get` + the conditional `tx.Put`s) cannot be expressed as a static
  envelope batch from a client that does not hold the unit's snapshot.

**Could the migrator decompose the bind into single-key Puts over gRPC?** In
principle: `RoutedUnitToken(routeKey)` is a PURE function of routeKey + routing
state (`pkg/cluster/blob_units.go:50` "PURE function of routeKey and the current
routing state (no network, no lease)"), and a `bref` value is a small
`blob.Pointer{ObjKey, Size, ContentHash}` JSON envelope (`blob.Pointer.Encode`).
So a `bref/{<slug>}/<unit>/<blobid>` key + its pointer value COULD be computed
and written via a plain gRPC `Put` (which routes + R=2-replicates). And the
row's `blobid` is a plain `pastes/<slug>` value Put.

**But this is REJECTED**, because it requires the migrator to re-derive the
cluster's `<unit>` token offline: it must reconstruct `shaleShardKey` +
`genUnitForKey` (the ring hash + generation) to compute `RoutedUnitToken` for
the bref key and the object key, OUTSIDE the cluster, with no guarantee it sees
the same live ring generation the running pods see. A generation mismatch (e.g.
if the cluster ever resharded) mis-keys EVERY object + pointer, and the bug is
silent (the writes "succeed", the reads 404). It also splits the row-update and
the bind into two non-atomic Puts (a crash between them leaves a row pointing at
a blobid with no pointer, or a pointer with no row reference). That is exactly
the "replicate the cluster's sharding logic yourself" fragility we are avoiding.
The gRPC client is the RIGHT tool for the raw legacy->sharded metadata copy (it
already is); it is the WRONG tool for the transactional blob bind.

(Bytes-only nuance: `StageBlob` IS a node->object-store direct PUT, so the
migrator COULD PUT the blob bytes to the collocated bucket itself via a plain
MinIO client and only route the bind. But computing the object key
`blob/<unit>/<blobid>` STILL needs the unit token -> same offline-routing
fragility. Don't split it; stage through the cluster so the unit token comes
from the live ring.)

### 2.2 Option A (transient cluster MEMBER) - REJECTED, rebalance risk

The migrator constructs its own `*cluster.BlobKV` with a `BindAddr` + the prod
`Seeds`, joining the prod cluster as a 4th node, then issues routed
`Transact`+`BindBlob`.

**Rejected.** A membership join is NOT free in this cluster. The reconcile loop
(`pkg/cluster/cluster.go`, `reconcileInterval = 5s`) evaluates ownership on every
membership change and drives a lease-handoff rebalance (acquire newly-owned
units, drain released ones). **There is NO observer / non-owning / client-only
join in shale** (confirmed: no such concept in `pkg/cluster/*.go`). Any node
that joins the ring with a `BindAddr` becomes an owner candidate, so a 4th node
joining a 3-node R=2 / UnitCount=16 cluster TRIGGERS a rebalance: ~1/4 of the 16
units' replicas migrate onto the transient node, then migrate BACK when it
leaves. During a write-freeze that data churn is "merely" wasteful, but it is a
real topology disturbance on the 2 GB boxes, it competes for MinIO/CPU with the
migrate writes, and a transient node that dies mid-migrate leaves the cluster
mid-rebalance. Joining the prod ring to run a one-time batch job is the wrong
risk profile. Rejected.

### 2.3 Option C (in-cluster, app wiring) - CHOSEN

**The migrator runs INSIDE one of the existing prod pods' process model: it
constructs the SAME `ShaleRepo` + `*cluster.BlobKV` the app constructs (same
`NewShaleRepo` config: same Seeds, same UnitCount=16, same R=2, same
ShardKeyFn, same BlobStore pointed at the collocated bucket), joins the EXISTING
cluster as a node that the app would have been anyway, and re-keys each record
via the SAME routed `Transact`+`BindBlob` path the app uses (`StageBlobStream` +
the seam's `Commit` -> `WithPendingBinds` -> `insertAuthoritative`/the site
bind).**

Two concrete shapes for "inside the app wiring", pick one (section 4):

- **C1 (a `migrate-blobs` subcommand in hostthisd / a sibling cmd in the repo)**:
  a binary built from the hostthis module that constructs a `ShaleRepo` exactly
  as `cmd/hostthisd` does, but instead of serving SSH/HTTP it runs the re-key
  loop, then exits. It joins as a transient cluster member (same rebalance
  caveat as A, BUT - critically - it is the SAME node count the steady state
  has if you run it as a REPLACEMENT for one paused app pod, see C-runbook).
- **C2 (a one-shot routed-write via a co-resident process)**: run the re-key
  loop as a goroutine/entrypoint variant inside an EXISTING blob-enabled pod
  (e.g. a Job that mounts the same config and joins, OR an admin verb on the
  running pod). Reuses a node that is ALREADY in the ring, so NO net membership
  change, NO rebalance.

**The key realization that makes C safe: run the migrator AS one of the cluster's
own nodes during a write-freeze, NOT as an extra node.** The cleanest form (the
recommended runbook, section 5): freeze app writes, then run the migrator as a
process that joins the cluster bringing the member count to exactly what the app
ran at (3), OR run it co-resident in a paused-but-present pod. Either way the
migrator is "a cluster node that happens to be running a batch loop instead of
serving requests" - the same routing, the same R=2, the same CAS, with ZERO new
sharding/replication logic of its own. It calls `ShaleRepo.StageBlobStream`,
`ShaleRepo.RouteKeyForSlug`, and a NEW thin `ShaleRepo.RebindLegacyBlob` method
(section 3) that runs the existing `insertAuthoritative`-style `{slug}` CAS to
set the blobid + bind the pointer, going through `runAuthoritative` ->
`r.kv.Transact(slug, ...)` -> `BindBlob`. The cluster does the routing + R=2; the
migrator does none of it.

**Justification (one paragraph).** Option C is the only mechanism that performs
the blobid+bref write through the cluster's OWN routing + R=2 + CAS instead of
re-implementing them. B can't express the transactional multi-key bind over the
wire (the bind surface is `*BlobKV.Transact`, in-process only; the gRPC server
exposes single-key + internal-fan-out RPCs, never a public routed Transact), and
faking it via offline-computed single-key Puts requires the migrator to
reconstruct the live ring's unit-token derivation outside the cluster - the
exact fragility we reject. A joins as an EXTRA node and triggers a rebalance on
the 2 GB boxes. C reuses `NewShaleRepo` + the phase-3 seam verbatim, so the
migrator inherits every routing/replication/CAS guarantee the app already has
and tested, and adds only a loop + one thin repo method.

### 2.4 The module-boundary consequence (where C must live)

Phase-3 doc section 7 says the migrator "lives in `infra/tools/`". **That claim
did not reckon with the sharded routing need and is WRONG for phase 4.** Option C
constructs a `storage.ShaleRepo` and calls `RouteKeyForSlug` /
`StageBlobStream` / a new `RebindLegacyBlob`. `ShaleRepo` is in
`internal/storage`, and **Go forbids importing another module's `internal/`
package** (`github.com/Zamua/hostthis/internal/storage` is unreachable from a
separate `infra/` module). The existing `cmd/hostthis-shale-migrate` and
`cmd/hostthis-metadata-migrate` live IN the hostthis repo for exactly this
reason: they import `internal/storage` / `internal/migrate`.

So the phase-4 migrator's CODE must live in **`hostthis/cmd/hostthis-blob-migrate/`**
(a sibling of the two existing in-repo migrators), `-tags slatedb`. The operator
DEPLOY config (the k8s Job, the image tag, the env wiring) lives in
**`infra/hostthis/`** (next to the existing `sharded/30-migrate-job.yaml`),
per the public-repo / private-infra split. This reconciles the CLAUDE.md
"ops code goes in infra/" rule with the Go module boundary: the rule's intent is
that operator-environment-specific code stays out of the app repo, but a tool
that MUST import `internal/storage` to route correctly cannot physically live
outside the module. The mitigation is that the tool is a thin, environment-FREE
binary (reads config from env vars exactly like `hostthisd`, hardcodes no
IPs/paths/secrets), exactly like the two migrators already in `cmd/`. It is
app-adjacent operator tooling, sharing the `slatedb`-tagged build the rest of
the shale path uses. See section 8 for the precise file placement.

---

## 3. The one new repo method: `ShaleRepo.RebindLegacyBlob`

The phase-3 seam binds blobs for a NEW write (insert / append / deploy). The
migrator needs to bind a blob onto an EXISTING row WITHOUT re-inserting it (the
row, its quota reservation, its expiry index are all already correct - only the
`blobid` field + the `bref` pointer are missing). Adding a new bind via
`InsertWithQuotaCheck` would double-charge quota and collide on the existing
slug. So phase 4 adds ONE thin method to `ShaleRepo` (`-tags slatedb`):

```go
// RebindLegacyBlob sets the blobid on an existing record's row(s) and binds the
// blob pointer, in ONE {slug}-shard CAS. It is the phase-4 migration primitive:
// the row already exists (it was migrated by the legacy->sharded copy with no
// blobid); this routed transaction reads the row, sets BlobID (paste head +
// matching version, or the site FileBlobs entry), and BindBlob(ref) -
// co-committed. Idempotent: if the row already carries this blobid AND the bref
// exists, it is a no-op (re-runnable). Routes + R=2-replicates via the cluster
// (runAuthoritative -> r.kv.Transact -> BindBlob), so the migrator does no
// sharding/replication itself.
func (r *ShaleRepo) RebindLegacyBlob(
    ctx context.Context, slug domain.Slug, kind RecordKind,
    verNum int, contentSHA string, ref cluster.BlobRef,
) error
```

It is `runAuthoritative(pasteKey, []BlobRef{ref}, func(tx, bind){...})` (the
SAME helper `insertAuthoritative` uses, `shale_repo.go:1509`), whose body:

1. `tx.Get(pastes/<slug>)` (or `sites/<slug>`); if absent -> `ErrNotFound`
   (the row should already be there post-metadata-migration; a missing row is an
   operator error to surface, not skip).
2. **Idempotency check**: if the row's `BlobID == ref.BlobID` (paste) / the
   `FileBlobs[sha] == ref.BlobID` (site) AND `tx.Get(brefKey(ref))` is present,
   return nil (already migrated). This makes the whole pass re-runnable.
3. Set `BlobID = ref.BlobID` on the paste head row (if `contentSHA` is the head's
   served sha) AND/OR on the matching `versions/<slug>/<verNum>` row; or set
   `FileBlobs[contentSHA] = ref.BlobID` on the site row.
4. `bind()` -> `tx.BindBlob(ref)` (writes `bref/{<slug>}/<unit>/<blobid>`).

Because `runAuthoritative` pins on `pasteKey` (the `{slug}` shard) and the bref
hash tag is `{<slug>}`, the row Put and the bref Put co-route to the SAME unit
and commit in ONE single-shard CAS, R=2-replicated by the cluster. This is the
identical guarantee `insertAuthoritative` gives a fresh paste.

The migrator stages the bytes first (`StageBlobStream`, which mints the blobid +
PUTs the object), then calls `RebindLegacyBlob` with the returned ref. A crash
between stage and rebind leaves a staged-but-unbound object that `SweepOrphans`
reclaims after grace (the SAME leak-only state a crashed fresh upload leaves).

`RebindLegacyBlob` is product-adjacent (it touches the row schema) but is a
MIGRATION primitive, so it carries a doc comment marking it as such and is
covered by the phase-3-style `blobmem`+real-cluster tests in
`internal/storage` (not in the cmd binary).

---

## 4. The migrator's loop (enumerate -> stage -> rebind -> verify)

The migrator, given a constructed blob-enabled `ShaleRepo` (call it `repo`) and
a MinIO client for the OLD `hostthis-blobs` bucket (call it `oldBlobs`):

```
for each record-blob (slug, kind, verNum, sha) enumerated from the metadata:
    if row already carries a non-empty blobid for this sha:   # idempotency, cheap pre-check
        skip  (counted as already-migrated)
    body := oldBlobs.Get(bareKey(sha))     # the magic+zstd object, verbatim
    if not found:                          # a row referencing a missing blob
        record + FAIL (do not silently skip - it is real metadata/blob drift)
    routeKey := repo.RouteKeyForSlug(slug)
    ref := repo.StageBlobStream(ctx, routeKey, bytes.NewReader(body), len(body), sha)
         # mints blobid, PUTs blob/<unit>/<blobid> into the NEW collocated bucket
    repo.RebindLegacyBlob(ctx, slug, kind, verNum, sha, ref)
         # ONE {slug} CAS: set blobid on the row + BindBlob, R=2-replicated, idempotent
    progress.record(slug, sha, ref.BlobID)   # checkpoint for resume (section 6)
```

**Enumeration** reuses the existing aggregating scans the repo ALREADY exposes
for the sweep: `ReferencedBlobSHAs` (`shale_repo.go:2239`, aggregates
`pastes/` + `versions/` across all shards, excludes tombstones) gives the paste +
version record-blobs; `ReferencedSiteBlobSHAs` (the site equivalent) gives the
site files. But those return shas only; the migrator needs `(slug, verNum, sha,
blobid_currently)`. So enumeration is a `ScanPrefix("pastes/")` +
`ScanPrefix("versions/")` + `ScanPrefix("sites/")` decode loop (the migrator
holds the cluster, so `repo.cluster.ScanPrefix` via an exported helper), reading
each row's slug + sha + current blobid + (for sites) each `FileBlobs` /
manifest file. The migrator decodes the SAME row structs the repo uses
(`pasteRow` / `versionRow` / `siteRow`) - it is in the same module, so it can.

**`StageBlobStream` reuses the bytes verbatim**: the old object is ALREADY in the
`magic + zstd(...)` at-rest format (it was written by the standalone
`CompressedBlobStore.PutPrecompressed`), and `StageBlob` streams what it is given
without re-encoding, so the migrator passes the old bytes straight through. (Do
NOT route the bytes through `EncodeCompressedBody` - that would double-encode an
already-encoded body.) The read path's `DecodeCompressedStream` then peels the
magic + zstd on the way out, exactly as it does for a freshly-uploaded blob.

---

## 5. The cutover runbook (the prod sequence)

The migrator writes blobs + binds while the OLD `hostthis-blobs` bucket stays
RETAINED + untouched (rollback safety). The full sequence, mirroring the
proven `infra/hostthis/SHARD-MIGRATION.md` cutover shape:

**Preconditions.** The legacy->sharded metadata migration is DONE (the sharded
R=2 cluster holds all the rows, with empty `blobid` fields). The blob-enabled
image (`feat/shale-blobs`) is BUILT + pushed, but NOT yet the serving image (or
it is serving but the read path still falls back to the old store for
empty-blobid rows - see section 7 dual-read).

1. **Freeze.** Make prod READ-ONLY so the migrator is the only writer (section
   6 for the mechanism). App reads keep serving from the OLD store (legacy rows
   have no blobid; the blob-enabled read path falls back to the detached store
   for empty-blobid rows, section 7). Writes (new pastes/deploys) are rejected
   with a maintenance response. The freeze window is the migrate duration
   (bounded by the blob count + the per-blob PUT latency, prod ~2s/PUT measured;
   N blobs * ~2s, parallelizable - section 6).

2. **Migrate.** Run the `hostthis-blob-migrate` Job (Option C, section 4): it
   joins the cluster (as the only writer during the freeze), enumerates every
   record-blob, stages each into the collocated bucket, and `RebindLegacyBlob`s
   the row + pointer. Idempotent + resumable (re-run on a mid-flight crash; it
   skips already-rebound rows). Records a manifest of `(slug, sha, blobid)` it
   wrote (for the verify pass + resume).

3. **Verify.** Run the verify pass (`--verify`, section 7.1): for EVERY
   record-blob, re-read the row's NEW blobid, `GetBlob(routeKey, blobid)` from
   the collocated bucket, decompress, and assert the bytes + sha equal the OLD
   bucket's `<sha>` object. Exit non-zero on ANY miss/mismatch. This is the
   zero-loss gate: **do NOT proceed past here until verify PASSES.** (The
   verify reads through the cluster, so it also proves the bref pointer routed +
   replicated correctly, not just that the object exists.)

4. **Flip config.** Set the blob-enabled image's config so the read path
   prefers the collocated blob plane (it already does for non-empty blobids; the
   flip is removing the fallback-to-old-store dual-read, OR pointing
   `HOSTTHIS_SHALE_BLOB_BUCKET` if it was a staging value). For a clean
   deployment where every row now carries a blobid, the fallback is dead code
   anyway.

5. **Roll image.** Roll the StatefulSet to the blob-enabled image (if not
   already serving it). `Recreate`/ordered-update per the deploy strategy. The
   new pods construct `NewBlobKV` with the collocated bucket; reads resolve via
   `ResolveBlobID` -> `GetBlob`.

6. **Unfreeze + verify live.** Lift the read-only freeze. Smoke: upload a NEW
   paste (exercises the READY-direct collocated path), read it back; read an OLD
   (pre-migration) paste (exercises the migrated blobid -> GetBlob path); read a
   migrated SITE (exercises FileBlobs -> GetBlob). All 200, bytes correct.

7. **Decommission (after a grace period).** The OLD `hostthis-blobs` bucket stays
   untouched for a rollback window (days). Once confident, GC it (delete the
   bucket / drop the `HOSTTHIS_S3_BUCKET` wiring). Until then it is the rollback
   anchor.

**Rollback** (any step before 7): the old bucket is untouched + byte-complete,
and the old image read every blob from it. Roll the image back to the
pre-phase-4 (detached-store) version + restore the old config; the migrated
blobids + brefs are simply ignored by the old image (which reads by sha from the
old bucket). No data was mutated in place; the rebind only ADDED a blobid field
(omitempty -> the old image's row decode tolerates the unknown-but-present field)
+ a bref key (the old image never reads bref). So rollback is "redeploy old image
+ old config", exactly as the prompt frames it.

---

## 6. The write-freeze mechanism

The migrator must be the ONLY writer during step 2 (so it does not race a live
upload that stages a NEW blob into the same shard, and so `RebindLegacyBlob`'s
idempotency check is against a stable row). Options, increasing invasiveness:

### 6.1 Recommended: a read-only maintenance flag on hostthisd

Add a `HOSTTHIS_READ_ONLY=true` env var the app honors: when set, the SSH upload
verbs (`Upload.Create`, `Manage.Update`, `DeploySite.Deploy`/`DeployToSlug`) and
the HTTP write surface reject with a clear "under maintenance, read-only" message
(a 503 / SSH stderr line), while READS keep serving. Set it on the StatefulSet,
roll the pods (they keep serving reads), run the migrate, then unset + roll back.

- **Pro**: reads stay up (no user-visible downtime for viewing pastes/sites),
  the freeze is a clean app-level guarantee (not a network hack), it is
  reversible by an env flip, and it is a generally-useful operational primitive
  (future migrations, incident response). It is a small, well-scoped feature.
- **Con**: it is an app change (a new env + the write-path guards), so it must be
  spec'd + tested + shipped in the blob-enabled image BEFORE the migrate. That is
  acceptable: it is part of the phase-4 deliverable, and `HOSTTHIS_READ_ONLY` is
  the kind of thing a hosted service wants anyway.

**Decision: ship `HOSTTHIS_READ_ONLY` as a phase-4 app change.** It is the
cleanest freeze, keeps reads up, and is the only option that does not require
taking the app fully down. (The write guards live in the service layer's
use-case entry points, gated on a config bool threaded from the env, NOT in the
domain - DDD-clean.)

### 6.2 Fallback: scale to a read-only posture by stopping the SSH listener

If `HOSTTHIS_READ_ONLY` is not ready, the cruder freeze is to stop accepting
writes by taking the SSH ingress down (the IngressRouteTCP for :22 / the SSH
Service) while leaving the HTTP read Service up. Uploads/deploys (SSH-only)
stop; reads (HTTP) continue. Less clean (it is a routing hack, and the HTTP
write surface - if any - is not covered), but needs no app change. Use only if
6.1 slips.

### 6.3 Last resort: full maintenance page

Flip the ingress to a maintenance page (writes AND reads down) for the migrate
window. This is the `SHARD-MIGRATION.md` "downtime blip" model. Simplest, but
user-visible read downtime for the whole migrate duration. Reject unless the
blob count is tiny (sub-minute migrate).

**The migrator co-resident with the freeze**: during the freeze, the migrator
joins as a cluster node. Because no app writes are happening, the only writer is
the migrator, so there is no Sybil-write / same-slug race. The migrator should
run with a BOUNDED write concurrency (e.g. a worker pool of K, K small like 4-8)
so it does not overwhelm the 2 GB boxes' MinIO + slatedb with parallel blob PUTs
+ CAS commits (the single-owner CAS contention note in the user's MinIO memory:
spreading across DISTINCT slugs is fine since they route to different shards;
do NOT hammer one slug). Parallelism across distinct slugs is safe and cuts the
freeze window roughly K-fold from the serial N*~2s.

---

## 7. Idempotency, dual-read, and the verify pass

### 7.1 Idempotency + resume

Two layers, both already designed in:

- **Per-record skip**: the cheap pre-check (row already has a non-empty blobid
  for this sha -> skip) makes a re-run pass over already-migrated rows nearly
  free (one routed Get per record, no stage, no write).
- **In-CAS idempotency**: `RebindLegacyBlob`'s step-2 check (row blobid ==
  ref.BlobID AND bref present -> no-op) makes even a partially-applied record
  safe to re-run. NB: a re-run that re-STAGES (mints a NEW blobid) before
  discovering the row is already bound to an OLD blobid will leave the
  newly-staged object UNBOUND -> `SweepOrphans` reclaims it. So the pre-check
  (skip before staging) is what keeps a re-run from leaking; it is the primary
  idempotency guard, the in-CAS check is the backstop.

- **Resume**: the migrator writes a progress manifest (append-only,
  `(slug, kind, verNum, sha, blobid, status)` per record) to a durable location
  (a MinIO object / a local PVC file). On restart it reads the manifest +
  skips completed records. Even without the manifest, the per-record skip makes
  a cold re-run correct (just slower - it re-scans). The manifest is an
  optimization + an audit trail, not a correctness requirement.

### 7.2 Dual-read window (the migration-window read fallback)

`ResolveBlobID` (`shale_repo.go:1362`) already returns `("", nil-ish)` for a
legacy row with an empty BlobID, and the doc comment says "the seam treats '' as
'sha-keyed' (no shale blob id), which during a migration window means the read
falls back". For the freeze to keep reads up (option 6.1), the blob-enabled read
path must, for an empty-blobid row, **fall back to reading `<sha>` from the OLD
detached bucket**. This requires the blob-enabled image to retain a read-only
handle to the old `hostthis-blobs` bucket during the migration window (a
`HOSTTHIS_LEGACY_BLOB_BUCKET` config that, when set, the `shaleblob.Unit.Read`
consults on an empty-blobid resolve). After step 4 (every row has a blobid),
this fallback is dead and the config is removed.

**This is a phase-4 app change too** (the dual-read fallback in
`shaleblob.Unit.Read` / the http serve path), and it is what makes the freeze a
read-UP freeze instead of read-down. Spec + test it with the freeze flag.
(Without it, the freeze must be read-DOWN - option 6.3 - for the migrate
window.)

### 7.3 The verify pass

A `--verify` mode (mirroring `infra/hostthis/migrate`'s `verifyNewCluster`): the
INDEPENDENT zero-loss reconciliation. For every record-blob:

1. Re-read the row from the cluster, pull its NEW blobid.
2. `GetBlob(routeKey, blobid)` from the collocated bucket (through the cluster,
   so it proves the bref routed + R=2-replicated, not just object existence).
3. Decompress (`DecodeCompressedStream`), recompute sha256 of the decompressed
   bytes, assert it equals the record's `ContentSHA`.
4. ALSO fetch the OLD bucket's `<sha>` object, decompress, and assert
   byte-identical to the new (a direct old-vs-new body compare).

Exit non-zero on any missing blobid, missing bref, GetBlob failure, sha
mismatch, or body mismatch. The verify does NOT trust the migrate's self-reported
counts; it re-derives the keyset from the metadata and re-reads every blob. This
is the gate that authorizes step 4+ of the runbook.

---

## 8. Where the tool lives (file placement)

```
hostthis/   (the app repo - the tool MUST live here: it imports internal/storage)
  cmd/hostthis-blob-migrate/
    main.go            # -tags slatedb; constructs ShaleRepo via the same env
                       #   NewShaleRepo reads; runs the enumerate->stage->rebind
                       #   ->verify loop; reads OLD bucket via a MinIO client.
    main_stub.go       # !slatedb stub (so `go build ./...` w/o the tag compiles),
                       #   mirroring cmd/hostthis-shale-migrate/main_stub.go.
  internal/storage/
    shale_repo.go      # + RebindLegacyBlob (the one new routed primitive, sec 3)
    shale_site_repo.go # + RebindLegacySiteBlob (the site FileBlobs variant)
    shale_blob_migrate_test.go   # blobmem + real-cluster tests for the rebind
                                  #   primitive (idempotency, co-routing, R=2)

infra/hostthis/   (the private infra repo - the operator DEPLOY config)
  k8s/sharded/
    35-blob-migrate-job.yaml     # the Job: image tag, env from the hostthis-env
                                  #   Secret, args [-dry-run | <copy> | -verify],
                                  #   the OLD bucket + NEW collocated bucket wiring.
  BLOB-MIGRATION.md              # the operator runbook (freeze->migrate->verify
                                  #   ->flip->roll->verify->decommission), the
                                  #   prod-copy staging validation, the rollback.
```

**Why the tool is in `hostthis/cmd/` and not `infra/tools/` (overriding the
phase-3 doc's claim + the CLAUDE.md "ops code in infra/" rule):** the routed
blob bind is only reachable through `*cluster.BlobKV.Transact`, which is wrapped
by `storage.ShaleRepo` (an `internal/` package). Go forbids importing another
module's `internal/`, so a tool in `infra/` (a separate module) physically
cannot call `RouteKeyForSlug` / `StageBlobStream` / `RebindLegacyBlob`. The two
EXISTING in-repo migrators (`cmd/hostthis-shale-migrate`,
`cmd/hostthis-metadata-migrate`) are in `cmd/` for exactly this reason. The
binary stays environment-FREE (config from env vars, no IPs/paths/secrets baked
in), so the public-repo cleanliness intent of the CLAUDE.md rule is preserved;
only the deploy GLUE (the k8s Job, the runbook) lives in `infra/`. This is the
honest reconciliation: the rule's spirit is "no operator environment in the app
repo", which this respects; its letter ("the tool goes in infra/") cannot be
met for a tool that must route through the sharded cluster.

---

## 9. Top data-loss risks + mitigations

**Risk 1 - the migrator re-implements sharding/replication and mis-keys (the
crux danger).** If the migrator computed unit tokens / routed writes itself
(option B-decomposed or A-offline), a ring-generation or shard-key mismatch
would silently write blobs + brefs to the WRONG unit, so post-cutover reads 404
for data that "migrated". **Mitigation: option C - the migrator routes through
the cluster's OWN `*BlobKV` via `RebindLegacyBlob`/`StageBlobStream`, deriving
the unit token from the LIVE ring (`RoutedUnitToken`), with R=2 done by the
cluster.** It does zero sharding logic of its own. The verify pass (7.3) reads
EVERY blob back THROUGH the cluster, so a mis-route is caught before cutover, not
in prod.

**Risk 2 - the old bucket is mutated / deleted before verify passes.** If the
migrator (or a cleanup step) touched the `hostthis-blobs` bucket before the new
plane was proven, a mid-migrate failure would have no rollback anchor. **Mitigation:
the migrator opens the old bucket READ-ONLY (Get only, never Put/Delete), the
runbook RETAINS it untouched through step 6, and decommission (step 7) is a
SEPARATE, gated, post-grace-period action. Rollback = redeploy old image + old
config; the old bytes are always intact.** The verify (7.3) compares
old-vs-new bodies directly, so it cannot pass unless the old bytes still exist
and match.

**Risk 3 - a write races the migrate (a new upload stages into a shard the
migrate is rebinding, or the migrate clobbers a concurrently-written row).** A
live upload during the migrate could create a row the migrate's enumeration
missed, or two writers could contend the `{slug}` CAS. **Mitigation: the
write-freeze (section 6, `HOSTTHIS_READ_ONLY`) makes the migrator the ONLY
writer, so there is no concurrent upload and no same-slug CAS contention from the
app. The migrator's own write concurrency is bounded + spread across distinct
slugs (different shards), so it never self-contends one owner's CAS.** Plus
`RebindLegacyBlob` runs inside a `{slug}` CAS read-checking the row, so even a
stray concurrent write conflicts (retried or surfaced) rather than silently
losing.

(Secondary, called out in section 1.3: **per-sha cross-shard orphan sweep**. If
we had chosen per-sha dedup, one object referenced by pointers on multiple shards
would be at risk from the mounted-unit-local `SweepOrphans` (a node sweeping unit
U sees only U's pointers, could classify a still-referenced-elsewhere object as
orphan). The per-record-blob decision (section 1.3) makes every object referenced
by exactly one pointer on one shard, eliminating this entirely. This is WHY
per-record-blob is chosen over the tempting per-sha optimization.)

---

## 10. Testing on a prod-data COPY (staging-first)

The staging cluster is sharded (R=2, UnitCount matching) and runs the blob image
(`infra/hostthis/k8s/sharded/overlays/staging`, `HOSTTHIS_SHALE_BLOB_BUCKET`
set). The `SHARD-MIGRATION.md` staging validation already established the
prod-copy-into-staging pattern (`hostthis-prodcopy/*` prefix mirror). Phase 4
extends it:

1. **Copy a sample of prod into staging MinIO.** Mirror a representative slice of
   the prod `hostthis-metadata` (the sharded metadata DBs) AND the prod
   `hostthis-blobs` bucket into the staging MinIO under a `hostthis-prodcopy/*`
   prefix (read-only on the prod side - `mc mirror` / a copy Job). A SAMPLE is
   fine (a few hundred pastes + a couple of sites + their blobs); the migrate
   logic is per-record, so a sample exercises every path (paste head, versions,
   pinned versions, site files, a row referencing a missing blob to test the
   FAIL path).
2. **Stand up a staging sharded cluster on the copy**, blob image, pointed at
   the `hostthis-prodcopy` metadata + a fresh staging collocated blob bucket,
   with the OLD `hostthis-prodcopy` blob bucket as the legacy-read source.
3. **Run the FULL runbook against staging**: freeze (`HOSTTHIS_READ_ONLY`),
   the migrate Job, the verify Job. Assert: every sampled record-blob ends with a
   blobid + a routed+replicated bref, GetBlob returns byte-identical bytes, the
   verify pass goes GREEN, reads serve old + new + sites, the read-only freeze
   blocks writes while reads stay up (the dual-read fallback), and rollback works
   (redeploy the pre-phase-4 image, confirm it reads the sampled pastes from the
   old prodcopy bucket).
4. **Measure the freeze window** on the staging box (the per-blob PUT latency on
   a 2 GB box * the prod blob count / the write concurrency) to size the prod
   maintenance window before scheduling it.
5. Only then schedule the prod cutover (operator go).

Because staging shares the box class (2 GB CPX11) + the MinIO topology with
prod, the staging run de-risks the CAS contention, the freeze-window estimate,
and the routing correctness against real-shaped data before prod.

---

## 11. Open questions (need the operator's decision)

1. **Per-record-blob vs per-sha dedup (section 1.3).** This doc DECIDES
   per-record-blob (each record re-stages its bytes, every object referenced by
   exactly one pointer on one shard, no cross-shard sweep hazard) over per-sha
   (preserve the old bucket's dedup, but a shared sha's object is referenced by
   pointers on multiple shards, which the mounted-unit-local `SweepOrphans` can
   mis-classify). The decision is grounded in the sweep's locality, but it costs
   storage proportional to prod's actual sha-sharing. **Open: is prod's
   sha-sharing high enough that the re-copy cost matters?** Cannot tell from
   code; needs a count of distinct-shas vs total-record-blobs in the prod
   metadata. If sharing is negligible (likely - most pastes are unique), the
   decision is free. If it is high (e.g. a popular shared template), per-sha
   with a dedup-aware sweep becomes worth a follow-up. Recommend: count it during
   the staging copy; ship per-record-blob regardless for the one-time migrate.

2. **`HOSTTHIS_READ_ONLY` + the dual-read fallback are NEW app changes the
   phase-4 deliverable must ship FIRST (sections 6.1, 7.2).** They are the only
   way to keep reads UP during the freeze. **Open: does the operator want to
   invest in the read-up freeze (ship `HOSTTHIS_READ_ONLY` + the legacy-blob
   dual-read), or accept a read-DOWN maintenance window (option 6.3) for the
   migrate duration?** The read-up path is more app code but no user-visible read
   downtime; the read-down path is zero app code but a maintenance page for the
   (measured) freeze window. Cannot decide from code; it is a
   user-experience-vs-engineering-cost call. Recommend the read-up freeze (the
   `HOSTTHIS_READ_ONLY` flag is independently useful), but it is the operator's
   call against the measured freeze window (step 10.4).
