# Vnodes, machines et déplacements : pistes pour Indexus

Recherche de sources primaires du **26 novembre 2026**, après le
[run AWS interrompu](../benchmarks/aws-resource-direct-2026-11-26.md).
Ce document distingue les mécanismes décrits par d'autres systèmes des
propositions pour Indexus. Il ne change ni le protocole ni les réglages AWS.

La direction proposée est une élasticité guidée par les **ressources physiques
et la qualité du service**, avec des transferts progressifs de sous-régions.
Un vnode reste une identité logique ; le nombre de processus n'est pas une
mesure de capacité disponible. L'autorité reste décidée par les participants
et leur état durable, indépendamment des décisions de placement.

## Ce que les autres systèmes séparent

| Niveau | Question | Application à Indexus |
| --- | --- | --- |
| Partition / fragment | Quel morceau peut changer de détenteur ? | Sous-région et droits installables |
| Vnode / acteur logique | Qui sert ce morceau ? | Identité, routage et store du détenteur |
| Machine physique | Qui paie CPU, RAM, IO et réseau ? | Tous les vnodes et travaux d'un hôte |
| Contrôle de capacité | Quand faut-il créer une machine ? | Besoins non plaçables, réservations et marge |

Ces niveaux ne correspondent pas partout aux mêmes objets. Un token Cassandra,
un acteur Riak, une tablet Scylla et une VM ne sont pas quatre variantes d'un
même processus Go. Leurs règles de réplication diffèrent également.

## 1. Dynamo : protéger les requêtes contre le travail de fond

Le papier SOSP **2007** décrit, au §6.5, un contrôleur commun auquel les tâches
de fond réservent des tranches de ressources. Il ajuste leur activité selon
les latences, la contention et les files des opérations clientes. Au §6.2,
des partitions stables distinctes du placement permettent de transférer des
fichiers plutôt que retrouver chaque objet par scan. Il s'agit du Dynamo
historique, pas d'une description de DynamoDB actuel.
[Papier original, §§6.2 et 6.5](https://cdn.amazon.science/ac/1d/eb50c4064c538c8ac440ce6a1d91/dynamo-amazons-highly-available-key-value-store.pdf).

**Proposition Indexus :** copies, validations, collecte, inventaires et autres
travaux de fond doivent puiser dans un budget local commun. Le p99 et l'attente
du writer servent de retour de contrôle, en plus du CPU. Une copie réseau
rapide reste nuisible si sa comptabilité bloque les commits clients.

Les écritures concurrentes et quorums de Dynamo ne remplacent pas la cession
exclusive d'Indexus. Nous retenons son contrôle des ressources.

## 2. Cassandra et Riak : plusieurs responsabilités dans un même runtime

Cassandra appelle vnodes les multiples plages de tokens d'une instance.
Davantage de tokens facilite certains rééquilibrages mais augmente les
relations et le travail de gestion. La configuration du **tag 5.0.0** expose
des limites de streaming par nœud et un chemin de transfert de SSTables
entières sous conditions ; ce chemin n'est pas une promesse de zero-copy
universelle, notamment avec chiffrement internode.
[Production](https://cassandra.apache.org/doc/stable/cassandra/getting-started/production.html),
[configuration versionnée](https://raw.githubusercontent.com/apache/cassandra/cassandra-5.0.0/conf/cassandra.yaml).

Riak distingue plusieurs types de handoff et encadre leur concurrence.
Le guide de la branche **OpenRiak 3.4** décrit les limites par nœud et au
cluster, les réémissions après échec et les surcharges transitoires lorsqu'un
nœud reçoit avant d'avoir cédé ses propres données.
[Riak KV 2.2.3](https://docs.riak.com/riak/kv/2.2.3/using/reference/handoff/index.html),
[guide OpenRiak](https://github.com/OpenRiak/riak/blob/openriak-3.4/docs/BuildAndScaleClusterGuide.md).

**Proposition Indexus :** garder plusieurs vnodes par machine, mais ne pas
multiplier avec eux les budgets, inventaires et copies autorisées. Quatre ou
cinq vnodes ne doivent pas créer quatre ou cinq contrôleurs physiques aveugles
à leurs voisins.

À plus long terme, héberger plusieurs vnodes dans un runtime commun mérite
un benchmark : workers et caches partagés pourraient réduire des duplications.
Ce n'est ni un gain garanti ni un prérequis au correctif. Le store actuel porte
des identités durables ; fusionner ses instances ou leurs WAL demande une étude
de reprise et d'isolation. On peut d'abord coordonner les processus existants.

## 3. ScyllaDB : rendre mobiles les données, compter tous les donneurs

L'article de **2024** décrit les tablets, indépendantes des nœuds et shards,
avec une organisation LSM facilitant le déplacement de fichiers. La release
**2026.3.2**, du 30 septembre 2026, corrige notamment des sources oubliées du
quota de streaming, une attente de statistiques après split/merge et des
contrôles topologiques réémis par plusieurs shards. Le contrôle de topologie
de Scylla utilise un leader Raft ; ce n'est pas un modèle de délégation P2P
à importer tel quel.
[Tablets](https://www.scylladb.com/2024/06/17/how-tablets/),
[notes 2026.3.2](https://forum.scylladb.com/t/release-scylladb-2026-3-2/5518).

**Proposition Indexus :** réserver la copie aux deux extrémités, compter tous
les vnodes co-résidents, et fusionner les demandes de mesure identiques.
L'organisation physique doit permettre de retrouver rapidement le morceau.
Créer un fichier pour chaque minuscule zone pourrait toutefois déplacer le
coût vers les fichiers et métadonnées ; les segments devraient regrouper des
zones avec un index permettant de sélectionner leurs lignes.

## 4. Ceph : placement, équilibrage et service des IO sont trois fonctions

La documentation **Squid** sépare le placement calculé CRUSH, les corrections
du balancer et les classes de service mClock. mClock distingue notamment les
IO clientes, la récupération et le travail de fond, avec réservations, poids
et limites. Ces paramètres expriment du service IO, pas un plafond de CPU.
La documentation avertit aussi que des files indépendantes peuvent compliquer
le partage effectif d'une même ressource.
[Balancer](https://docs.ceph.com/en/squid/rados/operations/balancer/),
[mClock](https://docs.ceph.com/en/squid/rados/configuration/mclock-config-ref/).

**Proposition Indexus :** une bonne destination n'autorise pas automatiquement
un nouveau travail de copie. Il faut aussi l'admettre dans le service physique
de la source et de la cible. Le compteur de slots ne fournit pas ce service.

## 5. CockroachDB : modéliser le physique et le travail promis

La documentation `stable` consultée décrit un allocateur distribué qui suit
la charge des stores, la pression CPU physique et les mouvements proposés
non encore visibles dans les métriques. Il estime l'effet sur source et cible,
effectue peu de changements puis réévalue. Les déplacements de lease peuvent
être moins coûteux que ceux des répliques parce que les données sont déjà
présentes sur certaines destinations.
[Replication layer, MMA](https://docs.cockroachlabs.com/docs/stable/architecture/replication-layer#multi-metric-allocator).

**Proposition Indexus :** compter la pression physique et les réservations
avant que la consommation soit mesurée. Cela répond directement au cas de
deux vnodes à 45 % consommant ensemble 90 % de l'hôte.

Une machine vide d'Indexus ne possède pas déjà la sous-zone : elle doit
installer le lot avant d'en devenir l'écrivain. Le transfert peu coûteux
d'une lease entre répliques à jour ne justifie pas de sauter cette étape.

## 6. Virtualisation : déplacer doit apporter plus que son coût

Broadcom explique que DRS cherche à donner aux VM les ressources nécessaires,
sans exiger une utilisation identique sur tous les hôtes. Le papier vSphere 6
décrit le coût du déplacement aux deux extrémités et les tendances de charge ;
ce papier est historique, ses valeurs ne sont pas les défauts actuels.
[DRS actuel](https://knowledge.broadcom.com/external/article/330129),
[papier historique](https://www.vmware.com/docs/vsphere6-drs-perf).

KubeVirt distingue concurrence de migrations et débit de copie. Sa documentation
traite la convergence lorsque les pages modifiées suivent ou dépassent la
copie, ainsi que les compromis de disruption. Les nombres configurés pour
une VM ne dimensionnent pas un lot d'autorité Indexus.
[Live migration](https://kubevirt.io/user-guide/compute/live_migration/).

**Proposition Indexus :** mesurer le délai jusqu'au premier soulagement utile
et le coût transitoire complet. Une bascule de petit lot peut être utile avant
la fin du join ; l'ordonnanceur doit en profiter. Le post-copy de RAM ne donne
aucune permission d'acquitter des écritures sur une zone non installée.

## 7. Kubernetes : séparer besoin logique et manque de machines

Le HPA utilise notamment le CPU rapporté aux `requests`, ce qui n'est pas un
pourcentage implicite du serveur. Il traite les métriques manquantes et les
pods pas encore prêts. Cluster Autoscaler raisonne sur les pods non plaçables ;
Karpenter regroupe les besoins et intègre les surcoûts de placement.
[HPA](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/),
[Cluster Autoscaler](https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/FAQ.md),
[Karpenter](https://karpenter.sh/docs/faq/).

**Proposition Indexus :** une demande de soulagement existe avant le boot.
Le système tente de la placer, réserve une destination ou un démarrage, puis
ne crée une machine que pour le besoin restant. Les démarrages déjà acceptés
ne sont ni ignorés ni comptés deux fois. La réserve en pourcentage reste une
marge ; elle ne remplace pas la file de demandes distinctes en attente.

## 8. Deux pistes algorithmiques, sans changer le routage maintenant

Slicer (OSDI **2016**) choisit des déplacements selon le bénéfice d'équilibrage
rapporté au changement d'affectation et borne ce changement. Il sépare son
contrôle du chemin de requêtes. Il utilise un contrôleur d'affectation et un
mécanisme optionnel de cohérence qui ne correspondent pas à l'autorité P2P
d'Indexus. Son coût de mouvement en espace de clés ne suffit pas à prédire
notre coût de copie en octets, sessions et validations.
[Papier original](https://storage.googleapis.com/gweb-research2023-media/pubtools/4405.pdf).

Les travaux sur le consistent hashing à charge bornée donnent des garanties
dans des modèles d'objets et de capacités définis. Ils ne bornent pas
automatiquement le p99, le CPU hétérogène ou le temps d'une migration réelle.
[Mirrokni, Thorup, Zadimoghaddam, version 2017](https://arxiv.org/abs/1608.01350v3).

**Proposition Indexus :** conserver XOR pour le placement nominal et sélectionner
des délégations profitables à partir de mesures. La priorité est de rendre
le contrôle existant cohérent, avant d'introduire un autre hashing ou une
recherche réseau plus complexe.

## Architecture minimale proposée

Ce qui suit est notre synthèse à tester, pas une configuration copiée.

### Une décision physique, plusieurs vnodes

Un arbitre local par machine dispose de faits horodatés : CPU total et propre
à chaque processus, RAM, disque, IO, attente des writers et opérations en cours.
Il coalesce les demandes de ses vnodes en une action utile. Une identité de
machine physique est distincte de l'adresse XOR d'un vnode.

| Pression observée | Première action à évaluer |
| --- | --- |
| CPU surtout consommé par des copies | Réduire et ordonner le travail de fond |
| CPU client élevé sur l'hôte | Déléguer à distance un morceau qui retire cette charge |
| Writer saturé, CPU physique encore disponible | Réduire la validation ; évaluer une séparation locale des responsabilités |
| RAM ou espace physique insuffisant | Déplacer et collecter à distance ; un split local ne libère pas la machine |
| Demandes de soulagement non plaçables | Réserver/créer de la capacité physique |

Un split local peut exploiter des cœurs inutilisés ou réduire une sérialisation,
mais il ne retire pas de CPU ou de données de la machine. Il ne doit pas être
présenté comme un soulagement physique automatique.

### Préparer sans geler, puis terminer un lot borné

La source et la cible réservent leurs ressources avant la fence. Le coût
comprend préparation, staging, validation, installation et collecte, et pas
seulement les octets réseau. Une copie locale acquiert un budget physique
composite, pas deux budgets indépendants sur la même machine.

Les réservations de ressources ne sont pas des droits d'écrire. Leur expiration
peut libérer une capacité d'ordonnancement, jamais recréer une autorité cédée.
Deux transferts opposés doivent éviter de retenir chacun une réservation en
attendant indéfiniment celle de l'autre : acquisition non bloquante et relâchement
des crédits, ou ordre déterministe des ressources, avant toute fence.

Le scheduler protège le trafic client et garde une voie de terminaison pour
les lots déjà irréversibles. Suspendre indéfiniment une installation après la
fence aggraverait précisément le freeze. Le delta final doit donc être borné
et ses moyens de terminaison disponibles avant de céder le droit.

### Choisir un morceau pour son soulagement, pas seulement ses items

Préférer un fragment dont le déplacement retire rapidement une part importante
de la pression, pour un faible coût transitoire. Mesurer sa fréquence, son
volume et son coût de contrôle. Une seule clé brûlante, non divisible, reste
limitée par la capacité de son écrivain ; augmenter le nombre de vnodes ne
supprime pas cette limite.

Le modèle distingue l'état pendant la copie de l'état après bascule : on ne
soustrait pas à la source un soulagement qui n'a pas encore eu lieu. Les
destinations promises comptent dès l'admission dans la projection de la cible.

### Deux horizons de contrôle

La réaction rapide utilise la demande reçue, l'âge de queue, les temps de
writer et les ressources physiques. L'ajustement plus lent dimensionne les
machines, consolide et retire avec hystérésis. L'horizon de prévision découle
des délais mesurés de boot et de soulagement, pas d'un délai fixe arbitraire.

La réserve dépend à la fois de la marge souhaitée, des demandes non placées,
des offres réellement prêtes et des boots déjà réservés. Les capacités CPU,
RAM, disque et IO restent des contraintes distinctes : des cœurs libres sans
espace pour le lot ne sont pas une offre utile.

La cible de 60 % reste une politique tant qu'aucune régulation effective ne
borne le travail. Un pourcentage ne constitue pas à lui seul une garantie de
p99 constant sous n'importe quelle charge.

## Priorités compatibles avec le code actuel

1. En parallèle : supprimer le contrôle de capacité redondant, borner l'accès
   aux sessions de staging et les préparations sous writer ; corriger le
   signal CPU physique et la demande masquée par les ACK.
2. Ajouter l'arbitrage de ressources commun aux copies de tous les vnodes
   d'un hôte, et démarrer tôt les tâches sûres des receveurs partiellement actifs.
3. Dimensionner le provisioning sur les demandes identifiées et la capacité
   réservée, avec motifs observables de retenue du scale.
4. Mesurer ensuite une organisation de snapshots réutilisables par morceau,
   en distinguant le chemin local de la copie distante. Ne pas introduire
   un nouveau format de stockage sans comparer coût et reprise.
5. Comparer une flotte fixe sans joins et une flotte élastique, à demande et
   données comparables. Faire varier séparément zones étrangères, sessions
   mortes et copies co-résidentes pour identifier les coûts qui grandissent.

Les points de passage sont : admission, premier lot durablement installé,
premier ACK au receveur, route directe apprise, charge effectivement retirée
de la source, puis fin du join et collecte. Aucun de ces changements de
contrôle n'exige une validation globale du réseau pour déplacer une sous-zone.

Liens : [politique actuelle](../design/placement-and-resources.md),
[limites de la simulation](../benchmarks/resource-sizing-2026-11-26.md),
[preuves et récapitulatif AWS](../benchmarks/aws-resource-direct-2026-11-26.md).

La suite [threads, maintenance et expériences 2025–2026](thread-scheduling-experiments-2026.md)
distingue les principes réutilisables des refontes de runtime/noyau et propose
les variantes locales à comparer avant une nouvelle campagne AWS.
