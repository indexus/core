# Indexus and the state of the art

> **Historical research context, 25 November 2026.** The comparisons and
> dated measurements below are preserved. Its Knowledge-size and authority
> descriptions refer to the predecessor, not the [local-ticket runtime](../protocol/local-permissions.md)
> or its [separate qualification](../protocol/local-permissions-verification.md).

**Statut : proposé.** Note de recherche.

Where Indexus sits among published systems, what a system of its kind is
expected to do, which open-source ideas it can adopt without breaking its
properties, and the benchmark and development plan that follows. Written
2026-11-17 from the protocol documents in [../protocol](../protocol) and the
measurements in [../benchmarks](../benchmarks). References are at the end;
every claim about another system points to one.

## 1. Summary

Indexus is a **range-partitioned store with one writer per zone**, like
Bigtable, HBase, TiKV or CockroachDB, but with a **decentralized control plane
taken from DHTs** (no master, no placement service: XOR ownership and
Kademlia-style gossip) and **locality-preserving keys** (a zone id is its
location bits followed by its collection), with **exact aggregates pushed up
the zone tree**.

Each of these pieces has precedents. Their combination, and in particular
exclusive write authority certified inside the commit transaction without
consensus or coordinator, is less common.

| Measured today | Expected of the family | Gap |
| --- | --- | --- |
| Linear throughput 16 → 64 vnodes on uniform load, flat latency, 0 lost write | Standard for shared-nothing partitioned stores | None on uniform load |
| About 1 000 acknowledged writes/s per vCPU | Tuned engines do more per core | Not compared on the same hardware yet |
| A spike that triggers splits wedges the cluster (REM-121) | Load-based split and merge in production systems | **Blocking** |
| A dead holder makes its zones unavailable; a lost disk loses them | Survive the loss of a minority of replicas | **Blocking** for production (REM-91) |
| Uniform keys only | Skewed keys (Zipf), real spatial data | Not measured |

## 2. What Indexus is, in the terms of this comparison

From [protocol/README](../protocol/README.md), [ownership](../protocol/ownership.md),
[write-authority](../protocol/write-authority.md),
[failure-model](../protocol/failure-model.md), [gossip](../protocol/gossip.md),
[scaling](../protocol/scaling.md) and [query](../protocol/query.md):

- **Geometry.** A zone is `(collection, location)`; its 96-bit id is the
  location's bits followed by the collection's. `owner(z) = argmin XOR(z, V_active)`
  is a pure function of the active set. Nearby locations share id prefixes, so
  a vnode owns a contiguous region of space (a trie cell).
- **Write authority.** An address is only a routing prediction. A write is
  acknowledged only if, inside the transaction that commits it, the local disk
  holds an installed, unconsumed grant for the zone and no fence closes it. A
  handoff runs freeze → stage → emit → install; the guarantee is *at most one
  writer*, with a possible gap with no writer between freeze and install.
- **Elasticity.** HotTrie splits a hot vnode at a canonical bit (`J_b = D xor e_b`),
  Regional ADD adds vnodes inside an ownership-closed prefix, COLD-2 folds cold
  vnodes back. MOVE changes the machine without changing the owner.
- **Knowledge.** Every node keeps the routing map (about 1.15 KB per vnode).
  Anti-entropy contacts one member per Kademlia bucket per round (GOS-1): about
  log2(N) exchanges per node per round instead of N−1.
- **Aggregates.** Each node sums the zones it holds and pushes boundary values
  to the owner of the parent zone, up to the root; polls repair lost pushes.
  Totals are exact once writes stop; there is no bounded convergence time under
  partition.
- **Failure model.** Crash-recovery, honest peers, storage that honors fsync.
  No user-data replication: if the only holder disappears before transferring
  authority, no other node may make itself writable.

Properties this document must not break (later sections test every idea
against them):

| # | Property |
| --- | --- |
| P1 | At most one node can acknowledge a write to a zone, checked in the commit transaction. |
| P2 | An acknowledged write is never lost while its holder's disk survives. |
| P3 | `owner` is a pure XOR function of the active set; heat, capacity and placement never feed it. |
| P4 | No master, no global validator: decisions are local transactions plus certificates. |
| P5 | Aggregates are derived from records and are exact once settled; a cache never grants authority. |
| P6 | Control-plane cost per node grows like log N; knowledge grows like the number of vnodes. |
| P7 | Locality: a spatial prefix maps to one trie cell, so a range or aggregate touches few nodes. |

## 3. Related work by family

### 3.1 Locality-preserving indexes over peer-to-peer networks

Hashing keys, as Chord or Kademlia do, balances load but destroys locality, so
range and spatial queries need a different design.

- **Prefix Hash Tree (PHT).** A trie built on top of an unmodified DHT, made
  for Place Lab's need for range queries; a linearization extension gives
  multi-dimensional and in particular 2-D geographic range queries [1, 2].
  Indexus is close in spirit (a trie over space), but the trie *is* the
  routing geometry instead of a layer over a hashing DHT.
- **P-Grid.** A self-organizing access structure for P2P information systems,
  built and maintained by randomized algorithms using only local interactions
  [3]. Its first paper assumes data are not skewed [3], the assumption Indexus
  cannot make for geographic data.
- **Mercury.** Multi-attribute range queries with contiguous (non-hashed)
  placement, one routing hub per attribute, and explicit load balancing by
  sampling [4].
- **Squid.** A Hilbert space-filling curve maps a multi-dimensional keyword
  space to a 1-D index, placed on Chord [5].
- **SCRAP and MURK.** The comparison of space-filling curves (SCRAP) and kd-tree
  partitioning (MURK) for multi-dimensional P2P queries, with load balancing
  [6]. Indexus's trie cells are a kd-tree-like partition of space, so the MURK
  trade-offs (locality versus balance) apply directly.
- **HyperDex.** Hyperspace hashing maps objects with several attributes into a
  multi-dimensional space where each server owns a region, and value-dependent
  chaining provides consistency and fault tolerance [7]. It is the closest
  "region of space per server, strongly consistent" precedent; unlike Indexus
  it hashes attribute values and uses a coordinator for configuration.

**Lesson for Indexus.** These systems all had to answer skew: what happens when
data or queries concentrate in a small part of the space. The answer is always
an active balancing mechanism, which in Indexus is HotTrie plus COLD-2.

### 3.2 Balancing range-partitioned data

- **Ganesan, Bawa and Garcia-Molina (VLDB 2004)** give online algorithms that
  keep storage balanced at all times for range-partitioned data, even against
  adversarial insertions and deletions, and build a P2P system with range
  queries on them [8]. A follow-up reports their guarantee as a ratio of about
  4.24 between maximum and minimum loads [9].
- **Karger and Ruhl** balance load in DHTs, including a protocol that moves
  nodes to arbitrary locations "where they are needed", and use it for range
  searches on ordered data [10].

**Lesson.** Indexus's HotTrie and Regional ADD are a form of "move nodes where
they are needed" with the extra constraint P3 (the new id is canonical, not
chosen freely). The literature offers provable max/min bounds; Indexus has a
measured bound for joins (cells within a factor 2, JOIN-1) but none yet for a
system under skewed load. That bound is a natural target for a paper.

### 3.3 Hierarchical aggregation

- **Astrolabe** builds a virtual hierarchical database with on-the-fly attribute
  aggregation, using a peer-to-peer protocol with no central servers [11, 12].
- **SDIMS** builds aggregation trees from a DHT, lets applications control how
  reads and writes propagate, and handles reconfigurations with lazy
  re-aggregation, on-demand re-aggregation and tunable spatial replication
  [13, 14].

**Lesson.** Indexus's summary tree is an SDIMS-style tree whose shape is the
geography itself. SDIMS's "control propagation per attribute" is the knob
Indexus lacks for its root hot spot (REM-118): the root owner receives every
summary of a collection. Making propagation adaptive (push eagerly when reads
are frequent, lazily otherwise) is compatible with P5 as long as values stay
versioned.

### 3.4 Membership, dissemination and routing security

- **Kademlia**: XOR metric, buckets by common prefix [15]. Indexus uses the
  metric for ownership and the buckets for anti-entropy partners (GOS-1).
- **SWIM** separates failure detection from membership dissemination; its
  expected detection time and per-member message load do not grow with group
  size, and updates piggyback on probes [16, 17]. HashiCorp's `memberlist` is
  a Go implementation [18].
- **HyParView and Plumtree** maintain partial views for gossip and broadcast
  along a tree embedded in the gossip overlay, using the other links to heal
  the tree quickly [19, 20].
- **S/Kademlia** hardens Kademlia against eclipse attacks, for example by
  making node ids costly to generate and by using disjoint lookup paths [21].

**Lesson.** Indexus's failure model excludes Byzantine peers and authenticates
members when configured, so S/Kademlia matters only for an open network. SWIM
and Plumtree are cost optimizations for knowledge and liveness; neither may
grant authority (P1).

### 3.5 Range-partitioned single-owner stores and auto-sharders

- **TiKV** splits a region by load ("Load Base Split") when its read load stays
  above a threshold for 10 consecutive seconds, choosing a split position that
  balances the two halves, and does not merge such regions quickly [22, 23].
  The default QPS threshold is 3 000 per second for regions under 4 GB [24].
- **CockroachDB** splits ranges by load (default threshold 2 500 QPS) at the
  first set of keys whose operations exceed the threshold, together with
  load-based rebalancing [25, 26]; a newer objective uses request CPU, with a
  default threshold of 250 ms [27].
- **Slicer** (Google) separates a reliable data plane that forwards requests
  from a control plane that makes balancing decisions off the critical path
  [28]. Production workloads keep the most-loaded task at 30 to 180% of the
  mean, even with skewed, time-varying load [29]. **Centrifuge** integrated
  leases and partitioning [30]. **Shard Manager** (Meta) is used by hundreds of
  applications on over a million machines [31].
- **Dicer** (Databricks, open-sourced January 2026) splits, merges and
  reassigns key ranges ("slices") in response to load and hot keys [32]. Unlike
  Slicer's leases for strong ownership, it chose eventually consistent
  assignments, favoring availability and fast recovery [33].

**Lesson.** These systems confirm that load-based split, hysteresis against
split/merge flapping, and a split point chosen from sampled load are standard
and expected. Indexus sits on the Slicer side of the ownership trade-off
(exclusive authority), but without a central control plane: its "control
plane" is local policy plus certificates. Separating the data plane (forwarding,
always on) from the control plane (splits, off the critical path) is exactly
what REM-121 shows is not yet true: a split stalls the write path.

### 3.6 Geospatial data on key-value stores

- **GeoMesa** stores spatio-temporal data in Accumulo, HBase or Bigtable by
  encoding locations along a space-filling curve into sorted keys [34, 35].
- **S2** uses a Hilbert curve on the sphere with 64-bit cell ids; **H3** uses a
  hierarchical hexagonal grid in which all six neighbors of a cell are
  equidistant [36, 37].

**Lesson.** Indexus's location string is itself a hierarchical cell id. S2 or H3
can be used by clients to map coordinates to Indexus locations without touching
the protocol. The choice of curve matters for skew and for range-query
fan-out; Indexus's trie (Z-like prefix order) inherits Z-order's discontinuities
at cell boundaries.

### 3.7 Verification and benchmarking

- **YCSB** defines standard serving workloads (A: 50/50 read/update, B: 95/5,
  C: read only, D: read latest, E: short scans), with Zipfian popularity [38, 39].
- **Jepsen and Elle**: Elle infers Adya-style dependency graphs from
  client-observed histories and detects isolation anomalies [40].
- **FoundationDB** built a deterministic simulation framework before the
  database itself and tests every feature under simulated faults [41, 42].
- **Spatial benchmarks**: SpatialBench (Apache Sedona) uses NYC taxi trips with
  skewed spatial generators, including clustered hot spots with heavy tails
  [43]; GeoBenchr evaluates spatio-temporal platforms with application-centric
  workloads [44].

## 4. What is expected of such a system

Rows are what a reviewer, an operator or a paper committee will ask. "Status"
is today's evidence; "How to show it" is the measurement still missing.

| Expectation | Typical bar in the literature and in production | Indexus status | How to show it |
| --- | --- | --- | --- |
| Single writer per partition | Leases or consensus leaders; checked by fault testing | P1 by construction, normative tests, 0 double writer in every audit | Partition and pause tests (F3, F4) with a linearizability checker per key |
| No acknowledged write lost | Survives node loss with replication factor 3 | Only while the holder's disk survives | Crash tests with audit (F1, F2); then replication (REM-91) |
| Availability under node failure | Minority loss tolerated within seconds | A dead holder's zones are unavailable until it returns | Measure unavailability window (F1, F2); replication to fix |
| Behavior under partition | Documented CAP choice; one side keeps writing | Unfrozen holders keep writing; handoffs wait | F4 with Jepsen-style histories |
| Horizontal scaling | Near-linear on uniform load | 99 to 100% efficiency 16 → 64 | Done; extend to 128, add Q3 (gossip cost) |
| Skew | Zipfian keys (YCSB), spatial clustering | Not measured | YCSB A/B/E with Zipf; SpatialBench-like clustered locations |
| Elasticity | Load-based split within tens of seconds, merges with hysteresis, no write stall | Split under spike wedges the cluster (REM-121) | elasticity.md scenario green, then time to equilibrium |
| Balance | Most-loaded server within a small factor of the mean | 1.03 × mean on uniform load after REM-120 | Same under skew |
| Control-plane cost | O(log N) messages per node, metadata O(partitions) | GOS-1 measured in simulation | Measure on AWS at 16/32/64/128 |
| Queries | Point, range, aggregates; documented freshness | Exact aggregates, root lag ≤ 2 s below saturation | Range-query latency vs area; freshness under skew |
| Efficiency | Comparable to peers on the same hardware | About 1 000 writes/s per vCPU | Same-hardware baseline (section 6) |
| Recovery | Restart and catch-up in bounded time | Restart keeps open handoffs open (REM-121) | Restart under load, time to one knowledge version |
| Security | Authenticated members; open networks need Sybil/eclipse defenses | Authenticated peers, non-Byzantine | State the model; S/Kademlia only if open |
| Operability | Metrics, traces, safe upgrades | Rich `/metrics`, sentinel | Rolling upgrade test |

## 5. Open-source ideas that keep Indexus's properties

Each idea is checked against P1 to P7. "Change" says what part of Indexus it
touches.

| Idea | Source | What it brings | Change | Properties at risk | Verdict |
| --- | --- | --- | --- | --- | --- |
| Split by sustained load, not by a single threshold crossing; split point from sampled keys | TiKV Load Base Split [22, 23]; CockroachDB [25, 26] | Avoids splitting on bursts; balanced halves | HotTrie trigger only | None if the split bit stays canonical (P3) | **Adopt**: sustained window (for example 10 s) plus sampling of which side of each candidate bit takes the load |
| Do not merge recently split or still-hot cells | TiKV [22, 23] | Prevents split/fold flapping | COLD-2 policy | None | **Adopt** |
| Data plane never waits on the control plane | Slicer [28] | Writes continue while a split prepares | Handoff implementation | P1 if the freeze window grows | **Adopt as a design rule**; REM-121 violates it today |
| CPU as split signal | CockroachDB [27] | Splits what is actually expensive | Trigger | None | Later, after items/QPS |
| Failure detection with indirect probes | SWIM, Lifeguard, `memberlist` [16, 18] | Fast, low-cost suspicion of dead holders | Observability, routing hints | P1 if suspicion were used to take over | **Adopt as advisory only**: suspicion may reroute reads and alert, never grant a write |
| Tree broadcast for knowledge deltas | Plumtree, HyParView [19, 20] | Fewer duplicate knowledge messages | Gossip transport | None (knowledge only) | Consider once Q3 is measured |
| Propagation control for aggregates | SDIMS [13, 14] | Fixes the root hot spot (REM-118) without losing exactness | Summary push policy | P5 if versions are dropped | **Adopt** with versioned contributions |
| Replicated log per placement group | Raft via `etcd-io/raft` or `dragonboat` [45, 46] | Survive disk loss (REM-91) | Storage and authority | P1 unless grants bind to the group | Already the recommendation of [design/replication](../design/replication.md); pick a library |
| Deterministic simulation with fault schedules | FoundationDB [41, 42] | Reproducible rare interleavings (REM-121 class) | Test harness | None | **Extend** `internal/simulation` with crash/restart during handoff |
| History checking | Jepsen, Elle [40]; a per-key linearizability checker such as Porcupine (Go) | Proof that P1/P2 hold under faults, from client histories | Experiment tooling | None | **Adopt** for F1 to F6 |
| Network faults | Toxiproxy (TCP only) [47]; `tc netem` for UDP/QUIC | Partitions and latency | Experiment tooling | None | Toxiproxy for `NET=tcp`; netem or the agent for HTTP/3 |
| Standard and spatial workloads | YCSB [38, 39]; SpatialBench generators [43] | Comparable, skewed loads | Load generator | None | **Adopt** in `cmd/experiment load` |
| Client-side cells | S2, H3 [36, 37] | Real coordinates to locations | SDK | None | Optional adapter |
| Hashing collection or location into the id | Consistent hashing | Spreads roots | Identity scheme | **P7, P3** | **Reject**: breaks locality and existing ids |
| Eventually consistent ownership | Dicer [32, 33] | Availability during reassignment | Authority model | **P1** | **Reject** for writes; acceptable for read caches only (already the read-mode design) |
| Costly node ids, disjoint lookups | S/Kademlia [21] | Open-network security | Identity and routing | P3 if ids stop being canonical | Defer until an open network is a goal |

## 6. A complete benchmark

The existing protocol ([benchmarks/protocol.md](../benchmarks/protocol.md):
passes, noise, audits) stays. What it adds:

**Workloads.**

| Name | Keys | Operations | Why |
| --- | --- | --- | --- |
| U (today) | uniform locations, 256 collections | writes | scaling baseline |
| Z-A / Z-B | Zipf-popular locations | 50/50 and 95/5 read/write (YCSB A, B) | skew, comparable with other stores |
| Z-E | Zipf-popular prefixes | short prefix scans plus inserts (YCSB E) | range queries |
| G | NYC TLC pickups or OSM nodes, real coordinates | inserts, area reads, aggregates | real spatial skew |
| G-hot | clustered generator with heavy tail | inserts | hot spots that force splits |
| Spike | 10× on one prefix, then calm | writes | elasticity ([benchmarks/elasticity.md](../benchmarks/elasticity.md)) |
| Faults F1 to F7, C1 to C6 | U and G | writes plus reads | [benchmarks/faults.md](../benchmarks/faults.md) |

Collection names are dispersed in every workload, so roots spread as they
would in a real deployment (REM-118).

**Metrics.** Acknowledged writes/s and reads/s, p50/p99 per operation, ack
ratio, max/mean load per node, splits and folds per minute, time to
equilibrium, unavailability window per fault, root freshness, gossip messages
and bytes per node, knowledge bytes per vnode, and for every run: audit
(0 lost, 0 double writer, exact totals) and a history check.

**Baselines on the same machines.**

| Baseline | Why | Compare on |
| --- | --- | --- |
| CockroachDB or TiKV | same single-owner semantics with Raft | U, Z-A/B/E, Spike, F1/F2 |
| Cassandra or ScyllaDB | raw write throughput per core | U, Z-A |
| GeoMesa on HBase | geospatial indexing on a sorted KV | G, area reads |

Baselines run with their documented defaults and replication factor 1 where
possible, and also with 3, so the cost of Indexus's missing replication is
visible rather than hidden.

**Sizes and cost.** 16 / 32 / 64 vnodes on i4i.xlarge as today; one baseline
per family at 16 nodes first. With the measured per-hour cost of 16 nodes
(about 8 $/h with load machines), each baseline in passes costs a few dollars.

## 7. Recommended development before going further

In order; each step keeps P1 to P7.

1. **Make splits never stall writes** (REM-121, REM-88). Reproduce the stuck
   writer lock with stacks on every node and a mutex profile, then reduce to a
   deterministic simulation test (section 5, FoundationDB idea). Rule from
   Slicer: the data plane never waits on a split in preparation. Done when the
   local spike scenario is green and folds back.
2. **Fault harness with history checking.** Partitions (netem or agent),
   pauses, crash during each handoff step, and a per-key linearizability check
   of client histories. Done when F1 to F6 are green with a checked history.
3. **Load-based split trigger with hysteresis.** Sustained window, sampled
   split side, no fold of recently split or hot cells (TiKV, CockroachDB).
   Measured on G-hot and Spike.
4. **Aggregate propagation control** for the root (SDIMS idea), with dispersed
   collection names in the generator.
5. **Replication** as designed in [design/replication](../design/replication.md),
   on an existing Raft library; authority bound to the replicated group.
6. **Benchmark against baselines** (section 6), then 128 vnodes.

## References

1. Chawathe, Ramabhadran, Ratnasamy, LaMarca, Shenker, Hellerstein. A Case Study in Building Layered DHT Applications. SIGCOMM 2005. https://www.read.seas.harvard.edu/~kohler/class/cs239-w08/chawathe05case.pdf
2. Ramabhadran, Ratnasamy, Hellerstein, Shenker. Prefix Hash Tree: An Indexing Data Structure over Distributed Hash Tables. 2004. https://bibbase.org/network/publication/ratnasamy-hellerstein-shenker-rangequeriesoverdhts-2003
3. Aberer. P-Grid: A Self-organizing Access Structure for P2P Information Systems. CoopIS 2001. https://infoscience.epfl.ch/record/54335/files/COOPIS2001.pdf
4. Bharambe, Agrawal, Seshan. Mercury: Supporting Scalable Multi-Attribute Range Queries. SIGCOMM 2004. https://www.comp.nus.edu.sg/~bleong/hydra/related/bharambe04mercury.pdf
5. Schmidt, Parashar. Flexible Information Discovery in Decentralized Distributed Systems. HPDC 2003. https://www.comp.nus.edu.sg/~cs6203/guidelines/topic3/range/hilbert.pdf
6. Ganesan, Yang, Garcia-Molina. One Torus to Rule Them All: Multi-dimensional Queries in P2P Systems. WebDB 2004. https://www.comp.nus.edu.sg/~cs6203/guidelines/topic3/multi-dimension/one-torus-to-all.pdf
7. Escriva, Wong, Sirer. HyperDex: A Distributed, Searchable Key-Value Store. SIGCOMM 2012. https://cs.uwaterloo.ca/~bernard/hyperdex.pdf
8. Ganesan, Bawa, Garcia-Molina. Online Balancing of Range-Partitioned Data with Applications to Peer-to-Peer Systems. VLDB 2004. https://www.vldb.org/archives/website/2004/protected/eProceedings/contents/pdf/RS11P3.PDF
9. Chawachat, Fakcharoenphol. A simpler load-balancing algorithm for range-partitioned data in P2P systems. 2012. https://api.emergentmind.com/papers/1210.7954
10. Karger, Ruhl. Simple Efficient Load Balancing Algorithms for Peer-to-Peer Systems. IPTPS / SPAA 2004. https://people.csail.mit.edu/ruhl/papers/2004/loadbalancing-iptps.html
11. van Renesse, Birman, Vogels. Astrolabe. ACM TOCS 21(2), 2003. https://typeset.io/pdf/astrolabe-a-robust-and-scalable-technology-for-distributed-2681334o7d.pdf
12. Birman, van Renesse, Vogels. Navigating in the Storm: Using Astrolabe. 2003. https://research.cs.cornell.edu/projects/Quicksilver/public_pdfs/Navigating%20in%20the%20Storm.pdf
13. Yalagandula, Dahlin. A Scalable Distributed Information Management System. SIGCOMM 2004. https://bohrium.dp.tech/paper/arxiv/53e9a914b7602d9703268323
14. SDIMS project page. https://www.cs.utexas.edu/~dahlin/projects/sdims
15. Maymounkov, Mazières. Kademlia: A Peer-to-Peer Information System Based on the XOR Metric. IPTPS 2002. (cited in [21])
16. Das, Gupta, Motivala. SWIM. DSN 2002. https://paperswelove.org/papers/swim-scalable-weakly-consistent-infection-style-pr-3d4b34e2/
17. SWIM (overview). https://en.wikipedia.org/wiki/SWIM_Protocol
18. HashiCorp memberlist. https://github.com/hashicorp/memberlist
19. Leitão, Pereira, Rodrigues. Epidemic Broadcast Trees. SRDS 2007. https://webarchive.di.uminho.pt/haslab.uminho.pt/jop/publications/export/tagged/647
20. Leitão, Pereira, Rodrigues. HyParView. DSN 2007. https://asc.di.fct.unl.pt/~jleitao/dsn07-leitao.php
21. Baumgart, Mies. S/Kademlia: A Practicable Approach Towards Secure Key-Based Routing. ICPADS 2007. https://telematics.tm.kit.edu/publications/Files/267/SKademlia_slides_2007.pdf
22. TiDB docs: Load Base Split (v5.0). https://docs.pingcap.com/tidb/v5.0/configure-load-base-split
23. TiDB docs: Load Base Split (v4.0 archive). https://docs-archive.pingcap.com/tidb/v4.0/configure-load-base-split/
24. TiDB docs: Load Base Split (dev). https://docs.pingcap.com/tidb/dev/configure-load-base-split/
25. CockroachDB docs: Load-Based Splitting (v23.2). https://www.cockroachlabs.com/docs/v23.2/load-based-splitting
26. CockroachDB docs: Load-Based Splitting (v20.1). https://www.cockroachlabs.com/docs/v20.1/load-based-splitting
27. CockroachDB docs issue on CPU-based load splits (cockroach PR #96128). https://github.com/cockroachdb/docs/issues/16243
28. Adya et al. Slicer: Auto-Sharding for Datacenter Applications. OSDI 2016. https://www.usenix.org/conference/osdi16/technical-sessions/presentation/adya
29. Google Research, Slicer abstract. https://research.google/people/daniel-myers
30. Adya, Dunagan, Wolman. Centrifuge: Integrated Lease Management and Partitioning for Cloud Services. NSDI 2010. (cited in [31] references) https://api.crossref.org/works/10.1145%2F3477132.3483546
31. Lee et al. Shard Manager: A Generic Shard Management Framework for Geo-distributed Applications. SOSP 2021. https://www.scilit.net/article/e0430ee436714054b71bbff93f37b3df
32. Databricks. Open Sourcing Dicer: Databricks' Auto-Sharder. 2026. https://fooqux.com/article/1907
33. Dicer and Slicer consistency trade-off. https://sysdesign-wiki.pages.dev/systems/slicer/
34. GeoMesa architecture. https://www.geomesa.org/documentation/3.0/user/architecture.html
35. GeoMesa architecture, Z-curves. https://geomesa.org/documentation/3.3/user/architecture.html
36. Geospatial indexing: geohash, quadtree, R-tree, S2, H3. https://handbook-hld.pages.dev/curriculum/building-blocks/geospatial-indexing/
37. H3 spatial index. https://carto.com/glossary/h3-spatial-index/
38. Cooper et al. Benchmarking Cloud Serving Systems with YCSB. SoCC 2010. https://puma.uni-kassel.de/layout/htmltype/tag/vergleich
39. YCSB core workloads. https://www.voltactivedata.com/?p=15922
40. Kingsbury, Alvaro. Elle: Inferring Isolation Anomalies from Experimental Observations. PVLDB 14(3), 2021. https://www.vldb.org/pvldb/volumes/14/paper/Elle%3A%20Inferring%20Isolation%20Anomalies%20from%20Experimental%20Observations
41. Zhou et al. FoundationDB: A Distributed Unbundled Transactional Key Value Store. SIGMOD 2021. https://muratbuffalo.blogspot.com/2022/03/foundationdb-distributed-unbundled.html
42. FoundationDB, CACM research highlight. https://cacm.acm.org/?p=227399
43. Apache Sedona SpatialBench. https://sedona.apache.org/spatialbench/datasets-generators/
44. GeoBenchr. 2026. https://arxiv.org/pdf/2603.09398v1
45. etcd-io/raft and Go Raft libraries. https://pkg.go.dev/go.etcd.io/etcd/raft
46. Dragonboat, multi-group Raft in Go. https://sourceforge.net/projects/dragonboat.mirror/
47. Shopify Toxiproxy. https://dev.co/testing/open-source/toxiproxy
