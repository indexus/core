# Horizontal scaling

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

Does acknowledged write throughput grow linearly with vnodes, at the same
load per vnode, with latency that does not grow with the network?

Method: [protocol.md](protocol.md), `MODE=passes`, 3 passes per size, each on a
cluster rebuilt from genesis, 40 s steps, uniform writes over 256 collections.
Machines: i4i.xlarge (4 vCPU, local NVMe), one vnode each, eu-west-3, TLS and
HTTP/3 between nodes; load from c6i.4xlarge machines (2 at 16 vnodes, 4 at 32),
never above 2.7 of their 16 cores.

## Result: 16 → 32 vnodes

| Writes/s per vnode | 16 vnodes: acked/s (CV) | 32 vnodes: acked/s (CV) | Efficiency per vnode, 32 vs 16 | Write p99, 16 / 32 | Busiest node, cores of 4 |
| --- | --- | --- | --- | --- | --- |
| 1 000 | 16 000 (0.0%) | 31 998 (0.0%) | 100% | ≤ 16 / ≤ 16 ms | 1.1 / 1.3 |
| 2 000 | 31 999 (0.0%) | 63 999 (0.0%) | 100% | ≤ 33 / ≤ 33 ms | 2.1 / 2.3 |
| 3 000 | 47 991 (0.0%) | 95 987 (0.0%) | 100% | ≤ 524 / ≤ 524 ms | 3.0 / 3.1 |

Every step of the 18 was green: at least 99.95% of offered writes acknowledged
in the steady part, no acknowledged write missing in the audit read-back, no zone
with two writers, every collection total exact, 0 UDP datagram dropped.

- **Linear.** Doubling the vnodes doubles acknowledged throughput at every load
  level tested, to the write (96 000/s at 32 vnodes, 48 000/s at 16). The pass
  criterion of [p2p-experiment.md](p2p-experiment.md) Q1 is efficiency ≥ 80%.
- **Flat latency.** At a fixed load per vnode the write p99 is in the same log2
  bucket at 16 and 32 vnodes (Q2: p99(N) ≤ 1.5 × p99(16)). The tail grows with
  load per vnode, not with network size: at 3 000/s per vnode the nodes are at
  three quarters of their CPU.
- **Balanced.** The busiest node handles 3.2% of the mesh requests at 32 vnodes
  (mean 3.1%) and 6.4% at 16 (mean 6.25%): max/mean ≤ 1.03 (Q9: ≤ 2).
- **Not yet the ceiling.** 3 000 writes/s per vnode is the highest rate tested
  in passes, and every pass was green. A single-pass ramp (`MODE=push`) on 16
  i4i.xlarge in an earlier session reached 52 440/s; not archived here.

Runs: `ops/experiment/results/exp-20261117-134420-after` (16 vnodes, commit
`54ba6e5`) and `exp-20261117-161419-scale32` (32 vnodes, commit `6a7c8fe`).
Between these commits only documentation and experiment scripts changed.

## Result: 64 vnodes

Run `exp-20261117-164829-scale64`: 64 × i4i.xlarge, one vnode each, 6 load
machines (at most 3.3 of 16 cores each), commit `a0443a3`, 3 passes.

| Writes/s per vnode | Pass 1 | Pass 2 | Pass 3 | Efficiency vs 16 (passes 1 and 3) | Write p99 (passes 1 and 3) |
| --- | --- | --- | --- | --- | --- |
| 1 000 | 63 996 | 63 997 | 63 996 | 100% | ≤ 16 ms |
| 2 000 | 127 994 | 110 901 (red, 87%) | 127 994 | 100% | ≤ 33 / ≤ 66 ms |
| 3 000 | 190 787 (99.4%) | 74 295 (red, 39%) | 190 578 (99.3%) | 99.3% | ≤ 524 ms |

Passes 1 and 3 are linear from 16 to 64 vnodes at every rate: 190 700
acknowledged writes/s at 64 vnodes against 47 991 at 16 (4 × 47 991 = 191 964).
The busiest node handled 1.6 to 1.8% of mesh requests (mean 1.56%).

Every step of the three passes was durable, the red ones included: no
acknowledged write missing in the audit read-back, no zone with two writers,
every total exact.

**Pass 2 was red, and not because of scaling.** Its cluster was built with an
unbalanced layout: one vnode owned a double cell (2.0 × the mean owned bytes and
writes), while two vnodes shared one normal cell, half each. At 2 000 writes/s
per vnode that vnode ran at twice the load of the others and saturated; relays
and retries then piled onto it (it handled 7.5 × the mean mesh requests at
3 000/s per vnode). Cause: a joiner asks the bootstrap for a JOIN-1 hint, the
half of the largest cell. The bootstrap learns of earlier joins through gossip
(10 s rounds), while joins follow each other every few seconds, so two joiners
can halve the same cell. Fix and regression test: issue REM-120. Until then, a
run must check owned-bytes max/mean ≤ 1.5 after the build and before the load.

## Replay after the elasticity work, 2026-11-19 (`5ef86c8`)

Same shape (i4i.xlarge, one vnode each, c6i.4xlarge loaders, HTTP/3, fixed
rates per vnode), after the zone-row, region-digest and zoneHolder changes
of 2026-11-18/19 and with replication off (its default). Results under
`ops/experiment/results/aws-i4i16-*`, `aws-tail16-*`, `aws-i4i32-*`,
`aws-i4i64-*`.

| vnodes | offered | acked | p99 | reference (2026-11-17) |
| --- | --- | --- | --- | --- |
| 16 | 16 000/s | 15 996/s | 16 ms | 15 996/s, 16 ms |
| 16 | 32 000/s | 31 992/s (3 passes) | 33 ms (3 passes) | 31 991/s, 33 ms |
| 16 | 48 000/s | 47 973/s | 262 ms | 47 975/s, 524 ms to 1 s |
| 32 | 96 000/s | 95 945/s | 262 ms | 96 000/s |
| 64 | 128 000/s | 127 964/s | 33 ms | 127 960/s, 33 to 65 ms |
| 64 | 192 000/s | 190 585/s | 524 ms | 190 937/s, 524 ms |

No regression: the same throughput at every size, the same or a lower p99.
A single pass at 32 000/s first showed p99 66 ms (seven bursts of about one
second); three passes on fresh clusters gave 33 ms each, with fewer slow
requests than the reference (0.26 to 0.38 % above 32 ms against 0.8 to
0.95 %). Every audit passed. An A/B on 2 × c6i.2xlarge with EBS (9a73d5f
against HEAD) gave the same at 16 000/s and +9 to +14 % under overload.

## Earlier measurements

Campaign 8, 2026-11-16, 16 × c6i.xlarge (gp3 disks), one pass per step:
per-vnode capacity equal at 4, 8 and 16 vnodes (1 518 writes/s per vnode green
at every size): see [p2p-findings-2026-11-15.md](p2p-findings-2026-11-15.md).
That tree had the storage stall fixed later ([storage-stall-2026-11-16.md](storage-stall-2026-11-16.md)).

## Next

- Fix REM-120, then replay 64 vnodes in 3 passes; then the ceiling per vnode
  with a ramp above 3 000/s, and 128 vnodes.
- Gossip cost per node at 16, 32 and 64 (Q3: grows like log N).
