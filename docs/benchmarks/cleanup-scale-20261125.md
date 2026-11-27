# Nettoyage, coûts de contrôle et taille du réseau

Passe du 25 novembre 2026. Départ : `7fa2de5`. Dernier changement de production de
cette passe : `a6e918b`. Aucun nouveau test AWS n'est engagé. L'analyse des graphes,
les projections et les sources sont dans
[Croissance du contrôle](../design/scaling-and-control-costs.md).

## Ce que l'on cherche à stabiliser

Les 3,9 secondes du précédent laboratoire sont un maximum de latence d'écriture,
pas une mesure de membership ni de gel par lot. Pour l'expliquer, il faut relier
la requête à sa file, au commit, au fsync, aux relais et au transfert concerné.
Les coûts identifiés ci-dessous sont démontrés dans le code ou par microbenchmark ;
ils ne reconstituent pas à eux seuls cette requête maximale.

La cible est un coût par zone/lot indépendant d'un historique global et aussi
indépendant que possible de la taille du réseau. Une convergence réseau complète
et un total global frais n'ont pas, eux, un coût constant gratuit.

## Nettoyage : périmètre et comptage honnête

Comptage des lignes physiques de **tous les fichiers Go du dépôt**, commentaires,
blancs et outils compris ; fichiers `_test.go` séparés. Ce périmètre est plus large
que certains anciens comptes du seul cœur : ne pas comparer les nombres absolus
sans reprendre le même périmètre.

| Couche | Avant, hors tests | Après, hors tests | Différence |
| --- | ---: | ---: | ---: |
| Façade indexus | 2 870 | 2 819 | −51 |
| Autorité/membership | 733 | 714 | −19 |
| Mesh | 4 516 | 4 490 | −26 |
| Plans/protocole pur | 1 082 | 853 | −229 |
| Primitives/modèle de réplication | 812 | 98 | −714 |
| Simulation | 337 | 30 | −307 |
| Stockage | 9 301 | 9 168 | −133 |
| Résumés | 2 441 | 2 445 | +4 |
| Arbre | 1 587 | 1 575 | −12 |
| **Dépôt entier hors tests** | **44 755** | **43 268** | **−1 487 (−3,3 %)** |

Ce ne sont **pas 1 487 lignes de texte physiquement effacées**. Une grande partie
est la bonne classification de modèles historiques dans les tests : réseau simulé,
machine de réplique, vecteurs et ancien DAG d'activation. Les primitives réellement
consommées par le runtime restent en production. Le total des lignes Go de tests
passe de 49 509 à 51 333 : les modèles et de nouveaux tests de concurrence restent
lisibles. Tous les fichiers Go réunis augmentent donc de 337 lignes.

Les suppressions effectives comprennent les API sans appelant, les parcours
redondants d'inventaire, les helpers de contrôle et le calcul d'agrégats temporaires
inutilisés sur chaque écriture. Un test qui ne vérifiait qu'un algorithme débranché
a été retiré ; les tests de contrats ont été conservés ou redirigés vers le vrai
chemin actif. Aucun serializer d'interface ni mode de réplication vivant n'a été
retiré parce qu'un scanner ne trouvait pas un appel direct.

## Changements de fonctionnement isolés du nettoyage

### Contrôle local et membership immuable

Avant, chaque commit local d'autorité sérialisait, hachait et comparait toute la
membership, même si seul un reçu ou une fence changeait. Le contrôle local garde
maintenant une référence par digest. Seuls les six producteurs réels de mutations
de membership écrivent son blob dans la même transaction que sa référence.
Les lectures internes réutilisent la version immuable ; les exports publics
restent détachés. Un chargement à froid ou une réouverture vérifie le digest.
Un cache chaud n'est pas une surveillance permanente de modifications externes
illégitimes du blob disque.

Benchmark `BenchmarkLocalControlMembershipSize`, deux répétitions, transactions
committées avec `NoSync` pour isoler la préparation ; **hors fsync et latence client** :

| Membres | Avant | Après | Allocations en octets avant → après |
| ---: | ---: | ---: | ---: |
| 16 | 18,7–20,9 µs | 9,8–10,0 µs | ~15 Ko → ~8 Ko |
| 64 | 38,7–38,9 µs | 9,1–10,3 µs | ~28 Ko → ~8 Ko |
| 256 | 121–123 µs | 9,4–10,5 µs | ~91 Ko → ~8 Ko |
| 1 024 | 454–479 µs | 9,1–9,6 µs | ~351–361 Ko → ~8 Ko |

Cette composante devient plate en nombre de membres. Les droits/transferts locaux
et les exports publics détachés ont toujours leur propre coût.

### Cellules XOR et planification des lectures

`Cell` réutilise maintenant le trie de placement déjà construit. Les bits de
branchement du chemin du vnode sont exactement les contraintes de sa cellule.
Un oracle indépendant compare les résultats par paires, y compris sur une vue
profonde et une vue dont l'index a été invalidé par `Add`.

Benchmark calculant les cellules de **tous** les nœuds d'une vue construite, deux
mesures d'une itération, sans allocations ; ce n'est pas une mesure de lecture réseau :

| Nœuds | Avant | Après |
| ---: | ---: | ---: |
| 64 | 40–44 µs | 5,7–6,7 µs |
| 256 | 580–606 µs | 27–30 µs |
| 1 024 | 11,7–12,0 ms | 141–143 µs |
| 4 096 | 183–184 ms | 675–680 µs |
| 10 000 | 1 176–1 192 ms | 1,76–1,83 ms |

Le chemin indexé prend au plus 96 décisions par cellule, environ logarithmiques
pour des identifiants équilibrés. Les petites vues et les vues en mutation gardent
le parcours sans index. Ces mesures peuvent contenir du bruit des tests voisins ;
le changement de complexité repose aussi sur le parcours et l'oracle, pas sur un
pourcentage de p99 déduit de deux échantillons.

### Spawn sans ancien graphe d'activation

La réservation de spawn conserve le digest frais, `ValidatePlan`, le plan entier
et sa réservation durable. Elle énumère directement les noms de `Plan.Add`, dans
**l'ordre du plan**, sans fabriquer de composants et de mouvements qui n'étaient
pas exécutés. Cet ordre peut différer de l'ancien ordre par composants.
`spawnN > 0` n'en tronque aucun. Le nouveau test régional vérifie trois ajouts
malgré une demande `spawnN=1`, leur réservation et l'absence d'activation prématurée.
Le modèle historique de composants reste dans les tests ; il n'est pas une
barrière d'autorité du runtime par tickets.

### Budget de polling et progression

Les polls de résumés héritent du contexte appelant, refusent une réponse tardive
après annulation et arrêtent les pairs suivants. La rotation avance selon les
pairs effectivement tentés, en mode stable et complet. Le test à 63 pairs interrompt
chaque tour après le premier RPC : aucun suffixe de fenêtre n'est affamé.

Cela ne crée pas une deadline dure d'`Update` : pushes autonomes, mutex et disque
restent présents. Si un passage complet ou l'export d'un seul pair ne tient jamais
dans son budget, il faut un round/page reprenable avec qualification versionnée.
Un passage incomplet n'est pas déclaré complet pour contourner cette limite.

## Ce qui grossit encore

Le benchmark `BenchmarkMembershipReferencePreparation` mesure une référence sans
corps réseau, préparée par la callback d'export du même type que le client :

| Membres | Préparation actuelle | Octets alloués par préparation |
| ---: | ---: | ---: |
| 64 | 87–104 µs | ~33 Ko |
| 256 | 306–355 µs | ~132 Ko |
| 1 024 | 1,02–1,13 ms | ~508 Ko |
| 4 096 | 3,26–3,70 ms | ~3,42 Mo |
| 10 000 | 7,62–7,65 ms | ~4,93 Mo |

La référence courte n'est donc pas encore un travail CPU constant. Il faut
publier et réutiliser une version préencodée, puis transmettre les records modifiés
avec réparation bornée. Cela est proposé dans l'analyse, **pas livré par cette passe**.

Les autres coûts restant visibles sont le graphe complet des polls de résumés,
la reconstruction des frontières avant filtrage, le cache d'export à un seul
parent, les parcours de fragments et d'exceptions et les reconstructions de vue.
`Children -> only` relit éventuellement clés/pages/sommes pour trouver une
localisation exacte ; il ne charge pas de payload utilisateur. Ne pas attribuer
un profil `Store.payload` à ce chemin sans trace causale.

## Qualification

Le départ build/vet/tests était vert. Après `a6e918b`, build, vet et suite Go
complète passent. Les suites complètes avec détecteur de courses passent sur
storage, indexus, summary, tree et protocol ; mesh et le modèle/primitifs
convergent passent aussi séparément avec `-race`. Le garde-fou des références
de tests dans la documentation passe après la mise à jour des cartes.
L'audit de couches signale zéro import interdit.

Les fuzzings ciblés du spawn réel passent mais ne couvrent que trois exécutions
sur cinq secondes demandées, avec fixtures durables : ce n'est pas une grande
exploration. Les scénarios historiques déplacés ne qualifient pas un nouveau
chemin de production par leur simple présence.
Les microbenchmarks et les modèles historiques ne sont pas des preuves du p99
AWS, ni une qualification d'un protocole de deltas qui n'est pas encore écrit.

## Laboratoire final avec profils

Run `lab64-cleanup-scale-20261125`, code de production `a6e918b`. Même machine
unique, 64 stores, 8 192 zones préchargées, fsync réel et mêmes paliers
50/100/200/400 écritures/s durant cinq secondes chacun. Après ces paliers, dix
nouvelles écritures/s continuent jusqu'au dernier join. Sont injectés pertes de
réponses d'installation, partition brève et traîne disque.

L'audit termine à **297,9 s** : **6 502 offertes, 6 502 ACK**, dont 2 752 après
les paliers ; zéro erreur finale, rejet, ACK perdu/dupliqué, double écrivain ou
gel restant. Le compteur final des handoffs ouverts vaut également zéro.

| Latence de ce run | p50 | p95 | p99 | maximum |
| --- | ---: | ---: | ---: | ---: |
| Depuis l'envoi | 169 ms | 796 ms | 964 ms | 3 424 ms |
| Depuis l'arrivée prévue | 231 ms | 1 529 ms | 1 744 ms | 3 430 ms |

**Ce n'est pas une comparaison de performance contrôlée avec le précédent run.**
Les profils CPU, mutex (échantillonnage 1/10) et blocages (taux 1 ms) ajoutent de
l'observation ; le nombre d'écritures de la traîne dépend du temps des joins.
La queue depuis l'arrivée prévue reste élevée. Le résultat ne valide ni une
latence uniforme ni un débit AWS à 16k/s.

Les compteurs de volume JSON request+response donnent encore 268,9 Mo de
membership (20 642 appels), 441,6 Mo de contributions de résumés (12 535 appels)
et 144,9 Mo de lectures de résumés (109 864 appels). La consolidation régionale
stage+install représente environ 0,317 Mo sur 346 appels. Le retrait du calcul
réseau dans les commits locaux n'a donc **pas** supprimé le trafic global de fond.
Ne pas présenter les différences avec les runs antérieurs comme un gain de
bande passante : la cadence des reprises/polls et le coût d'observation diffèrent.

### Ce que montrent les profils

Le profil CPU attribue environ 125 s cumulées à `Tree.ExportPage`, dont 124 s au
parcours des frontières et 97 s à `Snapshot.Children`, sur 774 s de samples CPU
cumulés entre threads. Ce sont des chemins imbriqués : **ne pas les additionner**.
Ils étayent le coût du graphe de réparation et des frontières recalculées.
Les appels système et le runtime dominent aussi le profil macOS ; ces samples
ne permettent pas de décomposer à eux seuls la durée d'une requête maximale.

Le profil mutex attribue une contention cumulée importante à `Join`, aux commits
storage et à la publication `zonelog.workOnce`. La contention de `Join` touche
les autres travaux qui attendent ce même join, pas automatiquement les écritures.
Les durées de profil sont cumulées sur les goroutines : 700 secondes de délai
cumulé ne signifient pas un verrou tenu 700 secondes.

La lecture du code et le profil de source confirment que `workOnce` construit le
segment hors `zonelog.mu`, puis reprend ce mutex pour publier la liste et écrire
le manifeste, avec synchronisation durable. Ce dernier I/O reste dans la section
qui bloque les lecteurs/écrivains du moteur. Il faut une publication de versions
et de manifests ordonnée avant de le déplacer ; un simple `Unlock` autour de
`writeManifest` serait une modification dangereuse de l'ordre WAL/manifest/GC.

Les profils bruts et leurs vues `pprof -top`/source sont conservés dans
`outputs/cleanup-scale-20261125/` hors dépôt, avec les configurations et l'audit
`lab64-evidence.json`. Les rapports `line-counts.json` et les logs des benchmarks
fixent les périmètres du comptage et des mesures.

## Précision sur les 964 ms

Le [contrôle par taille de réseau](latency-vs-nodes-20261125.md) distingue ce p99
agrégé des mesures après installation et des joins à charge constante. Les
964 ms ne sont pas le p99 d’un réseau de 64 nœuds installé, ni un temps de gel.
