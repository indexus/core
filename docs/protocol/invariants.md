# Invariants

**Statut : registre en cours d’alignement sur les permissions locales, 25 novembre 2026.**
Les règles de sûreté ci-dessous sont le contrat à garder ; leur réalisation de
bout en bout est distincte de leur énoncé. Voir la
[qualification du remplacement](local-permissions-verification.md). Les règles
opérationnelles historiques non encore requalifiées ne valent pas mesure du
nouveau binaire.

Normative names. A test referencing an ID checks that statement only within its
exercised scenarios; passing tests are not a general mathematical proof. Bounded
model exploration must state its bounds and assumptions. The verification
checklist is [verification.md](verification.md).

Safety invariants tolerate zero mismatches. A single counter-example is a bug. Liveness and performance are separate and are marked as such.

## ID-1 — one 96-bit space

Zones and vnodes live in `{0,1}^96`. A `VNodeID` is that ID. There is no intermediate hash.

`ID(z) = bits(location) || bits(collection)` filling the rest of the 96 bits. A location is a prefix of its descendants.

One width: 96 bits are 16 characters of the alphabet `0-9A-Za-z-_` (6 bits each)
and 12 bytes. A vnode name and a collection name are exactly 16 characters; a
location is the root `@` or 1 to 16 characters; so every `ID(z)` is 12 bytes.
`tree.CheckName` and `tree.CheckLocation` enforce it wherever a name or a zone
key is decoded, in Go and in the SDK (`checkName`, `checkLocation`).

Implementation qualification (2026-11-02): `MergeEncodings` does not preserve
the documented collection suffix for some partial-byte depths. This is an open
ID-1 implementation gap, not a change to the target formula. Correcting the codec
requires versioning/migrating persisted addresses; see
[component-boundaries.md](component-boundaries.md#identité-des-zones-et-adresses-coïncidentes).

## ZONE-ID-1 — identity is collection and canonical path

A zone's identity is `(collection, location)`, with the empty location normalized
to root. Its 96-bit ID is a routing address, not a unique zone identifier.
A parent and descendant may have identical IDs; they remain distinct zones with
independent records, barriers, forwarding traces, transfers and activation proofs.
The zero address is a valid zone address. Vnode identity restrictions are separate.
Parent-before-child relations come from paths in the same collection, not from ID
equality or XOR distance. During a partial handoff coincident zones can have
different authority heads, even though their geometric owner in one view is equal.

## OWN-1 — pure XOR ownership

`Owner(z, V) = argmin_{v in V} ID(z) ⊕ ID(v)`.

The only inputs are the zone ID and `V_active`. Capacity, host, heat, network and child markers do not enter this function.

The reference is `internal/testoracle`, which materialises each distance and does not call production `View.Owner` or `XORCompare`.

## TOPO-1 — `V_active` is the only ownership topology

Two views with the same active set return the same owner for every zone. Host placement, routing tables, the peer registry and capacity do not.

`NODE-TOPO-1` is the node form: `Owner` reads the local routing membership. XOR placement and acknowledged contacts are routing hints, never write permissions.

`V_active` is never empty, and two active vnodes never share an id. Duplicate ids are rejected. An empty view has no owner.

## CRDT-1 — merge laws

For compatible honest membership announcements, merge is commutative,
associative and idempotent. Vnode identity is immutable, the endpoint is a max
register, and retirement is irreversible with one fixed redirect. Contradictory
identities/endpoints or cyclic retirements are outside this compatible history;
refusing them is not an arrival-independent Byzantine conflict resolution.
Historical endpoint versions are not retained to audit older forks.

## CRDT-2 — eventual topology convergence

After membership mutation stops and all missing announcements are eventually
delivered, replicas expose the same active set and therefore the same pure XOR
placement. There is no global generation, certificate frontier or global approval
barrier. Retired identity tombstones remain reserved.

## AUTH-1 — a single authority head for each key

For each canonical zone, at most one durable disk may ACK new mutations.
Installed native rights include absent addresses; provisional installed points
are scoped by canonical zone, operation and ticket. Only the actual installed
owner may issue a cession; it retains the exact complementary region. Membership
divergence cannot create write permission. A zero-writer interval is allowed.

## AUTH-2 — a unique successor

A source pins an installed input and emits at most one cession for its unique
operation. It persists its fences and native consumption before receiver
installation. A receiver accepts only its exact confirmed ticket. Source retries
retain operation, region and ticket; local coalescing consumes both installed
siblings and cannot consume an input pinned by an ongoing operation.

## AUTH-3 — only the installed holder ACKs

An ACK requires current native or exact point permission, installed bytes, and
no applicable outgoing fence, rechecked in the mutation transaction. A fence is
bound to its consumed input right identity: it cannot suppress a distinct native
or installed point permission reacquired later on the same geometric region. Neither
active membership nor current published physical placement is an additional
write gate. An early physical receiver may ACK its installed pieces while the
source retains other pieces and membership still advertises the source.

Gossip installs no permission. A closed-ticket replay never remounts bytes or
rights. Native consolidation cannot overwrite updates already ACKed on points.
A stale routing view cannot authorize a writer or turn unknown coverage into an
empty successful read.

## AUTH-4 — restart cannot restore consumed authority

Restart preserves the local sequence, installed rights, confirmed admissions,
emissions and fences together. Cancellation is allowed only before the first
point/region fence, not merely before final regional emission. No timeout or
newer membership reopens fenced permission. A former holder becomes writable
again only through a later durable installation. Rolling back that durable state
requires a fresh storage incarnation, not reuse of old ticket numbers.

## ROUTE-1 — stale knowledge never grants authority

A stale view can route incorrectly. The receiver either holds current durable
authority, forwards along its successor chain, or refuses. It cannot ACK because
its local `Owner` result happens to name itself.

## ROUTE-REGION-1 — an ACK teaches the entry a region, as a hint

A write ACK carries the applier's native right region (`AppliedRegion`). The
entry node keeps `region → endpoint` (at most 1 024, most specific region
wins) and tries that endpoint first for later writes of the region, after the
exact-zone hint and before the route of its view. The hint grants nothing: the
target still checks its own installed right (`ROUTE-1`), and a hint whose
target did not apply the write is dropped. Test:
`TestRegionRoutesPreferTheMostSpecificRegion`.

## CRDT-3 — Current cannot mutate the replica

`Current()` returns an isolated view. Changing its fields or adding/removing a
vnode cannot change membership, endpoints or tombstones.

## ADD-1 and PLAN-CRDT-1 — ADD shrinks the prepared domain

For an existing vnode, adding competitors only shrinks its XOR cell. For a fixed
proposed ADD set and concurrent ADD-only growth of the base view, the currently
changed domain is contained in the initially changed domain. Current authority
must still be resolved: geometric inclusion never transfers a consumed token.
The mesh revalidates exact geometric regions, including empty zones.

## Legacy local topology contracts

`TOPO-2` and `TOPO-3` named kernel `TopologyStore.CAS` tests; that store was never wired to the runtime and was removed on 2026-11-13 (see the retired rows of [verification.md](verification.md)).

## PLAN-1 — `ValidatePlan` recomputes changes

A planner may only propose `ΔV = (Add, Remove)`. The changes are `{ z | Owner(z, V) ≠ Owner(z, V') }`, recomputed. Predicted owners from the planner are discarded.

## HOT-1 — a safe spawn is donor-only

`g(D) = 1 + max_{U ≠ D} LCP(D, U)`. A safe fork sits at `b ≥ g(D)`.

Its address is canonical: `J = D xor e_b`, so exactly bit `b` differs from
the donor. Among zones currently owned by `D`, `J` captures exactly those with
`z[b] != D[b]`. The current guard and this single-bit form are revalidated
against the current active view before authority moves; a changed guard forces
rebase.

If `Owner(z, V') ≠ Owner(z, V)`, then `Owner(z, V) = D`. Zero external steals. Not a statistical threshold.

This statement quantifies over the supplied set V. A peer's local validation
of its active view does not prove that V contains every relevant competitor.
`TestHotSplitGuardCannotCertifyAnOmittedCompetitor` exhibits local guard 0 versus
actual guard 1 and a key captured from the omitted peer. Extending HOT-1 to
incomplete P2P views requires a certified regional/lineage closure argument;
that argument is not implemented by checking a local digest or observing zero
`StolenRows`. Exclusive durable write authority is a separate invariant.

`TestCompleteViewHotWavesCommuteWithoutExternalCapture` checks four same-view
waves, 32 final vnodes and every leading-byte key in forward/reverse add order.
It qualifies the complete-view case, not the omitted-competitor case.

## HOT-2 — a safe split may be impossible

If no legal fork reduces pressure, the result is `ErrSafeSplitInsufficient`. No spawn, no reservation, no topology write.

## REG-1 — a regional ADD stays inside its region

`R = p*` is the set of ids that share prefix `p`. The region is ownership-closed when every zone in `R` is owned by a vnode that lies in `R`.

A zone outside `R` keeps its owner.

## CAP-1 — physical load is a hard cap

`P(m) = Owned + Residual + Staging + ReservedIncoming` and `P(m) ≤ C` at every step. Every write that increases `Owned` is an admission, not only a handoff reservation. Reserved bytes become staging without being counted twice. The kernel `Machine` ledger once tested against this rule was never wired to the runtime and was removed on 2026-11-13; the durable store below is the enforcement.

The durable store enforces this budget on encoded data keys/values, receipts,
mutation history, staging and retained snapshot payloads. Since 2026-11-19 each
control row counts its last key element and its value only (stored under
`data-usage-2`): the former count also added each zone's key once, which an
update could keep exact only by scanning the disk per changed node. A store
written before rescans once at its first capacity check. Bolt page allocation,
filesystem overhead and control-proof metadata are outside that counter. A hard
whole-filesystem quota is not implemented; deployment needs separate headroom.
With `storage.engine: "zonelog"` a record row counts what zonelog stores
(header, metrics, payload) instead of its JSON, for writes and bulk batches
alike.

## MOVE-1 — a host move preserves ownership

Moving a vnode from one machine to another keeps its ID and `V_active`. Every zone keeps its owner.

## MOVE-2 — one serving disk per physical placement

MOVE transfers grants before publishing the unique placement successor. Staging
or knowing the vnode ID does not make a second disk writable.

## HND-1 — CUTOVER_READY is not ownership

Snapshot, copy, mirror, freeze and staging do not add a vnode to the certified active set. A new vnode becomes a routing participant only through its certified activation. Writable authority additionally requires local grant installation and matching placement. `PREPARED` and `CUTOVER_READY` named phases of a kernel handoff pipeline that was never wired to the runtime and was removed on 2026-11-13.

## HND-2 — an ACKed write survives

If a write was acknowledged before or during the handoff, its effect and replay receipt survive cutover, subject to subsequent acknowledged updates/deletions. Disk-cut tests and a bounded independent model check this obligation; neither proves all interleavings.

With the zonelog store (docs/storage/zonelog.md, not wired), the mirror is
proved complete in another way than today's per-zone counter. The store has
one sequence for all its zones, so a zone's mutations are a subsequence with
gaps. A tail is the mutations of the zone with seqs in `(After, Through]`,
read from log frames that follow each other without a hole; the receiver
applies a tail only when its `After` is the `Through` it last recorded, in the
same frame as the mutations. Tails that join are therefore every mutation of
the zone, once each. After the final tail both sides compute the seal of the
zone (a digest of its live records in key order) and the receiver installs
only when they are equal. The interval check finds a lost, repeated or
reordered tail; the seal finds a lost mutation inside one. The receipt of a
write that was replaced before the handoff does not move: see RETRY-1.

## RETRY-1 — a retry is recognised inside a stated window

zonelog only; today's store keeps every receipt and states no window
(consistency.md). A retry returns the first result and changes nothing when:

- the stored record is still the one its request wrote, for as long as it
  stands, on any disk the record moves to;
- the request's write was replaced since, and its receipt is kept: the last
  262 144 writes of the store or 24 hours, whichever reaches further back
  (both configurable), on the disk that took the write;
- its bytes equal the stored record.

A request id reused with other bytes is a conflict while the record is still
that request's. Outside the window a write that names the seq of the record
it read is refused when the record has another seq; a write that names none
is applied. So a late blind retry can put an old value back over a newer one;
a guarded one cannot.

## HND-3 — single writer despite divergent views

AUTH-1/2/3 establish exclusion independently of topology convergence. Different
processes may route from different views; none can reopen a consumed disk grant.
Bounded checks do not amount to a proof of every unbounded execution.

## REC-1 — crash recovery around durable intent

Before durable capture, the source remains writable. After capture, retry resumes the recorded transfer; after freeze, the affected region stays fenced; after emission, the consumed token cannot be restored. Install mounts bytes and authority atomically. Recovery resumes these phases without electing a replacement for a lost disk.

## HND-4 — freeze partitions the log

Mutations `≤ F` are applied on the receiver. Mutations `> F` are not acknowledged by the donor. A write that races `FreezeZone(z)` is on one side or the other of that zone. It is not lost in between. Sibling zones keep their own `F`.

## MIR-1 — mirror convergence is operational

`ρ = λ_incoming / μ_mirror` describes the operational target. The causal implementation checks shrinking backlog over a bounded four-pass mirror attempt and refuses freeze while the backlog stays above a floor (256 mutations or one twentieth of the region's zones) without shrinking. A backlog under that floor freezes: under sustained writes the backlog never reaches zero, and the frozen catch-up of a bounded backlog is short, whereas refusing forever would make a loaded region impossible to split. This heuristic is not a measurement or theorem about long-term ρ.

## ACT-1 — activation proves acquisition inside its cell

An activation proves, against its observed causal dependencies, that its holder
has installed at least one certified grant **or one transferred zone** meeting
its XOR cell; every grant or zone in the proof must meet the cell. Complete
coverage is not required: the remaining zones stay with their current holders,
who keep acknowledging, and a request that reaches the active vnode for a zone
it does not hold is forwarded along `Authority.HolderOf` to that holder. The
vnode acquires the rest of its cell through the same handoff protocol (`Join`,
then `Rebalance`). A zone is therefore without writer only during its own
frozen phase, never while a sibling is being copied. A grant larger than the
cell is a valid proof; a single-point zone cube that intersects the cell is
also a valid proof. Maintenance returns excess grant area to its XOR owners.
Later ADDs can only shrink the cell. The mesh therefore publishes activations
with an empty dependency context: the cell is then taken against the root
alone, which contains the cell in every view holding the root (`ADD-1`), so
every proof that meets the real cell is accepted and the record stays
constant-size. The price is a weaker sanity check: a proof inside that larger
cell but outside the real one is accepted too; routing then forwards along
authority. No write permission depends on it. If an acquired grant or zone has already
moved onward, the historical proof does not reopen it; the installed authority
chain remains decisive for ACKs. Uncertified staged bytes cannot activate a
vnode.

An active vnode holding part of its cell owns the whole cell in every merged
view (`OWN-1`) while writable authority for the missing regions is elsewhere;
routing by view is a hint and never authorizes an ACK, which is why writes
follow authority rather than the view (see `handoff.md`).

## ACT-2 — activation components cover the plan

`⋃_i Changed(C_i) = Changed(Plan)`. An allowed activation of `C_i` does not apply a transition that belongs to `C_j`.

## DAG-1 — kernel scheduling model: bottom-up on PREPARED

A changed child that is `PREPARED` lets its parent start preparing. An unchanged child does not block. Siblings and independent branches run in parallel. The kernel scheduler that implemented this (`DAG.ReadyToStart`) was never wired and was removed on 2026-11-13.

The scheduler starts a parent once changed children are `PREPARED` (mirror converged), not when they are frozen. Copy of every zone in a MigrationSet may run in parallel; only the authority flip (`FreezeZone` → `TRANSFERRED`) is the later bottom-up wave (`ZONE-2`, `FreezeLevels`).

## ZONE-1 — atomic authority flip per zone

After `ZoneTransfer(z)` is durable, only the receiver may ACK writes of `z`.
The donor is `FORWARD_ONLY` for `z` even while it still holds residual bytes
and even while sibling zones of the same MigrationSet are still live on the
donor. A vnode, a geometric region and a parent-plus-children subtree are not
the unit of transfer.

## ZONE-2 — parent flips after its MigrationSet children

A parent zone in a MigrationSet becomes eligible to freeze and transfer only
after every descendant of that parent that is also in the MigrationSet is
already `TRANSFERRED`. Zones of one depth are independent and flip in small
units, side by side; deeper zones flip first (`FLIP-UNIT`, [handoff.md](handoff.md)). A zone snapshot carries only that zone's records, sequence and retry receipts;
it carries neither descendant bytes nor child markers (`storage.Snapshot`).
The bottom-up order remains an explicit contract of the current granular
implementation. It is not, by itself, the proof of single-writer authority:
installed rights and source fences enforce that rule. Removing the order for
native subregion lots remains a separate design/modeling step.

## MIG-1 — region certificate and residual GC close the MigrationSet

`EmitCausal` (the region certificate) and `CollectTransferred` (residual GC)
run only when every zone in the MigrationSet is `TRANSFERRED`. Zones created
in the envelope during the migration join the MigrationSet at write time and
must flip before the certificate. The certificate consolidates the
`ZoneTransfer` records; it is not itself the per-zone authority flip.

## HIER-1 — materialise is a parent mutation

Historical materialisation rule: the former parent snapshot carried child
markers. Current `storage.Snapshot` has no such markers: a new descendant is its
own canonical zone. While discovery is open, writes add new zones to the
MigrationSet. ZONE-2 requires the descendants already known to the operation
to transfer before their parent freezes; a zone born later still joins the
remaining migration work. The regional fence closes new-zone admission before
consolidation. Snapshot/delta capture
and fences preserve the writes of each zone; parent markers are not the
mechanism that proves this in the current implementation.

## FWD-1 — residual is never authoritative

The kernel forwarding table that FWD-1 and FWD-2 were tested against was never wired to the runtime and was removed on 2026-11-13; the rules below describe the causal serving path.

After cutover the donor is `FORWARD_ONLY`. Residual bytes are not a read. The serving path follows current authority proofs. A fresh routing `Owner` alone cannot authorize a read. Correctness does not depend on the trace TTL.

## FWD-2 — forwarding makes progress

Forwarding follows authority successors. It never treats a topology generation as permission. The hop bound terminates requests; it does not guarantee availability through arbitrary chains.

## AUTO-1 — autoscale order

```text
host imbalance a move can relieve  → MOVE
intrinsic vnode heat               → HotTrie
donor-only split impossible       → Regional ADD
a zone that cannot be cut         → atomic overload
no pressure                       → noop
```

Checked by cases. Process spawn requires a validated, durably reserved intent; no fallback invents a second XOR plan.

## MUT-1 — ordered coverage through F

`seq` increases. Snapshot is `seq ≤ S`. Mirror is `S < seq ≤ F`. After cutover, new writes are `seq > F`. Snapshot and mirror together contain every mutation up to `F`, once.

## MUT-2 — replay is idempotent

Applying `(zone, seq)` twice has the same result as applying it once. Reconnect and duplicate delivery must not double-apply.

## COLD-2 — opportunistic folds are hysteretic

An automatic fold requires a donor that has finished joining and remained below
the cool write rate for the quiet period. The quiet period starts when the join
finishes, even if the donor accepted writes on its first right earlier. Recovery
resumes a previously committed drain independently of this admission check. The survivor admits the pair if their combined owned
bytes fit the configured fraction of its capacity (45% by default) and the
configured item limit permits the move. These thresholds leave room before
the next capacity-driven split.

A pinned fold records the donor cell and both endpoints. Changes outside the
pair's XOR branch can proceed concurrently. Heat or a change to the pair can
cancel the remaining plan before a piece starts. Each started piece follows
the normal durable handoff. Retirement requires the pair to pass its topology
check again after all rights have moved. See [durable pair folds](cold-reconfiguration.md#durable-pair-fold-local-core).

## GOS-1 — bounded anti-entropy reaches everyone

Each maintenance round contacts one holder per non-empty XOR bucket, rotating, plus one former holder. For every target, the contacted partner is the target or shares a strictly longer prefix with it, so a fact crosses the network within the vnode trie's depth in rounds of successful exchanges. This is operational: no authority, ACK or data movement depends on who is contacted. See [gossip.md](gossip.md).

## SUM-2 — the summary repair poll is bounded

Child push carries summaries to their parent. The repair poll that catches a lost push asks every host only on start, on an explicit Update and for a few ticks after an authority change; otherwise each tick asks a rotating window of max(4, log2 N) hosts, so every host is still asked within ⌈N/window⌉ ticks. Before this, every node asked every host each tick: N·(N−1) calls per tick across the network, even idle.

## PREFIX-1 — a prefix read asks only its subtree

The requested rows and coverage are restricted to the prefix subtree. Start with
the membership placement candidates, follow scoped mandatory sources/successors,
and discover additional peers if requested geometric coverage remains unknown.
A successful fresh read requires full coverage, including empty address space,
plus all required referrals. Unknown gaps or unavailable required peers cannot
produce a partial success. Stable full-length locations normally need one actual
holder; the root can require all members. Extra discovery under stale routes is
not a violation of prefix scope.

## JOIN-1 — a joiner halves one of the largest cells

A node that starts without a name asks a bootstrap's public `/join-hint` for one. The hint keeps the prefix of one of the largest XOR cells, flips the next bit and randomizes the rest, so the owner and the newcomer split that cell in two. Sequential joins keep every cell within a factor 2 of every other; random names leave the largest cell up to 2^8 times the smallest at 16 to 100 vnodes. Measured on 16 real processes with uniform keys, the busiest node held 1.05× the mean instead of 2.05× to 4×. The hint grants nothing: authority still comes from activation and handoff.

## SCALE-1 — one scale-up in flight per vnode

After a scale-up request a vnode does not ask again until a vnode that was not active when it asked, sharing 64 bits with the requested target, is active, or three minutes pass (in a deep zone the donor itself shares those bits). The provisioner starts one vnode per target: a second ask for the same target within three minutes starts nothing. A planned vnode counts as joined under its spawned name (`landedNear`). A donor serves one joiner at a time and answers the others "busy" at once; once its autoscale has cooled down it refuses a new joiner, which leaves. A request that got no answer counts as possibly started; an explicit refusal frees the hold. Spawned names keep 64 bits of their target and are fresh at every attempt (SPAWN-1), so a retried spawn never reuses the identity of a failed one. A join that outlives its startup budget keeps running instead of exiting (JOIN-2): exiting freed the slot and left the donor's transfer half done.

Durable spawn reservations are advisory target points, not exact future vnode
names. A reservation retains its target and the placement owner observed when
it was created. It is satisfied when the active placement at that point changes,
including when a fresh nearby spawned identity differs from the planned name.
Endpoint-only moves do not satisfy it; retired candidates do not count as
active placement owners. Consumed
reservations stay consumed across restart and duplicate membership delivery.
Clearing a reservation authorizes a new capacity decision, never a client write;
installed local permissions remain the write gate.

## HANDOFF-STREAM — a handoff moves in bounded requests

A donor copies a region as preload pages of whole zones, at most 2 000 rows each, instead of one request carrying the region. It keeps the receiver's progress per zone across the convergence rounds and the warm and final phases: a later round preloads only the zones the receiver lacks, and the others advance through the mutation mirror. Each operation runs once on the donor, in the background, for at most 30 minutes; a request waits 20 s for it and otherwise answers `handoff-pending` (not-ready), and a retry joins the same job. Safety is unchanged: the receiver already accepted any subset of the region's zones and stages, freezes and installs as before. A single zone larger than the request limit still moves in one request.

## KNOW-LAZY — writers learn on a wrong route

A data ACK can teach an exact-zone endpoint hint. The hint includes its expected
physical endpoint epoch; it is preserved while published membership is older.
It grants no permission and does not synchronously merge global authority.
Repair of stale membership is bounded background control work. Failure or
saturation of that repair does not wait on the user commit.

## NET-1 — peers talk like clients

In the HTTP/3 build of the node (module `quic/`, `quic/cmd/node`), with `network.tls_directory` set, the node's p2p and monitoring ports serve HTTPS over TCP and HTTP/3 over UDP on the same port number (the gateway's `http3server`). The mesh client calls https peers over HTTP/3 and, if QUIC fails, retries the same request at once over TCP and keeps that peer on TCP for five minutes. Every mesh request is idempotent by its RequestID or Operation, so the retry is safe. Plain http peers stay on TCP. The default node (`cmd/node`) links no QUIC stack: it serves HTTPS over TCP and calls peers over TCP; production clients reach HTTP/3 through Caddy. Both builds fill or keep the same extension point, `internal/adapter/transport`, so the rule is about the transport only: no authority, ACK or data movement depends on it.

## NET-2 — HTTP/3 does not cap a connection at 100 requests

The HTTP/3 listener (`gateway/http3server`, used by the `quic/` node) accepts up to 4 096 concurrent requests per client connection. quic-go's default, 100, made each peer relaying writes, and each HTTP/3 client, wait for stream credit beyond 100 requests per node while every CPU stayed nearly idle: a goroutine dump of a saturated load showed 2 346 of 4 096 requests blocked in `OpenStreamSync` and 200 in progress (2 nodes × 100). Admission stays with the node's write batcher, which refuses with an explicit overload.

## KNOW-FAST — compact writes do not build a global frame

Data writes, reads and frozen transfer steps neither build global authority
frames nor wait for membership transport. Local rights, exact admission state
and data seals remain mandatory. The dedicated membership control exchange may
serialize its compact current records; that work is outside the data critical path.

## CAP-2 — a simulated vnode resource

With `limits.writes_per_second` set, a vnode applies at most that many writes per second (one second of burst) and refuses the rest as overload, like a saturated disk. A write this vnode applies costs exactly one token; every way out that applies nothing here (refused, relayed, failed) gives it back once, and a local retry after a cutover keeps the token it holds. It exists for scale experiments, so that the vnode rather than the shared test host is the bottleneck; production leaves it at zero.

## KNOW-CARRY-1 — one carrier per peer and version

Historical operational rule, retired by the membership-only control transport.
The former two-second full-Knowledge carrier wait is not part of the new data
contract. The new control transport keeps a bounded per-peer receipt and retries
an unknown reference once with a full membership body; it carries no authority
history. This identifier remains only to interpret earlier measurements.

## STORE-1 — a read sees one commit

A storage read transaction sees the state of one commit for its whole duration, across pages. It takes neither `storage.writeMu` nor the fsync path; it pins a zonelog snapshot, reads the mutable memtable in short shared-lock chunks and everything else outside `zonelog.mu`. A segment merged away, and the payload files it names, are closed only after every snapshot opened before the merge has closed. A read still shares CPU, disk and short internal locks with writes.

## STORE-2 — write transactions are serialised and durable at ACK

Storage write transactions apply one at a time, in `writeMu` order, each as one zonelog batch: all or nothing, one fsync. A write is acknowledged only after that fsync, and it is visible to every read that starts after the acknowledgement. Admission (`diskBatch`) may refuse a write before it enters a transaction (`ErrWriteOverload`); it never refuses one already inside.

## STORE-3 — retired control shapes are refused, never misread

`Open` refuses with `ErrUnsupportedFormat` a disk whose control state has a shape no reader of this build understands: no control version key, knowledge inline without its digest (before E5b), zone states inside the control document (before XFER-KEYS-1), or rows of the old warm discovery journal. A disk written by this build has none of them.

## XFER-LOT-1 — a lot of zones flips under one barrier

A granular handoff flips its zones in lots. For one lot the source freezes the zones, sends the last delta and the seals, emits the per-zone transfers, and the receiver installs them; from the freeze to that install the lot's zones have no writer. An emitted lot never thaws. A write that meets the freeze on the source waits for the local barrier at most 500 ms, then resolves the holder again; this bounds that one wait, not a write's latency. Lot size adapts to a 40 ms no-writer target (`flipUnitHoldGoal`, FLIP-HOLD-1): a lot halves above 80 ms and doubles below 20 ms, between 4 and 2 048 zones, starting at 16. It is a target, not a bound: a smaller target shortens the wait of a write that meets a lot and lengthens the handoff (more lots, more fsyncs).

A lot also has a cost that does not depend on its size (commits, one round
trip). A halving that shortened the lot by less than a quarter is undone and
its size becomes the floor until a fast lot shows that conditions changed:
on a saturated receiver smaller lots only multiply the commits. Test:
`TestAdaptiveUnitKeepsItsSizeWhenHalvingDoesNotPay`.

## XFER-LOT-ROWS-1 — a lot's proofs and acknowledgement are one row

The fence and the mount of a lot each write one row for the lot
(`meta/zone-lots/<operation>/<lot>`): its zones, each with the sequence and
digest of its seal. No zone has a `ZoneTransfer` row of its own; a zone's
state row names its lot, in the same commit. On the source the lot row also
holds the acknowledgement of the lot's install, set only by a call that
names every zone of the lot; a partial acknowledgement stays on the zones'
state rows. The write path, routing, reads and the resume still decide zone
by zone on the state rows: a transfer cut inside a lot resumes from the zones
not yet transferred. Rows per zone written before lot rows are still read,
and a transfer may hold both. Nothing changes on the wire. Design, model and
measurements: [lot-rows.md](../design/lot-rows.md). Tests:
`TestLotRowHoldsTheProofsOfItsZones`, `TestLotResumesFromTheZonesNotInstalled`,
`TestLotAcknowledgedInPartsCloses`, `TestTransferWithRowsPerZoneFinishes`.

## PLAN-SAMPLE-1 — a scale decision reads a bounded number of zones

Choosing where to split a vnode needs where its load lies in the address
space, not every zone. The zone index orders zones by id and knows the size
of each subtree, so the zone of any rank is found in the depth of the index.
A decision reads at most 512 zones at even ranks (`Store.PlanningSample`),
each weighted by the integer width of its rank slice (`ZoneStats.Stands`),
including the remainder when the count is not divisible by 512. A store
under that bound is read in full. The number of sampled zones is bounded;
rank lookup takes expected logarithmic depth in this treap, and reading the
rows and receipts of each sampled zone still depends on its density. The
first build of the derived index also scans the store. This is not an absolute
constant-time bound. An isolated hot or oversized zone can be missed: no
accuracy bound on heterogeneous load is claimed. The statistics are advisory, as
before: the plan is validated against the view and transfers follow rights,
not this list. A zone is atomic only if one zone alone exceeds the machine.
Tests: `TestPlanningSampleIsBoundedAndSpread`,
`TestSpatialIndexKnowsTheRankOfEveryZone`,
`TestDecisionFromASampleSplitsWhereTheFullOneDoes`,
`TestPlanningSampleWeightsCoverNonDivisibleInventory`.

## GRAIN-1 — a transfer delegates a bounded address prefix

A join does not move a vnode's region in one transfer. Each transfer takes a
grain: an address prefix, empty space included, that holds at most 512 zones
and a byte budget (`HandoffGrain`). Its installed right makes the receiver the
writer of the whole prefix, so later writes to it go there directly. The zone
bound is fixed: measured with one-record zones, fewer zones multiply control
commits and more degrade the tail of write latency. The byte budget starts at
1 MiB and follows the regional freeze that ends each transfer (target 100 ms,
between 256 KiB and 16 MiB); only a grain that filled its budget can raise
it. Test: `TestGrainBudgetFollowsTheRegionalFreeze`.

## WRITE-HINT-1 — an entry hint routes, never authorises

A vnode that relays a write it received from a client (`Hops == 0`) remembers, for that exact zone, the disk that applied the last one (`AppliedBy` of the ACK) when it differs from the holder it resolves. The next such write goes to that disk first. The hint grants nothing: the disk it names accepts only what `writableIn` allows there, and relays the rest to its own holder. A hint that fails is dropped and the write goes to the resolved holder in the same call (one retry; `RequestID` makes it idempotent). An ACK by the resolved holder retires the hint. The cache is bounded (4096 zones, ring eviction) and guarded by `mesh.routeMu`, a leaf never held across a call. Writes that already travelled (`Hops > 0`) ignore hints.

## SUM-3 — a child contribution is stored only while its holder is current

A parent stores a child's summary contribution only if its holder is the zone's current holder when it is stored, checked under `Tree.Mu`, which `refreshLocked` also holds. A contribution of an old holder that passed an earlier check cannot overwrite the new holder's.

## SUM-4 — a zone's total reads its local rows from one version

A zone's total is its local rows, minus the local rows below each child held elsewhere, plus that child's contribution. Those local reads are made from one committed version (`storage.ZoneSums`), for a total, for a `/sets` answer and for the edges of one flush. Otherwise a row a concurrent commit deletes (the collection of rows a transfer handed away) or adds (an install) between two of them is counted twice or not at all: a source still holding handed rows counted each collected zone twice (`TestJoinAfterBulkMovesItsZones`, about a run in three before). A flush moves to a newer version when rows reached the store without a local write (`refreshLocked`). Typed collections read their records in one scan that skips remote children by location; its pages may see several versions, but no row is counted twice.

## AUTO-3 — autoscale waits for transfers that progress

While a transfer of this disk is open, autoscale takes no decision (owned zones and items are moving), however long the transfer takes, as long as transfer steps commit. A step is an `updateCausal` commit that wrote something. After `transferStallLimit` (3 min) without a step, since the last one or since the transfer was first seen open, autoscale stops waiting and asks for capacity again; it neither resumes nor aborts the transfer.

## GC-1 — residual is deleted only after durability

Residual bytes may be dropped only when the receiver's state through `F` is durable, the receiver grant installation is durable, and the old donor is irreversibly fenced `FORWARD_ONLY`. Disk pressure is not a reason to skip that. A delayed install ACK must never
collect bytes belonging to a later installed descendant grant on the same disk.

## AUTO-2 — time to exhaustion

When growth is positive, `T_exhaust = (C - current) / growth`. Scale may start when that time is inside `k` scale intervals. Zero, negative and unknown growth, and a single spike, do not pin the decision. This is a deterministic clock test, not a safety invariant of ownership.

## EXP-1 — balloons are advisory-only

Retired 2026-11-13: the balloon planner was never wired to the runtime and its code was removed; skeleton refinement is not implemented either. See [cold-reconfiguration.md](cold-reconfiguration.md) for the design.

## Node wiring

These are not kernel mathematics. They check that the product calls the kernel.

| ID | Statement |
| --- | --- |
| NODE-OWN-1 | CRDT writes check durable authority; the older kernel wiring tests check pure XOR dispatch without tree exceptions. |
| READ-1 | Inventories come from installed, current disk grants. Since 2026-11-19 frozen source copies remain readable: the source serves the zones as of their freeze until the transfer installs, a receiver not yet installed forwards the read to that source, and a reader keeps the newer copy of a zone served twice. Since 2026-11-20 a source names the receivers of its transfers once a zone has flipped or the region is emitted, and the reader asks them for the zones they hold (`flippedIn`): a zone that flipped takes its writes on the receiver, and a read that stopped at the source's copy lost acknowledged writes (`TestReadDuringTransferSeesWritesOfFlippedZones`). A read during a transfer sees every write acknowledged before it started, or fails when a named receiver cannot be reached; before the first flip the source answers alone. Since 6a3015e, page rows and the receivers needed to complete them are captured in the same MVCC snapshot; completing a transfer cannot erase a required receiver from an older page. This is coverage of earlier ACKs on the refreshed normal read path, not a global instantaneous snapshot of concurrent writes. A missing shard (placement unknown, node down) is still an error, never an empty result. Cached summaries are advisory. |
| NODE-TOPO-1 | `Owner` reads a certified local view. The registry is only a route. |
| SCALE-WIRE-1 | Autoscale persists the exact validated plan and causal frontier as an intent; only certified activations affect the active set. |

## AGG-1 — versioned parent summaries

Parent totals are derived caches. A child publishes an absolute versioned
contribution; the parent replaces that child, never adds a bare delta. The
written zone folds after durable apply; same-vnode ancestors fold on the next
Update, never on the write path. Remote edges coalesce, ACK and retry. A missing
message is not zero. `GetAggregates` answers only for owned locations.

## Réplication

La réplication suit la conception convergente
([convergent-replication.md](../design/convergent-replication.md)) : REPL-1 et
REPL-3 dans [verification.md](verification.md). Le contrat proposé auparavant
(journal acquitté à majorité, REP-1 à REP-7) est archivé dans
[replication-majority-2026-11.md](../archive/replication-majority-2026-11.md).
