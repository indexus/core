# Batch ingest: `prep`, then hand the node sorted parts

Measured on 2026-11-14 on an Apple M1 Pro (8 CPU cores, 14-core GPU,
16 GB) on the whole Anglet scene.
Code: `internal/prep` (the file format, Go), `tools/prep` (the Go tool),
`tools/prep/lidar` (the vegetation points stage and the numpy records
stage, Python), `tools/prep/metal` (the records stage on the GPU, Swift and
Metal). The file the node ingests is section 5; the node side is
[../storage/bulk.md](../storage/bulk.md).

## In one paragraph

Before `prep`, a collection reached a node one HTTP write per record (`localdata
-write`, about 1 000 records/s: 18.6 minutes for the 1.12 M vegetation
voxels). Everything the node does with those writes (sort the keys, add the
sums, write sorted pages) can be done once, offline, in bulk, by the
publisher. `prep` reads a source in chunks, computes each item's key
(quantise, then interleave the bits of the Space), sorts by key, adds the
sums of each leaf record, gathers its payload, adds the sums of every zone
above, and writes **parts**: files of records already in the store's key
order, with checksums and a manifest. The node then installs a part as a
segment in one pass, without a memtable, a log or a merge.

## The result

Vegetation `AngVegetation001` from the raw LiDAR HD records of the scene
(3 195 files of 32 m, 212.9 M returns, 56.8 M of them in classes 3 to 5)
to 1 118 728 voxel records of 13 sums and their returns:

| | today | `prep`, numpy records | `prep`, Metal records | `prep`, Metal, from the cache |
|---|---:|---:|---:|---:|
| points stage | 40.2 s, one process | 9.4 s | 8.2 s | 2.0 to 2.3 s |
| records stage | 27.9 s (NDJSON) | 12.6 s | 3.2 s (1.8 s with binary payloads) | 1.9 s |
| **to a ready file** | **68 s** | **22.0 s** | **11.4 s** | **4.0 to 4.2 s** |
| into the node | + 1 119 s at 1 000 records/s | + one bulk install | + one bulk install | + one bulk install |
| file handed over | 798 MB NDJSON | 611 MB of parts + 10 MB of zones | the same | the same |
| peak memory | 352 MB | 3.5 GB (records), 0.8 GB per worker | 1.8 GB (records), 0.8 GB per worker | 1.8 GB |

- The bulk install is the node side ([../storage/bulk.md](../storage/bulk.md)), not measured here.
  Reading and checking every byte of the parts (`prep verify`: SHA-256,
  CRCs, key order, the 75 466 zone sums recomputed) takes 0.9 s: the order
  of magnitude of one pass over them.
- Today: 68 s + 18.6 min ≈ **19.8 min**. With `prep` on the GPU: 11.4 s to
  the parts, then the install; **4 s** to recompute from the cache of
  projected returns (after a change of the voxel, the sums or the payload).
- Same records: every key and every payload byte equal to today's builder run
  on the same Mac; every sum within 10⁻⁹ relative (one record, whose Σxy is
  1.8·10⁻⁸, is off by 2·10⁻¹⁵). Against the file of today,
  `vegetation-final.ndjson`, see section 4.4: it comes from another float
  library, and today's builder on this Mac differs from it exactly as `prep`
  does.
- Ground (`TerraLidHD2025v1`, through the intermediate `AngTerrain000001` tiles) and crowns
  (`AngVegCrowns0001`) are now built from the prepared returns: ground
  payloads identical to today's files (0.0 cm RMSE), crowns 50 060 of 50 062
  lines identical, the other two within 0.01; 12 to 13 s and 10 to 12 s.
- Today's NDJSON of every other collection goes through `prep ndjson` in
  0.08 to 2.1 s per file (7.3 s for the 798 MB of vegetation; section 4.5).

## 1. Bottom-up: what each source becomes

One record is a key (collection, location bits, depth, id), a weight, a few
float64 sums (IXR1) and an opaque payload. For every source of the Anglet
scene (`experiences/docs/collections.md` for the model):

| Source | Input (Anglet) | One record | Leaf depth | Records | What must be computed |
|---|---|---|---|---|---|
| LiDAR HD → vegetation | 212.9 M returns, 3 195 files, 30 B each: 6.39 GB as LAS records, 2.83 GB gzip (65 M returns/km² over the 3.3 km² of files); 56.8 M in classes 3-5 | one 2 m voxel of the 3D Space (east, north, up ±1024 m, 2 bits per axis per character): 13 sums `[n, Σx, Σy, Σz, Σxx … Σzz, Σr, Σg, Σb]`, payload 6 B per return | 5 (30 key bits) | 1 118 728, 51 returns each; 341 MB of returns | inflate, keep classes, project to the scene frame (float64), colour from the orthophoto, quantise to the voxel, interleave bits, sort, 13 sums per voxel, gather the returns, 75 466 zone sums (depths 0-4) |
| LiDAR HD → canopy (`CanopLidHD2025v1`, the target) | the same returns | a column of 2 m in `gps` (depth 8) and a slab in the ID; sums about the cell centre | 8 (48 key bits) | about the same | the same pipeline, 64-bit keys (a radix pass only where key bits vary: about 26 bits over the scene) |
| Buildings (BD TOPO + LiDAR) | 6 260 outlines | `0.<id>`: 14 sums + near mesh (2.5 kB); `1.<id>`: fine mesh (5.0 kB) | 16 (`gps`) | 6 260 × 2 | per building: outline moments, roof planes, meshes (CPU, geometry); then sort and write, trivial in size. Roof planes from LiDAR class 6 are the same primitive as the voxels: points → key (a roof cell) → sort → second moments per cell |
| Ground | LiDAR class 2 | a raster tile of 65 × 65 heights, 3 sums | tiles of quad level 19; levels 13-18 above | 2 224 (1 626 at level 19) | points → cell of quad level 25 → mean or minimum per cell (the same sort and segment, another reduction) → holes → tiles; the levels above are 2 × 2 reductions of the level below, as zones are |
| Orthophoto | IGN tiles | a WebP tile, 4 sums | tiles of quad level 18; 5-17 above | 588 | decode, resample, encode WebP (image codecs, CPU); the pyramid is 2 × 2 box filters |
| DVF | 546 278 sales | 5 metrics, no payload | 16 | 546 278 | parse, encode lat/lng (float64), sort |
| Weather | 4 554 to 67 808 cells per variable | `[area, v·area per hour]`, 73 sums | 16 | 67 808 (temperature) | parse, encode, sort |

**The minimal work** is the same for every source, and small for all but the
point clouds:

1. key = quantise + interleave the bits of the Space (Morton order with the
   collection's per-character offset), one per item;
2. sort the items by key;
3. one reduction per run of equal keys (the leaf record's sums) and a
   gather of its payload;
4. the zones above: each level is the reduction of the runs of the level
   below (sorted keys put every zone's records in one run);
5. write sorted columns and payloads, cut where the owners change.

**Bytes and operations for the vegetation**, and where the time goes
(measured, section 4):

| Step | Bytes | Work | Time |
|---|---|---|---|
| read + inflate | 2.83 GB → 6.39 GB | zlib, sequential per file | 18 to 20 CPU-s |
| keep classes 3-5 | 213 M → 56.8 M returns | one compare | (in project) |
| project L93 → scene | 56.8 M × 3 float64 | about 30 transcendental functions per return (6 iterations for the latitude) | 16 to 20 CPU-s |
| colour | one lookup per return in a 300 MB mosaic | the z18 tiles decoded once | 1.0 s + 6 to 8 CPU-s |
| quantise, offsets, chunks | 1.93 GB (34 B per return) | float64 floor, split | 6 to 10 CPU-s |
| keys + sort + runs + sums + gather | 56.8 M returns | GPU | **0.19 to 0.25 s** |
| records, zones, parts | 1.12 M records, 611 MB | CPU + GPU | 0.3 s + 0.9 to 1.9 s writing |

**The bottleneck is the decode of the raw format**, not the sort: inflate
and the float64 projection are 70 % of the CPU time; the GPU stages are
2 % of the wall time. This is why a cache of projected returns (1.5 GB) cuts
a recompute to 4 s, and why the next gain is in the source format (section
7), not in the GPU.

## 2. What exists, and what applies to an Apple GPU

- **Radix sort on GPUs.** The fastest GPU sorts are LSD radix sorts with
  8-bit digits. CUB's sort and its successor [Onesweep](https://arxiv.org/abs/2206.01784v1)
  (Adinets & Merrill, 2022) bin each digit with a prefix sum; Onesweep makes
  that sum single-pass (decoupled look-back), about 2n global memory
  operations per key and digit instead of about 3n, and sorts 29.4 G 32-bit
  keys/s on an A100. Decoupled look-back needs forward-progress guarantees
  between threadgroups that Metal does not document, so `prep` uses the
  classic reduce-then-scan form (three kernels per digit) with a stable
  in-tile ranking by SIMD-group ballots (the warp-level multi-split of the
  CUB/Onesweep family), and skips every digit where no key bit varies. On
  Apple GPUs, [forge-sort](https://docs.rs/crate/forge-sort/0.2.3/source/README.md)
  (Metal; MSD pass then three LSD passes) reports about 0.9 to 1.2 G 64-bit
  keys/s on an M4 Pro: the bar `prep`'s sort is measured against (4.2).
- **Runs and segmented reductions.** CUB's `DeviceRunLengthEncode` and
  `DeviceSegmentedReduce` ([CCCL docs](https://nvidia.github.io/cccl/cub/api/structcub_1_1DeviceRunLengthEncode.html))
  are the primitives of step 3: unique keys and run lengths of a sorted
  sequence, then a reduction per run. `prep` finds run heads per tile, scans
  the counts and reduces each run in one thread (runs are 51 returns on
  average). A hash group-by (cuDF's other path) would add the sums without a
  sort, but leaves the returns of a voxel in no fixed order: payloads must be
  deterministic, so the sort stays.
- **Morton codes and trees.** [Karras 2012](https://research.nvidia.com/publication/2012-06_maximizing-parallelism-construction-bvhs-octrees-and-k-d-trees)
  builds a whole radix tree (BVH, octree) in parallel from points sorted
  along a space-filling curve. Indexus does not need to build that tree:
  the tree is fixed by the alphabet, a zone is a key prefix, and sorted keys
  put every zone in one run, so the levels are plain reductions (step 4).
- **Out-of-core point clouds.** [Potree 2.0](https://cg.tuwien.ac.at/research/publications/2020/SCHUETZ-2020-MPC)
  (Schütz et al. 2020) splits a cloud with a hierarchical counting sort into
  chunks processed in parallel, and builds the coarser levels bottom up; it
  reports 6 to 9 M points/s end to end. `prep` takes the same shape (jobs of
  whole source files, bottom-up levels) with sums instead of subsamples.
  Entwine, PDAL and the 3D Tiles / I3S tilers follow the same chunk-then-tree
  plan.
- **LAS/LAZ decoding.** A LASzip chunk decodes sequentially; COPC
  ([description](https://lidarmag.com/?p=12309)) keeps a LAZ file as a
  clustered octree with a chunk table, so a reader fetches and decodes only
  the nodes it needs, and several at once. The Anglet raw records are gzip
  per 32 m file: the same property (one stream per file, decoded in
  parallel across files).
- **Precision.** Apple GPUs have no float64. Two f32 per value
  ("double-float", Dekker; [Thall 2006](https://hgpu.org/?p=1806) for GPUs,
  CCCL's [pairs of floats](https://nvidia.github.io/cccl/unstable/libcudacxx/extended_api/fp/fpmp.html))
  carry about 48 bits: `prep` adds the moments and the zone sums that way,
  measured at 10⁻¹³ relative.
- **What the Mac offers without installing anything.** Measured on 70 M
  keys (`bench1.py` in the run log): torch MPS sorts 22 M int64 keys/s
  (60 M int32) and `index_add_` adds 17 M rows of 13 f32/s; numpy's stable
  argsort does 1.2 M int64 keys/s. A hand-written Metal sort is 10 times
  torch MPS (4.2): Swift and Metal it is.

## 3. Design

```text
points stage (per source, CPU, 8 processes)     records stage (generic, GPU)                node
 raw file → inflate → keep → project (f64)  →   IXPT chunks → keys → radix sort        →    IXP1 parts
          → colour → cell + offset + payload     → runs → sums (double-float) + gather       + zones.ixp
          [cache of projected returns]           → records → one sort → zone levels          + manifest.json
                                                 → parts cut at owner zones
```

- **Two stages, split where the arithmetic changes.** The quantisation of a
  projected coordinate decides a key bit, and the projection is float64 with
  transcendental functions: to give the keys of the float64 reference, it
  runs on the CPU (the GPU has no float64). Everything after the integer
  cell is exact on the GPU: keys, sort, runs, payload gather, and sums in
  double-float. The points stage ends in **IXPT chunks** (section 5.7): per
  return its packed cell, its offset from the cell centre as two f32, its
  payload.
- **Parallel over files, no merge of partial voxels.** A job is whole source
  files (about 2 M returns), and voxels nest in the 32 m files, so no record
  is split between jobs. For a source where items straddle jobs, the sums are
  Abelian: each job emits partial records and equal keys add in the record
  sort (not built: Anglet does not need it).
- **Memory bounded by knobs, not by the data.** Workers hold one job each
  (`--chunk-points`, measured 0.7 to 0.8 GB per worker at 2 M returns per
  job; 8 workers); the GPU stage holds one batch of up to 8 M returns
  (650 MB of buffers) plus the records of the collection (0.5 GB here). For
  a département, the records would be written as sorted runs per batch and
  merged when the parts are written (section 7).
- **Idempotent, content-addressed.** The batch id is the SHA-256 of the
  inputs (raw source hash, orthophoto manifest hash, code hashes, options).
  No time, host or random order goes into a part: the same inputs give the
  same bytes. Measured: parts built from the raw files and from the cache
  are byte-identical, and so are parts built before and after changing the
  writer and the sort's scan.
- **Recompute.** `--cache DIR` keeps the projected returns (x, y, z float64
  and colour: 1.5 GB) under a key of the sources and the frame. A rerun reads
  them back and goes straight to cells: 4 s for the whole collection.
- **Progress.** Every stage writes one JSON line (`<out>.log.jsonl`):
  points, CPU seconds per step, records, seconds per GPU stage, peak memory.
- **Payloads as the node keeps them.** Today a payload is the JSON value of
  the NDJSON line (`storage.Record.Payload` is a `json.RawMessage`): for the
  vegetation, the string `"<base64>"`. `prep` writes that by default
  (`payloadEncoding: "json"`, a drop-in for today's readers), or the raw 6 B
  per return (`--payload binary`, 25 % smaller) once the node serves bytes.
- **Buildings and ground.** Ground and crowns are built from the prepared
  returns (sections 7.2, 7.3; the crowns segmentation on Metal). Roof
  planes: class-6 returns, key = the roof cell of quad level 25, the same
  moments; ground: class-2 returns, key = the cell, a minimum or a mean
  instead of a sum; raster pyramids: the zone levels with a 2 × 2 box filter.
  Only the points stage and the reduction change.

## 4. Measured

All on the M1 Pro, whole scene, `_run/.data/logs/ingest-run{B..F}.txt`.

### 4.1 Records stage, CPU against GPU (56.8 M returns → 1 118 728 records)

| Step | numpy, one core | Metal |
|---|---:|---:|
| read chunks (1.93 GB, page cache) | 0.36 to 0.39 s | 0.35 to 0.41 s |
| keys | 1.67 to 1.77 s | 0.03 to 0.04 s |
| sort (64-bit keys + 32-bit index) | 3.65 to 3.94 s | 0.07 to 0.10 s |
| runs + 13 sums | 3.0 to 3.3 s | 0.01 + 0.10 to 0.13 s |
| payload gather | 0.50 to 0.54 s | (with the sums) |
| records in float64, one sort of the records | 0.95 to 2.26 s | 0.23 + 0.01 s |
| zones (75 466, depths 0-4) | in the writer | 0.05 to 0.06 s |
| write the parts (611 MB with SHA-256 and CRCs; 495 MB binary) | 1.1 to 1.4 s | 0.85 s binary, 1.95 s JSON payloads |
| **GPU stages, returns per second** | about 6 M/s | **215 to 264 M/s** |

### 4.2 The sort alone (self test, random keys with 22 varying bits: 3 passes)

| Keys | GPU time | per kernel | keys/s |
|---|---:|---|---:|
| 8 M | 33 ms (53 ms with dispatch and the varying-bits check) | downsweep 31, upsweep 2.1, scan 0.3 ms | 240 M (150 M wall) |
| 56 M | 0.26 s (0.49 s wall) | downsweep 0.24, upsweep 0.011, scan 0.008 s | 215 M (113 M wall) |

The scatter (`downsweep`: 8 ballots per key per digit) is the whole cost;
the first version scanned the tile counts in one threadgroup (63 ms per 8 M
keys); a scan per digit row brought the scan to 0.3 ms. Sums in
double-float: 10⁻¹³ relative against float64 on 65 536 random values.

### 4.3 Points stage (8 workers)

| Step | CPU seconds |
|---|---:|
| inflate | 17.6 to 20.4 |
| project | 16.1 to 19.7 |
| colour (after a 0.9 to 1.0 s mosaic of the z18 tiles in shared memory) | 5.7 to 8.0 (20 when each process decoded its own tiles) |
| quantise, offsets | 5.7 to 9.9 |
| write chunks | 1.4 to 2.6 |
| **wall time** | **8.2 to 9.4 s** (2.0 to 2.3 s from the cache) |

### 4.4 Same records as today

| Compared | keys | payload bytes | sums within 10⁻⁹ relative |
|---|---|---|---|
| `prep` Metal / today's builder on the same Mac (numpy 1.26) | all equal | all equal | 1 118 727 of 1 118 728 (the last: 2·10⁻¹⁵ on a Σxy of 1.8·10⁻⁸) |
| `prep` numpy / `prep` Metal | all equal | all equal | all, with 10⁻¹² absolute |
| `vegetation-final.ndjson` / `prep` Metal | all equal | 7 of 340.6 M differ by one step of 7.8 mm | 1 118 251 (all but 385 with 10⁻⁸ absolute; worst 5·10⁻⁷) |
| `vegetation-final.ndjson` / today's builder on the same Mac | all equal | the same 7 | the same 477 |

`vegetation-final.ndjson` is not reproduced byte for byte by today's
builder on this Mac (numpy 1.26), nor with numpy 2.5.4 (the LiDAR venv),
nor on Linux with numpy 2.5.3 (1 099 of 4 174 lines equal on a 128 m
subset): the float64 transcendental functions differ in their last bit from
one library to the next, which moves a projected return by about 10⁻⁹ m.
Against the builder of the same machine, `prep` gives the same keys and the
same bytes.

### 4.5 Every other collection through `prep ndjson`

| File | Records | NDJSON | Time | Records/s |
|---|---:|---:|---:|---:|
| `dvf-2020.ndjson` | 546 278 | 95 MB | 2.1 s | 260 000 |
| `vegetation-crowns-1km.ndjson` | 50 062 | 9.7 MB | 0.21 s | 240 000 |
| `weather-temperature.ndjson` | 67 808 | 45 MB | 0.82 s | 83 000 |
| `vegetation-final.ndjson` | 1 118 728 | 798 MB | 7.3 s | 153 000 |
| `buildings.ndjson` | 6 260 | 15.8 MB | 0.15 s | 42 000 |
| `buildings-fine.ndjson` | 6 260 | 31.5 MB | 0.33 s | 19 000 |
| `terrain.ndjson` | 2 224 | 9.6 MB | 0.08 s | 29 000 |
| `ortho.ndjson` | 588 | 36.3 MB | 0.25 s | 2 400 |

The time is the JSON parse; sort and write are under 0.3 s. `prep cat` of
each batch gives back the NDJSON records with equal metrics and equal
payload values (checked record by record for the buildings, terrain and
orthophoto). `weather-ortho.ndjson` holds several collections in one file:
one batch per collection, so it must be split first.

## 5. The file the node ingests (IXP1)

### 5.1 A batch is a directory

```text
<batch>/manifest.json
<batch>/part-00000.ixp      records, sorted
<batch>/part-00001.ixp      ...
<batch>/zones.ixp           optional: the sums of every zone above the records
```

One batch = one collection. The directory name is the batch id (below). A
node ingests a batch whole or not at all: parts first, then one manifest
change (zonelog: one manifest rename) makes all of them visible.

### 5.2 manifest.json

```json
{
  "format": "indexus-prep-v1",
  "collection": "AngVegetation001",
  "metrics": 13,
  "metricNames": ["n", "sum_x", "..."],
  "records": 1118728,
  "payloadBytes": 456300000,
  "payloadEncoding": "json",
  "recordDepths": [5, 5],
  "splitDepth": 2,
  "parts": [
    {"file": "part-00000.ixp", "records": 81234, "bytes": 31000000,
     "first": "K3gUW/v", "last": "K3zzz/v", "sha256": "..."}
  ],
  "zones": {"file": "zones.ixp", "depths": [0, 4], "records": 23456, "sha256": "..."},
  "inputs": {"tool": "prep vegetation 1", "sources": {"<path>": "<sha256>"}, "options": {}},
  "id": "<hex sha256 of the canonical JSON of inputs>"
}
```

- `id` is a content address of what went in (tool and version, input file
  hashes, options). The same inputs give the same `id` and the same bytes in
  every part (no time, no host, no random order anywhere), so a batch that is
  run again is recognised and skipped, and a node that already holds a batch
  `id` can refuse it as a no-op.
- `first` and `last` are `location/id` of the first and last record of the
  part.
- `payloadEncoding`: `"json"` when a payload is the JSON value that today's
  NDJSON carries and the node stores (`storage.Record.Payload` is a
  `json.RawMessage`; for the vegetation the string `"<base64>"`), `"binary"`
  for raw bytes. A node that installs a `"json"` batch stores the payload
  bytes as they are, exactly what `localdata -write` would have stored.
- `splitDepth`: no part straddles a zone of that depth. A part holds one or
  more consecutive zones of `splitDepth` (grouped up to about 64 MB). With
  2 characters (4 096 zones) a node with a few vnodes gets whole parts; any
  finer owner range is cut inside a part (5.5).

### 5.3 A part file

Little-endian. A part is a header, blocks of up to 65 536 rows, an index of
the blocks and a footer. Within a block, columns follow each other, each
aligned to 8 bytes, the same columns a zonelog page has (minus seq and
request, which the node assigns).

```text
header  16 B   "IXP1" | u16 version = 1 | u16 m metrics | u32 flags (bit 0: rows carry payloads) | u32 0

block          u32 n rows | u32 id bytes | u64 payload bytes          16 B
               [n][13]u8   head: location bits (12 B, left aligned, as zonelog key bytes 2..13) | depth (1 B)
               pad to 8
               [n]u32      id end (exclusive, cumulative over the block)
               pad to 8
               [n]u64      payload end (exclusive, cumulative), only if flags bit 0
               [n]f64      weight (1 for a plain record, IXR1)
               [m][n]f64   metrics, one column per metric
               id bytes, pad to 8
               payload bytes, pad to 8
               u32 CRC-32 (IEEE, as zlib) of the block from its first byte | u32 0

index          per block: head of its first row (13 B) | 3 × 0 | u64 offset | u32 bytes | u32 rows   (32 B)
footer  32 B   u64 index offset | u64 total rows | u32 blocks | u32 CRC-32 of the index | "IXP1" | u32 0
```

CRC-32 rather than zonelog's CRC-32C: every producer has it at C speed
(Python `zlib.crc32`, Go `crc32.ChecksumIEEE`, a 1 KB table in Swift).

Rules a reader checks, and refuses the part when one fails:

1. Rows are **strictly increasing** by `(head, id)` compared as bytes, within
   a block, across the blocks of a part and across the parts in manifest
   order. This is the zonelog key order without the 2-byte collection
   number: `collection | location bits | depth | id`. No duplicate key.
2. Location bits are the 6-bit alphabet values of the location characters,
   most significant first (`decodeFixed` in sdk, `BASE64.Decode` in Go);
   bits past `6 × depth` are zero. Depth is 1 to 16 (0 is the root).
3. Every metric value is finite. `m` is the same for every row of a batch
   (the manifest's `metrics`).
4. Block CRCs, the index CRC, the footer magic and the manifest's `sha256`
   and `records` match.

Why columns and not the NDJSON line: a 13-metric vegetation record is
13 × 8 B of sums, 13 B of head and its payload; the NDJSON line is 713 B of
text. The parts of the vegetation collection are 611 MB with payloads kept
as today's JSON strings (495 MB with binary payloads) against 798 MB of
NDJSON, and a reader needs no JSON parser for keys and sums.

### 5.4 zones.ixp

Same file format, one row per zone that has records below it, for every
depth in `zones.depths`. The row's head is the zone (location bits and
depth), its id is empty, its `weight` is the sum of the weights below, and
its metrics are `m + 1` columns: column 0 is the number of records below,
columns 1..m the sums of the records' metrics. The sum of a zone includes
records at the zone's own location and every location under it, as zonelog's
`zoneRange` does.

A node does not need it: zonelog reads zone sums from its page index. It is
the check after an install (`Children(zone)` must equal these rows up to
float addition order) and a ready summary for a node that keeps sums in
memory.

### 5.5 What the node does with a batch

1. Read the manifest, refuse an unknown `format`, check every `sha256`.
2. For every row, the owner is the vnode closest to
   `ZoneKeyID(collection, location)` in the node's view. Rows are sorted by
   location bits, so the owners come in runs; a run that belongs to another
   node is forwarded as a byte range (or the part is sent whole when one
   owner holds it). Records of a depth `d` have ids whose first `6d` bits
   are their location bits: an owner boundary inside a part is one binary
   search on the heads.
3. For its own runs, write the rows into one new segment in order (zonelog
   `segWriter.add` takes rows in key order): key = collection number | head
   | id, weight, metrics, payload into the segment's payload file, seq from
   one contiguous range reserved for the batch, request hash computed as
   `localdata` computes its request id (so a later HTTP write of the same
   record is a retry and a no-op).
4. Install the segment with one manifest change (like `Ingest`), then
   compare `Children` of a few zones with `zones.ixp`.

Two modes. **fresh**: the key range of the batch must hold no live record of
the collection, the node refuses otherwise; this is the import of a new
collection or version (section 4.3 of `experiences/docs/collections.md`:
a new version is a new collection). **replace**: the node drops the
collection's range first (zonelog `Drop`), then installs. Mixing a batch with
live records of the same keys is not a bulk path: it is `Apply`.

### 5.6 Converting both ways

`go run ./tools/prep ndjson <in.ndjson> <batch dir>` turns any NDJSON of
today's builders (the `localdata -write` input) into a batch, and
`go run ./tools/prep cat <batch dir>` writes it back as NDJSON in key order,
so every builder can use the bulk path now, and every GPU output can be
compared record by record with the NDJSON of today.


### 5.7 IXPT: the chunk between the points stage and the records stage

Internal to `prep` (not for the node), little-endian:

```text
"IXPT" | u32 version 1 | u64 n | u32 payload width w | u32 bits per axis b | u64 0
[n]u32       cell: x << 2b | y << b | z (integers of the leaf grid)
[3][n]f32    offset from the cell centre, high part (x, y, z)
[3][n]f32    offset - high part: high + low is the float64 offset to about 2^-48
[n][w]u8     payload of each return
```

### 5.8 Binary payloads

A batch says `payloadEncoding: "binary"` when its payloads are raw bytes (the
6 B per return of the vegetation: 341 MB instead of 456 MB as JSON strings,
and 0.85 s instead of 1.6 s to write). The node keeps them as bytes (zonelog
row flag `flagBinary`) and serves them as they are on `/records?format=binary`
(IXRB); see [../storage/bulk.md](../storage/bulk.md). `vegetation.py
--payload binary` writes such batches. JSON stays the default.

## 6. Every batch Explore imports, in one command

`scripts/local/prep-all.sh` rebuilds `$DATA/batches/<collection>/` (one
batch per collection Explore reads, which `localdata -bulk` takes as a
directory; the crowns of section 7.3 are not built, Explore does not read
them). A batch whose
inputs did not change is skipped by its id; a second run with nothing
changed takes about 1 s.

| Collection | Built by | From |
|---|---|---|
| `AngVegetation001` | `tools/prep/lidar/vegetation.py` (GPU) | prepared returns (section 7) |
| `TerraLidHD2025v1` | `tools/prep/lidar/ground.py` (section 7.2) | prepared returns |
| `SceneExplo2026v1` | `tools/prep/scenes/scenes.py` | `experiences/scripts/world/catalogue.json` |
| `TempeAROME0128v2`, `RainfAROME0128v2`, `CloudAROME0128v2`, `WindUAROME0128v2`, `WindVAROME0128v2`, `SolarAROME0128v2`, `TownsAROME0128v2` | `tools/prep/weather/weather.py` | `$DATA/launch/weather-ortho.ndjson` (export of the former weather content) |
| `DvFMV2020idx0001` | `prep ndjson` | `$DATA/collections/dvf-2020.ndjson` (France) |
| `OrthoBDORT2024v1` | `prep ndjson` | `$DATA/collections/ortho.ndjson` (`experiences/scripts/world/ortho.py`) |
| `BuildLidHD2025v2`, `BuildLidHD2025f2` | `prep ndjson` | `$DATA/collections/buildings-v2.ndjson`, `buildings-v2-fine.ndjson` (`experiences/scripts/world/buildings`) |

Measured on the Mac for the whole scene, cold: about 45 s from nothing (21 s
of it the prepared returns, built once), 1.6 GB of batches.

- `prep ndjson -out DIR file...` makes one batch per collection. A
  collection found in several files is one batch: two batches of one
  collection would overlap in the node's key space (the node wants a
  batch's zone empty), so it is imported once and replaced as a whole when
  one of its files changes.
- Of two versions of a key the later stays, as writes would keep it. An
  index of the inputs (size, time, SHA-256, collections) avoids hashing and
  reading an unchanged file again.

`scripts/local/explore.sh` runs `prep-all.sh`, then imports every batch
with `localdata -bulk` (vegetation last, Explore already open), and keeps
each batch id in `$DATA/imported/<collection>`: an unchanged batch is
skipped, a changed one is sent with `-replace`.

## 7. Prepared returns: decode once

`tools/prep/lidar/returns.py` decodes the raw LiDAR HD of the scene once:
one IXRT file per 32 m tile under `$DATA/prep/lidar/<key>/`, every class,
returns grouped (ground, vegetation, building, other) in file order, x and y
float64 in the scene frame clipped to the tile, Z as the source integer (z =
Z × 0.01 − z0 is computed by the reader, the same float64 operation),
orthophoto colour, intensity, return bits and class: 27 B per return. The
key is a hash of the raw source hash, the recipe, the orthophoto manifest
and the format version. Every later build of a LiDAR collection reads only
its group, in one read per tile.

| | Value |
|---|---:|
| prepared returns, whole scene | 212.9 M returns, 3 195 files, **5.75 GB** (2.83 GB gzip of the source) |
| time to build them, once | 21 s with the default 5 processes (17 s with 8; inflate 16, projection 55, colour 17 CPU-s) |
| vegetation from them, then GPU | **5.3 to 5.4 s** (points 2.2 s, records 3.1 s) |
| same parts | byte for byte the parts built from the raw files |

Why float64 and not a quantised integer: the prepared returns give the same
keys and the same payload bytes as today's builder, and they stay the
reference for every collection built from them. An int32 at 0.1 mm per
axis would take 18 B per return (3.8 GB) but move about 2 800 returns to a
neighbouring voxel.

### 7.1 Reading them (stable)

```python
import sys; sys.path.insert(0, "$CORE/tools/prep/lidar")
import returns
d, manifest = returns.find("$DATA/prep")          # newest complete set: $DATA/prep/lidar/<key>/
for t in manifest["tiles"]:                       # file, bounds, count, classes per 32 m tile
    if t["classes"].get("6"):
        b = returns.read_tile(d / t["file"], "building")
        x, y = b["x"], b["y"]                     # float64 scene metres, clipped to the tile
        z = b["Z"] * 0.01 - b["z0"]               # float64, the scene datum
```

`read_tile(path, group)` is the stable interface (the IXRT layout of the
`returns.py` docstring, header version 1, sets of `returns.VERSION` "2"; a
change of either is a new key, so a new directory, never a rewrite). Groups:
`"ground"` (class 2), `"vegetation"` (3 to 5), `"building"` (6), `"other"`.
Fields per return, in source file order: `x`, `y` (float64), `Z` (int32,
source cm), `rgb` (uint8 × 3, orthophoto, 90 where no tile), `intensity`
(uint16), `bits` (return number | number of returns << 4), `class`; per tile
`bounds` and `z0`. A builder of building points reads the `"building"` group;
for the ground it reads `"ground"` or, already reduced,
`$DATA/prep/work/ground/corners-q.npy` (uint16, height = q × 0.01 − 100 at
the 0.5 m corners of the scene, the grid of the intermediate `AngTerrain000001` tiles)
with `corners-known.npy` (bool, measured before the fill). Tested on a
hand-written tile in `test_vegetation.py`.

### 7.2 Ground: `ground.py`

`TerraLidHD2025v1` and, as its intermediate, the `AngTerrain000001` tiles
(32 m tiles of 65 × 65 corner heights every 0.5 m, and 256 m tiles every
4 m; written but not imported) (raster tiles of the gps
Space, levels 13 to 19) from the ground group, with the arithmetic of
`buildings/{extract,terrain}.py` and `world/ground.py`:

1. per tile, 8 processes: the class-2 height sum and count of every 0.5 m
   cell (a bincount of 4 096 bins, float32 sums and uint16 counts as
   `extract.py`). Memory-bound and beside the read: CPU, not Metal (a GPU
   dispatch per tile would cost more than the bincount);
2. holes filled by push-pull (2 × 2 weighted pyramid, then three smoothing
   passes on the hole cells only: the same bits as `terrain.py`'s fill, a
   fraction of its memory);
3. corners, then tiles: fine depth 2, coarse depth 3; `TerraLidHD2025v1`
   reads the lattice in blocks of 256 rows and halves it level by level.

Compared with today's files (`launch/terrain-1km.ndjson`,
`collections/terrain.ndjson`): same 3 251 and 2 224 keys, **every payload
identical** (height RMSE 0.0 cm, metrics equal; the tolerance of
`docs/collections.md` is 3 cm). 12 to 13 s on the Mac (cells 6 s).

### 7.3 Crowns: `crowns.py`

`AngVegCrowns0001`, one record per tree plus one small crown per 8 m cell
of the low vegetation, in the layout of today's file and with the
arithmetic of `build_crowns.py`: canopy height model at 0.5 m (vegetation
height above the ground corners of 7.2, maximum per cell, 8 processes),
pits filled, smoothing, then the segmentation, then the 19 metrics as sums
per crown (partial sums of strips of tiles added, so they aggregate), then
the NDJSON of today's builder for `prep ndjson`.

The segmentation is where Metal pays: `prep-metal --peaks` (local maxima,
one thread per cell) and `--grow` (region growing, one dispatch per step,
8 steps per command buffer, 169 steps here) take **1.2 s against 9.3 s** in
numpy, same labels. `--metal` is set by `prep-all.sh`; without it the numpy
code of `build_crowns.py` runs.

Compared with `collections/vegetation-crowns-1km.ndjson`: 50 062 records,
same keys, 50 060 lines byte-identical; the two others differ by 0.01 in
one metric (a rounding at the last printed digit). Root sums equal.
10.4 to 12.3 s on the Mac.

## 8. Memory

No flag needed: `vegetation.py` takes a budget of min(2 GiB, RAM / 8) and
sets from it the returns per job (measured about 500 B per return in a
worker) and the GPU batch (72 B per return). The peak of the whole process
tree (resident memory of the driver and every descendant, sampled every
0.2 s; shared memory is counted in every process that maps it, so this is
an upper bound) is logged per phase. The ground and crowns rows were
measured with the code as committed, in the Linux VM of the Mac (4 cores,
budget forced to that of the 16 GB Mac, numpy segment), the others on the
Mac:

| Phase | Peak, all processes |
|---|---:|
| prepared returns, once (5 workers from the budget; 8 workers: 2.2 GB) | 1.66 GB |
| points (8 workers, 268 k returns per job) | 1.6 to 1.8 GB |
| records (prep-metal, 3 M returns per GPU batch) | 1.5 to 1.6 GB |
| ground: cells / legacy tiles / terra tiles | 0.29 / 1.06 / 0.89 GB (2.08 GB before the hole-only fill) |
| crowns: ortho + CHM / segment / stats / write | 1.31 / 0.79 / 1.27 / 0.60 GB (stats 4.1 GB with 8 workers and 500 k returns per batch: now workers = budget / 500 MB, 200 k) |
| `prep ndjson`, largest group (the former content collection, 820 MB of NDJSON) | 0.34 GB (1.9 GB when it held the payloads: it now reads them back from the files when writing) |

The records stage holds the payloads of the whole collection (341 MB here)
until the parts are written; for a département that is the sorted runs of
section 10.

## 9. Running it

```sh
go run ./tools/prep ndjson today.ndjson out/batch          # any collection of today
go run ./tools/prep verify out/batch
go run ./tools/prep compare -abs 1e-8 batch-a batch-b
go run ./tools/prep cat out/batch > back.ndjson            # NDJSON again, key order

xcrun swiftc -O tools/prep/metal/main.swift -o prep-metal && cp tools/prep/metal/prep.metal .
python3 tools/prep/lidar/vegetation.py <experiences> out space-tables.json \
        --backend metal --metal ./prep-metal [--cache cache/] [--payload binary]
python3 -m unittest discover -s tools/prep/lidar           # keys = the SDK's Space, sums = brute force
go test ./internal/prep                                     # format, order, zones, ZoneKeyID bits
```

## 10. What is left

1. **The decode is the bottleneck** of the first build: 36 of 50 CPU-s are
   inflate and the float64 projection of a raw format chosen for the
   browser. The prepared returns (section 7) pay it once; a GPU projection
   with a float64 fallback for returns within 10⁻⁶ m of a
   cell boundary (filtered predicates) would remove the projection from the
   CPU but not the inflate.
2. **The sort's scatter** is 4 times slower per key than forge-sort's on an
   M4 Pro: fewer ballots (4-bit sub-digits or `simd_ballot` on match masks),
   or an MSD first pass. Not worth it while the GPU is 2 % of the time.
3. **Collections larger than memory**: write each batch's records as a
   sorted run and merge the runs when writing parts; split parts per owner
   range at write time when the view is known.
4. **Buildings and orthophoto from the prepared returns**: buildings
   still come from the NDJSON of `experiences/scripts/world/buildings`
   (`collections/buildings-v2*`) through `prep ndjson`; the `"building"`
   group of 7.1 is their input.
5. **The gps canopy** (`CanopLidHD2025v1`): 64-bit keys work as they are;
   the moments about the cell centre are what the engine already sums.
6. **Where the Python lives.** `tools/prep` stays in core: the Go tool, the
   Metal engine, `tools/prep/lidar/{returns,vegetation,ixp}.py`. A builder
   of `experiences` either writes NDJSON and lets `prep ndjson` make the
   batch, or imports `ixp.py` (`sys.path.insert(0, "$CORE/tools/prep/lidar")`)
   to write a batch directly, or starts from the prepared returns with
   `returns.read_tile(path, "ground")`.
7. **Ground and crowns on the GPU beyond the segmentation**: the cell
   bincount of 7.2 and the CHM maximum are memory-bound and stay on the
   CPU; at a département scale a GPU min/sum over sorted cells (the sort of
   section 3) would replace them.


## Une commune entière (`tools/prep/commune`)

Le disque de 1 km tient dans le cube de 2 048 m de la scène ; une commune
(Anglet : 27 km², 4,7 x 8,8 km) non. `scripts/local/prep-commune.sh` la
découpe en pavés de 2 048 m (le carré de la scène décalé de 2 048 m * (i, j) :
le code de ce document y travaille tel quel, recette `frameShift`) et en
quarts de 1 024 m, l'unité de travail : retours COPC du quart (boîtes de
256 m, 64 m au bord du contour, reprise par boîte), retours préparés,
végétation (vegetation.py `--levels-above 2`), sommes du sol, toits ; puis
le brut et les retours préparés sont effacés. La végétation de la commune
est un lot `VoxelLidHD2025v1` dans un cube de 32 768 m et 7 caractères dont
les cellules de profondeur 2 sont les pavés : clé = préfixe du pavé | clé du
pavé >> 12, moments décalés (vegbatch.py) ; `prep verify` le vérifie. Le sol
(TerraLidHD2025v2) se fait par pavé et 384 m de marge (niveaux 19 à 16), puis
15 à 13 d'un réseau assemblé ; l'orthophoto (OrthoBDORT2024v2) de tuiles z18
coupées au contour ; les bâtiments (BuildLidHD2025v3/f3) par pavé
(experiences buildings.py). Disque au pic : environ 11 Go pour Anglet.
