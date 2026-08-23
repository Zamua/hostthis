# Port audit, before any celld adapter

Step 1 of `celld-migration.md`: audit the ports and the conformance suites, and
report what leaks before writing an adapter.

**Verdict: the ports are domain-shaped and no backend package leaks above
`internal/storage`, so the brief's read is right. But the transactional /
detached split is worse than "a capability difference expressed as branching",
and the conformance suite does not cover it. Passing the suite unmodified would
prove less than the brief assumes.**

---

## What holds

- **No backend import above `internal/storage`.** Verified across
  `internal/service`, `internal/http`, `internal/ssh`, `internal/domain`,
  `internal/render`, `internal/archive`. Zero hits for `Zamua/shale`. The
  invariant `deploy_site.go` states is true today.
- **The ports are domain-shaped.** `PasteAdmin`, `PasteRepo`, `SiteRepo`,
  `RoomRepo`, `KeyGateRepo` speak `domain.Slug`, `domain.Paste`,
  `domain.Identity`, quota caps and timestamps. No shard, epoch, transaction,
  unit or KV vocabulary anywhere in a service-layer interface. A celld adapter
  can satisfy these shapes.
- **The entity partitioning is real.** Every scan is scoped to one slug,
  identity, subnet or scope, so the cell boundaries the brief proposes are
  already drawn by the shard-key function.

## Finding 1: `IsTransactional()` selects between two protocols, not two tunings

The service branches on it in **three** places, and each branch is a different
algorithm rather than a different setting.

| site | transactional | detached |
| --- | --- | --- |
| `upload.go:193` | commit READY directly, no pending window, no finalizer | insert PENDING, return the URL, write the blob in a background goroutine, then `MarkReady` |
| `deploy_site.go:189` | pre-claim the slug BEFORE the untar, with deferred compensation to release it | no pre-claim |
| `deploy_site.go:253` | one commit attempt, slug already held | mint the slug in a retry loop AFTER the untar |

The upload split is the significant one: it is a different **observable state
machine**, not an implementation detail. `PasteStatusPending` exists only on the
detached path, and `internal/http/server.go:249` branches on it to serve a
loading page. So the same user action produces a different sequence of
observable states depending on which backend is wired in, and the choice is made
in the service layer.

celld lands on the detached side, so the experiment inherits the pending window,
the background finalizer and the loading page.

## Finding 2: the ports are the union of both protocols

This is the sharper form of Finding 1, and the brief did not name it.

- `PasteRepo.MarkReady` / `MarkFailed` exist **only** for the detached protocol.
  The transactional path never calls them (`upload.go:234` commits ready
  inline).
- `SiteRepo.PreClaimSlug` and `SlugClaimReleaser.ReleaseSlugClaim` exist
  **only** for the transactional protocol. The detached path never calls them.

So neither backend implements the whole port meaningfully. The port is the union
of two write protocols and `IsTransactional()` selects which half is live. A
third adapter does not extend a contract; it picks a side.

That is the thing to decide deliberately before writing celld, because celld
picking "detached" means adopting `MarkReady`/`MarkFailed` and leaving
`PreClaimSlug` unimplemented or returning an error, which is a port lying about
its own surface.

## Finding 3: conformance covers neither protocol-specific half

The suite is the stated proof of layering. It does not test the part that
differs.

- `MarkReady`, `MarkFailed`, `PasteStatusPending`: **zero** occurrences in
  `conformance_test.go`, `conformance_sites_test.go`, `conformance_rooms_test.go`.
- `PreClaimSlug` is covered only by `slug_claim_test.go`, a backend-specific
  test, not by conformance.

A celld adapter could therefore pass all three suites unmodified while its
pending window, its finalizer, its loading-page transition and its reconciliation
of abandoned PENDING rows went entirely unverified. **The green would be real
and would mean less than it looks**, on precisely the surface celld changes.

Recommendation: before the adapter, extend conformance to cover the lifecycle
that both protocols must produce, stated in observable terms rather than in
either protocol's vocabulary. Something like "after a successful create, a read
eventually observes READY, and never observes a state from which it cannot
reach READY". Both backends must satisfy it; how they get there stays theirs.

## Finding 4: the suite already concedes capability differences

`conformCaps` gates three assertions on backend strictness
(`StrictQuotaUnderConcurrency`, `StrictIdentityQuotaUnderConcurrency`), so "the
same suite, unmodified" is already a softer bar than it sounds: an adapter can
decline a quota-strictness assertion by declaring itself non-strict.

For celld this is good news rather than bad. A cell is single-threaded with
synchronous storage, so a check-and-write inside one cell is atomic by
construction. celld should pass at the **strictest** setting, which is stronger
than shale manages today (`conformance_local_test.go` sets
`StrictIdentityQuotaUnderConcurrency: false`). Worth asserting deliberately
rather than inheriting `false` by copy-paste.

## Finding 5: the topology reproduces the condition that forced `internal/durable`

Not in the brief, and it changes the adapter's scope.

hostthis keeps a durable intent log because its two writes cannot co-locate:
`pastes/<slug>` routes on the slug, while `identity_pastes/<id>/<slug>` and
`owner_doc/<id>` route on the identity. The split is forced by the READ paths,
since a reader holds a slug and not the owner, and the index reader holds an
identity and not the slugs. No hash tag fixes it, so no single transaction spans
the operation.

The proposed cell topology has exactly the same shape: `paste:<slug>` and
`identity:<id>` are **different cells**, and celld has no cross-cell
transaction. Creating a paste writes to both.

So celld does not remove the need for `internal/durable`; it reproduces the
condition that requires it. The intent log ports across cleanly: `Scope` is
already the owner, which maps onto the `identity:<id>` cell, and recovery stays
a single-entity read.

Two consequences for the plan:

1. The celld adapter needs a `durable.Log` implementation, and it now has a
   conformance suite of its own to satisfy
   (`conformance_intentlog_test.go`, added this week).
2. celld **alarms** are a better fit for intent resolution than the current
   boot sweep. Today resolution runs once per process boot, which leaves
   intents on units acquired later waiting for a reboot. A per-cell alarm is a
   durable timer scoped to exactly the entity that owns the intent.

## Finding 6: one port still takes a whole payload

`BlobStore.PutPrecompressed(sha string, body []byte)` takes the bytes whole,
which is the last `[]byte` payload on a service-layer port after this week's
constant-memory work. It matters less for celld, since blobs stay in the object
store either way, but a new adapter should not be written against it as though
it were the contract.

## Recommended order, revised

1. Decide Finding 2: does the transactional / detached split belong in the
   service or behind the port. Everything else depends on the answer.
2. Close Finding 3: extend conformance to the create lifecycle, in observable
   terms, and confirm **shale** still passes. That is the sabotage check on the
   new assertions.
3. Then stand up celld and write the adapter, with the intent log in scope from
   the start rather than discovered at integration.

Doing 3 before 2 means the suite cannot tell whether the adapter is right.

---

# Measured: do durable alarms fire on an idle cell?

The audit recommended alarms for intent resolution and TTL expiry. That advice
was unverified, and `infra/boardtogether/celld/README.md` documents a timer gap
that would have invalidated it: timers polled only while an event runs, so one
coming due on an IDLE cell fires on the next event rather than on time. A
dormant paste cell is exactly that case.

Boardtogether could not answer from operations - their cells heartbeat every 25s
so they have never exercised an untouched alarm - but they made the right
distinction: the README gap is about JS `setTimeout`, while the proposal rests
on durable `ctx.storage.setAlarm`. Different machinery, so it needed measuring
rather than inferring.

**Measured against stock v0.3.0** (the digest the fleet pins), a probe cell whose
`alarm()` handler records `firedAt` in its own storage. Recording inside the
handler is the discriminator: observing from outside necessarily wakes the cell,
so only the handler can say when it actually ran.

| case | config | due -> fired |
| --- | --- | --- |
| idle cell, 60s alarm | evict 5s | **+6 ms** |
| slow orphan scan, 30s alarm | evict 5s, waker tick 30s | **+7 ms** |
| far alarm, residency defeated | evict 5s, residency 1s, waker tick 5s, 300s alarm | **+4 ms** |
| node killed mid-flight | as above, 90s alarm, node down ~10s across the window | **+20 ms** |

Every observation happened 30 to 45 seconds AFTER the fire, so none of these is
the alarm being triggered by the act of looking.

**Conclusion: durable alarms fire on time on evicted cells, and survive a node
restart. The alarms recommendation stands.** Three things it is worth being
precise about:

- The near-alarm residency window does not explain it. Forcing
  `CELLD_ALARM_RESIDENT_MS=1000` against a 300-second alarm still fired at +4 ms.
- The orphan scan interval does not bound it either. A 30-second
  `CELLD_WAKER_TICK_MS` still produced +7 ms, so the waker is not a coarse poll
  that fires late by up to a tick.
- An alarm armed before a node dies still fires after a different process picks
  the cell up. That matters because a 7-day TTL will span deploys by
  construction.

**Limits of this measurement.** One node, one cell, local MinIO, minutes rather
than days. It establishes the mechanism, not behaviour under fleet load or over
a real TTL horizon. It also says nothing about the JS `setTimeout` gate, which
is a separate question that still needs measuring on v0.3.0 before anything is
built on WebSocket or RPC paths.

---

# Deployment requirements the adapter cannot choose for itself

Learned from `infra/boardtogether/DEPLOY.md` and from the boardtogether agent,
who have run celld in production since 2026-08-13. Copied rather than derived.

- **`CELLD_IDLE_EVICT_S` must be set explicitly.** 0.2.0 stopped evicting idle
  cells by default, so without it a cell never scales down on a timer: every
  paste anyone touches stays resident at roughly 8 MB until the node hits its
  memory-pressure threshold and sheds the LRU idle ones. That is a slow leak
  with a cliff, not hibernation, and it undercuts the "dormant pastes are nearly
  free" argument that motivated the port. 300 is the value boardtogether landed
  on. Verified indirectly here: the alarm probe ran with 5 and eviction did
  happen, so the mechanism works when configured.
- **Pin the image by digest**, as the fleet already does.
- **:8080 for ingress, :8081 peer-only behind a NetworkPolicy** that must never
  be exposed. Credentials in a Secret.
- **A fleet serves ONE application.** hostthis gets its own bucket rather than a
  prefix on boardtogether's: a prefix isolates the deploy pointer, because
  `deploy/current.json` is relative to it, but NOT the credential. Their staging
  user is scoped to the whole bucket, so a shared prefix would share write
  access with anyone holding that key.

**Rooms will not scale down.** celld does not shed a cell with active work or a
live host WebSocket, and an outbound WebSocket pins its cell resident. A room
cell therefore holds its memory for as long as anyone is connected. That is
correct behaviour, and it means rooms are the part of hostthis whose footprint
tracks concurrent users rather than dormant content.

For sizing: the celld pod serving boardtogether in production sits at 7m CPU and
34Mi against a 640Mi limit, so the runtime is cheap. Resident cell count is what
moves the number, at roughly 8 MB each.

---

# Measured: what rooms cost, and what happens to a client when the cell moves

Rooms were measured before the remaining adapters because they are the only
part that could send the port back to the topology: celld does not shed a cell
with a live host WebSocket, so a room holds memory for as long as anyone is
connected. Probe is a `Room` Durable Object using HIBERNATABLE (inbound)
sockets, in `celld/src/index.js`, driven from `internal/celld/roomprobe_test.go`.

## Resident cost: about 0.45 MB per connected room

| | pod RSS |
| --- | --- |
| baseline, no rooms | 32 Mi |
| 100 connected rooms | 78 Mi |
| after disconnect, ~10 min | 45 Mi |
| a SECOND 100 rooms | 90 Mi |

The second batch is what makes the first meaningful: +46 Mi then +45 Mi, so the
per-room cost is stable and the memory from the first batch was largely
reclaimed rather than retained. **Roughly 0.45 MB per connected room**, well
under the ~8 MB/cell figure quoted for celld generally.

One honest limit: these rooms hold almost no state, so a real room carrying its
KV would cost more.

**The reclamation is idle EVICTION, not garbage collection, and it is silent.**
RSS drifts back over minutes with no eviction lines in the log, which leaves two
candidate mechanisms. They are distinguishable in the bucket: a cell's
`cells/<Class>:<id>/own.json` names the node holding it.

    Room:      50 of 50 sampled -> {"node": "", "epoch": 2}   UNOWNED
    IntentLog: 13 of 13 sampled -> {"node": "", "epoch": 1}   UNOWNED

Every idle cell had been fenced and published unowned. So `CELLD_IDLE_EVICT_S`
does what it claims, the dormant-cell argument for the port holds, and the only
gap is that eviction is not logged at the default level. Slow-looking RSS is the
allocator, not retained cells.

(Reading those objects needs the `:` in the key percent-encoded in the SigV4
canonical URI; sending it raw returns `SignatureDoesNotMatch`, which looks like
a permissions problem and is not.)

**Rooms are not the memory problem the brief feared.** At this cost, a node
holds thousands of connected rooms before memory is the binding constraint.

## A moved cell closes the client's socket with NO close code

The reading that matters, and the one upstream's limitations page flags as its
thinnest coverage.

    client observed on move: status=StatusCode(-1)
                             err=failed to read frame header: EOF

Measured twice, and the second time WITHOUT the confound that invalidated the
first. The obvious method - port-forward to a pod, kill that pod - cannot
distinguish celld closing the socket from the port-forward dying with the pod.
So the real measurement connects the client through pod A, kills pod B, and
confirms pod A is still serving afterwards (`HTTP 200`). The client's own
ingress was healthy throughout and its socket still died.

Three consequences:

1. **The socket does not survive the cell moving**, even when the client's
   ingress node is alive. "Each node can be the WebSocket ingress for each cell
   through the peer tunnel" does not mean the connection survives a handoff.
2. **There is no close code to branch on.** A client sees a bare EOF, not 1001
   or 1012. Any reconnect story keyed on a close code would silently never fire.
3. So reconnect must be **unconditional on any disconnect, with backoff**. That
   is what hostthis's existing WebSocket lifecycle already does, so rooms on
   celld need no new client behaviour - but a design that assumed a clean close
   would have been wrong.

A rolling deploy therefore drops every room connection. With one replica that is
unavoidable; with more, it is still per-cell rather than per-node, because the
cell moves regardless of which node the client reached.

---

# Decision: four cell classes, with a measurement behind it

The class name is part of a cell's storage key (`cells/<Class>:<id>/...`) and
`celld deploy` REJECTS `renamed_classes`, so the taxonomy is effectively
permanent once prod holds data. That makes it worth deciding deliberately rather
than inheriting from a sketch.

`Paste`, `Identity`, `Subnet`, `Room`.

**Why not one class with structured ids.** The suggestion was that ids stay
under our control while class names do not, so `paste:<slug>` in a single class
would keep future taxonomy changes cheap. It does not: `idFromName` HASHES the
name, so the key is `cells/Room:<64 hex>` and the original name is not
recoverable. Changing a naming scheme orphans the old cells exactly as changing
a class does. The two options differ only on RENAMES, which are cosmetic;
RE-PARTITIONING is a data migration under both. Since the expensive case is
symmetric, per-kind classes win on call-site clarity and per-class SQLite
separation.

**The risk that decision carries** is that `Identity` should have been two
cells. Every upload touches it twice - reserve, then confirm - and a cell is
single-threaded, so one owner's uploads serialize there by construction. If that
is a bottleneck, splitting it later is the expensive symmetric migration above.

## Measured: the identity cell is not the constraint

Concurrent inserts from ONE identity, against a control of the same N inserts
across N DISTINCT identities. The control is the measurement: rising latency
with rising N proves nothing by itself, because HTTP, the client and the node
all contend too. The DIFFERENCE isolates the shared cell's single thread.

    n     shared wall/p50/p99        distinct wall/p50/p99      ratio
    1      80ms /  80ms /  80ms       82ms /  82ms /  82ms      0.97x
    4     953ms / 344ms / 619ms      683ms / 224ms / 356ms      1.40x
    8     638ms / 443ms / 564ms      505ms / 298ms / 437ms      1.26x
    16    619ms / 559ms / 599ms      581ms / 307ms / 508ms      1.07x
    32    946ms / 637ms / 680ms      760ms / 478ms / 751ms      1.24x

**About 1.2x, and flat in N.** True serialization would compound: N requests
through one thread would cost roughly N times a single request while the
distinct arm scaled out, so the ratio would climb with N. It does not.

**Both arms are pre-warmed, and that correction changed the answer.** The first
run showed shared FASTER than distinct at every N above 1, which was a confound
rather than a result: the distinct arm was paying to activate N cold cells while
the shared arm activated one, so it measured activation, not serialization. A
discarded warm-up burst in both arms removed it.

So the four classes are committed with evidence rather than judgement. Not
claimed: trivial payloads, N up to 32, and 32 concurrent uploads from a single
identity is already well past what this service sees. And ONE NODE, which means
no peer tunnel: at two or more replicas the identity cell may be owned by a node
other than the ingress, adding a hop these numbers do not include. That only
matters if the fleet grows past one, but the numbers do not cover it.

## Open question: CELLD_IDLE_EVICT_S is inherited, not chosen

The side finding above is the bigger one and it changes which knob matters.
**Activation dominates serialization at this scale.** The binding cost is not
one cell being busy, it is many cells waking up - and for a mostly-dormant paste
service that is the normal regime rather than an edge case.

That makes `CELLD_IDLE_EVICT_S` the main cost dial rather than a memory setting,
and it is currently 300 because that is what boardtogether picked for a
different workload. Both directions cost something real:

- **shorter**: less resident memory, but MORE activations, and activation is the
  dominant cost on exactly the dormant-heavy pattern hostthis has.
- **longer**: fewer activations, but more resident cells and a slow climb toward
  the 80% pressure threshold, where celld sheds under duress rather than on a
  timer.

There is a minimum on that curve and we have both instruments to find it: the
eviction check reads ownership without activating anything, and the concurrency
harness measures activation cost directly. Sweeping the window against a
dormant-heavy access pattern would locate 300 on it. 300 may well be fine; the
point is that nobody has checked.

---

# The quota is reconstructible, and that is worth protecting

One idea has now been applied three times in this adapter, and it is worth
naming as one rather than three:

- **membership over arithmetic** for release: "this slug is no longer counted"
  rather than "subtract N", so a replayed release cannot under-charge
- **absolute over incremental** for the charge: a version write sets the entry's
  size to the paste's new total rather than adding to it, so a replayed write is
  a no-op
- **readable state over invisible mechanism** for the eviction check: assert on
  ownership records in the bucket rather than trust a process that logs nothing

The consequence, which follows from the second and was not designed for:
**the charged total is a FUNCTION of the paste cells' current contents, not an
accumulated history.** It cannot drift irrecoverably, because it can be
recomputed: read each entry's slug, sum the retained bytes in its paste cell,
and compare against what the identity cell stores. A divergence is a bug
findable before a user notices their quota is wrong.

That check is not built. What matters is that the PROPERTY exists, because it is
easy to destroy by accident: the moment someone optimises `touch` into an
increment for a plausible-looking reason, the total becomes a history again and
nothing announces that it happened. If that optimisation is ever proposed, this
is the thing being traded away.

## Append and DeleteVersion are not mirror images

Both are two-cell writes on the quota path, and their ORDER differs because
their failure modes do:

- **Append can be refused**, so quota is checked FIRST, in the identity cell. A
  rejected append leaves no version behind - the same shape as create.
- **DeleteVersion cannot be refused.** There is nothing to check, so the only
  question is ordering: remove the version from the paste cell, THEN write the
  new total. A crash between them leaves the owner charged for bytes that are
  gone, which is conservative and repairable by the recomputation above. The
  other order frees the charge while the bytes remain, which is the money-losing
  direction and the one nothing is watching for.

So append is check-then-write, delete is write-then-settle.
