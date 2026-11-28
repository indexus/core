# Placement of a new vnode and machine resources

**Status: integrated on main, revised 2026-11-26. The resource reserve and
shared dashboard below are locally tested; not yet qualified on AWS. The four
sections marked "fix-host" (one decision per machine, backlog in the reserve,
copy budget per host, safe jobs before the end of the join) answer the AWS run
of 2026-11-26 and are tested locally only: nothing in them ran on AWS or on a
real multi-machine cluster.** Code: `internal/autoscale/placement.go` (`Place`),
`internal/autoscale/reserve.go` (`ReadyReserve`),
`internal/adapter/provisioner/placement.go` and `pressure.go`,
`internal/mesh/copy_budget.go`, `indexus/worker.go`, `cmd/experiment/agent.go`
(`GET /facts`), `ops/experiment/elastic/facts.sh` and `spawn.sh`,
`internal/app/node/main.go` (monitoring and safe jobs before the join).

A scale-up adds a vnode (an address). Its XOR address is chosen by the donor
(`PreferNear`); this document is only about the **machine** that hosts it.
Until now the hook took the machine with the most free slots.

The [AWS diagnosis](../benchmarks/aws-resource-direct-2026-11-26.md) identifies
remaining gaps between this admission policy and actual scale triggering.
The [P2P and virtualization comparison](../research/p2p-vnodes-and-physical-elasticity.md)
proposes host-level coordination and shared migration budgets; these proposals
are not yet implemented by the current policy.

## Why

Receiving a transfer is not free. For the whole join the receiving machine
pays CPU, writer time and disk I/O, on top of the writes the new vnode starts
to serve after about half a second.

Measured on AWS on 2026-11-26 (i4i.xlarge, 4 vCPU, 2 slots per machine): two
machines that each hosted two *receiving* vnodes at the same time reached 87 %
and 89 % host CPU, while the older donor machines stayed at 28-49 %.
Throughput collapsed. The cap of 32 machines was not reached and three spare
machines were up, empty. The slot count could not see any of this.

## What established systems do

Read on 2026-11-26. "Adopts" is what Indexus takes; the numbers are those of
the cited pages and may differ between versions.

| System | Rule | What Indexus adopts |
| --- | --- | --- |
| Cassandra | Outbound streaming is capped per node (`stream_throughput_outbound`, 200 Mbps when unset). The operations guide advises against adding several nodes at once and asks for two minutes between node starts. | One join at a time per machine. A rate cap on the transfer itself is not adopted yet (open point 1). |
| ScyllaDB (tablets) | One load balancer plans all migrations, moves tablets from more used to less used nodes by disk use, balances shards inside a node, and aims to keep streaming busy without overloading nodes. Several nodes may be added in parallel. | The target is chosen by the load of the machine, and a move inside a machine is a legitimate choice (`local`). I did not find the per-shard concurrency number in the pages read. |
| CockroachDB | A store 92.5 % full is no rebalance target, 95 % full receives nothing and is drained. Snapshots are rate limited (`kv.snapshot_rebalance.max_rate`, 32 MiB/s) and a store limits the snapshots it accepts at once. Load rebalancing acts only beyond a deviation from the mean (CPU 0.1, range count 0.05). | A fullness floor excludes a target; incoming transfers are bounded per receiver; a threshold with a margin rather than "strictly least loaded". |
| TiKV / PD | Store limit: a token bucket per store for `add-peer` and for `remove-peer`, per minute. `max-snapshot-count` (64) bounds what one store sends or receives at once. Above `low-space-ratio` (0.8 used) a store receives no data. Hot-region moves have their own small limit (4) and need a region to stay hot for minutes. | Per-machine bound on incoming joins, separate from the global rate (the provisioner cooldowns). A space ratio excludes a target. Heat must be sustained before acting: Indexus already has it on the donor (`PressureHold`, the write EWMA with its 15 % deadband). |
| FoundationDB | Relocations are bounded per source and per destination server (`RELOCATION_PARALLELISM_PER_SOURCE_SERVER`, `..._PER_DEST_SERVER`) and globally. A destination team must have free space above a pivot (the median, clamped between 5 % and 30 % free). Moves in flight count as load of the destination. | A join in flight counts against the machine (`Receiving`, and a machine just chosen is remembered for 15 s). Free space is a condition of the target, not a score. |
| Kafka | Reassignment traffic is throttled on the sending and on the receiving broker (`leader.replication.throttled.rate`, `follower.replication.throttled.rate`), chosen above the write rate and below what leaves room for clients. | The receiving side pays too: the budget is on the target. Same open point 1 for the rate. |

Shared principles, each checked against at least two of the systems above:

1. **Bound the incoming transfers per receiver.** TiKV store limit and
   `max-snapshot-count`, FoundationDB per-destination parallelism, CockroachDB
   snapshot reservation, Cassandra's one node at a time.
2. **Throttle the transfer so that foreground keeps headroom.** Cassandra,
   CockroachDB, Kafka, TiKV (`snap-io-max-bytes-per-sec`). Kafka states the
   bound on both ends.
3. **Exclude a target under a fullness floor** instead of only ranking it
   lower. CockroachDB 92.5 %/95 %, TiKV 0.8, FoundationDB pivot.
4. **Judge the physical unit that pays.** CockroachDB and TiKV reason per
   store, FoundationDB per storage server, ScyllaDB per node then per shard.
   None judges the logical range alone.
5. **Count work in flight as load of the target.** FoundationDB in-flight
   penalties, TiKV pending peers.
6. **Hysteresis.** CockroachDB deviation thresholds, TiKV hot-region cache
   hits, FoundationDB's minimum gap before a move.

One assumption of the brief is **not** confirmed as a general rule: "prefer
adding empty capacity over loading a busy machine". These systems place on the
least loaded eligible store; an empty store wins because it is the least
loaded, not because it is empty. Indexus also budgets the measured cost of a
join: reusable remote capacity needs complete, recent load observations and
resource headroom; otherwise an empty target comes first.

## The Indexus policy

### Facts

Each machine agent answers `GET /facts`:

| Fact | Source |
| --- | --- |
| `running`, `free` | the agent's slots, as `/count` |
| `cpu_pct` | Maximum of summed vnode CPU and whole-host CPU when `/proc/stat` is available. I/O wait and steal count as unavailable capacity. `host_cpu_ok` explicitly distinguishes the scope. |
| `receiving` | vnodes whose `/metrics` says `"joining": true`, plus live vnodes with no CPU reading yet (just started, or an old node that does not answer while it joins) |
| `vnodes` | their names, to find the donor's machine |
| `mem_free_pct`, `disk_free_pct` | `autoscale.SampleResources` on the slot directory: the collector the node uses |
| `cores` | `runtime.NumCPU()` |

The agent collects resource facts separately from the experiment fleet observer. `handoffs.open` counts the *donor's* operations, so
it does not say who receives; `autoscale.pressure` exists only when autoscale
ticks. The node now says it directly: `joining` is true from the start of its
join until the join has returned (`Node.Joining`).

### Decision

`autoscale.Place(machines, donorVNode, canBoot)` is pure. It returns a kind, a
machine and a one-line reason. A machine is a **target** when it has a free
slot, no vnode still joining, enough disk and memory, and
`cpu_pct + receiver budget <= 60`.

In order:

1. `existing`: an admissible remote machine, with known recent donor/target
   counts, **that the split relieves**. Reuse its slots before consuming a spare.
2. `new`: an empty machine that is a target (the warm spares); then any other
   admissible remote target when pair load is unknown.
3. Otherwise, while a machine can still boot: `new` with no machine. The hook
   boots one and fails the request; the donor asks again (it already does so
   when there is no free slot).
4. At the machine cap only: `local` if the donor's machine is a target, then
   any other target for process-bound requests. For a shared host CPU request,
   the fallback requires a remote target whose CPU including the receiver budget
   is below the source CPU. This allows a peer with many cold items to take
   traffic. Both paths retain receiver admission checks.
5. Refuse the spawn if no admissible machine exists. A quota or budget limit
   never authorizes bypassing resource admission.

**Why local comes last.** Under uniform writes a machine's write load follows
the items its vnodes own: at the stop of the AWS run of 2026-11-26 every
machine's write share matched its item share within one point. A receiver on
the donor's own machine moves nothing off that machine and adds the transfer
to it. In that run the first splits, at 2k/s and 6-16 % CPU, passed the local
headroom check; two machines then held three and four vnodes and carried half
of the writes of fourteen machines at 4k/s, and nothing moves a vnode off a
machine for CPU afterwards.

**Relief.** Agents report each vnode's advisory owned items (`items` in the
facts), only after an actual count and for at most the same 30-second age
budget used by node recounting. Never-counted or stale zeroes are unknown,
not empty. An unidentified running process also makes its host count unknown.
For uniform writes, estimate the transferred load as `m = ceil(v/2)` items from
a donor vnode owning `v`. For `m > 0`, the donor's load decreases. A remote
target owning `t` lowers the peak across the pair if
`(t + m) / target_cores < d / donor_cores`, where `d` is the donor host total.
With equal core counts, moving 25 from a host at 100 to one at 65 puts them
at 75 and 90. The larger post-split load falls from 100 to 90. Core count
approximates capacity across machines with similar per-core throughput.
Among eligible remote targets with known relief, the agent uses the same six
slot observations as its CPU window to estimate applied writes per second.
Each live process must supply a monotone counter over a recent window of about
ten seconds. The oldest endpoint must still be within ten seconds at placement.
When every candidate supplies this rate, the provisioner first compares the
remaining capacity in whole receiver CPU budgets, bounded by available slots.
At equal capacity it prefers lower applied traffic. For ties or incomplete
traffic observations the previous item-count, then CPU, ordering applies to
the whole candidate set.
The relief check needs only the donor and candidate counts: an unrelated
unknown machine cannot disable a known pair. CPU is a short observation window. Owned items are a proxy for uniform load; a hot spot
with few items is not seen by it (CPU admission still applies). Small hot-area
moves need not halve the donor, so this remains a conservative item heuristic,
not a proof of CPU relief or optimal packing. Four or five slots can be reused
on a remote machine; their actual admission still depends on physical headroom.

Two exceptions. If the entire fleet lacks resource observations, the host with the most free slots is selected (`slots`). A single host missing CPU is ineligible in an instrumented fleet. If an *empty* machine is already
under its disk or memory floor, one more would be no better, so step 4 is
skipped. This is the case of a local run on a laptop with a full disk.

### Constants

| Constant | Value | Why | Status |
| --- | --- | --- | --- |
| Instrumented receiver forecast | At least 0.50 cores | The agent uses the largest recent joining-process estimate among its live slots. A sample uses six CPU readings and expires after one minute. | policy to qualify |
| `receiverCores` (fallback and reserve-floor sizing) | 1.75 cores | two receivers on 4 vCPU used 87-89 %, about 1.75 cores each. It includes the writes the new vnode takes over, which land on the target too. | one measurement |
| `targetCPUMaxPct` | 60 % | retain burst and transfer headroom | control target, not a hard cap |
| `maxReceiversPerMachine` | 1 | two at once is what saturated the machines | one measurement |
| `targetDiskMinFreePct` | 40 % | the node's own `DiskMinFreePct`: lower, the new vnode would ask for a scale-up at once | aligned with the node |
| `targetMemMinFreePct` | 40 % | 100 minus the node's `MemRefusePct`: lower, the new vnode would refuse writes | aligned with the node |
| `placementMemory` | 15 s | a machine just chosen counts as receiving until its agent shows the vnode | assumed |
| CPU window | 10 s | sees a join start, ignores one commit burst | assumed |

On a four-core host the initial forecast of 0.50 cores admits a receiver up to
47.5% measured CPU. A recent 1.5-core observation lowers that threshold to
22.5%. Only one receiver is admitted at once. The reserve-floor formula still
uses the original 1.75-core unit so this change alone does not multiply spare
boots. Agents export the forecast and refusal reason in their machine facts.

In the first 131 seconds of `aws-64k-ebs-1128-064141`, 137 host observations
had a free offer before applying the CPU filter (free slot, no receiver,
measured CPU, sufficient memory and disk). Replaying only the initial CPU
forecast admitted 116 observations at 0.75 cores, 124 at 0.50, and 129 at
0.25. Across the 400-second injection, 924 consecutive 2–10 second samples
of the same joining process gave a median CPU cost of 0.186 cores and p90 of
0.355. 41 intervals exceeded 0.50 cores and 279 exceeded 0.25. These samples
omit short joins and measure the process, excluding kernel work outside it.
They motivate a starting estimate; they do not measure post-change EC2 count,
ACK latency or machine utilization.

A physical move retains the vnode address, increments its endpoint epoch and
transfers its rights before publication. The node supports this through its
move command. The AWS pressure path requests new addresses through the
provisioner. Its placement counters classify where those new processes start.
Monitoring reports transfers of a physical move separately and records confirmed
zone installations and the age of the last one for each open operation.

### Wiring

```
vnode under pressure -> POST /v1/scale (requester_id, prefer_near)
provisioner: facts.sh -> one JSON line per agent
             autoscale.Place -> kind, machine, reason
             spawn.sh with INDEXUS_PLACE_KIND / _AGENT / _REASON / _MACHINES
                             INDEXUS_PLACE_CAP_AGENT / _CAP_REASON
spawn.sh:    warm pool as before, then POST <agent>/spawn
```

`spawn.sh` preserves the chosen target even when the inventory changes between
planning and application. If that target disappears, the spawn fails and is
retried; it is never replaced by an unchecked slot. Without resource facts the
slot-count experiment mode remains available, but cannot qualify a CPU target.

### Independent pressure clocks

The donor tracks four fixed-size clocks: memory rise, CPU rise, projected
memory level and CPU level. Each clock starts when its own predicate becomes
true and resets when that predicate becomes false. Cooling one resource does
not cancel another resource's evidence. A fresh rise cannot hide a qualified
high level. Among qualified signals, memory rise, CPU rise, memory level and
CPU level keep that priority order.

Default limits and durations are unchanged (CPU 60 %, rise hold 12 s, level
hold 30 s). Continuous CPU above its limit therefore qualifies on the first
tick at or after its hold, despite intermittent rises or memory fluctuations.
This is a bound on signal qualification, **not on completed scale-up**: pending
capacity, cooldown, an outgoing transfer or the absence of a valid relief plan
may still delay a spawn. Successful launches and landed capacity reset the
clocks as before. Evaluation is constant work under the controller's existing
mutex, without storage or network I/O.

A process start is also not installed authority. A direct `/v1/scale` request
does not set the donor's `Wanted()` state. The spawned join can correctly be
refused with `ErrNotNeeded` if the donor no longer wants help and the operation
has not begun or activated. Once begun, the existing handoff/recovery rules
remain in force; neither a timeout nor an orchestrator response restores a
consumed write right. See the [timer and provisioner qualification](../benchmarks/pressure-placement-2026-11-26.md).

### Vnodes per physical machine

In the direct 16k experiment a vnode asks for relief at 50 writes/s. The
physical host target is 50% CPU. Additional processes share that host while
the receiver admission test allows another copy. Each host offers one incoming
join at a time. Stale measurements of a live process, missing host CPU,
insufficient memory or disk headroom suspend that offer. In automatic mode
the agent creates its process table and allocates network endpoints on demand.
The bootstrap slot retains its configured address. Other slots keep their assigned
ports across process restarts. The host OS must have ports and file descriptors
available for each admitted node.

For an evenly loaded fleet, writes/s per host at the CPU target divided by
the split rate gives a reference number of vnodes per host. The operating
density changes with traffic skew, the volume already stored, and the work of
the copies. The [50/s simulation](../benchmarks/aws-placement-100-20261127/granular50/rapport.md)
shows how much this estimate changes while a fleet is growing.

### Ready capacity during a wave of splits

The provisioner sizes the reserve in *receiver offers*. One machine can admit
one incoming join at a time, even when it has several free vnode slots.
`-readyReservePct` (AWS `READY_RESERVE_PCT`, initially 15) sets the spare
capacity to keep for the next requests. The calculation also accounts for
the splits queued on the source machines.

For example, ten 4-core hosts at a weighted average of 20% need zero
speculative spares. A donor without a usable remote destination still creates
demand. At 50% on the same fleet, 20 cores are used, 12 cores correspond to
30%, and 15% of the 8-core excess is 1.2 cores. Rounding up by the 1.75-core
receiver budget gives one spare offer. An existing host with enough headroom
already supplies that offer. The machine can be partly occupied.

Among known-load remote candidates passing admission, the group below 30% CPU
has priority. On a 4-core host at 29%, a new receiver would bring the estimate
to 72.75%; on 8 cores the estimate is 50.875%. Crossing the low-use mark alone
does not change the cost of a copy. The local replay of these rules is documented
with the [AWS placement trial](../benchmarks/aws-placement-100-2026-11-27.md).

Both sides of this calculation matter. On November 26, `booting=1 deficit=0`
repeated while several donors were waiting. On November 27, four source hosts
with up to 15 waiting vnodes triggered 24 boot requests before the hook's
snapshot first showed more than four live agents. The revised reserve uses
the copy budget on each source and reconciles accepted launches with the
same agent snapshot used for placement. **This revision has been tested
locally. Its effect on AWS fleet size and transfer wait time is experimental.**

| Term | Meaning |
| --- | --- |
| `floor` | Spare receiver offers based on CPU used above 30% of the surveyed fleet’s aggregate core capacity. Multiply the positive excess by the reserve percentage, divide by `receiverCores`, and round up. At or below 30% the speculative floor is zero. |
| `backlog_vnodes` | Distinct requesters whose last placement required a new remote machine. Retries refresh a request for 15 s (`waitingMemory`). Placement, refusal or expiry removes it. |
| `pending` | Concurrent offers those requests can use. For each identifiable source, count the smaller of its backlog and its positive `/facts.copy_slots`. Count a request individually if its source is unknown, ambiguous or has no known copy limit. |
| `ready` | Machines that pass `ReceiverOffer`. A machine selected for this spawn, recently promised to another spawn, or already receiving is excluded. |
| `waiting_remote` | This request needs a new machine because the placement search found no eligible destination. For example, the donor itself can have a free slot and meet generic receiver admission. A remote offer must still be available or on its way. |
| `warming` | Empty machines that first reported less than 60 s ago and are expected to become admissible once startup CPU subsides. Disk and memory admission still apply. |
| `unissued` | Locally reserved launches whose token has yet to appear in EC2, counted per instance within each batch. |
| `unseen` | Recently launched pending or running EC2 data hosts absent from the agent facts used by the provisioner. |

```
need   = floor + pending - ready
if waiting_remote: need = max(1, need)
boots  = max(0, need - warming)
launch = min(room, max(0, boots - unissued - unseen))
```

For example, four waiting vnodes on a source with one copy slot count as four
in the backlog and one in `pending`. With no ready or warming receivers and
a floor of one, the provisioner asks for two machines to be ready or booting.
If EC2 has already accepted one unsurveyed launch and there is room, the hook
launches one more. After a placement the remaining vnodes stay in the backlog
while their requests are refreshed. A later request reevaluates the available
slots and resources, so more machines can start as successive waves need them.

The hook matches launch reservations to EC2 client tokens, and compares EC2
private IPs with the surveyed agents. It retains credits within the existing
180-second boot window. An instance visible in EC2 occupies one place at the
budget, quota or operator cap. Reservations still missing from EC2 consume
additional places. Older unsurveyed instances stop contributing receiver
offers but continue to occupy places while in a tracked lifecycle state.

Read `backlog_vnodes` on the placement record and `reserve.pending` in
`GET /v1/placement`. When grouping reduces the count, `machines.log` shows
both `backlog_vnodes` and `source_lanes` next to the reserve terms.
The agent publishes the same positive copy limit configured for the nodes
sharing its budget directory. With a resource-aware placement but incomplete reserve facts, the
hook serves the selected target and suppresses speculative boots. An unserved
request can still trigger one launch when no boot is outstanding. A fleet that
reports no resource-aware placement uses the slot rule. Placement recalculates on spawn requests.
When a real shortage appears, the provisioner initially allows one outstanding
machine start. Once per 15-second observation interval with persistent shortage, the window
grows by one outstanding start when core-weighted CPU is at most 30%. For this calculation a host short
on RAM or disk, or lacking a host CPU reading, counts as at least 60% busy.
One tight host therefore cannot hide the capacity of a large idle fleet. Otherwise the
window can double. The hook still limits actual starts by the unmet deficit.
This controls outstanding starts; an arrival can release a credit within an interval. The hook subtracts already accepted
launches under the reservation lock. A named admissible target can start its
receiver before the hook replenishes the remaining reserve. A fully covered
demand clears the boot window. The [first 28 seconds of the AWS wave](../benchmarks/aws-placement-100-20261127/boot-wave-replay.csv)
give the same recorded requests to both versions of the allocator.

The timing and percentage defaults remain preliminary, and this rule uses
current requests rather than a forecast of write demand during a boot.

### One scale decision per machine (fix-host)

A vnode whose firing signal is the CPU or memory of its *machine* sends
`"reason": "host_pressure"` to `/v1/scale`. All the vnodes of that machine see
the same pressure. A vnode crossing the write-rate threshold also uses this
host ticket when shared CPU pressure has met its configured hold and its own
process remains below the CPU limit. It retains `signal=writes` for prefix
selection. A writer queue crossing its age or depth limit takes precedence
over the rate signal and can ask independently. A donor already copying out
waits on that copy for a shared CPU request. Answering each would start one receiver, and one more
copy, per vnode. The provisioner coalesces them by donor machine
(`pressure.go`):

1. The requester's machine is read from the facts (`/facts` lists the vnodes
   of each machine). Unknown machine, no facts hook, or another reason: the
   request is handled by vnode, as before.
2. The first admitted request owns the *relief* of its machine. After a
   confirmed capacity wait with no dispatch, a sibling with a higher holder
   arrival rate can take over on its next request.
3. While the relief is in flight, the other host-pressure requests of the
   machine get `200 {"status":"already_starting","coalesced":"host_pressure"}`,
   with a retry hint if their own address has no outstanding dispatch. A donor
   waits for landing only when its exact target was requested.

"In flight" is one of:

| State | Lasts | Constant |
| --- | --- | --- |
| asked | while the owner's request runs | none |
| waiting for a machine | 15 s after the last answer to the owner; the owner's own retries pass (it is the request being retried), any other vnode may take over afterwards | `reliefWait` = `placementMemory` |
| receiver placed, not yet reported | 15 s | `placementMemory` |
| receiver joining | as long as the facts show its machine receiving, at most 3 min | `targetPatience` |

The request may carry `"transfers_in_flight"`. It is logged with the decision
and decides nothing: whether copies already run is bounded by the copy
budget below, not by refusing a relief. Coalesced requests are counted in
`GET /v1/placement` (`decided.host_coalesced`).

### A copy budget per host (fix-host)

The receiver budget of placement bounds neither the source nor a
copy between two vnodes of one host: at t=428 s one host ran three outgoing
operations from two vnodes at 96 % CPU. `mesh.CopyBudget` is a counting
semaphore shared by the vnode *processes* of a host: a directory with one
file per slot, each held with `flock`. Node setting
`limits.copy_budget_directory` (off when absent) and `limits.copy_slots`; the
experiment agent gives every slot of a machine `<dir>/copy-budget`
(`-copy-slots`, negative for none).

| Rule | How |
| --- | --- |
| at most N copies per host | an outgoing grain takes one slot before its admission and keeps it until the grain ends |
| a dead process frees its slot | the kernel closes its files; the name left on the slot is taken over |
| source and receiver cannot wait for each other | a transfer holds at most one slot and nobody holds one while waiting for another; the receiver never waits |
| one transfer, one slot | the slot carries the transfer's id: a co-resident receiver finds it and takes none (`copy_slot_shared`) |
| admission before any fence | an unfenced source waits at most `copySlotWait`, then returns `ErrNotReady`. A new proposal takes its slot before partitioning or pinning the input. A previously durable warm operation keeps its identity on retry; exhaustion never starts an extra warm copy (`copy_slot_timeouts`) |
| no freeze is lengthened | only a grain with nothing fenced yet (phase before `TransferMoving`) waits; a resumed grain and a receiver's preload take a free slot or proceed at once (`copy_slot_busy`) |
| writes | never look at the budget |

| Constant | Value | Why | Status |
| --- | --- | --- | --- |
| slots | `max(1, cores/4)` | a conservative concurrency budget for background work; measured pressure also paces completed lots | assumed |
| `copySlotWait` | 10 s | longer than a grain usually lasts, shorter than `handoffWait` (20 s) | assumed |
| poll | 20 ms | `flock` has no queue; a waiter retries | assumed |

A new proposal has no durable transfer while it waits for host capacity.
During admission, `copy_slot_waiting` counts current waiters on this vnode
and `copy_slot_wait_ms` accumulates time across completed waits, including
expired or cancelled attempts. Once the store allocates its operation ID, the donor names the same locked
slot before contacting its receiver. During recovery the source visits its
irreversible transfers before warm operations that may wait for capacity.
An unavailable receiver can still delay installation after a fence. A timer
cannot safely give the source its old right back after a lost install reply.

After a completed lot has been installed and acknowledged, the source may
pause before the next lot when sampled CPU exceeds its configured target.
The duty share is the measured idle fraction, with a 10% progress floor.
The source keeps its host lane during this pause. Recovery with unfinished
fences and sealed-region completion never pause. Sampling continues while
autoscale is held by a transfer. Unknown samples clear the pacing estimate.
`copy_paced_lots` and `copy_paced_ms` expose this time separately from freezes.
These are scheduling choices to qualify, **not** a hard CPU quota or a
maximum freeze-time guarantee. No write path waits on this pacing timer.

Limits: reception is throttled only indirectly (a preload occupies a slot,
which a source then waits for; the bound on receivers stays
`maxReceiversPerMachine`). Collection of an installed transfer takes no slot.
`flock` gives no fairness. The budget counts copies, not bytes or CPU.

### Shared experiment view

See [the dashboard contract](../../ops/experiment/dashboard/README.md) and
[the resource simulation](../benchmarks/resource-sizing-2026-11-26.md).
`/facts` now distinguishes whole-host CPU availability and publishes
`receiver_ready` from the admission predicate itself. The renderer does not
copy the admission constants. Node `writes.applied` counts new durable
applications at the holder; entry ACKs and holder retry responses are different
counters. The fleet observer is independent of the pinned load generator.

The reason is logged by the provisioner (`placement decided`), written to
`machines.log` by the hook (`place=… chosen=… why=…`), and served by
`GET /v1/placement`: decisions and applications counted by kind, and the last
32 decisions.

### Monitoring before the join

`clusterBoot.Start` is split into `Open` (store, membership, vnode enabled)
and `Join`. The monitoring server now starts between the two. A vnode that
already acknowledges writes is visible, reports `joining`, and the agent's
`/spawn` returns at launch instead of after the join (it used to wait up to
120 s while the callers time out at 15 s and 60 s). `/health` still answers
503 until the vnode is active, and `/spawn?wait=ready` still waits for
`client_ready`.

### Safe jobs before the end of the join (fix-host)

The background jobs used to start after the first join attempt, up to 120 s
after the vnode began to acknowledge writes: no autoscale tick, and item
counters at zero (receivers showed 0 items with 6-19 MiB owned). The worker
(`indexus/worker.go`) now has two groups. The safe group starts as soon as
the store holds a first delegatable right (`Store.HasDelegatableRight`,
polled every 200 ms during the join): the first grain of a join, or what a
restarted vnode already held.

| Job | What it does | Changes rights? | Starts |
| --- | --- | --- | --- |
| observe | pings contacts, registers and discovers peers, recounts items and zones (`MeasureItems`, read only) | no; its refresh on a new peer is held, see below | first right |
| update | summary reconcile: refresh the view, fold, flush pending edges | no; the cold fold it can schedule is held | first right |
| autoscale tick | samples demand and resources, may reserve the name of a vnode to spawn (`ReserveSpawns`) and ask the provisioner for it | no right moves: the transfer it leads to is begun by the joiner and validated by the store (`BeginCausal`). This is the path by which a first installed grain can already help another vnode | first right |
| rebalance | gossip sync, `Resume` of interrupted transfers, `Rebalance` (a join), replication when enabled | yes | after the first join attempt returned |
| refresh on a new peer (inside observe) | one rebalance round | yes | held until the join returned |
| cold fold (scheduled by update) | folds the vnode into a survivor | yes | held until the join returned |
| metrics, monitoring | served from `Open` | no | before the join |

From `StartSafe` to `Start` the node holds maintenance
(`Node.HoldMaintenance`). A join that fails (a move whose source is gone, an
unreadable store) stops the safe jobs and waits for them before the process
closes the store. A join that ran out of time is not a failure: the vnode and
its safe jobs keep running, then the rest starts as before.

Why wait for a first right rather than start at `Open`: a vnode without any
right has nothing to count or to delegate, and a spawned vnode that is no
longer needed leaves without having run a job.

## Open points

1. **No rate cap on the transfer.** Every system above throttles the stream;
   Indexus bounds only how many joins a machine receives. A byte-rate cap alone would not bound foreground CPU or make
   `receiverCores` a guarantee. CPU and disk budgets must cover both endpoints.
2. **The constants rest on one run.** `receiverCores` should be measured per
   machine size, and separately for the transfer and for the foreground share.
3. **The donor's cost is bounded in count, not in CPU.** The copy budget
   limits the outgoing grains of a host; it reserves no cores and does not
   pace a copy. Placement still ignores the donor's cost.
4. **A short wait for a receiver.** After the local spawn hook confirms that it
   sent no launch, the provisioner keeps the original target for up to five seconds
   within the caller's request. Waiting calls share a fleet survey about once per
   second. On each survey it serves eligible requests in descending order of
   holder arrivals/s. This estimates the relief of a half split for uniform writes.
   Equal rates keep arrival order within the batch. A recheck can consume a
   concrete offer without running another boot hook while supply is absent.
   After the window the donor retries with fresh demand. This scheduling window
   imposes no bound on EC2 boot time or the wait for the first installed right.
5. **Demand and traffic skew.** Holder arrivals, in-flight writes and the age
   of the oldest write expose demand during a slowdown. Predicting how much
   of this traffic will follow a specific set of transferred zones remains
   an open sizing question.
6. **A new machine that never comes.** While a boot is possible the request
   waits. If EC2 refuses every launch while the cap leaves room, the launch
   is retried (the stamps expire after 180 s) and the request is never
   placed: nothing falls back to a busy machine in that case.
7. **Local runs.** All simulated machines share one host: each agent reports
   its own vnodes' CPU against all the host's cores, and the same disk.
8. **Direct EC2 mode** (`-launchTemplate` without `-localDir`) starts one
   instance per vnode and is not concerned.
9. **ScyllaDB's per-shard migration limit** was not found in the pages read;
   the row above is limited to what they state.

## Sources

- Cassandra, streaming: https://cassandra.apache.org/doc/4.0/cassandra/new/streaming.html
- Cassandra, topology changes: https://cassandra.apache.org/doc/4.1/cassandra/operating/topo_changes.html
- Cassandra (DataStax 2.1 guide), adding nodes: https://docs.datastax.com/en/cassandra-oss/2.1/cassandra/operations/ops_add_node_to_cluster_t.html
- ScyllaDB, tablets: https://docs.scylladb.com/manual/stable/architecture/tablets
- ScyllaDB, topology over Raft (load balancer, migration stages): https://github.com/scylladb/scylladb/blob/master/docs/dev/topology-over-raft.md
- ScyllaDB, how tablets work: https://www.scylladb.com/2024/06/17/how-tablets
- ScyllaDB, tablets questions: https://www.scylladb.com/2025/06/24/scylladb-tablets-answering-your-top-questions/
- CockroachDB, replication layer: https://docs.cockroachlabs.com/docs/stable/architecture/replication-layer
- CockroachDB, cluster settings v23.1: https://docs.cockroachlabs.com/docs/v23.1/cluster-settings
- CockroachDB, Raft snapshots tech note: https://go-mod-viewer.appspot.com/github.com/cockroachdb/cockroach@v20.2.0-alpha.1+incompatible/docs/tech-notes/raft-snapshots.md
- TiDB, store limit: https://docs.pingcap.com/tidb/dev/configure-store-limit/
- TiDB, PD configuration v5.4: https://docs-archive.pingcap.com/tidb/v5.4/pd-configuration-file/
- FoundationDB, data distributor internals: https://github.com/apple/foundationdb/blob/main/design/data-distributor-internals.md
- FoundationDB, data distribution and movement: https://github.com/apple/foundationdb/wiki/Data-Distribution-and-Movement
- Kafka, KIP-73 replication quotas: https://cwiki.apache.org/confluence/display/KAFKA/KIP-73+Replication+Quotas

### Recent activity and useful vnodes

Process/CPU/queue relief ranks zones by recent activity rather than requiring
half of the donor's item count to move. A small hot area can therefore justify
one canonical ADD, provided the geometric validation still succeeds and the
donor retains useful work. Item-limit and disk-relief constraints stay active.
A single indivisible address cannot manufacture an infinite chain of empty
donors. Splitting that load needs finer application addresses.

Activity is a bounded 4,096-entry LRU with exponential decay (10-second time
constant), updated only where a write is applied. One- and three-character
prefix counters recognize streams of distinct leaves. Prefix heat is advisory:
it affects ranking, never write authority or the atomic write-capacity check.
The 512-zone planning sample can still miss a concentrated hotspot; this is
not a claim of optimal placement or linear p99 at arbitrary cluster sizes.

The steps harness now honors `STEP_VNODES` (default: first `SIZES` value),
validated against initial host slots before creating resources. It previously
hard-coded four, making a larger requested initial population ineffective.
Five process slots per host remain the default; slots are capacity, not a
promise that five busy vnodes fit under the resource target. A future 16k/s
qualification can start with `HOSTS=4 SLOTS=5 STEP_VNODES=16 SIZES=16`, retaining
resource admission and existing CPU/memory/disk thresholds. This configuration
is prepared, not yet qualified at 16k/s. More vnodes improve delegation
choices; they do not add CPU to a saturated physical host.
