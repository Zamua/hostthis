# What "all in on celld" would actually look like

Written in answer to the question, not as a recommendation. Everything here is
either measured tonight or marked as unmeasured.

## The hard floor: two things can never move

**SSH stays a Go process.** celld exposes a Worker HTTP port and a peer port and
no TCP listener of any kind. hostthis's entire interface is `ssh hostthis.dev`.
So a Go process terminates SSH in every version of this, and it talks to cells
over HTTP. No rewrite removes that; it is a property of the runtime.

**Bytes stay in object storage.** celld holds no blobs - `r2_buckets` methods
throw. The cell stores references and the bytes live where they live now.

So "all in" is not "hostthis becomes a Worker". It is "hostthis's metadata plane
and its coordination become cells, behind a thin SSH shim".

## What moves in

| today | on celld |
| --- | --- |
| shale units, placement, epochs | cells, owned and fenced by celld |
| durable intent log in shale KV | same log, in the identity cell |
| owner document | identity cell's maintained summary |
| keygate rows | subnet cells |
| room state + `internal/relay` fan-out | room cells with hibernatable WebSockets |
| static site manifests | site cells; files served as bucket assets |
| boot sweep for expiry | per-cell alarms |
| create orchestrated from hostthisd | orchestrated inside a cell |

## What gets deleted

This is the substantive part, and it is large.

- **`internal/relay` + `relay/relaygrpc`, about 2,025 lines.** It exists to fan
  room state between pods because no pod owns a room. celld makes that problem
  not exist.

  Checked rather than assumed, because the argument depends on it: the committed
  manifest is `replicas: 3` with a rolling update, prod runs three pods across
  three nodes, and the peer transport is live (`HOSTTHIS_SHALE_GRPC_ADDR`, plus a
  headless peers service 66 days old). So the relay is required by the deployed
  topology. NOT established: that rooms carry enough cross-pod traffic for the
  fan-out to be busy. "Necessary for correctness" and "carrying load" are
  different claims and only the first is evidenced.
- **The shale-specific half of `internal/storage`**, about 4,920 lines by the
  brief's count: unit mounting, epoch fencing, placement, the CAS coordinator
  seam, tombstone handling, the acquiring-window retries.
- **The shale dependency itself**, and with it slatedb, the Rust FFI, and the
  `-tags slatedb` build split that shapes the test suite.
- **Much of the storage test volume**, 10,843 lines today, a lot of which pins
  distributed-KV behaviour a single-owner cell does not have.

Roughly 7,000 lines of distributed-systems code we currently maintain.

## What gets added

- The cell application. Today it is ~400 lines for pastes, identities and a room
  prototype; the full surface is plausibly 1,500-2,500.
- The Go client adapter, ~700 lines today, maybe 1,200 complete.
- A second operational surface: a celld fleet, its bucket, its credential, its
  failure modes and its runbook.

Net: hostthis gets materially smaller and simpler. That is real.

## What is genuinely better

Measured tonight unless noted:

- **Write parallelism per ENTITY rather than per shard.** shale serialises every
  key on a unit against every other, with 16 units; two unrelated owners collide
  by hash. Cells cannot collide. The gap widens with more users.
- **Dormant data is nearly free** - an evicted cell is a bucket object. Verified
  that eviction works, and that it needs `CELLD_IDLE_EVICT_S` set.
- **Alarms replace boot sweeps.** +4 to +20 ms on evicted cells, surviving node
  restarts. Strictly better than a sweep that runs once per process start.
- **The identity cell is not a bottleneck**: ~1.2x versus scaling out, flat in N.
- **Rooms stop needing a relay.**

## What is worse, or unknown

- **Every metadata call becomes a network call.** A create is three round trips
  today; it can become one by moving orchestration into a cell, but never zero.
- **Activation cost replaces uniform latency.** shale's mounted units are warm;
  a dormant cell pays a restore. Cheap storage with an occasional cold spike
  rather than uniform cost.
- **Durability semantics differ and have NOT been compared.** shale acks at W=2
  from two replicas. celld writes the cell's SQLite and replicates. Nobody has
  measured these against each other.
- **A moved cell drops WebSockets with no close code.** Manageable, since
  reconnect must be unconditional anyway, but it is a real regression in what a
  client can know.
- **The cell taxonomy is permanent.** Class names are in the storage key path
  and `celld deploy` rejects `renamed_classes`.
- **A fleet serves one application**, replacing the previous one silently.
- **celld is alpha**, with no placement controller and, by its own docs, thin
  test coverage on cross-node WebSocket behaviour.

## The part that is not technical

Two things matter more than any of the above.

**It is a bet on celld being maintained.** Deleting 7,000 lines of our own
distributed-systems code in favour of someone else's alpha runtime is not
reversible in practice. If celld stalls, the position is much worse than having
kept shale.

**shale loses its only real consumer.** hostthis is the workload that makes
shale a system rather than a library with nobody using it. Moving off removes
the pressure that keeps it honest. That is not a hostthis engineering question
and it does not belong to this repo, but it belongs in the decision.

## If it were done, the order

1. Rooms. Biggest deletion, uses the feature celld exists for, and the
   prototype already works.
2. Create orchestration into a cell. Three round trips to one.
3. Keygate and subnet cells. Small, and the hot-subnet risk needs measuring
   first - a NAT'd university through one cell is the sharpest untested case.
4. Sites.
5. TTL onto alarms.
6. Remove shale, last, and only once every surface has run on celld under real
   traffic for long enough to trust it.

Steps 1 to 5 are all reversible. Step 6 is the one that is not, and there is no
reason to take it early.

## What would change my answer

Nothing here says celld is better than shale for this workload, because nothing
measured that. The comparison that would matter is the same load against both
backends - throughput, tail latency, and behaviour under a node loss - and it
has not been run. Until it has, "all in" rests on a maintenance argument and a
parallelism argument, both real, neither measured against the incumbent.
