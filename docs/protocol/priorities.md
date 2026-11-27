# Scheduler priorities and preemption

**Statut : actuel.**

Scope: this is the reference cold/hot preemption model in `internal/protocol`.
The causal runtime does not implement a general P0/P1/P2/P3 scheduler. The durable pair fold finishes a started handoff before cancelling remaining
work on heat beyond its budget or topology drift, and ignores heat once every
piece is installed (COLD-2); it does not implement this reference
scheduler's full reversible preemption. General balloon execution remains unwired.
These classes do not establish
production queue isolation or a write-latency bound.

The scheduler does not treat reconfiguration work as one FIFO queue.

## Priority classes

| Priority | Work | Abort semantics |
|---|---|---|
| P0 | Recovery after irreversible `TRANSFER_OUT`, fencing completion | Never abort; finish/recover |
| P1 | Hot split, write-hot subdivision, emergency MOVE, hard-cap disk reclaim | Preempts cold work |
| P2 | Balloon / cold compaction / machine drain optimization | Fully preemptible before authority transfer |
| P3 | Opportunistic GC, packing, metrics-only work | Lowest priority |

## Hot split rule

When vnode `v` is selected for a hot split:

1. increment the regional `HotEpoch`;
2. invalidate every cold plan whose touched-vnode set contains `v` (or whose regional digest is now stale);
3. prevent any new cold operation from starting in that plan;
4. abort reversible cold handoffs and release their resources;
5. complete any authority transfer that already crossed its irreversible point;
6. cancel planned retirements for `v`;
7. execute the hot plan from the **actual current state**, including any cold transfers that already completed.

Completed cold actions are never rolled back.

## Disjoint regions

A hot event in region `R1` need not abort an independent cold plan in disjoint region `R2`.
Preemption is regional / vnode-scoped, not global.
