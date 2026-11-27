# Scaling

**Statut : actuel.**

`EvaluateMachine` does not mutate `V_active` and does not invent an owner. `ValidatePlan` recomputes XOR and discards the planner's predicted owners.

The order is not "always MOVE, then always HotTrie".

```text
host imbalance that a move can relieve     → MOVE
the vnode itself is hot                    → HotTrie
donor-only split cannot meet the target    → Regional ADD
a single zone cannot be cut                → atomic overload
no pressure                                → noop
```

A machine at 40% disk and 95% writes, with one intrinsically write-hot vnode, must HotTrie. Moving that vnode only moves the heat. `TestAUTO1_WritePrefersHotTrieNotMove` is that case. `TestAUTO1_MoveBeforeHotWhenDiskOnly` is disk pressure with a vnode that can move.

## MOVE

Same vnode id, same XOR owner, different machine. MOVE does not change `V_active`.

An explicit `-moveFrom` startup receives the same logical vnode on a new disk.
Actual local rights and directory exceptions move first; a compact endpoint at a
higher per-vnode epoch is published only after complete drain. Installed pieces
are writable before publication. Confirmed unfinished incoming transfers block
closing admission for a new MOVE; tentative header-only slots can be pruned.
Reservation and the MOVE gate share a disk transaction. See
[local permissions](local-permissions.md), including the terminal receipt and
old-epoch admission guards. There is no retained placement certificate chain.

## HotTrie

Safe split of the donor. `g(D) = 1 + max LCP(D, U)` over other active vnodes. A safe fork sits at `b ≥ g(D)`.

The new address is not searched or randomized: `J_b = D xor e_b`. It differs
from `D` at exactly `b`, including its suffix. For an owned zone `z`, the moved
set is therefore exactly `z[b] != D[b]`; it is not an LCP subtree. At most 96
candidate partitions are evaluated. Validation infers the sole changed bit,
recomputes the current guard, and rejects non-canonical or stale candidates.
For repeated add-only scaling, policy prefers the shallowest candidate where
both sides retain at least 20% of the donor. Choosing a slightly better-balanced
bit near 95 can create siblings with guard 96 and permanently exhaust that
branch; geometric locality is therefore not a valid final tie-breaker by itself.

If the owner of `z` changes, the old owner is the donor. Zero external steals.

If no such fork reduces pressure, the result is `ErrSafeSplitInsufficient`. No spawn, no reservation, no topology write (`HOT-2`).

## Regional ADD

A region is a prefix cell `R = p*`: every id that shares prefix `p`.

`R` is ownership-closed when every zone in `R` is owned by a vnode that itself lies in `R`.

```text
V_R = { v in V_active | v in R }
```

`REG-1`: a zone outside `R` keeps its owner. The validator proves containment of the complete new cells, not only the
inventory. The planner widens to the smallest enclosing ownership-closed prefix
when unconstrained leading bits require it; sparse topologies can require the
root region. Bounded exhaustive and independent-oracle tests check this geometry.

## Atomic zone

If one zone is above the target and cannot be cut, the plan stops. It does not invent an XOR exception.

## Choosing a machine

Choosing `J1`'s vnode id is not choosing a machine.

The logical plan emits vnode ids. Physical placement then picks hosts from disk headroom, write headroom, network, mirror capacity, handoffs already in flight, and heat correlation. A `ResourceOffer` is advisory. A reservation is what admits the bytes. Reserve before launch.

The experiment provisioner chooses the machine of a spawned vnode from CPU, memory, disk and joins in progress: see [placement and resources](../design/placement-and-resources.md).

## Process spawn

`EnableAutoscale` spawns a process only when `EvaluateMachine` produced a validated HotTrie or Regional plan. MOVE, atomic overload, and the absence of a plan do not spawn and do not run a second XOR split.

## Causal publication

Before spawning, the runtime persists a `JoinIntent` with the exact plan and
observed frontier. Intents coexist; they do not add active vnodes. A join rebases
against current authority and publishes once at least one installed grant or zone
proof reaches its cell (ACT-1). Acquisition of the remaining cell continues in
the background, with forwarding to current authority holders.
Further ADDs shrink cells; maintenance transfers excess authority to the converged
XOR owner. No global CAS or quorum is involved.

SoftLeave remains disabled, and the autoscaler only scales up. Pair compaction,
on by default, transfers installed
grants to one pinned survivor and publishes a durable causal retirement. It keeps
the genesis vnode and retired forwarding endpoints. This is a bounded retire-only
primitive; general balloon execution and physical packing remain unwired. See
[cold reconfiguration](cold-reconfiguration.md). Automatic host provisioning is
still distinct from explicit durable MOVE.

## Not wired

Skeleton refinement is not implemented. Its placeholder APIs have been removed. No balloon planner exists in code (`EXP-1` retired 2026-11-13).

## Cold balloons

The balloon pipeline (snapshot → `DesiredTopology` → `ADD` → `MOVE` → bottom-up pair-to-pair `HANDOFF` → `RETIRE`, preempted by any hot split of a touched vnode) is a design only; its unwired reference code was removed on 2026-11-13. See [cold-reconfiguration.md](cold-reconfiguration.md) and [priorities.md](priorities.md).

## Time to exhaustion

When growth is positive, `T_exhaust = remaining / growth`. Scale may start when that time is inside `k` scale intervals (`k = 2` in `exhaustSafety`). Zero, negative, and unknown growth do not fire on exhaustion. One spike must not pin the estimate (`RateEWMA`). These are decisions, not latency targets (`AUTO-2`).
