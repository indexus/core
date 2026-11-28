# Lot rows: one transfer row per lot instead of one per zone

Status, 26 November 2026, branch `claude-transfer`: **step 1 is implemented**
(the proofs and the acknowledgement of a lot are one row); steps 2 and 3 are
a design. The model is `LotRows.tla` (results below).

**Evidence levels.** *Measured*: counted by `TestLotTransferWork`
(`internal/storage/transfer_work_test.go`), a deterministic test that moves
one lot of one-record zones between two stores, without a network and without
fsync, for lots of 8 and 24 zones. *Read in code*: visible in the source,
nothing counted. *Assumed*: stated as such. No cluster run and no timing were
made: this document claims rows and reads, not seconds.

## Vocabulary

A join moves a vnode's region in **grains** (one transfer per address prefix,
at most 512 zones, GRAIN-1). Inside a grain, zones flip in **lots** (4 to
2 048 zones of one depth, FLIP-UNIT): one commit at the source (`FenceZones`),
one round trip, one commit at the receiver (`MountZones`), then the source
records the answer (`AckZoneInstalls`). A lot is the unit of the commits. It
was not the unit of the rows: each zone of the lot had its own rows.

## The rows before, and who reads them

All of these are control rows of the store (`\x00kv` collection). None is in
the capacity accounting: `dataUsage` counts `live`, `history`, `operations`,
`staging` and `transfer` only, so the zone-state, ZoneTransfer and lot rows
below cost disk but no quota (read in code, `usage.go`).

| Row (key) | Written | Read by |
|---|---|---|
| **Zone state** `meta/zone-states/<op>/<zone>`: `Frozen, Sealed, Transferred, Installed, Acked` | Source: at discovery (no flag), again at the fence, again at the acknowledgement. Receiver: at the mount | **Write path**, one point read per write: `outgoingFrozen` (is the zone fenced here?), `installedPermission` (is it installed here before its region?). **Routing**: `ZoneHolder` (`Transferred` → the receiver). **Reads**: `installedExactPoint`, `addMovedZones` (referrals to the receiver of each transferred zone). **ZONE-2**: `zoneReady` scans the states below a parent. **Resume**: `TransferZones` → `resumeZoneInstalls`, `pendingZones`. **Region close**: `fenceGranularRegion`, `SealCausalRegion`, `receivedRegionDigestPaged`, in pages of 512 checked against the states version. **Collect**: `compactZoneRows` |
| **States version** `meta/zone-state-ver/<op>` | with every state row | the O(1) check of the paged scans |
| **ZoneTransfer** `meta/zone-transfers/<zone>/<op>`: parent right, operation, ticket, zone, from, to, sequence, seal digest | Source: at the fence. Receiver: at the mount | **Resume**: `resumeZoneInstalls` (the requested lot), `ReadFencedLot` (saved snapshot, verified seal and matching proof in one read), a repeated fence. **Region close**: the seal digest of each zone, on both sides (`SealCausalRegion`, `receivedRegionDigestPaged`). **Collect**. Not read by the write path, routing or reads |
| **Fenced snapshot** `transfer/<op>/<zone>` | Source: at the fence | resume, a repeated fence; the collection reads its key to drop the zone's live rows |
| **Staged copy** `staging/<op>/<zone>/…`: header, sequence, one row per record and receipt | Receiver: at the preload; header again at the mount | the mount; a restart between preload and mount |

Of a ZoneTransfer row, only the zone, the sequence and the digest are its
own: the five other fields are those of the operation's cession, and the
install refuses a proof whose fields differ from it (`installZonesTx`).

### Measured baseline (before any change of this branch)

Rows and reads per step, written "per zone + per lot". Scans grow in pages
and are not linear: both raw counts are given (8 zones ; 24 zones).

| Step | Rows put | Rows deleted | Commits | Point reads | Scans | Staged-record JSON decodes | Seals |
|---|---|---|---|---|---|---|---|
| source copy | 0 | 0 | 0 | 1/zone | 2/zone | 0 | 0 |
| receiver preload | 4/zone + 4 | 0 | 1 | 18/zone + 8 | 16 | 0 | 1/zone |
| **source fence** | **3/zone + 4** | 0 | 1 | 8/zone + 8 | 43 ; 140 | 0 | 1/zone |
| **receiver mount** | **7/zone + 5** | 0 | 1 | 23/zone + 14 | 58 ; 142 | **2/zone** | 1/zone |
| **source ack** | **1/zone + 1** | 0 | 1 | 1/zone + 2 | 0 | 0 | 0 |
| source region seal | 4 | 0 | 2 | 1/zone + 17 | 2 | 0 | 0 |
| receiver region stage | 3 | 0 | 1 | 2/zone + 10 | 12 ; 15 | 0 | 0 |
| source region emit | 3 | 0 | 1 | 4 | 0 | 0 | 0 |
| receiver region install | 3 | 2 | 1 | 1/zone + 8 | 10 ; 13 | 0 | 0 |
| source complete | 2 | 0 | 1 | 2 | 0 | 0 | 0 |
| receiver collect | 3 | 6/zone + 1 | 3 | 4/zone + 9 | 20 ; 25 | 0 | 0 |
| source collect | 4 | 7/zone + 1 | 3 | 2/zone + 14 | 41 ; 90 | 0 | 0 |

The test starts after the admission: the discovery (one state row put per
zone at the source) is not in the table (read in code, `noteDiscovered`).

What the per-zone rows are, read in code against these counts:

- source fence, 3 puts: state, ZoneTransfer, fenced snapshot;
- source ack, 1 put: state;
- receiver mount, 7 puts: staged header (certification), state, ZoneTransfer,
  live record, live sequence, live receipt, ledger receipt;
- receiver preload, 4 puts: staged record, receipt, header, sequence.

So the state and ZoneTransfer rows are 4 of the source's 5 puts per zone
(discovery included) and 2 of the receiver's 11. The larger part of the
receiver's work per zone is the staged copy and the point reads, not these
rows: lot rows do not touch it (see "What this does not remove").

## Step 1 (implemented): the proofs and the acknowledgement of a lot are one row

### Rows

```
meta/zone-lots/<op>/<lot>   {Zones: [{Zone, Seq, DataDigest}, …], Acked}
meta/zone-states/<op>/<zone> {Zone, Frozen, Sealed, Transferred, Installed, Acked, Lot}
```

- `FenceZones` writes one lot row with the sequence and digest of each zone
  it cedes, and names it in each zone's state (`Lot`). No ZoneTransfer row.
- `MountZones` (and `InstallZones`) writes one lot row with the zones it
  installs in this commit, and names it in their states. No ZoneTransfer row.
- `AckZoneInstalls` sets `Acked` on the lot row when the call covers every
  zone of the lot: one row whatever the lot's size. A call that covers part
  of a lot (a resume that cut it differently) sets `Acked` on those zones'
  own state rows, as before. A zone is acknowledged if either says so.
- The lot number is the states version the commit passes: no counter of its
  own, unique per operation and per disk. It is local: it never travels.
- A lot row moves the states version, as a state row does: an
  acknowledgement invalidates the paged scans of the region close exactly as
  it did.
- The collection deletes the lot rows after the last state row, in the same
  bounded batches, then the version row.

The state row stays, one per zone, written once at the fence and once at the
mount, as before. It is what keeps the rest unchanged:

- the write path still decides with one point read of the zone's own row,
  never a lot row;
- routing, read referrals, `zoneReady` (ZONE-2) and the pending-zone plan
  read the same rows with the same flags;
- **resumption is per zone**: `Transferred`, `Installed` and the
  acknowledgement are still answered zone by zone, so a transfer cut inside
  a lot resumes from the zones not yet transferred, whatever lot they were
  fenced in. The receiver installs the zones of a message it lacks and
  skips the others; its own lot rows follow its commits, not the source's
  lots.

### Measured after step 1 (and after the install read-back fix of the same branch)

| Step | Rows put | Rows deleted | Point reads | Scans | Staged-record JSON decodes |
|---|---|---|---|---|---|
| source fence | 3 → **2/zone** (+5/lot, was +4) | | 8 → 7/zone | 43 ; 140 | |
| receiver mount | 7 → **6/zone** (+6/lot, was +5) | | 23 → 22/zone | 58 ; 142 → 42 ; 94 | 2 → **1/zone** |
| source ack | 1 → **0/zone** (+2/lot, was +1) | | 1/zone + 3 | | |
| receiver collect | | 6 → 5/zone (+2, was +1) | 4 → 3/zone | 20 ; 25 → 22 ; 27 | |
| source collect | | 7 → 6/zone (+2, was +1) | 2 → 1/zone | 41 ; 90 → 43 ; 92 | |

The region seal and the region stage each read one more row per lot (the
lot row: +18 and +11 point reads, were +17 and +10). Every other cell is
unchanged. The decode and the mount's scans are the
read-back fix (the install writes what the certification decoded), not lot
rows. Per one-record zone, over the whole transfer, lot rows remove 2 of the
source's 4 puts after discovery, 1 of the receiver's 11, and one delete on
each side; each lot pays one more row at the fence, the mount and the
acknowledgement, and each side one more delete per lot.

### On-disk compatibility

- **A store with rows per zone opens and finishes its transfers.** A state
  without `Lot` (the JSON of a build before lot rows) reads its ZoneTransfer
  row and its own `Acked`, as before. A transfer may hold both shapes: zones
  fenced before the upgrade keep their rows, zones fenced after go to lot
  rows, and the collection deletes both.
  `TestTransferWithRowsPerZoneFinishes` rewrites a transfer in flight to the
  old shape on both disks (the bytes an old build writes: the state JSON is
  identical, `Lot` is omitted when zero), reopens them, resumes, installs,
  acknowledges, closes the region and collects.
- **No format change is declared** (`indexus-flat-4` stays): nothing a build
  before lot rows wrote is read differently.
- **Downgrade is not supported while a transfer is open.** A build before lot
  rows that opens a store holding lot rows finds transferred zones without a
  ZoneTransfer row: it refuses to resume or close those transfers
  (`ErrNotReady`, `ErrConflict`) and keeps their fences. It does not grant a
  writer. This was read in code, not run.
- **Wire: unchanged.** `install-zone` carries the same `ZoneTransfers`,
  `Seals` and `Batch`; the region certificate commits to the same digest
  (each zone with its seal digest, in zone order). A node with lot rows and
  a node without exchange the same messages; which rows each keeps is its
  own business. The lot number is not sent.

### Safety argument, per guarantee

**Single writer per address.** The two decisions are unchanged and are still
made on the zone's own state row, in the same commits: the source refuses a
zone whose state is `Frozen` (`outgoingFrozen`), the receiver accepts a zone
whose state is `Installed` (`installedPermission`). The lot row is written in
the same zonelog batch as the states that name it (one frame, one fsync, all
or nothing): no state can name a lot that is not durable, and a lot row
without its states cannot exist. A state that names a missing lot is an
error, never "not fenced" (`zoneLotOf`).

**An acknowledged write is never lost.** The fenced content is still the
fenced snapshot row, durable in the fence's commit and kept until the
collection. What changed is where its proof is kept. The proof a resume
replays is rebuilt from the lot row (zone, sequence, digest) and the
operation's cession (parent right, ticket, source, target). Those cession
fields are fixed before the first fence: `BindCausal` sets the ticket before
`ConfirmCausal`, and the only later change to the cession is its region
digest and barrier at the region seal, which a zone's proof does not hold
(read in code: `granular_transfer.go`, `local_permissions.go`). The receiver
checks every field of a proof against its own copy of the cession and the
digest against its certified copy: a proof rebuilt wrongly is refused, not
installed. `TestFencedLotPowerLossBeforeFirstDelivery` (unchanged) and
`TestLotRowHoldsTheProofsOfItsZones` compare the proof read back after a
restart with the one the fence returned.

**Exact reads.** Reads use the state rows (`Transferred`, `Installed`) and
the fenced snapshots, none of which changed.

**A transfer cut anywhere resumes or cancels.**
- Cut before the fence commit: no state, no lot row; the zones are writable
  at the source; cancellation is still possible.
- Cut after the fence commit, before or during delivery: states and lot row
  are durable together. The resume lists the transferred, unacknowledged
  zones from their states and lot rows, reads the fenced snapshots and
  replays the same proofs.
- Message lost, duplicated or cut differently: the receiver installs the
  zones it has not installed and skips the others, so a zone that took writes
  since its install is never mounted again
  (`TestLotResumesFromTheZonesNotInstalled`).
- Cut after the receiver's commit, before the source's acknowledgement: the
  resume replays; the receiver answers without writing; the source
  acknowledges.
- Acknowledgement of part of a lot: recorded per zone. The lot row is marked
  only when the call names every zone of the lot, so a zone is never read
  as acknowledged because its neighbours are
  (`TestLotAcknowledgedInPartsCloses`; without the check the region freeze
  is accepted with a zone not installed, and the test fails).
- If a lot were nevertheless read as installed when it is not, the region
  certificate would still be refused: the receiver computes the region
  digest from the zones it installed.

**An install replayed after its transfer is forgotten is refused.** The rule
is the ticket's and is not touched: a transfer absent from the control state
mounts nothing. The collection deletes the lot rows with the states, so a
forgotten transfer leaves nothing a replay could find; the tests check that
a replayed mount and a replayed install are refused, write no row and leave
the zone as it was.

**What is durable before an acknowledgement stays durable before it.** No
commit was split or moved: the fence, the mount and the acknowledgement are
the same three commits, each with fewer rows.

## Model

`/home/claude/tla/LotRows.tla` (kept outside the repository, with the
`AddressAuthority` and `PendingAuthority` models and `tla2tools.jar`). It
models one row per lot on each side, the zone's fence and install as
membership of a lot row, resumes that carry any set of unacknowledged zones,
the region fence, certificate, install and collection, the cancellation
before the first fence, a second operation that can bring zones back, and a
message set where delivery never removes a message (duplicates, reordering,
replays long after), `Lose` removes one and `Crash` removes those of a disk.
Every variable but the messages is durable and each action is one commit.

Invariants: `SingleWriter` (at most one disk may write a zone),
`NoLostAck` (an acknowledged value is on the disk that may write its zone;
while none may, it is in the durable content the transfer resumes from),
`OneLotPerZone`, `TypeOK`.

Run: `java -cp tla2tools.jar tlc2.TLC -deadlock -workers 2 -config LotRowsA.cfg LotRows.tla`.

| Config | Rule | Bounds | Result | States generated / distinct | Depth |
|---|---|---|---|---|---|
| `LotRowsA.cfg` | design | 2 disks, 2 zones, values {1, 2}, 1 operation, loss and crash | **no error**, complete | 4 336 285 / 396 240 | 23 |
| `LotRowsC.cfg` | design | 2 disks, 2 zones, values {1, 2}, 2 operations (zones can come back), no `Lose`/`Crash` action | **no error**, complete | 60 230 785 / 5 022 461 | 31 |
| `LotRowsB.cfg` | design | 2 disks, 2 zones, value {1}, 2 operations, loss and crash | no error in what was explored; **stopped, not exhausted** | 110 601 409 / 11 568 716 (5 108 919 left on the queue) | 22 reached |
| `LotRowsReplay.cfg` | broken: a forgotten transfer is not remembered as closed | as A | **rejected**: `NoLostAck` violated in 12 states (a late install is mounted again over a write the receiver acknowledged since) | 300 557 / 53 592 | 12 |
| `LotRowsThaw.cfg` | broken: an unanswered lot is given back to the source's writers | as A | **rejected**: `SingleWriter` violated in 5 states (the lot's message is still installed) | 958 / 458 | 6 |

A message never delivered is a lost one, so for these invariants `Lose` and
`Crash` add no behaviour; config C leaves them out to reach two operations
with two values. TLC 2 workers, 2 cores: A 18 s, C 7 min 10 s; B was stopped
after about 8 minutes.

The first version of the model failed twice, both times on the model, not on
the design, and both findings are rules the code already has: a source must
not collect the rows of a zone it holds again by a later install (GC-1,
`dropCededZone`), and a fence constrains only the right it consumed, not one
the disk acquires later (`outgoingFrozen` matches the input right).

**Limits of the model.** Two disks, two zones, at most two operations; the
admission handshake, the warm copy, the mirror and the seal check are one
step or absent; reads are not modelled; a lot row and its zones' states are
one fact (the code writes them in one batch); the acknowledgement of a lot
is modelled as the set of its acknowledged zones. It is a bounded check of
the lot-level protocol, not a proof, and it says nothing of the Go code.

## Gate

| Condition | Result |
|---|---|
| The model passes | yes, at the bounds above (A and C complete; B not exhausted, no error found); both broken variants are rejected |
| No wire change that breaks a mixed cluster | yes: no message changed; the lot number is local |
| Every existing transfer and recovery test green, assertions unedited | yes: `internal/storage`, `internal/mesh` and the transfer and recovery tests of `indexus` pass; no existing test file was edited |

## What this does not remove, and what would

Step 1 leaves one state row per zone on each side. It is the row the write
path reads, and it cannot become a lot row by listing zones: a write must
learn in one small read whether its zone is fenced, and a lot row of up to
2 048 zones is neither small nor keyed by zone.

**Step 2 (design, not built): the fenced snapshots of a lot in one row.**
`transfer/<op>/<lot>` instead of `transfer/<op>/<zone>`: the source fence
goes from 2 puts per zone to 1. The row is bounded as a lot is (1 MiB, 8 MiB
for a zone alone). The collection reads the snapshot keys to find the zones
whose live rows it drops (`dropCededZone`): it would read the lot rows
instead. Local, no wire change. Not built here because it changes the
collection of ceded rows, which GC-1 guards, and that deserves its own
tests.

**Step 3 (design, needs its own model): the lot is an address sub-prefix.**
The fence of a lot would be one row `(collection, location prefix, depth)`
and no state row per zone: a write checks the prefixes of its location (at
most 16 point reads, or one seek with a suitable key), and a zone born under
a fenced prefix is fenced with it. This is the "native sub-region lots" the
protocol documents list as not delivered. It is not a re-encoding:
- it changes behaviour that tests assert today: a zone born at the donor
  under a flipped parent stays with the donor until its own flip
  (`TestGranularNewbornChildDuringParentCopyFlipsBeforeParent`, ZONE-2);
- zone ids alias their ancestors (`installedExactPoint`), so a prefix of ids
  is not a set of canonical zones: the lot must be defined on
  `(collection, location)`, and routing and read referrals with it;
- the scheduler cuts lots by count, heat and bytes, not along prefixes;
- the region digest and the resume must list the zones as fenced, not as
  they are when read.
`LotRows.tla` does not cover it: it models a lot as a set of known zones.

**Not a row question.** Per one-record zone the receiver still makes 4 puts
at the preload, 6 at the mount, 5 deletes at the collection and about 45
point reads. Options 2 and 3 of the duplicate-work audit (stage in the final
form; no staging) address the staged copy; the point reads are the subject
of its finding 3.

## Not verified

- No timing, no fsync, no cluster: the tables count rows and reads.
- Lots above 24 zones and zones above one record were not counted.
- A build before lot rows opening a store with lot rows (downgrade) was read
  in code, not run.
- `resumeZoneInstalls` asks `ZoneTransfers` zone by zone; each call decodes
  the zone's lot row. For a grain of 512 zones in few lots that is 512
  decodes of a row of up to 512 entries, on the resume path only. Not
  measured.
