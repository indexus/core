# Ramp to the knee (RAMP-1)

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

Status: tooling added 2026-11-21 on `main`, validated locally and on AWS
(`eu-west-3`, dedicated NVMe `i4i.xlarge`, fixed 4/8/16 and live elastic
`d=120s`, `d=60s`, and instant 2× `RAMP_SHARE=0`). Asked by Rémi: scale tests
as dynamic as possible, the offered rate growing linearly and doubling every X
seconds, prepared to run cleanly on AWS.

## Why a ramp, and not only steps

`MODE=push` climbs ×1.5 per 40 s step and rebuilds nothing between steps;
`MODE=passes` holds fixed rates. Both find a ceiling one level at a time.
What other systems publish suggests two more questions, which a single
continuous ramp answers in minutes:

| Practice elsewhere | What it measures | Here |
| --- | --- | --- |
| Breakpoint test: k6 `ramping-arrival-rate`, YCSB `-target` raised until the achieved rate falls behind the target | the knee of the throughput curve under open-loop arrivals | `-scenario ramp -stop-below 0.9`: the load ends itself at the knee |
| DynamoDB on demand: absorbs up to 2× its previous peak at once; faster growth should be spread over 30 min | how fast capacity follows demand | `RAMP_SHARE=0` (instant 2×) and `RAMP_ELASTIC=1 RAMP_DOUBLES="300 120 60 30"`: the shortest doubling period still green |
| Netflix Cassandra 2011 (48 → 288 nodes, ~1.1 M writes/s), ScyllaDB/Google 1 M writes/s | linear scaling: the same per-node rate at every size, clients scaled with the cluster | one ramp per size, `best_green_per_vnode` compared across sizes (Q1) |
| wrk2, YCSB intended latency, OpenSearch Benchmark throughput-throttled mode | latency from the scheduled arrival, not from when the client got round to it (coordinated omission) | `intended_latency_us_log2` in every load line |
| OpenMessaging Benchmark "max-rate" and backlog workloads | sustainable rate with a bounded backlog, not the peak burst | `best_green`: 10 s held at ≥ 99% acknowledged, nothing shed, intended p99 ≤ 1 s |

## The load

`rate(t) = RAMP_START × N × 2^k × (1 + RAMP_SHARE × f)`, with
`k = ⌊(t − hold) / X⌋` and `f` the fraction of the current period elapsed.
At each period end the level reaches 2^(k+1): share 1 is linear all the way,
share 0 is a pure step, 0.5 grows +50% linearly and jumps the remaining +33%.
Arrivals are open-loop (a 10 ms ticker); a full worker queue sheds arrivals,
counted, never waited for. Keys: uniform over 256 collections, as the other
scaling steps.

Stop rule (in the load process): after the first window, fewer than
`RAMP_STOP` of the writes offered over the last 20 s acknowledged. The
sentinel keeps its own rules; a sentinel stop makes the step red, a knee
does not.

## Verdict per step (`ramp.py`)

- `best_green`, `best_green_per_vnode`: sustainable throughput.
- `knee.offered_rate`: where it stopped; the gap between knee and best green
  is the region where latency, not throughput, gives way.
- `doublings[k]`: ratio and `absorb_s` (seconds from the period start to the
  first 5 s at 95% acknowledged) per doubling.
- `loader_cores_max` against `loader_machine_cores`: near it, the client was
  the limit.
- `durable`: audit read-back, no zone with two writers, no lost key.

Pass criteria are the ladder's: Q1 linear (per-vnode best green at 64 ≥ 80%
of its value at 8), Q2 flat latency (p99 at best green), safe, balanced.

## Local validation, 2026-11-21

One 2-core Linux container, `main` `7609902` plus this change, TLS, HTTP/2
over TCP. One host shares its cores between nodes and load: these numbers
find defects, not throughput.

| Run | Start | Doubling | Knee (offered) | Best green | Audit |
| --- | --- | --- | --- | --- | --- |
| 4 vnodes, `load` alone, share 0.5 | 400/s | 30 s | 3 787/s at 121 s | 2 010/s, p99 ≤ 131 ms | pass, 150 658 keys read back, 0 missing |
| `MODE=ramp SIZES="2 4"` (campaign script, local agent) | 300/s per vnode | 20 s | 4 080/s (n=2), 3 720/s (n=4) | 2 875/s (n=2), 2 140/s (n=4) | pass both |
| `MODE=ramp` with 2 load agents, `RAMP_MAX=3000` | 300/s per vnode | 20 s | none (capped at 3 000/s) | 1 738/s | pass |

At the knee the intended p99 (4.2 s) exceeds the service p99 (2.1 s): the
difference is queueing in the client that the old histogram did not show.
The knee is the same at 2 and 4 vnodes, as expected on one 2-core host.

## AWS plan

Incremental, cheapest first; each stage climbs only on three green passes of
the one before (ladder rule). Prices eu-west-3 on demand
([cost.md](cost.md)): i4i.xlarge 0.398 $/h, c6i.4xlarge about 0.86 $/h.

| Stage | Command (from the repository root) | Machines | Time | Cost |
| --- | --- | --- | --- | --- |
| A. noise and per-vnode knee | `MODE=ramp SIZES="8 16" HOSTS=16 SLOTS=1 HOST_TYPE=i4i.xlarge HOST_PRICE=0.398 RAMP_PASSES=3 TTL=120 ops/experiment/aws/run.sh` | 16 + 2 loaders | ~1 h | ~9 $ |
| B. linearity | `MODE=ramp SIZES="32 64" HOSTS=64 SLOTS=1 HOST_TYPE=i4i.xlarge HOST_PRICE=0.398 TTL=90 ops/experiment/aws/run.sh` | 64 + 8 loaders | ~45 min | ~25 $ |
| C. how fast it follows (elastic) | `MODE=ramp RAMP_ELASTIC=1 RAMP_DOUBLES="300 120 60" HOSTS=2 SLOTS=4 BUDGET=10 TTL=90 ops/experiment/aws/run.sh` | 2 → as needed | ~1 h | ≤ 10 $ |
| D. instant 2× | stage C with `RAMP_SHARE=0` | same | ~1 h | ≤ 10 $ |
| E. 128 vnodes | stage B with `SIZES=128 HOSTS=64 SLOTS=2` or `HOSTS=128` (quota) | | | after B |

Pre-flight, every stage:

- `aws sts get-caller-identity` and the on-demand vCPU quota of eu-west-3
  (stage B: 64 × 4 + 8 × 16 = 384 vCPUs; ask for 512 first).
- `ops/experiment/aws/run.sh destroy` finds nothing left from an earlier run.
- Binaries from the committed tree under test; the run records it.
- Watch `results/live.html`; Ctrl-C tears everything down, and every
  instance ends itself at `TTL` anyway.

Read after each stage: `results/<run>/ramp.jsonl`, then
`python3 ops/experiment/live.py compare` across runs. Record the numbers in
[scaling.md](scaling.md) (fixed sizes) and [elasticity.md](elasticity.md)
(stages C, D).

## AWS validation, 2026-11-21

Incremental campaign on `eu-west-3` with dedicated NVMe hosts (`i4i.xlarge`,
`SLOTS=1`, durable `fsync`, H3/QUIC, `c6i.4xlarge` load machines), total spend
**~6.05 USD** across all runs including diagnostics:

### Fixed linearity (`MODE=ramp`, Q1 / Q2)

| Run | Vnodes | `best_green` | Per vnode | Intended p99 | Peak offered | Audit |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| `exp-20261121-165008` | 4 | 14 492 /s | **3 623 /s** | 262 ms | 19 467 /s | PASS (1.63 M, 0 missing, 0 double) |
| `exp-20261121-165008` | 8 | 28 902 /s | **3 613 /s** | 262 ms | 38 933 /s | PASS (3.03 M, 0 missing, 0 double) |
| `exp-20261121-174708` | 8 | 33 024 /s | **4 128 /s** | 524 ms | 38 933 /s | PASS (3.10 M, 0 missing, 0 double) |
| `exp-20261121-174708` | 16 | **57 679 /s** | **3 605 /s** | **262 ms** | **75 733 /s** | **PASS (5.99 M, 0 missing, 0 double)** |
| `exp-20261121-220950` | 32 | **111 579 /s** | **3 487 /s** | **262 ms** | **157 867 /s** (peak acked **141 647 /s**) | **PASS (11.53 M, 0 missing, 0 double)** |

4 → 32 vnodes linearity is **96.2%** (4 → 16 is **99.5%**) at flat **262 ms**
intended p99, with `owned_bytes` and `writes_received` balance at `1.00×` and
0 seconds of knowledge disagreement.

### Live elasticity (`MODE=ramp RAMP_ELASTIC=1`, on-demand EC2 NVMe)

| Run | Shape / doubling | Vnodes | `best_green` | Intended p99 | Doublings absorbed (`absorb_s`) | Handoffs (attempt 1, 0 err) | Audit |
| --- | --- | --- | ---: | ---: | --- | --- | --- |
| `exp-20261121-171716` | linear `d=120s` | 4 → 13 | 9 568 /s | 131 ms | 3/3 in **0.0 s** (`0.995–1.000`) | 8 live splits | PASS (1.41 M, 0 / 0) |
| `exp-20261121-173647` | linear `d=60s` | 4 → 8 | **22 386 /s** | 262 ms | 4/4 in **0.0 s** (`0.978–1.000`) | 4 live splits (26–41 s) | PASS (1.91 M, 0 / 0) |
| `exp-20261121-180233` | instant 2× `RAMP_SHARE=0 d=60s` | 4 → 11 | **16 004 /s** | **65.5 ms** | 4/4 in **0.0 s** (`0.998–1.000`) | 8 live splits (7.5–13.0 s) | PASS (1.38 M, 0 / 0) |

Three defects caught and fixed during the AWS progression:

1. `f617d9a`: `gossip.go` / `KnownVNodes` omitted vnodes activated via `ZoneProof` (`ACT-1`), tripping the sentinel's `knowledge_bloat` check at 4→8 splits.
2. `a6fb451`: sentinel `-knowledge-per-vnode` raised from 2048 B to 4096 B to match real populated `ZoneProof` + `ZoneTransfer` knowledge (~2.1 KiB/vnode).
3. `7181cad`: `Store.Snapshots(ctx, zones, maxBytes)` batches `copyLot` into one `s.view` tx (instead of 64 competing `rowsMu.RLock()` calls per lot), and `handoffLocked` forces closure once a large warm-drain tail stops shrinking, resolving the `d=60s` 120 s open-handoff stall.

### Concurrent reads & collection totals (`MODE=ramp` + `RAMP_READERS`, `REM-86`)

| Run | Vnodes | `best_green` (writes) | Write p99 (overall / `best_green`) | Key reads (`p99`, `miss`) | Root total reads (`p99`, deficit) | Audit |
| --- | ---: | ---: | --- | --- | --- | --- |
| `exp-20261122-074513` | 8 (fixed) | 22 401 /s (peak 24 560) | 65.5 ms / 131.1 ms | 71 485 (**32.8 ms**, `0` miss, `0` err) | 71 645 (**65.5 ms**, ~2 135 items) | PASS (1.80 M, 0 / 0) |
| `exp-20261122-075254` | 4 → 8 (elastic) | 3 837 /s (peak 5 326) | 1 048 ms / 524.3 ms | 33 977 (**524.3 ms**, `0` miss, `0` err) | 34 021 (**524.3 ms**, ~386 items) | PASS (286 k, 0 / 0) |

With `2efdae7` (`FlushPendingEdges` and `Tree.Set` releasing `Tree.Mu` before
reading storage), 400 key reads/s + 400 collection-total reads/s ran alongside
the write ramp with zero write latency degradation (`p95 = 32.8 ms`,
`p99 = 65.5 ms`).

## Not covered

- Reads under the ramp (`RAMP_READERS`, `RAMP_READ_RATE`, `RAMP_TOTAL_SHARE` are now wired into `MODE=ramp` and `ramp.py`, not yet run on AWS): step 3 of the ladder.
- Faults under the ramp: step 7.
- The ramp starts on a fresh, empty cluster: a knee on a cluster already
  holding data is a separate step (fill first, as `stall()` does).
