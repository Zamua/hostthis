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
