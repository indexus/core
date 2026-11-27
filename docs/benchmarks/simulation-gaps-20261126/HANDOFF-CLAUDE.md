# Complément Codex : cas absents des simulations

Travail isolé dans le worktree `work/simulation-gaps-20261126`, branche
`codex/simulation-gaps-20261126`, base `eaef9dd`. Aucun chemin de production
modifié. Les deux nouveaux tests mesurent le code existant ; ils ne fixent pas
un coût élevé comme une garantie à conserver.

## Stockage : sessions encore présentes, pas historique déjà collecté

Nouveau test `TestCollectionWorkAcrossLiveSessionsAndReceipts` dans
`internal/storage/collection_work_axes_test.go`.

Pour 64 reçus disjoints dans la session collectée :
- 1 session : 136 lectures ponctuelles ;
- 8 sessions : 584 ;
- 32 sessions : 2120.

Le nombre de clés scannées reste 263 et il n'y a qu'un commit.
Sur cette fixture, lectures = (sessions + 1) * reçus + 8.
Le test compare la capacité incrémentale à son oracle avant/après, et garde
les autres sessions. NoSync et oracle exclus du temps mesuré : aucun chiffre
de latence AWS n'est déduit de ces temps de portable.

Les tests précédents `ReceiverCostIsIndependentOfHistory` faisaient varier
les sessions déjà collectées et mesuraient preload/mount. Ils ne couvraient
pas ce produit pendant la collecte avec sessions encore présentes.

## Résumés : cache stable != collections alternées

Nouveau test `TestInventoryDiscoveryWorkUnderChurn` dans
`internal/summary/inventory_churn_work_test.go`.

À 8192 zones, huit sondages :
- catalogue/collection stables : 1 reconstruction ;
- nouvelle zone à chaque sondage : 8 ;
- alternance entre deux collections, sans écriture : 8 ;
- inventaire au-delà du budget : 8.

Le résultat est comparé à l'oracle indépendant des ancêtres. Le cache est
un seul inventaire, et un inventaire spécifique à A ne sert pas B. Les logs
contiennent les temps de découverte seuls ; ce sont des diagnostics, pas un SLO.
Piste : mesurer le vrai mix de polls globaux/spécifiques avant d'élargir ou
d'indexer le cache. Ne pas confondre ce scan de catalogue avec la remontée
normale des contributions en O(log n).

## Nouveau petit modèle de files

`ops/experiment/sizing/queues.mjs` représente CPU physique, writer par vnode,
I/O partagé, workers/file bornés, latence intentionnelle et dette de maintenance.
Il conserve les travaux non terminés. Dix tests déterministes passent,
dont 64 identités vnode ; le runner compare deux pas de temps.

Les coûts sont synthétiques : ce n'est pas encore une prévision AWS. Ce modèle
n'exécute pas le protocole, le provisioner, le group commit ou les boots. Il
permet de rejeter une hypothèse favorable avant de la mesurer avec le vrai code.
Un scénario qui garde les ACK mais accumule du staging/collecte n'est pas vert.

## Autres décalages à intégrer avant une prévision

Le modèle fluide utilise 64 vnodes initiaux, 16 Gio et rampes de 60 s ; le run
AWS utilise 4 vnodes initiaux, 32 Gio et rampes de 120 s. Son contrôleur est
une politique idéale indépendante, pas le Controller/Place réel. Il n'a pas de
files ou de writer, et libère les données après un délai sans coût CPU/lock.

Il faut encore relier les contrôleurs de production, boot/admission effectifs,
frontière d'autorité installée vs placement XOR vs route connue, séquences de
collecte, invalidations d'inventaire, arrivée vs ACK et abandons normaux de join.
Une vue visuelle commune ne signifie pas que les deux moteurs se comportent pareil.
