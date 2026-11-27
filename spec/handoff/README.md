# Bounded granular recovery model

`GranularRecovery.tla` explores one donor/receiver authority chain, two child
zones and their parent. It separates durable freeze, certification, emission,
installation and the donor's installation ACK. Routing membership and a
working global Knowledge builder are not prerequisites for writing an already
installed zone. A single transient disconnect/lost response preserves durable
state, including a receiver installation whose ACK has not arrived.

This is a bounded abstraction, **not a verification of all Go executions or a
64-node exhaustive proof**. `Stage` abstracts the final mirror and durable seal
certification. Retransmission after installation stutters; Go fault tests
separately verify idempotent replay does not overwrite later ACKed writes.
It does not model simultaneous distinct overlapping authority operations,
cryptography, OS/fsync bugs, unbounded partitions, real-time latency or crashes
that lose durable storage. Those are not covered by the reported state count.

Under weak fairness for individual transfer steps and eventual reconnection,
TLC checks single-writer authority, retention of ACKed state at the writable
holder, children emitted before parent freeze, regional close after every
installation ACK, and eventual regional closure.

Two negative controls must fail:

* `MetadataBlocked.cfg`: retry requires an unavailable global metadata
  builder. Safety can hold while eventual closure fails. It models the
  scheduling dependency of the old recovery path, not a claim that an actual
  network or builder will remain stuck forever in production.
* `UnsafeThaw.cfg`: a timeout may reopen an already emitted source. TLC must
  find two possible writers. Removing durable fences is not a recovery fix.

Use the [official TLC command-line tools](https://github.com/tlaplus/tlaplus/blob/master/USE.md).
The reproducible runner pins the official
[v1.7.4 jar](https://github.com/tlaplus/tlaplus/releases/download/v1.7.4/tla2tools.jar)
(SHA-256 `936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88`).
The binary is not vendored. Java 11+ is required. We use one worker, including
for temporal checks, rather than relying on parallel liveness behavior.

```bash
python3 spec/handoff/check.py --jar /path/to/tla2tools.jar --out /tmp/indexus-tlc
```

Without a selection, exit 0 from the runner means all twelve positive checks passed
**and** the twenty-one intended counterexamples were found, not that the negative variants were safe.
Logs and machine-readable counts are saved in the requested output directory.
Each configuration has a 600-second timeout, configurable with `--timeout`.
Use repeated `--only CONFIG` arguments to run a subset; its result covers only
that selection, not the entire suite. Temporal checks run after graph exploration
with `-lncheck final`; a timeout is a failed, incomplete check, never a proof.

## Terminal receipt after receiver departure

`ReceiptAfterDeparture.tla` isolates S→R install with a lost reply, followed by
R's physical MOVE or logical retirement to P. Its **336 distinct states** check
`SingleWriter`, `SoundReceipt`, `ACKRetained` and `WriterHasLatest`. It includes
one later receiver ACK, source/old-receiver collection, loss of the old address,
same-disk address change and a physical return to the original disk/ticket
namespace. This is a safety-only model: it has no fairness/liveness property,
wall-clock time, multi-region directory or Go refinement proof.

The model starts after confirmed admission. Its honest departure producer closes
intake and resolves confirmed slots before publishing; no admission is reopened
after departure. The Go epoch/intake guards must realize that precondition;
merely forgetting the temporary `Moving` flag is insufficient. An announced
MOVE to a different owner at a higher epoch, or retirement at an adequate epoch,
can acknowledge **an already-issued source backup for collection only**. It never
grants write permission or carries the old receiver's ticket namespace to P.
Same-owner address updates are not a terminal receipt. A return to the original
disk conservatively uses its retained closed-ticket counter.

`DepartureBeforeDrain.cfg` publishes departure before the receiver resolves its
slot: `SoundReceipt` fails after **49 states**. `AddressAsDrain.cfg` incorrectly
uses a same-owner address change as a receipt and fails the same invariant after
**31 states**. Positive and intended-negative checks were executed separately
from any previously launched full runner; an earlier full run cannot silently
include newly added configurations.

## Cancellation stops at the first point fence

`FirstFenceCancellation.tla` isolates the interval before regional emission: a
point can already have been emitted and installed while its region is unfinished.
The source may cancel only before the **first point fence**, not simply before
regional emission. Confirmed moving state survives crash/recovery. The positive
safety-only model has **20 distinct states** and checks `SingleWriter`,
`CancellationBeforeFirstFence` and `MovingSlotRetained`. It has no temporal
property or wall-clock model.

`CancelAfterPointFence.cfg` deliberately permits source cancellation until
regional emission. TLC reaches the specified `SingleWriter` counterexample in
**20 states**: a point is installed, then cancellation reopens its source. This
small model complements the native-piece `ReceiverTickets` abstraction; it is
not a composition or refinement proof. Storage must test the actual first-fence
transaction and rejected cancellation as well.

## Local receiver tickets: runtime replacement under qualification

`ReceiverTickets.tla` explores the proposed local permission protocol without a
global authority graph or regional consumption floors. A source reserves a unique
operation, a receiver allocates a tentative ticket, the first preload confirms it,
and only then may the source fence and issue the cession. Confirmed slots survive
crashes and cannot expire unilaterally. Installation consumes the slot. A replay
against a closed ticket acknowledges without recreating rights.

The positive check explores **501,888 distinct states** (2,151,005 generated),
with two nodes, two abstract cells, sequence numbers at most three and at most
two offers (`TwoOffers`). It checks `SingleWriter`, `SoundACK`,
`ConfirmedUntilInstall` and eventual closure of issued transfers under the
model's fairness and eventual recovery assumptions. This is bounded exploration,
not a proof for unbounded operation sequences or the real 64-node implementation.

`TicketsReplay`, `TicketsForgetOpen` and `TicketsNoFence` deliberately remove,
respectively, closed-ticket rejection, retention of confirmed slots and source
fencing. The runner requires their specified invariant counterexamples.

`TicketData.tla` separately checks copied bytes and ACK retention across A→B→A,
with one revision. Its negative controls omit copying, collect before installation
ACK, or let an old collector delete reacquired data. Its setup assumes confirmed
admission slots. These two models are complementary abstractions: neither is a
composition or refinement proof of the other, or of Go storage transactions.
See [the model notes](ReceiverTickets-NOTES.md) for actions and scope.

The worktree's Go transfer runtime now uses local ticket permissions and
membership-only control. The target models do not by themselves qualify that
integration: read the [separate runtime evidence map](../../docs/protocol/local-permissions-verification.md).
The historical checkpoint/floor models below remain regression research artifacts,
not an alternate runtime mode or a proof of the ticket implementation.

## Geometric subdivision and empty space

`SubregionAuthority.tla` independently checks a donor and receiver transferring
three abstract geometric pieces, one at a time. A piece includes unwritten
addresses: permission moves with its certificate, without requiring a prior
row or ACK. The source retains the exact complement. Capture, fencing, final
seal, emission and installation are separate actions. One crash preserves the
chosen piece; recovery resumes that same choice. Weak fairness plus recovery
implies eventual complete coverage; `SingleWriter`, `ACKReadable` and
`ResidualWritable` hold in the bounded graph (2,318 states).

`UnsafeReselect.cfg` deliberately chooses a different piece on recovery while
retaining the old fence. TLC finds a double-writer counterexample (439 states
visited before failure). The Go implementation must therefore resume the
persisted output region, not re-run its inventory-based choice after a crash.
`TestHandoffGrainMovesFreshAddressesBeforeWholeJoin` is the implementation
bridge: both global frame builders stay locked through the first completed
piece, a new address is writable only on its receiver, retry retains the exact
certificate, and Join subsequently acquires the residual grants.

This second model abstracts independent geometric pieces; it does not prove
ordering of overlapping parent/child summary zones across operations, bound
the number of newly born rows in a piece, or bound wall-clock freeze duration.
The 512-zone selection in Go is advisory under concurrent insertion, not a
hard cap. Infinite partitions have no finite availability guarantee. Neither
model proves 16,000 writes/s or models the complete autoscaler.

The runner also includes the token-isolation model below.

## Isolation of independent authority tokens

`TokenIsolation.tla` starts with two certified, disjoint terminal source grants.
One receiver is permanently partitioned. With per-token serialization and two
available worker slots, the other grant must still install and finish under
weak fairness, while ACK retention and single-writer safety hold. The
`SerialDisk.cfg` negative control retains the former disk-wide network-length
serialization; TLC must find a liveness failure for the healthy receiver.

This models **one blocked and one healthy operation**, not an unbounded number
of partitions with finite resources. Two slow operations can occupy both local
worker slots until their RPC/deadline returns. The bound controls disk pressure;
it is not a limit on the number of nodes or machines in the cluster. Operations
consuming the same token remain serialized and the normal transactional
one-successor checks still apply. The token-gate implementation is tested for
cancellation and reference lifetime. The integration test
`TestIndependentTokenFinishesWhileAnotherReceiverIsPartitioned` fails on the
original `main` admission policy and passes with the new one without thawing
the partitioned source zone.

The install-reply stage had **six positive models and eight negative controls**.
With read successors there are **seven positive models and eleven expected negative controls**.


## Simulation de transport Knowledge (distincte de TLC)

`python3 spec/handoff/knowledge_sim.py --out /tmp/knowledge-simulation.json`
compare le transport complet et incrémental sur 64 pairs, trois graines,
1 536 faits, pertes de messages et quatre redémarrages de caches par run.
Les faits durables survivent ; les bases de transport peuvent être évincées.
Les six exécutions doivent converger vers le même ensemble de faits.
Il s'agit d'une simulation abstraite, pas d'un model checking TLA+, d'un modèle
Byzantin ou d'une prédiction de débit. Les garanties d'installation et de fence
sont testées séparément par les modèles TLC et le laboratoire Go.

## Forgetting authority history

`AuthorityCompaction.tla` explicitly clears local historical messages while
keeping current installation, pending operations and durable consumption floors.
The network can replay any previously issued message after compaction. Three
nodes and two zones permit a round trip, one crash/recovery and bounded backup
restoration. `SingleWriter` and `NoConsumedWriter` hold in 77,833 distinct states.
This checks safety, not temporal progress or ACK retention. The safe restoration
action **retains consumption floors**: it is not arbitrary rollback of an entire
disk under the same identity.

`CompactionReplay.cfg` removes the replay guard (332 states before failure).
`CompactionRestore.cfg` rolls back those floors as well (155 states before
failure). Both must violate `SingleWriter`.

`CoalescedRights.tla` checks consuming both installed sibling tokens before
transferring their parent (9 states). `UnsafeCoalesce.cfg` leaves one input
unconsumed and must violate `SingleWriter` (5 states before failure). Neither
model proves the Go observed-remove checkpoint merge or all regional queries.
The corresponding implementation tests cover those bounded merge/replay cases
and disk transfers separately; see
[authority-frontier.md](../../docs/protocol/authority-frontier.md).

## Lost install reply after compaction

`InstallReceipt.tla` checks a source emission, durable receiver installation,
local coalescing and deletion of historical receipt payloads. A retained
consumption context permits an obsolete retry to return an ACK without
reactivating the consumed child. `SingleWriter` and `SoundReceipt` hold;
weak fairness implies eventual source closure (11 states).

`ForgottenInstallReceipt.cfg` keeps the former requirement for a live child or
historical payload. Coalescing followed by compaction disables every future
reply; TLC must find a liveness counterexample (also 11 states). Context here
means accepted consumption evidence, under honest producers that consume only
rights they acquired. It does not model arbitrary forks or dishonest checkpoints.

The Go bridge is `TestInstalledReplySurvivesLocalCoalescingAndReceiptCompaction`.
The transaction scheduler additionally executes real storage methods on the
shared power-loss device; see
[handoff-reliability.md](../../docs/protocol/handoff-reliability.md). The bounded
model and this scheduler complement rather than formally refine one another.

## Read successors

`ReadSuccessors.tla` checks a stale A entry across A→B→C→A→B handoffs,
receipt compaction and deduplicated recursive reads. A positive run has 107
distinct states. Regional emission preserves its successor; acquiring a region
clears its previous outgoing pointer. The read begins after at least one
completed handoff and stops subsequent moves: it checks stable-directory
reachability, not all concurrent inventory cuts. Go tests cover a frozen
intermediate, MVCC cutover, unavailable receivers and actual transaction faults.

`ForgottenReadSuccessor` drops a forwarding pointer during receipt compaction;
`OneHopRead` stops after one referral. Both return incomplete successful reads.
`StaleReturnSuccessor` fails to clear an acquired region and creates a cycle.
All three must produce invariant counterexamples. This abstraction is not a
refinement proof of the Go directory overlay or full membership-only routing.

## Independent incoming and outgoing recovery

`ResumeIsolation.tla` isolates a healthy issued B→C operation while A→B remains
unavailable forever. Weak fairness for outgoing progress yields closure without
assuming recovery of A. The positive model has four states; the coupled variant
`IncomingBlocksOutgoing` finds a liveness counterexample in one state. It is a
scheduler abstraction, not a proof of Go transactions, arbitrary dependency
cycles or a wall-clock deadline. `TestIncomingFailureDoesNotStarveIndependentOutgoing`
bridges it to three real stores, lost installation ACK, a receiver write before
source recovery, and the original source remaining fenced. The earlier transfer
models assumed fair execution of enabled steps; they did not model the early
return in the runtime maintenance scheduler.
