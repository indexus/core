# Indexus benchmarks: reference documents

La [campagne de recherche comparative](search-campaign.md) décrit les paliers de 100 k à 10 M, les oracles et le pilote local sur deux pairs.

Le [passage AWS du 27 novembre avec un seuil de 100 écritures/s par vnode](aws-placement-100-2026-11-27.md)
atteint le palier de 16 000/s. Les cartes de trafic, le coût d’un déplacement
et le rejeu du placement expliquent où subsiste la pression physique.

Le [passage AWS du 27 novembre](aws-elastic-direct-2026-11-27.md), sur
`124ccdd`, est arrêté à 4k/s. Il sépare le p99 client en secondes du handler
serveur en dizaines de millisecondes, et reproduit un coût important du cache
de routes du générateur. Il ne qualifie pas 16k/s ; sa flotte est supprimée.

La [passe suivante sur l’élasticité](elastic-anticipation-2026-11-26.md) corrige
les curseurs invalidés par la maintenance, diffère les copies non admises,
et prépare davantage de vnodes initiaux. Elle documente le coût ajouté aux
écritures et sépare les contrôles locaux d’une qualification AWS.

Le [nouveau passage AWS direct](aws-settled-direct-2026-11-26.md) est arrêté
après saturation à 4k/s ; il ne qualifie pas 16k/s et son audit est incomplet.
La [qualification des trois commits de staging](claude-copies-2026-11-26.md)
sur `36e2b31` échoue sur un total exact 503, malgré tous les ACK relus.

Le [passage local avec audit corrigé du 26 novembre](settled-demand-2026-11-26.md)
vérifie 17 999 ACK, les totaux exacts et la prise de trafic avant la fin des
joins ; il conserve les limites des compteurs consultatifs et des ressources
partagées. Il ne constitue pas une nouvelle qualification AWS.

La campagne locale du protocole à permissions locales est décrite dans [qualification du 25 novembre 2026](local-permissions-20261125.md). Elle distingue les validations de sûreté, la disponibilité HTTP64 et les mesures de coût.
La comparaison locale p50/p95/p99 et la préparation des paires AWS figurent dans [latences avant/après](local-latency-20261125.md).

Le [contrôle du p99 par taille de réseau](latency-vs-nodes-20261125.md) sépare
le régime installé des joins et vérifie CPU, mémoire, GC, loopback et fsync.
Il précise pourquoi les 964 ms agrégées ne mesurent pas 64 nœuds installés.

La [réduction des redécouvertes](summary-work-20261125.md) détaille les boucles,
les corrections, les garanties conservées et les comparaisons locales.

> **Mesures historiques — contexte du 25 novembre 2026.** Les chiffres et
> profils de contrôle restent ceux des binaires, machines et dates indiqués dans
> cette page. Ils ne qualifient pas le [nouveau protocole local](../protocol/local-permissions.md).
> Sa [qualification distincte](../protocol/local-permissions-verification.md)
> ne transpose ni débit AWS ni temps de gel anciens.

What the protocol has been measured to do, on which machines, from which
commit, with how much noise. Method for every figure: [protocol.md](protocol.md).

## Summary of measured capabilities

| Capability | Measured | Conditions | Source |
| --- | --- | --- | --- |
| Sustained acknowledged writes | 190 700/s on 64 vnodes (2 of 3 passes), 96 000/s on 32, 48 000/s on 16 (3 of 3, CV 0.0%) | i4i.xlarge, one vnode each, 3 000 offered/s per vnode | [scaling.md](scaling.md) |
| Linear scaling 16 → 64 vnodes | 99 to 100% per-vnode efficiency at 1 000, 2 000 and 3 000/s per vnode; write p99 unchanged. One pass of three at 64 was red from an unbalanced join layout (REM-120) | same | [scaling.md](scaling.md) |
| Durability under saturation | 0 acknowledged write lost, 0 zone with two writers, in every step | same | same |
| Linear scaling 4 → 32 vnodes (ramp) | **96.2%** efficiency (14 492/s → **111 579/s**, peak **141 647/s**, flat **262 ms** intended p99; 4 → 16 is 99.5%) | i4i.xlarge, one vnode each, H3/QUIC, fsync | [ramp.md](ramp.md) |
| Live elasticity (linear `d=60s`, instant 2× `RAMP_SHARE=0`) | 4 → 8 vnodes at 22 386/s (`d=60s`), 4 → 11 at 16 004/s (instant 2×, 65.5 ms p99); all doublings absorbed in 0.0 s | same, on-demand EC2 NVMe | [ramp.md](ramp.md) |
| Linear scaling 4 → 16 vnodes (earlier pass) | per-vnode capacity equal at 4, 8 and 16 vnodes | 16 × c6i.xlarge, 2026-11-16, one pass | [p2p-findings-2026-11-15.md](p2p-findings-2026-11-15.md), campaign 8 |
| Load balance | busiest node 3.2% of mesh requests at 32 vnodes (max/mean 1.03) | same as above | [scaling.md](scaling.md) |
| Root-total freshness | p50 ≤ 524 ms, p99 ≤ 2.1 s up to 2 000 writes/s per vnode | 16 × i4i.xlarge | [freshness.md](freshness.md) |
| Write latency at fixed load per vnode | p99 ≤ 16 ms at 1 000/s per vnode, ≤ 33 ms at 2 000/s | 16 × i4i.xlarge | [freshness.md](freshness.md) |
| Cost | about 10 000 acknowledged writes/s per dollar-hour of node machines; about 1 000 writes/s per vCPU | i4i.large, i4i.xlarge, i4i.2xlarge, eu-west-3 on demand | [cost.md](cost.md) |

## Documents

| Document | Question | State |
| --- | --- | --- |
| [protocol.md](protocol.md) | How is a figure produced, and how noisy is it? | written |
| [freshness.md](freshness.md) | How late is a collection total? | written: 16 vnodes, before/after REM-112 |
| [scaling.md](scaling.md) | Does throughput grow linearly with vnodes up to 64? | written: 16 and 32; 64 to measure |
| [cost.md](cost.md) | Which machine and vnodes per machine give the most writes per dollar? | written: 3 shapes; i4i.large to measure |
| [ramp.md](ramp.md) | Does throughput scale linearly up to the knee, and how fast does elastic capacity follow 2× growth? | written: local + AWS 4→16 fixed and live elastic (`d=120s`, `d=60s`, instant 2×) |
| [elasticity.md](elasticity.md) | Does a 10× spike split and calm fold back? | scenario spec; live elastic ramps measured in [ramp.md](ramp.md) |
| [faults.md](faults.md) | Crashes, restarts, partitions, hostile clients | to measure |
| [p2p-experiment.md](p2p-experiment.md) | The questions and pass criteria | plan |
| [p2p-findings-2026-11-15.md](p2p-findings-2026-11-15.md) | Defects found and fixed while scaling | log |
| [storage-stall-2026-11-16.md](storage-stall-2026-11-16.md) | Storage stalls behind merges | log |

## Known limits

- The owner of a collection's root receives every summary of that collection.
  At 3 000 writes/s per vnode on 16 nodes its handling time reaches 100 to
  500 ms, pushes time out and back off, and the root total can lag by up to
  about 16 s. Writes are not affected. See [freshness.md](freshness.md).
- Sequential joins can leave a double cell: the JOIN-1 hint comes from a view
  that lags recent joins by up to a gossip round (REM-120). It made one pass in
  three red at 64 vnodes.
- The ceiling of one vnode per i4i.xlarge is not measured (3 000/s per vnode,
  the highest rate tested, was green).

- [Intégration du patch Claude et comparaison AWS, 26 novembre](claude-integration-20261126.md) : provenance, qualification et limites des campagnes.
