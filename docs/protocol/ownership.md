# Geometry, routing and write authority

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Statut : actuel.**

`Owner(z,V) = argmin XOR(z,V)`. The 96-bit identity encoding, XOR comparison,
HotTrie and regional geometry remain in `internal/protocol`. Capacity, heat,
host placement, the GPS tree and child markers are never inputs to `Owner`.

Each node has a local active view supplied by `TopologyReplica.Current()`. Views
may differ until anti-entropy delivers the missing activations. Equal active sets
produce equal owners. Registration and discovery are address books, not activation.
The set is nonempty because the pinned genesis vnode cannot be removed in v1.
Duplicate vnode names or identities with conflicting definitions are rejected.

The view is a routing prediction. ACK permission comes from an installed durable
grant, its unconsumed certificate chain, the local region fence and the current
physical disk incarnation. A holder may forward along the chain even while its
view still names an earlier route. After reconciliation, authority and the
converged XOR geometry agree.

A client writer may cache the causal active view and physical placement chain to
route directly. It sends its frontier with every request and merges the returned
delta. A stale route is corrected through durable forwarding; the same request ID
is preserved, so learning a route never creates a second logical write. If the
cached address is unreachable, the writer refreshes from another configured seed.
This cache contains no authority token and cannot turn a stale destination into a
writer.

Independent zones can be written in parallel on independent holders. A single
atomic zone still has one durable writer, so adding vnodes alone cannot make that
zone scale linearly; it must first be split by a valid HotTrie transition.

Authority regions include absent/future keys. They are fencing scopes, not a
second ownership heuristic. `View.Owner` remains pure and unchanged.

The kernel `TopologyStore.CAS` model of local transitions was never the CRDT runtime's activation mechanism and was removed on 2026-11-13. See [causal-topology.md](causal-topology.md).
