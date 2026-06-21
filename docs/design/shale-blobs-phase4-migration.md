# hostthis shale-collocated blobs - phase 4: the one-time PROD blob migration

Status: design. The contract for the in-hostthis migrator built on this branch
(`feat/shale-blobs`). All migration code is TEMPORARY and deleted immediately
after the one-time prod blob move completes (containment, section 3).

This is PHASE 4 of the shale streaming-blob effort. Phase 3 built the
transactional shale-collocated blob plane: the `*cluster.BlobKV` wiring in
`ShaleRepo`, the `bref/` `ShardKeyFn` case, the `service.BlobUnit` seam + the
`shaleblob.Unit` adapter, the `blob_id` row-schema field, and the READY-direct
upload collapse. Phase 3 ships a blob-ENABLED `hostthisd` that, for a FRESH
paste, stages the bytes into the collocated bucket and co-commits the pointer
with the metadata.

Phase 4 is the data move: every EXISTING prod blob lives in the detached
`hostthis-blobs` MinIO bucket keyed at bare `<sha>`; the metadata rows carry a
`ContentSHA` but NO `blob_id`. After the blob-enabled image is deployed, the read
path resolves a blob via `ShaleRepo.ResolveBlobID` -> `BlobKV.GetBlob`, which
needs a `blob_id` on the row and a `bref/{<slug>}/<unit>/<blobid>` pointer in
slatedb. Neither exists for legacy rows. **The migrator must, for every existing
record, copy its bytes into the collocated bucket under a freshly-minted
`blob_id`, set that `blob_id` on the metadata row, and write the `bref` pointer -
all routed to the correct shard and replicated R=2, so the new image can serve
the existing pastes/sites after cutover.**

---

## 0. THE PIVOT: run the migration AS hostthis, not as a separate tool

An earlier attempt built a SEPARATE infra migrate tool: a standalone Go module
that stood up its OWN transient 3-node cluster (bare `cluster.NewBlobKV` + a
hand-rolled convergence wait) over the quiescent prod buckets, using DIFFERENT
node IDs than the data's original writers. **It FAILED on staging.** Because its
nodes carried foreign identities, the thin bootstrap boot-DEFERRED the data's
existing serving markers as belonging to strangers, and its hand-rolled startup
never converged (the unsolved shale convergence gap around the foreign-marker
boot-defer path). A cluster that never converges cannot route a single write, so
the migration never started. **That tool is DELETED.**

The fix is to stop pretending the migrator is a new cluster. It is not: it is
the PROD CLUSTER, restarted in a different mode. So:

> **The migration runs AS hostthis itself.** A new one-shot binary,
> `cmd/hostthis-blob-migrate`, constructs the cluster via the app's EXACT,
> proven-to-converge wiring (`storage.NewShaleRepo`), reading the SAME
> `HOSTTHIS_*` env the app pods read, and runs with the app's OWN node identity.
> So it reclaims its OWN serving markers via the proven mass-restart path - there
> is no foreign-marker boot-defer, because the markers are not foreign. It is
> literally "the prod cluster brought up again," just running a re-key loop
> instead of serving HTTP/SSH.

Why this is the correct shape, point by point:

- **It reuses the app's convergence, not a hand-rolled one.** `NewShaleRepo`
  performs the FULL app startup - the gRPC server, the serving-marker
  acquire/reclaim, the reconcile loop - exactly as `cmd/hostthisd` does. That
  startup is the one we have already proven converges on restart in prod (the
  mass-restart auto-recovery contract). The migrator inherits it verbatim by
  calling the same constructor. It does NOT fall back to a bare
  `cluster.NewBlobKV`; a bare cluster is precisely the thin bootstrap that sank
  the separate tool.
- **Same node identity reclaims its own markers.** When a migrate pod boots as
  `hostthis-shard-0` (the same `HOSTTHIS_NODE_ID` the StatefulSet pod uses), the
  durable serving markers it finds in the bucket are its OWN prior markers, so
  the acquire path reclaims them at a strictly-higher epoch (the normal restart
  reclaim) rather than deferring them as a stranger's. Foreign-marker boot-defer
  is the failure mode that only arises when the migrator uses NEW node IDs; we
  eliminate it by using the app's IDs.
- **Zero copied routing/row/bind logic.** The migrator does NOT replicate
  `shaleShardKey`, the row structs, or the bind plumbing. It REUSES them in place
  because it is in the hostthis module: it calls one new `*ShaleRepo` method
  (`RebindLegacyBlob`) that itself reuses `shaleShardKey`, `RouteKeyForSlug`,
  the `pasteRow`/`versionRow`/`siteRow` structs, `ResolveBlobID`, and the
  authoritative-write + `BindBlob` plumbing (`WithPendingBinds` +
  `runAuthoritative`). There is no second implementation to drift from the app's.

The cost is that the migration code lives transiently inside the app repo. That
is acceptable BECAUSE it is fully contained and deleted the moment the migration
completes (section 3). The alternative - a separate tool that copies routing/row/
bind logic AND has to re-solve cluster convergence from scratch - is exactly what
failed.

---

## 1. The shape of the data, before and after

### 1.1 Old (current prod)

- **Blob bytes**: the detached `hostthis-blobs` MinIO bucket
  (`HOSTTHIS_S3_BUCKET`), keyed at **bare `<sha>`** (content-addressed, NO
  `<slug>/` prefix). Each object is a `magic + zstd(bytes)` body (`HZ\0\x01`
  prefix, `storage.magicV1`). A blob is shared across every record (paste /
  version / site file) that references the same `<sha>`: content-addressed
  dedup, ONE object per distinct sha.
- **Metadata rows** (sharded shale cluster, R=2, UnitCount=16): `pastes/<slug>`,
  `versions/<slug>/<NNNN>`, `sites/<slug>`, each carrying `ContentSHA`. On this
  branch the shared row structs ALREADY carry `BlobID string
  json:"blob_id,omitempty"` (paste + version) and the site row carries
  `FileBlobs map[string]string json:"file_blobs,omitempty"` (sha -> blob-id
  side-table). For legacy rows these are EMPTY (`omitempty` keeps them absent on
  the wire).

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
object + the pointer + the row's `blob_id`). **The content-sha is computed over
the DECODED (decompressed) bytes**, so the verify pass decodes a blob's body
before recomputing its sha - matching how the upload pipeline derived the sha in
the first place.

### 1.3 Per-record-blob, not per-sha (dedup granularity)

The old bucket has ONE object per distinct `<sha>` (global content dedup); the
new model keys by a minted `blobid` per `StageBlob` call. Phase 3 deliberately
does NOT do cross-record content dedup. The migrator stages the bytes ONCE PER
RECORD-BLOB (the paste head, each version, each site file), minting a distinct
blobid each time:

- **per-record-blob (CHOSEN)**: every object ends up referenced by EXACTLY ONE
  pointer on EXACTLY ONE shard, so the mounted-unit-local `SweepOrphans`
  referenced-set is always complete for every object. The storage cost of
  re-copying shared bytes is bounded by prod's actual sha-sharing (low; most
  pastes unique).
- **per-sha (REJECTED)**: would preserve the old bucket's dedup but a shared
  sha's object is referenced by pointers on DIFFERENT units, which the
  mounted-unit-local `SweepOrphans` can mis-classify as orphan on a node that
  mounts one shard but not the other. That re-introduces the exact
  cross-shard-orphan-sweep hazard phase-3 spent effort avoiding.

Concretely: the migrator enumerates record-blobs `(slug, kind, verNum, sha)` -
the paste head, each version row, each site file - and for each, copies `<sha>`
bytes -> a fresh `blob/<unit>/<blobid>`, sets that row's blobid, binds the
pointer.

---

## 2. The mechanism: NewShaleRepo in migrate mode

### 2.1 What the migrate binary constructs

`cmd/hostthis-blob-migrate` builds its cluster by MIRRORING
`cmd/hostthisd/metadata_shale.go`'s `buildMetadataShale` exactly: it reads the
same `HOSTTHIS_*` env and passes them to `storage.NewShaleRepo` with the IDENTICAL
`ShaleConfig`:

- `NodeID` from `HOSTTHIS_NODE_ID` (defaults to the hostname, as the app does),
- `Endpoint` / `Region` / `Bucket` / `AccessKey` / `SecretKey` / `UseSSL` /
  `DbName` (the metadata `hostthis-metadata` bucket + the `hostthis-sharded`
  key-prefix the app uses),
- `BindAddr` / `GRPCAddr` / `Seeds` (the same multi-node peer-discovery shape -
  so the migrate pods form the SAME ring the app pods form, addressed by the same
  stable peer DNS),
- `ReplicationFactor=2`, `UnitCount=16`, `RelaxedDurability` from
  `HOSTTHIS_METADATA_AWAIT_DURABLE`, `CacheBytes`,
- and the BLOB plane: when `HOSTTHIS_SHALE_BLOB_BUCKET` is set the binary builds
  the `blob.Store` via `blobstore.New` over the SAME object store (the
  `stripScheme(endpoint)` + creds + `UseSSL` shape `buildMetadataShale` uses) and
  passes it as `ShaleConfig.BlobStore`, so `r.kv` is non-nil and the
  `*BlobKV`-backed methods (`StageBlobStream`, `GetBlobStream`,
  `RebindLegacyBlob`) light up.

Because the config is byte-identical and the node IDs match the app's, the
migrate process IS the prod cluster: same unit DBs, same replica prefixes, same
fence epochs, same routing, same generation. `NewShaleRepo` gives it the full app
startup - the gRPC server, the serving-marker acquire (which RECLAIMS its own
prior markers, no foreign-defer), and the reconcile loop - so it converges via
the proven mass-restart path, not a hand-rolled wait.

### 2.2 The re-key uses the OLD bucket as a read-only source

The migrate binary ALSO opens a read-only MinIO client against the detached OLD
`hostthis-blobs` bucket (`HOSTTHIS_S3_BUCKET`), keyed at bare `<sha>`. For each
enumerated record-blob it GETs the old `<sha>` object (verbatim `magic+zstd`
bytes), STAGES those bytes into the new collocated bucket via
`StageBlobStream(routeKey, body, size, sha)` (mints a blobid, PUTs
`blob/<unit>/<blobid>`), then RE-KEYS the row + binds the pointer via the new
`RebindLegacyBlob` method. The old bucket is opened Get-only; the migrator never
writes or deletes it.

### 2.3 Why R=2 is correct here (no single-node trap)

The migrate process runs at `ReplicationFactor=2` inside the SAME multi-node ring
the app forms (`BindAddr`/`Seeds` set, all migrate pods up), so every
`RebindLegacyBlob` co-commit fans out through the cluster's own routing to BOTH
replica positions of the owning unit - exactly as a live app write would. There
is no "single node with a one-member ring writes only one replica" hazard,
because the migrate is the real R=2 topology, not a lone process. The FOUNDER
(the seed pod identity) runs the re-key loop; the other pods are ring members
that apply the forwarded replica writes and flush on shutdown. The verify pass
(section 5) reads each bref back via a quorum read at R=2, which can only pass if
the pointer landed on both replicas.

---

## 3. Containment (TEMPORARY - delete after the migration)

Every piece of migration code is temporary and removable in one move:

```
cmd/hostthis-blob-migrate/            self-contained CLI dir
  main.go                             -tags slatedb; mirrors buildMetadataShale's
                                      env -> ShaleConfig -> NewShaleRepo, opens the
                                      OLD read-only bucket, runs the
                                      enumerate -> stage -> rebind -> verify loop.
  main_stub.go                        !slatedb stub (fails loudly on a no-cgo build),
                                      mirroring cmd/hostthis-metadata-migrate's pattern.

internal/storage/shale_blob_migrate_TEMP.go   one new *ShaleRepo method:
                                      //go:build slatedb, big DELETE-AFTER header,
                                      func RebindLegacyBlob(...). Reuses the existing
                                      bind plumbing; copies NOTHING.

Dockerfile                            ONE added build+COPY line (commented temp) that
                                      builds cmd/hostthis-blob-migrate alongside hostthisd.

infra (private repo)                  the operator Job manifest + the runbook
                                      (BLOB-MIGRATION.md). NOT in this repo.
```

**Removal after the migration completes** = `rm
internal/storage/shale_blob_migrate_TEMP.go`, `rm -r
cmd/hostthis-blob-migrate/`, revert the one Dockerfile line, and delete the infra
glue. No migration logic is scattered into `upload.go`, `manage.go`, the http/
ssh layers, or any existing storage file, so the revert touches exactly these
four locations and nothing else.

---

## 4. The re-key method: `RebindLegacyBlob`

`internal/storage/shale_blob_migrate_TEMP.go` adds ONE method to `*ShaleRepo`
that re-keys a SINGLE legacy record-blob:

```go
func (r *ShaleRepo) RebindLegacyBlob(
    ctx context.Context,
    slug domain.Slug,
    kind LegacyBlobKind, // LegacyBlobPaste | LegacyBlobVersion | LegacyBlobSiteFile
    contentSHA string,
    ref cluster.BlobRef, // produced by StageBlobStream(routeKey, oldBytes, size, contentSHA)
) error
```

The caller (the CLI loop) first stages the old bytes
(`ref, _ := repo.StageBlobStream(ctx, repo.RouteKeyForSlug(slug), oldBody,
size, sha)`), then calls `RebindLegacyBlob` to set the row's blob_id and bind the
pointer in ONE authoritative `{slug}` transaction.

Inside, the method opens a routed transaction pinned on
`RouteKeyForSlug(slug)` (= `pastes/<slug>`, the `{slug}` shard, where the bref's
`{slug}` hash tag co-routes) by REUSING the existing `runAuthoritative(pinKey,
[]cluster.BlobRef{ref}, body)` helper - the SAME path
`insertAuthoritative`/`AppendVersionWithQuotaCheck` use to bind a fresh paste's
blob. `runAuthoritative` routes through `r.kv.Transact` when refs is non-empty
(the blob-binding path), hands `body` a `bind func() error` that issues
`tx.BindBlob(ref)`, and the body:

1. point-reads the target row inside the tx,
2. sets its blob_id (`pasteRow.BlobID` / `versionRow.BlobID` /
   `siteRow.FileBlobs[contentSHA]`),
3. calls `bind()` (the existing `runAuthoritative` callback that fires
   `tx.BindBlob(ref)`),

all in the SAME `{slug}` CAS - so the row update and the `bref` write co-commit,
R=2-replicated by the cluster, identically to a fresh upload.

Per kind:

- **`LegacyBlobPaste`**: reads `pastes/<slug>`, sets `BlobID = ref.BlobID` on the
  head row, AND sets `BlobID` on the version row whose `ContentSHA` matches
  `contentSHA` (the head's serving version, found by reading the slug's version
  rows before the tx and re-reading the matched version key inside the tx). This
  mirrors `insertAuthoritative` stamping the blob id on BOTH the head and its
  version.
- **`LegacyBlobVersion`**: finds the version row whose `ContentSHA` matches
  `contentSHA` (scan the slug's versions to resolve the `verNum`), then inside the
  tx point-reads `versions/<slug>/<NNNN>` and sets its `BlobID`.
- **`LegacyBlobSiteFile`**: reads `sites/<slug>`, sets `FileBlobs[contentSHA] =
  ref.BlobID` (allocating the map if nil), and writes the row back.

**Idempotency** (the migrate is re-runnable): if the target row already carries
`ref.BlobID` for this sha (`pasteRow.BlobID == ref.BlobID`, the matched version's
`BlobID == ref.BlobID`, or `siteRow.FileBlobs[contentSHA] == ref.BlobID`) AND the
bref is already present, the method is a no-op (returns nil without re-binding).
This makes a re-run after a mid-flight crash correct: it skips rows already
rebound. The CLI loop ALSO does a cheap pre-check (skip a row that already carries
ANY non-empty blob_id for this sha BEFORE staging) so a re-run does not mint a
fresh blobid and leak the newly-staged object; the in-method idempotency is the
backstop.

---

## 5. The single-process flow

`cmd/hostthis-blob-migrate` runs all three phases in ONE process after ONE
convergence, gated by `MIGRATE_APPLY` (false = dry-run-only; true = also apply).
Splitting the phases across separate pod-restarts is unsafe: a founder-only
restart re-cold-starts the ring asymmetrically (the joiners still hold the units
at bumped epochs), which fence-storms the cross-shard scan ("detected newer DB
client"). One converged process avoids that entirely.

On boot the founder FIRST waits for the ring's ownership map to SETTLE before any
scan: a cold migrate ring boot-defers units whose serving markers a prior tenant
(the old serving cluster just scaled to 0) still holds, then reconcile hands them
off over the next intervals; scanning during that churn races a peer's fence. The
gate polls the mounted-unit set until it is stable across several intervals, with
a bounded retry on the transient fence as a backstop. Then:

- **dry-run** (always): enumerate every record-blob from the metadata, confirm
  each referenced `<sha>` exists in the OLD `hostthis-blobs` bucket, report the
  count + any missing-blob drift. NO writes. Fail-closed on drift.
- **migrate** (only if `MIGRATE_APPLY=true`): the real copy. For each record-blob:
  skip if the row already carries a blob_id for this sha (idempotency pre-check);
  else GET the old `<sha>` bytes, `StageBlobStream` into the collocated bucket,
  `RebindLegacyBlob`. Bounded concurrency across DISTINCT slugs (distinct shards,
  no same-owner CAS self-contention); never parallelize within one slug.
- **verify** (only if applied): the zero-loss gate. Re-enumerate independently;
  for every record-blob re-read the row's NEW blob_id, `GetBlobStream` from the
  collocated bucket THROUGH the cluster (proves the bref routed + R=2-replicated),
  DECODE (peel magic+zstd), recompute the content-sha over the decoded bytes,
  assert it equals the record's `ContentSHA`, AND fetch the OLD `<sha>` object and
  assert byte-identical. Exit non-zero on ANY missing blob_id, missing bref,
  GetBlob failure, sha mismatch, or body mismatch.

The phases run on the FOUNDER (the seed pod identity) so a single writer drives
the re-key; its `RebindLegacyBlob` routes + R=2-replicates through the migrate
ring, and the other migrate pods hold the ring + apply the forwarded replica
writes. To inspect before applying, run once with `MIGRATE_APPLY=false` (the
founder dry-runs and holds), then bring the ring up again with `true`.

---

## 6. The cutover runbook (read-down)

The migrator writes blobs + binds while the OLD `hostthis-blobs` bucket stays
RETAINED + untouched (rollback safety). The cutover is READ-DOWN: take the app
down, migrate over the quiescent buckets, bring the blob-enabled app back up.

**Preconditions.** The sharded R=2 metadata cluster holds all rows (empty
`blob_id` fields). The blob-enabled image (this branch, the staging-validated
tag) is BUILT + pushed (and carries the temp migrate binary) but is NOT yet the
prod serving image. The collocated blob bucket `HOSTTHIS_SHALE_BLOB_BUCKET`
exists in MinIO (created, empty).

```
# 1. QUIESCE THE APP (read-down). Scale the app StatefulSets to 0 so NO node holds
#    any unit's slatedb open. There is NO "dry-run while the app is up": the migrate
#    ring opens the SAME units (a scan fences the live writer), so the FIRST migrate
#    boot of any kind happens AFTER scale-to-0. The migrate then runs as the app's
#    OWN node identities (hostthis-shard-seed-0/-0/-1) over the QUIESCENT prod
#    buckets; the public ingress serves nothing during this window.

# 2. INSPECT (MIGRATE_APPLY=false). Bring up the 3-pod migrate ring. The founder
#    converges, dry-runs (read-only), reports "records=N present-in-old=N
#    missing-in-old=0", and HOLDS. STOP if N==0 or missing-in-old>0 (drift).

# 3. APPLY (MIGRATE_APPLY=true). Tear the inspect ring down, bring it up again with
#    APPLY=true: the founder converges ONCE and runs dry-run -> migrate -> verify in
#    that single process. migrate stages + rebinds every record-blob at R=2; verify
#    (independent re-enumeration) reads each NEW blob_id THROUGH the cluster, decodes,
#    recomputes sha, and byte-compares to the OLD <sha> object. Idempotent +
#    resumable: on a mid-flight crash, re-apply - the per-record skip + in-method
#    idempotency make a re-run correct. DO NOT PROCEED until "verify GREEN".

# 4. FLIP + UN-QUIESCE. Patch the container command back to `hostthisd` (or scale
#    the StatefulSets back up on the blob-enabled image), with the prod overlay
#    pointing at the blob image tag AND HOSTTHIS_SHALE_BLOB_BUCKET set in the env.
#    The app pods open the SAME unit DBs at a strictly-higher epoch and recover the
#    migrate cluster's flushed tail.

# 5. SMOKE (live verify):
#    - read an OLD (pre-migration) paste: migrated blob_id -> GetBlob -> 200
#    - read a migrated SITE: FileBlobs -> GetBlob -> 200
#    - upload a NEW paste, read it back: the READY-direct collocated path -> 200

# 6. KEEP THE OLD BUCKET. hostthis-blobs stays UNTOUCHED as the rollback anchor for
#    a grace window (days). GC it only after confidence (step 7).
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
image + old config." The migrate cluster's mounts bumped each unit's fence epoch;
the rolled-back app simply opens at the next-higher epoch (fence is always
"strictly above durable"), so a higher epoch is never a problem for a re-open.

**The freeze is just "app quiesced."** With read-down there is no
`HOSTTHIS_READ_ONLY` flag, no dual-read fallback, no serving-path app change. The
migrate process is the ONLY thing touching the buckets between step 1 and step 4,
so there is no concurrent-writer race at all.

---

## 7. Idempotency, resume, and the verify pass

### 7.1 Idempotency + resume

- **Per-record skip** (primary): the cheap pre-check (the row already carries a
  non-empty blob_id for this sha -> skip BEFORE staging) makes a re-run nearly free
  and, crucially, keeps a re-run from minting a fresh blobid + leaking the
  newly-staged object. This is the main idempotency guard.
- **In-method idempotency** (backstop): inside `RebindLegacyBlob`, if the row
  already carries `ref.BlobID` AND the bref exists, return nil (no-op). Makes even
  a partially-applied record safe.
- **Resume**: an append-only progress manifest `(slug, kind, verNum, sha, blobid,
  status)` to a MinIO object or a PVC file lets a restart skip completed records.
  Even without it, the per-record skip makes a cold re-run correct (just slower).
  The manifest is an optimization + audit trail, not a correctness requirement.

### 7.2 The verify pass

The `verify` mode is the INDEPENDENT zero-loss reconciliation, run on a FRESH
migrate cluster (so it proves the writes survive a full close+reopen, exactly as
the app's restart will). For every record-blob:

1. Re-read the row from the cluster (`ResolveBlobID` / a routed Get), pull its NEW
   blob_id.
2. `GetBlobStream(routeKey, blobid)` from the collocated bucket THROUGH the
   cluster (so it proves the bref routed + R=2-replicated, not just object
   existence).
3. DECODE (peel magic+zstd), recompute sha256 of the DECODED bytes, assert it
   equals the record's `ContentSHA`.
4. ALSO fetch the OLD bucket's `<sha>` object, decode, assert byte-identical to
   the new (a direct old-vs-new body compare).

Exit non-zero on any missing blob_id, missing bref, GetBlob failure, sha
mismatch, or body mismatch. The verify does NOT trust self-reported counts; it
re-derives the keyset from the metadata and re-reads every blob.

**R=2-consistency note.** The verify cluster runs `ReadConsistency=ReadQuorum`
(the prod default, set in `NewShaleRepo`), so each `GetBlob`'s bref read is a
quorum read across BOTH replicas of the unit. So verify GREEN proves the pointer
is present on a quorum (both, at R=2) - the property a single-node migrator would
have FAILED, and the gate that catches any regression toward that shape.

---

## 8. Top data-loss risks + mitigations

**Risk 1 - the migrator never converges (the pivot's original failure).** A
migrator built on a bare cluster with foreign node IDs boot-defers the data's
serving markers and never converges, so the migration cannot start.
**Mitigation: run AS hostthis via `NewShaleRepo` with the app's OWN node identity
(section 0/2). It reclaims its own markers via the proven mass-restart path - no
foreign-marker defer, no hand-rolled convergence wait.**

**Risk 2 - the single-node R=2 trap.** A lone migrate process with a one-member
ring writes only one replica position per unit, leaving the second stale; shale
does NOT backfill a whole replica position. **Mitigation: the migrate runs as the
SAME R=2 multi-node ring the app forms (BindAddr/Seeds set, all migrate pods up),
so every write fans to both positions through the cluster's own routing. The
verify pass reads every bref back via a quorum read at R=2, which can only pass if
both replicas carry the pointer.**

**Risk 3 - the old bucket is mutated/deleted before verify passes.**
**Mitigation: the migrate opens `hostthis-blobs` READ-ONLY (Get only), the runbook
RETAINS it untouched through step 6, decommission (step 7) is a separate gated
post-grace action, and the verify compares old-vs-new bodies directly so it cannot
pass unless the old bytes still exist + match.** Rollback = redeploy old image +
old config; old bytes always intact.

**Risk 4 - a writer races the migrate.** With read-down (app quiesced) the migrate
process is the ONLY thing touching the buckets, so there is no concurrent app
write and no same-slug CAS contention from the app. The migrator's own concurrency
is bounded + spread across distinct slugs (distinct shards), so it never
self-contends one owner's CAS.

**Risk 5 - two writers hold a unit at once (split-brain).** Could happen if the
app were NOT fully down (a straggler) while the migrate opens the same units.
**Mitigation: step 1 quiesces the app and waits for the pods to be GONE (or patches
the SAME pods, which cannot double-open their own units) before the migrate runs.
slatedb's writer-epoch fence is the backstop, but the runbook relies on the app
being verifiably down, not on the fence.**

---

## 9. Testing on a prod-data COPY (staging-first)

The staging sharded cluster (3 pods, R=2, UnitCount=16) runs the blob image
(`HOSTTHIS_SHALE_BLOB_BUCKET` set). The prod-copy-into-staging pattern extends to
phase 4:

1. **Seed staging with old-shaped rows + old blobs.** Mirror a representative
   slice of prod into the staging MinIO: the prod `hostthis-metadata` sharded unit
   DBs AND the prod `hostthis-blobs` bucket (into a staging old bucket), read-only
   on the prod side. A SAMPLE is fine (a few hundred pastes + a couple of sites +
   their blobs, PLUS a deliberately planted row referencing a MISSING blob, to test
   the FAIL path). The rows have EMPTY `blob_id` (the pre-phase-4 shape).
2. **Run the FULL runbook against staging**: quiesce the staging app, run the
   migrate (mode=migrate) as the staging app's node identities over the old bucket
   + a fresh staging collocated bucket, run verify, then flip to the blob image +
   smoke. Assert: every sampled record-blob ends with a blob_id + a
   routed+replicated bref at R=2, GetBlob returns byte-identical bytes, verify
   GREEN, the ACTUAL hostthis blob image serves old + new + sites after restart,
   and rollback works (redeploy the pre-phase-4 image, confirm it reads the sampled
   pastes from the old bucket).
3. **Prove R=2 on the restarted cluster** (the crux gate): after the migrate + a
   staging app restart, kill ONE staging node and confirm reads of migrated blobs
   still 200 (the surviving replica carries the pointer).
4. **Measure the migrate window** on a 2 GB box (per-blob PUT latency * blob count
   / write concurrency) to size the prod read-down window before scheduling.
5. Only then schedule the prod cutover (operator go).

Because staging shares the box class + the MinIO topology with prod, the staging
run de-risks the CAS contention, the window estimate, the routing correctness, AND
the R=2-completeness against real-shaped data before prod. Critically, because the
migrate runs as hostthis ITSELF (the same `NewShaleRepo` wiring + node identities),
the staging run exercises the EXACT convergence path the prod migrate will - which
the deleted separate-tool approach could not, and is why it failed only on staging.
