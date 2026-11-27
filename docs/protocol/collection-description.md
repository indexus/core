# Collection description (IXC1)

**Statut : actuel.**

Status: proposal, v1. Prototype and tests: `internal/algebra` (`space.go`,
`descriptor.go`, `name.go`). Revised 2026-11-19.

A collection is fully described by a short binary object, the same for every
node and every client; its name is the hash of that object. The description
is mathematical only: how points become addresses, how zones aggregate,
where records sit. Never what the data means.

## 1. Principles

1. **Minimal.** The description holds what is needed to compute: turn a
   point into an address, an address back into values on each axis, and
   aggregate zones. Nothing about how values are read, drawn or projected.
2. **Agnostic.** No names, units, labels, sources, projections or payload
   formats. Those belong to an optional companion document (section 7).
3. **Exhaustive.** The encoding is stated bit by bit: no implicit default a
   client must know.
4. **Self-certifying and immutable.** The name commits to everything;
   changing anything gives a new collection.
5. **Faithful.** Today's SDK is one instance of the standard: 1,032 vectors
   produced by `sdk-js/src/tree` on six collection shapes in use are
   reproduced bit for bit (`testdata/space_vectors.json`).

| Part | Content |
| --- | --- |
| Geometry | c bits per character, W characters per address |
| Nonce | 16 random bytes, for uniqueness only |
| Record depths | records sit at depths [lo, hi] |
| Space | k axes, each a root interval [min, max]; the schedule: the axis cut by each of the c·W bits |
| Schema | the aggregation of every column (`sum`, `min`, `max`, `first`, `last`, `affine`), the same at every depth |

## 2. The encoding, mathematically

A collection has k axes (k ≥ 0; k = 0 is section 2.5). Axis a has a root
interval [min_a, max_a] (finite, min_a < max_a), Δ_a = max_a - min_a. The address has W characters of c bits,
N = c·W bits in all.

**Quantisation.** After n cuts, axis a is split into 2^n dyadic cells; a
coordinate x falls in

```text
j_a(x, n) = clamp( ceil( 2^n (x - min_a) / Δ_a ) - 1 , 0 , 2^n - 1 )
cell      = ( min_a + j Δ_a / 2^n , min_a + (j+1) Δ_a / 2^n ]      (the first cell includes min_a)
```

**Schedule (the mask).** A word σ of N axis indices: bit k of the address
cuts axis σ(k). It is stored in full, one byte per bit, so any mask can be
written explicitly: "this bit is this axis". Every axis must appear at least
once. Let n_a(d) be the number of times axis a appears in the first c·d
entries. The address bits are the bits of the cells j_a(x, n_a(N)),
interleaved by σ, most significant first: bit k is the next unread bit of
j_{σ(k)}.

**Rotation.** Character l of the address is

```text
A[ (b_l + rot(l)) mod 2^c ],   b_l = the c bits of level l,   rot(l) = index in A of name[l mod 16]
A = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_", first 2^c characters
```

The rotation spreads collections over the XOR id space, so that the same
place in two collections lands on different owners. It is a bijection at
each level: it changes the names of the children, not the tree.

**Cell.** The address without rotation (the bits b_l). Two collections with
the same Space have the same Cell for the same place; equal space digests
(section 4) tell a client a cell-by-cell join is valid.

### 2.1 Computing it: bisection, and its exactness

Clients and builders compute the address by bisection in IEEE 754 binary64,
which is what the SDK does: at bit k on axis a = σ(k), mid = (lo + hi) / 2;
bit 1 and lo = mid if x > mid, else bit 0 and hi = mid.

Bisection equals the exact quantisation above as long as every cell
boundary is a binary64 value. That holds for n cuts if and only if

```text
M_a ≤ 2^(53 + e_n),   e_n = min( v2(min_a), v2(Δ_a) - n ),   M_a = max(|min_a|, |max_a|)
```

where v2 is the 2-adic valuation (min_a = 0 counts as infinite). The largest
such n is the **dyadic level** D_a of the axis. A description is valid only
if n_a(record depth) ≤ D_a for every axis: within it, the address of a point
is the same mathematical object on every platform.

| Axis in use | D_a (exact cuts) | Cell at D_a |
| --- | --- | --- |
| latitude [-90, 90], longitude [-180, 180] | 48 | 6.4e-13 degree |
| lidar x [331000, 333048] / y [6276000, 6278048] | 45 / 41 | 5.8e-11 / 9.3e-10 m |
| time in seconds [1.7e9, 1.9e9] | 31 | 0.09 s |
| time in ms, 1990 to 2100 (Chronicle) | **21** | **27.6 minutes** |
| time in ms, [0, 2^42] (about 139 years) | 53 | 0.5 ms |

Measured by sampling and by the formula, the two agree on every axis above.
**Choosing bounds is a design act:** Δ a power of two, and min a multiple of
Δ / 2^k, keep D near 53. Past D, bisection stays deterministic, but cells
are no longer exact halves and the encoding depends on float rounding. The
standard refuses it.

### 2.2 From an address back to values

1. Remove the rotation: b_l = (index(location[l]) - rot(l)) mod 2^c.
2. Distribute the bits with σ: this gives j_a and n_a per axis, an integer
   cell. No float arithmetic in this step.
3. The value interval on axis a is (min_a + j Δ_a / 2^n, min_a + (j+1) Δ_a / 2^n]
   (computed by replaying the bisection; exact within D_a). Its centre is
   the representative value.

What the client does with those values (a map, a timeline, a sphere, a
chart) is outside the description.

### 2.3 Order

Ordered columns (`first`, `last`, `affine`) are folded in **cell order**:
children ranked by their unrotated bits (`ChildRank`), not by their
characters. With the rotation, character order (the storage order of
location strings) is a different order; a test folds a time series both ways
and only cell order gives time order.

Cell order is the order of the interleaving: inside a zone of depth d, it is
the order of axis a exactly when σ cuts only a below c·d
(`OrderedAxis`). A schedule that cuts space first and time last makes every
deep zone a time series, so ordered columns there mean "in time".

### 2.4 Schedules

| Schedule | Written as | What it gives |
| --- | --- | --- |
| Round-robin, today's only one | (0 1 … k-1) repeated | uniform refinement; the SDK walk's "step" c/k per character |
| Space then time | (0 1)^30 then 2^36 | each zone of depth 10 holds a time series of its place |
| Coarse time first | 2^6 then (0 1 2) repeated | top levels split by period, then space-time |
| Anisotropic | (0 0 1) repeated | axis 0 twice as fine as axis 1 |

The SDK walk handles only schedules where every character cuts every axis
the same number of times (`Step`). Others are valid descriptions; the walk
must learn to read per-character cut counts from σ before clients can draw
them.

### 2.5 Opaque addresses

A space with no axes has no schedule: the writer chooses every character of
an address, and an address decodes to nothing. Aggregation is unchanged. This
is the case of a routing by centroids (a vector goes down to the child whose
centroid, a sum divided by a count, is nearest): the tree, the owners and the
folds are the same, only the meaning of the characters is the writer's.

## 3. Canonical bytes

Little-endian, no padding. Decoding must re-encode to the same bytes,
otherwise the object is refused.

```text
"IXC1"  u8 version = 1   u8 c   u8 W   u8 0
[16]    nonce
u8      record depth lo   u8 record depth hi
u8      axes        per axis: f64 min  f64 max
[c·W]u8 schedule    the axis cut by every bit of the address (absent when axes = 0)
u16     runs        per run: u8 code  u16 count
                    code 0 sum, 1 min, 2 max, 3 first, 4 last: count columns
                    code 5 affine: count pairs (affine.a, affine.b)
```

The schema is written as maximal runs: two neighbouring runs never share a
code, a run is never empty, and a schema has at most 4,096 columns. An
embedding of 768 dimensions summed per dimension is one run of 3 bytes.

Floats are encoded by their IEEE 754 bits; 0 and -0 differ; NaN and
infinities are refused. A collection with three axes (latitude, longitude,
time) and five columns is 185 bytes, most of it the schedule; a collection with
opaque addresses and 768 sums is 32 bytes.

## 4. Names and digests

| Identifier | Definition | Purpose |
| --- | --- | --- |
| Collection name | first 96 bits of SHA-256("indexus.collection.v1\0" ‖ IXC1), 16 characters of A | addresses the collection; commits to everything |
| Full digest | all 256 bits | what nodes keep; detects a forged collision |
| Space digest | SHA-256("indexus.space.v1\0" ‖ axes ‖ schedule) | equal digests: same cells for the same places |

Forging two descriptions with one 96-bit name costs about 2^48 work and only
harms the forger's own collection; stealing an existing name costs about
2^96. Nodes refuse a second description for a name they hold.

## 5. Aggregation

One schema for the whole tree: every zone, at any depth, combines its
contents column by column with the column's kind (`docs/design/typed-columns.md`):
`sum` (with removal), `min`, `max`, `first`, `last`, and the ordered pair
`affine.a`/`affine.b`. A record is one value of that schema; a zone is the
fold of its contents. No change of operation between levels in v1.

**Order of a fold.** A zone folds its records and its child zones in cell
order: each element is keyed by its location with the rotation removed, one
rank per character (`CellOrder`); a prefix comes before its extensions, so a
record placed at a zone comes before everything below it, and a child zone
stands at its own location. Records at one location are folded by record id.
Commutative columns (`sum`, `min`, `max`) do not depend on the order;
ordered ones (`first`, `last`, `affine`) do, and with this rule the value of
a zone does not depend on where records are held (sparse storage, records
next to child zones, vnode boundaries): tested on 1,000 random layouts. One
invariant keeps the order total: a record is never held outside the child
zone that covers its location (a split moves them all down).

## 6. Circulation

A description is self-certifying and immutable, so the catalog of a cluster
is a grow-only set. Implemented in `internal/storage/collections.go`,
`internal/mesh/collections.go`, `internal/summary/typed.go`:

| Rule | Implementation |
| --- | --- |
| DESC-1, no fold without the description | a collection with a description whose schema is not all `sum` takes the typed fold; one without is legacy (sums). A write or an IXP1 batch whose width or record depth does not fit the description is refused |
| DESC-2, admission | `PUT /collection/<name>` with the IXC1 bytes: verified (hash = name), kept, then sent to every host of the routing view; acknowledged only once every host holds it (a retry is safe). A second, different description for a held name is refused (409) |
| DESC-3, transfers | before any zone goes to a disk (handoff, shadow copy), the donor sends it its whole catalog in one request |
| DESC-4, contributions | a child's contribution carries the hex SHA-256 of the description; a parent lacking it fetches it from the child and verifies it; a different digest is dropped and counted, never merged |
| DESC-5, readers | `GET /collection/<name>` returns the IXC1 bytes; the client checks them against the name itself |

## 7. What is not in the description

Everything about meaning and presentation: names of axes and columns,
units, which axes are a latitude and a longitude, whether an axis wraps
around, how to project or draw, what payloads contain and how they are
encoded.

That information can travel in a **companion document**, published by the
issuer (or anyone) for a collection name. It is outside the hash: it can be
corrected or extended without changing the collection, and several
companions may coexist for different applications. A separate norm can fix
its format later; this standard does not depend on it.

## 8. A rendering for humans (non-normative)

```json
{
  "name": "…16 characters…",
  "geometry": { "bitsPerChar": 6, "width": 16 },
  "records": { "depths": [10, 16] },
  "space": {
    "axes": [ [-90, 90], [-180, 180], [0, 4398046511104] ],
    "schedule": "(0 1)^30 2^36"
  },
  "schema": [ "sum", "min", "max", "affine.a", "affine.b" ]
}
```

The schedule is written as a word with exponents for readability; the bytes
always hold every entry.

## 9. Compatibility

- An SDK collection definition maps to the standard directly: a `linear`
  dimension is one axis; a `spherical` one is two axes, latitude then
  longitude (its distance and drawing rules stay in the client, or in a
  companion document); the schedule is round-robin; the rotation is the
  name's. That is how the 1,032 vectors are reproduced.
- Collections named by hand today are not self-certifying; they read as
  legacy (one column of sums). Moving one to the standard is a re-ingest into
  a hash-named collection.
- The SDK's mask is indexed with a hard-coded 6 (`Mask.at`): with another
  base it must use c.

## 10. Open decisions

1. Bounds of existing collections: Chronicle's time axis is exact for 21 cuts
   only; a re-ingest with dyadic bounds would give 53.
2. The walk for non-uniform schedules (2.4).
3. Name width: 96 bits (16 characters) or more.
4. The format of companion documents (section 7), if one is wanted.
5. Different operations per range of depths (projecting columns away high
   in the tree, learned summaries): left out of v1 on purpose; a later
   version would add it without changing v1 names.
