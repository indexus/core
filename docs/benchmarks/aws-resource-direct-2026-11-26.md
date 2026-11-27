# AWS : capacité disponible, mais élasticité bloquée pendant les joins

Run `aws-resource-direct-16k-20261126`, source `420061d`, générateur figé
sur `2362b35`. Analyse du 26 novembre 2026, après interruption et suppression
des ressources. Trois analyses indépendantes ont été croisées :
[placement](aws-resource-direct-20261126/analysis-placement.md),
[transferts et verrous](aws-resource-direct-20261126/analysis-transfers.md),
[distribution](aws-resource-direct-20261126/analysis-distribution.md).

## Résultat

La campagne n'a pas validé 16k/s. Le débit reste bon à 2k puis 4k/s, avant
de décrocher pendant la montée suivante. Elle a été interrompue à 8k demandées,
avec environ 3,3k ACK/s, malgré cinq machines vides et admissibles.

| Instant de charge | Demande/s | ACK/s | Vnodes | Machines | Vides | P99 client |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 105 s | 2 000 | 2 000 | 8 | 6 | 1 | 8,2 ms |
| 242 s | 3 985 | 3 987 | 8 | 6 | 1 | 16,4 ms |
| 399 s | 5 217 | 4 564 | 17 | 14 | 0 | 1 048,6 ms |
| 434 s | 6 384 | 4 113 | 20 | 18 | 0 | 2 097 ms |
| 515 s | 8 000 | 3 317 | 20 | 21 | 5 | 2 097 ms |

Le débit et les quantiles sont ceux de la fenêtre du dashboard, pas ceux de
toute une phase. Des ACK/s légèrement supérieurs aux arrivées/s correspondent
notamment au drainage d'une fenêtre précédente. Les abandons du générateur ne
sont pas des refus HTTP du serveur. À t515 sa file de 16 384 places est pleine.
Le relais reste autour de 0,2–0,3 % : le détour systématique par l'ancien
détenteur n'est pas l'explication de cette campagne.

À t515, la moyenne CPU pondérée de la flotte, calculée sur les fenêtres de
30 secondes, est d'environ **30,6 %**, tandis que certains hôtes dépassent
95 %. La capacité physique totale est donc très inégalement mobilisée.

Configuration : six `i4i.xlarge` au départ, quatre vnodes initiaux, cinq slots
par machine, plafond de 32 hôtes, H3 et routage direct. Rampes et plateaux de
120 s, seuil de demande 500/s/vnode, items 500 000, signaux de ressources
activés, réserve de réception 15 %, loader `c6i.4xlarge` avec 4 096 workers.
Les vraies machines ont quatre vCPU et environ **32 Gio de RAM** ; les 16 Gio
du modèle exploratoire étaient une hypothèse, pas leur configuration réelle.

## Ce qui est établi

### 1. Le déclencheur ne voit pas le CPU de la machine

Le placeur utilise les faits de l'hôte. `Node.AutoscaleTick` utilise en revanche
`SampleResources`, dont le CPU est celui de **son seul processus**, normalisé
par tous les cœurs de la machine. Deux vnodes à 45 % peuvent donc saturer
ensemble la machine à 90 %, tout en restant chacun sous le seuil 60 %.

Sur `10.77.1.145`, sept observations successives couvrent 90 secondes avec CPU
hôte ≥85 %, mais deux CPU individuels <60 % et deux débits appliqués <500/s.
À une observation : hôte 97,8 %, vnodes 53,9 % et 42,3 %. La somme des CPU des
processus confirme la saturation ; il ne s'agit pas seulement d'un indicateur
hôte comptant l'attente disque.

Le signal de demande est lui aussi incomplet : il est principalement incrémenté
après `Apply`, et le contrôleur reçoit `Queue: 0`. Quand le service bloque,
son débit servi baisse ; son signal d'écritures peut donc passer sous le seuil
alors que les arrivées externes augmentent. Les deux mécanismes peuvent laisser
une machine saturée sans demande supplémentaire de renfort.

### 2. Les transferts se disputent les mêmes ressources physiques

À t428, un hôte portant deux vnodes exécute trois opérations sortantes
simultanées, dont deux depuis le même vnode. Son CPU est d'environ 96 %,
confirmé par les compteurs des processus.

Le budget de réception de 1,75 cœur n'est pas un limiteur. Il ne réserve pas
le coût du donneur, ni celui de plusieurs rôles simultanés sur un même hôte.
Réduire simplement cette estimation pour remplir les cinq slots risquerait
d'accepter encore plus de copies concurrentes.

La réserve de 15 % ne compte pas les demandes distinctes en attente. Elle peut
ne réclamer qu'un receveur alors que plusieurs vnodes franchissent simultanément
leur seuil. Un boot en vol couvre alors cette petite réserve et retarde les
suivants. Cette politique introduite avant le run doit être corrigée : un
pourcentage de CPU observé ne remplace pas un backlog de renfort dédupliqué.

### 3. Un vnode peut écrire avant que ses workers soient démarrés

Le monitoring est ouvert tôt, mais `jobs.Start()` attend le retour de la
première tentative de `Join`, qui peut durer deux minutes. Le nouveau vnode
peut déjà appliquer des écritures sans faire tourner autoscale ni initialiser
ses compteurs d'items. Il faut démarrer tôt les tâches compatibles avec un
état partiellement installé, en testant séparément celles de reprise et de
rééquilibrage ; démarrer tous les jobs sans vérifier leurs préconditions ne
serait pas un correctif sûr.

### 4. Une grande partie du coût de transfert est du contrôle sous verrou

Sur un receveur, pendant une fenêtre de 68 s, la succession de commits tient
`writeMu` pendant **67,76 s**. La validation/comptabilité en représente 63,95 s.
Ce n'est pas un freeze continu de 68 s, mais presque aucune place restante
entre ces sections critiques pour les commits d'écritures.

Le profil montre `reservationDelta` à 33,35 % du CPU ; 99,37 % de ce coût
vient de `stagingSessions`, qui redécouvre les sessions par scan. Le profil
établit ce coût, mais pas encore la part causée précisément par les anciennes
versions et tombstones : cette amplification doit être reproduite dans le
vrai zonelog, pas seulement dans un faux lecteur de lignes visibles.

`PreloadCausal` exécute en outre `checkCapacity` deux fois dans la même
transaction. Ses données sont encore préparées sous `writeMu`. Le montage
préparé hors verrou n'a donc pas résolu le chemin de preload.

Le chemin `PlanningTotals` reste un parcours complet appelé par l'autoscale,
malgré le nouvel échantillon de décision à 512 zones. Il représente 14,38 %
du CPU du donneur profilé. L'échantillonnage ne borne donc pas tout le travail
effectué à chaque décision.

Dans les journaux, le pire `hold_max` vaut 974 ms, dont `install_max` 973 ms,
contre 38 ms de fence source sur la même opération. Ces valeurs sont des
maxima par opération journalisée, pas des quantiles clients ni la distribution
de tous les lots. Les unités portent environ 31 zones en moyenne : le coût
fixe n'est pas systématiquement amorti sur 512 zones.

## Distribution des données

Le générateur ne demande aucun hotspot. Les locations **acquittées** sont
uniformes sur leur premier caractère : coefficient de variation 0,61 % sur
64 symboles ; les collections principales ont un CV de 1,13 %. Cela ne
reconstitue pas les clés abandonnées ou restées en file.

À 2k/s, les huit vnodes appliquent environ 243–255 écritures/s chacun. Pendant
la montée, le max/moy des applications par vnode atteint 2,33. La géométrie
XOR souhaitée de la membership finale n'explique qu'un max/moy de 1,25 ; elle
ne suffit donc pas à expliquer la concentration observée. Cette géométrie
souhaitée n'est pas l'autorité déjà installée pendant les transferts.

À t515 :

| Machine | CPU hôte | Items affichés* | Applications/s** | Vnodes |
| --- | ---: | ---: | ---: | ---: |
| 10.77.1.145 | 95,4 % | 205 263 | 450 | 2 |
| 10.77.3.17 | 85,6 % | 262 491 | 556 | 2 |
| 10.77.7.69 | 76,6 % | 287 236 | 621 | 2 |
| Cinq hôtes vides | 0–0,8 % | 0 | 0 | 0 |

* Inventaire consultatif et retardé, pas un snapshot global.
** Recalculé avec `Δwrites.applied / Δuptime_seconds` sur environ 20 s.

Les deux dernières machines portent 32,8 % des applications et 34,3 % des
items affichés, pour une part XOR souhaitée de 18,75 %. En parallèle, trois
receveurs affichent zéro item malgré 6,8–18,9 Mio Owned et 18–64 applications/s.
Un zéro avant initialisation du compteur ne signifie donc pas une absence de
données ou de trafic.

Après arrêt des écritures, un receveur reste à 86,6 % CPU. Dans les instantanés
analysés, RAM utilisée <4 % et disque occupé <0,76 %. Le manque de RAM ou
d'espace ne suffit pas à expliquer le run ; cela ne mesure pas les IOPS ou
le temps de fsync de tous les hôtes.

## Limites du modèle et de la validation

Le modèle de dimensionnement simulait un budget partagé de copie, une
observation des ressources physiques et des nouveaux nœuds déjà capables de
participer au contrôle. Le système réel n'a pas encore ces trois propriétés.
Il omettait aussi le coût des validations, les sessions mortes et les scans
de comptage. Ses 465 scénarios ne prédisent donc pas le p99 de cette campagne.

Le ledger sauvegardé contient 1 715 230 ACK : 1 713 679 écritures principales
et 1 551 sondes. Le total des items affichés et celui des applications cumulées
sont d'autres mesures, non interchangeables. L'audit a été interrompu (exit
137) : **aucune conclusion de perte, duplication ou audit réussi n'est établie**.
Les profils après arrêt des écritures coïncident avec cet audit et des lectures
de contrôle ; ils ne décrivent pas un repos sans aucune requête.

Les 22 instances ont été terminées et l'absence du VPC, du security group,
du template, du rôle/profil IAM et du bucket de ce run a été vérifiée.
L'estimation EC2 entre lancement et demande de terminaison est 1,455 USD ; la
borne calculée jusqu'à vérification est 2,201 USD. EBS, S3, API, transferts et
taxes ne sont pas inclus ; ce n'est pas une facture AWS.

## Suite proposée et répartition du travail

1. **Élasticité / placement** : reproduire le cas deux vnodes à 1,8 cœur chacun
   sur quatre cœurs ; déclencher sur une pression physique, coalescer les
   demandes par machine et distinguer arrivées, attente, applications et ACK.
   Ne pas diffuser le même signal à cinq vnodes pour produire cinq spawns.
2. **Stockage / transfert** : conserver un seul contrôle final atomique de
   capacité, puis supprimer la redécouverte coûteuse des sessions. Un index
   doit représenter les sessions avec contenu de staging, même si leur droit
   est déjà fermé, jusqu'à la collecte effective ; ce n'est pas simplement
   la liste des transferts d'autorité ouverts.
3. **Chemin critique** : préparer le preload hors writer avec une validation
   de version pertinente au commit. Une version globale invalidée par toute
   écriture pourrait provoquer une famine de retries ; borner et tester la
   concurrence, sans affaiblir l'installation durable atomique.
4. **Ordonnancement physique** : partager le budget entre tous les donneurs,
   receveurs et collectes d'un hôte. Quand les copies saturent le CPU, en lancer
   davantage n'est pas nécessairement l'action de soulagement utile.
5. **Cycle de vie et réserve** : commencer les tâches sûres dès l'activation
   partielle ; mesurer le backlog distinct, les offres prêtes, les boots
   acceptés et les réservations, sans les compter deux fois.

Les tests proposés dans les trois analyses doivent couvrir le vrai zonelog
avec compaction retardée, les replays, la capacité exacte, crash/reopen,
les écritures sur une zone indépendante pendant un preload, et les lectures
pendant une bascule. L'autorité reste locale : ces corrections ne demandent
aucune validation globale du réseau.

## Dashboard demandé

Conserver les courbes globales et ajouter une vue de machines regroupant les
vnodes sous forme de cercles, avec état explicite et petites jauges physiques.
Les flèches doivent agréger les opérations par paire de machines, pas dessiner
chaque zone ni inventer la topologie gossip. Distinguer copie locale et distante.
Ces flèches décrivent les transferts de zones ; elles ne prouvent pas qu'une
migration générique d'un processus vnode existant entre hôtes est implémentée.

Conserver les items par vnode, par machine et leur somme observée, avec âge et
statut d'initialisation. Afficher séparément ACK du run, inventaire consultatif,
octets Owned/Staging/Residual et total racine demandé. Un total exact global
ne se déduit pas de la somme de compteurs asynchrones. La vue doit pouvoir
montrer « écrit déjà, join encore ouvert » et la raison d'un scale retenu.

Le défaut de taux de la première vue a été corrigé après ce run, avec un test
de régression : elle divisait les counters par les cinq secondes de l'agrégateur
alors que les observations des processus étaient espacées de quatre ou six
secondes. Le calcul sur les timestamps propres aux mesures évite une oscillation
artificielle de ±20 %. Ce correctif de télémétrie ne corrige pas le déficit
d'ACK observé ni le protocole du binaire testé.

Un prototype local fondé sur les vrais instantanés a été créé pour cette
discussion ; il n'est pas encore le rendu déployé du dashboard.

La [comparaison avec les systèmes P2P et la virtualisation](../research/p2p-vnodes-and-physical-elasticity.md)
précise les mécanismes de contrôle de ressources proposés pour la suite, leurs
sources primaires et les différences de garanties à conserver.
