# Lectures, transitions et point d'écriture — 2 novembre 2026

**Statut : historique.** Revue datée du 2 novembre 2026. Le fonctionnement actuel est décrit dans [chemin-ecriture.md](../architecture/chemin-ecriture.md).

Cette passe complète la [qualification des agrégations](aggregation-verification.md)
avec des lectures pendant les transitions et des sondes concurrentes des disques.
Elle a reproduit et corrigé trois défauts de lecture, sans ajouter de validateur
global ni modifier les règles transactionnelles d'autorisation d'écriture.

## Invariants et contrats

Pour une zone identifiée par `(collection, chemin)`, l'invariant de sûreté est :

`nombre de disques autorisés à appliquer une nouvelle mutation à l'instant t ≤ 1`.

Le protocole autorise **zéro writer** entre le gel durable de la source et
l'installation/activation de la destination, ou pendant une panne. Exiger
exactement un writer disponible à chaque instant dépasse le contrat actuel.
Plusieurs entrées HTTP peuvent renvoyer un ACK en relayant vers le même applier ;
ce ne sont pas plusieurs détenteurs autoritaires.

- Une lecture rafraîchie contourne le résumé. Aux coupes contrôlées, une réussite
  doit restituer les records et tombstones attendus, sans doublon, avec comptes
  et métriques exacts. Un shard gelé, non installé ou inaccessible est une
  indisponibilité explicite, jamais une réponse vide présentée comme complète.
- Une lecture ordinaire peut exposer un résumé ancien pendant le transfert.
  Elle doit converger après stabilisation et livraison des messages.
- Ces tests ne promettent pas un snapshot atomique global entre plusieurs régions
  indépendantes sous mutations continues, ni des résumés linéarisables.
- Un ancien détenteur ou une ancienne vue XOR ne confère aucun nouveau droit
  d'écrire : seuls les droits et fences durables locaux décident dans la transaction.

L'argument local de sûreté repose sur des historiques compatibles, des disques
à identité unique et des transactions persistantes : la copie n'accorde aucun
droit ; le gel ferme la source avant émission ; l'installation et l'activation
ouvrent la destination ensuite. La consommation persiste après redémarrage,
confirmation et GC. Une ancienne installation ne contourne pas un nouveau gel.
Deux départs d'un même token ne peuvent produire deux successeurs valides.
Les tests ci-dessous vérifient des scénarios bornés ; ils ne constituent pas une
preuve exhaustive de tous les historiques ou ordonnancements distribués.

## Corrections de lecture

1. **Résumé abandonné sur un ancien point d'entrée.** Après handoff, une nouvelle
   écriture était correctement acquittée par le nouveau détenteur, mais l'ancien
   ingress pouvait continuer à servir son résumé local indéfiniment. Le test
   échouait après guérison du réseau. `meshGet` résout désormais le placement
   courant et ne sert ce résumé local que sur le propriétaire physique actuel.
   Une lecture profonde distante utilise le chemin durable existant.
2. **Deux réponses d'agrégation pour un même vnode sur deux disques.** Le disque
   de remplacement possède le même nom logique ; il répondait aussi avant d'avoir
   le placement actif. `GetAggregates` vérifie le placement durable une fois par
   batch, puis la vue courante. Le cache de propriété des résumés suit également
   les changements de placement, même si le digest de membership n'a pas changé.
   Ce cache reste dérivé et n'autorise jamais une écriture.
3. **Redirection physique perdue.** Un MOVE conserve le nom du vnode. Le contact
   de lecture désigne maintenant le nouvel endpoint et l'enveloppe `/sets`
   conserve cette redirection même lorsque son nom logique est identique au nôtre.

Les contre-exemples sont persistés dans `post-handoff-read-before.log`,
`shadow-aggregate-before.log` et `physical-redirect-before.log` sous
`/Users/remihenriot/Documents/Cockpit/outputs/indexus-read-write-phases-2026-11-02`.

## Coupes durables : 78 contrôles

`TestAuthorityAndCompleteReadsAtEveryDurableHandoffCut` parcourt 17 transitions,
avec contrôle avant et après réouverture des deux disques. Il couvre deux zones
parent/enfant d'adresse identique, puis répète le parcours avec l'adresse zéro :
68 contrôles au total, sur Bolt synchronisé.

| État | Writer possible pour la zone concernée |
| --- | --- |
| Initial, capture et preload | source seule |
| Gel, certification, preuve émise non installée | aucun |
| Installation sans activation | aucun |
| Installation et activation | destination seule |
| Nouveau grant régional connu mais pas monté | aucun |
| Grant monté, confirmation et collecte | destination seule |

Les deux disques reçoivent simultanément de nouvelles opérations à chaque coupe.
Le test refuse plus d'un applier, vérifie le détenteur attendu et refuse un
inventaire déclaré complet lorsqu'il manque un record. Une zone sœur peut rester
writable alors que son descendant est gelé, même si leurs adresses sont égales.

`TestRetiredHolderWithStaleTopologyNeverRegainsWriterAfterTwoHandoffs` ajoute dix
coupes avec trois détenteurs et deux migrations successives. Le premier garde
volontairement une vue où il est le seul membre, puis reçoit un ancien delta :
il ne retrouve jamais le droit d'écrire.

## Réseau et API publiques

`TestReadsAndSingleWriterThroughHandoffAndLostMessages` arrête le transport avant
et après preload, mirror, stage-zone, install-zone, stage et install. Il couvre :

- un handoff normal ;
- une coupure avant installation ;
- un ACK d'installation perdu ;
- deux joins concurrents sur trois nœuds ;
- un déplacement physique d'un même vnode entre deux disques.

Chaque scénario insère et supprime pendant la copie, puis ajoute un nouveau
record à travers l'ancien ingress après le transfert. Il vérifie les snapshots
complets avec payloads et tombstones, `Get(refresh=true)` à quatre profondeurs,
l'absence de double application, la convergence de `Get` après partition et
la réponse d'agrégation depuis un seul propriétaire physique après stabilisation.
Le MOVE vérifie aussi la redirection de lecture superficielle et celle de `/sets`.
Les zones imbriquées partagent leur adresse et le même ID de record.

Pour les handoffs concurrents, les sondes utilisent un même ID d'opération sur
chaque disque : si le détenteur change entre deux appels, le second doit être un
replay. Cela évite de confondre deux writers simultanés avec deux détenteurs
successifs légitimes. Les coupes durables séparées contrôlent aussi l'éligibilité
avec des IDs d'opération distincts par disque.

## Lectures et insertions réellement concurrentes

`TestContinuousReadsKeepEarlierACKsDuringJoinsAndPartition` lance trois lecteurs,
2000 insertions, deux joins et une partition de 25 ms. Une quatrième boucle sonde
les trois disques avec une même nouvelle opération par tour.

Pour chaque lecture rafraîchie réussie, le compte ne doit pas être inférieur au
nombre d'ACK précédant le démarrage de la lecture. La borne supérieure inclut
l'unique écriture éventuellement committée mais pas encore acquittée. La somme
des index positifs est contrôlée avec les bornes correspondantes. Après guérison,
les lectures ordinaires doivent converger vers le compte 2000 et la somme
1999000 ; le snapshot final vérifie individuellement chaque payload.

Ce test utilise `NoSync` pour solliciter la concurrence. La durabilité et les
réouvertures sont qualifiées séparément par les coupes Bolt synchronisées.

## Limites de disponibilité et de coût

Le chemin durable distant collecte actuellement des inventaires complets.
Il peut donc devenir indisponible à cause d'un autre transfert en cours et coûter
cher sur un grand réseau. Des lectures bornées à une région/préfixe seraient une
optimisation distincte ; elles ne doivent ni réutiliser un vieux résumé comme
réponse exacte, ni introduire un validateur global.

La convergence suppose que les messages et les réparations finissent par passer.
Une partition permanente ne promet ni lecture fraîche partout ni writer disponible.
Les sondes et les audits sont des observateurs de test, hors du chemin de décision
P2P de production.

## Qualification finale conservée

Toutes les validations finales ci-dessous ont passé avec le détecteur de races :

| Vérification | Résultat du dernier rejeu |
| --- | --- |
| Coupes durables et ancien détenteur à vue périmée | 78 contrôles, aucun writer supplémentaire |
| Scénarios de transport et MOVE | 303 coupes sur 5 scénarios |
| Lectures aux coupes | 1040 réussites exactes, 2500 indisponibilités explicites |
| Charge continue | 2000 ACK, 616 lectures rafraîchies réussies |
| Indisponibilités pendant la charge continue | 485, sans faux résultat vide |
| Sondes concurrentes des disques pendant la charge | 1034, aucune double application |
| Après reprise | tous les IDs et payloads présents, compte 2000 et somme 1999000 |
| Lectures ordinaires après reprise | convergées sur tous les points d'entrée |
| Redirection physique et enveloppe `/sets` | conformes après MOVE |

Les nombres de coupes des joins concurrents dépendent de l'ordonnancement : ils
indiquent le parcours exercé, pas une énumération exhaustive. Les indisponibilités
sont des résultats attendus aux barrières contrôlées, pas des écritures perdues.

Les suites `indexus`, `cluster`, `cmd/node` et `benchmark` ont passé `-race`
(hors les deux anciens tests de débit extrême). Après la dernière correction
d'enveloppe, les suites `indexus`, `cluster` et `cmd/node` ont repassé `-race`.
La version finale des nouveaux tests, y compris les assertions de bornes des
métriques et de payloads, figure dans `final-phase-validation.log`.
`go vet ./...` et `git diff --check` ne signalent aucun problème.

Le dernier test continu utilise deux zones de même adresse. Des histoires plus
longues, davantage de régions indépendantes et la vérification formelle exhaustive
restent des extensions possibles ; aucune réussite de test ne démontre à elle
seule « tous les états possibles ».
