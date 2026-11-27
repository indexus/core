# Elasticity: useful vnodes and foreground headroom — 2026-11-26

Base: `907ae4e`. This continues the
[AWS failure diagnosis](aws-settled-direct-2026-11-26.md) and the
[Claude staging-copy qualification](claude-copies-2026-11-26.md).
No AWS campaign is launched by this change.

## Observations and decisions

| Observed or read in code | Change | Nature |
| --- | --- | --- |
| Exact paginated totals failed after quiet load because unrelated maintenance changed the global transaction version. | Track collection data revisions atomically with data, and bind cursors to queried authority/forwarding coverage. | Functional read correctness |
| A warm copy waited for a host lane, then bypassed it after ten seconds. Several loaded vnodes could therefore all begin copying. | Defer the same durable operation on exhaustion before its first fence. | Admission scheduling |
| Holding fewer lanes does not limit the CPU used by a long copy. | Under sampled CPU pressure, yield between completed lots; keep frozen recovery urgent. | Scheduling |
| Process relief required cardinality relief and ranked mostly stored items. | Allow a useful small move; rank recent write activity for CPU, queue and write requests. | Placement heuristic |
| Historical heat keys accumulated without eviction or decay; relayed ACKs also heated ingress nodes. | Bounded decaying activity cache; count only where a write is applied. | Bounded memory and placement heuristic |
| Steps always started four vnodes, regardless of requested size. | Honor `STEP_VNODES`, defaulting to the first `SIZES` entry; validate initial slots before provisioning. | Functional experiment wiring |
| Audit cancellation did not propagate through all HTTP stages; total mismatches repeated a full readback serially. | Context-bound requests and backoff, completed-read accounting, bounded mismatch diagnosis only after sampling. | Functional experiment reliability |
| A spawn-hook test occasionally observed an empty fake launch file. | Wait for a complete JSON record, retaining every existing assertion. | Test synchronization |

## Read correctness

An inventory cursor no longer invalidates because staging cleanup, counters,
receipts, or another collection committed. A readable data mutation in its
collection still invalidates it. Authority coverage and successor referrals
are canonicalized and hashed in the same read snapshot; changing them also
invalidates the cursor. This prevents silently stitching pages from different
ownership scopes.

The per-collection revision is committed with data, including bulk replacement.
There is no extra fsync. Only one revision row per touched collection is kept;
whole-disk queries retain conservative transaction-version invalidation.
This intentionally changes the deterministic disk digests. The golden values
were updated for that new durable metadata, not to hide different records.
Continuous concurrent changes to the queried collection can still prevent an
exact multi-page read from completing. This is not a global atomic snapshot.

A deterministic regression failed before the fix and passes after it. Other
cases cover mutation rejection, authority changes, reopen and bulk replacement.
The intermediate checkpoint `a7e4395` also passed the real-process demand
scenario: 17,999 ACKs all read, 17 exact collection totals correct, zero double
ownership, 4.829 seconds of observed audit settlement. That measurement does
not qualify later scheduler changes by itself.

## Transfer scheduling and guarantees

Normal outgoing warm grains need a host lane. They may wait before fencing,
then return `ErrNotReady` if no lane is available. The source remains writable
and resumes the same durable decision. A resource wait is observable but does
not count as another handoff attempt.

Default concurrency becomes `max(1, logical cores / 4)`, shared by processes
through flocks. It is a conservative starting policy, not a measured optimum.
Receivers and resumed moving operations remain nonblocking to avoid cycles
or prolonging unfinished fences. The budget is therefore not an absolute
concurrency bound for recovery and incoming work.

When sampled CPU exceeds the configured target, a completed lot yields using
the measured idle fraction as its duty share (10% minimum for progress).
Installation and source acknowledgement precede the pause. Frozen recovery
and sealed-region completion skip pacing. No authority or writer lock is
held during this timer. Sampling continues through the autoscale transfer
hold. Metrics expose `copy_paced_lots` and `copy_paced_ms` separately.

These changes neither introduce a global validation nor modify who may write.
Existing ordering, install checks, replay rejection and durable authority
remain the decision points. A timeout never restores a consumed right.
More vnodes improve available delegation boundaries; they cannot subdivide
one indivisible record address, nor supply missing physical CPU.

## Placement and preparation

Recent activity uses at most 4,096 entries with exponential decay (10-second
time constant). Two coarse prefix levels help recognize a stream of distinct
locations. Prefix heat only ranks candidates; it is not an atomic capacity
estimate or a grant. Sampling remains bounded to 512 zones and can miss a
hotspot. This is a useful heuristic, not an optimal allocation proof.

For CPU/queue/write relief, one small hot area may be useful even when it
contains much less than half the donor's items. The validated canonical ADD
must still move useful work without producing an empty donor chain. Item
and disk pressure keep their separate constraints.

The next comparison can prepare four hosts, five slots per host and sixteen
initial vnodes (`HOSTS=4 SLOTS=5 STEP_VNODES=16 SIZES=16`) instead of silently
starting four. Keep resource targets at their qualified values. A write-demand
threshold such as 200/s requests help; it is not a refusal cap. Spare machines
and admission remain necessary: filling more slots on one already saturated
host does not relieve it.

## Local verification

- `go test ./...` passes on the final protocol/storage/experiment code; the
  earlier `-count=1 ./...` pass also passed before the last stamp optimization.
- Full `-race` suites pass for storage, mesh, autoscale, protocol and indexus.
  Storage was repeated after the final stamp optimization (126.8 seconds).
- Audit cancellation/mismatch/retired-endpoint regression tests pass with
  `-race`; the full experiment suite also passes normally.
- Main and QUIC modules build. `gofmt -l .` is empty. Deployment wiring
  (7 tests) and spawn hooks (21 tests) pass.
- At source `7c7e0c0`, the granular safety scenario passes with 64 vnodes,
  1,024 seeded zones, four concurrent joins, continuous writes and a lost
  install ACK per operation. It observes all 3,492 acknowledged rows and
  checks unique writers. Join plus audit takes 7.356 seconds on this
  in-process fixture. That is a safety scenario, not 64 machines or an
  fsync/network-capacity benchmark; its varied-duration concurrent writer
  means the ACK count is not a throughput comparison.

### First real-process qualification, before the placement follow-up

Source `7ee5441` (code of `7c7e0c0`, plus documentation), HTTP direct,
400 writes/s for 45 seconds, five initial vnodes on one agent and a five-slot
reserve on another. Both agents share one 8-vCPU Linux VM. The full run and
setup are retained locally; [the compact evidence](elastic-anticipation-20261126/qualification-before-placement.json)
contains no issuer credentials.

- 18,000 offered, 18,000 ACKs, no shed/exhausted/unfinished writes.
- All 18,000 ACKs read; 17 exact collection totals match; no read error or
  duplicate ownership; eight vnodes answer. Observed audit settlement: 5.078 s.
- Published write histogram (17,596 observations): p50/p95/p99 bucket upper
  bounds 8.192 / 8.192 / 16.384 ms. From intended arrival: 16.384 / 16.384 /
  32.768 ms. Read histogram (342 observations): 2.048 / 4.096 / 8.192 ms.
  The final unpublished fraction is excluded; these are not exact quantiles.
- CPU pacing is exercised: 13 lots, 1,294 ms of cumulative pauses across
  processes. Peak sampled whole-VM CPU is 68.42%, so a hard 60% ceiling is
  **not** demonstrated.
- Advisory background item counts sum to 12,104 at the audit snapshot,
  whereas exact collection totals and row readback give 18,000. These sampled
  counts are still stale; they must not be presented as the exact cluster
  inventory. This pass does not fix their freshness.
- The harness's `spawned` summary is sampled at load end (2), while final
  placement telemetry includes a later application (3). Use timestamped
  events and the eight answering vnodes rather than equating those snapshots.
- The three containers and their network are removed by the harness. Existing
  unrelated local demo containers are preserved. No AWS resources are created.

This passes the previously failing inventory/total scenario. It is one run,
not a statistical speed comparison: placement identities and join timing vary.
It does not qualify physical-host isolation, H3 or a 16k/s ramp.

## Write-cost check

Three sequential 10,000-operation `BenchmarkApplyRecord` runs per version,
Apple M1 Pro, no fsync, no load test running concurrently:

| Version | ns/op (three runs) | allocs/op | Bytes/op (range) |
| --- | --- | --- | --- |
| `907ae4e` | 65,896 / 67,923 / 72,760 | 297 | 44,050–44,656 |
| first scoped-counter version | 69,514 / 76,311 / 76,611 | 329–330 | 45,141–45,879 |
| cut-sequence optimization | 74,408 / 71,793 / 74,511 | 311–312 | 44,313–44,824 |

The final median remains about 9.5% slower in this microbenchmark, with
14–15 additional allocations per operation. The scope fix is not free:
one small durable metadata row joins the existing batch. It adds no fsync.
Using the cut's sequence avoids fetching a counter, and buffered live records
avoid another authority lookup/reconstruction. Pure physical removals still
check authority, so collecting already ceded rows does not invalidate reads.
These three runs do not establish end-to-end latency, confidence intervals,
or sustained disk throughput. Do not claim a write-speed improvement from them.

## Latest Claude placement follow-up

After the first qualification, `claude/next` had one new commit, `052ca47`,
above the common base. It is cherry-picked as `b1a69b7`: remote relief takes
precedence over a same-machine split. `43a546a` then permits reusing an existing
remote host before spending an empty spare when complete pair observations
and physical admission justify it. Tests include the fifth slot on a four-core
host; this does not claim five busy vnodes always fit there.

Additional corrections expire uncounted/stale item observations, reject a
host's incomplete vnode list as a known load, make the candidate comparator
transitive, and stop an unrelated unknown machine from disabling relief
between a known donor/target pair. No physical threshold changes. The original
half-split item inequality remains a conservative heuristic for smaller hot
moves. See [the handoff for Claude](elastic-anticipation-20261126/HANDOFF-CLAUDE.md).

The fluid placement replay remains green: peak **modeled** CPU 47.5%, sixteen
hosts, zero local splits. It assumes uniform half-splits and fixed transfer
CPU/boot times; it is not another AWS measurement. Full Go, main/QUIC builds
and relevant race suites pass after this integration. The real-process
qualification below must be read with its exact source version.

### Real processes after the placement follow-up

Source `532d017`, same local demand scenario and physical limitations as
above. [Final compact evidence](elastic-anticipation-20261126/qualification.json):

- 17,999 offered and ACKed, all 17,999 read, all 17 exact collection totals
  match; no missing key, read error, duplicate ownership or unfinished write.
- Eight answering vnodes, three receivers started. The post-audit summary and
  placement counters now agree. Settlement observed for 5.568 seconds.
- Published write buckets (17,596 observations): p50/p95/p99 upper bounds
  **8.192 / 8.192 / 16.384 ms**. Intended arrival: **16.384 / 16.384 /
  32.768 ms**. Read buckets (351 observations): **2.048 / 4.096 / 8.192 ms**.
- Six paced lots, 654 ms cumulative pauses; sampled whole-VM CPU peak 66.98%.
  These values differ from the earlier run's placement/timing. They do not
  establish a speed gain or a hard CPU ceiling.
- Advisory counts sum to 24,187 while exact inventory is 17,999. Even a recent
  count is not a global cut: independently sampled nodes and paged counting
  during writes/transfers can disagree. Placement treats counts as a bounded-age
  heuristic, not ownership proof. This remaining measurement limitation must
  stay visible on dashboards.
- Test containers/network removed; no AWS resources created.

The local fixture has a full initial donor host, so it cannot by itself expose
or prove the old same-host placement preference. The new pure placement tests
and fluid replay test that policy; physical multi-host load still needs AWS.

## Remaining boundaries

- No claim of sustained 16k/s or p99 independent of cluster size follows from
  local qualification.
- CPU pacing is soft. Foreground load alone may exceed the target; placement
  must bring additional physical headroom.
- Receiver CPU admission remains conservatively estimated. Small instance
  types need separate measurements, not a lower threshold to force admission.
- Flock admission has no fairness guarantee. Incoming and recovery work can
  exceed normal warm-source concurrency.
- The audit now propagates cancellation through HTTP, backoff and work
  dispatch, reports incomplete reads explicitly, and does not re-read every
  ACK after an already complete readback. Sampled mismatch diagnosis uses
  32 workers. Cancellation, retired endpoints and genuine total mismatches
  are tested. This is locally qualified, not yet observed on AWS.
- This does not implement migration of an entire vnode process. It delegates
  zones and their authority to other vnodes.
