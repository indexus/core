# Cold reconfiguration: balloons, transition planning, preemption

> **Conception historique — contexte du 25 novembre 2026.** Cette proposition
> est conservée ; ses grants et annonces causales ne décrivent pas le nouveau
> contrôle par [tickets locaux](local-permissions.md). Le drain des slots,
> la migration des exceptions et le reçu terminal de retraite sont explicités
> dans ce contrat et sa [qualification](local-permissions-verification.md).

**Statut : proposé.** Seul le repli d'une paire calme (COLD-2) est du code ; le reste est une conception.

This document defines the reference cold-reconfiguration protocol used by the simulator.
Local durable `core` implements an opportunistic **pair fold** (COLD-2), on by
default in the node binary:
seal the source's incoming acquisitions, pin one survivor, transfer installed
authority and bytes, publish a causal retirement, and obtain a durable witness.
The general balloon `ADD / MOVE / packing` executor and skeleton W1 remain unwired;
the unwired kernel reference code (`PlanBalloon`, `BuildTransition`, `ColdReconfiguration`) was removed on 2026-11-13, so the balloon sections below are design, not code.
The simulator's optional global oracle is an observer unless explicitly enabled
as a trial gate; oracle-gated results are not P2P execution guarantees.
It is deliberately separate from the hot path: balloons optimize a stable region, but they
never own data, never grant write authority, and never directly mutate a zone.

## 1. Roles

Indexus separates four responsibilities:

```text
Balloon              finds a better final regional state
    ↓
DesiredTopology      V* + final Host*(v)
    ↓
TransitionPlanner    compiles current → target
    ↓
ADD / MOVE / zone HANDOFF / RETIRE / GC
```

The balloon answers **where should the region end up?**
The transition planner answers **which safe primitive operations reach that state?**
The handoff protocol performs only **pair-to-pair zone authority transfers**.

No cold plan is a distributed transaction.

## 2. Balloon input

For a closed/stable region `R`, collect an exact snapshot:

- current regional vnode set `V_R`;
- zone IDs and parent relation;
- zone bytes/items and slow write heat;
- current per-zone `Authority`;
- current vnode hosts;
- current machine resource offers;
- `MemberDigest(R)`;
- `HotEpoch(R)`.

The trial is read-only. It takes no lock, reservation, handoff, or authority.

## 3. Candidate final topology

The balloon evaluates candidate final vnode sets `V*` and placements `Host*`.
For every candidate:

```text
Target(z) = argmin XOR(z, V*)
```

Projected load of target vnode `v` is:

```text
Bytes(v)  = Σ Bytes(z),  Target(z)=v
Writes(v) = Σ Writes(z), Target(z)=v
```

A candidate is invalid when a hard resource bound is violated.

A lower bound on the number of survivors is:

```text
Nmin >= max(
    1,
    ceil(Bytes(R)  / TargetBytes),
    ceil(Writes(R) / TargetWrites)
)
```

For small regions the reference implementation enumerates candidate subsets exactly.
This is intentionally expensive but off the foreground path.

## 4. Cold objective

Among valid candidates, the reference ranking is lexicographic:

1. fewer vnodes;
2. fewer physical machines;
3. fewer bytes copied to reach the final placement;
4. smaller maximum projected machine pressure;
5. deterministic vnode-ID tie break.

This is a policy, not a safety theorem. Production may use a richer score.

The important rule is that **logical topology and physical placement are optimized together**.
If survivor `A` must finally live on `M7`, data should migrate directly to `A@M7`, not first to
`A@M1` and then be copied again by a later MOVE.

## 5. Transition decomposition

Let:

```text
V0 = current vnode set
V* = desired vnode set
```

Then:

```text
ADD    = V* \ V0
KEEP   = V* ∩ V0
RETIRE = V0 \ V*
```

The transition planner emits:

1. `ADD` missing target vnodes at their final hosts;
2. `MOVE` surviving vnodes whose current host differs from their final host;
3. for every zone `z`, a direct pair-to-pair transfer when:

```text
Authority(z) != { Target(z), Host*(Target(z)) }
```

4. order affected zone transfers strict bottom-up;
5. `RETIRE(v)` only after `AuthoritySet(v) = ∅`;
6. residual GC asynchronously afterwards.

A zone migrates directly from its **current authority** to its **final authority**. It does not
follow temporary intermediate XOR owners created by the sequence of ADD/RETIRE operations.

## 6. Bottom-up rule

For an affected hierarchy edge:

```text
child TRANSFERRED → parent becomes eligible
```

A parent snapshot therefore contains:

```text
DirectPayload(parent) + ChildMarkers
```

and never payload already delegated to transferred children.

Siblings and disjoint branches may execute in parallel subject to resource budgets.

## 7. Retirement rule

A vnode may be logically retired only when:

```text
AuthoritySet(v) = { z | Authority(z).VNode = v } = ∅
```

Only after all affected zone authorities have moved may the monotonic retirement tombstone be
published into the topology CRDT.

Residual bytes are not authoritative and are GC'd later.

## 8. Cold plans are interruptible

A balloon target is a direction, not a transaction that must finish.

If a plan has completed:

```text
Z1 → A
Z2 → B
RETIRE W1
```

and a new hotspot appears, those completed operations remain valid. There is **no rollback**.
The cold plan stops and a later balloon starts from the new current state.

This is essential: rollback would itself be another non-monotonic distributed reconfiguration.

## 9. Hot split preemption

Hot work always wins over cold optimization.

Every regional cold plan carries:

```text
BaseDigest = MemberDigest(R) at trial time
HotEpoch   = regional hot epoch at trial time
```

A hot split touching any vnode used by the cold plan increments the hot epoch and invalidates the
cold plan.

Rules:

### TRIAL

Discard immediately. Nothing was mutated.

### EXECUTING, current zone handoff still reversible

If the zone has not crossed durable authority transfer:

```text
abort preparation
unfreeze if necessary
release reservation / staging
stop remaining cold operations
```

### EXECUTING, current zone already transferred

Do not roll it back.

```text
finish durable bookkeeping for that zone
stop the remaining cold plan
```

### Already completed ADD / MOVE / zone transfer

Keep it. A future balloon may optimize it away later.

### Planned RETIRE

Cancel it immediately if the vnode becomes hot or is split.

A cold regional lease must never block hot work. A hot event invalidates/revokes the cold lease.

## 10. Priority order

The scheduler uses semantic priority rather than FIFO ordering:

```text
P0  recovery / completion after an irreversible authority transfer
P1  hot pressure relief: hot split, emergency MOVE, urgent disk reclaim
P2  cold balloon reconfiguration
P3  opportunistic GC / packing / background cleanup
```

A task may be promoted: for example residual GC that is required to avoid a hard disk limit is
`P1`, not background `P3`.

Safety rule:

```text
irreversible recovery > hot work > cold optimization
```

## 11. Stable convergence

If:

1. the balloon chooses `(V*, Host*)`;
2. no hot preemption occurs;
3. every scheduled pair-to-pair handoff eventually completes;
4. retirements and anti-entropy are eventually delivered;

then:

```text
V_current = V*
Authority(z) = Owner(z, V*) for every zone z
Host(v) = Host*(v) for every surviving vnode v
```

If a hot event preempts the plan, this final convergence claim is cancelled, but every already
completed primitive operation remains individually valid.

## 12. Complexity

For `n` regional candidate vnode IDs and `Z` zones, the exact reference balloon is exponential in
`n` because it enumerates subsets. The simulator intentionally caps the candidate pool (`n <= 12`
by default).

Approximate reference cost:

```text
O(2^n × Z × n)
```

This is acceptable only for a cold regional oracle. A production planner may use greedy / beam
search while preserving the exact same transition and safety rules.

## Durable pair fold (local core)

`internal/mesh.Service.Compact` selects a known survivor deterministically by ID.
Symmetric pairs fold from the larger ID into the smaller; the pinned genesis
vnode ranks first. This orients attempts acyclically without a global arbiter.
It admits a fold only when that survivor's post-remove XOR cell contains the
source's entire current cell and every installed native grant it holds. The
source must cover its whole cell, including empty keyspace, with installed native
permissions. The genesis vnode stays active. The planner never grants authority.

`internal/storage/cold.go` persists the drain, a single survivor pin, the source
activity counter and stable piece identities. A disk cannot be both a drain and
a survivor pin. Sealing intake blocks new acquisitions and physical MOVE; it does
not grant or revoke ordinary writes. Existing handoff transactions transfer the
whole native tokens, exact zone snapshots, mutation sequences and retry receipts.
Only durable install ACKs allow retirement publication. Zone identity remains
`(collection, canonical path)`, including parent/child paths with identical XOR IDs.

A retirement shares the activation dot/frontier clock and carries outgoing grant
proofs covering the historical source cell. It is accepted only after the source
has no known native or provisional zone authority. Historical dependency views
still include earlier activations; the current view applies the tombstone. Old
activation replay cannot resurrect the identity, and growth skips retired IDs.
The survivor persists the retirement and a witness receipt before releasing its
pin. The source records the witness ACK and keeps serving as a forwarding endpoint.
Neither process nor disk is deleted.

Heat or topology drift cancels remaining work, but only before a piece starts
(COLD-2). A fold tolerates `HeatBudget` writes (default 1024) on the source and
on the survivor; the source checks both before each unstarted piece. A started
handoff is finished even under heat. Once every piece is installed, heat no
longer cancels the fold: aborting would only send the data back through
reconciliation, the ping-pong a tepid shelf would otherwise repeat forever. A
topology drift still cancels it. Completed permissions are never rolled back. This is weaker hot preemption than
the reference reversible planner above, and is not a latency isolation guarantee.
Writes during a started copy can increase its final size, with transactional
disk capacity still enforced. The normal reconciliation path can return grants
to an active source after an
aborted fold. A later trial uses a fresh operation identity. No clock expiry
unseals a pending transfer or clears a survivor pin: a crashed source must resume.

The node binary enables it unless its config sets `"compaction": false`; a
library `Node` calls `EnableCausalCompaction(CausalCompactionConfig{...})`.
Defaults (COLD-2):

| Setting | Default | Why |
| --- | --- | --- |
| `CoolWriteRate` for `CoolFor` | 10 writes/s for 2 minutes | a tepid vnode is eligible, not only a silent one; only measured seconds count |
| `MaxCombinedFraction` | 45% of the survivor's `data_bytes` | splits fire near 90%: a freshly split pair (≈90% together) cannot refold, and a fold leaves room to double |
| `HeatBudget` | 1024 writes | a few writes during a fold do not cancel it |
| `Interval`, `Timeout` | 5 s, 90 s | a fold that outlives the timeout is durable and resumes |

Admission reads the O(1) byte counter on both sides: the source sends its owned
bytes, the survivor adds its own and compares with its own capacity. The former
row caps (4096 moved, 8192 combined) and the 32 MiB byte cap are removed: rows
were counted by scanning the vnode inside the admission transaction, and fixed
sizes kept any vnode created by a real spike from ever folding.

The autoscaler has no scale-down path: it only asks for capacity. Shrinking
after a spike is this fold's job.
`Node.Update` schedules at most one local trial; maintenance resumes persisted
work, including after restart with automatic trial creation disabled.
`Node.CompactCausal` is the explicit local API.

Qualification: `TestAutomaticDurableCompactionAfterInsertions` runs 1024 durable
insertions through `1 -> 4 -> 1`, checking exact inventory and aggregate reads
through every endpoint. `TestColdFoldRecoversEveryMessageCut` reopens both disks
at eight message cuts, then checks payloads, dedup receipts and refusal of writes
by the retired disk. Heat, lost release ACK, capacity rejection, repeated growth, HTTP restart and
delayed messages have separate tests in `internal/mesh/cold_test.go` and `cmd/node/cold_test.go`.
`TestColdFoldSingleWriterAtTransferBoundaries` probes four canonical paths at
45 paused message boundaries, including a new key during copying: no duplicate
applied write, with explicit zero-writer windows while frozen.
These are finite local fault tests, not a proof for arbitrary partitions or
Byzantine peers. No global validator or global topology oracle participates.
Geometric admission uses the peer's causal knowledge; convergence requires
communication and eventual recovery. General multi-survivor balloon transitions,
physical host packing/shutdown, tombstone GC, mixed-version compatibility and
extreme-volume latency remain unqualified by this change. Permanent ID exclusion
can exhaust the safe fork addresses of a repeatedly identical hot region; epoch
reuse and reclamation are not implemented. A permanent source loss can retain
a survivor pin; automatic timeout reclamation would require another safety protocol.


### Durable volume campaign (2026-11-02)

`indexus/TestLargeDurableColdCampaign` is opt-in. It uses real Node/mesh handlers
and eight independent fsynced Bolt stores, grouped writes (2 ms delay), 32 closed-loop
workers, 77-byte record IDs and three exactly representable metric dimensions.
Activations `1 -> 4 -> 8` are explicitly orchestrated between insertion batches;
post-load pair compaction is automatic. These runs do not test automatic hot
provisioning, a global validator, WAN transport, HTTP serialization cost or
throughput scaling across physical machines. The transport invokes peer handlers
in memory on one host. Reported write p95 is RPC service latency from worker start,
excluding the harness queue; it is not an open-loop overload measurement.

Each case deletes 10% of initial records, restores 5%, and checks complete payloads
(including tombstones), exclusive owner answers and all three metric sums at every
expected prefix. After compaction and again after reopening all eight stores, it
compares every retry receipt and mutation sequence against acknowledged results.
The 100k adversarial and 300k balanced cases also perform exact inventory reads
while cold transfers run: eight successful reads during the cold phase, zero mismatches in
this finite sample. Compaction happens without new writes in these volume runs;
heat preemption and transfer-boundary writer safety have separate fault tests.

| Profile | Initial records | ACKed mutations | Final live records | Prefixes checked per stable phase | Automatic 8 -> 1 |
|---|---:|---:|---:|---:|---:|
| balanced | 20,000 | 23,000 | 19,000 | 7,489 | 11.5 s |
| balanced | 100,000 | 115,000 | 95,000 | 7,489 | 26.3 s |
| 90% concentrated in three related prefixes | 100,000 | 115,000 | 95,000 | 3,777 | 83.8 s |
| balanced | 300,000 | 345,000 | 285,000 | 7,489 | 92.2 s |

All four cases passed. Insertion-phase p95 ranges: 18–34 ms, 19–38 ms,
22–37 ms and 21–45 ms respectively. The worst mutation-phase p95 was 157 ms
(the balanced 100k restore batch). These are separate runs, not latency guarantees.
The concentrated case moves a large shard and takes over three times the cold
wall time of balanced 100k. File-size sums reached about 1.0 GiB (100k balanced),
2.3 GiB (100k concentrated) and 2.7 GiB (300k balanced). Those sums include retired
stores, history, receipts and retained Bolt pages; logical retirement does not
shrink the physical database files. CPU versus disk contributions were not profiled.

Reproduce from the core repository (allow disk and memory headroom):

```sh
INDEXUS_VOLUME_ROWS=300000 INDEXUS_VOLUME_PROFILE=balanced \
INDEXUS_VOLUME_OUT=/absolute/path/result.json \
go test ./indexus -run '^TestLargeDurableColdCampaign$' -v -count=1 -timeout=30m
```

Set `INDEXUS_VOLUME_PROFILE=adversarial` for the concentrated workload. The ordinary
test suite skips this campaign unless `INDEXUS_VOLUME_ROWS` is set. Temporary test
databases are removed by test cleanup; JSON phase reports and logs can be retained.


An instrumented 2k adversarial correctness run under `-race` recorded 5,546
live-row instances across the seven successful retirement plans for 1,900 final
live rows. The large shard followed `240 -> 208 -> 144 -> 16` (first ID byte),
confirming repeated movement through the deterministic sibling folds. This is a
row-instance count at admission, excluding warm-copy duplication and retries;
it is not a measured byte-amplification ratio for the 100k campaign. Choosing a
survivor based on movement cost is a concrete optimization opportunity. This
campaign changes tests and documentation only; it does not change that policy.


### 500k-item extension (2026-11-02)

The balanced campaign also passed at 500,000 initial items and 575,000 ACKed
mutations: 50,000 deletes followed by 25,000 restores leave 475,000 live records
and 25,000 tombstones. All payloads, 7,489 prefix counts/metric sums and all
575,000 original mutation receipts remain exact after automatic compaction and
again after reopening all eight disks. Three full inventory reads during the
cold phase returned exact data, with no unavailable result in this finite sample.
The active trace was `1 -> 4 -> 8 -> 5 -> 3 -> 2 -> 1`; cold wall time was
139.0 seconds. Insertion-phase p95 was 35.4–42.4 ms; the worst mutation-phase
p95 was 80.4 ms (restores). Successful retirement plans moved 712,455 live-row
instances; this excludes warm-copy duplication and aborted attempts.

This extension uses a 2 GiB encoded-data budget per test store to allow staging
headroom (`INDEXUS_VOLUME_CAPACITY_BYTES=2147483648`); earlier main volume runs
used 1 GiB. The variable defaults to 1 GiB. Initial moved-byte admission remains
1 GiB. No production Go implementation or protocol rule changed. The measured
sum of database file sizes reached 4,758,388,736 bytes (about 4.4 GiB), including
retired disks and retained pages. The regenerable Go build cache was cleared
before the run to provide disk headroom. As above, latency excludes the harness
queue, with 32 closed-loop workers, real fsynced Bolt stores and in-memory peer
transport on one physical host; this is not a WAN or overload qualification.

```sh
INDEXUS_VOLUME_ROWS=500000 INDEXUS_VOLUME_PROFILE=balanced \
INDEXUS_VOLUME_CAPACITY_BYTES=2147483648 \
INDEXUS_VOLUME_OUT=/absolute/path/result.json \
go test ./indexus -run '^TestLargeDurableColdCampaign$' -v -count=1 -timeout=30m
```
