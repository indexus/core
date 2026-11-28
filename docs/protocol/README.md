# Protocol

**Transition, 25 November 2026.** The target is [local installed permissions and
receiver tickets](local-permissions.md), with [separate qualification](local-permissions-verification.md).
The replacement is wired in the worktree; complete qualification is in progress. Historical global-authority/checkpoint pages
remain decision records; their measurements do not qualify the replacement.

The geometry is deterministic XOR placement with convergent routing membership. A stale route never grants write permission.
The address algebra (ID, zone keys, regions, View/Owner, XOR trie, `Base`
alphabets) lives in `internal/tree`; plans in `internal/protocol` build on it.

The current mechanisms, step by step, are in [../architecture/](../architecture/README.md); each document here carries a status (current, proposed or historical) under its title.

- [local-permissions.md](local-permissions.md): target pairwise handoff, bounded current state, exact read coverage and trust boundary
- [local-permissions-verification.md](local-permissions-verification.md): checked kernels/models and outstanding disk/network bridges
- [authority-frontier.md](authority-frontier.md): historical checkpoint compaction and its trust boundary
- [causal-topology.md](causal-topology.md): replicas, certificates, frontier, ADD and MOVE
- [ownership.md](ownership.md): geometry versus authority
- [handoff.md](handoff.md): copy, mirror, freeze, install and recovery
- [consistency.md](consistency.md): ACK, forwarding and read limits
- [failure-model.md](failure-model.md): assumptions and availability boundaries
- [invariants.md](invariants.md): named safety and operational rules
- [verification.md](verification.md): scoped evidence, not blanket production claims
- [component-boundaries.md](component-boundaries.md): component owners, read surfaces and extreme-load investigation
- [distributed-runtime.md](distributed-runtime.md): startup and storage
- [gossip.md](gossip.md): bounded anti-entropy partners (Kademlia buckets, GOS-1)
- [linear-knowledge.md](linear-knowledge.md): control-plane knowledge linear in the cluster (E1-E5b, MAP-1, MAP-2)
- [scaling.md](scaling.md): validated intents and placement
- [cold-reconfiguration.md](cold-reconfiguration.md): balloon → DesiredTopology → transition planner (design; only the quiet pair fold is code)
- [priorities.md](priorities.md): recovery > hot > cold > background
- [query.md](query.md): derived client tree and cache
- [Local subtree fingerprint](subtree-fingerprint.md) covers guarded reuse, local scope, the portable monoid encoding and interoperability vectors.
- [ixr1.md](ixr1.md): the IXR1 record value stored by zonelog, and the design note that led to it
- [wire-block.md](wire-block.md): the client wire: IXB2 blocks of `/sets`, IXRB records of `/records`
- [handoff-reliability.md](handoff-reliability.md): seeded real-storage fault schedules, replay, fuzzing and local qualification
- [testing.md](testing.md): test levels and fault scenarios
- [aggregation-verification.md](aggregation-verification.md): oracle indépendant, précision et croissance locale
- [read-delegations.md](read-delegations.md): successeurs locaux durables et lectures sans Knowledge par saut
- [read-write-phase-verification.md](read-write-phase-verification.md): lectures en transition et exclusivité du writer
- [minimal-kernel.md](minimal-kernel.md): responsabilités et contrats du noyau actif

The previous quorum design is historical and archived outside the repository.
