# Query index

**Statut : actuel.**

This page is the logical view a client can build. It is not a kernel proof and not a global snapshot. The proofs of ownership and handoff do not depend on it.

## Two maps

```text
Logical view     the GPS tree the client walks
Routing topology   local V_active, the predicted XOR owner of each zone
Write authority    installed local native/point rights and confirmed tickets
```

They are not the same object. A child marker says a child exists. It does not say who owns it.

## What the client keeps

```text
frontier
visited
pruned
cursors
bounds
results
```

A walk that returns to a visited node is an error, not an empty answer.

## Aggregates

Parents are derived caches, not a second write log and not a grant of authority.

```text
Count(P) = Direct(P) + sum over children Count(child)
```

A child contribution is versioned (`child id`, generation, revision, holder, count,
sum). The parent replaces that contribution. Applying the same revision twice is
the same as applying it once. `parentCount += delta` without a version is not
allowed: a replay would double-count. Old authority cannot resurrect totals.

`GetAggregates` answers only for locations this vnode currently owns. Clients
route to the XOR owner of the parent they want to read. Summaries used to skip a
branch are advisory bounds, not certificates.

After a durable local apply, the written zone folds the delta at once and is
marked written; same-vnode ancestors fold on the next Update, in one
deepest-first pass where a shared ancestor is recomputed once, with no RPC and
no extra fsync. The write path never walks ancestors under the vnode's summary
lock. The delta comes from the record the write replaced, so a write never
rescans its zone. Only the invocation that
applied the write folds it: a relayed response naming this vnode as applier is
not folded again. The HTTP 201 does not wait for remote parents. A handoff
install is also a local durable commit and folds the mounted zones at once;
because a joiner publishes its activation only after its first install
(ACT-1), the regions installed before that publication are not writable yet
and the install-time fold sees nothing for them, so the activation folds the
inventory again.

A topology change moves parents between vnodes while their children did not
change, so nothing would push those children to the new parent owner before
the slow poll does. Every vnode therefore refreshes its cached view whenever
the replica's active set changes, on writes too, and re-pushes the owned zones
whose parent owner moved. The parent side refreshes its view before judging a
contribution, so a child pushing right after a handoff is not dropped as a
stale holder by a view that predates it.

Folds and deltas are reconciled through the zone's own sequence numbers: a
delta whose sequence a snapshot already covers is skipped, and a snapshot older
than an applied delta is skipped until the next fold, so every interleaving of
commits, deltas and inventory reads counts a row once. Another vnode is treated as remote until an authenticated MachineID
exists: the child pushes its latest absolute value, coalesced to at most one
attempt per second per child→parent edge, with ACK and retry of the latest
pending value. Pending boundary edges for the same destination are sent in
batches of at most 512 contributions, so diffuse locations do not create one
RPC per committed row. A parent that changes then applies the same rule toward
its parent.

Periodic `Update` is the repair path. Every tick flushes pending pushes.
Boundary polling runs at a slower one-minute cadence and for three consecutive
ticks after an authority change, since peers merge that change at their own
pace; durable inventory folding runs at startup, on authority change, on
activation, and at a five-minute safety cadence.
Capacity observation uses an index of materialised direct zones rather than
walking every derived ancestor. A failed poll keeps the previous totals instead
of publishing zero. Repeated refreshes cannot double-count; deletion of the
last record clears ancestors to zero. Metrics remain float64, not exact
decimals. There is no bounded freshness during a partition.

See the traffic measurements and the child-push design model (archived outside the repository)
for measured former all-to-all inventory amplification, scheduling limits, and
the locality windows.

Bottom-up exactness is the sum above, computed from the children the client
actually read.

## Keys of a zone

One rule, whoever answers. A `/sets` read of zone `Z` carries:

```text
Z          the records placed exactly at Z
L          when every record below the child Z+c sits at one location L:
           L, cut 4 characters below Z
Z+c        otherwise: everything below the child one character c under Z
```

A lone record at `aaZ9` is the key `aaZ9` in a read of `aa` and of `a`; in a
read of the root it is `aaZ9` too (4 characters). A lone record at `pQrStUv`
is `pQrS` from the root, `pQrSt` from `p`, and `pQrStUv` from `pQrStU`.
Two records at `xy12` are one key `xy12` with count 2. Records at `aac` and
`aacK7` make the key `aac`. So a key deeper than one character always means
"all of this child sits in that cell", which the SDK walk uses to split a
coarse cell without loading the child zone (`MAX_VIRTUAL_CHILD_DEPTH_DELTA`
= 4 binary levels: 4 characters, 24 bits, cover them for up to 4
dimensions). The key width of a block stays bounded at 4 characters.

How each answer knows it:

- A scan (a relay, or the owner with `refresh=true`) groups the holders'
  records by child character and keeps a group's location while it is the
  only one.
- The owner's summary is a compressed tree whose nodes are record
  locations, branches and ownership boundaries. For each child it walks down
  while exactly one part (the node's own records or one non-empty child) is
  left, and stops at the first split: a chain of at most 16 nodes, no scan.
  A child held by another vnode is a boundary: its holder sends the location
  with its contribution (`Contribution.Exact`, empty when there are
  several), computed by the same walk on its side.

So the v1 and IXB2 bodies of a zone are the same from its owner and from a
relay, byte for byte (`TestZoneKeysAreTheSameFromOwnerAndRelay`, three
nodes), with three limits: the owner's summary of a parent is an advisory
cache that lags its children until they push (see Aggregates); a peer that
does not send `Exact` makes its parent answer `Z+c` for that child until it
is upgraded; sums are float64, so a summary and a scan that add inexact values
in different orders can differ in the last bit. `/set` (one zone as JSON)
uses the same keys and no longer lists record ids.

## Stale topology

A hint can name an old owner. The serving node follows installed local permissions and current scoped referrals (`ROUTE-1`). Membership converges separately; inventory RPCs do not piggyback a global authority graph. The hint itself never grants write authority.

## Not a snapshot

Reading a parent and then its children can straddle a handoff. That is per-zone consistency. It is not one instant of the whole tree. An explicit watermark, if it is added later, has to be a separate request. It is not implied by a normal walk.

The mesh read path checks local fences, installed permissions and coverage in the same
read transaction as its inventory. A holder without installed coverage cannot
report a complete empty shard; an installed point may serve before its logical
endpoint is published. Unknown coverage is not empty coverage. Pages retain their
control/row watermarks and required referrals; a changed attempt is retried. This does not provide snapshot isolation across concurrent writes to different shards.

## Local successor referrals

Inventory RPCs carry no global Knowledge and have no full-bundle fallback.
Each page names required incoming sources, open point receivers and durable regional successors from the
same MVCC snapshot as its rows. A query follows the referrals recursively;
a failure at any required successor discards the result. Referred disks use
`InventoryPageRequest.Delegated`: they serve their local readable copies,
including a frozen intermediate copy and early point installs, without bouncing
back to the incoming source. Page watermarks and whole-attempt retries remain.

A regional emission writes its successor atomically with cession. Regional
installation clears successors inside the acquired region atomically with
mounting. These pointers survive operation receipt compaction; exact sibling
regions with the same successor coalesce. They never grant write permission.

The active runtime uses membership as a route hint and the current local
exception directory for required successors. Empty delegations migrate during
physical MOVE and logical retirement. There is no known global authority
frontier to download before reading. See [local permissions](local-permissions.md)
and their [qualification](local-permissions-verification.md) for the current
coverage contract; [read delegations](read-delegations.md) preserves the
historical design sequence.

### Direct records at a single address

`GET /records/direct?collection=C&location=L&limit=N` reads the live records
stored at exactly `L` on the addressed peer. The read scope is required.
The root `@` addresses records stored at the root itself. A single local
storage snapshot supplies both the installed, unfrozen write permission for
that address and its rows. The request works without a materialized summary.

A successful reply contains a `records` array, in ascending document ID byte
order. Each entry includes `collection`, `location`, `id`, and `metrics`;
an omitted `weight` represents one. Empty arrays encode as `[]`. Payloads
are omitted. The required limit ranges from 1 to 10,000. If the live result
exceeds the limit or the scan exceeds its 64 MiB budget for estimated metadata and any stored
opaque bodies needed to decode it,
the peer returns 413 and no rows. An invalid request returns 400, a peer
without installed authority returns 409, and a frozen owner returns 503.
The caller chooses the next peer. Each call sees its own snapshot.

### Immediate children observed on one peer

`GET /zones/children/local?collection=<name>&location=<prefix>` requires read
access. It returns e.g. `{"children":["a0","a_"],"complete":false}`. At root
(`@`) the addresses have one character. A leaf returns an empty list. Rows
placed directly at the requested prefix do not create a child.

The peer probes its own store for witnesses under immediate children. Treat
the array as traversal hints: additional children can exist locally or on
other peers, and a reported child can reflect a stale copy during handoff.
An empty array also carries `complete:false`. Exact readers need a separate
coverage mechanism. The request never fans out, follows referrals, warms the
summary cache or retains a cursor. The response sets `Cache-Control: no-store`.
### Local hints for direct child prefixes

`Store.ObserveChildPresence` reports two 64-bit masks indexed by the routing
alphabet. For a child in `seen`, this disk has a counted record at or below
the child at the observation's snapshot. A retained source copy during
handoff can supply that witness after another owner has deleted the record.
The parent itself is excluded. An `unknown` child had an inconclusive probe,
for example a tombstone ahead of a live key, a memtable update newer than the
snapshot, or an older segment outside the probe budget. A child with neither
bit has no observed physical key and all local sources for it were checked.
Other peers can hold children missing on this disk. At maximum address depth
there are no children.

The probe seeks once per child in each of the two memtables and in up to
eight recent immutable segments, stopping on the first source with a
physical candidate. It reads key heads and flags. It does not advance to the
next document, chase MVCC version chains, decode metrics, or fetch payloads.
The pinned view copies at most eight segment references. Across 64 children
there are at most 512 segment range checks and as many indexed seeks.
Skiplist navigation, segment indexes, compressed page sizes, cache misses
and snapshot registration/reclamation affect elapsed time. Cancellation is
checked between child ranges and segments; an I/O already in progress can
complete before cancellation returns.

`ZoneChildren` provides full local counts, weights, metric sums and unique
locations. In memory those totals fold the descendant rows. On disk they
use segment sums, and determining a unique location among retractions can
require a further key walk.
