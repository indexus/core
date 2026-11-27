# Authority frontier and checkpoint

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Status: implemented on the authority-frontier branch, 24 November 2026.**
This is the first stage of the Knowledge redesign. It retains the current
transport and complete known regional frontier. Membership-only dissemination
and migration of a directory of regional exceptions remain subsequent stages;
this checkpoint alone does not implement them. Durable local read successors
are now implemented as a prerequisite; see [read delegations](read-delegations.md).

## Three different facts

A route says where a request should go. A frontier says which regional rights
have not been consumed. An installed token says that this particular disk has
committed the corresponding data. Receiving a checkpoint changes the second
fact, never the third. `writableIn` still checks the disk's placement, the
current regional/zone holder, its durable outgoing fences and installation in
the write transaction.

Consider A giving Q to B, then receiving Q back. The two rights at A are
**different tokens**. An old A-to-B point flip must remain closed even though
A is again the holder of Q. Each new point flip therefore records its parent
token and that token's creation dot. A consumed parent never becomes live
again.

## What survives compaction

`AuthorityCheckpoint` contains:

- the fixed genesis identity;
- the terminal grants, each with its token and creation dot;
- a causal context: contiguous per-storage counters and intervals above holes.

A transfer allocates its dot durably when reserved. Retries retain that dot;
aborting a reservation records the skipped dot. An interval can represent
thousands of completed operations above one unfinished predecessor. The
context never acknowledges the missing predecessor itself.

The checkpoint merges as an observed-remove set. A grant present on only one
side survives exactly when the other side has not observed its creation.
A grant omitted after observation is consumed, so an old checkpoint cannot
resurrect it. A shared token must describe the same grant. Both the incoming
and merged frontiers must cover the root without overlap. Independent changes
to disjoint rights merge without choosing a global winner.

A receiver records the warm reservation (dot, parent, outputs) atomically with
its first copy, including an empty region. That reservation survives reopening
but is not a frozen/certified installation. A final certificate may advance its
barrier and data digest; it cannot change its authority inputs or outputs.
The admission transaction rechecks the actual parent, so unrelated concurrent
Knowledge changes do not cancel it.

After the completed reservation is collected, an old lot cannot recreate its
certified staging through Stage/Certify. Per-zone installation additionally checks the current
parent before mounting any lot bytes; this protects newer ACKs even if an old
certified staging image is present. The old parent also cannot override routing
merely because its source later reacquires the same region.

**25 November correction:** an obsolete regional Stage/Install may return a
successful idempotent ACK, without changing bytes or installed rights. Requiring
its child to remain live stranded the source after a lost ACK, coalescing and
receipt compaction. Cluster/source/recipient bindings are still checked. This
uses the accepted checkpoint's consumption context, not a new receipt history.
See the [reproduction and bounded liveness model](handoff-reliability.md).

Historical certificate payloads can then be forgotten. The durable disk keeps
its installed rights and operations still needed by local recovery. Completed
transfer receipts are removed only after their per-zone, staging and snapshot
collectors finish. Physical placement history and membership tombstones are
not compacted by this change.

This is not a constant-size claim. Size depends on the current regional
fragmentation, storage incarnations, membership records and unresolved clock
holes. For repeated full-region transfers between two fixed disks, it does
not depend on the number of completed transfers. Local coalescing can reduce
fragmentation when both exact sibling rights are installed on the same disk.

## Coalescing consumes two rights

A coalescing certificate has a primary input and one additional input. Both
must be live, have the same holder, be exact binary siblings and be installed
on the same disk. Its sole output is their parent with that same holder. The
checkpoint, installed-token replacement and local sequence counter commit
atomically. Merely noticing that two routes point to the same machine cannot
perform this operation.

## Maintenance and locks

`Store.CompactAuthority` takes an immutable snapshot, clones/merges rights and
builds the checkpoint outside `writeMu`. A short commit rechecks the exact
control version, facts digest and sequence counter. Concurrent reservations,
fences, installs or moves invalidate that commit; it returns `ErrReadChanged`.
There is no remote agreement or cluster-wide lock.

Compaction is refused during a local unfinished transfer, physical move or
cold drain/pin. These operations still need their existing receipts/proofs.
`Service.Resume` invokes it after recovery/collection. Consequently this first
stage does **not** establish an unconditional bound on the certificate tail
under an endless stream of overlapping transfers. It also does not prove
that all existing post-fence work is independent of history; that is a
qualification criterion for the completed redesign, not a delivered claim.

## A deliberate change of trust

Before compaction, a receiver can validate the retained certificate chain back
to genesis. After compaction, it trusts an accepted checkpoint and validates
new operations from its frontier. Membership admission digests attest records
already validated before their ancestors were discarded. These are digests,
**not signatures or Byzantine quorum certificates**.

The failure model is honest peers with durable crash recovery, duplication,
reordering and temporary partitions. Reusing the same storage identity after
restoring a backup that lost later durable fences is unsafe. Recovery must
retain those fences or create a fresh storage incarnation and reacquire rights.
An address or XOR proximity is never an ownership proof.

The sole disk format is now `indexus-flat-3` (durable read successors). Old disks are explicitly refused;
there is no implicit migration and no parallel legacy storage mode. This
branch requires homogeneous protocol binaries; mixed-version deployment is
not qualified. Do not point it at an existing production directory expecting
an in-place upgrade.

## Evidence and limits

`spec/handoff/check.py` now runs seven positive bounded models and eleven expected
negative controls (including the 25 November install-reply model). The new `AuthorityCompaction` model explicitly forgets
history while the network retains old messages. It includes replay, crash,
recovery and restoration with retained consumption floors. TLC explores
77,833 distinct states. Dropping the replay guard or restoring an old identity
without its floors each produces a double-writer counterexample.

`CoalescedRights` checks consuming both siblings before transferring their
parent. Its negative control consumes only one and produces two writers.
These are bounded abstractions, not a refinement proof of the complete Go
implementation, its observed-remove merge, regional reads or real-time bounds.

Implementation tests additionally cover concurrent disjoint checkpoint merges,
replay of older checkpoints, compact clock holes, both holder lookup paths,
100 pure authority round trips, and 30 actual disk-to-disk transfers with fsync,
periodic reopen, old Stage/Install replay and retention of the latest ACKed
update. A separate warm-admission test reopens a receiver and checks that the
reservation neither authorizes regional installation nor accepts changed
outputs. Regional tests verify the root total after a bulk join/checkpoint and
continuous reads across joins/partition starting from compacted checkpoints.
The 64-vnode laboratory also performs checkpoint maintenance before
its final reopen and data/authority audit. Its results are reported separately
from AWS; a local pass does not establish 16,000 writes/s.

See [handoff](handoff.md), [failure model](failure-model.md),
[Knowledge](../architecture/knowledge.md),
[the bounded models](../../spec/handoff/README.md) and
[the redesign rationale](../research/knowledge-convergence.md).
