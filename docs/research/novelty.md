# What Indexus does that its closest relatives do not

**Statut : proposé.** Note de recherche.

Companion to [state-of-the-art.md](state-of-the-art.md). Question: does the
Indexus algorithm already exist somewhere? Method: for each mechanism, find
the closest published system and state what is the same and what differs.
Written 2026-11-17. "Not found" means not found in this search, not proven
absent.

## Closest relatives

| System | What it shares with Indexus | What differs |
| --- | --- | --- |
| **Walkad** (Varvello, Diot, Biersack, IPTPS / INFOCOM 2009) [1, 2] | Space cut by a binary trie; cell keys placed in the Kademlia key space; a cell's coordinator is the XOR-closest peer to its key; split above a count threshold, merge below a lower one (hysteresis) | Peers keep **random** ids; cell ids are Gray-coded and XORed with a region id for balance; R replicas with no described update protocol; no aggregates; emulated with up to 1 024 peers |
| **Kademlia soft partitioning** (Protocol Labs / NOVA, ICDCSW 2022) [3] | A geographic or application prefix is prepended to Kademlia ids so that nearby content lands on nearby ids | Locality for lookups only; no ownership guarantee, no split, no aggregates |
| **Geodemlia** (Groß et al., P2P 2012/2013) [4, 5] | Kademlia-inspired overlay for location search | Random ids; routing by direction and distance; objects written once, a change is a new object; no load-balancing mechanism |
| **CAN** (Ratnasamy et al., SIGCOMM 2001) [6, 7] | Space partitioned into zones; a join splits an existing zone in half and transfers its keys | Keys are hashed to points (no geographic meaning); neighbor routing in d dimensions, not XOR |
| **Distributed quadtree** (Tanin, Harwood, Samet, VLDB J. 2007) [8, 9] | Regular recursive decomposition of space assigned to peers | Layered over Chord; quadtree cells hashed to the ring |
| **Scatter** (Glendenning et al., SOSP 2011) [10, 11] | Treats "each key managed by at most one node" as the key correctness property; split, merge and repartition of key ranges | Gets it with Paxos groups and 2PC between groups, which also gives availability |
| **Etna** (Muthitacharoen, Gilbert, Morris, 2005) [12] | All reads and writes for a configuration go through one primary | Replicated, quorum reads, configuration changes by consensus |
| **SDIMS** (SIGCOMM 2004) and **Astrolabe** | Aggregates up a tree with no central server | Tree derived from hashed attribute names or administrative zones, not from geography |
| **Moving-object DHT meta-index** (Hernández et al., Euro-Par 2008) [13, 14] | Spatial partitions plus a distributed index that gives exact and approximate aggregation | Separate meta-index on a DHT; aggregates per server |
| **Storage-enforced fencing** (common practice) [15, 16] | The storage path rejects a write unless it carries the current epoch, in the same atomic step | The epoch is normally issued by a consensus-backed lock service |

## Mechanism by mechanism

**1. Geographic prefix plus XOR ownership. Exists.** Walkad already makes
the coordinator of a spatial cell the XOR-closest peer to the cell's key [1],
and soft partitioning prefixes Kademlia ids with a region [3]. Indexus is not
first here.

**2. Splitting by placing a vnode. Partly new.** In Walkad the cell trie is
maintained separately from peer ids (cells split by object count, peers are
random) [1]. In CAN a joiner splits a zone, but in a hashed coordinate space
[7]. Karger and Ruhl move nodes "where they are needed" in a 1-D ring.
Indexus splits by adding a vnode at a canonical id (`J_b = D xor e_b`), so the
partition of space *is* the active vnode set: there is no second structure
(cell trie, ring map) to keep consistent. Not found elsewhere in this exact
form; it is a CAN-style split expressed in Kademlia's XOR geometry.

**3. One function for everything. Not found as such.** In Indexus, the same
pure function `argmin XOR(id, V_active)` gives the owner of a zone, the target
of a split, and the owner of the parent zone, which is where a summary is
pushed. Walkad, SDIMS and the distributed quadtree each need a separate
mechanism for at least one of these (cell labels, attribute hashing, hashed
control points). This is the most distinctive design property found.

**4. Exact aggregates on the geographic hierarchy. Probably new as a
combination.** Hierarchical aggregation without servers exists (Astrolabe,
SDIMS), and so do spatial aggregates over a DHT through a meta-index [13].
An aggregation tree that is the geographic zone tree itself, with parents
found by the ownership function and exactness checked by audit, was not
found.

**5. Write authority without consensus. The pattern exists; its use here
is unusual.** Storage-enforced fencing is standard: the store checks the
writer's epoch atomically with the write [15, 16]. Scatter shows how to keep
"at most one node per key" in a DHT, using Paxos and 2PC [10, 11]. Indexus
checks a grant in the commit transaction (fencing at the store) but produces
grants by peer-to-peer handoff, with no lock service and no consensus. The
price is explicit: a dead holder's zones are unavailable. So this is a
trade-off point (safety without a coordinator, availability given up), not a
new primitive.

**6. Logarithmic anti-entropy over a full routing map.** Both pieces are
known (Kademlia buckets; one-hop DHTs that keep full membership). The
combination is an engineering choice, not a contribution.

## Second pass: older and adjacent families

A second search, aimed at each claimed property, weakens two of them.

| Property claimed | Closest precedent | Consequence |
| --- | --- | --- |
| Split by placing a vnode; the partition *is* the vnode set | **P-Grid** (Aberer et al., 2001–2003) [17, 18]: each peer's binary path is its share of the trie; peers split or join subspaces by adding or removing a bit; the trie construction is unique, so no global coordination is needed | **Not new.** Indexus differs in how the path is chosen (canonical XOR id, not randomized bilateral exchanges) |
| One function also gives the aggregation parent | **DAT** (Cai and Hwang, IPDPS 2007) [19]: aggregation trees built implicitly from native Chord routing paths, with no parent-child membership to maintain | **Partly new.** Routing-implied aggregation trees exist; an aggregation tree that is the *geographic* hierarchy was still not found |
| Single writer without coordinator | **Token-based mutual exclusion** (Suzuki–Kasami 1982 and successors) [20, 21]: only the token holder may enter; regenerating a lost token needs consensus or global cooperation. **Li and Hudak** shared virtual memory (1986/1989) [22]: one owner per page, ownership moves between processors, distributed managers. | **The principle is not new.** Indexus's version is a durable token per zone, checked in the commit transaction, located by a pure function. The price (lost holder = unavailable until consensus or return) is the classic token-loss problem |
| Single writer without coordinator (counter-examples) | **Orleans** grain directory: coordinator-free, but admits duplicate activations when the cluster is unstable; v9 moved to a strongly consistent directory [23, 24]. **AT Protocol**: one signer per repository with revisioned signed commits, but authority changes go through the DID document, and the PLC directory is described as authoritative, like DNS roots [25, 26] | Most systems either give up the guarantee or keep a directory. Indexus keeping it without either is a real but narrow distinction |

## Verdict

After the second pass: the split-by-placement property belongs to P-Grid,
and coordinator-free exclusivity is token passing. What remains distinctive
is (a) exact aggregates carried by the geographic hierarchy and (b) the
integration: one function for ownership, split and aggregation parent, with a
durable token checked at commit. First-pass wording follows.

No single mechanism is new: XOR ownership of spatial cells (Walkad),
zone splitting on join (CAN), storage-enforced fencing, and serverless
hierarchical aggregation all exist. What was not found is the combination
organised around **one pure function of the active vnode set** that
decides ownership, splitting and the aggregation parent at the same time,
with **write exclusivity enforced at commit and no coordinator**, and with
**exact aggregates** on the geographic hierarchy.

What a reviewer would accept today, and what not yet:

| Claim | Evidence today | Status |
| --- | --- | --- |
| One function decides ownership, split and aggregation parent | Protocol docs, normative tests | Can be claimed |
| At most one writer per zone without a coordinator | Tests, 0 double writer in every audit | Can be claimed, with partition tests still missing |
| Linear scaling with flat latency | 16 → 64 vnodes, 99–100% efficiency | Can be claimed on uniform load |
| Elastic under hot spots | REM-121 open | Not yet |
| Survives node loss | No replication | Not yet; Scatter and Etna are the comparison |
| Better than Walkad or Scatter | No same-workload comparison | Not yet |

A paper framed as "a geographic store where placement, splitting and
aggregation follow from one function, with coordinator-free single-writer
safety" is defensible after REM-121 and the partition tests. A claim of
availability needs replication first.

## References

1. Varvello, Diot, Biersack. A Walkable Kademlia Network for Virtual Worlds. IPTPS 2009. https://www.usenix.org/legacy/events/iptps09/tech/full_papers/varvello/varvello_html/
2. Same, INFOCOM Workshops 2009. https://www.eurecom.fr/en/node/4905152
3. Enriching Kademlia by Partitioning. ICDCSW 2022. https://asc.di.fct.unl.pt/~jleitao/ngstorage/pubs/akos2022.pdf
4. Groß et al. Geodemlia: A Robust Peer-to-Peer Overlay Supporting Location-Based Search. P2P 2012. https://www.kom.tu-darmstadt.de/papers/GSR+12.pdf
5. Groß et al. Geodemlia: Persistent Storage and Reliable Search. P2P 2013. https://www.kom.tu-darmstadt.de/papers/GRS+13-2.pdf
6. Ratnasamy, Francis, Handley, Karp, Shenker. A Scalable Content-Addressable Network. SIGCOMM 2001. https://cishell.wiki.cns.iu.edu/Can_6422622.html
7. Content-addressable network. https://en.wikipedia.com/wiki/Content-addressable_network
8. Tanin, Harwood, Samet. Using a Distributed Quadtree Index in Peer-to-Peer Networks. VLDB J. 16(2), 2007. https://umiacs.umd.edu/node/16846
9. Same, summary. https://www.bohrium.com/paper-details/using-a-distributed-quadtree-index-in-peer-to-peer-networks/812108934063063040-2524
10. Glendenning, Beschastnikh, Krishnamurthy, Anderson. Scalable Consistency in Scatter. SOSP 2011. https://homes.cs.washington.edu/~tom/pubs/scatter.pdf
11. Paper summary, Scatter. https://muratbuffalo.blogspot.com/2019/05/paper-summary-scalable-consistency-in.html
12. Muthitacharoen, Gilbert, Morris. Etna. MIT-CSAIL-TR-2005-044. https://dspace.mit.edu/entities/publication/6549a7ed-750c-400c-b55a-bf7f495b5854
13. Hernández, Rodríguez, Marin. Complex Queries for Moving Object Databases in DHT-Based Systems. Euro-Par 2008. https://www.academia.edu/8353869/Complex_Queries_for_Moving_Object_Databases_in_DHT_based_Systems
14. Same, Springer. https://rd.springer.com/content/pdf/10.1007/978-3-540-85451-7_46.pdf
15. Split brain and fencing tokens. https://codemia.io/archive/split-brain-and-fencing-tokens
16. Write fences after failover. https://www.designgurus.io/answers/detail/how-do-you-implement-write-fences-postfailover-to-prevent-stale-writes
17. Aberer et al. P-Grid: A Self-organizing Structured P2P System. SIGMOD Record 2003. https://people.csail.mit.edu/pcm/papers/SIGMOD2003.pdf
18. Punceva, Aberer. Efficient Search in Structured P2P Systems: Binary v.s. k-ary Unbalanced Tree Structures. DBISP2P 2003. https://infoscience.epfl.ch/record/54386/files/DBIS_2003.pdf
19. Cai, Hwang. Distributed Aggregation Algorithms with Load-Balancing for Scalable Grid Resource Monitoring. IPDPS 2007. https://www.cecs.uci.edu/~papers/ipdps07/pdfs/IPDPS-1569010669-paper-1.pdf
20. Suzuki–Kasami algorithm. https://en.wikipedia.org/wiki/Suzuki%E2%80%93Kasami_algorithm
21. Nishio, Li, Manning. A resilient mutual exclusion algorithm for computer networks. IEEE TPDS 1(3), 1990. https://manuscript.isc.ac/Inventory/49/1263715.htm
22. Li, Hudak. Memory Coherence in Shared Virtual Memory Systems. ACM TOCS 1989. https://pdos.csail.mit.edu/6.824/papers/li-dsm.pdf
23. Orleans grain directory. https://learn.microsoft.com/dotnet/orleans/host/grain-directory
24. Orleans grain directory implementation (v9). https://learn.microsoft.com/en-us/dotnet/orleans/implementation/grain-directory
25. Newbold, Holmgren. Authenticated Transfer: Architecture Overview. IETF draft, 2025. https://www.ietf.org/archive/id/draft-newbold-at-architecture-00.html
26. Bluesky and the AT Protocol: Usable Decentralized Social Media (review). https://www.themoonlight.io/review/bluesky-and-the-at-protocol-usable-decentralized-social-media
