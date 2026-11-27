# Controlled P2P experiments

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

Goal: show, on real machines, whether the network scales horizontally and where
it stops. Every claim below has a measurement, a pass criterion and the
instrument that produces it. A run that fails a safety criterion is a bug, not
a slow result.

## Questions and criteria

| # | Question | Measurement | Pass |
| --- | --- | --- | --- |
| Q1 | Does throughput grow with nodes? | saturated acknowledged writes/s at N = 16, 32, 64, uniform keys | efficiency X(N)/N at 64 ≥ 80% of its value at 16 |
| Q2 | Does latency stay flat at a fixed load per node? | write p50 and p99 at rate ∝ N | p99(64) ≤ 1.5 × p99(16) |
| Q3 | What does the control plane cost one node? | gossip messages/s and bytes/s per node at steady state | messages grow like log2 N; bytes like log2 N × change rate, not N |
| Q4 | How fast does a fact reach everyone? | time from a join to one knowledge version on every node | ≤ trie depth × maintenance tick (GOS-1) |
| Q5 | Do stale routes stay cheap? | share of writes forwarded, hops per write | forwarded share and mean hops do not grow with N |
| Q6 | Does the network absorb a spike and shrink after? | backlog drain time, throughput dip during splits, handoff MB/s, vnode count after calm | splits finish, backlog drains, vnode count falls back (COLD-2) |
| Q7 | Is it safe when things go wrong? | acknowledged writes read back, zones claimed by two nodes, aggregates | 0 lost, 0 double owners, exact totals, under kills and restarts |
| Q8 | Where is the single-zone ceiling? | writes/s on one atomic zone | measured; this is the documented limit |
| Q9 | Is load balanced? | owned bytes and writes per node, max/mean | max/mean ≤ 2 under uniform keys |

Strong scaling (Q1) ramps the offered rate until acknowledged throughput stops
growing. Weak scaling (Q2, Q3) offers a fixed rate per node.

## Instruments

Node, `GET /metrics` on the monitoring port, cumulative since start:

- mesh traffic by request kind, sent and received: calls, errors, request and
  response bytes, time. Gossip (`exchange`) is separated from data and handoff.
- gossip: rounds, partners in the last round, knowledge bytes, knowledge
  version, vnodes known.
- writes: received, acknowledged, refused, forwarded; latency histogram.
- storage: owned bytes, capacity; process: CPU seconds, heap, goroutines.

Driver (`cmd/experiment load`): offered and acknowledged rate, client latency
histogram, every acknowledged key written to a log for the audit.

Observer (`cmd/experiment observe`): polls every node's `/metrics` and writes one
JSON line per node per sample. Derived series (per-node rates, distinct
knowledge versions, vnode count) come from that file alone.

Audit (`cmd/experiment audit`): reads back every acknowledged key, checks that
no zone has two writable owners, and compares totals.

## Scenarios

| Name | Load | What it stresses |
| --- | --- | --- |
| normal | uniform keys over many collections, constant rate per node, then a ramp | Q1, Q2, Q3, Q5, Q9 |
| spike | base load, then 10× on one prefix for 5 min, then 10 min calm | Q6, splits under load, COLD-2 folds |
| adversarial-hot | every write on one zone | Q8 |
| adversarial-deep | keys sharing ever longer prefixes | deep trie, GOS-1 depth, many splits |
| adversarial-churn | normal load while 10% of nodes are killed and restarted | Q7, stale routes, recovery |
| join | add nodes under load | Q4, handoff MB/s |

## Where it runs

1. Local real processes on one host: validates the chain and finds defects.
2. AWS, in a disposable stack separate from the live deployment, at 16, 32
   and 64 c6i.large nodes plus load machines. The campaign is one command from
   the operator's shell; results land in `ops/experiment/results/`.

### Replaying the steps insertion workload

AWS `MODE=steps` draws a 32-bit `STEP_SEED` and a six-character collection prefix `STEP_TAG` per campaign. Both are saved with the source manifest. Set both from an earlier campaign to compare the same sequence of offered keys on fresh clusters. Pass one uses `STEP_SEED`. Each subsequent pass increments it and has its own item ID prefix. A single producer chooses the collection and the 96-bit random location and assigns the write sequence number before worker dispatch. The sequence number distinguishes items even when locations coincide. Retries reuse the original request ID, location and payload. The ACK audit records the keys that actually finished, which can differ between runs due to scheduling and load shedding.
