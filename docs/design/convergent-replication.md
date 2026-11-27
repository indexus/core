# Convergent replication

> **Conception historique — contexte du 25 novembre 2026.** Les mathématiques
> et choix de conception sont conservés ; les statuts de branche et descriptions
> du transport appartiennent à leur révision d'origine. Pour le contrôle actif,
> lire [permissions locales](../protocol/local-permissions.md) et leur
> [qualification](../protocol/local-permissions-verification.md).

**Statut : proposé.** La réception et la réparation des écritures de secours sont branchées ; aucun code de ce dépôt n'en émet (voir [chemin-ecriture.md](../architecture/chemin-ecriture.md#écritures-de-secours-fallback)).

Le modèle de machine à états, ses vecteurs et ses injections de fautes sont
conservés dans les fichiers de test de `internal/replication/convergent`. Seuls
le comparateur de versions, la sélection des répliques et la configuration utile
sont compilés dans le runtime. Les tests positifs du modèle utilisent ces vrais
primitifs. Ce modèle historique autorise des écrivains de secours ; il ne constitue
pas une preuve de l'écrivain exclusif des tickets de transfert actuels.

Status: design. Written 2026-11-17. The transport-free core is in
`internal/replication/convergent`: merge rule, version vectors, `R(z)`,
the replica state machine (healthy, fallback, repair, clean) and a
deterministic network simulator. Not yet wired into storage or mesh. Closes the gap named in
[failure-model](../protocol/failure-model.md): today a dead holder makes its
zones unavailable and a lost disk loses them (REM-91).

This design replaces the consensus recommendation of
[replication.md](replication.md) with a lighter one. It keeps that document's
analysis of the current code. The majority journal it recommended
(`internal/replication/journal`, `model`) was removed on 2026-11-20; it is in
the git history at `7379aaf`.

## 1. Goal in three sentences

- **Healthy:** exactly one writer per zone, as today. Reads are exact.
- **Failure or partition:** every replica that clients can reach keeps
  accepting writes for the zones it replicates. Reads are marked degraded.
- **Repair:** replicas exchange what they miss and merge. No acknowledged
  write is lost or counted twice, and the zone returns to one writer.

No vote, no lease, no coordinator, no synchronized clock.

## 2. What does not change

| Property | Status |
| --- | --- |
| `owner(z) = argmin XOR(z, V_active)` | Unchanged. Replicas come from the same function. |
| Grants, fences and handoff certificates | Unchanged. A grant is never regenerated or voted. |
| Authority checked in the commit transaction | Unchanged for the owner's writes. |
| Aggregates derived from records, exact once settled | Unchanged. Merged records enter through the normal apply path. |
| No master, O(log N) control cost | Unchanged. Replication adds traffic only between a zone's 3 replicas. |

One guarantee is deliberately weakened. "At most one writer per zone"
becomes: **at most one writer per zone while its replicas can talk to each
other; otherwise several writers whose states merge deterministically.**

## 3. Replica set

`R(z)` = the 3 active vnodes closest to `z` by XOR, skipping vnodes hosted on
a machine already chosen. `R(z)[0]` is the owner. With fewer than 3 machines,
`R(z)` is shorter.

- Pure function of the active set, like `owner`. No directory.
- Locality holds: the replicas of a zone are its XOR neighbours.
- When the active set changes, `R(z)` changes. A new member starts with an
  empty version vector and receives everything through the normal sync
  (section 6). A former member keeps its copy until the 3 current members
  cover it, then drops it.

## 4. Data model

A zone is a map `ID → record`. A write replaces the record of its ID; a
delete is a record with `Deleted` set. This is today's model. Each stored
version gains two fields:

| Field | Meaning |
| --- | --- |
| `dot = (incarnation, counter)` | Unique identity of this write. `incarnation` is the writer's disk incarnation (already used by the topology); `counter` increases by 1 per write on that store. |
| `ver` | Zone Lamport clock: one more than the highest `ver` the writer has seen in the zone, pruned versions included. With one writer it only grows; after pruning, a new write still beats every older version. |

Each replica keeps, per zone, a **version vector**: for each incarnation, the
highest counter such that it holds every dot up to it.

### Merge rule

For one ID, the winner between two versions is, in order:

1. the higher `ver`;
2. at equal `ver`, the deleted one (delete wins);
3. otherwise the smaller payload digest.

The rule is a total order on versions, so merge is associative, commutative
and idempotent. It uses no wall clock.

A losing version is dropped and counted in `replication_overwrites_total`.
In healthy mode there is one writer, `ver` increases by 1 per write, and
nothing is ever dropped. Overwrites can only happen between writers that
could not see each other.

Request receipts replicate with their record, so retrying a request on any
replica returns the first result. The same request ID with two different
payloads on two writers is resolved by the merge rule and counted in
`replication_request_conflicts_total`.

### Deleted records

A deleted version may be dropped only when nothing concurrent with it can
still arrive. Covering its dot on every member is not enough: a version
written concurrently on a lagging member, or still in flight, would then
resurrect the record. The owner prunes when all of these hold:

- the zone is clean (5.4);
- every member's reported vector covers the owner's whole vector;
- no retired member may still bring versions back (section 7).

The owner then sends that vector with every heartbeat and push. A replica
prunes under it **before** merging the message, so a write made after the
prune is never shadowed by a tombstone the replica has not dropped yet.

## 5. The three modes

A zone is in one mode **per replica**; modes are local decisions.

### 5.1 Healthy

1. The owner commits the write locally, as today (authority checked in the
   transaction, group commit).
2. The owner sends the committed batch to the other members of `R(z)`. A
   replica stores it in a **shadow store**: durable, but not authoritative,
   not readable, not counted in any aggregate.
3. The owner acknowledges the client when at least one replica has made the
   batch durable.
4. If no replica confirms within `T_ack` (default 1 s), the owner
   acknowledges anyway and marks the response `durability: local`.

Replicas and owner exchange a heartbeat every `T_beat` (default 500 ms) on
the replication stream.

### 5.2 Fallback

A replica enters fallback for the zones of an owner when it has had no
heartbeat from that owner for `T_suspect` (default 3 s).

- It accepts writes for those zones. It needs no grant: a fallback write is
  not an authoritative write, it is a contribution to merge later.
- It stamps each write with its own dot and with `ver` from its shadow copy.
- It answers reads from its shadow copy plus its fallback writes, marked
  `consistency: degraded`.
- Responses to writes carry `mode: fallback`.
- It stops accepting when its fallback log reaches `fallback_max_bytes`
  (default: the zone capacity). Capacity limits are therefore soft in this
  mode.
- It leaves fallback at the first heartbeat from the owner.

Two replicas may be in fallback for the same zone at the same time. That is
allowed; they merge.

Clients and forwarding nodes try `R(z)[0]`, then `R(z)[1]`, then `R(z)[2]`.
A replica that is not in fallback redirects to the owner.

A false suspicion is harmless: it produces two writers for a short time, and
the merge rule resolves it.

### 5.3 Repair

1. A replica leaving fallback sends its fallback writes to the owner.
2. The owner applies each one through the normal apply path, in one
   transaction per batch, using the merge rule against its current version.
   Aggregates update by exact delta, as for any write.
3. The owner replicates the result to `R(z)` as in healthy mode.
4. When the owner has confirmed a batch, the replica moves it from its
   fallback log to its shadow store.

### 5.4 When a read is exact

The owner marks a zone **clean** when, for every other member of `R(z)`:

- it has heard a heartbeat within `T_suspect / 2`, and
- that heartbeat reported an empty fallback log, and
- its version vector covers the replica's.

A read served by the owner of a clean zone is exact. Otherwise it is marked
`consistency: degraded`. The margin between `T_suspect / 2` and `T_suspect`
assumes local timers do not drift by a factor of two; if they did, the
consequence would be an incomplete read not marked as such, never a lost
write.

### 5.5 Members that leave R(z)

When `R(z)` changes, a store that leaves it stops accepting writes and
sends everything it holds to the new owner until the owner confirms. The
owner's zone is not clean while it waits for such a former member, which
may hold fallback writes. A retired member (section 7) is not awaited for
cleanliness.

### 5.6 Topology changes

A split, fold, MOVE or handoff of a zone starts only if the zone is clean.
These operations move authority, and authority does not merge. A zone in
fallback is served until repair, then may be handed off.

## 6. Synchronisation

Used for repair, for a new member of `R(z)` and for lost messages.

1. Every `T_sync` (default 10 s) and at every reconnection, a replica sends
   the owner, per zone, a digest of its version vector.
2. If digests differ, the two exchange version vectors.
3. Each side sends the versions the other's vector does not cover, in
   batches, in dot order.

Zones are grouped by pair of machines, so one stream carries all zones the
two machines share.

## 7. Permanent loss of an owner

If an owner's disk is gone, its grant is gone and will not come back.

1. An operator declares the incarnation lost. This is a **retirement fact**
   in the topology, which is already a grow-only set that all nodes merge.
2. The vnode leaves the active set. `owner(z)` now designates the next XOR
   vnode, which already holds a copy.
3. That vnode merges its shadow store and fallback log into its
   authoritative store and installs a grant whose proof is the retirement
   fact.

If the "lost" node was only unreachable and returns, its incarnation is
retired: it must not serve. Writes it acknowledged meanwhile are sent to the
new owner as fallback writes and merged (A1 holds). Exact reads on the new
owner may miss them until then: retiring a live node is an operator error,
and this is its cost. It then rejoins with a new incarnation.

Because a retirement can be wrong, every member records the retired
incarnation durably and **no delete is pruned** while it is listed. It
leaves the list when its handover arrives, or when the operator confirms the
disk is destroyed (`Forget`).

Automatic declaration is out of scope for the first version.

## 8. Costs to measure

| Cost | Expected | Measure |
| --- | --- | --- |
| Storage | × 3 | bytes per record per machine |
| Write latency | + one round trip and one fsync on a replica, per batch | p50/p99 against today at the same load |
| Throughput per vCPU | lower: each machine also stores two shadow streams | writes/s per vCPU at 16 vnodes |
| Extra fields | about 20 bytes per version | record size |

## 9. Tests

Every test names what must be true. A test is valid only if it fails when the
named defect is injected.

### 9.1 Unit and property tests

| ID | Test | Passes when | Must fail if |
| --- | --- | --- | --- |
| U1 | Merge laws on random version sets (1 000 seeds) | merge is associative, commutative, idempotent; all orders give the same bytes | rule 3 is removed |
| U2 | Delete against concurrent write at equal `ver` | the deleted version wins on every replica | rule 2 is removed |
| U3 | Version vector | a gap (dot 3 before dot 2) is never covered | the vector takes the max counter |
| U4 | `R(z)` | pure; 3 distinct machines; `R(z)[0] == owner(z)`; one join changes at most one member | machine check is removed |
| U5 | Receipts | a retry on another replica returns the first result | receipts are not replicated |
| U6 | Deleted-version cleanup | a delete survives a concurrent older write from a lagging member, and is pruned once all members are equal | pruning once one replica covers it |
| U7 | Clean rule | a zone is never clean while any replica reports a non-empty fallback log | that clause is removed |

### 9.2 Deterministic simulation (`internal/simulation`)

Each scenario runs with 20 seeds. At the end, after repair:

- **A1** every acknowledged write is present, or was overwritten by a
  version that wins under the merge rule;
- **A2** all members of `R(z)` hold identical bytes for the zone;
- **A3** every aggregate equals the sum over the records;
- **A4** every read marked exact contained all writes acknowledged before it
  started.

| ID | Scenario | Extra check |
| --- | --- | --- |
| S1 | No fault, 3 machines | no overwrite, no degraded read, one writer per zone |
| S2 | Owner crashes, restarts after 30 s | writes accepted during the crash; A1–A4 |
| S3 | Owner isolated from both replicas, clients on both sides | both sides accept; A1–A4; overwrites only on IDs written on both sides |
| S4 | The two replicas isolated from each other and from the owner | three writers; A1–A4 |
| S5 | Owner paused 10 s (no crash), then resumes | false suspicion; A1–A4; no error to clients |
| S6 | Crash of a replica during repair, at each step of 5.3 | repair resumes; no duplicate; A1–A4 |
| S7 | Owner disk lost before any replica confirmed (`durability: local`) | only writes marked `local` may be missing |
| S8 | Owner disk lost, retirement declared (section 7) | next XOR vnode serves; A1–A4 for confirmed writes |
| S9 | "Lost" owner returns after retirement | it does not serve; its writes are merged; A1–A4 |
| S10 | Join changes `R(z)` during fallback | new member converges; A2 |
| S11 | Split requested on a zone in fallback | refused until clean, then succeeds |

Implemented with 11 scenarios (S1–S11, S7 being "very lossy links"; S12 is
inside S3), 20 seeds by default, `CONVERGENT_SEEDS` for wider sweeps.
| S12 | Same request ID, different payloads, on two sides | one deterministic winner; conflict counted |

### 9.3 Local cluster with real faults

Six nodes, 1 200 writes/s, one test at a time. The load client records every
acknowledgement with its mode. Checks A1–A4 are computed from that history.

| ID | Fault | Passes when |
| --- | --- | --- |
| C1 | kill -9 one node for 10 s | acknowledged ratio ≥ 99% during the fault; A1–A4 |
| C2 | kill -9 one node, never restarted, retirement declared | ratio ≥ 99% after `T_suspect`; A1–A4 |
| C3 | network cut between two halves for 30 s | both halves accept; A1–A4 within 60 s of healing |
| C4 | SIGSTOP one node for 10 s | no client error; A1–A4 |
| C5 | no fault | throughput and p99 against the run without replication, reported in `docs/benchmarks` |

### 9.4 Regression

The existing normative tests, `-race` on core packages and
`INDEXUS_LONG=1` must stay green with replication on and off.

## 10. Implementation order

Each step is mergeable alone and ends with its tests green.

| Step | Content | Tests |
| --- | --- | --- |
| 0 | Prerequisite: a split no longer stalls writes (REM-121), interrupted handoffs resume or abort (REM-88) | elasticity scenario green |
| 1 | `dot`, `ver`, merge rule, version vector, as pure code | U1–U3 |
| 2 | `R(z)` | U4 |
| 3 | Shadow store, healthy-mode replication, acknowledgement after one replica, sync | U5, S1, S7, C5 |
| 4 | Fallback, repair, clean rule, degraded marking | U7, S2–S6, S12, C1, C3, C4 |
| 5 | Topology changes and cleanup | U6, S10, S11 |
| 6 | Retirement of a lost incarnation | S8, S9, C2 |
| 7 | AWS run at 16 vnodes with faults | costs of section 8 |

## 11. Open decisions

| Decision | Default taken here | Alternative |
| --- | --- | --- |
| Acknowledge after one replica | yes, with fallback to `durability: local` after `T_ack` | refuse instead of acknowledging locally |
| Degraded reads | served and marked | refused |
| Concurrent writes to one ID | highest `ver`, then delete, then digest | keep both versions and expose them |
| Declaring a disk lost | operator | automatic after a long timeout agreed by the two replicas |
