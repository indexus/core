# Search benchmark campaign

Measure how many **correct, sufficiently fresh results per second** a client receives and what it costs to serve them. Run the local pilot first. The published turbopuffer figures below are comparison targets. The pilot uses small HTTP fixtures and an in-memory bitmap, so its timings have a different scope.

## Reproduce the local pilot

From the core checkout, with Python 3, Node, the Go toolchain/dependencies cached and a prepared Indexus SDK checkout compatible with these tests:

```sh
python3 scripts/perf/search-pilot.py /path/to/sdk-js /tmp/indexus-search-run-01
```

The output directory must be new. The pilot runs two real HTTP peers on loopback, writes its fixture into disposable storage, and uses no cloud credentials. It saves revisions, working-tree status, runtimes, architecture, commands and raw measurements. A missing or skipped HTTP scenario fails the run.

| Output | What it exercises |
|---|---|
| `two-peers.jsonl` | Five Go/JS oracle scenarios: BM25, dense/sparse search and RRF with direct reads versus discovery from roots, filtered kNN, array membership, range aggregates, and selective subtree refresh through edit/delete/ABA. The fixtures log their RPCs or answers. Go test durations include setup and process startup. |
| `bitmaps.tap` | SDK bitset and presence tests, including deletions, signed word boundaries, incompatible snapshots, reordered projections and unknown masks. |
| `bitmap-count.json` | 100,000 synthetic ordinals, 991 deletions, two postings and a plain-set oracle. Records build time and five batches of 100 count calls in a single client process. The first batch includes JIT warmup. |
| `fingerprint.txt` | Three local Go benchmark repetitions each with the subtree result cache evicted or retained. 512 persisted rows, elapsed ns/op, allocations and underlying keys/op. OS and storage caches can be warm in both cases. |

The bitmap pilot can also run separately at 1 M ordinals. For dense vector scale, use the SDK’s SIFT driver on a local dataset. Record the SDK revision for each run.

```sh
node scripts/perf/search-bitmaps.mjs /path/to/sdk-js 1000000
(cd /path/to/sdk-js && node scripts/perf/sift-session.mjs /path/to/sift 1000000)
```

A local SIFT1M replay with SDK `b41bb6c` selected 62,500 of one million rows.
Summed over 64 queries, prefiltering plus exact search took 3.86 s.
Prefiltering, building the smaller IVF and querying 16 cells took 2.69 s
with recall@10 of 99.53%. The global IVF and the smaller IVF returned
identical hits at 16 probes. This was one run on a shared Apple M1 Pro
with the data already in memory at the start of each strategy.
[Raw measurements and input hashes](sift-session-1m-20261128.json).

## Upstream reference workload

The official nightly archive for **2026-11-12** has one namespace, 10 M documents, top 10, target 8 requests/s and a measured duration of 600 s. The table uses client-side combined observations, including warm-up classifications when they occur in the measurement window. [Nightly workflow](https://github.com/turbopuffer/tpuf-benchmark/blob/7789dc5fac4ad55dbc8169ab759a09485febec1a/.github/workflows/nightly.yml) · [Website profiles](https://github.com/turbopuffer/tpuf-benchmark/tree/7789dc5fac4ad55dbc8169ab759a09485febec1a/benchmarks/website)

| Workload | Queries | Achieved req/s | p50 / p90 / p99, ms | Publisher’s result |
|---|---:|---:|---|---|
| ANN hot | 4,799 | 8.00 | 17 / 21 / 49 | [JSON][vh] |
| ANN cache disabled | 1,979 | 3.32 | 2,169 / 3,784 / 5,343 | [JSON][vc] |
| BM25 hot | 4,799 | 8.00 | 26 / 131 / 489 | [JSON][fh] |
| BM25 cache disabled | 4,796 | 8.01 | 372 / 461 / 655 | [JSON][fc] |
| Hybrid hot | 4,797 | 8.00 | 193 / 765 / 1,368 | [JSON][hh] |

ANN uses cosine distance over 1,024-dimensional Cohere Wikipedia embeddings and pseudorandom query vectors. BM25 uses MS MARCO text and queries. Hybrid uses MS MARCO query text and embeddings, two top-10 branches, RRF and eventual consistency. The hot profiles wait for indexing and a 100% cache-hit rate. The cold profiles disable the service cache on every request. These archives contain latency and throughput; recall and judged relevance must be measured separately. [Vector profile](https://github.com/turbopuffer/tpuf-benchmark/blob/7789dc5fac4ad55dbc8169ab759a09485febec1a/benchmarks/website/vector-10m-hot.toml) · [Hybrid profile](https://github.com/turbopuffer/tpuf-benchmark/blob/7789dc5fac4ad55dbc8169ab759a09485febec1a/benchmarks/website/hybrid-10m-hot.toml)

The homepage publishes a different hot ANN p90 (17 ms). Use the dated archive to compare runs. The v3/v2 table reports relative p90s between two turbopuffer versions, including group-by and filtered sorting. The 100-billion-vector experiment reports approximately 200 ms p99 with fan-out across shards; it is a separate architecture experiment. For a third vector engine, Milvus can be measured using VectorDBBench at the same corpus, metric and recall target. [Homepage](https://turbopuffer.com/) · [v3/v2](https://turbopuffer.com/v3) · [100B ANN](https://turbopuffer.com/blog/ann-v3) · [VectorDBBench](https://github.com/zilliztech/VectorDBBench)

## Campaign matrix

| Workload | Corpora and scale | Validation and distinguishing measurements |
|---|---|---|
| BM25 and text | MS MARCO v2.1 at 100 k, 1 M and 10 M; separate fixtures for prefix, phrase, typo and Unicode offsets | Freeze tokenizer, query list and field weights. Use qrels matched to the selected document IDs, report judged-query coverage and MRR@10/nDCG@10. Keep exact-token oracle and bounded-fuzzy completion separate. |
| Dense vectors | SIFT 100 k / 1 M at 128D L2; Cohere Wikipedia 100 k / 1 M / 10 M at 1,024D cosine | Compute exhaustive blocked search over the same vectors after ingestion precision conversion. Freeze tie handling and top 10. Record recall vs latency at several probe budgets (e.g. target recall 0.90, 0.95, 0.99). Hold out real domain queries in a separate series from pseudorandom upstream queries. |
| Sparse and hybrid | Same MS MARCO IDs and embeddings across backends, first 100 k then 1 M / 10 M | Check sparse dot products against a direct sparse oracle; fuse fixed ANN/BM25 candidate depths against a reference RRF with identical ties and constants. Record nDCG, bytes, RPCs and separate per-branch/fusion time. |
| Filters, Bloom, bitmap | Independent and correlated categories at 0.1%, 1%, 10%, 50%, 100% observed selectivity; 100 k, 1 M, 10 M | Scan live IDs for exact AND/OR/NOT/IN and filtered top-k. Vary Bloom bits/value and bitset density; count true/false positives, false negatives, postings built, bytes allocated, bytes on disk and reads skipped. Include deleted, missing, null and repeated values. Unsafe omissions invalidate the trial. |
| Aggregation, sort, pagination | Anglet building attributes for field semantics; deterministic synthetic low/high cardinality with 100 k, 1 M, 10 M rows | Compare counts, groups, aggregates, ordering and pages to a full scan/sort with stable tie-breakers. Verify histogram rank/value bounds against exact quantiles. Track build/merge costs, peak working memory and over-the-wire bytes. |
| Distributed discovery and refresh | Identical corpus on 1/2/8 physical peers; direct known addresses and discovery from roots; 0%, 0.1%, 1%, 10%, 100% of subtrees mutated | Compare both paths to a fresh exhaustive stable-view oracle. Vary branching, skew, budgets and concurrency. Include no-op refresh, add, edit, delete, update/undo, partial ownership, handoff, restart, offline peer and budget unknown. Count distinct probes, data RPCs, bytes and changed leaves. Record coverage, unknowns, stale answers and time to converge after acknowledged writes. |

### Adaptive sessions on the same observed selection

Run 1, 4, 16 and 64 interactions per session. Disclose the next filter, facet
or ranking only after the preceding response. Compare Indexus with sequential
turbopuffer requests and with one materialization reused on the client. For
queries known together, also run a multi-query baseline. Charge both engines
for the initial selection, index construction, subsequent reads and client
memory. Plot cumulative wall time and recall after each interaction.

For Nearby, report the first page, a ready page and a page requiring new
branches separately, with page sizes 1, 16 and 64. Record branch reads,
sorts, retained rows and time to first useful page; repeat with a slow client
and cancellation. For change detection, mutate payloads while preserving
counts, vary the fraction of changed branches, preview changes, then reload a
subset. Record the cost after unrelated writes as well as on an idle disk.
For multi-vector search, compare exact MaxSim scores to token-by-token dot
products, varying token count per document and candidate depth.

Use original dataset releases and archive the selected IDs, model, dtype, normalized vectors, query vectors/text, qrels, filter attributes, mutations and seed with checksums. Corpus size and vector dimension alone cannot reproduce an experiment. [Wikipedia vectors](https://huggingface.co/datasets/Cohere/wikipedia-2023-11-embed-multilingual-v3) · [MS MARCO vectors](https://huggingface.co/datasets/Cohere/msmarco-v2.1-embed-english-v3)

## Execution, cost and interpretation

1. Start with the local correctness fixtures, then 100 k and 1 M. Advance to the official 10 M profile after checking result quality and the machine’s RAM/disk budget. A 10 M × 1,024 f32 matrix alone occupies 40.96 GB before IDs, text, indices or replication. At 100 M/1 B use a separately budgeted sharded test and document the changed hardware and routing.
2. Fix engine version, index parameters, index-ready barrier, client/servers and region, storage medium, CPU and RAM limits, consistency, returned attributes, K, query distribution and concurrency. Publish native mode separately when modes differ. A fresh process, cold local disk cache and explicit service cache bypass are distinct experiments. Account for Indexus client CPU, RAM and transfer costs, including discovery and reranking.
3. Replay a fixed open-loop schedule, first 8 req/s for 600 s, then double offered rate until saturation. Record planned arrival, dispatch, completion, timeout, error, completeness, result count and quality per request. Publish offered, achieved and correct-complete req/s, queueing, p50/p90/p95/p99, and sample counts. At the initial target only about 48 requests fall in the upper 1% per run. Repeat independently at least three times and report per-run values and variation.
4. Time ingest, index construction, readiness, reads and updates separately. Measure resident/peak RAM, CPU-seconds, disk space, I/O and network by client/server, storage requests and rebuild/compaction. Report cost per million successful requests meeting the same quality/consistency target, and storage per million documents per month. Itemize hardware or billed rates, date, utilization, idle time and egress. Mark inaccessible metrics as unknown. This pilot provisions no machines and provides no price projection.
5. Retain raw samples, manifest, oracle checks and anomalous responses. Treat incomplete traversal as an observed outcome. For filtered recall, use min(K, eligible documents), report zero-eligible queries separately and disclose dropped/late queries. Compare performance only for matched protocol and semantic contracts.

[vh]: https://raw.githubusercontent.com/turbopuffer/tpuf-benchmark/070f9ef998c3470b25945766b2dac8b8667ba931/build/2026-11-12/website/vector-10m-hot.json
[vc]: https://raw.githubusercontent.com/turbopuffer/tpuf-benchmark/070f9ef998c3470b25945766b2dac8b8667ba931/build/2026-11-12/website/vector-10m-cold.json
[fh]: https://raw.githubusercontent.com/turbopuffer/tpuf-benchmark/070f9ef998c3470b25945766b2dac8b8667ba931/build/2026-11-12/website/fulltext-10m-hot.json
[fc]: https://raw.githubusercontent.com/turbopuffer/tpuf-benchmark/070f9ef998c3470b25945766b2dac8b8667ba931/build/2026-11-12/website/fulltext-10m-cold.json
[hh]: https://raw.githubusercontent.com/turbopuffer/tpuf-benchmark/070f9ef998c3470b25945766b2dac8b8667ba931/build/2026-11-12/website/hybrid-10m-hot.json
