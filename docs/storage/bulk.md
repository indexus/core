# Bulk ingest: prepared batches in one cut

**Statut : actuel.**

A node installs a prepared batch of records without writing them
one by one: one pass over the batch, one zonelog segment, one receipt. The batch is the
preparation pipeline's: IXP1 parts and a manifest, written by `tools/prep`
or `localdata -stage` ([../ingest/pipeline.md](../ingest/pipeline.md),
section 5; code `internal/prep`).

## The batch, as the node takes it

A directory: `manifest.json` (`indexus-prep-v1`), `part-NNNNN.ixp` (records
sorted by key in column blocks, each with a CRC-32), optional `zones.ixp`
(the sums of the zones above the records). Over HTTP the same files as a tar
stream: the manifest first, then the parts in manifest order, then
`zones.ixp`. The node reads the batch with `prep.Each`, which checks every
part's SHA-256, block CRC, index and footer, key order within and across
parts, and the record count. On top of that the node checks:

- **One collection, one zone.** The batch's zone is the deepest zone holding
  its first and last record (their common location prefix; `@` for a whole
  collection). Every record is at or below it by key order.
- **Weighted rows.** A row's weight is the number of items it stands for
  (0 means 1; finite, not negative). It is kept on the record (`Weight`,
  omitted when 1) and counted in the zone sums and `/sets` as that many
  items; zones.ixp is checked against the weights as written (a plain row
  of weight 0 sums 0 there); metrics are summed as given. Ordinary writes take the same field.
  At most 128 metrics (`storage.MaxMetrics`; a wider batch or write is refused, never stored without its metrics), finite values, payloads that are JSON (made canonical,
  compact and HTML escaped, as a write stores them) unless the batch says
  `payloadEncoding: "binary"` (below).
- **zones.ixp**, when there is one: its SHA-256, and that its rows at its
  shallowest depth (the root, from `localdata -stage`) equal the sums of the
  records, to a relative 1e-9.
- **Batch key** = SHA-256 of format, collection, metric count and the parts'
  SHA-256 (16 bytes, hex): the same parts are the same batch, whatever made
  them. The manifest's `id` (a hash of the inputs) is kept beside it.

Where this differs from section 5.5 of pipeline.md: the node does not forward
runs it does not hold (the tool splits a batch by owner, below; a node
refuses the rest); fresh mode wants the batch's zone empty, not only the key
interval of the batch; replace retracts and deletes the old records in the
segment itself, so the cut is still one manifest rename (no `Drop` first);
rows get no request hash, which would cost a SHA-256 per record; `zones.ixp`
is checked before the cut, against the records, not after it against
`Children`.

## What the node does

`POST /bulk[?replace=1]`, body the tar stream, `Authorization: Bearer` with
the `write` scope, like `/item`. There is no forwarding: a node takes the
zones it holds.

1. **Spool.** The tar is read into `<storage>.causal.db.bulk-*` beside the
   store. After the manifest, a batch already installed is answered at once
   (`"Already": true`) without reading the rest. Open removes spools a crash
   left.
2. **Quiet check.** Refused (503) while a handoff, a move or a cold fold is in
   progress: those copy zones by their sequence and history, and a batch has
   neither.
3. **One pass under the write lock.** Writes, fences and handoff transitions
   wait (they take the same lock); reads go on. The pass reads every part
   (the checks above) and the writable authority of this disk for each zone a
   record is in, against the causal state, which cannot change meanwhile. It
   streams the rows into one zonelog segment (`zonelog.BeginBulk`,
   `AddHead`: an IXP1 head is the zonelog key without its collection
   number). At the end: the zone sums and the capacity.
4. **The cut.** The segment is synced, then named by a manifest rename. The
   batch's receipt is a row of the same segment (collection
   `zonelog.Control`, id `bulk/<batch>`). Then a second control batch records
   the receipt and the usage. A crash between the two is finished by Open
   (`reconcileBulk`): it finds the zonelog receipt and recounts the rows.
5. **Summaries.** The node rebuilds its summaries once from the disk (the
   forced inventory fold). Each boundary below a remote parent is queued
   once and goes with the next flush, in batched contributions (paged polls
   take them in pages of 8 192). Cached `/sets` reads of the collection are
   dropped. The answer comes after this: the batch is queryable when the call
   returns.

Without `replace` the zone must hold no record on this node (409 otherwise);
with `replace` the segment retracts every record of the zone this node holds
and deletes those the batch does not write again: afterwards the zone is the
batch. Records outside the zone are untouched.

**History, receipts, sequences.** A batch writes no history, no per-record
receipt and no zone sequence: its zones have sequence 0 until a write. A later
write over a bulk record is an ordinary write (its zone's history starts
there). A handoff carries bulk zones in its frozen snapshot, which holds every
record; the mirror has nothing to replay below sequence 0. Retrying a batch is
recognised by its content, for ever (the control state keeps the receipt).

**Answers.** 200 with the receipt (`Batch`, `Source`, `Zone`, `Records`,
`Zones`, `Replaced`, `Bytes`, `Already`); 400 invalid batch; 409 overlap; 421
a zone held elsewhere or frozen; 503 busy (retry); 507
capacity. A stream may name only the files of its manifest, by base name.

## The tool

```sh
localdata -bulk PATH -write ENDPOINTS -issuer ISSUER [-replace]
localdata -stage DIR -records FILE.ndjson
```

`PATH` is a batch directory, a directory of them, or an NDJSON file that is
staged first (one IXP1 batch per collection, zone `@`, parts cut at depth-2
zones, `zones.ixp` for depths 0 to 2, the last version of a key kept, held in
memory: `tools/prep` is for large inputs). With several endpoints each batch
is split by the XOR owner of its zones (names read from `/ping`), one batch
per node; a node that does not hold its part refuses it.

## Measured

`scripts/research/bulkbench.sh` on this container: 2 vCPU Xeon 2.1 GHz, 8 GB, ext4 on
a virtual disk, real fsync; the node as `explore.sh` starts it
(`GOMAXPROCS=4 GOMEMLIMIT=1024MiB`, `omit_history`). Vegetation is one
record per depth-5 zone, 13 metrics, a 200-byte JSON payload; large is 20 to
60 KB of payload. Raw bytes (IXR1 value plus flat key): 340 B and 40 081 B
per record; the IXP1 batches are 385.9 MB and 802.3 MB. Numbers vary by
about 30 % with the load of this shared machine.

| | first IXP1 run | summaries from zonelog | streaming | engine cut (2026-11-14) |
| --- | ---: | ---: | ---: | ---: |
| 1.12 M vegetation, end to end | 8.0 s · 139 656/s | 5.72 s · 196 000/s | 4.16 s · 268 912/s | **4.43 s · 252 500/s** |
| node RSS peak, vegetation | 1 026 MB | 95 MB | 64 MB | **99 MB** |
| large (802 MB), end to end | 111 MB/s | – | 141 MB/s | **144 MB/s** |
| node RSS peak, large | 221 MB | – | 28 MB | **27 MB** |
| records served after the bulk | 1 120 000 | 1 120 000 | 1 120 000 | 1 120 000 |

- The 3 s fold after a bulk is gone: owned zones are read from zonelog's
  sums (`Children`) at each read; memory holds only the contributions of
  remote children and the boundaries to push. The radix is deleted.
- The tar is read as it arrives (`prep.ReadPart`): each part once, its
  SHA-256 and block CRCs on the way, no spool. Payloads are copied once,
  into the segment's payload file.
- Disk / raw: 1.095 (vegetation record files), 1.001 (large).

Ordinary writes, `localdata -write` of 20 000 vegetation records,
concurrency 32 (two runs each, same machine, same minute):

| engine | records/s | RSS peak | disk |
| --- | ---: | ---: | ---: |
| bbolt (rows and control in one file, one fsync) | 3 090 · 3 180 | 83 MB | 50.4 MB |
| zonelog rows + bbolt control (two fsyncs a group) | 3 760 · 3 640 | 66 MB | 40.6 MB |

Group commit amortises both fsyncs over up to 256 writes; the zonelog side
is cheaper per row than bbolt's B-tree pages. Since the control state moved
into zonelog there is one fsync per group.

## Using it

```sh
localdata -bulk <batch dir> -write <node URLs> -issuer <issuer URL> [-replace]
```

`scripts/local/explore.sh` runs this for every batch built by
`scripts/local/prep-all.sh`. Re-running is safe: a batch already installed
is answered from its manifest. To load a collection again with other
records, pass `-replace`. A data directory of an earlier engine (a bbolt
file at the store's path) does not open: start from an empty one.
`localdata -export` reads a stopped zonelog store.

## The engine cut (2026-11-14)

zonelog is the only record engine. Deleted: the `records` interface and
`boltRows`, the planning-counter index (planning stats are counted from the
rows), the offline bbolt audit, the metrics walkers, and in `indexus` the
summary radix. `storage.engine` accepts only `zonelog`. Since then bbolt
left the module altogether: no migration of the bbolt control file, no
export of bbolt records (`ExportOfflineRecords` reads zonelog). Tests, run
on zonelog only:

- removed, they read bbolt's record layout or the radix:
  `TestInventoryPagesResumeAfterEmptyZone` (an empty zone bucket),
  `TestCausalPrefixInventoryFiltersBeforeDecodingRecords` (a corrupted bbolt
  row), `TestFlatZoneTablesClearOnlyRecordsAndPreserveCursorOrder`,
  `TestMetricHeaderMatchesCanonicalRecords`,
  `TestWalkInventoryMetricsMatchesSnapshotAndStopsOnError`,
  `TestPlanningTotalsCacheTracksCommittedVersionAndRejectsMissingIndex`,
  `TestRadixSummariesMatchEveryLogicalPrefix`,
  `TestRadixForeignAggregateCutsOffLocalDescendants`,
  `TestCompactReadsDoNotMaterializeUnaryPrefixes`,
  `TestSummaryZoneSequencesKeepFoldAndDeltaIdempotent`,
  `TestSummaryZoneRecomputeIsReplaceNotDelta`,
  `TestPublishedSummaryValuesRemainImmutableOnDirectDelta`,
  `TestSharedUnarySummaryMatchesWireCorrectionAndMetricWidths`,
  `TestSummaryInventoryFoldRetriesWhenANewerDeltaRejectsTheSnapshot`,
  `TestSummaryMetricsFoldPreservesCompensationAndIgnoresPayload` (zonelog
  sums were plain float64 at the cut; they are compensated again since
  format `zonelog-2`, see [zonelog.md](zonelog.md));
- rewritten: `TestDiskDigestOfFixedMutationSequence` (new digests: the
  control half), `TestOmitHistoryKeepsTheLiveRecordOnly`,
  `TestPlanningStats*` (bytes as zonelog rows take them, no index rollback),
  `TestFlatZonesPreserveRecordsReceiptsCountersAndRestart`, the child
  ordering tests on `newerContribution`
  (`TestSummaryChildrenMatchMapOracleThroughReorderedVersions`,
  `TestAcceptChild*`, `TestSummaryEpochOrdersNewIncarnationBeforeVolatileCounters`),
  `TestNodePrefixRefreshAndRemoteDeepReadUseFilteredInventory` (no write
  queue to keep).

Lines of `internal/storage` + `internal/zonelog` + `indexus/summary*`
(since November 2026 `internal/summary`) (production · tests): 6 450 · 5 702 before this branch (no zonelog), 11 321 ·
8 040 before the cut, 10 122 · 7 536 after. zonelog itself is 2 830 · 1 381.

## Payload compression (2026-11-14)

Payload files are 64 KiB flate frames (fastest level, two workers off the
ingest goroutine); a read decompresses the frames it needs. `/records` is
gzipped when accepted (`/sets` already was). Disk and wire against the
binary payload bytes (`TestPayloadDiskAndWire`, `PAYLOADBENCH`):

| batch | payload MB | payload files / payload | JSON · gzip MB | IXRB · gzip MB |
| --- | ---: | ---: | ---: | ---: |
| buildings v2 (6 260) | 18.8 | 0.58 | 25.9 · 12.4 | 19.1 · 10.5 |
| buildings v2 fine (6 260) | 31.3 | 0.70 | 42.6 · 21.9 | 31.6 · 20.6 |
| terrain (2 224) | 18.9 | 0.39 | 25.5 · 9.5 | 19.0 · 7.4 |

Vegetation (1.12 M records, 200-byte JSON payloads): record files 416.9 MB
before, 251.3 MB after (0.66 of raw), bulk 4.6 s end to end (243 000/s).

## Binary payloads and `/records`

A batch with `payloadEncoding: "binary"` keeps its payloads as bytes
(zonelog `flagBinary`, `Record.Binary`); the JSON check is skipped. A read
gives the payload as the JSON string of its base64 with `"binary": true`, and
`/records?format=binary` sends the bytes themselves (IXRB,
[wire-block.md](../protocol/wire-block.md)). JSON stays the default. A write
may carry `Binary` with a base64 string. `/sets` answers IXB2 only.

## Three writers of IXP1

The part and manifest format has three implementations, kept equal by
`go run ./tools/prep verify <dir>` (which reads with the Go one):

| Implementation | Lines | Used by |
| --- | ---: | --- |
| `internal/prep` (Go, the reference: `part.go`, `batch.go`, `stream.go`) | ~1 000 | `tools/prep`, `localdata -bulk`, the node's reader |
| `tools/prep/lidar/ixp.py` (Python, numpy) | ~200 | the LiDAR and weather builders |
| `tools/prep/metal/main.swift`, section "the part file" (Swift, CRC-32 and SHA-256 by CryptoKit) | ~160 | `prep-metal` (macOS GPU) |

A later Rust prep tool (see the core scan, C.4 step 3) would replace the
Python and Swift writers with one; the Go reader stays the reference, and
shared vectors (a batch per writer, checked by `verify`) belong under
`core/testdata/vectors/` first.

## Not done

- A bulk commits its receipt in a second batch after the cut (`reconcileBulk` covers a crash
  between them): the receipt's byte counts are known only at the end.
- No request hash per row (pipeline.md 5.5.3): a later HTTP write of the
  same record is an ordinary write (same bytes, a new sequence).
- Splitting by owner reads names from `/ping` and the XOR rule; a node whose
  zones moved by a handoff refuses the part (421) and the tool stops.
