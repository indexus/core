# Distributed runtime

> **Historique — remplacé le 25 novembre 2026.** Cette page conserve le
> protocole à preuves/checkpoints et ses outils antérieurs. Le runtime actif du
> worktree utilise [permissions locales et tickets](local-permissions.md) ; sa
> [qualification propre](local-permissions-verification.md) remplace les statuts
> d'implémentation ci-dessous, sans effacer ces décisions historiques.

**Statut : actuel.**

The executable uses only the causal runtime described in
[causal-topology.md](causal-topology.md). No metadata quorum is needed for a local
write, ADD, anti-entropy exchange or authority handoff.

## Start and join

Go 1.26 is the current module requirement. Adapt `ops/config/node.json`:

```sh
go run ./cmd/node check -config node.json
go run ./cmd/node init -config node.json
go run ./cmd/node serve -config node.json
```

For a joining node, set distinct storage/listen addresses and
`network.bootstrap: ["http://127.0.0.1:21000"]`, retain the cluster/network IDs,
and run `serve` without `init`. The same origins drive discovery and authority
anti-entropy. Discovery does not publish an activation. A shadow node can
receive handoff controls before becoming client-ready.

Each maintenance tick exchanges knowledge with about log2(N) partners, one per
XOR bucket, rotating, not with every host. See [gossip.md](gossip.md).

## Storage and restart

Keep `<storage>.causal.db` and `<storage>.causal-identity.json`. The identity file
binds the configured vnode to the disk incarnation before client service starts.
Restart with the same `identity.name` and endpoint (or the same persisted node certificate,
which retains the name). An automatically generated name is not recovered from
the data disk by the CLI. A changed endpoint requires explicit MOVE.
An empty replacement database cannot inherit that identity. Corrupt files are
errors, never instructions to reset state. Restoring or cloning an old live disk
while another copy still serves is outside the single-incarnation failure model.

Earlier storage formats are not read or migrated: a node starts from an empty
data directory, and a file at the store path is refused at open.

## Move a host

Start the receiving host with the **same vnode name**, a new storage path and a
new endpoint, using `node move -from http://old-host:p2pPort -config receiver.json`. The receiver stages bytes,
the source fences and transfers its grants, and a durable placement successor
activates the new disk. Repeat the same request after an ambiguous result. The
old host can forward stale requests and must not be destroyed before success.

MOVE keeps the logical active set unchanged. There is no `SoftLeave` API. Compaction is on by default (`"compaction": false` disables it) and performs opportunistic pair folds (COLD-2) with intake sealing,
survivor pins, durable handoffs and a witnessed causal retirement. Retired
endpoints keep forwarding; no host is shut down. General balloon ADD/MOVE/packing
execution remains unwired. See [cold reconfiguration](cold-reconfiguration.md).
Skeleton refinement has no implementation or placeholder API.

## Causal-only API

There is no legacy runtime switch, quorum runtime or in-memory/WAL fallback.
`NewNode(settings, contactFactory, bootstraps)` creates a facade;
`EnableCausalCluster(service)` attaches its durable engine. Before attachment,
data APIs return `ErrNotReady`. `NewSettings` takes name, port, job delay and
delegation threshold; the former cache expiration parameter is removed.

All authority handoffs use `/cluster/`. The old `/copy`, `/transfer`, `/leave`,
`/queue`, `/checkpoint` and snapshot management endpoints are removed, together
with their S3 dependencies. The `-archive`, `-leaveTimeout`, `-topologyMode` and
`-legacyRuntime` flags are removed. Stopping a process preserves its vnode state
for restart and does not perform physical host drain or automatic termination.
Pure historical kernel models remain regression tests of local statements.

## Limits

The protocol handles crash-recovery peers, not Byzantine participants. Enable
node authentication and TLS for the intended deployment. Proof/fence compaction,
automatic disk failover, user-data replication and logical REMOVE are not supplied
by topology convergence. Queries refuse unavailable shards instead of inventing
complete answers. See [verification.md](verification.md) for measured coverage.
