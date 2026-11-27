# REM-83: storage stalls (local, 2026-11-16)

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

The isolated cause is head-of-line blocking between merges and memtable flushes.
The old single background worker cannot flush an immutable memtable until its
current merge finishes. After the active memtable fills too, `Apply` waits on
`flushed`. Storage serializes transactions through `diskBatch`, so every write
behind that transaction waits too. Raising admission limits would only lengthen
that queue. Admission itself refuses immediately; it has no condition wait.

This establishes a local mechanism, **not yet the attribution of the AWS tail**.
The native local run did not reproduce a one-second tail. An explicitly injected
one-second merge reproduces the observed shape without slowing the WAL or flush
I/O; the native run records a smaller memtable wait as well. Confirm
with stage metrics on AWS before drawing the production conclusion.

## Reproduction

Go 1.26.0, darwin/arm64, local filesystem, real fsync (NoSync=false).
`TestStorageSustainedLoad` offers five writes every 4 ms for at least 30 s:
1,250 writes/s, 200 payload bytes plus control-row overhead, fresh keys,
1 MiB memory budget (64 KiB per memtable), batch limit 256, collection delay
1 ms. The small memory budget forces many flush/merge cycles in 30 s. This is a
storage scheduling experiment, not a reproduction of the AWS data volume or
application validation cost. At 256 MiB the same cycles require more data.

Arrivals use an absolute schedule independently of ACKs, then all accepted
writes drain. The printed p50/p99/max are exact submission-to-callback-start
samples (including earlier callbacks within the group). `/metrics` queue time
ends at group start instead. Native runs use only the real device. `merge`
mode seeds four segments, then delays creation of the first merge output by
exactly one second; all other filesystem operations are unchanged. Its setup
runs `Flush` concurrently and drains it after the load; that maintenance call
can also rotate active memtables during the run, identically before and after.
The native scenario has no such maintenance call. No AWS.

```sh
export GOCACHE="$HOME/indexus-personal/.cache/go-build"
export GOMODCACHE="$HOME/indexus-personal/.cache/go-mod"
export TMPDIR="$HOME/indexus-personal/.cache/tmp"
export GOTMPDIR="$TMPDIR"
mkdir -p "$TMPDIR"
INDEXUS_STORAGE_LOAD=1 go test ./internal/storage -run '^TestStorageSustainedLoad$' -count=1 -v
INDEXUS_STORAGE_LOAD=merge go test ./internal/storage -run '^TestStorageSustainedLoad$' -count=1 -v
```

Base revision is `b6a71db2e0502be5fb8712e687d3717b229dac2f`.
Baseline is commit `99386d3` (instrumentation, original scheduler). Fix: `6a353d5`. The fix
retains the exact load generator. Peak admitted was sampled by the producer once per tick in all runs; the last
run also includes the newly added exact peak counter. Raw stage histograms, cumulative nanoseconds,
and exact maxima are in `storage-stall-local-2026-11-16.json`.

| Scenario | p50 before commit | p99 | max | peak admitted | writes / refusals |
| --- | ---: | ---: | ---: | ---: | ---: |
| Native, before | 4.677 ms | 31.138 ms | 107.167 ms | 145 | 37,505 / 0 |
| Native, after | 4.502 ms | 26.685 ms | 51.682 ms | 80 (sampled), 85 (exact) | 37,505 / 0 |
| Merge delayed 1 s, before | 9.699 ms | 576.820 ms | 863.679 ms | 1,095 | 37,505 / 0 |
| Merge delayed 1 s, after | 12.595 ms | 53.141 ms | 112.661 ms | 195 | 37,505 / 0 |

In the native runs, maximum memtable condition wait went from 80.079 ms to
zero, even though the maximum merge increased from 209.893 to 319.630 ms.

A rare blocked group is easy to miss in commit p99: the injected baseline has
one roughly one-second transaction among 2,100 commits, yet roughly a thousand
of 37,505 writes queue behind it. Commit and write-wait histograms have different
denominators. A short commit p99 does not rule out occasional storage stalls.

Maximum measured time by stage, delayed-merge runs:

| Stage | Before | After |
| --- | ---: | ---: |
| Immutable memtable / condition wait | 832.221 ms | 16.207 ms |
| Background merge | 1,020.844 ms | 1,052.340 ms |
| Background flush | 69.391 ms | 95.600 ms |
| Publication under store lock (manifest fsync, cleanup) | 52.282 ms | 50.575 ms |
| Apply store lock acquisition | 14.330 ms | 47.644 ms |
| WAL sync lock acquisition | 21.840 ms | 24.917 ms |
| Admission mutex acquisition | 12.846 ms | 31.085 ms |
| Storage writer lock acquisition | 0.239 ms | 0.010 ms |
| Storage rows lock acquisition | 0.033 ms | 0.002 ms |
| Group callback validation | 8.615 ms | 8.475 ms |

Flush/merge timers cover segment building and its file/directory syncs; input
iterator setup, the flush WAL barrier and publication are outside those timers.
The merge remains slow while the long memtable stall disappears. Background
work and foreground stages overlap: do not sum them. `memtable_full` includes
condition-variable reacquisition; it may record several wakeups for one Apply.
The fix removes dependence on merge completion, not real flush throughput or
manifest I/O. Increased I/O concurrency can increase ordinary commit latency;
this is not a claim of a general throughput improvement.

## Change and safety

Flush and merge each have one on-demand worker. Still exactly one active and
one immutable memtable, and at most one merge. The configured memtable/cache
budgets are unchanged; overlapping builders add one flush builder (including
its 1 MiB buffered writer and memtable-sized filter), not a new memtable queue.
Only the flush worker consumes `imm`. Flush publication appends to the segment
list, leaving the one merge's captured `[lo,hi)` valid. Merge publication retains
the segments appended while it built. Both publish under the existing store
lock and manifest protocol; files are removed only after the manifest commits.
`Flush` and `Close` wait for both workers.

WAL framing, sync, visibility and ACK paths are unchanged. A flush calls
`sync(imm.maxSeq)` before publishing; foreground Apply still returns only from
`sync(upto)` after fsync. Only a flush advances `durable`; a concurrent merge
preserves it. This keeps HND-2, HND-4, MUT-1/2, REC-1 and authority/receipt
atomicity from `docs/protocol/invariants.md`; this performance test alone is
not a proof of those invariants.

`TestCommitsProgressDuringMerge` pauses a merge indefinitely while leaving WAL
and flush I/O available. Three batches overflow both memtables; they must all
commit within 500 ms before the merge is released. The old scheduler fails this
bound; the new scheduler passes. The test then drains both workers and reopens
and checks every acknowledged row, covering the concurrent publication order.

## Metrics and AWS confirmation

Existing `disk.wait_us_log2` is submission **through commit completion**, not
strictly before commit. Its schema and meaning remain compatible.
`disk.stages.<reason>` adds `us_log2`, cumulative `nanos`, and `max_nanos` for
`queue`, `admission_lock`, `write_lock`, `rows_lock`, `validation`,
`zonelog_lock`, `memtable_full`, `sync_lock`, `flush`, `merge`, `publish`.
`queue` is per accepted write (once, including callbacks replayed after failure);
validation is per transaction attempt; background stages are per operation.
`disk.pending`, `pending_bytes`, `peak_pending`, `admission_refused` expose the
admission backlog and refusals. Fixed stage names bound cardinality. Snapshots
are cumulative since open and are not atomic across all stages. Subtract
histogram/cumulative-time snapshots for each load step; `max_nanos` and
`peak_pending` are lifetime maxima, so restart between before/after runs.

Replay **only the NVMe tier**, with the same 16 machines / one vnode each,
original data, memory budget, group settings and write mix. Compare baseline
instrumented build and fixed build on fresh processes, at 1,000 then 1,250
writes/s/vnode for at least 60 s after warmup. Record one metrics snapshot per
second and at step boundaries. Check queue p99, pending peak, memtable-full
histogram/time, merge/flush/publication time, commit p99, errors and acknowledged
throughput. Confirmation: baseline tail aligns with merge + memtable-full
stalls; fixed memtable stall and queue tail fall while commit fsync stays short,
with no rise in refusals or loss of ACKed records after restart. If publication
or validation dominates instead, this local mechanism does not explain that
AWS run; these metrics distinguish those cases.

## Validation

- `go vet ./...`: PASS (final code).
- `go test ./internal/storage ./internal/zonelog ./internal/architecture`: PASS
  (58.240 s / 111.961 s / 1.004 s under concurrent verification load).
- `go test -race ./internal/storage ./internal/zonelog`: PASS on final code,
  77.419 s / 238.910 s; no race reports.
- `INDEXUS_LONG=1 go test ./internal/simulation/...`: PASS, 312.919 s.
- `go test ./...`: FAIL on final code because loopback listeners are denied.
  Storage, zonelog, mesh and architecture pass (56.784 s / 114.843 s /
  224.447 s / 1.854 s); the failing packages are listed below.
- The exact regression test fails on `99386d3` with
  `commits waited >500ms for a merge with WAL and flush I/O available`, then
  passes with the fix, including reopen checks. The 500 ms bound covers the
  three foreground Apply calls, not setup, draining or reopen time.
- All four 30-second load runs pass with 37,505 writes and zero refusals.

The local managed sandbox refuses TCP listeners, including loopback, with
`listen tcp6 [::1]:0: bind: operation not permitted` (also IPv4). This affects
existing HTTP/integration tests in `indexus`, adapters, `internal/app/node`,
`internal/autoscale`, and `tools/localdata`. Reproduced on the baseline with
`go test ./internal/autoscale -run '^TestScalePolicyProvisioningTimeoutIsBounded$'`.
No network tests were skipped or weakened to hide this restriction. The full
suite must be replayed in an environment that permits loopback listeners.
