# One fleet view for simulation and AWS

`view.js` and `view.css` render both sources without network dependencies.
The simulator embeds them through `sizing/build-view.mjs`; `live.py html`
embeds the same files into the existing self-contained `live.html`.
`live.py compare` retains campaign overlays for historical comparisons.

The view has global write/read/latency curves, machine resource bars, selectable
vnodes with holder application rates, and observed transfer edges as source →
destination rows. It does **not** invent gossip links. Each source snapshot can
be inspected on the time slider. The view is read-only.

## Schema 1

Each time point retains the existing flat global keys (`offered`, `acked`,
`p50_ms`, `p95_ms`, `p99_ms`, `reads_s`, `read_failed_s`, `read_p50_ms`, `read_p90_ms`, `read_p95_ms`, `read_p99_ms`,
`fresh_p99_ms`, `vnodes`, `open`, `frozen`, `shed`, `queued`, `in_flight`).
`x` is seconds since the campaign's first point. Its `fleet` contains:

- `schema: 1`, `source: aws | simulation`;
- `machines`: `id`, `label`, `state`, `cpu`, `ram`, `disk`, optional disk
  throughput `io`, `cpu_scope`, `slots`, `ready_offer`;
- `vnodes`: physical instance `id`, logical `label`, `machine`, `state`,
  `applied_s` (real) or `estimated_s` (model), `items`, optional `monitor`;
- `transfers`: `id`, source/target machine, phase, age, frozen state and
  optional `progress` and transferred workload `share`.

Resource values are percentages. Missing values are null/absent, never guessed
zero. The real collector groups monitors by physical hostname; `/slots` maps
transfer destinations when available. A logical vnode present on two hosts is
two physical instances, not evidence of two writers for the same zone.

`writes.applied` is incremented only after a newly applied durable mutation.
Holder hop counters include idempotent responses, and HTTP entry ACK counters
include forwarded writes: neither substitutes for that counter. Counter reset,
identity change, or first observation makes the rate unknown for that sample.
Rates use the delta of process uptime, matching the samples of the counter;
using the aggregator's five-second interval would introduce a 20% oscillation
when it observes node samples four then six seconds apart.
The first sample after upgrading an old archive cannot supply this rate.

## Observation and freshness

The node observer and load generator are unchanged. Independently,
`live.py machines AGENTS_FILE DIRECTORY` samples `/facts` and `/slots` every
15 seconds with at most eight workers and a two-second HTTP timeout.
The agent list follows new registered machines. `machines.jsonl` is included
in append-only evidence publication. No N² adjacency polling is added.

`agg` emits the fleet alongside its usual five-second global point. Node
samples expire after ten seconds; previously seen nodes remain visible as
stale. Machine resource facts expire after 45 seconds. A missing or failed
machine sample does not appear healthy. A stale donor retains its last
transfer as stale with unknown frozen state. No stale interval is bridged by a
valid-looking workload curve. Historical flat archives still render their
global curves, explicitly without a fleet inventory.

Whole-host CPU provenance comes from `host_cpu_ok`; older facts only establish
the vnode scope. `receiver_ready` comes from the Go admission predicate, so the
dashboard does not duplicate its thresholds or receiver CPU budget.
Empty, measured, admissible machines are warm. AWS boots which have not yet
registered are **not observed** by this collector; their count is `?`, not zero.
Boot reservations remain enforced by the provisioner/hook. Simulation knows
its own boot state and can show it directly.

Real transfer byte progress and per-zone freeze duration are not currently
exported: they remain unknown. The open/frozen operation counters are not a
measurement of how long every zone's writes waited. Client reads are reported
only when the chosen load generator records them.

## Reproduction

From the core repository:

```bash
python3 ops/experiment/live.py replay /absolute/run/phase /absolute/replay.jsonl
python3 ops/experiment/live.py html /absolute/replay.jsonl /absolute/live.html
node ops/experiment/sizing/build-view.mjs /absolute/sizing.fragment.html
node --test ops/experiment/sizing/model.test.mjs
python3 -m unittest discover -s ops/experiment -p 'test_*.py'
python3 -m unittest discover -s ops/experiment/aws -p 'test_*.py'
python3 -m unittest discover -s ops/experiment/elastic -p 'test_*.py'
```

`live.py`, `fleet.py`, and the `dashboard` directory must stay together when
rendering locally. AWS uploads the current `fleet.py` with the observer script;
it does not change the pinned load generator to obtain these measurements.

See [the simulation and its limits](../../../docs/benchmarks/resource-sizing-2026-11-26.md)
and [the production placement policy](../../../docs/design/placement-and-resources.md).

The growth chart counts client-ready vnodes and observed physical machines at
each sample. The tile also shows how many vnode processes answered. The gossip
membership count can differ from both. Density divides ready vnodes by observed
machines when each known machine has recent evidence. On AWS the denominator
excludes unregistered boots. In the simulator it includes known booting hosts.

Les percentiles de lecture agrègent les histogrammes des lectures ponctuelles
réussies sur chaque fenêtre. La fraîcheur du total racine a sa propre courbe.
Les anciens exports peuvent inclure des échecs de totaux dans `read_failed_s`.
