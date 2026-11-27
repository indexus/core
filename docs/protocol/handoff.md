# Durable local handoff

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Statut : historique.** Ce document trace les décisions du handoff. Le fonctionnement actuel est décrit dans [transfert-de-zone.md](../architecture/transfert-de-zone.md).

> **Intégration du 25 novembre :** le chemin nominal courant regroupe désormais
> fence+émission dans `FenceZones`, puis dernier delta+certification+montage dans
> `MountZones` : un commit de chaque côté. Le déroulé historique détaillé
> ci-dessous conserve les anciens échanges séparés. Voir
> [l'état des lots](local-permissions.md#lots--état-de-lintégration-du-25-novembre)
> pour la préparation MVCC, les reprises bornées et les limites restantes.
> L'ordre `ZONE-2` et la consolidation régionale sont toujours présents ; les
> lots natifs de sous-régions ne sont pas implémentés par cette passe.

A topology merge does not transfer authority. Authority flips **atomically per
zone**, not per geometric region or vnode. A region (`token ∩ XOR cell`) is
only the planning envelope: because `id(z) = bits(location) || suffix(collection)`,
that envelope is a subtree, and transferring it as one grant would move a parent
with its children. The serving path therefore separates three facts:

- **DesiredOwner(z)** — XOR owner on the local view (`OWN-1`). A routing hint.
- **Authority(z)** — disk that may ACK `z`. The leaf grant's holder, replaced by
  the `To` of the latest `ZoneTransfer(z)` until the region certificate consumes
  that grant.
- **Residual(z)** — bytes still on the donor after `TRANSFERRED`. They have no
  authority; `CollectTransferred` may drop them only after the whole MigrationSet
  has flipped (`MIG-1`, `GC-1`).

```text
BeginCausal: persist scoped intent, discover MigrationSet keys in pages (source stays writable)
  → resumeZoneInstalls         replay any emitted zone whose install ACK was lost
  → bottom-up, in adaptive units (4 to 2 048 zones, 40 ms no-writer target) bounded by 1 MiB (8 MiB single-zone cap), deepest first, frozen then hottest first (FLIP-UNIT):
        copyLot                copy and converge this lot while source stays writable
        FreezeZones            fence only this lot
        final mirror; CertifyZones (seals only; immutable-snapshot repair on representation mismatch)
        EmitZoneTransfers      irreversible
        install-zone           receiver mounts lot live and ACKs its writes immediately
        first installed lot    publishes activation (ACT-1, proof may be a zone)
  → FreezeRegion               short commit: no new zone may appear at the donor
  → transfer remaining lots    drain stragglers born during earlier copies
  → SealCausalRegion           digest from immutable per-zone proofs, no payload scan
  → stage / EmitCausal         one region certificate (consolidation)
  → install / CompleteCausal   CollectTransferred GCs residuals; compact ZoneTransfers
```

Each donor copies and converges a unit immediately before its authority flip;
it does not wait for a region-wide warm copy. Only the authority flip
(freeze → `TRANSFERRED`) is bottom-up (`ZONE-2`). After `ZoneTransfer(z)` only
the receiver ACKs `z` (`ZONE-1`); other zones of the same MigrationSet may still
be copying or still live on the donor.

The source's local write transaction checks the per-zone fence and
`Authority.HolderOf(z)`. A racing write on `z` is either included through that
zone's `F` or refused after `FreezeZone(z)`. Unfrozen siblings stay writable.
The region certificate covers the whole geometric envelope, including zones
absent from both inventories, and is emitted only when every zone in the
MigrationSet is `TRANSFERRED` (`MIG-1`). `ZoneTransfer` records are compacted
out of the gossiped closure once that certificate is present: a certificate per
zone would explode residual cubes.

Each copy round reads only the moving region from disk and ships the mirror
backlog of many zones in one request and one receiver transaction. Diffuse data
puts one record per zone, so a round that paid one call and one transaction per
changed zone could not catch up with live writes; the batch is bounded (512
zones per request) and a failed batch rolls back whole. Per-zone freeze and
install are one zone per request.

While a MigrationSet moves, the source's XOR view already names the receiver as
DesiredOwner but Authority stays on the source until that zone's
`ZoneTransfer`. A node that receives a write therefore never pre-routes by the
view: it applies when `HolderOf(z)` is its disk and the zone is writable, and
otherwise forwards to `HolderOf(z)`. Pre-routing by view made a write bounce
between the two until their views merged, up to the 64-hop bound, for the
duration of the transfer.

A join copies and completes each donor independently, without waiting for the
other donors to finish their warm copies. Authority flips bottom-up inside
each donor. The joiner publishes its activation as soon as the first zone is
installed (`ACT-1`), with a grant proof or a zone proof. From that point writers
route to the joiner; a write for a zone still held by a donor is forwarded to
that donor. Between a donor's `EmitZoneTransfer(z)` and the joiner's
`install-zone(z)` there is no writer for `z`: that window is one zone's frozen
phase, independent of siblings and of other donors. A parent does not freeze
while a child in the same MigrationSet is still untransferred. New zones created
at the donor after their parent has flipped stay on the donor until their own
leaf flip; `FreezeRegion` then closes the door. The receiver serialises its
installs on its own disk. Regions not acquired by `Join` are pulled by
`Rebalance`. The frozen snapshots of a transfer live in their own bucket, not
in the causal state blob, so state reads and writes during a transfer stay
small. The warm capture reads its snapshot in a read transaction and persists
only the intent and the MigrationSet; each `FreezeZone` binds that zone's fence
to its final inventory. Source collection after the region install runs in
bounded batches off the join's critical path and resumes after a restart.

Retries preserve operation identity and mirror progress. A restarted warm copy
can refresh its snapshot boundary without resetting the receiver's persisted
sequence. A frozen zone uses its immutable inventory; a frozen operation uses
its immutable region certificate. A lost `install-zone` or region-install ACK
resumes the same `ZoneTransfer` / certificate; it cannot choose a different
successor. Diverging warm mirrors remain writable and resumable. Frozen zones
never thaw because of a timeout or a newer gossip view.

`PREPARED` bytes grant no write permission. `CUTOVER_READY` means certified through
F; it still does not authorize the receiver. Only durable `install-zone` (or the
region install), active vnode membership and the matching physical placement
permit ACK. Placement changes use the same grants but a separate host-successor
chain.

Concurrent overlapping transfers serialize at the holder of the consumed token.
Independent tokens can advance in parallel. Autoscale reserves every addition
of the validated plan in planner order, without truncation or an activation DAG.
The former component builder remains only as a historical geometric test model;
local durable tickets and installed pieces determine actual activation.
The mesh scheduler copies and flips authority bottom-up in small units
(`FLIP-UNIT`, `flipUnits` in `internal/mesh/flip_units.go`):

- A unit is 4 to 2 048 zones of one depth (adaptive, FLIP-HOLD-1), neighbours in collection and
  location order, targeting 1 MiB for grouped lots while a single zone up to
  8 MiB travels alone. While a lot flips, only its zones have no writer. A
  single oversized zone (> 8 MiB) currently returns `ErrInventoryItemTooLarge`
  while still writable; paged copy of one large zone is not implemented yet.
- Deeper zones flip first, so a parent flips after every child of the
  operation (`ZONE-2`). A new child zone born during a parent's copy forces a
  rebuild of the bottom-up waves before that parent freezes.
- Within a depth, already-frozen zones finish first on retry, then the most
  written units go first (the node's zone heat), so the donor sheds write load
  from the first lots on without waiting for a regional warm copy.
- Each zone keeps its own durable state. A handoff cut anywhere resumes from
  the zones not yet transferred, whatever units they were in. Next step if
  O(zones) metadata still dominates at scale:
  [transfer-units.md](transfer-units.md).

Source GC requires durable receiver installation of the region certificate and
an irreversible source fence, and that every zone in the MigrationSet has
already transferred authority (`MIG-1`). It also verifies that no later
installed descendant grant has taken ownership of the same bytes on this disk:
a delayed old ACK cannot collect returned data. It need not wait for every
topology replica to converge. Certificates and the consumed-token fence remain;
acknowledged payload copies and, once collected, per-zone states are released.
A final activation may still be
pending, during which requests can be unavailable. The receiver retains the
certified grant, zone transfers and vnode identity, so publication is
reconstructible. A shadow inventory must refuse the read, not return empty.

A finished transfer is compacted. The receiver drops its per-zone states when
it installs the region certificate; the source drops them once residual
collection ends, which also compacts older databases during maintenance. The
record keeps the certificate and its `Emitted`/`Installed` flags, which replay
needs. The per-zone fences are no longer needed: the consumed token already
refuses the source (`AUTH-3`). A delayed `stage-zone` or `install-zone` for an
installed operation is ignored at the transfer level, never zone by zone:
otherwise it would mount old bytes over later acknowledged writes (`HND-2`).
`TestCompletedTransferIsCompactedAndLateZoneMessagesAreNoOps` reproduces that
loss without the guard. Kept, these states grew the control document with every
handoff the node ever took part in.

Recovery reads persisted intent, MigrationSet, `ZoneTransfer`, certificate,
install and placement state. It never infers authority from `view.Owner(z)==self`
alone. The old quorum workflow is archived; its local state-machine regression
cases remain test fixtures and are not an executable protocol.

## Granular transfer qualification (2026-11-21)

New mesh transfers use `BeginCausal`: the intent commits before discovery, and
new writes add their zones to the MigrationSet in their own transaction.
Discovery pages keys without materialising regional payloads. Per-lot snapshots
are copied while their source is writable (one adaptive unit targeting 1 MiB per
lot, 8 MiB single-zone cap, deepest first, already-frozen then hottest first);
convergence checks live sequences via `ZoneSeqs` without re-reading payloads,
and normal frozen traffic is mutation tails and seals (`CertifyZones`).
Canonical seal encoding (`Metrics: nil`) prevents memtable-vs-segment
mismatches, while immutable-snapshot repair remains for legacy mirrors; seal
verification is never weakened.

The final regional fence checks atomically (via `zoneStatesVersion` updated by
`putZoneState`) that every known zone has already installed and been ACKed,
draining late-born waves warm instead of fencing their copy; after a bounded
number of warm drain rounds (8) under unbounded zone creation,
`ForceFreezeRegion` closes discovery so consolidation and residual GC cannot
starve. `fenceGranularRegion`, `SealCausalRegion`, `StageCausal` and
`EmitCausal` scan `ZoneState`/`ZoneTransfer` rows in bounded 512-zone read
pages, compute digests outside any store lock, and commit after an O(1) version
check so live client writes are never stalled behind an O(zones) metadata lock
hold. Writes acknowledged on early-installed lots survive regional
consolidation. Total closure work still scales with O(number of zones), so this
does not establish a constant-time handoff or a hard freeze percentile.

An emitted zone whose install or install ACK was lost is resumed with the same
proof (`resumeZoneInstalls`). It never thaws on timeout. Residual collection
still waits for regional installation; no payload is collected just because a
route names a new owner.

Bounded runtime evidence (in-process transport, NoSync fixtures):

- `TestFirstLotWritesBeforeRemainingRegionIsCopied`: first installed lot accepts
  a write before 1 100 seeded zones have been copied; donor refuses the same lot.
- `TestGranularHandoffFlipsHottestUnitFirstBeforeRegionCopies`: hottest unit
  moves and accepts writes on the receiver before the remaining region is copied.
- `TestGranularHandoffResumesFrozenAndLostACKWithoutOverwritingLiveWrites`:
  mid-lot failure after `FreezeZones` resumes the frozen lot first; lost
  `install-zone` ACK replay preserves live receiver writes.
- `TestGranularNewbornChildDuringParentCopyFlipsBeforeParent`: child born during
  parent copy triggers a bounded wave rebuild and flips before its parent (`ZONE-2`).
- `TestGranularScaleKeepsACKsAndOneWriterAcrossVnodes`: eight vnodes with writes
  through the original entry point verify every ACK once and single-writer probes.
- `TestGranularRegionFenceSurvivesReopenBeforeConsolidation`: persisted regional
  fence still refuses source writes after reopen before consolidation.
- `TestPagedStageCausalAllowsConcurrentWritesAndRejectsBadDigest`: concurrent
  live writes on the receiver interleave through `StageCausal` (4 096 zones,
  512-zone pages) while tampered digests are still rejected with `ErrConflict`.

Neither local copies nor remote copies grant authority; any future local
transport must pass the same certification, fence, install, placement and replay
gates. Adding a vnode on an existing machine does not add physical CPU/disk
capacity or fault isolation.
