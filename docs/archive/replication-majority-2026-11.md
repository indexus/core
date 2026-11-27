# Réplication par journal majoritaire (archivé le 20 novembre 2026)

Contrat proposé le 16 novembre 2026 par [replication.md](../design/replication.md),
remplacé par la réplication convergente
([convergent-replication.md](../design/convergent-replication.md)). Il n'a jamais
qualifié le runtime. Son modèle borné et son journal (`internal/replication/model`,
`internal/replication/journal`, environ 1 800 lignes) n'étaient importés par
aucun code de production et ont été retirés du dépôt le même jour ; ils restent
dans l'historique git, au commit `7379aaf`.

## Contrat archivé

Contrat cible de la [note de conception](../design/replication.md), pas une
qualification du runtime actuel. L'autorité appartient à un groupe de placement
(couvrant aussi les zones absentes), représenté par son primaire élu. Une
configuration stable contient trois votants sur des machines distinctes ; une
copie en préparation ne vote pas. « Durable », « engagé » et « appliqué » sont
trois états distincts. Ces règles étendent AUTH-1/2/3/4, HND-2/4, REC-1 et MOVE-2 ;
elles ne rendent pas le stockage actuel répliqué. Le
[modèle borné](https://github.com/indexus/core/tree/7379aaf/internal/replication/model/README.md) précise ses hypothèses
et les limites de ses certificats atomiques.

### REP-1 — ACK majoritaire

Tout succès de mutation repose sur une entrée engagée, durable sur au moins deux
membres de la configuration compétente, puis appliquée avec son reçu avant
l'ACK client. Le seul fsync local n'autorise aucun succès. Une configuration
conjointe exige les majorités de l'ancienne et de la nouvelle configuration.

Vérification : après chaque événement, le modèle contrôle l'entrée et les témoins
durables de chaque ACK, ainsi que le quorum votant de chaque engagement.
`TestFaults/local-fsync-ACK` doit réfuter l'ACK après le seul fsync local.
L'intégration devra couper les processus entre proposition, persistance,
engagement, application et réponse, sur tous les chemins de mutation dont bulk.

### REP-2 — préfixe engagé conservé

Toute entrée engagée appartient au journal de chaque futur primaire. Le préfixe
engagé ne recule pas ; deux commandes différentes ne peuvent être engagées au
même index logique du groupe, quels que soient les termes et les redémarrages.
Un suffixe seulement persisté peut être remplacé.

Vérification : le modèle compare les journaux élus au préfixe engagé lors de
l'élection, conserve l'historique des engagements et vérifie leur présence durable
après chaque pas. `TestFaults/election-without-log-check` réfute une élection sans
contrôle de fraîcheur du journal. À l'intégration : élection après perte du
primaire, anciens append retardés et reprise d'un suffixe non engagé.

### REP-3 — époque exclusive

Un terme du groupe possède au plus un primaire élu. Seul son représentant élu
peut engager ; une ancienne époque ne peut obtenir un nouvel engagement après
l'élection d'une époque supérieure, rouvrir une fence ou émettre un successeur
concurrent. Un ancien primaire peut ignorer sa destitution, sans pouvoir former
le quorum requis. Reconnaître tardivement un engagement déjà acquis ne constitue
pas un nouvel engagement de l'ancienne époque.

Vérification : le modèle conserve les élections par terme et les certificats
d'engagement, et refuse tout engagement d'un terme destitué. Tester le singleton
isolé face aux deux autres votants, les votes durables après redémarrage et les
messages d'anciennes époques. L'intégration devra aussi refuser le service de
lectures fraîches sans confirmation de majorité (par exemple ReadIndex).

### REP-4 — application exacte

L'application suit le préfixe engagé dans l'ordre. Données, tombstones, reçus et
index appliqué progressent atomiquement et de façon idempotente. Une proposition
non engagée n'est pas exposée comme acquise ; un rejeu ne produit pas un second
effet. Le contrat de rétention des reçus n'est pas modifié implicitement.

Vérification : le modèle vérifie l'index appliqué et l'état de contrôle dérivé du
préfixe ; il ne modélise pas les octets des données ou les reçus. La livraison 2
devra vérifier leur atomicité réelle par réouverture après coupure à chaque
frontière durable et par retry du même RequestID, y compris après suppression.

### REP-5 — promotion sûre

Une copie non votante ne reçoit ni vote ni autorité d'écriture par sa seule
présence. Avant promotion, elle possède exactement le préfixe engagé à travers
la barrière F, les données et le contrôle nécessaires à l'installation. Le
changement de configuration conserve l'intersection des décisions par consensus
conjoint ou un mécanisme équivalent ; le retrait du donneur ne précède pas ce
rattrapage.

Vérification : le modèle exige le préfixe complet chez la destination et un
certificat des deux majorités avant le remplacement d'un votant.
`TestFaults/promotion-before-catch-up` doit réfuter une promotion anticipée.
L'intégration devra vérifier snapshots scellés, reçus, configuration durable,
messages périmés et crash à chaque phase de reconfiguration.

### REP-6 — transfert répliqué

Fence, barrière F, données engagées et succession d'autorité sont des décisions
durables du groupe. La perte d'un primaire ne les efface pas : les survivants
peuvent reprendre le même transfert. Aucun timeout ni redémarrage ne rouvre une
fence consommée. La progression nécessite une majorité disponible et des
livraisons finalement réussies ; la sûreté ne dépend pas de cette équité.

Vérification : après chaque pas le modèle vérifie que la perte de n'importe quel
membre laisse une copie complète du préfixe engagé, contrôle inclus.
`TestTransferSurvivesPrimaryLoss` retire le primaire à chaque frontière durable,
élit un survivant et termine le transfert jusqu'au service par la destination.
L'intégration devra reprendre ces scénarios sur disques réels, sans l'ancien
primaire et sans réinitialiser la configuration ni les fences.

### REP-7 — copies distinctes

Les trois votants d'une configuration stable résident sur trois machines
distinctes, avec des identités de disque/incarnation distinctes. Un disque vide
réutilisant un nom de vnode constitue une nouvelle copie : il rejoint comme
non-votant et n'hérite d'aucune autorité de l'ancien disque.

Vérification : les identités du modèle sont quatre machines/disques distincts
par construction ; il ne simule pas leur effacement. L'admission et le placement
réels devront refuser deux votants sur une même machine et tester le remplacement
par une incarnation vide, sans import d'un ancien grant.
