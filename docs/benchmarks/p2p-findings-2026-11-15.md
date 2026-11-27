# Live-network findings, 2026-11-15

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

## AWS, 2026-11-15 night (8 × c6i.2xlarge, TLS, HTTP/3 between nodes, `MODE=push`)

| run | load | best green step at 8 / 16 / 32 vnodes (acked/s) | first red step at 32 |
| --- | --- | --- | --- |
| 205402 | 1 × c6i.2xlarge | 3 592 / 4 748 / — | 5 840 of 6 400 |
| 213009 | 1 × c6i.8xlarge | 3 593 / 4 748 / — | 5 158 of 6 400 |
| 224313 | 2 × c6i.4xlarge, NET-2 | 5 367 / 6 929 / — | 5 519 of 6 400 |

Every audit passed and every run removed its resources (a final sweep found
nothing). In the first two runs the cluster plateaued near 5 000 writes/s at
every size with every CPU nearly idle (load machine 1 core of 32, busiest node
≤ 0.66 core). Cause (NET-2): quic-go accepts 100 concurrent streams per
connection by default; a goroutine dump of a saturated local load showed 2 346
of its 4 096 requests waiting for stream credit and 200 in progress. With the
limit at 4 096 and two load machines the plateau rose by about 1.5× at 8 and 16
vnodes, but 32 vnodes still stop near 5 500/s: the service time of a relayed
write grows from 5 ms to about 1 s while nodes stay under 0.9 core and handle
only ~170 writes/s each. Another wait grows with the cluster.

Second cause (KNOW-FAST), found with the goroutine capture on a saturated
local cluster (2 vnodes, 2 synchronized load machines, 4 000 writes/s): about
760 goroutines per node were parked on one mutex in `knowledgeFrame`, taken by
every client write and every mesh reply just to return the cached frame. With
the check outside that lock: 4 061 of 4 000 offered acknowledged (97.6%
before), relayed-write service time 211 → 83 ms, the queue on that lock gone.
The next AWS run measures both fixes at 8 to 64 vnodes.

## Horizontal scaling proved, 2026-11-16 (AWS, HTTP/3, 2 synchronized load machines)

With NET-2, KNOW-FAST and exact audit totals (the audit had compared
acknowledged writes to the root owner's summary, one tick behind; AUDIT-2 read
every acknowledged key of the "short" collections back: none lost).

Campaign 8: 16 × c6i.xlarge, one vnode per machine up to 16, two at 32;
40 s steps, offered rate × 1.5 per step.

| vnodes | machines | best green step (acked/s) | per vnode | first red step |
| --- | --- | --- | --- | --- |
| 4 | 4 | 6 072 | 1 518 | 6 808 of 9 112 |
| 8 | 8 | 18 371 | 2 296 | 17 989 of 27 337 |
| 16 | 16 | 24 303 | 1 518 | 32 614 of 36 450 |
| 32 | 16 (2 each) | 31 934 | 998 (1 996 per machine) | 39 973 of 48 600 |

Per-vnode capacity is the same at 4, 8 and 16 vnodes on one machine each
(the 1 518/s step is green at every size and the next step, 2 277/s, red at 4
and 16, green at 8): 100% efficiency at 16 vs 4, within the 1.5× step. At 32
the 16 machines carry two vnodes each and the per-machine figure stays at
about 2 000/s. Every step was durable (no acknowledged key missing, no zone
with two writers) and every exact total matched.

Campaign 7 (8 × c6i.2xlarge for every size, so up to 8 vnodes per machine):
12 128 / 15 727 / 21 601 / 28 772 acked/s at 8 / 16 / 32 / 64 vnodes: more
vnodes on fixed hardware still add throughput.

Open: a join under load still sends whole knowledge bundles (throughput /8
during the handoff on one host); `deep=true` without `refresh=true` answers
from the root owner's summary, which lags by up to one tick (documented).

## Latency under load, 2026-11-16 (campaign 8 re-read)

At the same load per vnode, write latency does not depend on the network size:
p99 65 / 65 / 33 ms at 300 writes/s per vnode on 4 / 8 / 16 vnodes, 2.1 s at
1 519 per vnode on all three. The tail grows with load per vnode: p95 crosses
1 s from about 1 000 writes/s per vnode while the step is still green, so the
capacity at a p99 near 100 ms is about 450 per vnode, a third of the green
figure.

The tail is inside the nodes. The receiving node's histogram (receipt to
acknowledgement) matches the client's; the load machines held 4 015 of 4 096
workers idle; nodes used at most 1.8 of 4 cores on green steps. 94% of writes
are relayed (a random entry node, 15 of 16 not the owner); the owner spends
20.7 ms on average per relayed write at 16 vnodes and 16 200 writes/s, with
writers waiting in `diskBatch.submit`. One goroutine commits groups one after
the other, so one slow commit holds
every write behind it: that fits a median of 8 ms with a tail past 1 s. In
zonelog a commit can wait on the log's fsync or on a full memtable whose
predecessor is still being written. `/metrics` now has `disk.commit_us_log2`
and `disk.wait_us_log2`, and every push step derives `write_p99_ms`,
`commit_p99_ms` and `commit_wait_p99_ms`: if the commit p99 is near the write
p99 the disk is the tail, otherwise the relay is.

## Long battery, 2026-11-16

`INDEXUS_LONG=1` failed on two tests, on main before and after the layer
refactor, and the `-race` suite on a third; none was a defect of the node:

- `TestConvergence_PartitionRemergeKeepsTheUnion` read items with `/set`,
  which answers aggregates, so even items written before the partition read
  as lost. It now reads records (`GetRecordsContext`); totals were exact.
- `TestQueueBackpressureOnNode` made an oversized write with 1 024 metrics,
  refused by validation (at most 128) before the store's admission it meant to
  test. It now uses 128 metrics, more bytes than the queue admits.
- `TestMergesPointAtLargePayloads` assumed how the memtable splits into
  segments; under CPU contention none merged. It now writes until a merge.

Still open: under `-race`, `TestContinuousReadsKeepEarlierACKsDuringJoinsAndPartition`
(indexus) runs out of its fixed 45 s budget, before and after the refactor; it
passes without `-race`.

## Ladder, 2026-11-15 evening (TLS + HTTP/3, one host, 2 CPUs)

Palier 0 (tools): rare-event journal (`handoff start/end` with operation id
and attempt), open-handoff gauge in `/metrics`, `experiment sentinel`, CAP-2
write cap per vnode. Proof: a node frozen with SIGSTOP at 18:34:35 under load;
the sentinel stopped the run at 18:34:46 (`node_down`, 3 polls), wrote the
incident and ran the teardown.

Palier 1 (fixed vnodes, uniform writes, cap 100 writes/s per vnode, 80 offered
per vnode, 45 s): 3 green passes of 3 at 1, 2 and 4 vnodes; 100% acknowledged,
audits green, p99 8–16 ms.

Saturation (130 offered per vnode against the same cap): acknowledged writes
per vnode 100.0 / 100.0 / 100.0 / 100.2 at 1 / 2 / 4 / 8 vnodes, i.e. 100%
per-vnode efficiency up to 8. This host cannot say more: the next steps
(16+ vnodes) need machines (AWS step).

First sentinel finding: refusals under overload were counted as stagnation;
the rule now judges only writes that were not refused.

Push without cap (how far one host goes, TLS + HTTP/3, load on the same host):

| vnodes | best green step (acked/s, ≥ 95% of offered) | first red step | limit |
| --- | --- | --- | --- |
| 2 | 1 800 | 2 527 of 2 687 (94%) | host CPU |
| 4 | 1 980 | 2 006 of 3 147 (64%) | host CPU |
| 8 | — | 1 748 of 2 204 (79%) | host CPU: more processes, same 2 CPUs |

Knowledge stayed at 1.3 KB per vnode and every audit passed. The ceiling is
this host's: past 4 vnodes more nodes only share the same CPUs. The next
answer needs machines: `MODE=push` on AWS (see `ops/experiment/README.md`).

Rehearsal finding: after a reset the campaign targeted dead slots (3% of
writes refused at the client); `endpoints()` now lists live slots only.

Real node processes over HTTP on one host (2 CPUs), driven by
`cmd/experiment` (plan: [p2p-experiment.md](p2p-experiment.md)). One host
shares its CPUs between every node and the load: throughput figures are this
host's, not the protocol's. Costs per node and failure modes are not.

## Fixed

| Rule | Defect found | Before | After |
| --- | --- | --- | --- |
| SUM-2 | every node polled every host for summaries each tick, idle or not | 1 695 summary calls, 15.7 MB at 16 nodes / 60 s | 452 calls, 4.8 MB; mesh bytes per node −24% |
| PREFIX-1 | a one-key read asked every holder | 1 read = N calls | 1 call; reads served ×2.6, read p50 524 → 131 ms, writes +37% under the same mixed load |
| JOIN-1 | joiners took random names: largest cell up to 2^8 × the smallest | items max/mean 2.05 (normal) to 4.01 (ramp) at 16 nodes | 1.05 |
| — | an invalid key answered 503 "retry", so clients retried forever | 503 | 400 |
| — | the advisory item count never refreshed under sustained writes, so the items scale-up trigger could not fire during a load | count frozen at 0 | refreshed at least every 30 s |
| SPAWN-1 | a spawn used its target as identity; retries ran the same identity twice | duplicate vnode names, "conflicts with committed state" | fresh name keeping 64 bits of the target |
| JOIN-2 | a join past its 2-minute startup budget killed the process, freeing the slot and abandoning the donor's transfer | spawn / die / respawn loop | join continues in the background |
| SCALE-1 | a timed-out scale-up request was retried while its node was still joining | 4 joiners on one saturated donor | one scale-up in flight per vnode |
| HANDOFF-STREAM | a handoff was one synchronous request carrying the region; every round resent it whole | one preload of the region per round, 30 s budget | pages of ≤ 2 000 rows; later rounds send only missing zones (1 052 → ≤ 534 snapshots for 520 zones in 3 rounds); one background job per operation, retries join it |
| KNOW-LAZY | every acknowledged write returned the full knowledge bundle; during a split it grew from 3.6 KB to 2.6 MB and writers spent the run decoding it | scaling benchmarks: 1 600–1 760 of 12 000 writes unavailable, ≈16 acked writes/s | 0 unavailable, both benchmarks pass |
| NET-1 | the mesh used HTTP/1.1–2 over TCP, clients HTTP/3 | — | 3-node TLS cluster: 13 206 mesh calls over HTTP/3, 0 TCP, 0 fallbacks, audit green |

Also added: `retired_exit_seconds` (opt-in: a retired vnode exits after a grace
and frees its slot), `scaling.items_per_vnode` and `scaling.resource_signals`
(deterministic, item-driven scaling for experiments).

## Held

Every audit that read data back passed: 0 acknowledged writes lost, 0 zone
writable on two nodes, under ramps to saturation, one hot zone and concurrent
readers. 0 point reads missed an acknowledged write. COLD-2 folded 8 vnodes into
one after a hot run, with every row on the survivor.

## Open

1. **Knowledge between nodes is a full bundle, and grows with zones moved.**
   (Item 1 of the first register, the synchronous handoff, is fixed:
   HANDOFF-STREAM.) Per-zone transfers live in the knowledge until their
   region certificate: 2.6 MB during one split of 7 vnodes. Every mesh call
   that sees a new version carries it whole, so a split still drops acked
   writes to ≈ 200/s of 4 000 offered in the scaling benchmark. Next: send
   knowledge as a delta against the peer's version, or keep zone transfers out
   of the gossiped bundle. The sentinel's `knowledge_bloat` rule fires on it.
   **Fixed 2026-11-18** (`28fc073`, `ac9f890`): per-zone transfers stay
   between source and receiver as local rows (ZONE-GOSSIP-1, XFER-KEYS-1) and
   intents no longer carry the per-zone forecast (INTENT-LEAN-1); a local
   split keeps the knowledge at 4 kB (was 12-15 MB). See
   [elasticity.md](elasticity.md), "Diagnosis, 2026-11-18".
2. **Abandoned transfers stay in the replicated knowledge.** A failed split left
   the knowledge at 4 to 9.6 MB for 3 vnodes (4.5 KB normally), 32% of the mesh
   bytes. Knowledge must stay O(vnodes) after a failure, not O(zones moved).
   **Bounded 2026-11-18**: an abandoned transfer's per-zone facts are rows on
   its source and receiver only; nothing of it is gossiped.
3. **`/set` defaults to `deep=true`.** A collection total from a naive client is
   an exact scan of every holder: O(N) calls and O(rows) bytes per read
   (344 MB for 6 600 totals). The summary tree (`deep=false`) is the scalable
   read; the default is an API decision.
4. **A stuck transfer fences its zones.** After the elastic phase, reads on a
   live node answered `durable owner fence is closed`: a transfer that never
   finished leaves its zones neither readable nor writable. Same root cause as
   item 1, seen as availability.
5. **`retired_exit_seconds` is premature.** A retired vnode that exited 60 s
   after its witnessed retirement was still contacted by rebalance while zones
   stayed fenced. The agent keeps it off by default; the option stays opt-in
   until what still depends on a retired vnode is known.
6. **Retired vnodes keep their disk.** The copied rows stay on the retired
   source (11 MB each in the 8 → 1 fold) until the process and disk are
   reclaimed.
