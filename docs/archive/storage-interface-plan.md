# The storage interface for the cut (migration step 1)

## Status, 2026-11-14 (`feat/bulk-ingest`)

A store is one zonelog directory, `<path>.records`; there is no bbolt file.
Record rows are zonelog records; the control state (meta, authority and
fences, zone sequences, receipts, history, staging, counters) is rows of
the collection `\x00kv` in the nested buckets bbolt gave it
(`internal/storage/kv.go`: `Tx`, `Bucket`, `Cursor`), so the code above
it is the code that ran on bbolt. An update commits its record rows and its
control rows as one zonelog batch: one frame, one fsync, all or nothing.
Readers take `rowsMu` and see one commit; scans call back outside zonelog's
lock, a page at a time, so a callback may read the store again.

A store from earlier on this branch (control state in a bbolt file beside
the rows) is moved once at open and its file renamed `.migrated`; a bbolt
file that holds the records too is refused (`ErrOldEngine`) and read only by
`ExportOfflineRecords`. The pinned disk digests
(`TestDiskDigestOfFixedMutationSequence`) are unchanged: the control rows
hold, in order, the keys and values bbolt held.

Left from the plan: control state as control records with `IfSeq`, freeze
as a seq and a seal, the cluster-wide seal switch, and a control half
written for zonelog rather than kept in bbolt's shape.

The rest of this page is the plan as it was written.

## Plan

Prepared, not applied. `internal/storage`, `indexus/` and `internal/mesh` are
being edited on `fix/node-resources`; this page is what to do once that
branch is merged. Call sites below were counted on `refactor/layers-v2` at
`3b2aec1` and must be counted again on the merged tree (last section).

## The cut is inside `internal/storage`, not around it

`*storage.Store` has two halves in one type:

- a **state machine**: grants, fences, transfers, cold folds, placement. It
  is 2 600 lines (`causal_data.go`, `causal_zone.go`, `cold.go`, `staging.go`,
  `routing.go`, `permission.go`, `causal_cache.go`) and it is what the tests
  of verification.md exercise. It stays.
- **bytes**: records, sequences, history, receipts, staged copies, usage
  counters, all written as bbolt buckets in the same transaction as the
  state. About 2 300 lines. This is what zonelog replaces.

So step 1 does not put an interface between `mesh`/`indexus` and storage:
every exported method they call keeps its name and signature, and no file
outside `internal/storage` changes. Step 1 puts one interface between the
two halves, with bbolt behind it.

## The interface

```go
// records is the bytes under the state machine: one node's records, its
// control state, and the staged copies of zones it is receiving.
type records interface {
	// ID is the identity of this disk; grants name it.
	ID() string

	// Apply commits a batch atomically and durably. check runs under the
	// write lock on the state the batch applies to and may refuse it: the
	// fence and authority check of a write. Control state is a record of the
	// collection zonelog.Control in the same batch.
	Apply(batch []zonelog.Mutation, check func(zonelog.Getter) error) ([]zonelog.Result, error)

	Get(collection, location, id string) (zonelog.Record, bool, error)
	// Scan visits the live records at or below a zone in key order, from
	// behind `after` when it is set: one page of an inventory.
	Scan(collection, zone string, after *zonelog.Record, visit func(zonelog.Record) bool) error
	// Children is the count, the sums and the one-location rule of a zone
	// and of its 64 children.
	Children(collection, zone string) ([65]zonelog.Sum, error)
	// Seq is the last write readers see: the watermark of a read.
	Seq() uint64

	// Since is the mutations of a zone after a seq, as one interval.
	Since(collection, zone string, after uint64) (zonelog.Tail, bool, error)
	// Seal is the digest of the live records of a zone.
	Seal(collection, zone string) (zonelog.Seal, error)
	// Drop deletes up to limit records of a transferred zone.
	Drop(collection, zone string, limit int) (int, error)

	// Stage opens the staged copy of an incoming transfer: a store of its
	// own, in no read of this one. Mirror applies a tail to it and records
	// its progress; Ingest installs one of its zones here in one cut.
	Stage(operation string) (staged, error)
	Ingest(from staged, collection, zone string) error
	Unstage(operation string) error

	Stats() zonelog.Stats
	Close() error
}

type staged interface {
	Apply(batch []zonelog.Mutation, check func(zonelog.Getter) error) ([]zonelog.Result, error)
	Mirror(name string, tail zonelog.Tail) error
	Seal(collection, zone string) (zonelog.Seal, error)
	Get(collection, location, id string) (zonelog.Record, bool, error) // the progress record
}
```

`*zonelog.Store` has every method of `staged` and of `records` except the
three `Stage` ones, which are a directory per operation next to the store
(`<data>/staging/<operation>/`): 30 lines in `internal/storage`.

The bbolt side of step 1 is the existing code moved behind these methods,
bucket by bucket:

| Method | bbolt today |
| --- | --- |
| `Apply` | `applyWrite` in `diskBatch`; `check` is `causalWritable(tx, zone)` |
| control records | the `meta` bucket: causal state, transfer intents, summary epoch, cold state |
| `Scan` | `eachInventoryZone` + `snapshot` (`read.go`, `inventory_page.go`) |
| `Children` | `ZoneMetricsContext` / `WalkInventoryMetricsContext` (`inventory_metrics.go`) summed by child |
| `Seq` | `Revision` |
| `Since` | `MutationsBatch` over the `history` bucket, one zone at a time |
| `Seal` | the same digest computed by a scan, so that both sides of the dual-run agree; `Snapshot.Seal` stays until the cut |
| `Stage`, `Mirror`, `Ingest` | the `staging` and `transfer-snapshots` buckets (`staging.go`: `stageSnapshot`, `mirrorZone`, `installStagedZone`) |
| `Drop` | the delete loop of `CollectTransferred` |
| `Stats` | `storedDataUsage` |

## What `mesh` and `indexus` call, and what happens to it

Counted with the command of the last section; `×n` is call sites outside
`internal/storage`, tests excluded.

**Unchanged in step 1 and at the cut** (state machine; reads and writes its
state through control records instead of the `meta` bucket):
`CausalState` ×16, `CausalRouting` ×22, `MergeCausal` ×4, `CausalKnowledge`,
`CausalHolder` ×2, `CausalInitialized`, `InitCausal` ×2, `InitCausalReceiver`,
`CaptureCausal`, `PreloadCausal`, `StageCausal`, `EmitCausal`,
`InstallCausal`, `CompleteCausal`, `FreezeRegion`, `FreezeZones`,
`EmitZoneTransfers`, `StageZones`, `InstallZones`, `CollectTransferred`,
`BeginCausalMove`, `PublishCausalMove`, the ten cold-fold methods
(`ColdStatus` ×7, `BeginColdDrain`, `PinColdSurvivor`, `ReleaseColdPin`,
`AbortColdDrain` ×2, `PublishColdRetirement`, `WitnessColdRetirement`,
`CompleteColdRetirement`, `ConfirmColdPin`, `RequestColdAbort`), `Identity`
×4, `Close`.

**Same signature, new body at the cut** (bytes):

| Method | Callers | At the cut |
| --- | --- | --- |
| `ApplyRecordContext` | `mesh/service.go` | `records.Apply` of one mutation with the fence check; `Mutation.Seq` is the store's seq, not a per-zone counter |
| `CausalInventory`, `CausalInventoryPrefix`, `CausalInventoryPage`, `InventoryRegion` | `mesh/service.go` ×4 | the completeness guards stay; rows come from `Scan`; the page cursor keeps `(location, id)` and the watermark is `Seq` |
| `MutationsBatch` | `mesh/service.go` | one `Since` per zone of the batch |
| `Mirror`, `MirrorBatch` | `mesh/service.go` ×2 | `staged.Mirror` |
| `ZoneMetricsContext`, `WalkInventoryMetricsContext` | `indexus/summary_cache.go` ×2 | `Children` (step 6 then removes the fold that calls them) |
| `PlanningStats`, `PlanningTotals` | `indexus/scale_policy.go`, `indexus/monitoring.go` ×4 | counts and bytes from `Children` of the owned regions and `Stats`; no per-write counter |
| `CapacityUsage` | `indexus/scale_policy.go` | `Stats`: segments, payload files and log |
| `Revision` | `indexus/summary.go` | `Seq` |
| `NextSummaryEpoch` | `indexus/mesh_runtime.go` | a control record |
| `SeedLocalRecords`, `SealLocalSeed` | `tools/localdata/main.go` | `Apply` in batches |
| `OpenWithOptions` | `app/node/mesh.go`, `tools/localdata/main.go` | the path names a directory, not a `.causal.db` file |

**Deleted at the cut** (bbolt only):

- `Options.PageSize`, `BatchWrites`, `BatchSize`, `BatchDelay`, `OmitHistory`
  and the config key `storage.omit_history`: there is no history to omit
  and grouping is the log's. `MaxPendingWrites`/`MaxPendingBytes` stay as an
  admission counter in front of `Apply`.
- `write_batch.go` (`diskBatch`), `zone_table.go`, `history.go`,
  `receipts.go`, `usage.go`, `planning_stats.go`, `inventory_metrics.go`,
  `revision.go`, `inventory_revision.go`, `summary_epoch.go`, `codec.go`, the
  record half of `apply.go`, `read.go`, `staging.go`.
- `RequestReceipt` and `Snapshot.Requests`: a record carries the hash of the
  request that wrote it (`Record.Request`); receipts of replaced writes stay
  on the disk that took them (RETRY-1).
- `MutationRange{After, Through}` per zone and the `ErrSequence` check of
  `mirrorZone` on `m.Seq == seq+1`: replaced by the interval check of
  `Mirror` and by the seal.
- `ZoneSeal.Digest` as the hash of the JSON snapshot: replaced by
  `zonelog.Seal`. The wire keeps `ZoneSeal`; only how it is computed changes,
  on a cluster-wide switch like the one ixr1.md describes.
- The bbolt-only tests: `mmap_growth_test.go`, `disk_digest_test.go`,
  `insert_cost_bench_test.go`, `metric_header_test.go`, `seal_bench_test.go`.

**Already without a caller outside the package** at `3b2aec1`, and
removable before step 1 if their tests move to the batch forms: `Apply`,
`ApplyRecord`, `Certify`, `CertifyBatch`, `CausalFrontier`,
`EmitZoneTransfer`, `FreezeZone`, `InstallZone`, `StageZone`, `Inventory`,
`InventoryContext`, `LoadCausalTransfer`, `Mutations`, `Prepare`,
`PrepareCausal`. (`Write` and `Snapshot` have callers that this count cannot
tell from other types' methods of the same name: check by hand.)

## What the state machine must do differently

These are the places where one Bolt transaction did two things at once.

1. **Fence and write.** Today `applyWrite` reads the fence in the write
   transaction. With `records`, the fence is a control record and the check
   is the `check` of `Apply`: same lock, same state. `FreezeZones` is an
   `Apply` of control records; a write that raced it is in the log before
   the fence or refused after it (HND-4).
2. **State transitions.** Each `db.Update` that reads the causal state,
   changes it and writes rows (33 of them) becomes: read the control record,
   build the batch, `Apply` it with `IfSeq` set to the seq of the control
   record read. A transition that lost a race gets `ErrStale` and starts
   over. Transitions are already serial per store; the guard is there so
   that a mistake is an error and not a lost update.
3. **Frozen snapshot.** `FreezeZones` stores each zone's snapshot in
   `transfer-snapshots`. With `records` it stores the seq `F` and the seal;
   the rows are read again by `Scan` when needed: after the fence nothing in
   the zone changes.
4. **Install.** `installStagedZone` moves staged rows to live rows and marks
   the grant installed in one transaction. With `records`: `Ingest`, then an
   `Apply` of the control record. A crash in between leaves rows that no
   read serves (the grant is not installed); recovery sees the transfer
   emitted and not installed, drops the zone and ingests again.
5. **Capacity.** `ErrDataCapacity` is checked on the exact usage counter in
   the write transaction. With `records` it is checked on `Stats` before the
   write: a bound that can be exceeded by one batch.

## Order of work

1. Merge `fix/node-resources`. Recount the call sites.
2. Delete the methods without a caller (one commit, tests moved).
3. Add `records` and `staged`; move the bbolt code behind them file by file,
   `apply.go` first, one commit each, the package's tests green in between.
   No behaviour change: `TestDiskDigestOfFixedMutationSequence` keeps its
   digests.
4. Add the zonelog implementation and the flag for the dual-run (step 3 of
   zonelog.md).

Recount:

```sh
for m in $(grep -hoE '^func \(s \*Store\) [A-Z]\w*' $(ls internal/storage/*.go | grep -v _test) | awk '{print $4}' | sort -u); do
  echo "$m $(grep -rnE "\.$m\(" --include=*.go internal/mesh indexus internal/app internal/adapter cmd tools | grep -v _test | wc -l)"
done
```
