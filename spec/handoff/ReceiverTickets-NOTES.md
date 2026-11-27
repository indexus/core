# Local receiver tickets: model boundaries

`ReceiverTickets.tla` is the proposed local permission protocol, not a model of
legacy global certificate validation. Each honest disk keeps installed native
rights, a monotone sequence, and current transfer slots. Source operation IDs,
receiver tickets and local coalesces use that same durable sequence namespace.
No regional floor, certificate ancestry, global authority or quorum is modeled.

The source remains writer while reserving a tentative receiver ticket. The first
preload confirms that exact ticket; only its durable confirmation permits source
emission. A confirmed slot survives crashes and compaction until installation or
source cancellation. Tentative slots may disappear without a clock/lease. An old
zero-ticket admission may allocate a new tentative slot, but an already bound
source never changes tickets. Replayed old install packets acknowledge closure
without mounting rights. Coalescing consumes two current rights and allocates a
fresh ID. Crash/recovery retains durable state; compaction explicitly deletes
receipts. Network packets may be delayed/duplicated and replies lost.

## Checked bounds and limitations

Positive run (25 November 2026): **501,888 distinct states**, 2,151,005 generated,
depth 28, 5 min 40 s, no invariant or temporal violation. Two disks, two abstract
address cells, at most three sequence allocations per disk and **two source
offers** (`TwoOffers` state constraint). Invariants: `SingleWriter`, `SoundACK`,
`ConfirmedUntilInstall`; temporal property: every issued transfer closes after
faults stop, recovery and install/reply/complete actions are weakly fair.

The two-offer constraint is an explicit exploration bound, not a protocol rule.
TLC warns that state constraints can truncate liveness behavior. The checked
liveness statement concerns only this constrained reachable graph; it is not an
unbounded theorem. The earlier unconstrained run was interrupted and is NOT a
passing check. No claim of exhaustive 64-node checking or refinement to Go follows.

This model treats a transfer as one native piece. Granular point fences and data
staging are separate obligations: source cancellation is forbidden after the
**first point fence**, and confirmed receiver state must not be removed after an
installed point. Storage must reject coalescing a source input pinned by an
ongoing operation; the abstraction permits replacing that input before emission
and does not claim liveness for unissued operations.

`TicketData.tla` checks an orthogonal one-region abstraction: two successive
handoffs A→B→A, one acknowledged data revision, durable confirmed slots established
before emission, installation with bytes, closed-ticket ACK after receipt
compaction, lost replies, crashes/recovery, and source collection delayed until
after physical return. It abstracts away tentative admission, uses transfer
indices as opaque finite names, and has no regional epoch comparison. It does
not formally compose with ReceiverTickets or prove the disk implementation.

TicketData positive: **1,472 distinct states**, 4,981 generated, depth 15; all
four safety invariants and fair transfer completion passed. Mutation controls
are necessary checks against vacuous success:

| Configuration | Mutation | Expected invariant violation | Distinct states |
|---|---|---|---:|
| `TicketDataNoCopy.cfg` | Install right without payload | `WriterHasLatest` | 20 |
| `TicketDataEarlyCollect.cfg` | Source collects before install ACK | `ACKRetained` | 22 |
| `TicketDataOldCollector.cfg` | Old source collector deletes reacquired data | `WriterHasLatest` | 282 |

The counter rollback/storage-incarnation requirement is an assumption: stable
counters, confirmed slots and installed rights survive crash recovery together.
A rollback of that durable state requires a fresh storage identity. Peers are
honest; these digests bind messages and do not authenticate Byzantine claims.

ReceiverTickets negative controls also reached the expected failures:

| Configuration | Mutation | Expected invariant violation | Distinct states |
|---|---|---|---:|
| `TicketsReplay.cfg` | Reinstall a closed ticket after a return | `SingleWriter` | 251,980 |
| `TicketsForgetOpen.cfg` | Forget a confirmed slot before installation | `SoundACK` | 7,796 |
| `TicketsNoFence.cfg` | Source retains its right while issuing | `SingleWriter` | 3,331 |

The replay control requires the return path (depth 16) and took 2 min 18 s.
The other two controls took four and two seconds respectively. TLC exit status
12 is expected for these deliberate invariant violations; positive runs must
finish successfully with `No error has been found`.


## Point cancellation obligation checked separately

`FirstFenceCancellation.tla` checks the point-before-region cancellation gap in
20 distinct states, without a liveness or timing claim. Its negative configuration
`CancelAfterPointFence` permits cancellation until regional emission and reaches
two writers after an already installed point (20 states visited). This is why
storage must reject source abort from its first point fence, and why confirmed
moving admissions cannot be expired. The native-piece abstraction above still
does not compose formally with this model; runtime transaction tests remain
required. See [the target qualification map](../../docs/protocol/local-permissions-verification.md).
