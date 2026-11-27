# Testing

**Statut : actuel, en partie obsolète.** Il cite encore `internal/cluster`. Voir [stockage-et-verrous.md](../architecture/stockage-et-verrous.md).

Names and status live in [invariants.md](invariants.md) and [verification.md](verification.md).

`go test ./...` runs the short chain. `INDEXUS_LONG=1` explores more space. It adds no rule. A rule that exists only under `INDEXUS_LONG=1` is not yet a default rule.

Read `internal/protocol/protocol_contract_test.go` first. Each subtest carries an id (`OWN-1`, `PLAN-1`, `HOT-1`, …) and the matching `TestOWN1_…` function runs on its own.

## Reproducible handoff checks

[handoff-reliability.md](handoff-reliability.md) describes the current seeded
scheduler over real storage transactions, its power-loss device, independent
checks, fuzz corpus and pinned TLC models. Run
`bash scripts/research/check-handoff.sh --quick`; full local qualification also
requires the pinned `TLA2TOOLS_JAR` and `--full`. No AWS resource is provisioned.

## Where

| Level | Place | Question |
| --- | --- | --- |
| Spec | `docs/protocol/invariants.md` | What must be true |
| Kernel | `internal/protocol` | Which implementation |
| Contract | `protocol_contract_test.go` | The short reading |
| State space | `internal/modelcheck` | Does any bounded causal or zone-tree schedule break a rule |
| Node | `indexus` | Does the product call the kernel |
| Cost | `internal/simulation` | What a still-correct mesh costs |

`internal/testoracle` is the slow argmin. It does not import the protocol. `internal/testkit` holds no invariant.

A simulation never turns a hypothesis into a guarantee.

## Safety and the rest

Safety (`OWN-1`, `HOT-1`, `MOVE-1`): zero mismatches. One counter-example is a bug.

`HND-2`, `AUTH-1/2/3` and `ROUTE-1` have bounded disk and scheduler evidence under the [failure model](failure-model.md). Views can diverge. These tests do not cover every unbounded crash schedule.

Operational (`MIR-1`, `AUTO-2`): decisions and refusal conditions, not theorems.

Performance (time-to-relief, bytes copied, machine count): simulation only.

## Seeds

A random test that fails must log `seed=` and `step=` (`testkit.DumpSeedOnFailure`). `INDEXUS_SEED` replays the same source.

## Causal runtime coverage

`internal/causal` checks merge laws, dependency holes, exact geometric regions and
single-successor validation. `internal/cluster/causal_*test.go` reopens disks at
transfer cut points. `internal/mesh` exercises concurrent joins, divergent views,
partitions, lost install responses, physical MOVE and warm-mirror divergence.
`internal/modelcheck/causal_test.go` explores 398 bounded states, including 240
with differing routing owners. The simulation fixture uses the causal runtime. The former quorum fixture has
been removed after comparison; its previous failing logs remain available.

Keep failed logs. A corrected targeted rerun is not an uninterrupted green LONG
run. The September validation record is archived outside the repository (runtime audit).

## Incremental load campaigns

The benchmark harness (archived with its campaigns outside the repository) added reproducible optimistic,
normal, empirical-density and adversarial datasets, stepped open-loop arrival
rates, durable-core runs, lower-rate replays and per-run reports. Saturation and
inconclusive recovery are separate from safety failures. It does not replace the
fault scenarios above or measure HTTP/multi-machine throughput.
