# Experiment: hostthis on celld

A brief for an experimental branch that moves hostthis's metadata plane off
shale and onto celld, behind the existing repository ports.

This is an experiment. shale stays the default. The goal is a second adapter
that passes the same conformance suite, and a staging deployment that proves
it.

## 1. What celld is

`github.com/denoland/celld`. A Rust daemon that embeds V8 and runs Cloudflare
Workers and Durable Object bundles on your own machines. One binary plus an
object-storage bucket; no control plane, no consensus service. Nodes coordinate
only through conditional writes on the bucket.

The unit is a **cell**: a named entity with a private SQLite database.

Properties that matter here:

- **Exactly one owner per cell**, enforced by ownership records written with
  conditional writes and carrying a **fencing epoch**. Every activation
  advances the epoch, and an epoch never has two writers. The epoch is also the
  prefix of the replication key path, so the data path is fenced too.
- **Single-threaded per cell.** Two requests to the same cell never run at the
  same instant. Storage calls are synchronous, so nothing interleaves.
- **Hibernation.** An idle cell stops being resident. A cell no node owns is
  *inactive* and lives only in the bucket, costing nearly nothing. About 1,000
  resident cells per 8 GB node; under memory pressure celld fences and releases
  the least-recently-used idle cells.
- **Alarms.** Durable per-cell timers.
- **Hibernatable WebSockets.** Roughly 50k clients per cell at the default
  128 MB isolate heap.
- **Static assets** served straight from the fleet bucket, no cell involved.
- **WASM imports** are supported, and `workers-rs` (Rust) is first-class:
  `worker-build` emits a JS shim plus a `.wasm`, and celld resolves entrypoint
  and Durable Object classes through the shim.

What celld does **not** provide: blob storage (`r2_buckets` methods throw),
any TCP listener (HTTP and WebSocket only), KV, cross-cell queries, TLS
termination. It is alpha: one application deployment per fleet, the peer
protocol does not terminate TLS, and there is no placement controller.

## 2. Why hostthis fits

hostthis is already partitioned by entity. Every `scanPrefix` call site is
scoped to one slug, one identity, one subnet or one scope. There is no global
scan anywhere in the codebase. The `{slug}` and `{apex}` hash tags are already
statements that one entity's keys must co-locate.

That is a cell boundary, already drawn.

## 3. Proposed cell topology

| cell | holds | replaces |
| --- | --- | --- |
| `paste:<slug>` | paste doc, versions, versions doc, slug owner, blob owner, staged refs | `pastes/`, `versions/`, `versionsdoc/`, `slugowner/`, `blobowner/`, `staged/` |
| `identity:<id>` | owner doc, the identity's paste index, first-seen | `ownerdoc/`, `identitypaste/`, `identityfirstseen/` |
| `subnet:<cidr>` | keygate rows | `keygate/`, `keygateidentity/` |
| `room:<app>/<id>` | room state, room values, WebSocket clients | `room/`, `roomvalue/`, `roomcreate/`, `roombytes/` |

Blobs stay where they are. celld holds no bytes; the cell stores references and
the bytes come from the existing object store.

## 4. The two hard parts

**SSH cannot move.** celld serves HTTP and WebSocket only. The entire hostthis
interface is `ssh hostthis.dev`, so `internal/ssh` stays a Go process and
becomes a *client* of celld. That introduces a network boundary where today
there is an in-process call. Budget for it: latency, retries, and error
translation across a wire that did not exist.

**No transactional blob binding.** shale binds the blob write to the metadata
write. celld cannot. The celld adapter therefore lands on the existing
detached-store path: stage refs, commit metadata, reconcile, sweep orphans.
That machinery already exists for the local and slatedb backends.

## 5. Hot cells: the write paths, not the read paths

One cell is one thread and there are no replicas, because sole ownership is the
design. shale's answer to a hot key is R>1 with `Nearest` reads; celld has no
such answer.

Hot **reads** are already handled: a CDN fronts paste delivery, and bodies
should continue to be served as static content rather than read through a cell.
Keep that true and read load is a non-issue.

The risks that survive a CDN are all on the write and session paths, in
descending order of concern:

1. **The subnet keygate cell.** `KeyGate.Admit` runs once per SSH session,
   before any verb dispatch, keyed by IP subnet. Subnets are shared
   involuntarily: a university, an office behind NAT, a CGNAT range. One cell
   per subnet puts every session from that network through a single thread
   before any work begins. In shale this is one key on one shard that can
   serve concurrently and, at R>1, from more than one node. **This is the
   sharpest regression risk in the migration.** Consider sharding the cell
   name below the subnet, or keeping keygate on a different store.
2. **Room cells.** Live WebSocket traffic that no CDN can absorb, serialized
   per room. Probably acceptable, since a room is a small group by nature, but
   it should be measured rather than assumed.
3. **The identity cell.** Every upload touches it for quota and the paste
   index. A script or CI key doing bulk concurrent uploads serializes there.
4. **Paste cell writes.** These serialize, but a paste has one owner pushing
   to it, so serialization is correct rather than a bottleneck.

Related asymmetry worth measuring: waking an inactive cell pays a restore from
the bucket, and a CDN miss on a dormant paste is exactly when that happens.
shale has no per-key activation cost. Expect cheap storage with an occasional
cold-start spike at the p99, rather than shale's uniform latency at uniform
cost.

## 6. What scales better, and what does not

- **Many pastes: celld.** A dormant paste is an inactive cell in a bucket
  costing nearly nothing. In shale every key sits in a mounted unit on a live
  node. A paste service is overwhelmingly dormant, so this is the strongest fit.
- **Many users: celld.** A user's paste list is a KV prefix scan today. In a
  cell it is an indexed SQLite query, with real ordering, pagination and
  counting.
- **Many versions of one paste: neither distributes it.** Both co-locate a
  slug's versions by construction. celld lists them better; shale can serve
  them from a shard that is concurrently serving other slugs.

## 7. The layering requirement

The service layer must not learn that celld exists. Today's structure is
already close, and the audit should confirm rather than assume:

- The ports live in `internal/service`: `PasteRepo`, `SiteRepo`, `RoomRepo`,
  `KeyGateRepo`, plus `BlobStore` and `BlobUnit`. They are domain-shaped
  (`InsertWithQuotaCheck`, `Get(domain.Slug)`, `MarkReady`), which is right.
- `deploy_site.go` states the invariant explicitly: the service never imports a
  backend package. Keep that true.
- Backend names appear above `internal/storage` only in comments. That is
  acceptable, but the comments will go stale; fix the ones that describe a
  shale-specific mechanism as though it were the contract.
- **The real leak to examine** is the split between the "collocated
  transactional" path and the "detached store" path in `upload.go` and
  `blobunit*.go`. That is a backend *capability* difference expressed as
  service-layer branching. celld lands on the detached path. Decide
  deliberately whether that branch belongs in the service or behind the port.

The proof of layering is the conformance suite. `internal/storage` already has
`conformance_test.go`, `conformance_sites_test.go` and
`conformance_rooms_test.go`, plus the `internal/storagetest` harness. **The
celld adapter must pass the same suites, unmodified.** If a suite needs
changing to accommodate celld, that is a finding about the port, and it should
be reported rather than patched around.

## 8. Scope of the experiment

1. Audit the existing ports and conformance suites against the above. Report
   what leaks before writing the adapter.
2. Stand up celld. It needs a bucket that supports conditional writes. Check
   whether the existing MinIO deployment does (`If-None-Match`); if not, use a
   separate bucket. Verify before planning around it.
3. Write the celld adapter behind the existing ports. shale remains the
   default and must keep working.
4. Pass the conformance suites with both adapters.
5. Deploy to staging and exercise it: create, update, version, delete, expire,
   deploy a static site, open a room.

Report the LOC delta. For reference, today: 20,921 non-test Go lines total,
`internal/storage` is 7,814 of them (4,920 in shale-specific files), and the
storage tests are 10,843 lines. Much of that test volume pins distributed-KV
behaviour that a single-owner cell does not have.

## 9. Conventions

Terse single-line Conventional Commits, no body. No em dashes anywhere. Branch
rather than committing to `main`. Merge with squash or rebase, never a merge
commit. Cut the semver tag before deploying anything, staging included. Scope
test runs to the affected tests; run the full suite once at the end rather than
in every step.
