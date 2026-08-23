# hostthis on celld: where the experiment got to

A verdict rather than a running total. Branch `experiment/celld`; shale remains
the default and prod is untouched.

## What runs

A celld fleet in `hostthis-staging`, own bucket on the shared staging MinIO, own
scoped credential. **31 conformance assertions pass against it**, and the suites
are UNMODIFIED - the same assertions the in-memory and shale backends pass. That
was the stated bar for the experiment and it is the only claim here that is not
a measurement of celld itself.

| surface | state |
| --- | --- |
| `durable.Log` | complete, 11 assertions |
| paste create lifecycle | complete, 7 assertions |
| owner index and quota | complete, 9 assertions |
| `PasteAdmin` (16 methods) | complete |
| rooms | measured, not ported |
| sites, keygate | shale only |
| blob plane | stays on shale; celld holds no bytes |
| SSH | stays Go; celld is HTTP/WS only |

## What the experiment established that was not known this morning

Each of these is measured, and several contradict what we believed when we
started.

**About celld:**

1. **Alarms fire on evicted cells**, +4 to +20 ms, and survive a node restart.
   The documented `setTimeout` gap does not apply to durable alarms.
2. **The 0.1.0 timer/event-gate bug is gone in 0.3.0** - measured by
   boardtogether with a 0.1.0 control that reproduced the original failure.
3. **A moved cell drops its WebSocket with NO close code.** Reachability through
   the peer tunnel is not connection survival. Clients must reconnect
   unconditionally; nothing can branch on a status.
4. **A connected room costs ~0.45 MB**, stable and reclaimable. Rooms are not a
   capacity fork.
5. **Activation dominates serialization at this scale.** Many cells waking is
   more expensive than one cell being busy - the reverse of the hot-cell
   intuition, and the regime a dormant-heavy paste service lives in.
6. **Idle eviction works but is SILENT**, and is off by default since 0.2.0.
   Without `CELLD_IDLE_EVICT_S` the dormant-cells-are-cheap argument fails.
7. **A fleet serves ONE application, and a second deploy replaces it silently.**
   A real hazard: it would have taken boardtogether's staging down.
8. **Class names are in the storage key path and `renamed_classes` is rejected**,
   so the cell taxonomy is permanent once data exists. Temporal's immutable
   shard count did not disappear; it moved.
9. **MinIO supports `If-None-Match`** - verified on all three instances, so
   celld's ownership records are safe here.
10. **The identity cell is not a bottleneck**: ~1.2x versus scaling out, and
    FLAT in N, so it is a fixed overhead rather than a queue.

**About hostthis, found by trying to port it:**

11. The service branched on a backend capability (`IsTransactional`) in three
    places, producing a different observable state machine per backend. Removed.
12. Slug reservation was a shale mechanism in a domain-shaped contract. Moved
    below the port.
13. The conformance suite covered NEITHER protocol-specific half, so a new
    backend could pass everything while its create lifecycle went unverified.
    Nine assertions added; shale passes all of them, which is what makes them
    the contract rather than an invention.

Findings 11 to 13 are improvements to hostthis that hold whether or not celld
ever ships. They are the part of this experiment that has already paid.

## What would need deciding before this went further

1. **Continue or stop.** Sites, rooms and keygate remain. Nothing found so far
   argues against continuing; equally, nothing requires it.
2. **`CELLD_IDLE_EVICT_S` is inherited, not chosen.** It is 300 because
   boardtogether picked 300 for a different workload, and since activation
   dominates it is now the main cost dial. Both directions cost something and
   the curve has not been swept.
3. **The upstream WebSocket report is written and NOT sent.** celld disables
   pull requests and asks for a patch by email, so it is person-to-person
   contact and therefore the operator's call.
4. **Unrelated, but it will bite:** `TestRoomWireValue/invalid_utf8` fails on Go
   1.27 on `main` today. CI is green on an older toolchain. Someone should own
   it before CI upgrades.

## What this does not claim

One node, so no peer tunnel and no cross-node hop in any latency number.
Trivial payloads. Concurrency to 32. Minutes, not days. And celld is alpha - the
version is pinned by digest for that reason.

The experiment shows the port is FEASIBLE and that the ports are honest enough
to carry a backend they were not designed for. It does not show that celld is
better than shale for this workload, and no measurement here was aimed at that
question.
