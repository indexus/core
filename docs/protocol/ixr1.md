# IXR1: binary records, flat keys, bounded history and receipts

**Statut : actuel.** La note de conception de la fin compare avec bbolt, l'ancien moteur.

Design note, 2026-11-13, written against the former bbolt engine. Status:
the record value (IXR1, under *Format*) is what the zonelog engine stores
in its frames and segments ([../storage/zonelog.md](../storage/zonelog.md));
the bbolt costs, flat keys and migration below are the rationale that led
to zonelog, not a description of the current disk. The migration was never
built: nothing ran in production, a store of an earlier engine does not
open, and the node module no longer links bbolt.

## What one insert costs today

`BenchmarkInsertCost` (`internal/storage`, one insert per transaction, no
fsync, a record with five metrics; Xeon 2.1 GHz, 2 threads):

```text
                 time/insert   puts   buckets   bytes of keys+values   of which the record
same zone          ~160 µs      7       0            1129                    114
distinct zones     ~147 µs      7       2            1129                    114
```

The seven values written for one record:

```text
history/<zone>/<seq>            153 B  JSON  the mutation: the record again, request id, seq
live/<zone>/\x01<id>            110 B  JSON  the record
live/<zone>/\x02<request id>    222 B  JSON  the receipt (its digest is 32 JSON numbers)
live/<zone>/sequence              8 B
meta/data-usage                  60 B  JSON  rewritten by every insert of a commit
meta/planning-stats-v1/<zone>    48 B  binary
operations/<request id>         222 B  JSON  the same receipt, node-wide
```

Four JSON encodings (record, mutation, receipt, usage). A new zone adds two
buckets (`live/<zone>`, `history/<zone>`); data with about one record per
zone (552 959 zones for about 570 000 records) pays that on every insert.
The disk holds about ten times the record. In the CPU profile of this
benchmark `json.Marshal` is 7 %, the bbolt commit 24 %, the puts 10 %, the
authority check before the write lock 9 %.

Nothing is ever removed: the history keeps every mutation and the two receipt
tables keep every request id.

## Format

### Record value (IXR1)

Little-endian. Collection, location and id are in the key and are not
repeated in the value.

```text
0   u8        version = 1
1   u8        flags: bit 0 deleted, bit 1 payload present
2   u8        m, number of metrics
3   u8        0
4   u32       payload bytes p
8   f64       weight (1 for a plain record)
16  [m]f64    metrics
..  [p]u8     payload
```

A record with five metrics is 56 bytes (114 in JSON). `count = Σ weight`
becomes one more additive sum of a zone, so a pre-aggregated record (a
weather cell, a 2 m² surface cell) is one record, not `weight` records.

### Receipt value

```text
0   u8        version = 1
1   [3]u8     0
4   u32       unix day of the write
8   u64       seq
16  [32]u8    digest of the record value
```

48 bytes (222 in JSON). Collection and location are the key's.

### Flat keys

One bucket per kind, no bucket per zone:

```text
records     zone (12 B) | depth (1 B) | id            → IXR1 record
receipts    zone (12 B) | depth (1 B) | request id    → receipt
zones       zone (12 B) | depth (1 B)                 → seq u64 | planning counters (48 B)
history     zone (12 B) | depth (1 B) | seq u64       → request id   (only while a transfer needs it)
requests    request id                                → zone (12 B) | depth (1 B)
```

`zone | depth` is `ID(z)` of ID-1 plus the location length; ZONE-ID-1 says a
parent and its descendant can share an id, the depth byte keeps them apart.
Keys of one zone stay next to each other: a zone read is one range scan.
A read of everything below a zone is one range scan too, but only if ids
sort like locations; that needs the ID-1 codec gap (`MergeEncodings`,
invariants.md) closed first. Until then the key keeps today's
`collection \0 location` in place of `zone | depth`.

## Expected gain

From the table above, for the same insert:

```text
              today      IXR1 + flat keys
puts            7            5            (history only during a transfer; usage once per commit)
buckets/zone    2            0
disk bytes     1129     250 to 330        (values 56 + 48 + 56 + 13, keys of 9 to 35 B)
JSON encodes    4            0
```

Between 3.4 and 4.5 times less disk per record, before any receipt expires. Time: the encodings are 7 % of an insert and the per-zone buckets
are part of the 10 % of puts and of the 24 % commit (each bucket is a page of
its own to spill); a 15 to 25 % shorter insert is a fair expectation, to be
measured with the same benchmark. Reads gain more: a zone scan no longer
opens one bucket per position.

## Bounded history and receipts

This was step 3 of the node plan and is **not shipped**: no bound was found
that is both useful and provable as a local change. What blocks it:

1. **The guarantee is stated without a window.** consistency.md: receipts and
   tombstones move with the data, "so retrying an old insert cannot undo a
   newer delete". Forgetting a receipt makes a late retry a new write: it can
   put back an old value over a newer one. A window needs a second guard (the
   retry carries the seq or version it expects) or an accepted, written loss.
2. **Receipts are certified bytes.** `Snapshot.Seal` hashes the receipts of a
   zone, and the receiver's mirror refuses a mutation whose request id it
   already holds (`mirrorZone`, `ErrConflict`). If the source forgets a
   receipt that a warm copy still holds, a valid later write with that id
   stops the handoff. Pruning must be the same function of the zone state on
   both sides and inside staging: a change to HND-2.
3. **History has a reader the source cannot see.** `copyAndMirror` replays
   `(progress of the receiver, frozen seq]`; a missing entry is
   `ErrSequence` and the handoff does not complete. The receiver's progress is
   not on the source's disk, and the warm snapshot is read before the transfer
   intent is written (`captureCausal`), so "log only during a transfer" needs
   the capture reordered and a fallback that restages the zone.
4. **A window per zone bounds nothing here.** With one record per zone each
   zone keeps its one receipt and its one mutation. Only a node-wide or a
   time window shrinks the store, and time has to be in the receipt (the
   `unix day` above): a disk format change.

Proposed rule, to decide with the format:

- **History**: written only while an outgoing transfer covers the zone, from
  the seq of its first snapshot; deleted when the transfer installs or
  aborts. A range that is gone restages the zone. Bound: writes during a
  transfer.
- **Receipts**: kept `R` days (proposal: 7). A retry inside the window is
  answered as today. Outside it the write is refused unless it carries the
  zone seq it read, so an old insert still cannot undo a newer delete; a
  blind late retry gets a conflict, never a silent overwrite. Expiry is by
  day buckets, applied the same way to live and staged zones, and the seal
  hashes only unexpired receipts as of the frozen day.

## Migration

`meta/storage-format` is `indexus-flat-1` today and a node refuses any other
value. IXR1 is `indexus-flat-3`: no in-place upgrade, no mixed read path.
A node is rebuilt with the offline export (`localdata -export`, the
path `explore.sh` already uses to translate an old base) and rejoins; in a
mesh, node by node through the normal join handoff, which carries records as
values and not as disk pages. Snapshots and mirrors on the wire keep their
JSON form in a first step, so old and new nodes can hand off to each other;
only then does the seal move to IXR1 bytes, on a cluster-wide switch.

## Invariants touched

| ID | Why | What must be shown again |
| --- | --- | --- |
| ID-1, ZONE-ID-1 | zone id and depth become the disk key | coincident parent and child stay distinct zones |
| HND-2 | history only during a transfer; receipts expire | writes during copy, replay, lost install ACK; restage when a range is gone |
| AUTH-4, REC-1 | new format marker | restart and wrong-format refusal |
| ZONE-1, MIG-1 | staging uses the same flat keys | per-zone ACK exclusion and GC |
| AGG-1 | weight enters the sums | `count = Σ weight`, delta from the replaced record |
| new: RETRY-1 | the retry window | same id and bytes inside `R` days return the first result, across handoff; outside, never a silent overwrite |
| WIRE-1 | none | the IXB2 body does not change |

The disk digest test (`TestDiskDigestOfFixedMutationSequence`) pins today's
bytes and gets new digests with the format.

## Local changes that keep today's bytes

Not done in this step (it stopped at the bound above), listed for whoever
takes the write path next. Each leaves the committed bytes identical, which
the disk digest test checks:

- write `meta/data-usage` once per commit instead of once per insert of the
  group (one JSON encoding and one put less per insert in a batch);
- build the mutation as `{"RequestID":…,"Seq":…,"Record":` + the record
  bytes already encoded (one encoding less);
- write the receipt bytes by hand (fixed field order) instead of reflection.

Measured ceiling: all JSON encoding is 7 % of an insert, so these save a few
percent; the format is where the cost is.
