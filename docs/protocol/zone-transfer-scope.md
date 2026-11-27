# Zone transfers stay between source and receiver

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Statut : historique.** Ce document trace la décision de garder les transferts de zone entre source et receveur. Le fonctionnement actuel est décrit dans [transfert-de-zone.md](../architecture/transfert-de-zone.md).

Status: adopted 2026-11-18 (REM-121). Answers open question 1 of
[linear-knowledge.md](linear-knowledge.md) and open items 1 and 2 of
[p2p-findings-2026-11-15.md](../benchmarks/p2p-findings-2026-11-15.md).

## Problem

A handoff flips authority one zone at a time (`ZONE-1`, [handoff.md](handoff.md)).
Each flip is a `ZoneTransfer` fact, and every one of them was gossiped to every
node until the region certificate replaced them. A split under load moves tens
of thousands of zones. The local 2-vnode spike of 2026-11-18 measured the
knowledge bundle going from 2 kB to 12.5 MB, then 15.5 MB, about 30 000
transfers of ~400 bytes. Every batch of 64 flips made a new version, and every
mesh call or reply carrying a new version carried the whole bundle. Nodes spent
their CPU encoding and decoding it, writes stopped, and monitoring stopped
answering. The cost is O(nodes × zones moved) per split, against the target of
knowledge proportional to vnodes.

## Rule

`ZONE-GOSSIP-1`. A `ZoneTransfer` is a fact between its source and its
receiver. It is persisted by both and is not part of the knowledge bundle that
other nodes receive, with one exception: a transfer named in the `ZoneProof` of
an activation travels with that activation, so that any node can check the
proof (`ACT-1`). The region certificate still reaches everyone and, as before,
consolidates the transfers of its operation.

A publisher cites at most one zone in an activation's `ZoneProof`, and none when
its installed grants already meet its cell: one piece meeting the cell is enough
for `ACT-1`.

`INTENT-LEAN-1`. For the same reason, a reserved join intent carries the
decision (vnodes to add, base view), not the planner's per-zone forecast
(`TopologyPlan.Changes`). Validation recomputes changes from the view and
ignores the stored ones, and only the intent's author reads it. After the
zone-transfer change, the same local spike still showed one 12 MB bundle: the
split's intent listed every moving zone with its statistics.

## Why it is safe

The facts a third node derived from per-zone transfers, and what replaces them:

| Use by a third node | Without the per-zone facts |
| --- | --- |
| Write authority (`ZONE-1`) | Never decided by a third node: the source refuses after `FreezeZone(z)` and only the receiver's disk, holding the transfer, can ACK. Unchanged. |
| Write routing | A write reaches the source (grant holder) or the receiver (view owner). The source forwards along its local `HolderOfZone`, the receiver forwards back for zones it does not hold yet. One more hop until the certificate, as for any stale route. |
| Region reads (`QueryHolders`) | Until 2026-11-19 a source with an open transfer refused a complete read (`ErrFrozen`). It now serves its zones as of their freeze until the transfer installs (READ-1); a zone served by both sides is kept once, the newer copy. Afterwards the source's reply carries the region certificate, the reader's authority digest changes, and the read retries on the new holders. |
| Activation proof (`ACT-1`) | The cited transfer travels with the activation. |
| Retirement check | The retiring vnode checks, with its own complete knowledge, that it is not the receiver of an open transfer before it publishes the record. Third nodes' check was redundant. |

Nothing about authority is weakened: the single-writer predicate is replayed in
the local transaction of the source and of the receiver, both of which hold the
transfer durably (`write-authority.md`).

## Cost

Knowledge stays O(vnodes) plus the region certificates of open operations
during a split. The knowledge version changes once per activation and once per
certificate, not once per batch of zones. The source and receiver keep paying
O(zones) for their own transfer, which they need anyway.

## Alternatives considered

- Gossip deltas against the peer's version. Every node would still store,
  validate and re-encode every transfer: the total stays O(nodes × zones). Still
  useful later for certificates (E5), not for this.
- One fact per batch of 64 zones. 64 times smaller, same growth, and a change of
  fact format and digests.

The same shape is used by Redis Cluster slot migration: only source and target
know the per-key state, and the cluster map changes once at the end.

## Tests

- `TestZoneTransfersStayBetweenSourceAndReceiver`: with a migration stopped
  after its first zone install, a third node learns no per-zone transfer from
  the source, is served the read by the source, accepts the receiver's activation
  with only the cited transfer, and reads every row once after the migration
  without syncing first. Fails when every transfer is gossiped and when the
  cited one is not (defect injection).
- `TestReservedIntentCarriesNoZoneForecast`: a reserved intent keeps its
  vnodes and drops `Changes`.
- Existing handoff, ZONE-1, activation and retirement tests unchanged.
- Local spike: knowledge stays at a few kB through the split, and writes do not
  stall.
