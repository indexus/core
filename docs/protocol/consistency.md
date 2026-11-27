# Consistency

**Statut : garanties de lecture/écriture actuelles.** Le contrôle d’autorité a été remplacé par les [permissions locales](local-permissions.md) ; le moteur et ses verrous sont décrits dans [stockage-et-verrous.md](../architecture/stockage-et-verrous.md).

The topology is eventually consistent; write authority is exclusive. Those are
separate guarantees. A stale view can misroute a request but cannot reopen a
consumed grant. Exactly one current holder may ACK; during a frozen transition
there can temporarily be no available writer.

A successful write commits its record, sequence, history and request receipt in
one local zonelog-backed transaction. Retrying the same request ID and payload returns its
prior result. Reusing that ID for a different mutation is a conflict. Receipts and
tombstones move with the data, so retrying an old insert cannot undo a newer delete.

The default store groups concurrent writes (up to 256, collection window 2 ms).
Each caller waits for the successful synchronized commit before HTTP 201. A
failed callback is rejected independently when the group-commit worker retries the remaining batch.
The window bounds collection delay, not disk or queue latency.

The immutable record is encoded once before entering the single-writer
transaction; the same bytes define its request digest and stored value.
Authority and fences are still checked inside that transaction, even when a
read-only preflight already accepted them. Sequence, history, both receipt
indexes and physical admission remain atomic with the record. Grouping or
pre-encoding does not authorize an early ACK.

New zones consult the transaction's exact cached causal snapshot when joining
an existing warm handoff. Registration copies only the affected control maps
before persistence; a rollback cannot mutate the shared snapshot. Creating an
unrelated zone does not deserialize the whole transfer history again.

Forwarding follows durable authority successors and is bounded at 64 hops. It
piggybacks topology knowledge. The hop bound prevents unbounded work; it does not
prove availability through an arbitrarily long chain or a partition. The node
write entry applies the same bound when two holders' placements disagree during
a join and a write bounces between them until their views merge.

Cross-shard reads collect current authoritative inventories. A missing shard
is an error, never an authoritative empty result. A pending handoff is not an
error (READ-1, 2026-11-19): its source keeps serving the zones as of their
freeze, so a read during a transfer can lag the latest acknowledged writes of
a zone that already flipped, and converges once the transfer installs. The
reader retries if its authority knowledge changes during collection. This guards
cutover boundaries; it is not a common timestamp across independent user writes.
The runtime redirects shallow non-owner reads instead of serving unauthoritative
cache entries. Deep reads and
explicit refresh collect authoritative inventories and derive counts and metric
sums from their records. Separately, the written zone's summary folds after
durable apply, same-vnode ancestors fold on the next Update, remote parents
receive a versioned child push, and periodic
Update polls children to repair lost messages. GetAggregates serves the owned
parent cache. An unreachable shard preserves the previous totals instead of
publishing zero. Repeated refreshes are idempotent; successful refreshes include
deletions.
After writes stop and transfers settle, a successful fresh read sees the settled
totals. There is no bounded convergence time under partition and no global
snapshot during concurrent writes.

Parent and child owners are independent XOR evaluations. Aggregates are derived
from records and do not grant authority. See [query.md](query.md).

Physical admission counts live records, logs, retry receipts, staging and incoming
receipt reservation, including persisted snapshot payloads. Control proofs and
B-tree/filesystem overhead need separate headroom; this is not a filesystem quota.
