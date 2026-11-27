# Intégration des correctifs de collecte et d'inventaire de Claude

Base `main` : `557e07f`. Branche examinée : `claude/next`, arrêtée à
`6576cc2`, six commits descendants directs. Les six commits sont conservés,
avec trois corrections de revue séparées. Aucun essai AWS n'est lancé ici.
Le [dernier run AWS](aws-claude-fixes-2026-11-26.md) reste la référence de
performance mesurée en réseau ; cette intégration ne valide pas 16k/s.

## Changements retenus

| Changement | Pourquoi il est retenu | Portée |
| --- | --- | --- |
| Collecte des reçus déjà inscrits au registre | Retirer des copies ne change pas leur réservation, déjà nulle ; le calcul évite les recherches par session. | Optimisation du contrôle de capacité ; dernier contrôle atomique et oracles conservés. |
| Différence entre inventaires persistants de zones | Les sous-arbres partagés sont sautés, au lieu de parcourir le catalogue entier pour chaque ajout. | Optimisation du parcours ; une suppression entraîne la reconstruction complète. |
| Réutilisation de l'inventaire pour les pages suivantes | Évite que la création de zones relance une découverte complète à chaque page. | Ordonnancement du travail de découverte ; les valeurs et l'autorité restent relues. |
| Inventaire partagé entre collections | Chaque collection lit sa tranche de la même liste triée, sans copie. | Optimisation ; fusion immuable des ajouts conservée. |
| Compteurs de construction/extension/réutilisation | Permettent de distinguer l'absence de coût d'un cache systématiquement invalidé. | Instrumentation. |
| Traitement des sorties normales dans la sentinelle | Le dernier run s'était arrêté sur un join jamais activé, sorti normalement. | Correctif fonctionnel, restreint pendant cette revue comme indiqué ci-dessous. |

Les assertions existantes ont été conservées. Les adaptations de tests ne
changent que les appels après évolution de signature et les compteurs de
mesure qui distinguent désormais extension et reconstruction.

## Trois corrections supplémentaires de revue

### 1. Ne pas masquer la sortie d'un vnode déjà actif

La version proposée retirait de la surveillance tous les slots sortis avec
le code 0. Cela masquait aussi un nœud en service qui s'arrêterait proprement.

La sentinelle mémorise maintenant, pour chaque génération de processus
identifiée par endpoint et date de démarrage, les activations observées :
`client_ready=true`, transfert reçu ou écriture appliquée. Un code 0 ne retire
le slot de la surveillance que si aucune activation n'a été observée.
Les sorties non nulles et les codes inconnus restent surveillés.

Les nouveaux tests couvrent ces trois preuves d'activation, l'alarme après
sortie 0 d'un processus actif et la réutilisation du même endpoint par un
nouveau join abandonné. Le comportement repose sur les sondages : il n'ajoute
pas de reçu durable d'activation. Une activation entièrement entre deux
sondages n'est pas une preuve que l'observateur pourrait reconstruire.
Un retrait prévu d'un vnode déjà actif reste surveillé tant qu'aucun état
explicite de retrait n'est traité ; le code de sortie seul ne suffit plus.

### 2. Fusionner les nouvelles frontières en une seule allocation de liste

Le parcours différentiel était efficace, mais la mise à jour de la liste
appelait `Clone` puis `Insert` pour chaque nouvelle frontière : un produit
nouveaux éléments × taille de la liste retenue.

La revue trie et déduplique les ajouts, retire ceux déjà présents et publie
une seule union immuable. Les pages déjà publiées gardent leur liste intacte.
Le coût de fusion inclut encore un parcours de la liste retenue lorsqu'elle
s'agrandit ; on ne revendique donc pas un coût total strictement proportionnel
au seul delta. Cette liste reste soumise à son budget existant.

Petit benchmark local, trois itérations par variante, 256 collections initiales :

| Nouvelles frontières | Octets alloués/op avant → après | Temps/op avant → après |
| ---: | ---: | ---: |
| 16 | 159 208 → 17 128 | 22,1 → 18,6 µs |
| 128 | 1 542 344 → 74 824 | 609 → 74,9 µs |

Les octets sont plus utiles que ces temps courts pour expliquer le travail
retiré. Un seul passage local n'est pas une estimation du gain de p99 AWS.
[Avant](claude-collect-summary-20261126/batch-before.log),
[après](claude-collect-summary-20261126/batch-after.log).

### 3. Charger une liste vide une seule fois

Dans le nouveau calcul différé des réservations, `copies == nil` signifiait
« pas encore chargé ». Or une liste de sessions chargée mais vide était aussi
`nil`. Un lot sans staging recommençait donc la découverte pour chaque requête,
et cette découverte réexamine les mutations de la transaction.

Un état `copiesLoaded` distingue ces cas. Le nouveau test de travail reproduit
16 puis 64 chargements pour 16/64 requêtes sur le code non corrigé ; après
correction il impose au plus un chargement par calcul, y compris quand le
résultat est vide. Il vérifie aussi que la réservation reste nulle et que le
comptage correspond à l'oracle complet.
[Reproduction avant](claude-collect-summary-20261126/empty-inventory-before.log),
[test vert après](claude-collect-summary-20261126/empty-inventory-after.log).
L'échec avant correction concerne ce nouveau test de régression, pas une
assertion existante affaiblie pour permettre la fusion.

## Mesures locales sur les changements de Claude

- Collecte après installation, 64 reçus : **71 lectures** avec 0, 8 et 32
  sessions en attente, un commit. Le produit par session a disparu de ce chemin.
- Huit sondages à 8 192 zones, avec une nouvelle zone avant chaque sondage :
  **une reconstruction et sept extensions**. Le journal distingue maintenant
  les deux événements ; un simple changement de pointeur ne signifie plus
  une reconstruction complète.
- Huit sondages alternant deux collections : **huit reconstructions avant, une après**
  le sixième commit (`6576cc2`, intégré en `3bd9bd0`). L’oracle indépendant
  et le test de partage entre collections passent ; aucune assertion affaiblie.
- Inventaire au-dessus du budget choisi : encore **huit reconstructions**.

Voir [la collecte](claude-collect-summary-20261126/collection-after.log) et
[les inventaires](claude-collect-summary-20261126/inventory-after.log).
Les mesures de travail sont sur le vrai stockage/code de résumés ; les petits
temps locaux ne sont pas une mesure des fsync, du réseau ou du placement AWS.

## Limites laissées explicites

Le chemin sans reçu au registre — notamment une réservation annulée avant
installation — conserve les recherches reçus × sessions. Le
[test des sessions encore présentes](simulation-gaps-2026-11-26.md) reste donc
utile et ne doit pas être supprimé parce que la collecte normale s'améliore.

Les inventaires trop grands restent coûteux. L’inventaire partagé couvre toutes
les collections : il peut donc atteindre le budget plus tôt qu’un cache limité
à une seule collection.
Le budget de cache n'a pas été relevé. L'identité de l'inventaire d'une page
peut aussi changer si un autre sondage remplace le cache partagé : il ne s'agit
pas d'un protocole de snapshot global par identifiant de poll.

Le budget mémoire agrégé, la priorité entre `writes` et `host_pressure`, le
choix du premier fragment utile et le modèle multi-processus ne sont pas
modifiés. Les modèles de files restent synthétiques. Aucune nouvelle
conclusion AWS de durabilité, de p99 ou de linéarité n'est déduite ici.


## Vérification de l'arbre intégré

Sur le source final `630ce85` : `gofmt -l .` vide, compilations core et QUIC
réussies, `go test -count=1 ./...` vert et `-race -count=1` vert sur storage,
summary et cmd/experiment. Le paquet mesh complet passe dans la suite normale ;
son `-race` complet n'a pas été relancé pour cette intégration.
[Commandes, durées et résultats](claude-collect-summary-20261126/validation.json).

Un laboratoire supplémentaire à huit vnodes, routage direct, 512 zones initiales
et 200 écritures/s sur quatre phases de trois secondes passe aussi. Il injecte
une partition de 750 ms, une traîne disque de 25 ms pendant 750 ms, puis rouvre
un store avec la même identité. Audit : 2400 ACK, 2912 records retrouvés
(512 initiaux + 2400 nouveaux), aucun ACK absent ou dupliqué, aucun double
écrivain constaté à l'audit et aucun gel restant. Les tentatives transitoirement
refusées sont réessayées ; le nombre d'échecs définitifs vaut zéro.

Sur ce seul passage avec fautes, p50/p95/p99 des ACK : 33,9/133,0/203,0 ms ;
p99 depuis l'arrivée prévue : 204,2 ms. Un appel de cutover atteint 922 ms
pendant ce scénario ; cela ne constitue ni un gel global ni une borne normale
promise. Il n'existe pas de comparaison avant/après équivalente pour ces temps.
[Résumé du labo](claude-collect-summary-20261126/lab8-summary.json),
[journal](claude-collect-summary-20261126/lab8.log).

Ce sont huit vnodes dans un seul processus sur une machine, avec dispositifs
disque partagés simulés. Ce banc ne qualifie ni les flocks entre processus,
ni le placement/boot AWS, ni la capacité à tenir 16k/s. Le laboratoire lourd
64 vnodes et une nouvelle campagne multi-machine restent hors de cette passe.


## Vérification du sixième commit

Sur le source `3bd9bd0` : gofmt vide, builds core et QUIC réussis,
`go test -count=1 ./...` vert (tests lourds opt-in non activés), et
`go test -race -count=1 ./internal/storage ./internal/summary ./internal/mesh
./cmd/experiment` vert, y compris mesh complet. Les suites parallèles servent
à la correction, pas à une comparaison de latence.
[Suite complète](claude-collect-summary-20261126/final-sixth-all-tests.log),
[race](claude-collect-summary-20261126/final-sixth-race.log),
[mesure des inventaires](claude-collect-summary-20261126/final-sixth-inventory.log).
Le labo à huit vnodes décrit plus haut concerne l’arbre précédent `630ce85` ;
il n’a pas été relancé après ce dernier changement de cache.

## Retour final de Claude et prochaine qualification

Le sixième commit a été repris sans réintroduire les copies par frontière :
le conflit de fusion conserve `mergeBoundaryKeys`, et retire seulement la clé
`collection` du cache. Le tri reste lexicographique par collection puis location,
ce qui permet deux recherches binaires et une tranche sans allocation.

La recommandation de qualifier les vnodes dans des processus distincts est
retenue. Le laboratoire Go partagé ne mesure pas les flocks entre processus,
les quotas CPU par processus, ni la vision réelle de `/proc/stat` sous Linux.
Avant AWS : agent réel avec quatre à cinq processus vnode, budget de copies
1 puis 2, GOMAXPROCS 1/2/défaut, mêmes charges et seuils, mesures de tenue du
writer et de `host_pressure`. Ce banc n’a pas été exécuté dans cette intégration.
Le modèle à files ne sera qualifié de calibré qu’après incorporation de ces
mesures ; les lois de travail seules ne donnent pas les temps de service disque.
Aucun nouveau lancement AWS n’est autorisé par ce rapport : il reste à demander
explicitement dans la suite des essais.


La [qualification multi-processus suivante](local-multiprocess-2026-11-26.md)
est maintenant disponible : flocks réels, pression CPU machine, calibration
partielle et correction d’une redirection ignorée dans le lecteur de totaux du
loader. Elle ne qualifie pas encore le placement multi-machine du provisioner.
