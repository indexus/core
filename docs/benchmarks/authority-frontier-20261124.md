# Authority checkpoint qualification — 24 November 2026

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

**Scope: first stage, branch `codex/authority-frontier-20261124`.**
[Contract and trust boundary](../protocol/authority-frontier.md).
This qualifies authority compaction, not membership-only Knowledge or the full
replacement of the regional delegation directory.

## Recorded local run

`lab64-frontier-admission`, 64 real service/store/facade instances, HTTP/JSON,
real fsync, 32 simulated shared-device gates on one physical computer. Joins
are scheduled externally; this is not an AWS autoscaler or 16k/s qualification.

```bash
INDEXUS_BENCH_NO_USAGE_SCAN=1 INDEXUS_LAB64=1 \
INDEXUS_LAB_UNIQUE_ZONES=1 INDEXUS_LAB_DIRECT=1 \
INDEXUS_LAB_SEED_ZONES=8192 INDEXUS_LAB_BASE_RATE=50 \
INDEXUS_LAB_PHASE_SECONDS=5 INDEXUS_LAB_TAIL_RATE=10 \
INDEXUS_LAB_TIMEOUT_SECONDS=600 INDEXUS_LAB_OUT=/tmp/frontier-lab \
go test ./indexus -run '^Test64NodeDurableHTTPRecovery$' -v -count=1
```

Rates are 50/100/200/400 arrivals/s, followed by 10/s while joins finish. The
transport loses the first installation ACK of each operation, temporarily
partitions a receiver and injects disk delay. At the end, all 64 disks perform
maintenance/checkpointing, exchange their compacted state, reopen one receiver
and undergo data and independent writable-inventory audits.

| Observation | Result |
| --- | ---: |
| Completed vnodes | 64 |
| Offered / ACKed writes | 5,976 / 5,975 |
| Shed / terminal failures | 0 / 1 |
| Seeded records | 8,192 |
| Observed records | 14,167 |
| Missing ACKed records / duplicate ACKed records | 0 / 0 |
| Missing / duplicate writable ACKed records | 0 / 0 |
| Frozen transfers remaining | 0 |
| Audit elapsed time | 259.888 s |
| ACK latency p99 / intended-arrival latency p99 | 1,702.527 / 2,230.449 ms |
| Historical certificate payloads on each disk after maintenance | 0 |
| Checkpoint grants before final all-peer exchange | 126–128 |
| Authority clock actors / outstanding intervals | 32–33 / 0 |

The terminal failure is zone `aSEEg6H9Yr_XvnkR`: all five attempts returned
`durable owner fence is closed`. This is not loss of an ACK; availability is
still imperfect. Do not hide it by increasing the laboratory's retry count.

The exchange RPC envelope counter records **507,043,122 bytes** on calls carrying
Knowledge. It includes the entire request/response envelope, not just the
Knowledge JSON. This run also includes the new final checkpoint exchange, and
other verification processes shared the computer during part of the run.
It is **not** a controlled speed comparison with the earlier delta-only runs.
The result does not show that global metadata is negligible or that freezes
became shorter. Membership-only dissemination and directory migration remain
necessary before making those claims.

## Other verification

- `go test ./...`: passed when run without the 64-node laboratory concurrently.
- `go vet ./...`: passed.
- Race detector: checkpoint/dot/coalescing/admission/cutover/token-isolation
  tests in causal, storage and mesh passed.
- Race detector: root totals after bulk + checkpoint and continuous reads
  across joins/partition passed three repetitions in `indexus`.
- 100 pure authority round trips and 30 actual disk round trips keep the
  checkpoint bounded for two fixed incarnations. Disk tests reopen receivers,
  replay old regional and point installations and retain the latest ACKed update.
- TLC: five positive bounded models and seven expected negative controls.
  New compaction model: 77,833 states; sibling consumption: 9 states. Removing
  replay floors, restoring an identity without them or consuming only one
  sibling each produces its intended double-writer counterexample.

A batching assertion in `TestCausalSummaryPushBatchesDistinctRemoteEdges` failed
under concurrent laboratory I/O, including targeted repeats. The final quiet
full suite passed with its assertion unchanged. This timing sensitivity is
recorded, not treated as an authority-safety result.

The models do not constitute a refinement proof of all Go executions or
exhaustive verification of 64 nodes. The safe restoration model retains durable
consumption floors; arbitrary backup rollback under the same disk identity is
unsupported. See [the reproducible model runner](../../spec/handoff/README.md).
