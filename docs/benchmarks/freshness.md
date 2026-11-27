# Freshness of collection totals

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

How long after a write is acknowledged does the root total of its collection
count it? The root total is the summary tree's answer (`deep=false`): every
node pushes the sums of its boundary zones to the owner of their parent
(coalescing window 1 s, batches of 512), up to the owner of the root.

Method: [protocol.md](protocol.md). The load sends a probe write every 200 ms
and polls the root total until it counts it. Values are log2 buckets: "≤ 2 097 ms"
means the quantile falls between 1 049 and 2 097 ms.

## REM-112, before and after (2026-11-17)

REM-112: a write that landed in a boundary zone while that boundary was being
pushed was not sent until the next write in that zone or the next poll (5 s, up
to 2 min when idle). Commit `633c486` keeps such an edge queued. The test
`TestWriteDuringEdgePushIsSentNext` fails 3 of 3 times without the fix.

Runs: `exp-20261117-134420-before` (commit `54ba6e5` with `fold.go` from
`da0734b`) and `exp-20261117-134420-after` (`54ba6e5`), launched together.
16 × i4i.xlarge, one vnode each, 2 × c6i.4xlarge load machines, 3 passes each on
a cluster rebuilt from genesis, 40 s steps. Every step of both runs was green:
no acknowledged write lost, no zone with two writers.

| Writes/s per vnode | Acked/s (mean of 3, CV) | Freshness p50 | p99, median of passes | max over passes | Write p99 |
| --- | --- | --- | --- | --- | --- |
| 1 000, before | 16 000 (0.0%) | ≤ 524 ms | ≤ 2 097 ms | ≤ 2 097 ms | ≤ 16 ms |
| 1 000, after | 16 000 (0.0%) | ≤ 524 ms | ≤ 2 097 ms | ≤ 2 097 ms | ≤ 16 ms |
| 2 000, before | 32 000 (0.0%) | ≤ 524 ms | ≤ 2 097 ms | ≤ 2 097 ms | ≤ 33 ms |
| 2 000, after | 31 999 (0.0%) | ≤ 524 ms | ≤ 2 097 ms | ≤ 2 097 ms | ≤ 33 ms |
| 3 000, before | 48 022 (0.1%) | ≤ 1 049 ms | ≤ 4 194 ms | ≤ 8 389 ms | ≤ 524 ms |
| 3 000, after | 47 991 (0.0%) | ≤ 1 049 ms | ≤ 16 777 ms | ≤ 16 777 ms | ≤ 524 ms |

Up to 2 000 writes/s per vnode (32 000/s in total, the busiest node at about
2.1 of 4 cores), freshness is the same before and after, within one bucket: half
the probes are counted at the root within about half a second, all within about
2 s. The fix brings exactness, not a visible gain at this resolution.

At 3 000 writes/s per vnode the nodes run at about 3 of 4 cores and the tail
grows in both runs, more after the fix (p99 of the three passes: 4.2, 4.2,
4.2 s before; 16.8, 16.8, 8.4 s after). About 100 probes per step make p99
close to the maximum: this is a small sample.

## Where the tail comes from at saturation

Every summary of a collection funnels into one node, the owner of its root.
In these runs that is one node for **every** collection: a zone's id is its
location bits followed by its collection, so a root's id is set by the
collection name alone, and the load names its 256 collections and the probe's
with one shared prefix (`<tag>u…`, `<tag>p…`). One node received all
`summary-contribute` calls of the step (1 380 to 2 325), the 15 others none.
Collections with unrelated names would spread their roots over the nodes; a
single heavily written collection would not. At 3 000 writes/s per vnode that
node also takes its share of writes:

| Step at 3 000/s per vnode | `summary-contribute` handled by the root owner, mean | Pushes failed (all nodes) |
| --- | --- | --- |
| before, passes 1 / 2 / 3 | 105 / 135 / 107 ms | 7 / 16 / 12 |
| after, passes 1 / 2 / 3 | 391 / 510 / 131 ms | 65 / 60 / 23 |
| at 2 000/s per vnode, pass 1 (before / after) | 14 / 19 ms | 0 / 0 |

A push that takes more than 2 s (`summaryRPCTimeout`) counts as failed and
backs off 3 s, then 6, then 12 (`summaryRetryAfter`, doubled). Failures are spread
evenly over the 15 nodes that are not the root owner: they are waits on that one
node, not on the network (0 UDP datagram dropped). A backoff of 12 s plus one
window fits a probe seen after 8 to 16 s.

The likely mechanism, not yet proven: accepting a contribution takes the
summary lock `t.Mu` once per contribution, and that lock is also taken by every
local write (`OnLocalWrite`) and while valuing each pending edge, which reads
the store. Keeping stale edges queued (the fix) means more valuations under the
lock on a saturated node. Tracked as its own issue; next step is a lock and CPU
profile of the root owner at 3 000 writes/s per vnode.

## What this shows

- Below saturation the root total of a collection lags an acknowledged write by
  about half a second at the median and at most about 2 s, at 16 000 and
  32 000 acknowledged writes/s on 16 nodes.
- The tail at saturation comes from one hot node, the owner of the roots (all
  of them here, because of how the load names its collections), and from the
  retry backoff of summary pushes. It does not affect writes: every
  step stayed green, and point reads (`deep=true`) do not depend on the summary
  tree.

## Earlier figures

A 32-machine run on 2026-11-16 measured a median of about 1 s and a p99 of
about 4 s (probe every 200 ms). Its result directory is not archived in this
repository; it will be replayed with the protocol above.
