# Indexus Core — documentation

La géométrie, la connaissance de topologie et le droit d'écriture sont
distincts : `owner(z) = argmin XOR(z, V_active)` dit qui *devrait* tenir une
zone ; seuls des droits installés et les fences du disque autorisent le commit.
Le remplacement par [permissions locales et tickets](protocol/local-permissions.md)
est branché dans le worktree ; consulter sa [qualification](protocol/local-permissions-verification.md)
avant d’attribuer un résultat au nouveau protocole.
Vocabulaire commun aux quatre dépôts : [glossaire](glossary.md).

## Pour commencer

- [getting-started.md](getting-started.md) : construire, un nœud, y mettre des données.
- [local-experiences.md](local-experiences.md) : toute la pile en local (un nœud, la gateway, Explore).
- [glossary.md](glossary.md) : les mots partagés par core, sdk, dashboard et experiences.

## Architecture (référence)

[architecture/README.md](architecture/README.md) : les pages qui décrivent le
code branché, mécanisme par mécanisme. Elles font foi quand un document plus
ancien les contredit.

- [stockage et verrous](architecture/stockage-et-verrous.md) : zonelog, lectures MVCC, group commit, carte et ordre des verrous, formats refusés.
- [Knowledge](architecture/knowledge.md) : contenu, versions, transport entre pairs, effet sur les écritures.
- [transfert de zone](architecture/transfert-de-zone.md) : join, bascule par lots, clôture régionale, prédicat d'écriture.
- [chemin d'une écriture](architecture/chemin-ecriture.md) : entrée, relais, autorité, ACK, réplication, secours.
- [autoscale](architecture/autoscale.md) : demande de capacité, attente pendant un transfert.
- [matrice des pannes](architecture/pannes.md) : chaque point d'interruption et sa reprise.

## Protocole

[protocol/README.md](protocol/README.md) est l'index de la spécification
active. Les plus lus :

- [invariants](protocol/invariants.md) et [verification](protocol/verification.md) : règles nommées et preuves.
- [Empreinte de sous-arbre](protocol/subtree-fingerprint.md) pour comparer des observations locales, composer les contributions disjointes et reproduire le vecteur de référence.
- [wire-block](protocol/wire-block.md) : le fil client, IXB2 pour `/sets`, IXRB pour `/records`.
- [write-authority](protocol/write-authority.md), [causal-topology](protocol/causal-topology.md), [read-modes](protocol/read-modes.md), [write-routes](protocol/write-routes.md).
- [distributed-runtime](protocol/distributed-runtime.md) : démarrage et stockage.
- [linear-knowledge](protocol/linear-knowledge.md) et [gossip](protocol/gossip.md) : une connaissance de contrôle linéaire, des partenaires d'anti-entropie bornés.
- [propriétés mathématiques et informatiques](design/mathematical-and-distributed-properties.md) : espace `{0,1}^96`, définitions `IXC1`, algèbre monoïde, convergence CRDT, P2P et synthèse des mesures.
- Pistes d'application et programme d'expériences (document historique absent de ce checkout) : applications ML et hors ML, descentes en parallèle (géo × sémantique), suites théoriques et ordre des essais.
- Apprentissage continu : bilan et perspectives (document historique absent de ce checkout) : l'expérience du 20 novembre, sa reproduction (0 écart), les limites et les prochaines expériences.
- [benchmarks](benchmarks/README.md) : linéarité horizontale, élasticité live et coût AWS.

## Stockage et ingestion

- [storage/zonelog.md](storage/zonelog.md) : le moteur de stockage du nœud.
- [storage/bulk.md](storage/bulk.md) : installation d'une batch préparée d'un coup.
- [ingest/pipeline.md](ingest/pipeline.md) : `tools/prep`, le format IXP1, les constructeurs et leurs mesures.

## Exploitation

- [production-configuration.md](production-configuration.md) : chaque champ de configuration.
- [production-build.md](production-build.md) : binaires de production.
- [http3.md](http3.md) : HTTP/3 en option : la gateway (module `gateway/`) et le nœud HTTP/3 (module `quic/`).
- [benchmarks/p2p-experiment.md](benchmarks/p2p-experiment.md) et [benchmarks/p2p-findings-2026-11-15.md](benchmarks/p2p-findings-2026-11-15.md) : le banc P2P et ses premiers résultats.
- [cli.md](cli.md) : proposition d'une commande `indexus` qui remplacerait les scripts.
- [../ops/README.md](../ops/README.md) : configuration, maillage local, déploiement AWS.
- [../ops/local/README.md](../ops/local/README.md) : un maillage local avec autoscale.

## Paquets

| Dossier | Rôle |
| --- | --- |
| `indexus/` | Runtime d'un nœud : records, zones, lectures, écritures, résumés, bulk, maintenance, autoscale |
| `internal/tree/` | Algèbre des adresses : ID, clés de zone, régions, View/Owner, trie XOR (bibliothèque standard seule) |
| `internal/protocol/` | Capacité, plans, handoff, forwarding : des règles vraies sans réseau ni disque |
| `internal/causal/` | Membership de routage et noyau pur des permissions locales (campagne locale qualifiée sur `e65dfd3`) |
| `internal/zonelog/` | Le moteur de stockage : journal, segments, fichiers de payload, sommes compensées |
| `internal/storage/` | L'état durable du nœud sur zonelog : records, fences, certificats, reçus, reprise |
| `internal/prep/` | Le format de batch IXP1 |
| `internal/mesh/` | Anti-entropie bornée, copie/miroir, activation et forwarding |
| `internal/metrics/` | Compteurs du process pour `GET /metrics` |
| `internal/adapter/` | HTTP, TLS, issuer, provisioner, logs. Pas une API publique |
| `internal/modelcheck/`, `internal/simulation/` | On essaie de casser les règles, puis on mesure le coût |
| `internal/architecture/` | Frontières de dépendances entre couches |
| `cmd/` | Binaires : `node`, `issuer`, `provisioner`, `experiment` (banc P2P) |
| `gateway/` | Module Go à part : la gateway HTTP/3 optionnelle (quic-go). Le module du nœud ne dépend d'aucun module hors de la bibliothèque standard |
| `quic/` | Module Go à part : le nœud en HTTP/3 (NET-1), qui remplit `internal/adapter/transport` |
| `tools/` | `localdata` (import, bulk, export), `prep` (constructeur de batches, Python, Metal) |
| `scripts/local/`, `scripts/perf/`, `perf/` | Lanceur local, préparation, suite de mesure Go au format `indexus-perf/1` (voir `sdk/docs/perf.md`) |
| `scripts/research/` | Bancs d'essai des docs de stockage, hors exploitation |
| `ops/` | Configuration d'exemple, maillage local, déploiement AWS, banc P2P (`ops/experiment`) |

## Archives

[archive/](archive/README.md) garde quelques plans exécutés et instantanés,
non maintenus. Les comptes rendus plus anciens (campagnes de benchmark,
audits, ancienne conception par quorum) sont archivés hors des dépôts.
