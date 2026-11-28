# Anti-entropy partners

**Statut : actuel.**

Every replica keeps compact current membership records and retirement tombstones.
Their size depends on names, endpoints and retained identities; the historical
1.15 KB/vnode routing-map estimate does not describe the new format. The
[64-cache test](local-permissions-verification.md) measures its own JSON fixture,
not a universal size bound. Until 2026-11-15 each
maintenance round also *talked* to every known host, and to every host that
ever donated a certificate. That is N·(N−1) exchanges per round, made
sequentially by each node. Removing this all-host partner schedule reduced
that term; it did not remove quadratic traffic from changed full snapshots or
all-host summary polling. See [the cost analysis](../design/scaling-and-control-costs.md).

The runtime now picks its partners the way Kademlia picks its contacts.

## Rule

Group the current holders by the length of their common prefix with this
vnode's ID, which gives at most 96 non-self buckets. In round `r`, contact one member
of each non-empty bucket, rotating within the bucket by `r` and a per-vnode
salt. Also contact one host from the rotating list of current transfer contacts:
these may still be the only contact a stale peer has. The current runtime does
not reconstruct a former-certificate graph.

At each maintenance tick, `Service.GossipTargets` follows the compressed XOR
path toward its own address. Each opposite subtree is one bucket. The ranks
of its leaves form a contiguous interval, so selecting the rotating peer
takes constant work per branching bit. The same rule works before the local
address appears in the view. In a balanced tree the path length is
O(log N), with a hard limit of 96 bits for skewed placement. Local transfer
contacts are considered separately. A control-state change that leaves
membership unchanged reuses the same immutable placement index.

The list sent to maintenance contains only this round's peers. The durable
membership and the placement index are still replicated. A changed or missing
version can require a full membership body. Once the two peers have exchanged
receipts, preparing a version reference does not export, encode or hash that
body again. These bounds describe steady-state partner selection and references;
membership changes, bootstrap and exact regional reads have separate costs.

## GOS-1

> In every round, for every source `s` and target `t`, the partner `s`
> contacts in `t`'s bucket is `t` itself or shares a strictly longer prefix
> with `t`.

A fact held by `s` therefore reaches `t` within `depth` rounds, whatever member
each bucket picks, where `depth` is one plus the longest prefix two vnode IDs
share. Exchanges are push-pull, so the same holds in the other direction.
Rotation adds coverage: each member of a bucket is contacted within `|bucket|`
rounds, so no peer, stale or slow, is starved.

The bound assumes successful logical rounds over a stable, known set of
participants. It is not a wall-clock bound during joins, overload or partition.
Membership converges for compatible announcements from honest participants;
conflicting endpoint forks and retirement cycles are refused, not resolved by a
general CRDT join. A partition supplies no finite convergence-time bound.

Gossip carries membership records, not installed grants. A write is checked
against local permission, and a handoff runs between its participants directly.
An honest terminal departure announcement may justify collecting an already-issued
backup after a lost ACK; it still never authorizes a writer. See
[the exact terminal-receipt contract](local-permissions.md). GOS-1 is a liveness
and cost claim, not a safety claim.

## Measured

`TestGOS1_BucketGossipCrossesNetworkWithinTrieDepth`, synchronous push-pull,
six origins per row. "Spike" stacks seven eighths of the IDs under one hot
prefix, the lopsided trie that a burst of splits leaves.

| Shape | Vnodes | Depth bound | Worst rounds | Messages per round | All-to-all |
| --- | --- | --- | --- | --- | --- |
| random | 64 | 14 | 3 | 406 | 4,032 |
| random | 1,024 | 21 | 4 | 10,589 | 1,047,552 |
| spike | 64 | 41 | 2 | 775 | 4,032 |
| spike | 1,024 | 52 | 3 | 23,578 | 1,047,552 |

Per node, a round costs about log2(N) exchanges, and at most `depth` in a
lopsided trie, instead of N−1.

## Defect injection

| Injected defect | Caught by |
| --- | --- |
| Drop buckets deeper than 20 | `TestGOS1_EveryRoundContactsCloserToEveryTarget` only. Spread timing hides it: detours through shallow buckets still deliver. |
| No rotation inside a bucket | `TestGossipRotationCoversEveryMember` |
| No rotation of former holders | `TestGossipRotationCoversEveryMember` |

## Not done yet

The partner count is bounded; changed snapshot payloads are not. A different
acknowledged version still sends the whole current membership. A same-version
reference has constant wire size, but the current producer still exports,
serializes and hashes the full membership to prepare it. This CPU cost is
measured separately from transport.

The target is one immutable current version and compact announcements of changed
member records, with deduplication and bounded anti-entropy repair. `NeedBody` is
the current missing-reference reply; there is no `NeedKnowledge` authority
exchange. A future delta format must preserve restart and loss recovery, root
identity checks, retirement tombstones and conflicting-announcement rejection.
It must not restore a global history of authority events. See
[the staged proposal and primary sources](../design/scaling-and-control-costs.md).
