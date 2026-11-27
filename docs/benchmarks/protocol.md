# Benchmark protocol

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

How every figure in the reference documents of this directory is produced.
A figure without its run directory, commit, number of passes and noise is not
a reference figure.

## What a run is

One command from the repository root builds the binaries from the tree under
test, creates a disposable AWS stack, runs a campaign from a load machine,
downloads the results and removes the stack:

```sh
RUN=exp-$(date +%Y%m%d-%H%M%S)-label MODE=passes PASSES=3 RATES_PER_VNODE="1000 2500" \
  SIZES=16 HOSTS=16 SLOTS=1 LOADERS=2 HOST_TYPE=i4i.xlarge LOADER_TYPE=c6i.4xlarge \
  NET=h3 TTL=60 ops/experiment/aws/run.sh
python3 ops/experiment/passes.py ops/experiment/results/$RUN
```

- The stack has its own VPC, security group, IAM role and bucket, tagged
  `Project=indexus-experiment`. It never touches `ops/deploy`. Every instance
  terminates itself at the TTL even if nobody removes it.
- `results/<run>/tree.txt` records the commit, a digest of uncommitted
  changes, the machines and the plan parameters.
- Region eu-west-3. Nodes speak TLS and HTTP/3 between each other (NET-1);
  the load reaches nodes over HTTPS.

## Machines

| Role | Default | Why |
| --- | --- | --- |
| Node host | i4i.xlarge (4 vCPU, 32 GiB, local NVMe) | the node's data on local NVMe, mounted noatime |
| Load | c6i.4xlarge (16 vCPU), 2 or more, synchronized | the load must not be the limit |

Every host raises `net.core.rmem_max` and `wmem_max` to 7.5 MB for QUIC.
One vnode per host unless the run says `SLOTS` > 1.

## A step

A step offers a constant rate for `STEP_SECONDS` (40 s by default) from every
load machine at one shared start instant, each with its own seed and key prefix
(LOAD-SYNC). The last three quarters of the step are its steady part. Then:

- the sentinel (`experiment sentinel`) watches every node during the step and
  stops it on a stop signal (see the ladder skill: stagnation, a handoff open
  more than 2 min, a loop, split knowledge, knowledge bloat, a fenced zone, a
  node exit);
- the audit reads back a sample of acknowledged keys (5 000 per step), and
  every key of any collection whose total disagrees (AUDIT-2), checks that no
  zone has two writers, and compares totals.

A step is **green** when the sentinel did not stop it, the audit is durable
(no acknowledged key missing, no zone with two writers, every node answering)
and at least 95% of offered writes were acknowledged in the steady part.

## Modes

| Mode | Question | Procedure |
| --- | --- | --- |
| `push` | where does one size saturate? | from `PUSH_START` writes/s per vnode, ×1.5 per step until the first red step |
| `passes` | how stable is a figure? | the same `RATES_PER_VNODE`, `PASSES` times, each pass on a cluster rebuilt from genesis |

Reference figures come from `passes`. A `push` ramp finds the range to measure;
its single run says nothing about noise.

## What each row records

`push.jsonl` has one row per step: offered and acknowledged writes/s, per
vnode, green, and where the time goes:

| Field | Meaning |
| --- | --- |
| `write_p99_ms` | a write at its entry node, receipt to acknowledgement |
| `commit_p99_ms` | one storage group commit |
| `commit_wait_p99_ms` | a write's wait for its group commit |
| `fresh_p50_ms`, `fresh_p99_ms` | from a probe write's acknowledgement until the root total counts it (probe every 200 ms) |
| `busiest_node_cores`, `loader_cores` | who is the limit |
| `udp_rcvbuf_errors` | datagrams the hosts dropped |
| `pass` | pass number in `passes` mode |

Latency and freshness histograms are log2 buckets in microseconds. A p99 of
"1 048 ms" means the p99 falls in the bucket that ends at 2^20 µs: between
524 and 1 048 ms. Reference documents write these as "≤ 1 048 ms". A change
smaller than one bucket is invisible.

## Noise

`passes.py` reports, per size and rate, the mean acknowledged rate across
passes, its coefficient of variation (CV) and the median of each tail. A
difference between two runs is a result only when it exceeds twice the larger
CV of the two, and the two runs used the same machines and rates.

## Limits

- Local single-host runs find defects and per-node costs, not throughput: one
  host shares its CPUs between every node and the load.
- AWS on-demand machines in one availability zone: no cross-zone latency.
- The audit samples acknowledged keys; it reads every key only of collections
  whose totals disagree.
