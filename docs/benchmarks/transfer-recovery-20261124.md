# Transfer recovery, demand and evidence — 24 November 2026

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

The AWS direct run `exp-16k-direct-dynamic-20261124-113116` tested `6c1f58e`.
It did **not** validate 16,000 writes/s. It was stopped by the operator; no final
durability audit completed. All 49 instances remaining at shutdown were
terminated, and enabled-region checks found no other active instances.

## What the graphs establish

At t=60 s: 2,000 arrivals/s, 2,000 ACK/s, no loader queue drops, p99 16.384 ms.
At t=580 s: 16,000 arrivals/s, 9,175 ACK/s, 4,282 loader queue drops/s, p99
33.554 s, 48 known vnodes. The observed membership later reached 56 vnodes.
This exceeded the previous 32-vnode ceiling; the new 64-machine policy limit
was not shown to bind. Membership is not a count of ready, equally loaded
writers. The item imbalance reached about 7.6; that is not a measurement of
per-node write-rate imbalance.

The dashboard's final 48.8% was a ratio of the last ACK/s and arrivals/s
samples. They do not refer to exactly the same request cohort: queueing,
retries and drain can shift acknowledgements into later intervals. It is not
a final lost-record percentage. The long horizontal tail reused old metrics
after the observer ended; handoff logs still showed work progressing.

One operation (`3be4d90…`, receiver `10.77.4.164:21100`) ran for 875.91 s and
failed in warm copy. Its next attempts failed after 145.59 s at stage and
179.94 s at warm copy. A fourth attempt was still progressing in saved logs.
For its first long sweep, freeze and emit calls accumulated 367.28 s, about
42% of the 869.65 s sweep. These durations include writer contention and
commits; the evidence cannot isolate fsync, CPU or a particular lock cycle.

## Sizing was not the same as throughput demand

The run used `ITEMS=20000`, `WRITE_CAP=0`, two slots per data machine and one
16-vCPU loader with 16,000 workers. Item mode disables ordinary host resource
signals. No separate writes/s scaling threshold was configured. A growing
insert-only data set therefore kept requesting splits even at constant rate.

The intended four-step schedule contains about 4.86 million arrivals:
120 × (2000 + 4000 + 8000 + 16000) plus 60 × (3000 + 6000 + 12000).
If every arrival inserts a distinct record and all succeed, keeping below
20,000 records per vnode would require roughly 243 vnodes. The policy allowed
128. This is a sizing comparison, not a claim that 243 vnodes were required
for throughput, nor that this later ceiling caused the measured early stall.

16,000 workers allow about one second of mean in-flight time at 16,000
requests/s before all workers are occupied. The 64,000-entry queue adds only
four seconds of arrivals. Once completions lag, retries occupy workers and the
queue fills, even with spare loader CPU. The old `shed` graph meant arrivals
not submitted by this queue, not server admission refusals. Raw load counters
were not published before phase completion, preventing a complete postmortem
of its queue occupancy. Increasing workers alone is not a transfer fix.

## Changes and preserved guarantees

* Granular preloads and mutation mirrors use the already prepared dependency
  path, as do their compact responses. Catalog admission is self-certifying
  and also avoids a global Knowledge response. Global exchange remains at
  operation boundaries. Per-zone proof, digest, identity and installation
  checks are unchanged; a routing hint never grants write authority.
* The initial inventory has a durable completion checkpoint. Interrupted
  discovery resumes safely; a completed discovery is not repeated on every
  retry. New zones register with the migration set in the same transaction
  as their creation, including staged installation.
* A retry with already-frozen zones no longer starts with another global
  preparation exchange. Unacknowledged emitted proofs are replayed before new
  work. Frozen-before-emission zones retain their ordering and durable fences.
  An error never implicitly restores authority to the donor.
* Failed cutovers contribute their duration to diagnostics and adaptive lot
  sizing; failed installs do not increment the installed-zone progress count.
  This duration covers the entire cutover call, including pre-fence waiting;
  it is not an exact durable no-writer interval across retries.
* Demand is sampled during handoffs, so resuming autoscale does not average a
  current spike over the whole join. A separate `SCALING_WRITES` threshold
  can trigger scaling without imposing a hard write cap. AUTO-3's authority
  stability gate remains: requesting more peers must not redelegate unstable
  ownership. Removing that gate alone would not establish a safe protocol.
* `/metrics` records readiness, autoscale state, handoff phase and age, and
  whether its worker is currently running or waiting for retry. Slow writer
  diagnostics distinguish lock wait from lock hold, are rate-limited and log
  after unlocking. The observer captures stacks for old handoffs even if its
  metrics endpoint is fast. No per-record payload is logged.
* Live graphs distinguish known/ready nodes, loader queue drops, server 503
  attempts, in-flight work and intended latency. Stale tiles are hidden. The
  observer continues through drain/audit. Incremental evidence is published
  while the phase runs, in complete-line parts with confirmed offsets.
* Loader drain is bounded to two minutes by default. Outstanding client
  requests are then canceled and the final summary records unfinished work.
  Cancellation does not undo a server commit. ACKed keys still require audit.

## Local validation and limits

A JSON transport test copying 512 zones measured 228,930 snapshot bytes and
4,879 bytes of global Knowledge across the whole handoff (2.13%). This fixture
has a small topology and legacy collection names. It proves removal of the
per-lot full-bundle path; it does not bound metadata percentage for arbitrary
numbers of nodes, schemas or tiny single-zone handoffs. Normal per-zone proofs
are part of the transfer protocol, separate from global Knowledge bytes.

The frame-builder test holds both peers' global builders through every lot,
including preloads, while transfers and immediate receiver writes succeed.
Fault tests cover stage failure, lost install ACK and live writes before
replay. Discovery is checked across a store reopen, with late zone creation.
Failed-lot tests verify reporting, adaptive reduction and persistence of the
source fence. The observer and publisher have isolated local tests.

A local scale run with 32 vnodes / 4096 initial zones, four concurrent join
workers, concurrent writes, new zones and injected lost install ACKs passed:
8,993 ACKs, 8,993 records observed, one-writer checks satisfied, join/audit
16.91 s. This is an in-process correctness fixture, not an AWS/fsync throughput
benchmark and not a claim that 16k/s is now sustainable.

The remaining physical limit must be measured with the new raw traces. Fast
handoff completion is still required to release AUTO-3. Delegating onward
before regional completion would require a separate authority design and
proof; these changes do not pretend that removing a guard supplies that proof.
No new paid AWS run is part of this validation.


## 64-vnode laboratory and a measurement correction

`indexus/lab64_test.go` uses 64 real mesh services, 64 real stores and actual
loopback HTTP/JSON. Two vnodes share each of 32 device gates; every store still
uses the same physical computer and filesystem. The default adds 1 ms per
network call and per device sync, retains real fsync, injects one lost install
ACK per operation, a 750 ms receiver partition and a 750 ms disk tail, then
reopens one receiver before auditing all ACKs. It is not 32 independent EC2
machines, QUIC, a power-loss test or the production Node autoscaler: joins are
scheduled in doubling steps with eight concurrent Join callers. Per-node
GossipTargets/Sync runs outside writes; a direct client refreshes metadata over
HTTP once per second. Exact-zone ACK hints remain advisory.

**Important measurement correction:** Go test binaries enable differential
capacity rescans on every transaction. Earlier local 32/64-node runs above
used that expensive correctness mode. Their ACK-retention checks remain
valid, but their durations and queue drops must not be interpreted as production
performance. Production uses incremental capacity accounting. The laboratory
now requires an explicit choice: `INDEXUS_BENCH_NO_USAGE_SCAN=1` for that
production path, or `INDEXUS_LAB_USAGE_SCAN=1` for differential correctness scans.
The latter is deliberately much slower. The preloaded full-scan run was stopped
for this configuration error, without a completed audit.

Two address profiles are distinct: the default rewrites 4,096 existing zones;
`INDEXUS_LAB_UNIQUE_ZONES=1` instead uses deterministic uniformly distributed
16-character locations and 256 collections, matching the structural shape of
AWS inserts. `INDEXUS_LAB_SEED_ZONES` preloads independent existing zones before
the client load. Seeded records are audited separately from client ACKs.
Do not compare different profiles as a throughput speedup.

Example exercising existing data, ongoing new addresses and production capacity
accounting without provisioning any cloud resource:

```bash
INDEXUS_BENCH_NO_USAGE_SCAN=1 INDEXUS_LAB64=1 \
INDEXUS_LAB_UNIQUE_ZONES=1 INDEXUS_LAB_DIRECT=1 \
INDEXUS_LAB_SEED_ZONES=16384 INDEXUS_LAB_BASE_RATE=100 \
INDEXUS_LAB_PHASE_SECONDS=10 INDEXUS_LAB_TIMEOUT_SECONDS=600 \
INDEXUS_LAB_OUT=/tmp/indexus-lab64 \
go test ./indexus -run Test64NodeDurableHTTPRecovery -v -count=1
```

Rates here are 100/200/400/800 arrivals/s, scaled to the single local machine;
the default is 2k/4k/8k/16k. Neither setting predicts a 32-machine capacity.
`events.jsonl` records configuration, arrivals, ACKs, queue, each disk's writer
stages, freezes, completed joins and actual ACK appliers. Failed attempts and
periodic stacks make prolonged work inspectable. Every successful run must
finish all 64 joins, find each ACK and seed exactly once, leave no outgoing
freeze, and find exactly one writable disk for each of 16 sampled addresses.
A passing test does not mean all offered arrivals succeeded: drops and terminal
unacknowledged requests remain explicit audit fields.

## Experimental geometric handoff pieces

Moving an existing point does not move nearby unwritten addresses. With unique
insertions, those new addresses continue to be created on the donor until its
whole geometric region closes. Point flips alone therefore do not establish
that write pressure is transferred proportionally during a large join.

Join may now explicitly permit `Request.Subdivide`: a donor samples keys only
and chooses a smaller subregion targeting at most 512 existing zones, then
uses the unchanged certificate partition validator to transfer it and retain
the exact complement. The chosen output is persisted in the existing transfer;
a retry reuses it exactly. Colliding point IDs cannot be subdivided. The target
is advisory under concurrent creation and is not a hard bound on frozen rows,
bytes or time. Physical moves and cold retirement retain their full-piece
contract. Join is context-bounded rather than limited to 256 pieces; unchanged
conflicts back off rather than spin.

The joiner already selected its donor token from its proof graph. An empty
prepared `preload` verifies that proof and the receiver identity using the
existing protocol; it neither installs authority nor grants permission to
write. Missing dependencies fall back to full preparation before fencing. Prepared copies,
subregion jobs and pending polls use compact replies. Stale-view conflicts use
the full validated repair path. A final exchange once per donor preserves the
Join convergence barrier after installed coverage. Receiving metadata never
installs bytes or grants permission to ACK.

**Open scalability tradeoff:** smaller regions add permanent region certificates.
Their cumulative graph and periodic full gossip can grow with the number of
pieces. This prototype must be assessed against both closure progress and
metadata growth; reduced data-path waiting alone is not evidence of scalable
16k/s elasticity. The two TLA+ models in `spec/handoff` check bounded safety and
fair recovery, not that performance claim or full concurrent autoscaler behavior.


## Independent regions must not share a network-length lock

The original donor admitted one background handoff per disk and held one
`handoffMu` across its network exchanges. After the first geometric certificate,
separate terminal residual tokens are disjoint. A partitioned receiver for one
of them must not prevent another from completing. Admission now permits two
background jobs for distinct tokens. A reference-counted, context-aware token
gate serializes each consumed token, including its waiters and orphan recovery;
the store's transactional writer lock still serializes actual commits. No
successor, fence, digest or receiver-install check is removed.

A targeted Go test partitions A after its source freeze, then completes B's
independent token and writes at B while A remains frozen. The same test fails
on `6c1f58e` with `handoff in progress`. The third TLA+ model checks this bounded
isolation property and a disk-serialized negative control. This is not an
availability promise when all worker slots or the disk itself are blocked.

Diagnostics now separate successful and failed per-attempt cutover call
histograms and the interval from `FreezeZones` returning to a successful install
reply. The former includes pre-fence queueing and the donor's final ACK commit;
the latter excludes freeze preparation. Neither is the full no-writer period
across retries. Cutover waiters are released before logging. Open-operation
phase/age and the saved stacks remain necessary for prolonged failures.


## Incremental Knowledge transport — 2026-11-24

The proposal was written before implementation in
`docs/research/knowledge-convergence.md`. The existing authority validators and
durable installation rules are unchanged. Negotiated packets carry missing facts
from a validated receipt; receipt coverage is bounded and conservative. A cached
full-frame reference can be probed on first contact when the peer may already
have learned it elsewhere. A miss refuses before dispatch and retries complete
once. Legacy peers never receive an unnegotiated delta.

An independent abstract model (`spec/handoff/knowledge_sim.py`) converged for
64 peers and 1,536 facts in three seeds, with message losses and four cache
restarts per run. It reduced synthetic encoded traffic by about 99.2%. This is
not a forecast for the real protocol, a disk model or an ownership proof.

The following actual Go/HTTP/fsync runs share the same parameters: 8,192 seeded
zones, 64 Node facades, 32 simulated device gates on one physical computer,
50/100/200/400 writes/s for five seconds each, then 10/s until joins complete.
An install ACK is lost once per operation; the receiver partition and I/O tail
are enabled. The differing duration of the final stream makes offered totals
different. These are individual descriptive runs, not statistically controlled
EC2 throughput comparisons.

| Run | General exchange RPC bytes carrying Knowledge | Offered / ACKed | Final failures | Elapsed | Intended p99 |
| --- | ---: | ---: | ---: | ---: | ---: |
| `lab64-summary-routing` (complete control) | 1,510,276,949 | 5,671 / 5,671 | 0 | 216.14 s | 1,689.94 ms |
| `lab64-knowledge-delta` | 531,169,132 | 5,538 / 5,538 | 0 | 206.03 s | 1,853.04 ms |
| `lab64-knowledge-delta-bounded` | 532,341,687 | 5,486 / 5,486 | 0 | 198.56 s | 1,770.15 ms |
| `lab64-knowledge-reference` | 430,767,270 | 5,493 / 5,492 | 1 | 199.45 s | 1,578.45 ms |

`Wire.KnowledgeBytes` counts whole request/response envelopes when they carry
Knowledge, not an isolated serialization of the Knowledge body. Exchange RPCs
carry no zone payload; this is a useful control-traffic measure, but other RPC
kinds must not be interpreted as exact Knowledge payload bytes.

All four runs completed 64 joins, reopened the same durable identity, retained
all ACKed rows, and finished with no duplicate writable ACKed records, no frozen
regions and no durable pending transfers. Writes, prepared-copy steps and summary
RPCs carried zero Knowledge bodies. The final run's single terminal failure was
zone `cP3F6nUKrFYuxBEe`, refused as `durable owner fence is closed` on all five
client attempts. It is a failed offered write, not a lost acknowledged write.
The retry budget was not increased to hide it.

The final exchange-traffic reduction is 71.48% relative to the complete control.
Its p99 is slightly lower, but maxima and intermediate runs do not establish a
reliable freeze-latency improvement. Failed-attempt histograms remain distinct
from successful cutovers; neither measures a hard end-to-end freeze bound across
all retries. Remaining CPU/validation/history costs are explicitly open.

The final transport counters show 12,032 delta packets, 3,559 complete packets,
11,223 references, 265 missing delta bases and 19,623 coverage evictions. These
count packet construction and cache behavior, not an exactly-once network send.
They expose substantial remaining amplification instead of declaring the control
plane to be a negligible fraction of zone payload.

Each run directory contains `manifest.json`, the exact `source.patch`, JSONL
samples, wire counters, stacks and an independent writable-inventory audit.
The test `TestKnowledgeReferenceReusesReceiptLearnedFromAnotherPeer` fails on the
first delta implementation and passes with the cross-peer reference probe. The
MVCC inventory cut test preserves `NeedKnowledge` on a pre-dispatch probe miss;
its watermark and ACK-retention assertions remain unchanged and pass 20 repeats.

The TLC recovery/subregion/token models still pass their three positive graphs
and four intended negative controls. They do not model the new receipt transport.
An eventual-delivery argument and Go fault/race tests cover that transport here;
no exhaustive formal refinement of the Go implementation is claimed.
