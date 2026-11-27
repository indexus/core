# Réplication des zones d’Indexus

**Statut : proposé.** Conception de la réplication ; il cite encore Bolt.

*Remplacée par [convergent-replication.md](convergent-replication.md). Son contrat (REP-1 à REP-7) est archivé dans [replication-majority-2026-11.md](../archive/replication-majority-2026-11.md) ; le journal et le modèle sont dans l'historique git (`7379aaf`).*

*Note de conception — 16 novembre 2026. Lecture du dépôt à `b6a71db`, sans modification ni exécution des tests. Les mécanismes proposés ci-dessous ne sont pas implémentés.*

**Recommandation : un primaire et deux répliques, avec journal durable acquitté à majorité et protocole de consensus pour les élections et les changements de configuration.** La réplication doit couvrir les données **et les décisions d’autorité**. Elle devient ensuite le mécanisme de préparation des joins, handoffs et splits.

## 1. État actuel en dix lignes

1. `Node.meshWrite` construit une requête identifiée et passe par `MeshData`, puis `mesh.Service.Handle` ([mesh_runtime.go](../../indexus/mesh_runtime.go)).
2. Le service tente l’application locale ; sinon il suit l’autorité durable, avec une limite de forwarding de 64 sauts ([service.go](../../internal/mesh/service.go)).
3. `ApplyRecordContext` pré-vérifie l’autorité, entre dans la file, puis la vérifie à nouveau dans la transaction ([apply.go](../../internal/storage/apply.go)).
4. Cette transaction conserve ensemble record, séquence **par zone**, mutation complète, reçus et compteurs de capacité.
5. `diskBatch` sérialise les group commits ; chaque appel attend leur résultat synchronisé avant de réussir ([write_batch.go](../../internal/storage/write_batch.go)).
6. Le moteur actuel est **zonelog** : données et contrôle partagent un lot atomique ; la visibilité suit la synchronisation du journal ([records.go](../../internal/storage/records.go), [log.go](../../internal/zonelog/log.go)).
7. Les résumés sont dérivés après application ; propagation et réparation des ancêtres sont asynchrones et ne conditionnent pas l’ACK.
8. Le handoff copie puis rejoue le miroir ; il gèle et transfère les zones des feuilles vers les parents, avant consolidation régionale ([handoff.md](../../docs/protocol/handoff.md)).
9. `FreezeZones` persiste ensemble fence et snapshot final ; `InstallZones` vérifie zone, séquence et empreinte avant installation ([causal_zone.go](../../internal/storage/causal_zone.go)).
10. L’autorisation exige activité, placement du disque, autorité installée et absence de fence : au plus un écrivain, aucun ACK perdu pendant un handoff, mais aucune reprise automatique après perte du seul disque ([permission.go](../../internal/storage/permission.go), [failure-model.md](../../docs/protocol/failure-model.md)).

**Écart documentaire à traiter :** plusieurs textes parlent encore de Bolt, de `cluster` ou de zonelog « non branché ». Le code de `storage.OpenWithOptions` ouvre désormais uniquement zonelog. Ses séquences physiques restent locales au disque ; elles ne remplacent pas les séquences logiques par zone conservées par `internal/storage`.

## 2. Conceptions possibles

Notations : `Dₚ` est le temps de persistance du primaire ; `Rᵢ + Dᵢ`, l’aller-retour vers une réplique incluant sa persistance. Les estimations excluent files et application.

| Conception | Latence d’écriture | Panne du primaire | Autorité et fences | Complexité |
|---|---|---|---|---|
| **Primaire + journal, majorité 2/3** | Avec append parallèle : environ `max(Dₚ, min(Rᵢ + Dᵢ))`. Un premier prototype séquentiel ajoute environ un aller-retour et une persistance distante. | Élection parmi les survivants, préservation du préfixe engagé, rattrapage avant service. Deux membres disponibles suffisent. | Proche du modèle actuel, mais élections, fences et consommation des grants doivent être ordonnées durablement par le groupe. | Élevée, mais concentrée : journal, consensus, snapshots, configuration. |
| **Chaîne A → B → C** | En attendant la queue : traversée des deux liens, persistances intermédiaires et retour d’ACK ; davantage de dépendances séquentielles. | Reconstitution de la chaîne et détermination de son préfixe sûr. Le choix d’une nouvelle tête exige lui aussi une autorité exclusive. | Une tête unique s’accorde avec l’écrivain unique ; les fences doivent traverser la chaîne avant bascule. | Chemin courant simple ; reprise, changement de chaîne et gestion du retard complexes. |
| **Quorum sans primaire** | Au moins une phase vers une majorité ; ordre, conflits et lectures peuvent imposer plusieurs phases. | Pas de primaire à remplacer, mais les opérations partiellement acceptées restent à résoudre. | Un simple `W=2, R=2` ne suffit pas pour ordonner updates, suppressions et transferts. Accepter plusieurs écrivains changerait le contrat. | Très élevée : ordre par zone, déduplication, réparation et refonte de l’autorité. |

La première option conserve le mieux les responsabilités existantes. **« ACK de deux disques » n’est toutefois pas un protocole d’élection.** Il faut aussi garantir qu’un candidat conserve toutes les entrées engagées et qu’un ancien primaire ne puisse plus engager de nouvelles mutations.

## 3. Architecture recommandée et transitions

### Journal, engagement et application

Employer un consensus de type Raft, de préférence via une bibliothèque éprouvée à sélectionner. Chaque entrée porte l’identité complète de zone `(collection, chemin canonique)`, le terme, un index logique de réplication, le `RequestID`, l’empreinte et la commande.

Distinguer impérativement :

- **Persisté** : entrée durable sur un disque.
- **Engagé** : entrée garantie par le consensus à majorité.
- **Appliqué** : effet installé dans l’état consultable et les reçus.

L’ACK client suit l’engagement et l’application locale. Une entrée persistée seulement sur l’ancien primaire peut être abandonnée après élection ; elle ne doit donc pas avoir été exposée comme une écriture acquise.

Cela impose une adaptation substantielle : aujourd’hui, `ApplyRecordContext` rend l’état visible après le seul commit local. Un wrapper qui réplique **après** cet appel et retarde uniquement l’ACK laisserait apparaître des effets susceptibles de disparaître. Prévoir un journal de propositions séparé, puis une application idempotente des commandes engagées avec leur index appliqué.

Les reçus, tombstones et séquences logiques suivent la réplication. Leur contrat actuel reste exact, sans introduire implicitement la fenêtre de rétention décrite dans certaines anciennes sections zonelog.

### Granularité et autorité

Chaque zone doit avoir trois copies, mais pas nécessairement son propre groupe d’élection. Commencer par des **groupes de placement couvrant plusieurs zones et leur espace encore vide**, avec un journal commun et des positions par zone. Cela amortit heartbeats, fsync et métadonnées ; la taille des groupes reste à mesurer.

La géométrie XOR demeure inchangée. Le placement des copies est distinct du choix du propriétaire logique.

Le point le plus délicat est l’autorité : les grants actuels nomment un vnode, un hôte et une incarnation de disque. Ils ne permettent pas à une réplique survivante de remplacer seule ce disque. Il faut étendre ce modèle pour lier l’autorité au groupe répliqué et à sa configuration, puis autoriser son représentant élu. **Ni `HolderOfZone`, ni le gossip actuel ne suffisent à réaliser cette extension.**

Les décisions de gel, émission, installation et placement doivent être engagées avant publication ou ouverture des écritures. Un primaire remplacé ne peut alors émettre un autre successeur à partir d’une consommation connue seulement de son ancien disque.

### Handoff : rattraper puis promouvoir

Une destination devient d’abord une réplique non votante : snapshot, reçus, contrôle pertinent et rattrapage du journal. Réutiliser les contrôles de progression, scellement et rejeu du staging actuel, sans considérer sa copie comme une permission d’écrire.

Quand elle est prête :

1. Engager la fence et sa barrière logique `F`.
2. Vérifier que la destination possède l’état exact jusqu’à `F`.
3. Engager le transfert d’autorité, puis l’installation correspondante.
4. Promouvoir la destination et rétablir trois copies si nécessaire.

Si la destination change le groupe de placement, la reconfiguration doit préserver l’intersection des majorités, par configuration conjointe ou mécanisme équivalent du consensus retenu. Ajouter une copie ne lui donne pas immédiatement un vote ; retirer l’ancien membre ne précède pas le rattrapage.

Une panne au milieu reprend les décisions engagées, même sans l’ancien primaire. Les fences irréversibles restent fermées ; aucun timeout ne les annule.

### Join et split

Un nouveau nœud héberge d’abord des copies ; il devient ensuite primaire des zones prévues par le plan. Conserver `MigrationSet`, l’ordre enfants avant parents, la découverte des zones nouvelles et la couverture de l’espace absent.

Le split HotTrie reste une partition géométrique des zones du donneur. Il ne découpe pas automatiquement les records d’une zone atomique surchargée. Si les copies nécessaires existent déjà au bon endroit, la transition évite une nouvelle copie complète ; sinon, le préchargement demeure nécessaire.

Le [benchmark P2P](../../docs/benchmarks/p2p-findings-2026-11-15.md) attribue aussi la chute de débit pendant les joins aux bundles de connaissance complets. La réplication ne supprimera donc pas seule le facteur huit : le volume du contrôle et sa diffusion doivent rester bornés.

## 4. Pannes, lectures et invariants

Le contrat cible couvre une panne indépendante de machine ou de disque, avec stockage honnête et majorité joignable. Il ne garantit pas la conservation après destruction de toutes les copies contenant une entrée.

| Injection | Résultat exigé |
|---|---|
| Primaire mort après fsync local, avant réplication | Aucun ACK ; proposition éventuellement abandonnée, aucun effet provisoire exposé. |
| Primaire mort après majorité durable, avant ACK | Entrée conservée après élection ; retry avec le même identifiant sans second effet. |
| Mort immédiatement après ACK | Effet et reçu retrouvés chez le nouveau primaire. |
| Réplique lente ou déconnectée | Écritures possibles avec l’autre ; retard mesuré, rattrapage borné ou nouveau snapshot. |
| Partition 1 contre 2 | Le côté majoritaire peut progresser ; le singleton ne peut plus acquitter de nouvelles opérations. |
| Deux primaires supposés, messages retardés | Aucun conflit engagé au même index ; termes anciens refusés et suffixes non engagés réconciliés. |
| Crash pendant fence, transfert ou reconfiguration | Reprise du même engagement ; aucun grant consommé rouvert. |
| Disque vide réutilisant un nom de vnode | Nouvelle incarnation, admission comme nouvelle copie, aucune autorité héritée. |

Tester aussi les ACK réseau perdus, suppressions suivies de retries, snapshots altérés, parents/enfants d’adresse XOR identique, changements simultanés de placement et pression disque pendant réparation.

**Les lectures demandent une adaptation explicite.** Un ancien primaire isolé peut ignorer sa destitution. Une lecture fraîche doit confirmer l’autorité auprès d’une majorité et attendre l’application du préfixe engagé, par exemple avec un mécanisme de type ReadIndex. Les résumés restent des caches éventuellement cohérents ; leur publication doit distinguer les incarnations du primaire et éviter les doubles contributions après promotion.

Propositions pour [invariants.md](../../docs/protocol/invariants.md) :

- **REP-1 — ACK majoritaire :** tout succès de mutation repose sur une entrée engagée, durable sur au moins deux membres de la configuration compétente.
- **REP-2 — Préfixe conservé :** toute entrée engagée appartient au journal de chaque futur primaire ; aucun conflit engagé n’existe au même index.
- **REP-3 — Époque exclusive :** seule l’autorité élue peut engager ; une ancienne époque ne peut ouvrir une fence ni créer un successeur concurrent.
- **REP-4 — Application exacte :** données, reçus et index appliqué progressent atomiquement ; aucune proposition non engagée n’est exposée comme acquise.
- **REP-5 — Promotion sûre :** toute promotion conserve le préfixe engagé et toute reconfiguration préserve la sûreté des majorités.
- **REP-6 — Transfert répliqué :** fence, barrière, engagement des données et succession d’autorité survivent à la perte du primaire.
- **REP-7 — Copies distinctes :** les trois membres votants occupent trois machines distinctes ; un remplacement vide rejoint comme nouvelle incarnation.

Étendre `AUTH-1/2/3/4`, `HND-2/4`, `REC-1` et `MOVE-2` en conséquence. L’ancien contrat « un disque installé autorise l’ACK » devient insuffisant.

## 5. Étapes livrables et effort

Estimations en semaines de travail d’un ingénieur connaissant le dépôt, incluant intégration et tests.

| Livraison | Test d’acceptation | Effort |
|---|---|---:|
| **Contrat et modèle borné** : groupes, termes, autorité, configuration | Exploration indépendante de trois membres : partitions, élections et transfert ; zéro perte d’ACK ou conflit engagé | 1–2 sem. |
| **Journal et application idempotente**, encore sans bascule automatique | Coupures aux frontières proposition/persistance/engagement/application ; réouverture et rejeu exact | 2–3 sem. |
| **Trois copies, majorité et élection**, topologie fixe | Tuer chaque primaire après un ACK, isoler l’ancien, vérifier records et reçus ; aucun service frais par le singleton | 3–5 sem. |
| **Réparation et remplacement** : snapshots, non-votants, reconfiguration | Retard dépassant la rétention, disque effacé et crash à chaque changement de configuration | 2–3 sem. |
| **Handoff puis split par promotion** | Régressions de handoffs successifs, crash après fence, descendants avant parents, joins sous charge | 2–3 sem. |
| **Qualification EBS/NVMe et exploitation** | Campagnes multi-machines avec suppressions de disques, audits exacts et mesure de reconstruction | 1–2 sem. |

**Total indicatif : 11–18 semaines-ingénieur**, avec forte incertitude sur l’intégration du consensus au graphe d’autorité. L’ordre est volontaire : disponibilité à topologie fixe avant élasticité.

Les écritures bulk constituent un autre chemin de mutation dans `storage/bulk.go` : elles doivent être intégrées au protocole, ou explicitement refusées pour les groupes répliqués jusqu’à leur prise en charge. Aucun chemin ne doit conserver un ACK purement local.

Réutiliser notamment les tests de fences successives, copies scellées, ACK d’installation perdus et messages tardifs. Les tests de coupure zonelog apportent une base locale ; ils ne qualifient pas le consensus. Le benchmark signale encore un dépassement de budget sous `-race` pour les lectures continues pendant joins et partition : conserver cette limite visible.

## 6. Conséquences pour EBS et NVMe

Le changement décisif est le passage de « survie du disque propriétaire » à « survie d’une majorité contenant le journal engagé ».

Avec ACK 2/3, chaque écriture acquittée possède **au moins deux copies durables**, pas nécessairement trois à cet instant. La troisième peut être en retard. Une panne indépendante est tolérée ; deux pertes rapprochées avant réparation peuvent détruire les seules copies d’une écriture.

Le NVMe local devient envisageable lorsque élections, réparation et reconfiguration sont qualifiées. **Le fsync reste nécessaire** sur les copies acquittantes : éphémère signifie reconstructible après perte de machine, pas dispensé de persistance locale.

Prévoir trois machines distinctes, puis un placement selon le domaine de panne visé. Traverser plusieurs zones de disponibilité augmente la protection contre une panne corrélée, avec une latence à mesurer. Une capacité de remplacement et de reconstruction doit être disponible ; la redondance nominale ne suffit pas si le rattrapage ne rejoint jamais le débit entrant.

Le benchmark relie les longues queues d’écriture à `diskBatch.submit`, aux commits et aux attentes de memtable. Le NVMe peut améliorer ces composantes, mais la majorité ajoute réseau et persistance distante : **aucun gain net de p99 n’est encore démontré**. Comparer EBS et NVMe à charge identique, avec les histogrammes `disk.commit`/`disk.wait`, le retard des répliques et le temps de restauration du facteur trois.

Conserver EBS pendant l’introduction de la réplication, puis autoriser NVMe groupe par groupe après qualification. Les sauvegardes restent nécessaires pour les suppressions erronées et corruptions logiques : la réplication propage aussi ces erreurs.