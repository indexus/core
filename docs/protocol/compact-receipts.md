# Reçus compacts et rejeu exact

**Statut : actuel, en partie obsolète.** Il cite encore Bolt. Voir [stockage-et-verrous.md](../architecture/stockage-et-verrous.md).

Un reçu d'écriture conserve maintenant `(RequestID, Seq, Collection, Location,
Digest)`. `Digest` est l'empreinte du record complet reçu, y compris son ID, ses
métriques et son marqueur de suppression. Le reçu ne contient plus une deuxième
copie de cette valeur. Le journal de mutations utilisé pour le rattrapage garde
les mutations complètes.

## Pourquoi le résultat d'un rejeu reste le même

Une écriture fournit toujours son identifiant et son record. Dans la transaction
Bolt, le détenteur vérifie d'abord son droit d'écriture, puis recherche le reçu.
Si l'identité de requête, la zone et l'empreinte correspondent, il reconstruit la
mutation retournée avec la séquence conservée et le record fourni. Il n'applique
aucune mutation, ne fait pas avancer la séquence et ne met pas à jour les agrégats.
Un même identifiant avec un contenu différent reste un conflit.

Cette équivalence suppose, comme les autres engagements du protocole, l'absence
de collision de l'empreinte SHA-256. Elle fonctionne après écrasement et après
suppression : le résultat ancien vient de la requête rejouée, pas de la valeur
courante. Sans fournir la requête originale, ce reçu seul ne permet plus de
récupérer son ancienne valeur. Cela correspond à l'API actuelle, qui écrit un
record complet. Une future opération calculant un résultat distinct de sa
requête devrait conserver ce résultat si elle promet de le restituer au rejeu.

## Transfert et validation

Snapshots et registres locaux/globaux contiennent les mêmes reçus compacts. Le
scellement vérifie leur identité de zone, les identifiants uniques, les empreintes
non nulles et les séquences non nulles au plus égales à la barrière. L'ancienne
valeur absente ne peut plus être rehachée depuis le reçu seul : l'engagement
`ZoneTransfer.DataDigest` de la source lie ensemble les records actuels et tous
les reçus. L'installation vérifie cet engagement avant d'ouvrir l'écriture.

Le rattrapage conserve ses mutations complètes. Pour une mutation recouvrant le
snapshot initial, le destinataire compare l'identifiant, la séquence, la zone et
l'empreinte de son record au reçu. Les trous et les modifications de contenu
restent refusés. Les compteurs de capacité incluent toujours les reçus dans les
deux index et la réservation de leur installation.

## Ce qui n'est pas supprimé

Tous les identifiants de requêtes restent conservés. Aucun TTL, bail, acquittement
client ou effacement automatique n'est ajouté. La taille d'un reçu dépend des
identifiants et du chemin de zone, mais plus de la taille des métriques de
l'ancienne valeur. Le nombre de reçus et le journal local ne sont donc pas bornés
par cette modification.

Pour N requêtes, avec des valeurs de taille moyenne P et des identifiants/chemins
de taille moyenne I, les reçus passent d'un coût O(N × (P + I)) à O(N × I), plus
une empreinte et une séquence par requête. Un snapshot garde séparément les
valeurs actuelles. Le journal local de rattrapage conserve son coût en payloads.

La purge future peut s'inspirer des acquittements et sessions de
[RIFL](https://sigops.org/s/conferences/sosp/2015/current/2015-Monterey/printable/126-lee.pdf),
mais elle exige de refuser les anciennes requêtes après leur oubli. Le présent
changement ne prétend pas implémenter RIFL. Les échanges de connaissance par
version restent conservés ; la diffusion de véritables deltas par preuve et la
purge de l'historique d'autorité restent des travaux distincts.

Le format des reçus persistés et transférés change. Les anciennes bases locales
ne sont pas prises en charge ; aucune migration n'est fournie.

## Qualification

- Rejeu exact des trois versions d'un même objet, dont sa suppression, avant et
  après transfert puis réouverture ; `applied=false`, état courant inchangé.
- Contenu différent pour un même identifiant refusé ; recouvrement du snapshot
  par le miroir accepté seulement pour la mutation originale.
- Première copie altérée, y compris l'empreinte d'un reçu, refusée à l'installation
  par la preuve source ; scellement conservé après réouverture.
- Oracle de campagne conservé : il calcule indépendamment chaque empreinte depuis
  les mutations acquittées et compare l'identité complète de tous les reçus,
  ainsi que les données, agrégats et séquences.
