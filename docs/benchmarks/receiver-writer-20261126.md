# Receiver writer: what a receiver's past cost each preload

Follow-up of the AWS run `aws-resource-direct-16k-20261126` (binary `420061d`),
whose analysis is in
[aws-resource-direct-20261126/analysis-transfers.md](aws-resource-direct-20261126/analysis-transfers.md).
On one receiver the writer lock was held 67.76 s out of 68 s, 63.95 s of it
in validation and accounting; `reservationDelta` was 33 % of its CPU, 99.4 %
of that in `stagingSessions`; `PreloadCausal` ran the capacity accounting
twice; `PlanningTotals` read the whole store at every autoscale tick.

This note is local work on branch `fix-store`, 2026-11-26. Nothing here was
run on AWS. Every number below is from one 2-core container, `NoSync`
stores in a temporary directory, Go test binaries; they compare a commit
with the next one on the same machine and say nothing about AWS latencies.

## Result

| | before | after |
| --- | ---: | ---: |
| keys examined by a preload, receiver with 200 collected sessions | 43,206 | 0 |
| keys examined by the mount of the same lot | 14,633 | 224 |
| point reads of the preload | 742 | 392 |
| capacity checks in the preload's transaction | 2 | 1 |
| preload, median of 5, 200 collected sessions | 13.3 ms | 1.2 ms |
| preload, median of 5, no collected session | 1.5 ms | 1.2 ms |
| keys examined by an autoscale tick, 40,000 zones | 47,171 | 0 |

The hypothesis is confirmed: the enumeration of the staging sessions walked
the deleted rows of every session the receiver had already collected. It
is refuted for the foreign zones: what else the receiver holds did not
enter the cost.

## 1. Reproduction

`TestReceiverCostIsIndependentOfHistory`
(`internal/storage/receiver_history_test.go`). One identical lot, 16 zones
of 4 records, is preloaded (`PreloadCausal`) and mounted (`MountZones` by
seals) on receivers that differ only by:

- the staging sessions they staged, installed and collected before: 0, 20
  or 200, each of 4 zones of 8 records and 8 receipts (72 staged rows),
  staged with `stageSnapshot` and collected with `compactStaging`;
- the foreign zones they hold: 10,000 or 40,000, one record each;
- for one case, where the dead rows are: in the memtable, or in a second
  zonelog segment behind a first one that holds the foreign zones.

Storage identities are seeded, and half of the dead sessions sort before
the live one, half after: the run is deterministic except for the times.

Counters, all test-only: `zonelog.ScanWork` (keys a `ScanWith` examined, and
the dead ones among them: tombstones and retracted keys), `rangeScans`,
`pointReads`, `rowCommits`, `capacityChecks`. Times are the wall time of
the one call, median of 5 runs of the test (minimum in parentheses).

Before is commit `3ffd17d` (the test and the counters on top of `784ca98`),
after is the head of the branch.

### Preload

| dead sessions | foreign zones | dead rows in | keys (dead) before | keys after | point reads | checks | time before | time after |
| ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 10,000 | memtable | 2 (0) | 0 | 742 → 392 | 2 → 1 | 1.52 ms (1.32) | 1.22 ms (1.01) |
| 20 | 10,000 | memtable | 4,322 (4,320) | 0 | 742 → 392 | 2 → 1 | 1.61 ms (1.49) | 1.14 ms (1.00) |
| 200 | 10,000 | memtable | 43,206 (43,200) | 0 | 742 → 392 | 2 → 1 | 13.25 ms (11.26) | 1.22 ms (1.08) |
| 0 | 40,000 | memtable | 2 (0) | 0 | 742 → 392 | 2 → 1 | 1.44 ms (1.31) | 1.12 ms (0.97) |
| 20 | 40,000 | memtable | 4,322 (4,320) | 0 | 742 → 392 | 2 → 1 | 1.87 ms (1.58) | 1.20 ms (1.11) |
| 200 | 40,000 | memtable | 43,206 (43,200) | 0 | 742 → 392 | 2 → 1 | 5.66 ms (5.05) | 1.66 ms (1.16) |
| 200 | 40,000 | second segment | 43,206 (43,200) | 0 | 742 → 392 | 2 → 1 | 12.62 ms (9.81) | 1.46 ms (1.32) |

### Mount

| dead sessions | foreign zones | dead rows in | keys (dead) before | keys after | point reads | checks | time before | time after |
| ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 10,000 | memtable | 232 (0) | 224 | 636 → 640 | 2 | 4.76 ms (4.12) | 5.29 ms (3.81) |
| 20 | 10,000 | memtable | 1,672 (1,440) | 224 | 636 → 640 | 2 | 5.60 ms (3.73) | 3.62 ms (3.44) |
| 200 | 10,000 | memtable | 14,633 (14,400) | 224 | 636 → 640 | 2 | 9.36 ms (6.18) | 5.43 ms (3.95) |
| 0 | 40,000 | memtable | 232 (0) | 224 | 636 → 640 | 2 | 3.66 ms (3.50) | 4.01 ms (3.36) |
| 20 | 40,000 | memtable | 1,672 (1,440) | 224 | 636 → 640 | 2 | 3.73 ms (3.50) | 4.26 ms (3.72) |
| 200 | 40,000 | memtable | 14,633 (14,400) | 224 | 636 → 640 | 2 | 5.11 ms (4.71) | 4.89 ms (3.47) |
| 200 | 40,000 | second segment | 14,633 (14,400) | 224 | 636 → 640 | 2 | 7.02 ms (6.42) | 4.42 ms (3.72) |

Scans: 4 → 0 for the preload, 66 → 64 for the mount. Commits: 1 for each
step, before and after. The 224 keys of the mount are the staged rows of
the lot itself, read once to certify the copy.

The times of single calls of one to ten milliseconds are noisy on this
machine (the mount at zero history is 4.76 ms before and 5.29 ms after,
with the same work counters and a lower minimum): read the counters first.
The time of the mount without history is not shown to have changed.

### What grows with history and what does not

Before:

- **Grows with the collected sessions**: the keys examined, exactly one
  per staged row those sessions ever had. 200 sessions of 72 rows are
  14,400 tombstones: the mount walks them once (one capacity check that
  enumerates), the preload three times (two checks, and the enumeration of
  a check walks the dead rows up to twice, see below). Time follows.
- **Does not grow**: scans, point reads, commits, capacity checks. The
  number of scans is the same; each one is longer. This is why the paged
  scan test with a fake reader of live rows could not see it.
- **Does not depend on the foreign zones**: 10,000 or 40,000, same counters.
- **Does not depend on where the dead rows are**, memtable or segment, as
  long as no merge dropped them.

After: none of the counters depends on the collected sessions, on the
foreign zones or on where the dead rows are. The test asserts it on the
keys and on the commits.

## 2. The tombstone hypothesis

Confirmed, with three precisions.

**The mechanism.** `stagingSessions` found the sessions with one seek per
session through the rows `staging/<session>/…`. `Tx.scanRows` →
`scanPaged` → zonelog `ScanWith` → `eachKey`, which visits every key of the
range and steps over the ones whose newest version is a tombstone before
it reaches a live row. `compactStaging` deletes the staged rows of an
installed transfer; zonelog keeps each deletion as a tombstone until a
merge that includes the oldest segment drops it. All of the measured extra
keys are dead keys (`dead` column): 43,200 of 43,206.

**More than once per enumeration** (read in code, consistent with the
counts). `scanPaged` fills a first page of 8 live rows before it calls the
visitor, and a `ScanWith` only stops at a live row: `within` is not asked
about tombstones. A seek that should stop at the first row of the next
session therefore runs on until it has 8 live rows or leaves the control
collection, through every dead row in between. With dead sessions on both
sides of the live one, both seeks of one enumeration walk dead rows.

**Tombstones, not old versions.** A key is examined once whatever the
number of its versions (`eachKey` merges the versions of a key). The
measured growth is deleted rows. Replaced versions of live staged rows
(a sequence row rewritten by each mirror batch) were not measured
separately and, read in code, cost per source that holds a version, not
per version in the memtable.

**How long the tombstones live** (measured on the small stores, read in
code for the rule). zonelog merges everything when dead and replaced rows
outnumber the live ones (`mergeable`), and a merge that includes the
oldest segment drops tombstones. With 10,000 foreign zones a flush after
the 200 sessions triggered that merge and the walk disappeared (6 keys).
With 40,000 it did not: the 14,400 tombstones stayed in the second segment
and the cost was the one of the memtable case. A receiver with much live
data keeps the tombstones of its staging history for a long time. The
captured AWS logs report 838,191 zones in the progress lines of 1,924
transfer operations (not unique zones): that order of staged and deleted
rows is what such a receiver walked.

**Not established**: the mix on the AWS receiver itself (tombstones in the
memtable or in segments, live sessions) was not captured. The profile
gives the cost of the enumeration, this reproduction gives a mechanism
that produces it and scales with history; the two were not tied by a
measurement on the AWS data.

## 3. Changes

Each is one commit. *Functional fix*: the result of an operation changes.
*Scheduling change*: when or under which lock work runs. *Optimisation*:
same results, less work.

### 3.1 Staging session index — optimisation, with a new durable row

`internal/storage/staging_index.go`.

- `staging-sessions/<session>` → the number of staged rows of the session,
  the reservation row `staging/<session>` included; no row at zero. The
  row at `staging-sessions` itself marks a store that keeps the index.
- Maintained in `Tx.commit`, the one place where every update of a store
  ends, from the update's own pending rows: one point read per changed
  staged row (shared with the usage accounting, `Tx.committedSize`), no
  scan. Same zonelog batch as the staged rows: exact after every commit and
  after a crash at any point.
- It follows content, not transfers: a mounted and closed transfer keeps
  its row until `compactStaging` deletes its last staged row.
- The index rows of collected sessions are tombstones too, one per session
  instead of one per staged row (measured: 404 keys instead of 43,206 for
  200 sessions when the writer read the index rows). The writer therefore
  reads no index row either: `Store.staged` holds the indexed sessions in
  memory, loaded at Open and updated after each commit, under `writeMu`.
  A read transaction or a preparation on a snapshot reads the index rows
  of its own version.
- `pendingCausal` enumerated the sessions and returned, in every case, what
  its last line returns (`!allInstalled(state)`): the enumeration is gone.

Results do not change: the capacity accounting gets the same session list
(compared in every test, below).

Writers of the staging prefix, all covered by the commit hook; none calls
the index itself:

| Writer | What it writes or deletes |
| --- | --- |
| `prepareSnapshots` (`PreloadCausal`) | reservation row `staging/<operation>` |
| `stageSnapshot` via `stageOnce` (`PreloadCausal`) and `restageSnapshot` (`StageZones`, mount by snapshots) | records, receipts, header, sequence; `restageSnapshot` drops the copy first |
| `mirrorZone` (`MirrorBatch`, final delta of a mount) | receipts, records, sequence, replay hashes |
| `certifyZone` (`CertifyZones`, mount by seals) | header (rewritten, count unchanged) |
| `mountPreparedTx` | the rows a mount preparation encoded, set as they are |
| `InstallCausal`, `CancelCausal` | delete the reservation row |
| `compactStaging` / `dropStagingBatch` | delete every staged row, 256 per transaction |
| `ApplyFallback` | fallback session: record, receipt, version, reservation |
| fallback merge (`fallbackMetaTable`) | winner rows of a `fallback-meta` session |
| `AckFallback` | delete acknowledged rows; `tx.drop` of an emptied session |
| `PreloadShadow`, `MirrorShadow` | the shadow store, another zonelog store with no capacity accounting: not indexed |

Compatibility: a store written by an older build has staged rows and no
index. `backfillStagingIndex` runs in the update that opens the store,
counts the staged rows once and writes the index and its marker; later
opens skip it. Not covered: a store written by this build, then by an older
build, then by this build again (the marker is there and the index stale).

Tests:

- `checkStagingIndex` compares the index with the staged rows. It runs at
  every commit of every test of the package (`verifyUsage`), Open included,
  so the crash and replay schedules of `handoff_schedule_test.go` check it
  after each recovery; `stagingSessions` compares the sessions in memory
  with the index rows at every enumeration.
- `assertStagingIndex` is the helper for other tests (index against rows,
  memory against index); it is called at the end of the collection, mount,
  fallback and disk digest tests.
- `TestStagingIndexCheckFindsBothMismatches`: a staged row without index
  row, a miscount, an index row without staged rows.
- `TestStagingIndexFollowsContentNotTransfers`, `…WithinOneUpdate` (stage
  and drop in one update, refused update), `…BackfillsAStoreOfAnOlderBuild`,
  `…SurvivesPowerLoss` (power cuts with torn writes at each of the first
  four disk operations of a staging update and of a collection batch).

The disk digest test pins the control rows byte for byte. Its three
digests are unchanged: the helper leaves the new bucket out, so they still
prove that every bucket that existed is written as before, usage included.
The new bucket is checked against the staged rows instead.

### 3.2 One capacity check per preload — optimisation

`prepareSnapshots` no longer checks the capacity; `writeCausalChecked`
does, once, after the last write of the update. Same verdict: the removed
check saw a subset of the writes the kept one sees, in the same
transaction.

Tests: `TestPreloadChecksCapacityOnce` (one check per preload and per
replay), `TestPreloadOverCapacityIsRefusedExactlyAndWritesNothing` (a
receiver with exactly the bytes takes the preload; one byte short it
answers `ErrDataCapacity`, the zonelog sequence and the control rows are
unchanged; with room again the same offer passes).

Other paths, read in code: `StageZones`, `CertifyZones`, `InstallZones`,
`MirrorBatch`, `StageCausal`, `InstallCausal` check once. `MountZones`
checks twice (table above): once on its preparation, outside the writer,
once in its commit. The first is not a second check of one transaction;
it refuses a full receiver before it takes the writer. It stays.

### 3.3 Zones of an autoscale tick from the zone index — optimisation, and an advisory number changes meaning

`Store.PlanningZones` returns the count the zone index keeps
(`spatialCount`). `Node.retainedOwnedMetrics`, the tick's call site, asks it
instead of `OwnershipStats` → `PlanningTotals`.

Measured, one write between two calls (so that the cached totals do not
answer), median of 5:

| zones | `PlanningTotals` | keys | `PlanningZones` | keys |
| ---: | ---: | ---: | ---: | ---: |
| 10,000 | 12.4 ms | 11,701 | < 1 µs | 0 |
| 40,000 | 56.1 ms | 47,171 | < 1 µs | 0 |

The number is not the same one. `PlanningTotals` counts the zones the disk
may write; the index counts the zones it holds rows for, so a fenced zone,
or a ceded zone not yet collected, is still counted. On a disk that owns
all it holds they are equal (tested). The autoscaler only forwards this
number to the provisioner (`owned_zones`) and takes no decision from it
(read in `internal/autoscale`, not changed here).

`PlanningTotals` itself is unchanged, on purpose. Its remaining callers
need the items of the owned zones, which only the rows give:

- the item limit of a cold fold (`PinColdSurvivor`, mesh cold drain,
  REM-124): a refusal threshold, kept exact;
- `Node.Count`, the background item count behind the items trigger of the
  autoscaler and the monitoring page: at most every 30 s under writes,
  1.16 % of the CPU in the receiver profile. An estimate from
  `PlanningSample` would make that trigger approximate; not done.

Monitoring reads (`MeasuredOwnershipStats`) already open no transaction.

## 4. Proposal: prepare the preload outside the writer

Not implemented. `PreloadCausal` still seals and encodes its snapshots
under the writer.

What is left under the writer, measured on a preload of 31 zones of 32
records (2,046 staged rows, about 20 ms): the 31 seals cost 2.2 ms and the
JSON of the rows 1.8 ms when run alone, about a fifth of the call. The
rest is 5,092 point reads (2.5 per staged row), the control document and
the zonelog commit. Preparing outside the writer would remove about a
fifth of this hold, not most of it.

How it would work, following `prepareMount`:

1. On a snapshot, with a private overlay: run `prepareSnapshots`, keep the
   rows it wrote under `staging/<operation>`, the progress map, the error
   if any, and for each zone the header and sequence rows it read
   (`stagingVersion`).
2. Under the writer: every admission check of today, in today's order;
   then compare the header and sequence rows of each zone with the ones
   read; then set the prepared rows; then the control document and the one
   capacity check.
3. A mismatch retries the preparation. The stamp is per zone of this
   operation: a write to another zone, another transfer or a client write
   does not invalidate it. Only a mirror batch or a second preload of the
   same zone does.

What must be settled before it is safe, and why it was not done here:

- **The stamp does not see a collection** (read in code, not reproduced).
  `compactStaging` deletes the rows of a session in key order, 256 per
  transaction: between two batches a zone can have lost rows and kept its
  header or its sequence row, or the reverse. `TransferReserved` is a
  collectable phase (`stagingCollectable`) and is also the phase of a first
  preload. A preparation could then be applied over a copy that a
  collection changed without changing the stamp. A mount returns early on
  an installed or cancelled transfer, which is where collections run. The
  stamp of a preload would need the session's index count as well, or a
  proof that no collection runs on a transfer that can still preload.
- **Error precedence.** A staging error found by the preparation
  (`ErrConflict` of another source, a seal error) must still be returned
  after the admission errors, at the point where `prepareSnapshots` runs
  today: carry it into the update, and return it only if the stamp holds.
- **Crash and replay** are unchanged in principle (the preparation writes
  nothing, the commit is the same single batch), but the retry path and
  the idempotent replay (`stageOnce` on an existing copy) need their own
  tests with a concurrent mirror, a concurrent collection and a power cut.

A cheaper step first, enabled by the index: a session that has no index
row has no staged row, so the existence reads of its rows (one per staged
row in the usage accounting, one per receipt in `reservationDelta`) can be
answered without reading. That is about 3,000 of the 5,092 point reads of
the preload above. Not implemented, not measured.

## 5. Not verified

- No AWS run, no multi-node run, no run under client load: the effect on
  the writer hold time, on ACK/s and on p99 of the AWS scenario is not
  measured. The 67.76 s / 68 s figure is not shown to be gone.
- The session mix and the tombstone placement on the AWS receivers.
- Memory of `Store.staged` with many fallback sessions (one per zone with
  a pending fallback): it is a sorted slice copied when a session appears
  or disappears, fine for tens of sessions, not measured for thousands.
- The index scan of read transactions (`FallbackZones`) still walks the
  tombstones of the index rows, one per collected session.
- `Node.Count` still reads every row, at most every 30 s under writes.
- A store written alternately by this build and an older one.
- `Test64NodeDurableHTTPRecovery` was not run (resource limits).
