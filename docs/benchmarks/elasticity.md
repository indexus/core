# Elasticity: spike, split, calm, fold

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

Does the network absorb a 10× spike on one prefix by splitting vnodes under
load, then fold back when the spike is over, without a frozen zone or a lost
write? How long until it is back in equilibrium?

## Scenario

| Phase | Load | Expected |
| --- | --- | --- |
| Base | uniform writes, 4 vnodes, 300 writes/s | steady, one knowledge version |
| Spike | 10× the base rate on one prefix (`-scenario spike -spike-factor 10`) for 2 min | vnodes that own the hot prefix exceed their scaling trigger and split; new vnodes join through the provisioner one at a time; acked rate recovers |
| Calm | no writes for 7 min | cold vnodes fold back (COLD-2); retired vnodes leave and free their slot |

The scaling trigger is item-driven (`scaling.items_per_vnode`, 20 000 by
default in experiments) so that splits are deterministic and the vnode, not
the host, is the bottleneck. Tools: `ops/experiment/elastic_local.sh` (local),
the `dynamics` phase of `ops/experiment/aws/scenarios.sh` with a provisioner
(AWS).

## Measurements

From the observer (`metrics.jsonl`, one sample per node every 2 s) and the
event journal (one line per handoff, split, fold, join, retirement):

- vnode count over time, and the time from the spike start to the first split
  and to the last join;
- acked over offered during the spike, and the deepest dip;
- handoff duration and MB/s;
- **time to equilibrium**: from the end of the spike until the vnode count is
  stable for 60 s, no handoff is open, and one knowledge version is shared;
- after the calm: zones fenced with no transfer in progress (must be 0, REM-88),
  retired vnodes still holding slots, knowledge bytes per vnode;
- the audit with every acknowledged key read back (`-sample 0`).

## Green

The common criteria of [faults.md](faults.md), plus: every split finishes or
aborts within 2 minutes, the vnode count after the calm is back within one of
its count before the spike, and no zone is left fenced.

## Results

None yet in this protocol. Earlier single runs: COLD-2 folded 8 vnodes into one
after a hot run with every row on the survivor
([p2p-findings-2026-11-15.md](p2p-findings-2026-11-15.md), "Held"); open
problems then were stuck transfers fencing their zones and knowledge growing
with zones moved (items 1, 2 and 4 of "Open" there).

A first local run on 2026-11-17 (2 agents × 8 slots, spike to 3 000 writes/s)
was lost when the host crashed; it will be replayed with fewer slots.

### Local run, 2026-11-17: red, the spike wedged the cluster (REM-121)

`ops/experiment/results/local-elastic-20261117-184217/`, commit `dbbf892`,
one host (Apple M1 Pro), `elastic_local.sh` with 2 vnodes to start, 1 agent of
6 slots, split at 20 000 items, 200 writes/s then 10× on one prefix for 2 min,
every write entering through the genesis vnode.

| Phase | What happened |
| --- | --- |
| 0 to 60 s, base load | 200/s offered, 200/s acknowledged |
| 60 to 90 s, spike | 2 000/s offered, about 2 000/s acknowledged |
| about 90 s | both vnodes past 20 000 items ask for peers; the provisioner answers slowly (timeouts, then `429 requester_cooldown`); the genesis opens a handoff to a new vnode |
| from about 90 s on | **0 writes acknowledged until the end of the load**: 512 requests in flight for more than 10 minutes; the 5-minute load took 12 min 54 s |
| handoff | first attempt 505 s, failed (the joiner did not answer); second attempt never ended |
| calm, 7 min | no fold; knowledge versions still differ between nodes; the new vnodes own nothing (`known_vnodes` stays 2) |
| restart of every node from its disk | the handoffs are still open after the restart and the rebalance refuses to start ("handoff in progress"); reading back the 73 673 acknowledged keys did not finish in 20 min |

The genesis' goroutine dump shows the write path stopped behind the storage
write lock: 370 writes waiting for their group commit, the group commit and
101 knowledge merges waiting for `Store.writeMu` for up to 14 minutes, and no
goroutine holding it. Investigation in REM-121, with REM-88 (zones fenced by an
abandoned transfer) and REM-87 (joins under load).

What this run shows, and does not:

- The split under load does not work yet on this build: the elastic scenario
  is red before any question of speed. This is the first item to fix before
  any elasticity figure.
- Durability after the wedge is not established: the read-back could not
  finish while zones stay fenced. The disks are kept for an offline check.
- One host, every write through one vnode, and a slow provisioner make the
  conditions harsher than a real network; they do not explain a lock held for
  minutes.

### Diagnosis, 2026-11-18: what wedged the split, and what was fixed

Reproduced in 75 s with a short spike (2 vnodes, ×10 for 60 s) that stops on
the first stall and dumps every node; measured with per-step transfer timings
and CPU profiles of every node. Each cause below has a test that fails
without its fix.

| Cause | Fix | Commit |
| --- | --- | --- |
| A lock cascade: a long reader (whole-store inventory, control decode, knowledge rebuild) held `rowsMu`; the writer waiting for it blocked every new reader | inventory read in pages; decoded control cached; merged knowledge published by the writer | `ae30b68` |
| Every concurrent call to a peer carried the whole knowledge bundle | one carrier per peer and version | `ae30b68` |
| The gossiped knowledge grew from 2 kB to 12-15 MB during a split: per-zone transfers and the split plan's per-zone forecast | ZONE-GOSSIP-1, INTENT-LEAN-1 ([zone-transfer-scope.md](../protocol/zone-transfer-scope.md)) | `28fc073` |
| One 30 s budget for all maintenance: a slow partner left `Resume` with an expired context, so frozen handoffs never moved (REM-88) | a deadline per phase; resume every transfer past a failure | `28fc073` |
| Every write read and compared the whole control document; every handoff batch rescanned all stored data for capacity; ZONE-2 scanned the MigrationSet per zone | stored control version; incremental usage; counted ZONE-2 index | `2444462` |
| Every batch rewrote, hashed and revalidated the knowledge, one entry per zone moved | per-zone transfers as local rows (XFER-KEYS-1, [transfer-state-keys.md](../protocol/transfer-state-keys.md)) | `ac9f890` |

Same 300 s local spike (2 vnodes, 6 slots, split at 20 000 items, ×10 for 60 s):

| | 2026-11-17 | 2026-11-18, `ac9f890` |
| --- | --- | --- |
| acked writes during the split | 0/s, never recovered | ≈2 000/s of 2 000 offered |
| knowledge bundle | up to 15.5 MB | ≤ 4 kB |
| install cost per batch of 64 zones, start → end | — (stuck) | 52 → 61 ms |
| longest transfer | never ended | ≈25 800 zones in ≈3 min |

### Local run, 2026-11-18: writes hold, splits too slow (`ac9f890`)

`ops/experiment/results/local-elastic-20261118-153109/`, same setup as the
2026-11-17 run (2 vnodes, 1 agent of 6 slots, split at 20 000 items, 200
writes/s, ×10 for 2 min, 7 min of calm).

| Phase | What happened |
| --- | --- |
| spike, 60 to 180 s | ≈2 000/s acknowledged of 2 000 offered, up to 3 000/s while queues drained; knowledge ≤ 6 kB |
| after the spike | 200/s acknowledged, with pauses of 1 to 5 s every 10 to 20 s (to explain) |
| splits | two handoffs start at 92 s (warm copy ≈30 s, then the final phase); about 11 min each at ≈45 zones/s (flat per-batch cost of ≈1.4 s for 64 zones, 7 nodes on one host) |
| after 13 min | one new vnode active; no fold; the audit waits, complete reads being refused while a transfer is open |

Red on its criteria: a split must finish within 2 minutes. What remains is a
fixed cost per batch (five round trips for 64 zones), no longer a cost that
grows with the transfer. Next: larger batches, measured (run `114`).

### Local run, 2026-11-18 evening: entries spread, the cost is the control document (`436f070` + loader discovery)

`ops/experiment/results/local-elastic-entries-20261118-222117/`. Less biased
bench: writes enter through every live node (`load -discover-every 5s`,
seeded with the genesis only; the loader learns peers from `/neighbors` as a
peer would), two agents of 3 slots standing for two machines, 3 vnodes at
start, same spike (200 writes/s, ×10 on one prefix for 120 s, 300 s of load).

| | |
| --- | --- |
| entry points learned | up to 7 (every spawned node) |
| writes relayed by the entry node | ≈ 87 % (random entry, one holder per zone) |
| genesis split | > 60 000 zones to move; still open after 23 min at ≈ 44 zones/s |
| zones/s after the load stopped | unchanged: **the split does not compete with the writes** |
| time per step, that transfer | freeze 9.6 min, emit 9.3 min (both local transactions on the source), install 1.6 min, stage 1.8 min, mirror 0.5 min |
| after ≈ 220 s | node `21100` stops answering mesh calls (30 s timeouts); acknowledged writes near 0 cluster-wide |

Freeze and emit are 83 % of the transfer. Each rewrites the whole control
document, whose `CausalTransfer.Zones` holds one `ZoneState` per zone of the
operation (60 000 here): JSON-encoded, hashed and cloned per batch of 64, so a
batch costs O(MigrationSet) and a transfer O(n²/64). The earlier runs moved
fewer zones and did not show it. This is the `ZoneState` half of XFER-KEYS-1
([transfer-state-keys.md](../protocol/transfer-state-keys.md)), deferred then
"until a measure says otherwise": this is that measure.

Not a cause: competition with the writes (rate unchanged without load), the
network steps (≈ 15 %). Throttling writes protects a node but would not
shorten this split. Open: the mesh timeouts of `21100`.

### 2026-11-19: green on the short profile (`32fca76`, `49ab73a` and the zoneHolder fix)

`ops/experiment/elastic_short.sh`: 2 agents of 3 slots, 3 vnodes at start,
split at 5 000 items, 200 writes/s then ×10 on one prefix for 60 s, writes
through every live node, 60 s of calm, audit of 20 000 sampled keys.
Result `ops/experiment/results/short-elastic-20261119-073450/`.

| | 2026-11-18 evening | 2026-11-19 |
| --- | --- | --- |
| zone flips | 44 zones/s | ≈ 700 zones/s |
| splits | > 23 min, never finished | 17 136 zones in 44 s; every split ≈ 30 s |
| refused writes | up to 1 500/s | 0 (131 716 of 131 718 acked) |
| handoff attempts failed | most | 0 of 12 |
| audit | not reachable | pass: 0 missing, 0 double-owned zones, totals exact |

Four causes, each a cost that grew with the zones moved:

1. every freeze and emit rewrote all `ZoneState`s of the operation (rows now, XFER-KEYS-1);
2. the region stage carried every zone's bytes, over the 32 MB mesh limit
   (it now carries the zones without an install ACK; the certificate commits
   to per-zone seals, `regionDigest`);
3. region install and emit dropped the per-zone rows in their transaction,
   past the 30 s mesh timeout (now batched in `CollectTransferred`);
4. `zoneHolder` scanned the store index for each zone, so an inventory walk
   froze a node for tens of seconds (now one point read per open transfer;
   found with goroutine stacks, which the short profile now captures when a
   transfer stays open).

The long profile (20 000 items, 7 min of calm) and AWS remain to confirm.

### 2026-11-19 evening: load-driven scaling, and a fairer test

**Load-driven scale-up.** A vnode's write demand (applied, or refused at its
cap) now forces a scale-up at `scaling.writes_per_vnode`, 70% of the cap by
default; machines start on demand (one spare kept warm) and are released
once empty. Fixed on the way, each found with goroutine stacks or node logs:
quadratic prefix lookups in large updates, a usage count that scanned the
disk per changed node, a transfer chasing zones born during it, a donor
answering joiners one queued job each, SCALE-1 landing on the donor itself,
an intent waiting forever for a planned name that SPAWN-1 never uses, and
late joiners of a past spike churning the cluster.

**The ×8 spike is not the criterion.** One prefix ×8 in a second, absorbed by
scale-out within 72 s, asks more than reference systems promise: DynamoDB on
demand absorbs 2× the previous peak at once and asks to spread faster growth
over 30 minutes; CockroachDB splits on load and moves replicas over minutes;
Riak's handoffs are operator-driven. Best spike result (AWS, run 4): first
split 34 s (was 295 s), 4 → 6 vnodes during the spike, 4 → 12 after it,
55–68% acked during it. It stays a robustness check only (no loss, no stall).

**Steps profile (the criterion).** `load -scenario steps`: uniform keys, a
plateau, then ×2 over a ramp and held, three times; `ops/experiment/steps.py`
gives the verdict per level (≥ 95% acked on the second half of each plateau).
AWS `MODE=steps`, 4 × c6i.2xlarge, 4 slots each, cap 1 000 writes/s per vnode,
ramp 60 s, plateau 120 s (`results/aws-dyn-load-20261119-210107`):

| level | offered | acked |
| --- | --- | --- |
| 1 000/s | 1 000/s | 100% |
| 2 000/s | 2 000/s | 100% |
| 4 000/s | 3 975/s | 99.4% |
| 8 000/s | 7 951/s | 99.4% |

4 → 16 vnodes (every slot by the third minute); audit of 2.2 million
acknowledged keys: 0 missing, 0 read error, totals exact. One pass; the
observer and the double-owner check saw only the first 4 vnodes (fixed
after: `every_monitor`, endpoints refreshed before the audit).

### 2026-11-20: write stalls after splits, and three green passes

Steps profile on AWS with machines on demand (2 hosts of 4 slots at start,
the provisioner starts the others), 1 000 → 8 000 writes/s, cap 1 000/s per
vnode, 4 → 28 or 32 vnodes. Writes acknowledged after 4 s or more, per pass:

| Run | Code | Writes ≥ 4 s | p99 (median window) | Audit |
| --- | --- | --- | --- | --- |
| `aws-machines-20261120-062942` passes 1, 2 | before | 3 255, 6 437 | 524 ms, 1 s | pass |
| `aws-fix-20261120-110327` | 068fa1c | 5 910 | — | pass |
| `aws-fix2-20261120-130348` | 87ff579 | 3 | 33 ms | pass |
| `aws-steps3-20261120-142243` passes 1, 2, 3 | 025ee52 | 51, 529, 691 | 66, 66, 33 ms | pass ×3 |

Every plateau of the last four passes was 100 % acknowledged with no hole.

The stalls were one node at a time going silent for 10 to 28 s after a
split, while no transfer was open and its fsyncs stayed fast; 97 % of
writes are relayed once, so every node relaying to it waited too. Stacks
saved by the observer (`stacks/`, summed by `ops/experiment/stalls.py`)
named two holders:

- `compactZoneRows` after an install read each batch from the start of
  the operation's rows under the exclusive rows lock (068fa1c);
- the summary tree lock, which every write took, held while valuing edges
  through a walk of every incoming contribution (d191973, 87ff579).

On 8 fixed vnodes (`MODE=stall`: 2.4 million items, writes, reads of
totals, no writes, a hot prefix) no node stalled: the stalls need splits.

Freshness of the root total during the steps passes (probe every 200 ms,
p99 per 5 s window): median 1 to 2 s, p90 4 to 8 s, up to 17 to 33 s
during splits.

Left: reads still wait for the rows lock behind the group commit's fsync
and the compaction deletes (up to 958 waiting on one node, one node silent
8 to 14 s per pass).

### 2026-11-20 evening: consolidation, local

No AWS run. After the day's changes (b118b68), against the tree of the
morning of 2026-11-19 (62ccfa9), on the same 4-core arm64 VM, 3 runs each:

| Benchmark | 62ccfa9 | b118b68 |
| --- | --- | --- |
| `BenchmarkCausalInsert` durable-update, distinct zones | 362 to 382 µs | 354 to 367 µs |
| `BenchmarkCausalInsert` durable-batch, distinct zones | 215 to 249 µs | 219 to 238 µs |
| `BenchmarkInsertCost` distinct zones | 42.3 µs | 41.9 to 43.7 µs |
| `BenchmarkHandoffZoneThenRegion` | 79.3 to 80.0 ms | 79.1 to 80.5 ms |
| `BenchmarkPlanningStats` 8192, inventory and seal | 61.8 to 63.0 ms | 63.3 to 63.4 ms |
| zonelog `BenchmarkApply` batch-256 | 5.0 to 5.8 µs | 5.1 to 5.7 µs |
| zonelog `BenchmarkChildren` depth 2 | 63.9 to 69.9 µs | 63.1 to 68.8 µs |
| zonelog `BenchmarkScanZone` | 71.4 to 74.1 µs | 66.9 to 75.1 µs |

Single-node write and read costs are unchanged within the noise of the
runs. New: `BenchmarkSiblingTotals`, 64 sibling totals in 0.22 ms through
a flush's zone sums against 12 ms one by one.

Checks: `go vet ./...`, the whole suite, the whole suite under `-race`
(one race found and fixed, 68b1967), the long simulation
(`INDEXUS_LONG=1`). Not yet measured on AWS: 2470ae1 and dc63ec3 (summary
flushes), c435edf (compaction deletes), 591e236 (write routes).

### 2026-11-21: local cutover cost and independent donors

`Join` now lets each donor copy and complete its region independently. The
activation regression blocks one donor during preload and checks that the
joiner already applies writes from the other donor and forwards the remainder.
It fails against `0e9c453`, passes after the change, and passes under `-race`.
This removes the cross-donor warm-copy barrier; the per-donor warm convergence
and regional completion are still present.

`BenchmarkZoneCutover` measures `FreezeZones` through `InstallZones` on two
separate stores on one Apple M1 Pro, with real fsyncs, one record and one metric
per zone. Seed writes and warm preload are outside the timer. No RPC,
serialization, activation, final mutation backlog or competing writes are
included. These are storage-only lower bounds, not client latency or cluster
percentiles.

A separate `BenchmarkNewCollectionZoneCutover` gives each zone a collection not
registered in the receiver's live-record engine. Previously `Apply` committed
the manifest once per new collection. It now persists their IDs in one manifest
before committing the batch's log frame. A failed manifest commit stops further
writes until reopen, so provisional in-memory IDs cannot bypass durability.

| New collections / zones in one cutover | Before, mean (3 samples) | After, mean (10 samples) | After, observed max |
| --- | --- | --- | --- |
| 1 | 27 ms | 29 ms | 38 ms |
| 16 | 188 ms | 35 ms | 40 ms |
| 64 | 685 ms | 39 ms | 44 ms |
| 512 | 6 193 ms | 115 ms | 121 ms |

For comparison, with one collection shared by all zones, the after means are
28, 34, 44 and 136 ms for 1, 16, 64 and 512 zones (10 samples each); the
512-zone maximum is 261 ms. These tiny records do not qualify the current
512-zone batches for large payloads or large retry ledgers.

Reproduce the storage measurements:

```sh
go test ./internal/storage -run '^$' \
  -bench '^Benchmark(ZoneCutover|NewCollectionZoneCutover)$' -benchtime=10x
```

The next cluster qualification should aim for a small-lot mean below 100 ms
and p99 below 250 ms on nearby machines with SSDs, treating a no-writer window
above 1 s as a failed run to investigate. These are targets, not measured
cluster guarantees. Record first write applied on the receiver separately from
cutover duration and full handoff completion. Future lot selection must account
for bytes and final mutation backlog as well as zone count; a single oversized
zone needs separate qualification.

### 2026-11-21: unified granular FLIP-UNIT comparison (local, Apple M1 Pro)

`BenchmarkJoinFinalPhase` (4 096 distinct-collection zones, in-process
transport, NoSync fixtures, `INDEXUS_BENCH_NO_USAGE_SCAN=1`, 3 samples each):

| Branch | Lot size / order | First writable lot (`first_install_ms`) | Longest no-writer window (`hold_max`) | Full `Join` (`ns/op`) | Result |
| --- | --- | --- | --- | --- | --- |
| `origin/main` (`0e9c453`) | 512 zones, map order, warm region | > 300 ms | `install_max` 863 ms | — | Failed at `stage` (`Metrics` seal mismatch fixed by `46e9549`) |
| `feat/convergent-replication` (`f03d638`) | 64 zones, heat-first, warm region, 2 in flight | > 200 ms (after warm copy) | 211 to 253 ms | 4 161 ms | Pass |
| `codex/granular-handoff` (`3fa7b2c`) | 512 zones + 8 MiB, map order, per-lot copy | 132 ms | ~102 to 164 ms (sum of step maxima) | 1 066 ms | Pass |
| Unified (`proposal/main-unify-handoff`) | 64 zones + 8 MiB, frozen then heat-first, per-lot copy (`ZoneSeqs`) | **51 ms** | **22 to 37 ms** | **1 195 ms** | Pass |

Real-`fsync` storage cutover lower bounds on the unified branch (5 samples each):
`BenchmarkNewCollectionZoneCutover/zones=64` mean 53 ms (max 59 ms) vs
`zones=512` mean 103 ms (max 138 ms); `BenchmarkZoneCutover/zones=64` mean 74 ms
vs `zones=512` mean 123 ms. These are local single-host lower bounds, not
remote cluster percentiles.

In-process NoSync scale stress runs on unified `main` (`INDEXUS_SCALE_CONCURRENT=1`,
`INDEXUS_SCALE_PARALLEL=4`, `INDEXUS_SCALE_LOSE_INSTALL_ACK=1`,
`INDEXUS_SCALE_NEW_ZONES=1`, 512 initial zones, ACK presence + single-writer probes):

- 16 vnodes: 2 012 ACKs = 2 012 observed rows, 2.14 s (`-race` pass at 16 vnodes).
- 32 vnodes: pass.
- 64 vnodes: 2 616 ACKs = 2 616 observed rows, 4.06 s.

Durable HTTP + `fsync` + `pacedLink` measurement fixture (`scripts/perf/handoff.py`,
512 MiB / 65 536 zones, 8 KiB payload, 50 MiB/s + 2 ms RTT, production
accounting, Apple M1 Pro, Go 1.26, `GOMAXPROCS=8`) on unified `main` (`d55ffe2`):

| Metric (512 MiB / 65 536 zones, HTTP + `fsync`) | `origin/main` (`0e9c453`) | `codex/granular-handoff` (`7237ef7`) | Unified `main` (`d55ffe2`) |
| --- | --- | --- | --- |
| First receiver ACK | 51.89 s | 2.47 s | **2.38 s** |
| Lot pause (`freeze_call → install_ACK`, mean / p95 / p99 / max) | 535 / 602 / — / 703 ms | 96.8 / 125.5 / — / 269.8 ms | **63.5 / 85.0 / 98.9 / 145.6 ms** (1 024 lots) |
| Strict fence window (`fence → install_ACK`, mean / p95 / p99 / max) | — | — | **51.4 / 68.8 / 81.4 / 105.7 ms** |
| End-to-end client ACK (mean / p95 / p99 / max) | — | 33.5 / 57.4 / — / **963.1 ms** | **23.9 / 50.6 / 71.3 / 272.7 ms** (1 563 ACKs; 191.5 ms at closure) |
| Whole `Join` | 135.87 s | 100.22 s (547 lots) | **116.46 s** (1 024 lots) |
| Audited ACKs | — | 66 726 / 66 726 | **67 100 / 67 100** |

During the 3.48 s post-last-lot closure window (113.0–116.2 s), every live
client write completed in 9.7–19.9 ms (`eachZoneStatePaged`, 512-zone pages +
O(1) `zoneStatesVersion` commit check). Local single-host numbers do not
qualify multi-machine or WAN percentiles.
