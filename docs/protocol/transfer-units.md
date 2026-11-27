# Transfer units: authority moves in small pieces

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Statut : historique.** Ce document trace la décision de transférer l'autorité par petits morceaux. Le fonctionnement actuel est décrit dans [transfert-de-zone.md](../architecture/transfert-de-zone.md).

Status: step 1 adopted 2026-11-21 (`FLIP-UNIT`); steps 2 and 3 are a design,
not code. Asked by Rémi: relieve a hot machine as fast as possible, by
transferring ownership progressively in small pieces instead of blocking a
whole region.

## Problem

A zone is `(collection, location)` of a record (`recordZone`). With deep
locations nearly every record is its own zone: a vnode of 20 000 items holds
about 20 000 zones, and a split moves 8 000 to 32 000 of them (AWS,
2026-11-20 and 21). Until the causal checkpoint of 2026-11-02 a location
became a zone of its own only once it held `delegation` items (5 000 by
default); under that count its items stayed in the parent zone. That
threshold is gone, and the cost of a transfer is O(zones):

- the final phase took 12 s (median) to 146 s (worst) on AWS, against 2 to 4 s
  for the warm copy;
- 11 fixes since 2026-11-13 exist only to make per-zone work affordable.

## Step 1 (done): small flip units

See [handoff.md](handoff.md), `FLIP-UNIT`. Storage is unchanged: every zone
keeps its fence, its seal, its `ZoneTransfer` and its state row, so every
guarantee is the one already proved. Only the order and the size of the work
changed: units of 64 zones, deepest first, most written first, two in flight.

Local benchmark (`BenchmarkJoinFinalPhase`, 4 096 zones, no fsync, 4 cores):

| Units | Final phase | Longest time a unit had no writer |
| --- | --- | --- |
| 512 zones, one at a time (before) | 1.64 s | 250 to 310 ms |
| 64 zones, one at a time | 1.65 s | 46 to 64 ms |
| 64 zones, two in flight (now) | 1.4 to 1.5 s | 58 to 105 ms |
| 64 zones, four in flight | 1.3 to 1.4 s | 140 to 180 ms |

The total stays O(zones), about 0.4 ms a zone: the units of one store queue on
its write lock, so more units in flight lengthen each hold more than they
shorten the total.

## Step 2 (design): the unit carries the authority

Make the unit, not the zone, the thing that is fenced, sealed and
transferred. A unit is a subtree `(collection, prefix P)` of the moving region
holding at most `U` items (`U` configurable, the old `delegation` read as a
maximum). Storage zones do not change.

| Guarantee | Per zone today | Per unit |
| --- | --- | --- |
| One writer (`ZONE-1`) | fence and `ZoneTransfer` per zone, replayed in the local transaction of both disks | fence and transfer row per unit; the source refuses a location under a frozen `P`, only the receiver acknowledges after the unit's install |
| No acknowledged write lost | seal per zone at its freeze | seal of the unit (sequence and digest per zone) at its freeze; the receiver checks its copy and only the delta is sent |
| Zones born during the transfer | second pass, then the region freeze | covered: a zone born under a frozen `P` is frozen with it |
| Bottom-up (`ZONE-2`) | deeper zones first | units are disjoint subtrees; records exactly at an inner prefix are a unit of their own, after its children |
| Split takes from the donor only (HotTrie) | moved set decided per zone by bit `b` | a unit never straddles bit `b`: it is cut deeper |
| Reads never go back (`READ-1`) | source until installed, newer copy kept | the same, per unit |
| Region certificate, consumed token | at the end | unchanged |

Expected gain: O(units) rows and transactions instead of O(zones), and no
second pass. Not measured yet.

## Stop and resume

Step 1 changes nothing here: the durable state is per zone.

Step 2 raises these, to settle before any code:

1. **A unit cut between freeze and install.** Its prefix is fenced on the
   source and not yet writable on the receiver. Rule kept from today: a frozen
   unit never thaws; the retry sends the same seal. A write under `P` waits
   in memory on the source, unacknowledged, for a bounded time (2 s for
   example), then is refused as today. Nothing unacknowledged needs to be
   durable.
2. **Unit boundaries after a restart.** They must not be recomputed from
   live counts, which moved: the unit list of an operation is persisted once,
   at the start of the final phase, and replayed.
3. **A receiver that restarts with part of the units installed.** It holds a
   transfer row per installed unit and serves those; the source resumes the
   others. `flippedIn` (reads of flipped zones) reads unit rows.
4. **Lookup cost.** The write path and `zoneHolder` must find the frozen `P`
   covering a location in a few reads: at most 16 prefixes, or an index per
   open transfer.
5. **Totals.** The summary tree must stay exact when a unit holding zones of
   several depths changes holder.

## Step 3 (only if measured): what to add back

- seal-only stage: `stage-zone` sends the seal, not the rows the receiver
  already holds;
- one call per unit instead of stage, emit and install;
- placement of vnodes on machines by load (REM-85).
