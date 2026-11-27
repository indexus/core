# Qualification des correctifs de voisinage et de spawn — 26 novembre 2026

Les deux blocages isolés dans la [qualification précédente](qualification-9f0f09c-aws-2026-11-26.md)
ont des correctifs et des tests de régression. Sur AWS, le cluster croît maintenant
jusqu’à six vnodes sur trois machines, limite explicite de ce banc. Les 25 999
écritures acquittées sont retrouvées. Cela qualifie le câblage et les cas observés,
pas la capacité à 16 000 écritures/s ni une borne universelle de gel.

## Ce qui change et pourquoi

Source testée : `ec4b0af14412dd3d19084727e8717f09be4d19a0`, sur `3a53979`.
Le loader reste figé à `2362b35`. Aucun changement de `claude/next`.

1. Les contacts de voisinage conservent le schéma de leur adresse, depuis le
   bootstrap jusqu’à la découverte et aux redirections. Un nœud TLS construit
   des contacts HTTPS et emploie le transport partagé à sélection H3 paresseuse.
   Le délai du client voisinage passe de 2 à 5 secondes pour permettre les deux
   secondes de tentative QUIC puis le repli TCP/TLS. Un test avec UDP muet
   reproduit l’échec de l’ancien délai. Ce délai est un plafond d’échec du probe,
   pas une attente imposée à une écriture ni un accord global.
2. Une réservation de spawn désigne un point de placement et son propriétaire
   avant la demande. Elle cesse d’être en attente quand un autre membre actif
   dessert ce point. L’identité réellement créée est volontairement fraîche et
   voisine : attendre l’identité exacte planifiée conservait la demande pour
   toujours. Un changement d’adresse du même vnode ne suffit pas ; un membre
   retiré non plus. Les droits installés restent seuls responsables des ACK.
3. Le filtrage des réservations utilise une seule vue de placement préparée par
   merge, plutôt que réexporter la membership pour chaque réservation. La liste
   vide sort immédiatement. La réservation est dans un fichier dédié ; aucun
   format de migration de disque ancien n’est conservé.

Ce sont des corrections de comportement, pas un nettoyage à comportement constant.
Le diff ajoute environ 85 lignes Go de production ; les tests et explications
représentent l’essentiel de l’augmentation totale. Ce coût explicite couvre la
conservation du transport et la réservation durable ; aucune baisse globale du
nombre de lignes n’est revendiquée dans cette passe.

## Vérifications avant AWS

`go build ./...`, `go vet ./...`, `go test -p 1 ./...`, tests du module QUIC,
`-race` sur peer, p2p, node, storage, indexus, mesh et QUIC : tous passent.
Les tests Python de déploiement et de spawn passent également.

Les nouveaux tests reproduisent sur l’ancienne source le probe HTTPS erroné,
la réservation non satisfaite par une identité voisine et sa persistance après
réouverture. Les tests couvrent aussi retrait, rejeu, déplacement d’endpoint,
et conservation des permissions. Le test UDP muet échoue avec le délai de
2 secondes et passe avec celui de 5 secondes, sans dégradation en HTTP clair.

## Labo 64 nœuds : comparaison sur la même machine AWS

Campagne `qual-fixed-1126-132654`, eu-west-3. Une c6i.4xlarge, passages
séquentiels, GOMAXPROCS=8, HTTP loopback, 64 services et leurs stores,
8 192 zones initiales, 32 Mio par store, fsync et maintenance actifs,
40 écritures/s au total pendant les joins. Les latences ci-dessous sont des
quantiles des traces individuelles. Les allocations sont cumulées, préparation
et audit compris, et ne représentent pas le RSS.

| Source / passage | ACK | p50 ms | p95 ms | p99 ms | Dernier join s | Allocations Gio |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 9f0f09c / référence | 1600 | 9,91 | 26,12 | 53,82 | 38,76 | 16,35 |
| ec4b0af / répétition 1 | 1640 | 9,95 | 24,72 | 65,22 | 41,00 | 17,34 |
| ec4b0af / répétition 2 | 1618 | 9,98 | 26,73 | 65,39 | 40,47 | 16,71 |
| ec4b0af / avec fautes | 1619 | 9,92 | 24,59 | 64,24 | 40,50 | 16,66 |

Les quatre audits passent : aucun ACK manquant ou dupliqué, aucun doublon
chez les détenteurs écrivables, aucun gel restant. Le passage avec fautes
inclut partition, réponse perdue après installation durable et retard d’I/O.
Pas de swap ; CPU hôte inactif au moins 68,6 %. Les ACK varient avec la durée
exacte du scénario. La population de joins se termine un peu plus tard avec
le candidat ; la référence n’a qu’un passage dans cette campagne.

**Le p99 augmente d’environ 21 % face à cette référence.** On ne conclut donc
pas à l’absence de régression de latence ni à un gain de performance. Ce labo
utilise des contacts simulés en HTTP et n’exerce pas le correctif TLS réel.
Il faut une série plus longue et des fenêtres de joins alignées pour attribuer
cet écart ; ces données seules ne prouvent pas sa cause.

## Placement réel, lectures et disponibilité pendant les joins

Deux i4i.xlarge initiaux, deux slots par machine, troisième machine autorisée,
TTL 30 minutes. Nœuds H3 avec profils activés, clients de charge en TLS/TCP,
compaction désactivée pour isoler ce câblage. Seuil items=2 500 ; déclenchement
par les nœuds, sans appel manuel de scale. Charge initiale 400/s pendant 20 s,
puis 200/s pendant 90 s. Cette charge modérée vérifie la fonction, pas la
capacité maximale. Des lecteurs tournent pendant les joins, avec contrôle
des totaux profonds et relecture séparée des clés déjà acquittées.

- 7 999 puis 18 000 écritures demandées et acquittées, zéro shed ou épuisement.
- 25 999 clés contrôlées à l’audit, zéro manquante ou erreur de lecture ; les
  totaux finaux des 16 collections correspondent aux ACK.
- 1 545 lectures ponctuelles et 607 contrôles de total observés pendant la
  charge : zéro manque, erreur ou déficit. 1 093 relectures supplémentaires de
  clés initialement acquittées passent aussi.
- Les cinq nouveaux vnodes sont observés disponibles avant la fin de leur
  join. Le premier a un compteur HTTP ACK positif environ 0,44 s après sa
  première observation, tandis que son join termine environ 3,69 s après.
  Ce compteur seul ne distingue pas ACK appliqué localement et relais.
- Audit de propriété final : zéro double détenteur observé et zéro transfert
  encore en mouvement. Ce relevé n’est pas une photographie atomique de
  l’autorité sur tous les disques ; il ne prouve pas l’invariant à tout instant.
- Bornes supérieures des buckets de latence client, charge initiale : p50/p95
  4,096 ms, p99 8,192 ms ; charge live : 4,096 / 4,096 / 16,384 ms. Le p99
  intentionnel est 16,384 ms dans les deux phases. La dernière seconde partielle
  n’est pas nécessairement dans l’histogramme ; l’audit porte sur tous les ACK.
- Cinq profils CPU et piles de receveurs récupérés (HTTP 200), ainsi que leurs
  facts avant/après. Les six probes HTTPS finaux répondent 200.

Les décisions appliquées comprennent deux placements locaux, deux placements
sur machine nouvelle pour le vnode et un placement distant sur machine
existante. Le provisionneur observe sept décisions `new_wait` pendant l’admission
et le démarrage ; le cas de repli après plafond n’est pas qualifié comme succès.
Le CPU maximal relevé par nœud est 38,1 % de l’hôte ; mémoire et disque restent
largement disponibles. Les instantanés ne sont pas une borne de saturation
entre deux prélèvements.

## Pourquoi le cluster reste déséquilibré

À l’audit les six vnodes détiennent respectivement 1 531, 13 182, 1 581, 1 592,
6 441 et 1 672 items. Les logs du receveur le plus chargé montrent des demandes
répétées refusées avec `no free slot: capacity=3 reason=operator accounted=3`.
La limite des trois machines a donc effectivement arrêté l’expansion. Aucune
conclusion sur l’équilibrage sans cette limite n’est tirée de ce banc.
L’audit ne montre pas de transfert bloqué à sa clôture ; cela n’exclut pas les
cas historiques à forte charge. Le prochain essai doit augmenter le plafond
et observer séparément attente de machine, première installation, premier ACK
local, apprentissage de route directe et clôture du join.

## Arrêt, coût et éléments conservés

Les instances, le réseau, le bucket, les identités IAM et le modèle de lancement
sont supprimés après collecte. Le relevé JSON joint contient la vérification
indépendante et l’estimation EC2 ; les fichiers bruts, profils, piles, logs,
commandes et empreintes des binaires restent dans le dossier local de campagne.
Estimation EC2 haute de cette campagne : 0.473 USD ; cumul des deux qualifications : 0.959 USD. Ce coût n’est pas une facture et exclut stockage, S3 et transfert.

Les tests ne qualifient ni la compaction activée, ni un réseau réel de 64
machines, ni 16k/s, ni une borne maximale de gel. La campagne 16k/s reste une
étape distincte à dimensionner et financer après cette qualification.

[Relevé machine et résultats compacts](qualification-ec4b0af-aws-2026-11-26.json).
