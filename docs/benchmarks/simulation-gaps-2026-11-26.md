# Ce que nos simulations ne pouvaient pas anticiper sur AWS

Analyse et nouveaux diagnostics sur la base `eaef9dd`, le 26 novembre 2026.
Le [dernier run AWS](aws-claude-fixes-2026-11-26.md) améliore le débit sans
stabiliser 8k/s. Les simulations ne contredisent pas ce résultat : elles ne
représentaient pas plusieurs travaux qui saturent effectivement les nœuds.
Aucun correctif de production ni nouvel essai AWS n'est inclus ici.

## Trois observations nouvelles, reproductibles localement

### 1. La collecte dépend encore des sessions présentes

Le nouveau test `TestCollectionWorkAcrossLiveSessionsAndReceipts` utilise le
vrai Store et zonelog. Il fait varier séparément les reçus de la session à
collecter et le nombre de sessions contenant encore du staging.
Les requêtes des sessions sont disjointes : il faut aussi mesurer le travail
fait pour constater qu'une copie n'existe pas dans une autre session.

| Sessions présentes | Reçus à collecter | Lectures ponctuelles | Clés scannées | Commits |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 1 | 10 | 11 | 1 |
| 1 | 16 | 40 | 71 | 1 |
| 1 | 64 | 136 | 263 | 1 |
| 8 | 64 | 584 | 263 | 1 |
| 32 | 16 | 536 | 71 | 1 |
| 32 | 64 | 2 120 | 263 | 1 |

Dans cette fixture, le nombre de lectures est exactement `(S + 1) × R + 8`.
Il reste donc un produit reçus × sessions, même quand le nombre de scans
ne grandit pas. Ce n'est pas le planificateur de feuille autrefois quadratique.
La forme de croissance est observée sur ces cas ; ce n'est pas une formule
universelle de coût pour toutes les mutations.

Le test précédent `TestReceiverCostIsIndependentOfHistory` faisait varier les
sessions **déjà collectées** et les zones étrangères, puis mesurait preload et
mount. Il a correctement gardé le correctif des tombstones, mais ne couvrait
pas cet autre axe ni le coût de la collecte elle-même.

Les compteurs exacts et l'index des sessions sont vérifiés avant/après.
La fixture travaille au niveau stockage : elle ne rejoue pas un handoff réseau
complet. `NoSync` est actif et le scan différentiel de correction est hors de
la mesure ; les temps du portable ne sont pas des prévisions de fsync AWS.
[Journal complet](simulation-gaps-20261126/collection-work.log).

### 2. Des lecteurs de collections différentes font reconstruire le cache

Le nouveau test `TestInventoryDiscoveryWorkUnderChurn` appelle le vrai code de
frontières. Pour 8 192 zones et huit demandes :

| Activité pendant les demandes | Reconstructions | Temps de découverte cumulé, un passage local |
| --- | ---: | ---: |
| Catalogue stable, demande globale identique | 1 | 0,51 ms |
| Une nouvelle zone avant chaque demande | 8 | 3,09 ms |
| Deux collections alternées, aucune écriture | 8 | 2,05 ms |
| Inventaire trop grand pour le budget choisi | 8 | 2,76 ms |

Le résultat final est comparé à l'oracle indépendant des ancêtres. Les durées
sont indicatives, sans assertion de performance. Le nombre de reconstructions
est le constat robuste de ce passage.

Le cache actuel contient un inventaire. Un inventaire global peut servir une
collection particulière ; un inventaire propre à A ne peut pas servir B.
L'alternance A/B le remplace donc à chaque fois. Il faut varier le **mix des
lecteurs**, pas seulement ajouter des écritures au banc.

Le coût à surveiller est `nombre de cache misses × zones visitées`, avec le
tri des frontières trouvées. Ce n'est pas la remontée normale d'une contribution
sur les parents de l'arbre. [Journal](simulation-gaps-20261126/inventory-work.log).

### 3. Des ACK corrects peuvent masquer une dette de maintenance

Le petit modèle `queues.mjs` conserve les travaux de fond retardés au lieu de
les oublier au prochain tick. Dans un scénario **synthétique** de 64 identités
vnode dont huit ont une part de trafic, 31 883 des 32 000 tâches clientes
terminent, mais 1 419 quanta de collecte restent dus. Un critère limité aux
ACK déclarerait presque le succès alors que le nettoyage prend du retard.

Un autre contre-exemple garde la même charge de collecte demandée, mais change
son unité atomique : quanta de 1 ms ou de 100 ms, chacun à 50 % d'occupation
demandée du writer. Le p99 synthétique passe de 8,2 à 131 ms, avec un CPU moyen
très proche. Les deux pas de temps testés gardent cette conclusion.
Ce ne sont ni des latences Indexus ni un gain attendu sur AWS. Le JSON complet
est compressé dans l'annexe ; le runner le produit en clair dans le dossier choisi.
[Scénarios et paramètres](simulation-gaps-20261126/queues.json.gz),
[table de résultats](simulation-gaps-20261126/queues.csv).

## Pourquoi les anciens modèles ne pouvaient pas prévoir le décrochage

| Élément | Modèle existant | Ce qui manque pour le run réel |
| --- | --- | --- |
| État initial | 64 vnodes dans le dimensionnement fluide, placement initial compacté | Quatre vnodes sur quatre machines distinctes, six hôtes initiaux, puis arrivées |
| Machines/charge | RAM par défaut 16 Gio ; rampes de 60 s | 32 Gio, rampes de 120 s, mêmes délais et paramètres effectifs |
| Politique | Contrôleur idéal du modèle, taux de trafic connus | Vrai `Controller`, priorité des signaux, `Place`, réservations et refus effectifs |
| Service client | Calcul de demande de ressources ; pas d'exécution de requêtes | Workers, files bornées, files serveur, tentatives, ACK et abandon |
| Writer | Aucun writer exclusif ni commit | Validation, comptabilité, attente, tenue, fsync et taille des transactions |
| Collecte | Libération d'espace après un délai | Travaux/lectures/CPU sous writer ; dette qui peut grandir |
| Résumés | Pas de coût d'inventaire ni de cache | Mélange global/collections, mutations de catalogue, budget et invalidations |
| Copie | Budget strict aux deux extrémités, charge déplacée avec la fraction copiée | Budget souple réel, collecte hors budget, lots installés et trafic effectivement retiré |
| Visibilité | État immédiatement disponible au contrôleur | Échantillons décalés, compteurs d'items tardifs, arrivée au détenteur distincte de l'offre |
| Fin d'un join | Fin du travail modélisé | Join abandonné sans activation, retrait normal, panne réelle et interprétation sentinelle |

Le transport des tests `internal/simulation` utilise le vrai protocole et
zonelog dans un seul runtime Go, sur un seul hôte, avec réseau contrôlé. Il ne
modélise pas la contention de plusieurs processus sur un NVMe/CPU physique,
les boots AWS ou la file du générateur. Les tests longs sont opt-in :
`TestMain` ne lance pas ces scénarios sans `INDEXUS_LONG=1`. Un simple
`go test ./...` vert ne signifie donc pas que le réseau long ou le labo 64
vnodes a été exécuté. Le commentaire historique « Bolt-backed » a été corrigé.

Le laboratoire `indexus/lab64_test.go` va plus loin avec HTTP et des dispositifs
partagés simulés, mais reste dans un seul processus. Un réglage de GOMAXPROCS
y agit sur tout le banc, pas sur cinq runtimes Go indépendants.

Le renderer commun au dashboard et au dimensionnement est utile pour lire les
états. Il ne rend pas leurs moteurs équivalents.

## Ce que le nouveau noyau de files ajoute

`ops/experiment/sizing/queues.mjs` est un modèle déterministe d'exécution limité :

- CPU physique partagé entre les vnodes d'un hôte, indépendamment de leurs writers
  (uniquement les coûts fournis, sans surcoût OS/Go implicite) ;
- writer unique par vnode, occupé par le CPU de validation et l'attente I/O ;
- service I/O partagé configurable, sans prétendre reproduire un NVMe ;
- nombre de workers et file clients bornés, abandons et travaux inachevés visibles ;
- latences depuis admission et arrivée prévue, avec histogrammes bornés ;
- dette de chaque source de maintenance, sans une allocation par tick manqué ;
- partage de trafic inchangé sur un receveur prêt tant qu'aucune installation
  externe ne lui attribue de part ;
- horloge `stepsRate` conforme aux rampes AWS de 120 secondes.

Dix tests déterministes vérifient ces mécanismes, dont un cas à 64 identités.
Avec les quinze tests du modèle fluide, les 25 tests Node passent.
`go test ./...` passe avant et après ; `go test -race ./internal/storage ./internal/summary`
passe également. [Résultats des validations](simulation-gaps-20261126/validation.json).
Ces suites ne lancent pas les scénarios longs opt-in.
L'identité comptable est `offert = terminé + abandonné + en file + en cours`.
Pour le fond : `travail émis = terminé + restant`. Un cas sans ACK a un p99
indisponible, jamais un zéro présenté comme une réussite.

La mémoire dépend des limites de workers/file, du graphe déclaré et du nombre
de sources de maintenance, pas de la liste de toutes les requêtes terminées.
Les limites de pas et d'événements bornent le travail du banc. Les douze petits
scénarios s'exécutent ici en environ 0,71 s, avec RSS maximal 124 Mio et zéro
swap observé. [Mesure du processus](simulation-gaps-20261126/queues-resources.txt).

**Ce que ce noyau ne fait pas :** ni group commit réel, ni threads/processus
Linux, ni protocole H3, ni autorité/CRDT, ni bootstrap/provisioner, ni données
transférées, ni reprise durable. Les installations sont des entrées supposées
validées ailleurs. Le modèle ne prouve donc ni écrivain unique réel ni
conservation durable des ACK. Il ne génère pas une prévision AWS calibrée.

## Conditions pour obtenir une prévision défendable

### Mesurer des unités de travail, puis leurs combinaisons

Faire varier indépendamment : octets, records, zones, collections, reçus par
transaction, sessions de staging présentes, tombstones/historique collecté,
nombre de writers sur un hôte et nombre de vnodes participant aux polls.
Le même volume en Gio peut produire des coûts très différents.

Pour chaque opération, garder : attente/tenue du writer, CPU, allocations/GC,
lectures ponctuelles, clés visitées, commits/fsync et octets. Mesurer les
petits cas seuls puis en concurrence émission/réception/collecte/résumés.
Une somme de p99 par étape n'est pas le p99 d'une requête : il faut conserver
les corrélations ou des traces par requête/lot.

Un contrôle nécessaire par writer est que le travail moyen demandé reste
inférieur à son temps de service disponible. Si écritures et collecte demandent
ensemble plus d'une seconde de writer par seconde, une des files doit grandir
ou du travail doit être refusé. Des cœurs libres sur d'autres machines ne changent
pas cette contrainte tant que l'autorité et le travail restent sur ce writer.
Des unités atomiques longues peuvent dégrader le p99 bien avant cette limite.

### Rejouer le vrai contrôle et distinguer les étapes de soulagement

Le banc doit utiliser le contrôleur et les décisions de placement de production,
pas seulement une politique qui prend les bonnes décisions par construction.
Rejouer aussi leurs refus, cooldowns, demandes déjà en cours, boots, chauffe,
ressources promises et état des copies. Le dernier run n'a exercé aucune
demande `host_pressure` observée, malgré le nouveau compteur CPU physique.

Il faut distinguer machine prête, processus démarré, données préchargées,
droit installé, première écriture appliquée, route directe apprise et baisse
mesurée de demande sur la source. Le placement XOR souhaité ne suffit pas à
connaître la répartition effective pendant les joins. Une clé uniforme n'implique
pas que l'autorité déjà installée soit uniformément répartie.

### Calibrer sur un run, contredire sur un autre

Les profils actuels identifient des chemins, mais n'exposent pas une distribution
complète et corrélée des services par quantum. On ne doit pas choisir des coûts
pour retrouver artificiellement le décrochage à 8k puis appeler cela une validation.

Calibrer des coûts sur une trace, fixer les paramètres, puis comparer à une
trace indépendante : début du déficit, goodput, dette et durée des joins, parts
CPU par rôle, hôtes chauds/vides, occupations du writer et files. Publier les
écarts et la sensibilité au pas de temps et aux coûts. Une amélioration n'est
retenue que si elle conserve les audits et ne reporte pas le travail dans la
collecte. Aucun second run AWS n'est lancé ici pour obtenir ce résultat.

## Reproduction et coordination

```sh
node --test ops/experiment/sizing/model.test.mjs ops/experiment/sizing/queues.test.mjs
node ops/experiment/sizing/run-queues.mjs /tmp/indexus-queues
INDEXUS_BENCH_NO_USAGE_SCAN=1 go test ./internal/storage -run '^TestCollectionWorkAcrossLiveSessionsAndReceipts$' -count=1 -v
go test ./internal/summary -run '^TestInventoryDiscoveryWorkUnderChurn$' -count=1 -v
```

Les deux diagnostics ajoutent des fixtures et des oracles sans modifier les
assertions existantes. Ce travail reste séparé des corrections de production de
Claude. Le [récapitulatif pour Claude](simulation-gaps-20261126/HANDOFF-CLAUDE.md)
énumère les nouveaux axes mesurés et les limites à garder dans les prochains essais.
