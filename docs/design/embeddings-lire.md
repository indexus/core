# Embeddings and LIRE (SPFresh) on Indexus

**Statut : proposé.** Conception (centroïdes, SPFresh/LIRE) ; rien n'est branché.

Status: study, measured on public sets, and plan, 2026-11-19. Simulation: `scripts/research/lire/main.go`
(standalone, standard library only).

## 1. The question

SPFresh keeps a vector index fresh with postings (clusters of a few hundred
vectors around a centroid) and LIRE: a posting that grows splits by 2-means,
vectors near the split move to the posting whose centroid is now nearest,
a posting that shrinks merges into a neighbour. Indexus zones already have
what a posting needs: a count and sum columns (centroid = sum / count),
splits and folds driven by capacity, one writer per zone, and convergent
summaries. Two ways to place vectors in zones:

- **Fixed space**: the encoding of the description as it is. A projection of
  the vector to k axes (random, or principal axes), bisection of fixed
  bounds, `c` bits per character.
- **Routing by centroids** (LIRE): a space without axes (opaque addresses,
  already in IXC1 v1); the writer sends a vector to the zone of the nearest
  centroid; splits, reassignment and merges as in LIRE.

## 2. Measurements

### 2.1 Public sets (the decisive test)

Every base vector inserted in file order, one delete of a random live vector
every 5 inserts after the first quarter, then 500 queries; recall@10 against
brute force over the live vectors. `ivf` is the static reference (k-means over
the final live vectors with as many postings as LIRE ends with: FAISS
IVFFlat built once, its best case, no freshness).

**OpenAI `text-embedding-3-small`, 1,536 dimensions** (Hugging Face
`Qdrant/dbpedia-entities-openai3-text-embedding-3-small-1536-100K`, 75,000
vectors, 500 held out as queries; 63,326 live at the end):

| nprobe | LIRE (stream) | IVF static | fixed space |
| --- | --- | --- | --- |
| 4 | 0.734 at 1.2 % | 0.764 at 1.2 % | 0.342 at 0.8 % |
| 16 | 0.895 at 4.7 % | 0.903 at 4.8 % | 0.632 at 3.0 % |
| 32 | 0.942 at 9.4 % | 0.943 at 9.3 % | 0.769 at 6.0 % |
| 64 | 0.973 at 18.7 % | 0.976 at 18.3 % | 0.878 at 11.8 % |

**SIFT1M**, 128 dimensions (first 200,000 base vectors, 500 of the official
queries; 170,001 live):

| nprobe | LIRE (stream) | IVF static | fixed space |
| --- | --- | --- | --- |
| 8 | 0.730 at 0.9 % | 0.770 at 1.0 % | 0.462 at 0.5 % |
| 16 | 0.867 at 1.8 % | 0.892 at 2.0 % | 0.628 at 1.1 % |
| 32 | 0.952 at 3.6 % | 0.961 at 3.8 % | 0.767 at 2.1 % |
| 64 | 0.989 at 7.2 % | 0.991 at 7.5 % | 0.888 at 4.1 % |

(percentages: vectors read per query, of the live vectors.)

- **LIRE maintained in a stream matches a static k-means index built over
  the final data**: within 1 point on the OpenAI embeddings from nprobe 16,
  within 1 to 3 points on SIFT, at the same number of vectors read. The
  criterion set before the test (within 1.5x of the reference) is met by a
  wide margin.
- **The fixed encoding needs about 2 to 2.5 times more reads** for the same
  recall (OpenAI: 0.88 at 11.8 % against 0.90 at 4.7 %; SIFT: 0.89 at 4.1 %
  against 0.87 at 1.8 %).
- Maintenance: 0.82 (OpenAI) and 0.86 (SIFT) vectors moved per insert.
- Boundary replication (a vector also written to its second posting when
  d2 <= (1 + eps)^2 d1) gains nothing at equal reads on the OpenAI set:

  | eps | copies per vector | recall@10, nprobe 16 | nprobe 32 |
  | --- | --- | --- | --- |
  | none | 1.00 | 0.895 | 0.942 |
  | 0.01 | 1.21 | 0.895 | 0.938 |
  | 0.03 | 1.53 | 0.892 | 0.935 |
  | 0.15 | 1.99 | 0.893 | 0.934 |

  It costs space and moves; v1 goes without it.
- Caveat: inserts and searches rank all postings exactly; in Indexus they
  descend a tree of centroids (64 per level), a little less exact.

### 2.2 One million vectors (AWS c7i.8xlarge, 2026-11-19)

Same stream (1 M inserts, about 150,000 deletes, about 850,000 live), 1,000
queries. `lire-tree` is LIRE as the node would run it: a tree of centroids
(sum / count of each zone, at most 64 children), writes descend it; the
search ranks every posting centroid (column "ranked") or descends the tree
best-first (column "descent").

**SIFT1M** (128 dimensions), vectors read per query in percent of the live:

| Index | ≈ 0.7 % | ≈ 1.4 % |
| --- | --- | --- |
| LIRE flat (writes compare every posting) | 0.893 (0.72 %) | 0.954 (1.43 %) |
| LIRE tree, ranked | 0.866 (0.72 %) | 0.942 (1.44 %) |
| LIRE tree, descent | 0.759 (0.72 %) | 0.870 (1.44 %) |
| IVF static | 0.896 (0.85 %) | 0.957 (1.66 %) |
| fixed space | 0.773 (0.82 %) | 0.874 (1.61 %) |

**OpenAI `text-embedding-ada-002`** (1,536 dimensions, the other column of
the 3072-1M set):

| Index | ≈ 0.7 % | ≈ 1.4 % |
| --- | --- | --- |
| LIRE tree, ranked | 0.874 (0.68 %) | 0.924 (1.35 %) |
| LIRE tree, descent | 0.787 (0.68 %) | 0.863 (1.35 %) |
| IVF static | 0.901 (0.76 %) | 0.942 (1.48 %) |
| fixed space | 0.690 (0.90 %) | 0.802 (1.76 %) |

**OpenAI `text-embedding-3-large`** (3,072 dimensions, 1 M):

| Index | ≈ 0.7 % | ≈ 1.4 % |
| --- | --- | --- |
| LIRE tree, ranked | 0.805 (0.68 %) | 0.862 (1.35 %) |
| LIRE tree, descent | 0.729 (0.68 %) | 0.820 (1.35 %) |
| IVF static | 0.844 (0.75 %) | 0.888 (1.47 %) |
| fixed space | 0.574 (0.92 %) | 0.702 (1.83 %) |

Build of 1 M vectors in 3,072 dimensions: 4 min 50 s for LIRE (85 centroid
comparisons per insert), 13 min 24 s for the static k-means.

- At one million vectors LIRE stays close to the static reference: equal when
  writes see every posting, 2 to 4 points under it through the tree (the gap
  grows a little with the dimension: 3 points at 1,536, 4 at 3,072).
- The tree makes writes 13 times faster (SIFT: 18 s against 3 min 54 s for
  1 M inserts; 83 centroid comparisons per insert, depth 3).
- **The tree descent at search time is the weak point**: 8 to 11 points under
  ranking every posting, for the same vectors read. Inner centroids (means of
  whole subtrees) guide poorly. Ranking every posting centroid is cheap for
  the node (5,000 centroids) but heavy for a client in high dimension
  (5,000 × 1,536 floats = 31 MB); to try: a wider beam on the first levels,
  posting centroids served by the parent zones (they hold them as child
  sums), product-quantised centroids for the client.
- The fixed space is 10 to 20 points under at equal reads.

### 2.3 Search through the tree, and the Indexus addressing rule (AWS, step 3)

Recall@10 at nprobe 32 (about 0.7 % of the vectors read) and centroids
compared per query. "Ranked" compares the query with every posting centroid
(about 4,600 to 5,000); "beam" keeps the `nprobe` nearest zones at each level
and opens them; "best-first" pops one zone at a time.

| Tree (1 M vectors) | ranked | beam | best-first |
| --- | --- | --- | --- |
| SIFT1M, fan-out 64, restructures upward | 0.866 · 4,628 | 0.866 · 1,581 | 0.759 · 189 |
| SIFT1M, fan-out 64, grows down only | 0.870 · 4,099 | 0.868 · 2,036 | 0.775 · 202 |
| SIFT1M, fan-out 16, grows down only | 0.856 · 4,214 | 0.850 · 746 | — |
| 3-large 1,536, fan-out 64, restructures upward | 0.817 · 5,043 | 0.817 · 1,494 | 0.766 · 272 |
| 3-large 1,536, fan-out 64, grows down only | 0.837 · 4,122 | 0.836 · 2,059 | 0.809 · 289 |
| 3-large 1,536, fan-out 16, grows down only | 0.824 · 4,239 | 0.821 · 785 | 0.784 · 170 |

- **Growing down only (the rule Indexus addresses need) costs nothing**; it
  is even slightly better on the text embeddings (0.837 against 0.817).
  Only the vectors of the splitting posting ever move.
- **The beam descent gives the quality of ranking every posting.** With a
  beam equal to nprobe, the loss is under 1 point.
- **Fan-out 16 compares 5 to 6 times fewer centroids** (746 to 785 against
  4,100 to 5,000) for 1 to 2 points less. The cost of a search then grows
  with the depth of the tree, beam × fan-out × depth, not with the number of
  postings: about 32 × 16 × 6 = 3,000 centroids for 10^9 vectors (5 million
  postings, 6 levels of 16), against 5 million to rank them all.
- Best-first descent is 5 to 11 points under: not to be used.

### 2.4 Ten million vectors, 1,024 dimensions (AWS r7i.8xlarge, step 4)

Cohere `wikipedia-2023-11-embed-multilingual-v3` (English files 0000–0100):
10 M inserts with 15 % deletes (8.5 M live), and the same 1,000 queries from
file 0100 at 1 M and 10 M. Capacity 256 per posting; recall@10 against the
exact neighbours of the live set. Ranking every posting unless noted; the
beam (beam = nprobe) is within 0.5 point of it. Log:
`_work/datasets/result-aws-cohere.txt`. One instance, 58 min, ≈ 2.45 $.

| | 1 M (850 k live) | 10 M (8.5 M live) |
| --- | --- | --- |
| Exact neighbours (32 vCPU) | 42 s | 7 min 23 |
| Postings, tree 16 / 64 | 5,313 / 5,274 | 51,879 / 50,633 |
| Depth, tree 16 / 64 | 5 / 3 | 8 / 5 |
| Centroids compared per insert, 16 / 64 | 44 / 106 | 58 / 137 |
| Build, tree 16 / 64 (static IVF) | 1 min 05 / 1 min 38 (5 min 53) | 13 min 19 / 19 min 50 |
| Vectors moved per insert | 1.10 | 1.02–1.07 |
| Recall, ~11,000 read: tree 16 / tree 64 / IVF | 0.902 / 0.918 / 0.931 | 0.800 / 0.819 / — |
| Recall, ~22,500 read: tree 16 / tree 64 / IVF | — / 0.955 / 0.963 | 0.868 / 0.883 / — |

- **Same gap to static IVF as on OpenAI** at 1 M: 1.3 points for fan-out 64,
  2.9 for fan-out 16, with a build 3.6 to 5.4 times faster and no training.
  Cohere is harder than OpenAI 3-large for every index alike.
- **Scale ×10 costs ×2 in reads, not ×10.** For the recall of 1 M at 11,000
  vectors read (0.90–0.92), 10 M needs a little more than 22,500 (0.87–0.88
  there): the read grows like the number of postings near the query, not like
  the base. The measured points stop at nprobe 128; the next run should go to
  256–512 to place 0.90 and 0.95 exactly.
- **Insert cost grows with the depth only**: 44 → 58 centroids per insert
  (fan-out 16) for ten times the vectors, and build time ×12 for ×10 data.
- **Vectors moved per insert stay at about one** (split plus LIRE
  reassignment), at both sizes: the write amplification does not grow.
- Not measured at 10 M: static IVF (its training grows with the number of
  postings times the training sample, about 100 times the 6 minutes of 1 M).
  The comparison to IVF rests on the 1 M point.

### 2.5 Synthetic clusters (first study)

Stream of 60,000 inserts and 9,000 deletes, 64 dimensions, 1,000 clusters
then 500 new ones halfway (the distribution moves), cluster spread 1.0 per
dimension (clusters overlap), zone capacity 256, 400 queries, recall@10
against brute force. Search ranks zones by their centroid distance and scans
the `nprobe` nearest.

| Placement | zones read (nprobe) | vectors read per query | recall@10 |
| --- | --- | --- | --- |
| LIRE | 8 | 1,552 (3.0 %) | 0.58 |
| LIRE | 16 | 3,093 (6.1 %) | 0.68 |
| LIRE | 32 | 6,179 (12.1 %) | 0.80 |
| LIRE | 64 | 12,314 (24.1 %) | 0.90 |
| fixed, 8 principal axes, c = 1 | 32 | 4,832 (9.5 %) | 0.57 |
| fixed, 8 principal axes, c = 1 | 64 | 9,661 (18.9 %) | 0.74 |
| fixed, 16 principal axes, c = 2 | 64 | 7,891 (15.5 %) | 0.71 |
| fixed, 16 random axes, c = 2 | 64 | 8,179 (16.0 %) | 0.69 |
| fixed, 8 principal axes, c = 6 | 64 | 4,486 (8.8 %) | 0.60 |

- **LIRE reads 2.5 to 3 times fewer vectors for the same recall** (0.57:
  1,552 against 4,832; 0.70: about 3,500 against about 8,000). With
  separated clusters (spread 0.45) LIRE is at 0.99 from one zone, the fixed
  space at 0.27.
- **Reassignment** adds little here (0.684 against 0.672 at nprobe 16);
  splits by 2-means do most of the work.
- **Stale centroids**: writers that route with a copy refreshed every 1,000
  or 5,000 inserts lose nothing measurable (0.696 and 0.668 at nprobe 16).
  A client may cache the routing level.
- **Maintenance**: 0.6 to 0.9 vectors moved per insert for LIRE, 0.6 to 5.6
  for the fixed space (its splits move every vector of the zone, and c = 1
  splits most often).

## 3. More characters, larger bases, a larger space?

- **Depth is not the limit.** A tree of zones of capacity C over n vectors
  uses about log2(n / C) bits: 8 for this test, 22 for 10^9 vectors. The
  address has 96 (16 characters of 6 bits). With 16 or 32 principal axes the
  results are identical to the last digit: bits beyond the depth reached are
  never cut. More characters change nothing for search.
- **What the first bits cut is what matters.** Principal axes beat random
  ones slightly (0.71 against 0.69); a fixed bisection still cuts at the
  middle of bounds fixed in advance, not where the data is, and cannot follow
  a distribution that moves. A data-dependent partition (LIRE) is the gain.
- **The base sets how a split looks.** c = 6 splits a zone into 64 children
  at once: many tiny zones (median 4 vectors), more zones to rank per query.
  c = 1 or 2 splits finer. For routing by centroids, 2^c is the fan-out of the
  routing tree: 64 children per zone is a good fan-out (a level compares 64
  centroids), and 4 characters already address 64^4 = 16.7 M postings, about
  4·10^9 vectors at 256 per posting. **Base 64 and 16 characters are enough;
  the space to change is not the address but what a zone holds.**
- **The space of an embedding collection** is then: opaque addresses (no
  axes), the schema `sum × d` plus `sum` of ‖x‖² (variance, split decisions),
  the count. Optional: a fixed space of a few principal axes as a coarse first
  level is not needed (the routing tree is its own coarse level).

## 4. Work left, in order

### 4.1 Width (storage and wire)

| Item | Today | Needed |
| --- | --- | --- |
| Metrics per record | done (2026-11-20): zonelog IXR2 values and wide pages (ZLS4), `MaxMetrics` 4,096; narrow rows unchanged byte for byte | — |
| Records on the wire | done: IXRB v3 (u16 count) only above 255 metrics; golden shared with the SDK | — |
| Zone sums on the wire | done: IXB2 kind 4 "wide rows" (u32 count, flag bit 3), `columns=` up to 4,096; reference.py golden | — |
| Record precision | done: a zonelog page of 64 columns or more whose every value is a float32 exactly stores them on 4 bytes (byte 11 of the page header, segment ZLS4): 0.53 of the bytes for 128-dim vectors, values and sums unchanged; any other page keeps f64 | — |
| Zone value on the wire | JSON floats in contributions, IXB2 f32/f64 columns | 768 floats = 6 KiB f64 per zone; a `/sets` read of 64 children 390 KiB: f32 by default for embeddings, per-column projection (ask only the count and centroid columns needed) |
| Typed fold | reads every record of a zone per read | not used here (all sums): embeddings stay on the zonelog sums path, O(1) per zone |

### 4.2 Routing by centroids (the LIRE protocol on Indexus)

1. **Placement (write).** The writer descends from the root: at each zone it
   reads the 64 children's sums (one `/sets` read, cached), picks the nearest
   centroid, goes down until a zone without children, writes the record
   there. Address = the chosen characters. Stale caches are acceptable
   (measured above); a write to a zone that has split since is forwarded by
   its owner to the nearest child (one hop, the owner has the fresh sums).
2. **Split (owner of an oversized zone).** 2-means on its records (it holds
   them), then the two halves become two children (a character each), records
   move by delete and insert under the new addresses. One writer per zone:
   no race on the split itself. The children's sums are pushed as today.
3. **Reassignment.** After a split, the owner asks the owners of the nearby
   postings (nearest centroids among its siblings and its parent's siblings)
   to move the vectors now nearer to a new centroid: each owner moves its own
   records (delete here, insert there). Bounded: a few neighbours, as LIRE.
4. **Merge.** A zone under a floor (capacity / 8) folds: its records move to
   the nearest sibling, its address is retired.
5. **Search (client).** Descend by centroids with a beam (keep the `b` best
   zones per level), then scan the `nprobe` nearest postings' records and rank
   exactly. The sums are already summaries: no extra index.
6. **Consistency.** Moves are delete + insert, versioned per zone as every
   write; sums converge (CRDT); a reader may see a vector twice or not at all
   for the time of a move: the search keeps the nearest copy (ids are unique).

The node needs one new piece: the **operation that moves records between
zones on behalf of a split** (an owner-local batch of deletes and inserts,
idempotent by operation id). Everything else is the existing write path.

### 4.3 Measurements still to make

- Done: SIFT1M, OpenAI 1 M (1,536 and 3,072), Cohere 1 M and 10 M (1,024),
  beam descent over the tree (sections 2.2 to 2.4).
- 10 M with nprobe up to 512, to place recall 0.90 and 0.95.
- Throughput on AWS (registered budget), once 4.1 and 4.2 are in.
