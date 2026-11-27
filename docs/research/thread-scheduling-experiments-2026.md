# Vnodes, threads et maintenance : expériences issues des travaux récents

Revue du 26 novembre 2026. État étudié : `204317f`, avec les mesures AWS de
`420061d`. Ce document prépare des expériences pour Claude ; il ne rapporte
aucun nouveau benchmark ni gain mesuré. Aucun changement de protocole ou
nouveau déploiement AWS n'est nécessaire pour les premières étapes.

Lire aussi [élasticité P2P et virtualisation](p2p-vnodes-and-physical-elasticity.md)
et [le diagnostic AWS](../benchmarks/aws-resource-direct-2026-11-26.md).

## Ce que nous cherchons à améliorer

Une machine héberge plusieurs vnodes. Chacun reçoit des écritures, prépare des
lots, installe des données et entretient son stockage. Le besoin est double :
laisser les vnodes emprunter la capacité disponible, tout en empêchant plusieurs
copies de consommer simultanément toute la marge nécessaire aux clients.
Un cœur réservé par vnode serait une politique différente, à comparer plutôt
qu'à supposer meilleure.

Le diagnostic actuel montre surtout de la validation coûteuse sous `writeMu`,
des scans de staging répétés et un signal CPU individuel qui masque la pression
physique. Préempter une goroutine ne lui retire pas son mutex : une meilleure
équité CPU peut même allonger la tenue du verrou en temps réel. Nous devons donc
séparer coût inutile, concurrence admise et ordonnancement des tâches exécutables.

Quatre ou cinq processus voyant chacun quatre CPU peuvent chacun choisir quatre
P Go. Cette somme n'est ni une réservation de cœurs ni une preuve de problème :
il faut mesurer les tâches effectivement exécutables et leurs temps d'attente.

## Travaux récents et transpositions possibles

Les principes proposés dans la dernière colonne sont nos hypothèses pour
Indexus. Les résultats des auteurs ne constituent pas des prévisions de gains.

| Publication primaire | Apport pertinent | Ce qui est testable ici / ce qui ne l'est pas directement |
| --- | --- | --- |
| [Aspen-Go, NSDI 2025](https://www.usenix.org/conference/nsdi25/presentation/guo), dans *The Benefits and Limitations of User Interrupts for Preemptive Userspace Scheduling* | La préemption fine reste limitée par les files et les choix des couches supérieures. | Mesurer les files et bornes de travail. Le prototype modifie Go et dépend des interruptions utilisateurs ; ce n'est pas une option du Go standard. |
| [HATS, FAST 2026](https://www.usenix.org/conference/fast26/presentation/ren), *Holistic and Automated Task Scheduling for Distributed LSM-tree-based Storage* | Coordonne lectures et maintenance plutôt que de régler chacune isolément. | Adapter l'admission de copie aux attentes des écritures et lectures. Le choix de répliques de Cassandra ne s'applique pas directement à notre autorité unique. |
| [HarvestContainers, NSDI 2026](https://www.usenix.org/conference/nsdi26/presentation/hall) | Récupère des cœurs momentanément libres tout en protégeant les tâches sensibles, notamment des interférences réseau. | Emprunter la marge disponible puis la rendre sous pression. L'artefact inclut un module noyau et une intégration Kubernetes : « sans réécrire l'OS » ne signifie pas « sans composant noyau ». |
| [Svalinn, OSDI 2026](https://www.usenix.org/conference/osdi26/presentation/pardeshi) | Sépare admission du débit et contrôle de latence, selon plusieurs goulots, dont les verrous. | Des crédits par ressource/classe de travail sont plus pertinents qu'un unique pourcentage CPU. Commencer avec les files explicites et les mesures existantes. |
| [vBOIDs, OSDI 2026](https://www.usenix.org/conference/osdi26/presentation/manakkal) | Regroupe les tâches et équilibre à deux niveaux pour limiter les oscillations. | Machine → vnodes → lots constitue une structure de régulation possible. Le scheduler noyau du papier, évalué à très forte densité de threads, n'est pas un réglage immédiat de nos VM. |
| [SBB, OSDI 2026](https://www.usenix.org/conference/osdi26/presentation/hu-kang), *Eliminating Centralized Bottlenecks in Userspace Network Runtime* | Distingue allocation CPU, préemption et équilibrage des workers. | Garder ces responsabilités séparées. Son runtime réseau spécialisé et ses mécanismes matériels demanderaient une qualification distincte. |
| [Come Hell or Still Water, NSDI 2026](https://www.usenix.org/conference/nsdi26/presentation/hu-chaolei) | Des rafales de quelques disques virtuels dégradent les autres ; le système emploie un double bucket et une séparation de tâches. | Comparer budget machine seul et budget machine + contributeur. Il s'agit de notre adaptation aux vnodes, sans reprendre les gains du block store Alibaba. |
| [DINGO, OSDI 2026](https://www.usenix.org/conference/osdi26/presentation/athlur), *Scaling the IO Wall with Declarative IO* | Expose les données et échéances de maintenance pour créer des réutilisations entre tâches. | Tester un snapshot versionné partagé par deux consommateurs pendant un lot, avec mémoire bornée. Le contexte HDD et son ordonnanceur distribué ne sont pas transposés. |
| [Atropos, SOSP 2025](https://yigonghu.github.io/paper/sosp25-atropos.pdf), *Mitigating Application Resource Overload with Targeted Task Cancellation* | Cible le travail responsable de la surcharge plutôt que toutes ses victimes. | Tester l'annulation coopérative d'une préparation chaude relançable. Une cession durable ne peut jamais être annulée pour améliorer une latence. |

Ces travaux prolongent [Arachne, 2018](https://www.usenix.org/conference/osdi18/presentation/qin),
[Shenango, 2019](https://www.usenix.org/conference/nsdi19/presentation/ousterhout),
[Caladan, 2020](https://www.usenix.org/conference/osdi20/presentation/fried) et
[Junction, 2024](https://www.usenix.org/conference/nsdi24/presentation/fried).
Ils justifient de distinguer attribution des ressources et exécution du travail.
Ils ne justifient pas encore de remplacer Go, Linux ou H3 dans Indexus.

## Architecture expérimentale minimale

L'agent physique tient un budget partagé par les processus de la machine.
L'émetteur et le receveur réservent chacun leur capacité avant de commencer un
lot. Le placement considère les capacités déjà promises, pas seulement le CPU
observé lors du dernier échantillon.

Trois catégories doivent rester distinguables :

1. Les écritures et lectures clientes, avec âge de file et charge offerte.
2. La préparation chaude et la maintenance différables, découpées en unités bornées.
3. La terminaison d'une bascule déjà engagée, dont les ressources ont été réservées.

Une priorité absolue aux clients affamerait les transferts. Une priorité absolue
aux copies reproduirait le décrochage. Tester un partage avec progrès minimal
pour chaque transfert, puis l'ajuster selon les latences et la marge physique.
Commencer par une seule dimension mesurée ; ne pas construire d'emblée un
optimiseur universel CPU/RAM/disque/réseau.

Les autorisations de ressources sont des outils d'ordonnancement. Leur expiration
ne rend jamais un droit d'écriture déjà cédé à la source. Aucun accord global
n'est ajouté au cutover. Attendre un crédit se fait avant les verrous, et jamais
avec un sommeil ou un yield sous le writer.

## Ordre des expériences à confier à Claude

| Essai | Modification isolée | Question et critère d'abandon |
| --- | --- | --- |
| E0 — travail redondant | Éviter le double `checkCapacity` dans la même transaction ; réutiliser l'inventaire de sessions si ses dépendances restent inchangées. | Le CPU et la tenue de writer baissent-ils à travail identique ? Abandon si la validation atomique finale ou la reprise perd une garantie. |
| E1 — copies concurrentes | Une puis deux copies chaudes actives par machine, comptant tous ses vnodes et les deux rôles émission/réception. | Réduit-on le p99 sans bloquer les bascules engagées ni retarder excessivement le premier soulagement ? |
| E2 — partage Go | `GOMAXPROCS=1`, `2`, puis défaut, à flotte fixe de quatre puis cinq processus sur quatre vCPU. | La latence runnable explique-t-elle une part significative du retard ? Rejeter si les gels en temps réel augmentent ou si le débit utile baisse. |
| E3 — mémoire/GC | `GOGC=100` puis `200`, avec enveloppe mémoire agrégée fixée et `GOMEMLIMIT` explicite. | Réduit-on réellement le CPU par ACK sans pression mémoire, swap ou déplacement du coût vers les lectures ? |
| E4 — phases de maintenance | Décaler les passages périodiques coûteux avec un jitter stable par vnode ; conserver le premier passage immédiat. | Évite-t-on des scans simultanés ? Ne pas retarder liveness, reprise urgente ou fermeture irréversible. |
| E5 — isolation Linux | Cgroups distincts et `cpu.weight`, d'abord sans quota dur. | L'équité entre processus aide-t-elle ? Cela ne sépare pas les goroutines clientes et copie dans un même processus. |

E0 est d'abord une correction de travail inutile identifié ; E1 est la première
expérience de politique. Faire E2/E3 séparément, après ces témoins, pour attribuer
les gains. Ne pas cumuler cinq réglages dans une variante « optimisée » impossible
à expliquer.

La suppression du double contrôle exige de vérifier que les deux appels portent
bien sur la même transaction et que le contrôle final voit toutes les mutations.
Le cache de sessions nécessite une invalidation locale correcte. Une session
encore porteuse de staging n'est pas nécessairement un transfert d'autorité ouvert.
Un test concurrent doit modifier ces dépendances entre préparation et publication.

### Budget mémoire commun

`internal/autoscale/resources.go:defaultHeapLimit` choisit actuellement la moitié
de la RAM hôte par processus sur les grandes machines, sauf `GOMEMLIMIT` explicite.
Cinq limites de ce type ne forment pas une enveloppe physique sûre. Le run ne
montre pas de saturation RAM ; c'est néanmoins un défaut de dimensionnement
à traiter avant d'augmenter fortement la densité.

Pour le labo, choisir une enveloppe commune après réserves système, caches et
buffers, puis la répartir entre processus. Une limite Go est souple et n'est
pas un plafond RSS. La cible de 60 % doit être observée à l'échelle physique,
pas déduite de la somme de ces limites.

### Détails Go et Linux à ne pas confondre

- Le binaire AWS utilisait Go 1.26. Green Tea y est déjà activé par défaut ;
  l'activer n'est donc pas une nouvelle optimisation. Voir les
  [notes Go 1.26](https://go.dev/doc/go1.26).
- `GOMAXPROCS` borne le parallélisme Go, pas le quota CPU ni tous les threads OS.
  Sa détection des quotas cgroup ne partage pas les ressources entre processus
  concurrents sans quotas. Voir [Go 1.25 et les conteneurs](https://go.dev/blog/container-aware-gomaxprocs).
- `cpu.weight` est relatif sous contention ; `cpu.max` peut suspendre un groupe
  après épuisement du quota périodique. Un quota dur peut donc dégrader le p99.
  Voir [cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html).
- `LockOSThread` ne réserve pas un cœur physique et `Gosched` ne crée pas une
  priorité de goroutine. Réduire le nombre de P peut prolonger un commit en temps réel.
- `sched_ext`, l'affinité stricte, DPDK et les forks de Go restent des pistes
  secondaires, après preuve d'un goulot d'ordonnancement. La version/configuration
  exacte du noyau AWS n'a pas été capturée ; ne pas présumer sa disponibilité.

## Banc et mesures : une expérience qui peut contredire notre hypothèse

D'abord flotte fixe sans joins, puis un join, puis deux transferts sortants et
entrée/sortie simultanées. Garder commit, seed, nombre de zones, volume, charge
offerte, fsync et machine identiques. Tester séparément distribution uniforme
et hotspot contrôlé. Faire varier indépendamment sessions anciennes, versions
mortes et zones étrangères pour détecter les coûts qui grandissent sans travail utile.

Publier au moins trois répétitions non profilées par variante, avec ordre alterné,
échauffement identique et résultats individuels. Ajouter un run profilé court
pour expliquer un symptôme. Une amélioration doit dépasser la dispersion observée.

Relever ensemble :

- p50/p95/p99 des écritures et lectures, côté service et depuis l'arrivée prévue ;
- offre, ACK/s, erreurs, refus, reprises et âge/volume de file, sans omission coordonnée ;
- attente et tenue du writer par type de commit, validation, fsync, octets et lignes parcourues ;
- CPU physique et par processus, latence runnable, CPU/pauses GC, allocations, RSS et swap ;
- premier lot installé, premier ACK receveur, route directe apprise, soulagement source,
  fin du join et plus ancien transfert sans progrès.

`runtime/metrics` et un court `runtime/trace.FlightRecorder` peuvent départager
scheduler, mutex, I/O et GC. Mesurer aussi leur surcharge. Les histogrammes
cumulatifs doivent être différenciés entre échantillons.
Sources : [runtime/metrics](https://pkg.go.dev/runtime/metrics),
[FlightRecorder](https://pkg.go.dev/runtime/trace#FlightRecorder).

### Limite du labo actuel

`Test64NodeDurableHTTPRecovery` place plusieurs vnodes **dans un seul processus**,
sur un seul hôte physique. Les champs `hosts`/`slots` représentent des dispositifs
simulés. Faire varier `GOMAXPROCS` dans ce test n'émule donc pas cinq processus
Go concurrents : E2/E3/E5 exigent des processus distincts et un générateur externe.
Ne pas publier leurs résultats depuis ce labo comme des mesures d'isolation machine.

Le labo existant reste utile pour les garanties et l'amplification de travail :

```sh
INDEXUS_LAB64=1 INDEXUS_BENCH_NO_USAGE_SCAN=1 \
INDEXUS_LAB_NODES=8 INDEXUS_LAB_DIRECT=1 INDEXUS_LAB_FAULTS=0 \
INDEXUS_LAB_PHASE_SECONDS=5 INDEXUS_LAB_RATE=400 \
INDEXUS_LAB_OUT=/tmp/indexus-exploration-reference \
go test ./indexus -run '^Test64NodeDurableHTTPRecovery$' -count=1 -timeout=10m
```

Cette commande est une configuration de départ, pas un résultat ni une calibration
validée sur le portable. Employer un dossier neuf par run, puis rejouer avec
`INDEXUS_LAB_FAULTS=1`. Les comparaisons runtime multi-processus demandent un petit
harnais distinct, à préparer avant de lancer la matrice.

Une baisse du p99 obtenue en refusant davantage d'écritures, en diminuant l'offre
ou en abandonnant les transferts n'est pas un succès. Conserver les audits
d'écrivain unique, des ACK et des lectures/totaux exacts, avec crash et reprise.

## Décision proposée

Retenir maintenant E0 puis E1 ; instrumenter pour décider si E2/E3 valent la peine.
Conserver cinq vnodes par machine comme hypothèse de densité à comparer à quatre,
sans imposer un cœur à chacun. Le placement optimise le temps jusqu'au premier
soulagement sous budget physique, plutôt que le remplissage de slots ou la fin
complète du join. Les changements de runtime ou de noyau restent conditionnés à
une preuve de coût que nous n'avons pas encore.

## Deux extensions si les premiers résultats le justifient

**Réutilisation bornée de lecture.** Comparer deux consommateurs du même snapshot
immuable à une lecture/décodage unique réutilisée pendant le lot. Identifier la
version dont chacun dépend ; une copie chaude ne remplace pas la preuve du dernier
delta. Injecter une écriture entre préparation et fence. Abandonner si la mémoire
suit tout le transfert ou si une version périmée peut être acceptée.

**Annulation ciblée avant fence.** Comparer l'abandon de requêtes clientes victimes
à la suspension d'une préparation chaude longue, puis à sa reprise. Conserver les
engagements irréversibles et leurs ressources réservées. Le test doit montrer moins
de travail inutile, pas seulement déplacer les délais vers la fermeture des joins.

La mesure de travail existante a aussi été rejouée pendant cette revue :
`INDEXUS_BENCH_NO_USAGE_SCAN=1 go test ./internal/storage -run '^TestLotTransferWork$' -count=1 -v`
passe en 2,30 s. Elle compte les opérations de transfert ; elle ne mesure ni le p99
ni l'isolation physique. La première page de scan est déjà bornée à huit dans
`420061d` : ce n'est pas une optimisation restant à appliquer au run diagnostiqué.
