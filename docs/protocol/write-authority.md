# Adresse et droit d'écriture

> **Historique : protocole prédécesseur à preuves/checkpoints.** Le remplacement
> par [droits locaux et tickets](local-permissions.md) est branché dans le worktree
> le 25 novembre 2026 ; ses [preuves séparées](local-permissions-verification.md)
> ne reprennent pas automatiquement les résultats de cette page. Les mentions
> « actuel » ci-dessous appartiennent à cet instantané historique.

**Statut : actuel.**

**Extension sur la branche authority-frontier (24 novembre 2026).**
La [frontière et les checkpoints](authority-frontier.md) remplacent l'ascendance
ancienne par un checkpoint accepté, sans installer de droit par gossip.
Les nouvelles opérations restent validées depuis cette frontière. Une fusion
locale de deux droits frères consomme ses **deux** entrées atomiquement.
Le contrat lié précise la confiance, les rejets de rejeu et les étapes qui
restent distinctes de cette première compaction.

Le droit d'écriture est l'état du protocole qui permet à un stockage d'acquitter
une mutation. C'est le verrou de bascule entre l'ancien et le nouveau détenteur
d'une zone. Il est vérifié dans la transaction qui enregistre la mutation.

## Ce que donne l'adresse

L'ID géométrique et la vue des vnodes actifs déterminent la destination XOR :

```text
owner_cible(z, vue) = argmin XOR(z.ID, vnode.ID)
```

La vue est une connaissance locale propagée entre pairs. Pendant un ajout ou un
déplacement, deux nœuds peuvent calculer des destinations différentes. Un vnode
peut également devenir routable après l'acquisition d'une première zone, avant
la fin du transfert de toute sa cellule. Le routage suit alors l'autorité durable
pour les autres zones. Une adresse ne prouve pas que les octets et les reçus des
écritures précédentes sont installés sur le disque destinataire.

L'ID seul n'identifie pas non plus toute zone : des parents/enfants ou collections
peuvent avoir le même ID géométrique. Les bascules par zone utilisent l'identité
complète `(collection, chemin)`.

## Exemple d'une bascule A → B

Pour une zone que B doit acquérir, les vues peuvent diverger pendant la copie :

| Étape | A peut acquitter | B peut acquitter |
| --- | --- | --- |
| Copie initiale et rattrapage | Oui | Non |
| A a persisté le gel, installation finale en cours | Non | Non |
| B a installé les données, reçus et droit correspondant | Non | Oui |

Le gel de A précède l'ouverture de B. A garde sa fermeture après redémarrage.
Une copie en attente chez B et des informations de routage reçues par gossip ne
lui permettent pas d'acquitter une écriture. Un disque vide reprenant un nom de
vnode ne récupère pas le droit du disque précédent.

La garantie est **au plus un détenteur pouvant acquitter**, avec une interruption
possible entre fermeture et installation. Les lectures de cache autorisées
peuvent traverser cette interruption ; elles ne modifient pas le droit d'écriture.

## Règle locale minimale

La décision d'acquitter exige simultanément :

1. Le vnode local est actif et son placement correspond au disque courant.
2. Les preuves désignent ce disque comme détenteur de la zone.
3. Aucune fermeture locale applicable n'interdit cette mutation.
4. L'installation correspondante est terminée : jeton terminal natif, ou
   installation de cette zone pour l'opération exacte de transfert.

Un jeton terminal n'a, par définition, aucun successeur. Un ancien jeton encore
présent dans l'historique d'installation ne peut donc pas rouvrir un ancêtre
consommé. Les certificats partitionnent le domaine précédent sans chevauchement
ni trou, y compris l'espace sans données. Des transferts indépendants utilisent
leurs transactions locales ; aucune validation globale n'est ajoutée.

Ces garanties supposent des participants respectant le protocole et la
conservation de l'état disque acquitté. La convergence de la connaissance suppose
la livraison éventuelle des messages et la stabilisation des changements ; aucun
délai maximal n'est garanti.

## La preuve engage les données transférées

Chaque `ZoneTransfer` contient désormais `DataDigest`, l'empreinte canonique des
records et du registre des reçus dans la copie gelée à la source. L'installation
vérifie ensemble l'identité complète de la zone, sa séquence et cette empreinte.
Un même numéro de séquence ne suffit pas à identifier le contenu.

Une copie finale scellée chez le destinataire ne peut plus être remplacée avant
installation : seul un rejeu équivalent de la même source est accepté. Une copie
finale ne peut pas non plus faire reculer une copie chaude déjà rattrapée. Les
refus annulent toute la transaction, y compris les autres zones du lot et les
preuves de contrôle. Après installation, les anciens messages de staging restent
des opérations sans effet.

Ces vérifications sont locales et ne créent aucun validateur global. Elles
refusent un transfert incohérent ; elles ne réparent pas automatiquement une
première copie corrompue déjà scellée et ne constituent pas une protection contre
un pair malveillant forgeant à la fois données et preuves.

Les reçus ne dupliquent plus les anciennes valeurs : voir
[reçus compacts et rejeu exact](compact-receipts.md).

Le format des preuves change : les anciens transferts par zone sans empreinte
sont refusés. Aucune migration des bases locales historiques n'est fournie.

## Une seule représentation validée

Le cache conserve désormais une seule `TopologyReplica`, qui possède déjà le
graphe d'autorité validé. Routage, inventaires et contrôle d'écriture consultent
ce graphe via des méthodes en lecture seule. Les valeurs `Grant` et les listes de
jetons retournées sont détachées ; aucune map de preuves n'est exposée.

Les décisions de fermeture et d'installation restent dans l'état transactionnel
local. Le cache est associé aux octets exacts du contrôle lu par la transaction ;
le partage du graphe ne partage jamais une ancienne décision d'autoriser une
écriture. Les contrôles redondants de successeur après résolution d'un jeton
terminal ont été retirés ; le contrôle de consommation lors de la préparation
explicite d'un transfert reste nécessaire et conservé.

## Vérification

- `TestSameXORAddressDoesNotBypassFreezeOrDiskIdentity` : même route après gel,
  après redémarrage et sur un disque neuf ; aucun contournement de la fermeture.
- `TestReplicaProofQueriesReturnOnlyDetachedTerminalGrants` : oracle XOR
  indépendant sur 256 adresses, jetons terminaux sans successeur et absence
  d'alias mutable dans les résultats.
- `TestSealedStageRejectsNonEquivalentReplaysBeforeInstall` : données, reçus,
  séquence et source inchangés après scellement, y compris après réouverture.
- `TestFirstFrozenCopyMustMatchSourceProofBytes` : une première copie altérée
  ne peut pas être installée avec la preuve des octets originaux.
- `TestZoneTransferRequiresImmutableDataCommitment` : empreinte obligatoire et
  conflit de contenu refusé sans modifier la connaissance.
- Les tests existants de rollback, transferts interrompus, reçus, anciens hôtes,
  parents/enfants de même ID et writer unique sont conservés.

Voir aussi [topologie causale](causal-topology.md) et
[modes de lecture](read-modes.md).
