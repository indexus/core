# Causal topology and exclusive authority

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Statut : actuel.**

**Extension sur la branche authority-frontier (24 novembre 2026).**
La [frontière et les checkpoints](authority-frontier.md) remplacent l'ascendance
ancienne par un checkpoint accepté, sans installer de droit par gossip.
Les nouvelles opérations restent validées depuis cette frontière. Une fusion
locale de deux droits frères consomme ses **deux** entrées atomiquement.
Le contrat lié précise la confiance, les rejets de rejeu et les étapes qui
restent distinctes de cette première compaction.

A stale view may route a request to the wrong place. It must never authorize that
place to acknowledge a write. The runtime has three independent responsibilities:

1. Geometry: `View.Owner(z) = argmin XOR(z, V)`; unchanged kernel mathematics.
2. Convergence: a grow-only set of activation and retirement facts on every node;
   the derived active set can shrink.
3. Authority: irreversible local handoff certificates, persisted with disk fences.

## Topology replica

`internal/causal.TopologyReplica` implements `Current`, `Merge`, `Frontier`,
`DeltaSince` and `Digest`. `Current` returns a fresh view. Its generation field is
zero: the field remains for kernel compatibility, not as a distributed clock.
The digest describes a set, not a globally ordered decision.

An activation has a vnode, a disk-incarnation dot, a causal dependency frontier,
a physical holder and authority proofs. An intent reserves exact planned vnode
identities but never activates them. An activation appears in `Current` only once
its dependencies and a valid installed authority proof reaching its cell are
available (ACT-1). Shadow data and
peer registration alone cannot publish a vnode.

The frontier acknowledges contiguous validated dots. Receiving operation A:3
before A:2 never acknowledges A:2. Pending activations stay outside the view.
Certificate and placement chains have separate causality; deltas include their
closure instead of treating the control frontier as a transfer frontier.
Missing certificate ancestors are refused atomically and fetched on retry.

Merge laws apply to compatible valid histories: union is associative,
commutative and idempotent. Equivocation (two payloads for one dot or two
successors for one authority) is a protocol error, not last-writer-wins data.
Under quiescence and eventual delivery, replicas converge on the same active set
and therefore the same XOR result. No finite convergence time is promised.

For a short explanation of why an address is insufficient during a handoff, see
[address and write authority](write-authority.md).

## Authority, including empty space

A grant covers a 96-bit region `(mask, value)`, not just a list of existing rows.
The exact XOR cell is the intersection of the first-differing-bit constraints
against each competing vnode. This is checked against an independent exhaustive
XOR reference over bounded domains.

A certificate consumes one previous token and partitions its region into disjoint
outputs with complete coverage. The transferred output goes to the receiver;
residual outputs remain with the sender. Every key follows a single successor
chain even when the authority tree branches for different keys. Overlapping
changes to one token serialize on its holder's disk. Other tokens need no global
lock. The granularity is conservative: disjoint subregions of the same unsplit
token can temporarily serialize too.

Holders bind vnode, endpoint and durable disk identity. A certificate contains the
previous token, operation identity, holders, region partition and data digest.
Per-zone sequence barriers are included in the sealed snapshots; the scalar
`Barrier` is only their maximum, not a replacement for those per-zone positions.
The earlier per-zone `ZoneTransfer` proof also commits to the frozen records and
request receipts through a mandatory `DataDigest`. Installation checks the zone,
sequence and digest atomically. A sealed receiver copy is immutable even before
installation; an older final copy cannot regress unsealed mirrored progress.

Gossip learns certificates but **never installs writable authority**. The receiver
must mount matching bytes, request receipts and its grant in one disk transaction.
The sender persists consumption before transmitting the certificate. A stale
sender remains fenced after restart, irrespective of its topology view.

## ADD and rebase

`ValidatePlan` describes effects against an observed view. A durable `JoinIntent`
retains that plan and frontier before process spawn. Join and reconciliation
revalidate against the current local view and the current authority holders.
An ADD can only shrink existing XOR cells. Concurrent preparations can therefore
be larger than the final cells; background reconciliation transfers the remaining
excess through the same authority protocol. A historical acquisition proof can
justify an activation without reopening authority already transferred onward.

This applies to ACT-1 zone proofs too: after a region certificate replaces the
per-zone transfer records, any certified historical grant to the activating
holder can attest that acquisition. Requiring the current terminal grant would
forget an activation when its proof zone moves again. Replaying that history
must retain the activation while resolving writes through the current successor.
`TestACT1_CompactedZoneProofSurvivesOnwardTransfer` covers both obligations.

## Physical placement and logical removal

MOVE preserves the vnode and active set. A separate `PlacementRecord` chain has
one successor per prior physical holder. The old host first stops admitting new
incoming handoffs, transfers its grants and bytes, then persists the placement
successor. The receiver cannot ACK merely because it staged those grants; its
local placement must also name its disk. Retries and restart retain the same move.

Logical removal is available through pair folds, on by default (COLD-2).
Retirements share the activation dot/frontier clock; proofs cover the historical
cell with outgoing installed grants. The source seals intake and the survivor
pins itself before transfers. A durable witness precedes completion. Historical
views preserve earlier activations, while current views exclude tombstoned names
and reserve retired IDs against reuse. Old activation replay cannot resurrect a
vnode. SoftLeave and skeleton W1 remain disabled. General balloon ADD/MOVE/packing
execution is still advisory; see [cold reconfiguration](cold-reconfiguration.md).

## Dissemination

Mesh requests piggyback local topology and certificate knowledge; refusals retain
that knowledge in the response. Periodic maintenance exchanges state even without
client traffic, resumes persisted transfers and reconciles authority with XOR.
This implementation sends full proof closure, not a compressed certificate delta.

An entirely known delta is an idempotent no-op: merge checks exact content
identities and avoids cloning and revalidating the unchanged proof graph.
Any unknown or conflicting fact still follows atomic validation. Canonical
transmission order uses the already validated digest keys, without re-encoding
certificates during sorting. Neither optimization treats an activation frontier
as acknowledgement of transfer proofs.

Bulk zone-transfer validation builds a temporary index by the existing conflict
identity `(operation, collection, location)`. It performs the same content and
sequence checks as singleton insertion, without rescanning all earlier records
for every transfer. The index is neither persisted nor an authority source.

It favors inspectable recovery over metadata compactness. Proof/fence GC needs a
separate safety rule and is not implemented.

## References

The set semantics follow [Shapiro et al., A comprehensive study of CRDTs](https://www.lip6.fr/Marc.Shapiro/papers/2011/Comprehensive-CRDTs-RR7506-2011-01.pdf).
Causal dots are informed by [Almeida et al., Dotted Version Vectors](https://arxiv.org/abs/1011.5808).
Neither source proves Indexus authority transfer; its independent obligations and
bounded evidence are listed in [verification.md](verification.md).

## Warm zone discovery

A zone born during a warm handoff is registered transactionally with its write
in `causal-migration-zones`, keyed by operation and canonical collection/path.
This discovery journal cannot grant authority or override a freeze/installation.
It avoids rewriting the complete control document for every new zone. Control
operations fold the journal into their detached transfer state before computing
child/parent dependencies, freezes and certificates. Completion removes the
journal in the same transaction that records the completed handoff.

Permission/knowledge caches decode only the authority control document and remain
keyed by its exact bytes. The separate journal contains no permission decision.
Restart, rollback and aliased parent/child tests check the separation explicitly.

## Knowledge key

The certified knowledge (`CausalState.Delta`) is stored under its own control
row, `meta/causal-v1-knowledge`, and rewritten only when it changes; handoff phases
rewrite only the control document `causal-v1`. The control document keeps the
knowledge digest (`Knowledge = Hash(Delta)`), so a change of knowledge alone
still changes the control bytes that permission and knowledge caches key on.
Reads check the stored bytes against that digest; a missing or altered key, or
a control document that carries both inline knowledge and a digest, is refused.
A control document without a digest was written before this split (every
zonelog store up to 8c76df2): `Open` refuses it with `ErrUnsupportedFormat`
(STORE-3). Older binaries cannot read the split form; downgrade is not
supported. Decoded knowledge is cached by
digest and handed out as deep copies, because callers append to it. Tests:
`TestKnowledgeKeyKeysControlCaches`, `TestOpenRefusesRetiredControlShapes`,
`TestKnowledgeKeyRefusesInconsistentDocuments`,
`TestKnowledgeReadsAreIndependentCopies`, `TestTopologyDeltaCloneIsDeep`.
