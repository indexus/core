# Cost per acknowledged write

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

Which machines give the most acknowledged writes per dollar, and does packing
several vnodes on one machine help?

Method: [protocol.md](protocol.md), `MODE=passes`, 3 passes, 16 vnodes in every
run, eu-west-3 on-demand prices of 2026-11-17 (i4i.xlarge 0.398 $/h, i4i.2xlarge
0.796 $/h, i4i.large 0.199 $/h). Cost counts node machines only, not load machines.

## Result: four shapes of 16 vnodes

| Shape | Node cost | Highest green rate tested | Acked writes/s per $/h | Write p99 at that rate | First red rate |
| --- | --- | --- | --- | --- | --- |
| 16 × i4i.xlarge, 1 vnode each | 6.37 $/h | 47 991/s (3 000 per vnode) | 7 530 | ≤ 524 ms | not reached |
| 8 × i4i.xlarge, 2 vnodes each | 3.18 $/h | 31 759/s (2 000 per vnode) | 9 980 | ≤ 1 049 ms | 3 000 per vnode: 29 182/s (61%) |
| 4 × i4i.2xlarge, 4 vnodes each | 3.18 $/h | 31 859/s (2 000 per vnode) | 10 010 | ≤ 1 049 ms | 2 500 per vnode: 29 105/s (73%) |
| 16 × i4i.large, 1 vnode each | 3.18 $/h | 31 993/s (2 000 per vnode) | 10 060 | ≤ 1 049 ms | 2 500 per vnode: 29 565/s (74%) |

Every step, green or red, was durable: no acknowledged write lost, no zone with
two writers, exact totals. A red step is one where fewer than 95% of offered
writes were acknowledged: the machines were full.

What this shows:

- **About 1 000 acknowledged writes/s per vCPU, whatever the shape.** A
  2-vCPU i4i.large carries about 2 000 writes/s with one vnode, a 4-vCPU
  machine about 4 000/s with two vnodes and an 8-vCPU machine about 8 000/s
  with four. The 1-vnode run was not saturated at
  3 000/s per machine (3 of 4 cores), so its figure per dollar is a lower bound.
- **Packing vnodes does not buy throughput, only fills machines.** Two vnodes
  on one 4-vCPU machine at 2 000/s each run that machine at full CPU (1.9 cores
  per vnode); the write p99 is then ≤ 1 s against ≤ 33 ms for one vnode per
  machine at the same 2 000/s per vnode. At equal load per machine the
  latencies match: 8 × i4i.xlarge at 1 000/s per vnode and 16 × i4i.xlarge at
  2 000/s per vnode both run 2 000/s per machine with a p99 ≤ 33 ms.
- **The disk is shared.** With several vnodes per machine the group commit p99
  rises to ≤ 131 ms, against ≤ 33 ms for one vnode per machine at its
  highest rate: vnodes on one machine share its NVMe and its fsyncs.

Runs: `exp-20261117-134420-after`, `exp-20261117-161419-cost-xl2`,
`exp-20261117-161419-cost-2xl4`, `exp-20261117-164832-cost-large` in `ops/experiment/results/`. The cost-xl2 run
tested 1 000 / 2 000 / 3 000 per vnode and cost-2xl4 1 000 / 2 000 / 2 500 /
3 000: two campaigns launched side by side shared one plan file, since fixed.

## Next

- The real ceiling of one vnode per i4i.xlarge (ramp above 3 000/s).
- Cheaper storage: c6id / c7gd (local NVMe) and c7i with gp3, now that the
  storage stall is fixed.
