# Typed summary columns (B8)

> **Conception historique — contexte du 25 novembre 2026.** Les mathématiques
> et choix de conception sont conservés ; les statuts de branche et descriptions
> du transport appartiennent à leur révision d'origine. Pour le contrôle actif,
> lire [permissions locales](../protocol/local-permissions.md) et leur
> [qualification](../protocol/local-permissions-verification.md).

**Statut : proposé.** Conception des colonnes typées (DESC-*).

Status: wired in on branch `feat/collection-description` (worktree
`_work/core-desc`), see "Implementation" at the end.
Written 2026-11-19.

## Why

Every column of a zone summary is a sum today (`wire.Abelian`, an abelian
group). The propagation protocol does not need that: a parent replaces the
versioned contribution of each child, it never applies an unversioned delta
(`docs/protocol/query.md`). Any monoid therefore converges. Typed columns give:

| Kind | Algebra | New capability |
| --- | --- | --- |
| `sum` | abelian group (today) | unchanged |
| `min`, `max` | semilattice | exact extremes per zone, certified bounds, exact top-k in low dimension |
| `first`, `last` | ordered monoid | first / last value of a time range (open / close) |
| `affine` (a, b) pair | ordered monoid | state of a linear recurrence `h = a*h + b` over any prefix: exponential moving averages, linear filters, compounding, SSM state |

## What the prototype proves (`internal/algebra`, `go test -race`, fuzzed)

- Identity and associativity for every kind; `first`, `last`, `affine` are not
  commutative and must be combined in address order.
- Fold invariant and cut independence: on random base-4 trees with records at
  leaves and interior zones, any cut read in address order equals the root.
- Convergence: a register that keeps the newest version per child converges
  under duplicates, reordering and two replicas merged in either order.
- Removal: `max` needs a zone rescan only when the removed record held the
  max; measured 7.19 rescans per 2,000 removals on average (theory H_n - 1 = 7.18).
- `affine`: the state after any of 4,096 prefixes is read in at most 12 zones and
  matches the sequential scan.
- `min`/`max` are bit-identical in any order; float sums are not (Neumaier
  bounds the error). Audits need a tolerance for `sum` and `affine` only.
- Cost of `Combine` on 8 columns: 41 ns (sums), 55 ns (sum/min/max mix),
  229 ns (four affine pairs), one allocation each (Linux container, 2 vCPU).

## Where sums are assumed today

| Place | Assumption | Change |
| --- | --- | --- |
| `internal/wire/aggregate.go` (`Abelian`) | `Sum` / `Substract` with Neumaier compensation | keep as the fast path when every column is `sum`; delegate to `algebra.Schema` otherwise |
| `internal/wire/set.go:79` (`Put`) | replaces a key by subtract old, add new | if the schema is not invertible, refold the block's children in key order (at most 64) |
| `internal/summary/store.go:52` (`correct`) | subtract local value, add remote | recompute the part from its contributions in child order |
| `internal/summary/fold.go:112` (`recordDelta`) | a write is an add plus a remove | when `Remove` reports rescan columns, queue the zone for the existing authoritative refresh (inventory read) |
| `internal/zonelog/record.go:97` | per-zone sums as Neumaier pairs in segment indexes | add per-column min/max (immutable segments make them exact); fold `first`/`last`/`affine` in key order at segment build; tombstone of an extreme triggers rescan or waits for compaction |
| `internal/prep` and `tools/prep` (IXP1 `zones.ixp`) | offline zone sums | compute with `algebra`; manifest gains `kinds` |
| IXB2 (`docs/protocol/wire-block.md`) | metric columns are sums | new section kind 4 "column kinds" (one u8 per column); old clients skip unknown kinds, rows are unchanged |
| `sdk-js/src/tree/abelian.js` | `abelianSum` / `abelianSubtract` | combine by kind; same golden files as the Go encoder |
| `internal/wire/set.go` iteration, `summary` parts | maps, order irrelevant for sums | ordered kinds need children in key order: sort keys before folding |

## Order of work

1. Declare `internal/algebra` in `internal/architecture/boundaries_test.go` (standard library only).
2. Collection schema: `kinds` in the IXP1 manifest and the collection registration; absent means all `sum` (backward compatible).
3. Wire and summary layer (`Abelian` delegation, `Set.Put`, `correct`, sorted children).
4. Write path rescan through the authoritative refresh; test that a deleted max is gone after the next fold.
5. `zonelog` segment index and IXP1 offline zones.
6. IXB2 section 4 and the SDK, with shared golden files.
7. Audit: exact comparison for `min`, `max`, `first`, `last`; tolerance for `sum`, `affine`.

Rough effort: 3 to 5 weeks for one engineer, steps 3 to 5 being most of it.
Steps 1 to 3 alone already serve `max`/`min` on bulk-installed collections.

## Collection description, named by its hash

Today a collection exists nowhere as an object: a 16-character name, a Space
defined by the client, and a metric count in the IXP1 manifest. Nodes cannot
know how to aggregate, so they sum.

A collection gets a **description**: purely mathematical, no column names, no
units, no business meaning (that metadata lives elsewhere, keyed by the name).
Prototype: `internal/algebra/descriptor.go` and `name.go`.

| Field | Content |
| --- | --- |
| geometry | bits per character, characters per address |
| nonce | 16 random bytes: two collections with the same algebra stay distinct |
| space | axes [min, max] and the schedule (`docs/protocol/collection-description.md`) |
| record depths | records sit at depths [lo, hi] |
| schema | the kind of every column, the same at every depth |

**The name is the hash.** `name = first 96 bits of SHA-256("indexus.collection.v1" || IXC1 bytes)`,
printed in the routing alphabet: a valid 16-character collection name.

- Self-certifying: any node checks a description received from anyone against
  the name, without an authority or a signature.
- Binding: changing any operation, bound, depth or the nonce changes
  the name, hence every zone key. The operations of a collection cannot change
  under its name; an audit recomputes zones from records with the description
  the name commits to.
- One encoding: IXC1 is canonical (decoding re-encodes to the same bytes, or
  is refused) and is also the transfer format (32 to 185 bytes in the tests).
- Collisions: 96 bits give about 2^48 work to forge two descriptions with one
  name, which only harms the forger's own collection; taking over an existing
  name needs a second preimage, about 2^96. Nodes keep the full 256-bit digest
  and refuse a second description for a name they hold (`ErrCollision`).
  Wider names (the width is a parameter) would raise the bar to 2^64.

Tested (`go test -race`, decoder fuzzed): deterministic names; every
mathematical change gives a distinct name; a flipped bit anywhere, trailing
bytes or a wrong name are refused; the catalog is idempotent and detects a
colliding description. Sparse storage (records held at any depth next to
child zones, folded in address order) gives the same value at every zone on
1,000 random layouts.

v1 has one schema for every depth. Different operations per range of depths
were prototyped (projections, opaque learned steps) and left out to keep v1
small.

### How the description follows responsibility

The description is control state, stored as records of the reserved control
collection, committed with data in the same `zonelog` frame. Rules, to become
named invariants:

- **DESC-1 (no fold without description)**: a node folds, acknowledges or
  serves a zone only if its catalog holds the verified description of the
  zone's collection.
- **DESC-2 (genesis)**: the first write of a collection, or an IXP1 batch,
  carries the IXC1 bytes; the node verifies hash = name and admits them in the
  same transaction as the data.
- **DESC-3 (transfer)**: a MigrationSet ships the description of every
  collection it touches, once, in its prepare phase; `install-zone(z)` admits
  it in the transaction that mounts `z`.
- **DESC-4 (parent push)**: a child contribution carries the 32-byte digest;
  a parent missing it pulls the bytes from the child, which holds them by
  DESC-1. A digest mismatch is an alarm (collision or bug), never a merge.
- **DESC-5 (readers)**: `GET /collection/<name>` returns the IXC1 bytes; the
  SDK verifies them itself. No reader trusts a node for the algebra.

Legacy collections (human-readable names, no description) read as one
column of sums. Moving one to typed columns is a re-ingest into a hash-named
collection, as a new version already is today.

The Space (axes, root intervals, schedule) is part of the description too: the full standard, with the normative
encoding checked against the SDK, is `docs/protocol/collection-description.md`.

## Open questions

- Width: 128 metrics stays the cap per record; an `affine` pair uses two.
- `first`/`last` on ties at one location: insertion order (record id) is the tiebreak; it must be the same on every node.
- Different operations per level (and learned functions) are out of v1: they
  would make only boundary-aligned cuts valid.

## Implementation (branch `feat/collection-description`)

| Part | Where |
| --- | --- |
| Algebra, description, names | `internal/algebra` (pure, declared in `boundaries_test.go`) |
| Catalog, write and batch checks | `internal/storage/collections.go`, `apply.go`, `bulk.go` |
| Typed zone reads | `internal/storage/zone_records.go` (live records, no payloads) |
| Typed fold | `internal/summary/typed.go`: zone value, child keys, set total; `groups.go` for the authoritative read |
| Admission, transfers, contributions | `internal/mesh/collections.go`; `copyAndMirror` and shadow preload send the catalog; `Contribution.Digest` |
| API | facade `indexus/collections.go`; `PUT`/`GET /collection/<name>` (`internal/adapter/http/p2p/collections.go`) |

The typed fold reads the records of a zone at each read or push (O(n log n));
legacy and all-sum collections keep the zonelog sums unchanged. Not done yet:
IXB2 section 4 (kinds on the wire), the SDK, a row format for more than 255
metrics, per-segment indexes for large typed zones.
