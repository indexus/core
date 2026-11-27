# zonelog: the node's store

**Statut : actuel.** Les lectures MVCC (snapshots) et la carte des verrous sont dans [stockage-et-verrous.md](../architecture/stockage-et-verrous.md).

`internal/zonelog` is the only storage engine of the node. A store is one
zonelog directory, `<path>.records`: record rows and the control state
(meta, authority and fences, zone sequences, receipts, history, staging,
counters, as rows of a reserved collection) commit
together, one frame and one fsync per write group. Zone sums are
compensated (Neumaier pairs, format `zonelog-2`), payload files are
compressed, and a record carries up to 128 metrics
(`storage.MaxMetrics`). Bulk batches are installed as segments
([bulk.md](bulk.md)). Control rows are paths (`internal/storage/tx.go`):
the bucket names bbolt kept a row in and its key, escaped.

No earlier format is read. Nothing ran in production before zonelog, so
there is no migration: a file at the store's path (a store of the former
bbolt engine) fails the open with a message, a segment whose footer is not
"ZLS3" fails it too, and the node module requires no module outside the
standard library (`go.mod` has no `require`; the HTTP/3 edge is the
separate `gateway/` module).

This page says what the engine is, what it measured when it replaced bbolt
(2026-11-14), and its limits. The executed plan of that change is in
[../archive/storage-interface-plan.md](../archive/storage-interface-plan.md).

## Above a ceiling

- **Code size.** 2 442 lines without tests (2 024 of code), against an aim
  of 1 600, before reading indexes and filters through the cache, which
  added about 215 lines of code to `segment.go`. Where they go is in the
  last section.
- **Disk under overwrites.** Records that are only inserted take 1.01 to
  1.23 times their raw bytes. Records that are replaced often take more
  between merges: 1.5 times the live bytes with 3 KB payloads each replaced
  20 times, and 2.1 times with 300 B payloads, where the receipts of the
  replaced writes (about 35 B each, kept 24 hours; no longer written) weigh
  as much as the rest. Measured in a small test (400 records), not in the benchmark.

- **Children of the largest zones of a large bulk segment.** At 10 and
  15 M records, with each collection imported as one segment, the warm
  children sums of the vegetation root and of a depth-1 zone take 0.8 to
  1.4 ms at p50 and up to 4 ms at p99 (ceiling 1 ms): they add up hundreds
  of index entries per child. Unchanged by reading the indexes through the
  cache (section "At 2, 10 and 15 M records").

Everything else measured is under its ceiling (table below).

## Design

```text
log        files of frames. A frame is one batch of mutations:
           u32 bytes | u32 CRC-32C | first seq | seqs | count | (request hash, key, IXR1 value)...
           whole or absent after a crash. One fsync covers every frame
           appended while the previous fsync ran.
memtable   a skiplist of the rows not yet in a segment. A row is visible to
           readers only once its frame is synced.
segments   immutable NNN.seg, sorted by key: pages of 64 to 256 rows as
           columns (key heads, flags, seq, request hash, weight, one column
           per metric, ids), then the page index and a filter of the keys.
           Open keeps, per 64 pages, the first key, the sums and where their
           index entries are; entries (by 64) and filter (by 4 KB) are read
           through the page cache and checked by CRC.
payloads   NNN.val, written with segment NNN. A merge copies payloads under
           4 KB into its own file and keeps pointing at the larger ones.
           A segment ends with its tail (page index with sums, filter,
           payload files, counts) in little-endian binary, footer "ZLS3".
manifest   format, identity of the disk, segments, collections. Replaced by
           rename, then a directory sync.
```

**Key.** `collection number (2 B) | location bits (12 B) | depth (1 B) | id`.
Sorted as bytes, everything at or below a zone is one range, and a zone and
its first child (same bits) are kept apart by the depth byte (ZONE-ID-1).
One store per node; a vnode is a set of key ranges in it.

**Write.** `Apply(records)`. Under the write lock each record is compared
with the stored version of its key; the rows go to the memtable and the
frame to the log. Then one fsync, shared with the other callers, and only
then do readers see the rows and the caller its acknowledgement. Apply
returns only an error: `internal/storage` reads the records it replaces
itself, in its transaction. Its control state is records of a reserved
collection in the same frame as the data: one atomic cut.

**Background.** A full memtable becomes a segment; four neighbouring
segments of one size class merge; all merge when replaced versions and
tombstones outnumber half the live records; a segment that points into a
payload file more than a third dead is rewritten to copy out of it. A writer
waits when the previous memtable is still being written. No timer and no
goroutine exist while nothing is written.

**Unchanged writes.** Equal bytes are a no-op: the write takes no seq, as
is the delete of a missing record. Retries of a request are
`internal/storage`'s (its receipts, control rows): zonelog keeps no request
ids. The request hash of each row is still written, always 0; receipt rows
of earlier releases are in no read and merges drop them.

**Zone sums are read, not maintained.** Every row adds to the sums of its
range. A write over a live version that sits in an older segment also stores
a *retraction* row (the old weight and metrics negated, tagged with the seq
of the version it cancels), so the sum of a range is the plain sum of its
rows in every segment and the memtable. A merge drops a retraction together
with the version it cancels. The index keeps the sums of each page, and of
each run of 64 pages; a page ends early where keys cross a zone boundary, so
the children of a large zone start on page boundaries. `Children(zone)` is
65 neighbouring ranges, answered from the index plus at most a few pages.
It also gives the key rule of query.md: for each child, the one location
that holds all its records, or none when they are at several. That is read
from the first and last key of the range, which are in the index too.

**Memory.** One number, `MemoryBytes` (256 MB; `storage.memory_bytes` in
node.json): a sixteenth for each of the two memtables, an eighth for the
page cache. The cache holds pages, index entries, filter chunks and payload
frames, least recently used out first: what is read often stays, the rest
is read from the disk (or the kernel's page cache) again. What stays in
memory whatever is read is about 0.03 B per record (per 64 pages a key and
the sums, per 4 KB of filter a CRC, per 64 KB of payload 24 B): 0.4 MB at
15 M records. A segment being written holds its filter, 1.25 B per row of
that segment (18 MB for a bulk of 14 M records), until it is synced; its
index entries go to a spill file `NNN.idx.tmp` past 1 MB. About a quarter
of `MemoryBytes` is alive; the rest is for what is in flight and for the
garbage collector. No `GOMEMLIMIT` is needed to stay under it.

**Can this replace the summary radix and the pushes?** For zones whose
records are on this disk, yes: count, weight, metric sums and the key of
every child come from the store, exact after every acknowledged write, with
nothing to fold, repair or rebuild at startup. Contributions of children
held by another vnode stay as they are.

## Measured

`scripts/research/storebench.sh`, all four stores run again on 2026-11-14 with the
finished store. Same generated records for every store, one process per
store, real fsync, no `GOMEMLIMIT`; 2 vCPU Xeon 2.1 GHz, 8 GB, ext4 on a
virtual disk. 1 000 000 vegetation records (depth-5 locations, 13 metrics,
200 B payload), 20 000 with 20 to 60 KB payloads, 500 000 DVF-like (depth
16, clustered, 5 metrics, no payload). Raw record = IXR1 value plus flat
key: 339 B, 39 907 B and 74 B.

`bbolt` and `no-history` are `storage.OpenWithOptions` with group commit and
`Apply`, `OmitHistory` off and on, measured while storage still ran on bbolt
(today `-store storage` and `storage-nohistory` run it on zonelog).
`flat-mock` is bbolt with the four flat buckets and binary values of
[ixr1.md](../protocol/ixr1.md), data path only: a floor for that design, not
a store. Its code (`compare/flat_test.go`) left with bbolt; it is at commit
74937c0.

| | bbolt today | no-history | flat-mock | zonelog | zonelog before step 2 | ceiling |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| inserts/s, vegetation, 256 callers | 5 551 | 8 348 | 6 914 | **73 764** | 54 074 | ≥ 20 000 |
| inserts/s, DVF, 256 callers | 6 284 | 8 492 | 8 246 | **80 308** | 61 222 | |
| inserts/s, 20–60 KB payloads | 2 003 | 4 534 | 2 409 | **4 817** | 2 134 | |
| inserts/s, one call of 256 | – | – | – | 151 527 | 86 860 | ≥ 20 000 |
| inserts/s, one by one (fsync each) | 284 | 285 | 336 | **6 038** | 6 261 | |
| disk, all records, MB | 5 147 | 3 309 | 1 976 | **1 233** | 1 222 | |
| disk / raw, all records | 4.56 | 2.93 | 1.75 | **1.09** | 1.08 | ≤ 1.5 |
| vegetation: B/record (× raw) | 2 799 (8.3) | 1 945 (5.7) | 1 011 (3.0) | **371 (1.09)** | 352 (1.04) | ≤ 1.5 |
| large: B/record (× raw) | 86 393 (2.2) | 47 598 (1.2) | 48 396 (1.2) | **40 125 (1.01)** | 40 104 (1.00) | ≤ 1.5 |
| DVF: B/record (× raw) | 2 035 (27.5) | 1 579 (21.3) | 665 (9.0) | **91 (1.23)** | 87 (1.18) | ≤ 1.5 |
| RSS after load, MB | 3 557 | 2 526 | 1 852 | **92** | 244 | ≤ 256 |
| RSS peak, MB | 3 557 | 2 527 | 1 854 | **203** | 257 (411 without `GOMEMLIMIT`) | ≤ 256 |
| open after kill | **1 ms** | 2 ms | 1 ms | 139 ms | 803 ms | |
| open after kill, s per GB | 0.0003 | 0.0006 | 0.0007 | 0.11 | 0.66 | < 2 |
| CPU over 60 s idle, ms | 0.3 | 0.3 | 0.4 | 1.5 | 0.4 | < 2 % of a core |
| children sums, zone depth 0, p50 / p99 ms | 10 467 | 11 026 | 101 / 121 | **0.15 / 0.28** | 0.07 / 0.15 | < 1 |
| children sums, zone depth 1, p50 / p99 ms | 161 / 198 | 160 / 248 | 1.5 / 3.4 | **0.09 / 0.32** | 0.06 / 0.23 | < 1 |
| children sums, zone depth 2, p50 / p99 ms | 2.5 / 5.1 | 2.5 / 4.5 | 0.02 / 0.07 | 0.05 / 0.08 | 0.03 / 0.11 | < 1 |
| children sums, zone depth 3, p50 / p99 ms | 0.05 / 0.11 | 0.05 / 0.12 | 0.001 / 0.009 | 0.01 / 0.04 | 0.01 / 0.02 | < 1 |
| children sums, zone depth 4, p50 / p99 ms | 0.02 / 0.04 | 0.02 / 0.04 | 0.001 / 0.001 | 0.006 / 0.02 | 0.006 / 0.02 | < 1 |
| one zone, 64 records, p50 / p99 ms | 1.8 / 3.4 | 1.8 / 2.9 | 0.003 / 0.01 | 0.02 / 0.14 | 0.02 / 0.06 | |

How to read it:

- What step 2 changed in the numbers. Large payloads are written twice
  (log, payload file) and no longer by every merge: 2.3 times faster. A
  smaller memory share, no table of request ids and a cheaper tail made
  small records faster too. Each row holds 8 more bytes (the request hash):
  the ratios rose by 0.01 to 0.05. `Children` now also returns the
  one-location rule, and the page cache is half what it was: p50 up to
  twice the earlier figure, still under a third of the ceiling. Open no
  longer rebuilds a table of request ids from the log.
- Per-dataset bytes come from loading that dataset alone at a fifth of the
  size. For zonelog they are segments and payload files; the log adds 64 to
  85 MB whatever the data (it is inside the 1 233 MB and the 1.09).
- Children sums on bbolt are a storage scan (`CausalInventoryPage`, metrics
  only), the path of a relay or of `refresh=true`. A node answers `/sets`
  from its in-memory summary instead. One sample at the root for the two
  bbolt stores.
- bbolt's RSS is the mapped file: the kernel can take it back, but it is
  what the user saw as memory and swap. Run to run it moved between 2.2 and
  3.6 GB for the same data.
- Idle CPU is the Go runtime in every case: 1.5 ms in 60 s is 0.003 % of a
  core.
- bbolt's own figures moved by up to 30 % against the first run of this
  table (4 319 to 5 551 inserts/s on vegetation): one run each, on a shared
  virtual machine. The order of the stores did not change.

With three times the records (4.56 M, 3.6 GB on disk): RSS 83 MB after load
and 219 MB at peak, 64 000 inserts/s on vegetation and 5 200/s on large
payloads, children sums p99 ≤ 0.95 ms (the page cache is then 20 MB, the
indexes 12 MB), open after kill 0.18 s, disk 1.06 × raw.

Package benchmarks: `go test -bench . ./internal/zonelog`.

### At 2, 10 and 15 M records (2026-11-14)

`TestScale` in `internal/zonelog/scale_test.go` (its comment says how to
run it, one process per phase). The Anglet collections in proportion: 95 %
vegetation (one record per depth-5 voxel, clustered, 13 metrics, a 100 B
payload: a third of the real one, for the disk of the benchmark machine;
payloads go to payload files, not to pages, indexes or filters), 5 %
DVF-like (depth 16, 5 metrics), one in 5 000 an orthophoto tile (20 to
60 KB). Each collection is one bulk segment, then 50 000 writes in batches
of 256 make two more segments. Real fsync, `MemoryBytes` 256 MB, same
machine as above; *before* is the store with resident indexes (9cb3a20),
*after* this one. The segments are byte for byte the same.

| | 2 M before | 2 M after | 10 M before | 10 M after | 15 M before | 15 M after |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| disk, MB | 543 | 543 | 2 623 | 2 623 | 3 929 | 3 929 |
| resident indexes and filters, MB | 5.8 | 0.05 | 27.9 | 0.24 | 44.3 | 0.39 |
| Go heap after open, MB | 6.6 | 0.9 | 29.1 | 1.3 | 45.2 | 1.8 |
| RSS after open, MB | 12.5 | 6.5 | 36.5 | 6.8 | 52.0 | 7.3 |
| page cache after warm reads, MB | 26.1 | 31.9 | 3.9 (floor) | 31.8 | 3.9 (floor) | 31.6 |
| RSS after warm reads, MB | 42.0 | 43.8 | 41.0 | 44.6 | 61.2 | 44.3 |
| RSS peak, bulk import, MB | 44.8 | 25.8 | 132 | 44.8 | 227 | 59 |
| RSS peak, import and writes, MB | 79 | 89 | 132 | 112 | 227 | 122 |
| bulk import, s | 3.8 | 3.9 | 17.5 | 16.0 | 27.8 | 23.8 |
| writes/s after the import | 108 317 | 92 405 | 99 602 | 95 742 | 90 232 | 92 267 |
| open, ms | 37 | 44 | 85 | 69 | 117 | 105 |

Reads at 15 M, p50 / p99 in ms. Warm: three passes after one that loads
the cache. Cold: before each read the store's cache is emptied and its
files are dropped from the kernel's page cache (`POSIX_FADV_DONTNEED`), so
every page, index block and filter chunk comes from the virtual disk.

| 15 M records | warm before | warm after | cold before | cold after |
| --- | ---: | ---: | ---: | ---: |
| vegetation children of the root | 1.28 / 2.56 | 1.37 / 4.14 | 4.8 / 10.0 | 11.5 / 20.0 |
| children of a depth-1 zone | 0.75 / 1.58 | 1.16 / 3.71 | 2.1 / 6.7 | 3.4 / 5.0 |
| children of a depth-2 zone | 0.61 / 1.30 | 0.66 / 1.48 | 1.1 / 5.1 | 1.4 / 2.3 |
| children of a depth-3 zone | 0.09 / 0.38 | 0.09 / 0.23 | 0.31 / 0.62 | 0.54 / 1.31 |
| children of a depth-4 zone | 0.06 / 0.18 | 0.02 / 0.05 | 0.30 / 0.68 | 0.43 / 2.24 |
| DVF children of a place | 0.02 / 0.12 | 0.03 / 0.15 | 0.09 / 0.33 | 0.31 / 0.86 * |
| vegetation zone of depth 3, ≤ 64 records with payloads | 0.61 / 1.99 | 0.43 / 1.83 | 0.97 / 2.35 | 1.49 / 4.55 |
| DVF place, ≤ 64 records | 0.04 / 0.09 | 0.02 / 0.08 | 0.11 / 0.26 | 0.39 / 0.87 |
| one vegetation record (`Get`) | 0.42 / 0.97 | **0.02 / 0.07** | 0.67 / 1.16 | 1.09 / 1.80 |
| one orthophoto tile (`Get`) | 0.03 / 0.08 | 0.03 / 0.10 | 0.26 / 0.41 | 0.28 / 0.73 |

\* one read of 86 ms in that run, a stall of the virtual disk (other runs
had one such read in 1 000, in any column); p99 of a second run on the
same data.

How to read it:

- Memory no longer grows with the records. Before, the indexes took 2.9 B
  per record out of the cache's 32 MB: from 10 M the cache was at its floor
  of 4 MB and a warm `Get` read the disk (0.42 ms). After, the cache keeps
  its 32 MB at any size and holds what the reads use (0.02 ms).
- The peak of an import drops from 227 to 59 MB at 15 M: a bulk writer
  spills its index entries, and opening the new segment no longer decodes
  its whole tail.
- Cold reads cost an index block (about 20 KB) or a filter chunk (4 KB) on
  top of the pages they read before: up to twice the time when nothing is
  in any cache, 11.5 ms for the root of 14 M voxels. The kernel keeps the
  files it read in its own page cache, outside the process and given back
  under pressure, so a read after a restart is between the two columns.
- Warm children of large zones (root, depth 1 and 2) take about a
  millisecond before and after: they add up hundreds of index entries per
  child in a 14 M-row bulk segment. Run to run, those figures move by 20 %
  (a second run after the change on the same data: 1.20 / 2.79 ms at the
  root, 0.99 / 2.09 at depth 1).
- At 2 and 10 M the reads are the same shape (results of all three sizes
  are printed by the test); at 10 M before, a warm `Get` was already
  0.41 ms, after 0.02 ms. With `MemoryBytes` 512 MB at 15 M: the same
  latencies (these reads fit in 32 MB), RSS 80 MB after warm reads.

## Crash safety

Every file operation goes through an `FS`. The test one (`memfs_test.go`) is
a disk that loses power: a file keeps only what a sync made durable, plus,
at random, part of what was written since, ending in garbage; a creation, a
rename or a removal is undone unless the directory was synced after it.

- `TestPowerLossKeepsAcknowledgedBatches`: the power goes off at 60 random
  disk operations (append, flush, merge, manifest rename, log rotation,
  open), and twice more after each recovery. Every reopen holds a whole
  number of batches, all those acknowledged, equal to a brute-force model
  (records, scans, zone sums, one-location rule). Removing any one of four
  syncs from the code (log write, new log file, segment, manifest rename)
  makes it fail.
- `TestKillDuringWritesKeepsAcknowledgedBatches`: a writer process with real
  fsync, killed at 12 random moments.
- `TestTornLogKeepsAcknowledgedBatches`: the log cut at 40 random bytes.

Not covered: a disk whose fsync lies, bit rot in synced data (a page or a
frame with a bad CRC is an error, there is no repair), a full disk.

## What the authority and handoff layers need

`internal/storage` keeps authority, fences, receipts, history and staged
copies as rows of a reserved collection (`internal/storage/tx.go`), and
moves zones with them: zonelog
itself has no tail, mirror, seal, ingest or drop.

| Need | zonelog | Test |
| --- | --- | --- |
| Control state committed with data | records of a reserved collection in the same frame | `TestUnchangedWritesAndControlRecords` |
| Format refusal | `Format` in the manifest (its `ID` is kept as written; the disk's identity is `internal/storage`'s `storage-id`) | `TestIdentityAndFormat` |
| Bulk install of a zone | `BeginBulk`: one segment, the manifest rename is the cut | `TestPowerLossDuringBulkIsAllOrNothing` |
| No dirty read | rows above the synced seq are skipped by every read | `TestWriteIsInvisibleUntilSynced` |

## Risks and limits

- **A young engine.** The disk model of the power-loss test is a model.
- **One background worker.** A long merge delays the next flush, and writers
  wait once two memtables are full. Throughput includes these waits; the
  latency of a single write during them was not measured.
- **Overwrites.** See the first section. A payload file is kept while a
  segment points at two thirds of it.
- **Open files.** One per payload file. With 40 KB payloads that is one per
  16 MB written: 6 000 for 100 GB. Above that they must be opened on demand.
- **Memory.** Indexes and filters are read through the cache (2.9 B per
  record, 44 MB at 15 M, no longer resident). What still grows: 0.03 B per
  record resident, and while a segment is written its filter (1.25 B per
  row of that segment). When the reads' working set (with writes: the
  filters of every segment, 1.25 B per record) is larger than an eighth of
  `MemoryBytes`, reads go to the disk more often; `MemoryBytes` is the knob.
- **Cold reads.** A read whose index block or filter chunks are not cached
  reads them first: one more disk read per segment it touches (up to six
  4 KB chunks for a `Get` that finds its key).
- **Seqs are local to a disk.** A record gets a new seq when its zone moves.
- **Float sums.** A sum is exact in which rows it counts; adding the same
  float64 values in another order can differ in the last bit, as in query.md.
- **Merges need free disk**: up to the size of the segments being merged,
  without their large payloads.

## Where the lines go

`wc -l` of the files without tests, for their current size. The handoff
calls (`Since`, `Mirror`, `Seal`, `Ingest`, `Drop`), request receipts and
`IfSeq` were removed in November 2026: `internal/storage` never called them.

| Part | What |
| --- | --- |
| `fs.go` | the file interface and its directory implementation (the power-loss test needs it) |
| `key.go` | location bits, keys, zone ranges |
| `record.go` | record, row, sums, IXR1 value |
| `memtable.go` | skiplist, versions readers may see |
| `segment.go` | page layout, page cache, index and filter read through it, range sums, segment writer and its spill |
| `log.go` | frames, recovery, group commit and visibility |
| `store.go` | open and manifest, payload files, `Apply`, flush and merge, stats |
| `read.go` | get, scan, children, zone totals and the one-location rule |
| `bulk.go` | bulk segments |
