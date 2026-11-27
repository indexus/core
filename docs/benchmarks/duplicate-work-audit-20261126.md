# Duplicate work across layers: audit of 26 November 2026

Scope: work done more than once on the same data along the write, transfer,
control, summary and open paths, read hop by hop on branch `claude-dup`
(start `45f2f5c`). Line numbers are those of the branch after the fixes.

**Evidence levels.** *Measured* means a micro-benchmark or a CPU profile run
for this audit on a 2-core machine, without fsync, on small inputs; the
benchmark is named. *Read in code* means the duplicate is visible in the
source and nothing was timed. The 813 s join profile quoted in the request
(`ExportPage` 145 s, `zoneTotal`/`Children` 110 s, `boundariesIn` 66 s) is
the owner's; it is cited as context, never as a measurement of this audit.
No cluster run, no AWS run and no lab test were made: every "after" number
is a micro-benchmark, not a cluster result.

## Findings, ranked by expected impact

| # | Path | Where | What is done N times | Cost class | Evidence | Safe to remove? | Status |
|---|------|-------|----------------------|------------|----------|-----------------|--------|
| 1 | Summary export (boundary poll) | `ExportPage` `internal/summary/accept.go:204`, `wireWithLocked` `store.go:460`, `zoneTotal` `store.go:188`, `Snapshot.Children` `internal/zonelog/read.go:244` | Each exported key pinned its own zonelog snapshot and read the 65 sums of its parent to use one slot: up to 64 siblings read the same parent 64 times | per exported key, per poll page | **Measured**: `BenchmarkExportSiblings` 16.1 ms → 0.48 ms per export of 32 siblings (2,441 → 303 allocations) | Yes. A flush already values its edges this way (`valueEdges`); the page is valued under `Tree.Mu` from one committed version | **Fixed** (`7b815b1`) |
| 2 | Receiver of a transfer | `stageSnapshot` `internal/storage/staging.go:88`, `certifyZone` `:209`, `installStagedZone` `:229`, `compactStaging` `:284` | Every transferred record is written twice as data and deleted once, read back twice, JSON-decoded three times (table below) | per transferred record | Read in code | Not without a design decision: staging is the crash-recovery and replay-refusal state | Option 1 **done** on `claude-transfer` (the install writes what the certification decoded: 1 read-back and 1 decode less per record, `TestMountInstallsThePreparedCopyWithoutReadingStaging`); options 2 and 3 **proposed** |
| 3 | Client write | `Tx.value` `internal/storage/tx.go:293` → `zonelog.Store.Get`; `applyWrite` `apply.go:153`, `putCounted` `tx.go:307` | One write makes 12 point reads in zonelog, each pinning a snapshot of its own (17 for the first write of a zone). Two repeat a read of the same transaction (zone sequence, ledger receipt), three read state that only changes at control commits (control version, cold drain, cold pin), two look for a row the write is about to create (history row, zone receipt) | per write | **Measured**: reads counted with a counting reader around `applyWrite` (table below); profile of `BenchmarkApplyRecord`: 15.6 % of the write's CPU, 4.6 % for the control version alone | The two repeats are safe to remove locally; the rest needs a reader or a cache for a writer transaction | **Proposed** |
| 4 | Client write | `appendHistory` `internal/storage/apply.go:232` | The mutation row re-encodes the whole record, already encoded as `recordRaw` in `newWriteRequest` `:91` | per write | **Measured** (same profile): 9.9 % of the write's CPU | Yes if the bytes stay identical, but it means building a durable row by hand | **Proposed** |
| 5 | Source cutover | `cutoverLot` `internal/mesh/flip_units.go:270`, `prepareFence` `internal/storage/causal_zone.go:58` | The seal of each fenced snapshot (copy, sort, JSON, SHA-256 of the lot) was computed by the fence, dropped, and computed again while the lot had no writer | per lot, inside the no-writer window | **Measured** cost of what was removed: `BenchmarkLotSeal` 10.0 ms and 2.9 MB per 1 MiB lot. The removal itself is read in code | Yes: same snapshot, same function, same index | **Fixed** (`94180c6`) |
| 6 | Source copy | `copyLot` `internal/mesh/handoff_stream.go:344` (`json.Marshal(snap)` at `:357`) | Each snapshot is JSON-encoded only to measure it, then encoded again by the transport; the zone is read whole a second time by `prepareFence` and encoded twice more there (seal, fenced row) | per lot: 2 full reads, 4 encodings | **Measured** cost of the sizing pass: `BenchmarkLotEncode` 7.0 ms per 1 MiB lot. The count is read in code | The sizing pass enforces `transferZoneBytes` exactly; an estimate would move lot boundaries | **Proposed** |
| 7 | Client write, summary flush, export | `Store.Endpoint` `internal/storage/causal_data.go:207`; callers `writeAttempt` `internal/mesh/write.go:105`, `contributionOf` `internal/summary/fold.go:390`, `wireWithLocked` `store.go:470` | Each call opened a read transaction, pinned a snapshot and read the control version again, for a value that only changes with the control state | per write, per valued edge, per exported key | **Measured**: `BenchmarkEndpointAfterWrite`, 8 reads after one commit 11.4 µs / 80 allocations → 2.0 µs / 6; the first read after a commit is unchanged (1.7 µs) | Yes: `committedCausal` is checked against the zonelog seq before and after | **Fixed** (`0e8414e`) |
| 8 | Summary boundary inventory | `boundaryKeys` `internal/summary/export_inventory.go:36`, `boundariesIn` `store.go:346` | The whole zone inventory is walked again each time a zone is born or dies (spatial revision), which a join does continuously | per spatial revision change, O(zones) | Read in code (owner's profile: 66 s) | Incremental maintenance changes the cache's contract | **Proposed** |
| 9 | Client write | `putRecord` `internal/storage/records.go:291`, `Tx.commit` `:381` | The record was converted to its zonelog form (a JSON pass over the payload) to size it, then again to write it | per write | **Measured**: 6.1 % of the write's CPU in the profile; 110–120 µs → 106–112 µs per write, three runs each, close to noise | Yes: the same value, kept between the two uses | **Fixed** (`b330a2d`) |
| 10 | Client write (overwrite) | `Tx.record` `records.go:250` → `Snapshot.Get` `internal/zonelog/snapshot.go:248` | The previous record is read with its payload (frame decompression), copied, returned and, for a relayed write, sent back on the wire; its only reader uses `Deleted` (`OnLocalWrite` `internal/summary/accept.go:47`) and its size | per overwrite | Read in code | Needs a point read without payload in zonelog, and a wire change to stop echoing `Previous` | **Proposed** |
| 11 | Receiver collect | `compactStaging` `internal/storage/staging.go:284` | Every batch scanned the prefix from the start, under the writer, through the rows deleted by the batches before | per batch of 256 rows | **Measured**: `BenchmarkCompactStaging` 131.7 ms → 94.8 ms for 16,384 rows in the memtable; not measured on segments | Yes: same deletes, same phase check, cursor reset when a pass finds nothing | **Fixed** (`7f53978`) |
| 12 | Receiver install | `installStagedZone` `staging.go:229` | After clearing the zone, each staged record was looked up in the store, under the writer, to find the row it replaces: none | per installed record | **Measured**: `BenchmarkInstallStagedZone` 4.81 ms → 4.20 ms for 2,000 records | Yes: `clearZone` has just listed every committed row of the zone | **Fixed** (`83a428c`) |
| 13 | Receiver, snapshot-carrying install | `stageZonesTx` `internal/storage/causal_zone.go:272`, `stageSnapshot` `staging.go:88`, `certifySnapshot` `:201`, `certifyZone` `:209` | Three seals of one snapshot in one transaction: on staging, again on the same value in `certifySnapshot`, then on the copy read back | per zone on the fallback path | Cost per seal **measured** (10.0 ms per MiB); the count is read in code | The second is a pure repeat; the third checks the stored copy and should stay | **Proposed** |
| 14 | Source resume | `resumeZoneInstalls` `internal/mesh/handoff_stream.go:263` (`Seal` at `:289`), `FencedSnapshots` `causal_zone.go:651` | `FencedSnapshots` seals each snapshot to check it against its proof, the caller seals it again | per zone, resume path only | Read in code | Yes, by returning the seals as `FenceZones` now does | **Proposed** |
| 15 | Transfer passes | `fenceRegion` `internal/mesh/handoff.go:278`, `pendingZones` `:411`, `hasNewPendingZone` `:342`, `resumeZoneInstalls`, `Store.TransferZones` `internal/storage/zone_states.go:275` | The full zone-state list of the transfer is read and decoded again at each pass: once in `resumeZoneInstalls` and at least once in `transferZones`, for up to 6 drains plus the stragglers | per pass, O(zones of the region) | Read in code | A list of pending zones kept across passes must still see zones born meanwhile | **Proposed** |
| 16 | Control state | `Store.CausalState` `internal/storage/causal_data.go:202`, `cloneControl` `:50` | The whole control state (rights, every transfer) is cloned to read one transfer or one field: `handoff.go:30`, `:107`, `:203`, `:413`; `service.go:578`, `:690`, `:732`, `:778`, `:803`, `:823`, `:1014` | per handoff, per pass, per resume | Read in code | Yes for single-field readers, through the committed snapshot | **Proposed** |
| 17 | Control state | `MembershipReplica` `causal_data.go:43` | A membership is rebuilt by merging every member where a shared immutable one exists (`pureMembership`, `causalSnapshot.membership`): under the writer in `sourceEndpoint` `internal/storage/granular_transfer.go:136`; also `handoff.go:47`, `service.go:830`, `cold.go:148/220/316/377`, `causal_data.go:296/821/845/893/936/988` | per transfer begin, per move, per cold step | Read in code | Yes where the membership is only read; the callers that merge into it need their own | **Proposed** |
| 18 | Open | `scanSpatial` `internal/storage/spatial_index.go:312`, `ensureSpatial` `:265`; `bulk.go:190` | The spatial index is rebuilt by reading the header of every row at first use after each open, and after a bulk replacement; it is not stored | per open, O(rows) | Read in code | Needs a persisted zone list kept exact by every commit | **Proposed** |
| 19 | Open | summary tree (`LocalTransferRevision` `internal/storage/store.go:346`) | Edges, incoming contributions and the boundary inventory are rebuilt by a full fold and a poll of the other hosts after each open | per open | Read in code, not examined in depth | Derived state; persisting it is a design decision | **Proposed** |

## Counts per path

### One client write (applied locally)

Read in code, with the profile shares measured on `BenchmarkApplyRecord`
(8 metrics, 1 KiB payload, no fsync; `encoding/json.Marshal` is 19.8 % of
the write's CPU in total).

| Step | Encodings / hashes / reads |
|------|----------------------------|
| HTTP decode (`meshhttp/http.go:203`) | 1 JSON decode of the request |
| `newWriteRequest` (`apply.go:91`) | 1 JSON encoding of the record (`recordRaw`), 1 SHA-256 |
| `causalWritable` (`permission.go:9`) | twice: before the writer (a hint) and under it (the check). Each reads the control version row. The second is required |
| control rows read under the writer | 11 point reads, counted: control version 1, cold drain 1, cold pin 1, ledger receipt 2, usage 1, row bytes 1, zone sequence 2, zone receipt 1, history row 1. The first write of a zone makes 16: control version 4, zone sequence 3, storage id 1 |
| `putRecord` | 1 point read of the previous record with payload (finding 10); 1 conversion to the zonelog form (2 before fix 9) |
| `appendHistory` | 1 more JSON encoding of the whole record (finding 4) |
| `putRequestReceipts` (`receipts.go:44`) | 1 receipt encoded once, written twice (zone and ledger): required, the first travels with a handoff, the second answers retries |
| commit | the kept zonelog form; zonelog encodes and compresses the frame once |
| after the commit | `Endpoint` and `WriteRegionHint`: 1 control version read for both (2 before fix 7) |
| summary | `OnLocalWrite`: a mark, no read |
| HTTP encode | 1 JSON encoding of the response, with `Mutation.Record` and `Previous` |

A write relayed by another node also carries the record back in the
response (`Mutation.Record`) and the previous record (`Previous`), which
the relaying node does not use (`indexus/mesh_runtime.go:167`).

### One transferred record, at the receiver (normal path: preload, then mount by seals)

| Operation | Count | Where | Required? |
|-----------|-------|-------|-----------|
| JSON decode from the wire | 1 | transport | yes |
| Hash | 1 | `stageSnapshot`: seal of the preloaded snapshot, kept in the header | yes: a repeated preload must be the same bytes (replay refusal) |
| Write to staging | 1 | `stageSnapshot`, one row per record, JSON | yes today: a crash between preload and mount resumes from it |
| Read from staging + JSON decode + hash | 1 | `certifyZone` → `stagedSnapshot`, in `prepareMount`, outside the writer | yes: it proves the staged copy plus mirrored mutations is exactly the fenced snapshot (exact reads) |
| Read from staging + JSON decode | 1 | `installStagedZone`, under the writer | **no**: the same rows were read and decoded by the certification, and the mount already proves they did not change in between (`stagingVersion`) |
| Point read of the live row | 0 (1 before fix 12) | `putCleared` | no |
| Write live | 1 | `installStagedZone`, zonelog form | yes |
| Delete from staging | 1 | `compactStaging`, a tombstone per row | follows from staging |
| Receipts | read 2, written 2 | staged, then ledger and live zone | the two destinations are required |

So each record is written 3 times (staging, live, tombstone), read back 2
times, JSON-decoded 3 times and hashed 2 times. On the fallback path (the
install carries the snapshot) add one staging rewrite and two seals
(finding 13).

### One lot, at the source

| Operation | Count | Where |
|-----------|-------|-------|
| Full read of each zone with payloads | 2 | `Snapshots` (`read.go:127`) for the copy, `liveSnapshot` in `prepareFence` for the fence |
| JSON encoding of each snapshot | 4 | sizing in `copyLot`, transport of the preload, seal, fenced row |
| Seal | 1 (2 before fix 5) | `prepareFence` |
| Control state clone | 1 per pass | `pendingZones` |

The second read is required: the fence must capture the zone at its final
sequence, and the fenced row must be durable before the cession. The sizing
encoding is not.

## What needs a decision

### Receiver staging written twice (finding 2)

1. **Keep staging, drop the second read.** `prepareMount` already reads,
   decodes and certifies every staged record outside the writer, and
   `mountPreparedTx` refuses to commit if the staging header, sequence or
   zone state changed since. The decoded records can ride in
   `preparedMount` to `installStagedZone`, which then reads nothing under
   the writer. Safety: the argument that lets the mount reuse
   `prepared.pending` today covers the records too, since staging only
   changes through the header or the sequence. Nothing durable changes.
   This is the smallest step and removes the only part of the install that
   reads payloads under the writer. It was not done here because it threads
   state through five functions of the install path, two of which
   (`InstallZones`, `StageZones`) are also entered without a preparation.
2. **Stage in the final form.** Write staged records as zonelog record rows
   of a shadow collection, so the install is a rename of key range rather
   than a rewrite. zonelog has no rename; it would need one, with the same
   atomicity as a batch. Removes one write and the tombstones. Changes the
   storage format.
3. **No staging: install on arrival, readable only after the seal.** The
   record is written live once, hidden by the zone's install flag.
   Removes a write, a read and the tombstones, but a cancelled transfer
   must then delete live rows, and "a preload never touches a live zone"
   stops being true. This changes the crash-recovery argument and is not
   recommended without a simulation first.

### Control rows read one by one in a write (finding 3)

A writer transaction holds `writeMu`, so nothing commits under it, yet each
`tx.value` pins a zonelog snapshot and searches the memtable and segments.
The two exact repeats can go without any of this: `applyWrite` reads the
zone sequence, then `putCounted` reads it again to count the difference,
and `replayReceipt` finds no ledger receipt, then `putCounted` looks for it
again. Passing the value already read removes 2 of the 12 reads. For the
rest, two options: pin one snapshot for the whole update (`updateLocked`) and read
through it; or keep the handful of rows every write reads (control version,
usage) in the store between commits, as `committedCausal` does for the
control state. The first is small but changes what a `peek` sees inside an
update; the second adds a cache that every commit must keep exact.

### History row from the bytes already encoded (finding 4)

`recordRaw` is exactly the `Record` member of the mutation's JSON. The row
can be assembled around it. The bytes would be identical, and a test can
pin that against `json.Marshal`, but a durable row is then built by string
concatenation and breaks silently if `Mutation` gains a field. A cleaner
variant is a `Mutation` whose `Record` is a `json.RawMessage` at this one
call site.

### Sizing a lot without encoding it (finding 6)

Either the transport accepts the bytes `copyLot` already produced (a
`json.RawMessage` per snapshot in the preload request, which changes the
request type but not the wire), or `copyLot` sizes with the estimate
`Snapshots` already computes and `transferZoneBytes` is enforced by the
transport's body cap. The first keeps the exact bound.

### Boundary inventory (finding 8) and state rebuilt at open (findings 18, 19)

The inventory can be maintained from the spatial changes each commit
already computes (`spatialChanges`), instead of a walk per revision. The
spatial index can be stored as one row per zone, written in the commit that
creates or empties the zone, and loaded at open with a key-only scan of
that prefix. Both make a derived structure durable or incremental, and both
need the invariant "never stale after a crash" stated and tested first.

## Fixes made here and how each was checked

| Fix | Test (fails before, passes after) | Measurement |
|-----|-----------------------------------|-------------|
| 1 `ExportPage` | `TestExportPageReadsSiblingParentsOnce`: 32 keys under 1 parent read children 32 times before, once after; every value compared with the store | `BenchmarkExportSiblings` |
| 5 seals | `TestFenceZonesReturnsTheSealOfEachSnapshot`: seals equal `Seal()` of each snapshot, match the proofs, survive a replay and certify the receiver's copy. Before the fix the call did not return them: the "before" is a compile failure, not a failing assertion | `BenchmarkLotSeal` (cost of the removed call) |
| 7 `Endpoint` | `TestEndpointIsFreeBetweenCommits`: 10 allocations per call before, 0 after; a new epoch is seen after its commit | `BenchmarkEndpointAfterWrite` |
| 9 one log encoding | `TestWriteEncodesItsRecordForTheLogOnce`: 2 conversions per write before, 1 after; a row put twice then removed commits its last state | profile and `BenchmarkApplyRecord` |
| 11 `compactStaging` | `TestDropStagingBatchResumesAfterTheLastRow`: the second batch scanned from the prefix before, from the cursor after. `TestCompactStagingDropsEveryBatch`: no row left, staging usage 0, a moving transfer keeps its staging | `BenchmarkCompactStaging` |
| 12 install | `TestInstallStagedZoneReadsNoRecordRowAfterTheClear`: 50 point reads before, 0 after. `TestInstallStagedZoneReplacesTheLiveRows`: content and usage after an install over existing rows | `BenchmarkInstallStagedZone` |

Two counters were added for these tests, as `controlValueReads` and
`usageScans` already are: `logEncodes` (storage) and `childrenReads`
(summary). Each is one atomic add on its path.

Verification: `gofmt -l .` prints nothing; `go build ./...`; `go vet` and
`go test -count=1` on `internal/storage`, `internal/zonelog`,
`internal/summary` and `internal/mesh` pass; the summary, export, poll and
boundary tests of `indexus` pass. The rest of the `indexus` suite and the
lab test were not run.

## Not examined

- `internal/wire` (set and record block codecs) and the HTTP adapters
  beyond counting the JSON passes of a write.
- zonelog internals: whether one transaction decompresses the same value
  frame several times (`cache.get`, `valFile.read`), rows decoded by
  `page.row` and then discarded, and what flush and merge rebuild. Segment
  indexes and filters are read from the segment tail through the cache at
  open; that much was read, nothing was timed.
- The typed-collection summary path (`typedValueWithIncoming`,
  `typedSetWithIncoming`), `FlushPendingEdges` beyond `valueEdges`, and the
  poll side (`PollRemoteHosts`, `acceptAll`).
- Cold drain and fold paths (`internal/storage/cold.go`,
  `internal/mesh/cold.go`), shadow replication, bulk ingest, the autoscaler.
- The read path (`/sets`, inventory pages, read delegation).
