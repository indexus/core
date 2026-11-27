# Per-zone transfer state under its own keys (E5a, REM-87)

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Statut : historique.** Ce document trace la décision XFER-KEYS-1 (états de zone en lignes). Le fonctionnement actuel est décrit dans [transfert-de-zone.md](../architecture/transfert-de-zone.md).

Status: 2026-11-18, `ZoneTransfer` rows done (see "Result"). `ZoneState`
rows needed after all: a 60 000-zone split spends 83 % of its time in freeze
and emit, each rewriting the whole `Zones` map
([elasticity.md](../benchmarks/elasticity.md), evening run). First step of E5 in
[linear-knowledge.md](linear-knowledge.md). Follows
[zone-transfer-scope.md](zone-transfer-scope.md).

## Measured problem

Local spike, 2 vnodes, split at 20 000 items, step timings logged by
`transferZones` every 5 s (`ops/experiment/results/rem087-batch-20261118-130022`):

| Per batch of 64 zones | after ≈2 500 zones | after ≈13 000 zones |
| --- | --- | --- |
| freeze (source, local) | 40 ms | 113 ms |
| emit (source, local) | 50 ms | 157 ms |
| install (receiver) | 170 ms | 810 ms |
| throughput | 120 zones/s | 35 zones/s |

Freeze and emit touch only the local store, so their growth is the store's
own cost. Every per-zone step rewrites, in one transaction under `writeMu`,
data that grows by one entry per zone already moved:

1. the control document `causal-v1`: `CausalTransfer.Zones`, one
   `ZoneState` per zone of the operation (~100 B each);
2. the knowledge document: `Delta.ZoneTransfers`, one `ZoneTransfer` per
   flipped zone (~400 B each), marshalled and hashed whole
   (`writeCausalDigest`);
3. the validated replica: `CausalState.Replica()` keys its cache by
   `causal.Hash` of the whole delta, so every call re-encodes it.

A transfer of `n` zones in batches of `b` therefore costs O(n²/b): about
4 minutes and falling throughput for 30 000 zones. Batches of 256 instead of
64 gave ≈1.7× (same run, variant `b256`): a constant factor, not a fix.

## Rule

`XFER-KEYS-1`. A step of a transfer writes O(batch), never O(zones already
moved).

- Each `ZoneState` is stored under its own key
  `meta(xfer/<operation>/<collection>/<location>)`. `CausalTransfer` keeps the
  operation's summary only: certificate, `Frozen`, `Emitted`, `Installed`,
  and counters `Zones`, `Transferred`, `ZonesInstalled`.
- Each `ZoneTransfer` fact is stored under its own key
  `meta(zt/<operation>/<collection>/<location>)`, not in `Delta`. It is local
  to source and receiver already (`ZONE-GOSSIP-1`); `Delta` keeps only the
  ones cited by an activation, as the gossiped bundle does today.
- The in-memory authority index (`Authority.byZone`, `zoneIndex`) gains an
  incremental `addZoneTransfer` path: a batch adds its transfers to the
  current replica instead of rebuilding it from a re-hashed delta.
- The region certificate (`EmitCausal`, `CompleteCausal`,
  `CollectTransferred`) deletes the operation's `xfer/` and `zt/` keys in
  pages, as it compacts `ZoneTransfers` today.

Nothing about authority changes: the same facts, persisted in the same
transaction as before, only under separate keys. `ZONE-1`, `ZONE-2`,
`MIG-1` and the single-writer predicate replayed in the transaction are read
from the same values.

## Readers to move

Grep `\.Zones\[`, `transfer.Zones`, `Delta.ZoneTransfers` in
`internal/storage` (`causal_zone.go` 21, `causal_data.go` 10, `handoff.go` 3,
`permission.go` 2, `query.go` 1) and `internal/mesh/service.go` (3). Each
becomes one of:

- a point lookup of one zone (`permission.go`, `FreezeZones`,
  `EmitZoneTransfers`, `StageZones`, `InstallZones`): read its key;
- a scan of one operation (`transferZones` planning, `CompleteCausal` checks,
  `publishActivation`, `completeInventorySnapshot`, `outgoingFrozen`): a
  prefix scan of `xfer/<operation>/`, or the counters when only "all
  transferred?" or "any frozen?" is asked.

`completeInventorySnapshot` and `outgoingFrozen` run on every read and every
write: they must use the counters (`Frozen` count, `Transferred == Zones`)
or a point lookup, never a scan.

## Migration of stored data

A control document written before this change still has `Zones` inside
`CausalTransfer` and `ZoneTransfers` inside `Delta`. On open, move them to
their keys in one transaction (they are bounded by one open operation per
peer), then never read the old fields again. Test with a store written by the
current code.

## Order of work, for the implementer

1. Differential test first, not committed (rule 5 of linear-knowledge.md):
   random sequences of freeze / emit / stage / install / complete / restart
   on two stores, old and new code side by side, comparing after each step
   `writableIn` for every zone, `HolderOfZone`, `CausalInventory` and the
   gossiped `Knowledge`. Keep a copy of the old code in the test package.
2. Keys and counters for `ZoneState` (`XFER-KEYS-1`, first bullet). Run the
   differential test, then `go test ./internal/storage/ ./internal/mesh/`.
3. Keys for `ZoneTransfer` and the incremental authority path (second and
   third bullets). Same checks.
4. Compaction on certificate, and the migration on open, each with a test
   that fails without it.
5. One short spike (`096-rem087-batch.sh`, variant `b64` only): emit and
   install per batch must stay flat from the first to the last zone.
6. Full battery, one commit. Remove what became obsolete in the same commit:
   the decoded-control cache if the control document is now small, and the
   gossip filter `gossipedZoneTransfers` if `Delta` no longer holds
   per-zone transfers.

Out of scope: batch size (keep 64 until the flat cost is measured), and
pipelining the five steps of a batch.

## Result (2026-11-18)

Profiles first showed four costs that were not the documents themselves
(fixed separately, commit 2444462): the control document read and compared on
every permission check, a full usage scan after every handoff batch, ZONE-2
scanning the MigrationSet per zone, and `publishActivation` decoding the
whole state after every install.

Then the `ZoneTransfer` facts moved to rows, one per (zone, operation) under
`meta/zone-transfers`, on the source and the receiver:

- `EmitZoneTransfers` and `InstallZones` write the rows; the knowledge no
  longer changes per batch, so no node re-encodes, hashes or revalidates it.
- `writableIn` and the write relay (`Store.ZoneHolder`) apply the rows: a
  flipped zone's holder while its grant is unconsumed (not `Closed`).
- `MergeCausal` adds the rows an activation's `ZoneProof` cites (ACT-1):
  the cited transfer joins the knowledge with the record.
- The region certificate (`EmitCausal`, `InstallCausal`) drops the rows of
  its operation.
- Transfers written into the knowledge by an older build are still read
  there; no migration.

Same 300 s local spike (`rem087-xferkeys-20261118-151002`): install per batch
52 ms at the start, 61 ms at 25 000 zones (was 54 → 750 ms); the longest
transfer finished ≈25 800 zones in ≈3 min. The `ZoneState` map in the control
document (≈100 B per zone) no longer shows as a growing cost; it stays as is
until a measure says otherwise.
