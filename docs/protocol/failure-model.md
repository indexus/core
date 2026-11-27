# Failure model

**Statut : permissions locales branchées dans le worktree, qualification complète en cours.**
Voir [local-permissions.md](local-permissions.md) et sa
[qualification](local-permissions-verification.md) pour distinguer modèle et
chemins effectivement testés.

The protocol assumes honest crash-recovery peers, collision-resistant binding
digests, and storage that preserves successful synchronized zonelog commits.
It is not a Byzantine protocol. A receiver trusts a cession from the actual local
owner; it no longer reconstructs every delegation back to genesis. Membership
announcements identify routing destinations and never grant installed rights.
A MOVE/retirement producer also attests that intake was closed and confirmed
admissions resolved before departure; this terminal fact can justify collection
of an already-issued source backup. The old epoch remains closed to new admission.
It is an honest local producer assumption, not a signed Byzantine certificate.

An ACK means that the authorized disk committed the record, revision and retry
receipt durably. A handoff copies data and receipts, fences the source and installs
bytes with permission at the receiver. The receiver's confirmed admission survives
until install or a valid source cancellation before its first fence.

| Fault | Contract |
| --- | --- |
| Process restarts with durable state | Resume the same operation, ticket, region and fences; retain ACKed mutations |
| Lost reply or duplicate old packet | Retry the same operation; a closed ticket ACKs without remounting rights or bytes. An honest terminal departure record can close an already-issued source for collection only; same-disk address changes cannot |
| Reordered membership/movement announcements | Newest retained endpoint wins; retirement is permanent; no write permission is created |
| Missing membership or old endpoint | Route repair/discovery can lag; installed writes do not await global metadata |
| Receiver lacks a confirmed slot or matching staged bytes | Refuse new installation; no guessed authority |
| Temporary partition | Existing unfenced owners can write; affected handoffs wait, preserving fences |
| Reconnection and eventual delivery | Compatible membership converges; persisted handoffs resume under scheduling/admission assumptions |
| Receiver unavailable after irreversible cession | Wait for recovery; no timeout may reopen the source |
| Missing read coverage or required peer | Wait or fail explicitly; never return an authoritative empty/partial total |
| Permanent loss of the only owning disk | No automatic takeover or recovery of its sole data copy |
| Backup rollback under the same storage identity | Unsupported: old tickets/counters/fences can resurrect permission; use a fresh incarnation and reacquire rights |
| Byzantine peer, cloned simultaneously serving disk, dishonest fsync | Outside the model |

Confirmed moving slots are not time leases. A tentative header-only admission
can be discarded before its first confirmed preload; this cannot let a source
fence without a valid confirmation. Once any point is fenced, cancellation until
regional emission is already too late. Recovery must continue that same operation.

Safety permits an interval with no writer. Neither a fixed maximum freeze nor
availability during an infinite partition follows from eventual convergence.
Local transaction serialization and fsync still exist; the removed dependency is
cluster-wide authority validation on the data path, not all local locks.

No compatibility deployment is required: retired wire/control formats are
refused rather than silently interpreted as the new protocol. The exact disk
version is an implementation detail documented with the runtime once finalized.
Tests with a power-loss device or model checker cover their specified cut points,
not every filesystem failure or an unbounded proof. Historical benchmarks belong
to their recorded binary and workload.
