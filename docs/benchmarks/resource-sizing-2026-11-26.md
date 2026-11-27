# Vnodes, machines and transfer headroom — 2026-11-26

This is a resource-planning experiment, not a distributed execution benchmark.
The question is how to pack several vnodes per machine without spending the
headroom needed for incoming/outgoing copies and short foreground bursts.
The model and both dashboards share the same fleet representation. The model
does not execute authority, disk durability, reads, ACKs or summary propagation.
It cannot establish their correctness or predict write p99.

## What was integrated first

Claude's bounded planning sample, `05f9c86`, is integrated on main. It samples
at most 512 spatial ranks instead of reading statistics for every zone on each
split decision. Review found that equal integer weights lost the remainder:
1,023 zones sampled at 512 ranks were represented as only 512 zones.
`ddd4b95` assigns each sample its rank-bin width, restoring a represented total
of 1,023 in that regression case. The sample remains advisory: a rare hotspot
or heterogeneous zone sizes can still be estimated poorly. Treap lookup work
is expected O(512 log Z), plus data rows inspected within sampled zones; the
cold index build is not bounded independently of Z.

## Reference experiment

The deterministic JavaScript kernel is in `ops/experiment/sizing/model.mjs`.
Run `node ops/experiment/sizing/sweep.mjs /absolute/output` to reproduce all
465 cases. The output includes exact configuration, model SHA-256, summaries,
CSV, four example traces, and events for interactive replay. The checked-in
CSV and provenance accompany this document under `resource-sizing-20261126/`.
Tests exercise conservation of workload, progressive installation, bounded
receiver admission, dirty-copy stalls, metadata cost, causal forecasting,
incremental collection and explicit model scope exhaustion.

The controller uses the current load and a trailing 20-second slope. It never
looks ahead at an unannounced hotspot or doubling. It forecasts over boot time,
requests one boot per control round, counts boots once, and cannot use them
until ready. Placement is a fleet-wide reference calculation, **not** the
deployed peer-to-peer planning algorithm.

| Input | Reference value | Evidence status |
| --- | --- | --- |
| Logical vnodes | 64 initially; 512 model bound | experiment scope |
| Machine | 4 cores, 16 GiB RAM, 900 GiB disk | configurable instance-shaped model |
| CPU/space/IO ceiling | 60% | requested control target |
| Anticipation / cold threshold | 45% / 35% | hypotheses; sweep 40–55% anticipation |
| CPU per write | 1,300 µs | hypothesis; sensitivity 650–2,600 |
| Vnode base RAM / zone metadata | 192 MiB / 2,048 B | hypotheses; both swept |
| RAM cache / disk amplification | 2.5% of data / ×2 | hypotheses |
| Owned data per item | 870 B | two earlier AWS node counters; workload-specific |
| Data per vnode | 0.25 GiB; volume scenario 64 GiB | experiment inputs |
| Items per zone | 1; 128 in large-volume cases | major density assumption, exposed in UI |
| Copy budget | 10% CPU shared per host; at most 100 MiB/s | model policy, not deployed pacing |
| Copy cost / disk and network service | 250 MiB per core-second / 500 MiB/s | hypotheses, not measured capacities |
| Boot delay / collection delay | 45 s / 20 s | hypotheses, varied in UI/sweep |
| Ready reserve | 15% of observed CPU, converted to empty reception capacity | model policy; sweep 0/10/20/30% |

There is at most one incoming and one outgoing copy per physical host in the
model. The shared budget covers both, including co-resident operations. If a
source is already overloaded, a small rescue budget lets copies progress; that
can exceed 60% and is recorded. Destination admission checks projected CPU,
RAM, full incoming disk occupancy and IO. It never clips resource demand to
make a chart appear compliant.

Installed authority/load advances as copying progresses. Uniform ordering
moves proportional load; the optional hot-first scenario assumes the first
10% of bytes carry 50% of load. That assumption is not a claim about actual
placement. Collection can release already installed portions after a delay,
or retain them until the full vnode completes. Uncopied data is never collected.
Dirty updates consume transfer capacity and can prevent convergence. Data
growth assumes offered inserts proceed; under overload this is a conservative
storage-demand assumption, not an assertion that those writes were ACKed.

## Results and interpretation

Means across seeds 7, 19 and 41, adaptive copy policy, 15% reserve:

| Workload | Vnode slots/host | Machine·min | Host·s beyond 60% | Peak hosts | Copies unfinished at 20 min |
| --- | ---: | ---: | ---: | ---: | ---: |
| Ramp 2k → 16k/s | 4 | 373.6 | 0 | 20.0 | 0 |
| Ramp 2k → 16k/s | 5 | 321.0 | 0 | 18.0 | 0 |
| Unannounced local hotspot | 4 | 386.4 | 34 | 22.3 | 0 |
| Unannounced local hotspot | 5 | 335.1 | 28 | 19.7 | 0 |
| Instantaneous doublings | 4 | 375.3 | 0 | 20.0 | 0 |
| Instantaneous doublings | 5 | 330.2 | 0 | 18.0 | 0 |
| 64 GiB/vnode | 4 or 5 | 808.3 | 0 | 46.7 | 3 |

Five slots save about 14.1% machine·minutes relative to four on this ramp.
That does not establish five as a universal optimum: base memory, zone density,
per-write CPU and copy cost change the answer. Large data is limited by resource
admission and copy duration rather than slots; allowing more slots does not
make the three outstanding copies disappear.

The hotspot is deliberately not a success case for a hard 60% guarantee.
Its estimated CPU need can exceed one whole machine before migration relieves
it. Host·seconds count affected hosts and duration, not a global pause. A larger
empty pool cannot instantaneously move installed authority or memory. If load
can double without notice, foreground F and copy C need approximately
`host_base + 2*F + C <= 60%` to remain below the target before relief. With a
10% copy budget this leaves less than 25% for foreground before the burst.
Otherwise admission/backpressure or transient excess is unavoidable.

## Production changes and the remaining gap

The AWS default is now five process slots, with resource signals enabled.
The item-trigger default rises from 20,000 to 500,000; it remains configurable
and can be disabled with `ITEMS=0`. No automatic item count is inferred from
disk percentage: physical space and owned logical bytes are distinct signals.
The provisioner exposes `READY_RESERVE_PCT` (default 15). It converts observed
CPU into one immediately admissible reception offer per eligible host,
excluding the selected target and reservations. The hook subtracts accepted
boots once and retains budget/quota caps. Unlike the conservative simulation
reserve, production may count spare resource capacity on an occupied machine.
If CPU/RAM/disk facts are missing it retains its explicit slot-based fallback.
Reserve replenishment runs on spawn requests, not continuously during idle.
The model can consolidate and release idle hosts; this change does not add
a real EC2 idle-release controller. Experiment TTL/teardown still bounds those
machines.

The real admission estimate remains 1.75 cores for a whole joining vnode. It
includes foreground writes and is based on one earlier observation, not pure
copy cost. Replacing it by 10% without enforcing copy pacing would overbook
receivers. The model's host-wide adaptive copy limiter and general relocation
of an existing vnode process are **not implemented by this change**. Current
spawn moves delegated zones into new vnodes. The shared view labels the actual
source and only the transfer progress which is measured.

Before a new AWS campaign: qualify receiver cost separately from foreground,
both endpoints' CPU/IO and headroom, real initial/data density, concurrent
donors sharing one host, ACK and regional-read audits, and the transition from
first installed lot to first direct write. Compare four/five slots with the
same source and pinned loader. Record CPU/RAM/disk plus workload latency; a
resource model's zero overflow is not a write-latency result.

## Ideas from other schedulers

Kubernetes distinguishes resource packing scores and capacity constraints;
its scheduling requests are reservations, not instantaneous CPU measurements.
Indexus likewise needs admission before ranking hosts. See
[resource bin packing](https://kubernetes.io/docs/concepts/scheduling-eviction/resource-bin-packing/).

The historical VMware DRS performance paper treats migration as a cost/benefit
decision; hysteresis and migration costs matter alongside utilization. This
informs the reference model, without importing its 2016 defaults as current
settings. See [the DRS paper](https://www.vmware.com/docs/vsphere6-drs-perf).

KubeVirt exposes migration concurrency and bandwidth controls. Pre-copy can
fail to converge if dirty work outpaces transfer. Indexus needs those resource
lessons, while retaining its own installed-authority rule; VM post-copy does
not supply an ownership proof. See
[live migration](https://kubevirt.io/user-guide/compute/live_migration/).

CockroachDB's historical rebalancing note separates balancing storage from
balancing load, and its settings expose snapshot controls. That separation is
more useful than a universal item cap. See the
[historical note](https://github.com/cockroachdb/cockroach/blob/master/docs/tech-notes/rebalancing.md)
and [settings](https://github.com/cockroachdb/cockroach/blob/master/docs/generated/settings/settings.html).

Related: [shared dashboard contract](../../ops/experiment/dashboard/README.md),
[placement policy](../design/placement-and-resources.md),
[next AWS qualification](aws-16k-sizing-2026-11-26.md).
