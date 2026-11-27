# Experiments on a live network

What is measured and why: [p2p-experiment.md](../../docs/benchmarks/p2p-experiment.md).

| File | Role |
| --- | --- |
| `../../cmd/experiment` | `load` (scenarios, concurrent readers), `observe` (polls `/metrics`), `audit` (read-back, double owners, totals) |
| `report.py` | per-run summary and the scaling table across runs |
| `cluster_local.sh` | N real node processes on loopback |
| `aws/` | disposable AWS stack and the one-command campaign |

## Local

```
go build -o bin/node ./cmd/node && go build -o bin/issuer ./cmd/issuer && go build -o bin/experiment ./cmd/experiment
COMPACTION=false BIN=$PWD/bin N=16 DIR=/tmp/exp ops/experiment/cluster_local.sh up
cd /tmp/exp && N=16 TARGETS=$(paste -sd, targets) MONITORS=$(paste -sd, monitors) \
  ISSUER=http://127.0.0.1:22000 EXPERIMENT=$OLDPWD/bin/experiment PCT=15 \
  $OLDPWD/ops/experiment/aws/scenarios.sh /tmp/exp/results
python3 $OLDPWD/ops/experiment/report.py /tmp/exp/results/*
DIR=/tmp/exp $OLDPWD/ops/experiment/cluster_local.sh down
```

One host shares its CPUs between every node and the load: local runs find
defects and per-node costs, not throughput.

TLS: `cluster_local.sh` runs plain HTTP. The agent runs TLS when given a CA.
HTTP/3 between nodes (NET-1) needs the node built from the `quic/` module,
which links quic-go; the production node (`cmd/node`) and the loader
(`cmd/experiment`) link no QUIC stack and speak HTTPS over TCP
([docs/http3.md](../../docs/http3.md)):

```
(cd quic && go build -o ../bin/node ./cmd/node)   # instead of ./cmd/node
```

```
bin/experiment ca -dir /tmp/exp/ca
bin/experiment agent -slots 8 -node bin/node -dir /tmp/exp/slots -issuer http://127.0.0.1:22000 \
  -ca /tmp/exp/ca -bootstrap https://127.0.0.1:21100 &
curl "http://127.0.0.1:22100/spawn?genesis=1&wait=ready"; curl "http://127.0.0.1:22100/spawn?wait=ready"
export SSL_CERT_FILE=/tmp/exp/ca/ca.crt   # for load, observe, audit and curl
```

`GET /metrics` reports `transport.http3`, `transport.tcp` and `transport.fallbacks`.

## Live curves

`ops/experiment/live.py` follows a run from the counters alone, one point
per 5 s: writes offered, acknowledged and slow, vnodes, items, transfers,
the slowest node, freshness of the root totals, summary flushes, network.
AWS runs open `results/live.html` in the operator's browser; the page
reloads itself. `live.py replay DIR` rebuilds the points of a finished
phase and `live.py compare` overlays up to four runs.

## Ladder on machines (AWS)

`ops/experiment/aws/run.sh` runs a campaign from the operator's shell, with
the operator's AWS credentials; it removes everything it created, also on
failure, and every instance terminates itself at TTL minutes.

```
MODE=push HOSTS=8 SLOTS=8 SIZES="8 16 32 64" LOADERS=2 ops/experiment/aws/run.sh
```

`LOADERS=k` adds load machines (LOAD-SYNC): each runs `experiment loadagent`,
and the coordinating one gives every step to all of them with one start time
and 1/k of the rate each (own seed, own id prefix); their seconds are summed and
their acknowledged keys merged before the audit. `NET=tcp` runs the production
node (HTTPS over TCP) instead of the `quic/` node.

Each step also records, in its directory: every node's full `/metrics` at the
first and last poll (`metrics-first.json`, `metrics-last.json`; `push.jsonl`
derives `mesh_by_kind`, the service time per mesh request kind, and the busiest
node's share), and every node's and load machine's goroutine stacks three
quarters into the step (`stacks/node-*.txt`, `loader-*/stacks.txt`;
nodes expose `/debug/stacks` only when the agent starts them).

`/metrics` times a write at the node that received it
(`writes.latency_us_log2`, receipt to acknowledgement, relay included) and the
store's group commit (`disk.commit_us_log2`: one transaction and its fsync;
`disk.wait_us_log2`: one write from submission to its commit's end;
`disk.writes / disk.commits` is the mean group). Buckets are powers of two in
microseconds; `push.jsonl` gives their p99 per step (`write_p99_ms`,
`commit_p99_ms`, `commit_wait_p99_ms`).

When a collection's exact total differs from what was acknowledged, the audit
reads back every acknowledged key of that collection through every node
(AUDIT-2): `mismatch_keys_lost` counts the keys found on none, i.e. lost writes;
zero means the total read itself is wrong.

Per size, the load machine starts at 300 writes/s per vnode (`PUSH_START`) and
multiplies by 1.5 each 40 s step (`STEP_SECONDS`) until less than 95% is acknowledged or the sentinel stops
the step; a size is climbed only if the previous one had a green step.
`results/<run>/push.jsonl` has one line per step (offered, acked, per vnode,
audit, sentinel reason). No cap: the machines are the resource. One
`c6i.2xlarge` load machine; if it sheds arrivals while nodes are idle, the
loader is the limit, not the cluster.

## Ramp to the knee (RAMP-1)

`MODE=ramp` is the dynamic counterpart of `push`: one open-loop step per
size whose offered rate doubles every `RAMP_DOUBLE` seconds (default 60)
after `RAMP_HOLD` (30) at `RAMP_START` writes/s per vnode (500), until fewer
than `RAMP_STOP` (0.9) of the writes offered over 20 s are acknowledged. The
load then stops itself (`knee.json`), the cluster drains, the audit reads it
back. `RAMP_SHARE` (1) is the share of each doubling done linearly; the rest
is one jump at the end of the period: 1 is a continuous ramp, 0 an instant
2x step, 0.5 grows +50% linearly then jumps +33%. Design and plan:
[docs/benchmarks/ramp.md](../../docs/benchmarks/ramp.md).

```
MODE=ramp SIZES="8 16" HOSTS=16 SLOTS=1 HOST_TYPE=i4i.xlarge HOST_PRICE=0.398 RAMP_PASSES=3 ops/experiment/aws/run.sh
MODE=ramp RAMP_ELASTIC=1 RAMP_DOUBLES="300 120 60" HOSTS=2 SLOTS=4 BUDGET=10 ops/experiment/aws/run.sh
```

`ops/experiment/ramp.py DIR VNODES DOUBLE HOLD` gives one line per step in
`ramp.jsonl`: `best_green` (the highest acknowledged rate held 10 s with at
least 99% acknowledged, nothing shed and an intended p99 at most 1 s), per
vnode, the knee, and per doubling its ratio and `absorb_s`. Every load line
now also has `intended_latency_us_log2`: from the write's scheduled arrival,
queueing in the load machine included, so an overloaded cluster cannot hide
its tail behind a waiting client (coordinated omission). The load machines
are sized for `RAMP_MAX` (5 000 writes/s per vnode at the largest size;
40 000 per c6i.4xlarge), and the offered rate never goes above it.

## Machines on demand (AWS)

`HOSTS` sets the initial machine count. In `MODE=steps`, the default
`SLOTS=auto` lets a machine host additional vnodes as CPU, memory and disk
allow. The agent offers one new receiver at a time and checks the offer again
when starting it. An explicit positive `SLOTS` selects the fixed layout.

In auto mode `/facts.receiver_ready` and the second field of `/count` report
the current offer (zero or one). `/facts.free` counts unoccupied addresses,
including on a machine still warming, and `address_slots` is the address limit. AWS reserves 100 monitor ports (19100–19199) before the
diagnostic range at 19200. P2P uses the corresponding offsets from 21100.
An unused address has no process, listener or metrics poll. In the capacity
plan `max_vnodes` is the fleet's address limit when `resource_slots` is true.
The observed density depends on workload, headroom and incoming transfers.
An explicit `GOMEMLIMIT` takes precedence. Otherwise each new process receives
a soft Go memory limit based on the larger of its host's CPU count and the
number of live processes including itself. Existing processes retain the limit
they received at start.

The AWS harness has no implicit 16/64/128-host
ceiling, and the provisioner no longer freezes the vnode limit at startup.
It uses `-spawnMax 0 -localDir ...`: the hook manages fleet capacity. Direct
EC2 provisioning still requires a positive `spawnMax`.

Before creating resources, `aws/capacity.py` computes the smaller of:

- the machine count affordable for `TTL` minutes under `BUDGET` (default $5),
  including the real loader instance type and count;
- the regional standard-family On-Demand vCPU quota, minus other running or
  pending campaigns and the loaders;
- `HOSTS_MAX`, only if explicitly supplied. `HOSTS_CEILING` remains a deprecated
  explicit alias; neither has a default cap.

CPU counts come from EC2 instance descriptions. Linux On-Demand prices come
from AWS Pricing unless `HOST_PRICE` / `LOADER_PRICE` are supplied. An initial
fleet that exceeds the bounds is rejected, rather than silently raising them.
This is a compute-cost estimate over a full TTL, not a billing cutoff: storage,
network and public IP charges are additional. Other AWS limits, subnet address
capacity (the experiment uses a /20 subnet), or unavailable instances can still
reject launches; AWS errors appear in `machines.log`.

The resulting `capacity.json` records each limit and the binding reason. Hooks
read it from S3 before applying a placement or reconciling fleet capacity. Raise the policy during a running experiment:

```bash
bash ops/experiment/aws/run.sh capacity RUN --max-hosts 64 --budget 25 --dry-run
bash ops/experiment/aws/run.sh capacity RUN --max-hosts 64 --budget 25
# Or remove the operator cap, retaining the budget and quota bounds:
bash ops/experiment/aws/run.sh capacity RUN --max-hosts auto --budget 25
```

Replace `RUN` with the run ID. The update rechecks quota and publishes the policy
without restarting nodes. A lower limit prevents new launches; it does not kill
existing machines or cancel launches already reserved. The port range stays fixed for the run. Each resource-admitted machine can
add vnodes in that range whenever its headroom recovers. The node
IAM role can read the policy but cannot write `plan/` or increase its own budget.

Host 0's hook (`elastic/spawn.sh`) maintains a warm pool (`SPARE`, `SPARE_RATIO`)
and reserves each pending launch under a lock. It also counts EC2 machines that
have not registered their agent yet, excluding load generators. Reservations
are conservative while booting; they can temporarily reduce available room.
A new host registers under `agents.d/` once its agent answers. The observer and
audit discover machines there. At a full fleet, free slots remain usable; a
failed spawn reports the capacity and reason instead of a hidden vnode cap.
Teardown terminates every instance tagged with the run.

With `MODE=steps`, loader sizing and the default worker count follow the peak
(8 × `STEP_RATE`), including when `LOADER_TYPE` is supplied explicitly. An
explicit `STEP_WORKERS` is preserved. `live.py` plots loader CPU to distinguish
client saturation from cluster saturation. Dynamic policy changes and limits
above 32 vnodes are covered by local mocked-AWS tests; this revision has not
been deployed to AWS yet.

## Ladder

`ladder_local.sh` runs one ladder step: per pass a fresh TLS cluster, the
sentinel, uniform writes at `RATE` per vnode against `CAP` per vnode (CAP-2),
the audit and the teardown, also on failure. It stops at the first red pass.

```
BIN=$PWD/bin N=4 PASSES=3 RATE=80 CAP=100 ops/experiment/ladder_local.sh /tmp/ladder/step1-n4
```

`experiment sentinel -agents URL -out DIR [-on-stop CMD]` polls every node's
`/metrics` and exits 3 on the first stop rule: stagnation (writes neither
acknowledged nor refused), a handoff open more than 2 min, one operation tried
3 times, knowledge versions split for 60 s with no handoff open, knowledge
above 2 KB per vnode, a node that stops answering. The incident (reason, last
metrics of every node, the poll history) goes to `DIR/incident`.
The AWS campaign always runs TLS with a CA made for the run; `NET=h3`
(default) builds the node from `quic/`, `NET=tcp` runs the production node.

## AWS

Needs an AWS CLI with credentials and Go (no Terraform). From the repository root:

```
ops/experiment/aws/run.sh                  # 8, 16, 32 nodes
SIZES="16 32 64" ops/experiment/aws/run.sh
ops/experiment/aws/run.sh destroy          # remove a stack left behind
```

- Every resource is created for the run (its ids in `aws/.state/<run>.ids`) and tagged `Project=indexus-experiment`:
  the live deployment in `ops/deploy` is never touched.
- One fresh stack per size, destroyed before the next size starts.
- The stack is destroyed on success, on error and on Ctrl-C. Every instance also
  terminates itself at a fixed deadline (`TTL`, default 90 min) that survives
  reboots, in case the script itself dies.
- No port is open to the internet; nodes talk inside their security group and
  results travel through the run's S3 bucket, deleted with the stack.
- Defaults: `c6i.large` nodes, one `c6i.2xlarge` load machine, eu-west-3.
  The stack creates its own VPC and public subnet; `SUBNET_ID` uses yours instead.

Cost order of magnitude, on-demand eu-west-3: sizes 8+16+32 take about 25 min
each, a few euros in total; 16+32+64 about twice that.

Results: `ops/experiment/results/<run>/n<size>/<scenario>/` with `load.jsonl`,
`metrics.jsonl`, `audit.json`, and `report.md` across all sizes.

## Transfer recovery and demand diagnostics (2026-11-24)

For throughput-driven scaling, use `ITEMS=0 SCALING_WRITES=<measured threshold>`.
`SCALING_WRITES` sets `scaling.writes_per_vnode` without imposing a write cap;
`WRITE_CAP` remains a separate admission limit. Choose the threshold from a
steady-state per-vnode measurement with transfer headroom. A record-count
trigger (`ITEMS=20000`) keeps requesting splits as stored data grows, even at
constant throughput; it is a different experiment. Initial configuration and
both thresholds are printed in `tree.txt` / the launcher output.

For the direct 2k, 4k, 8k and 16k writes/s staircase, run
`ops/experiment/aws/steps-16k.sh --plan` to print the benchmark settings.
`--run` launches the experiment and opens its dashboard. The preset selects
a split at 50 writes/s per vnode, a 50% host CPU target, resource-based
per-machine process admission, and no write-rate cap. Individual settings
can be overridden, e.g. `SCALING_WRITES=100 ops/experiment/aws/steps-16k.sh --plan`.
The original 500,000-item trigger stays enabled for direct comparison with
the preceding run; `ITEMS=0` isolates throughput-driven splits.

The provisioner starts with one EC2 boot in flight per fleet when the resource
survey first shows a shortage. Additional requests in the same 15-second
placement interval share that credit. If the shortage persists, the window
doubles at most once per interval (1, 2, 4, …), bounded by actual receiver
demand plus any reserve justified by measured CPU. An idle interval of a
minute or a fully covered shortage starts the next episode at one. Warming
agents consume credit and the hook deducts pending EC2 instances and
unissued reservations before acquiring more. The machine cap still applies.
`GET /v1/placement` reports the boot window alongside the reserve.
When an eligible agent has room, it admits the vnode immediately. With
known reserve demand the hook reconciles spare capacity independently, and
logs the actual boot count in `machines.log` as a `reserve:` event.
AWS uses the surveyed agent identities alongside fresh EC2 inventory and
boot reservations. A complete resource decision avoids re-polling every
agent's slot count. Requests without an eligible destination, including at
the machine cap, can share one in-flight capacity reconciliation. Other
waiters receive a retryable failure immediately. Requests with a concrete
fallback can still place their own vnode at the cap.

A replay of the recorded 05:44:15–05:44:43 UTC burst on 27 November
retains its observed requests and assumes no newly requested EC2 has
registered during those 28 seconds. The previous hook launched 15 machines
by 05:44:22 and nine more at 05:44:43. Starting with a fresh window,
the new controller would request one at 05:44:15 and one at 05:44:31.
This replay exercises the controller's response to the recorded input;
it does not predict throughput or the later demand after changing capacity.

The loader reports its pending queue, in-flight writes and worker capacity.
`shed` means an arrival was dropped by the **loader's full queue**. HTTP 503
counts attempts (including retries); it is a separate series. ACK/s divided by
arrivals/s is a throughput ratio, not the percentage of unique requests that
will eventually commit. `load-summary.json` records final ACKs, queue drops,
exhausted retries and unfinished queued jobs. `-drain-timeout` bounds worker
drain to two minutes by default; cancellation does not undo a server commit,
and an unacknowledged attempt may have committed. The final audit still checks
ACKed keys and ownership; the summary alone is not a durability proof.

The observer stays alive through worker draining and audit (one-hour bound,
overridable by `OBSERVE_SECONDS`). Old handoffs trigger goroutine snapshots
once per minute/node, even when `/metrics` answers quickly. Metrics include
the current handoff phase, phase age and whether a retry worker is running.
Slow writer logs distinguish time waiting for `writeMu` from time holding it;
logs are emitted after releasing it, at most once every five seconds/store.

During the AWS phase, `evidence.py` publishes new complete lines of metrics,
load counters and load logs every 30 seconds. Parts are at most 8 MiB; confirmed
byte offsets prevent resending the entire growing file. Stack files are sent
once. ACK key files stay excluded. Reconstruct a stream by concatenating its
`.part` files in lexicographic offset order. Partial trailing lines wait for
completion. S3 confirmation precedes local checkpoint advancement, so upload
failure retries the same object. The live page explicitly hides stale tiles.
