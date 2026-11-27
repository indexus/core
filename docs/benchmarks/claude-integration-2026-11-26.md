# Intégration Claude / Codex — 26 novembre 2026

Les vingt commits de `claude/next`, de `d5b609a` à `0980a46`, sont réunis
avec les correctifs de voisinage et de réservation de `main` (`5e5890d`).
La branche Claude est conservée pour son travail en cours.

## Ce qui est repris

- Suppression des fonctions sans appelant et déplacement du client de test
  `mesh.Writer` dans les fichiers de test.
- Fonctions de périphérie découpées et `mesh/service.go` réparti par rôle.
- Contrat des requêtes décrit dans une table, avec ses tests.
- Snapshot autoscale propriétaire de sa map de pression, pour éviter l'encodage
  concurrent d'une map modifiée par un tick.
- Copie de staging certifiée transportée depuis la préparation jusqu'au commit
  d'installation, après vérification de ses versions : pas de seconde lecture
  des données sous le verrou d'écriture pour ce chemin.
- Preuves et ACK regroupés par lot. Les états et les reprises par zone restent
  présents. La lecture des anciens formats reste dans ce changement : ce n'est
  pas une suppression complète des chemins historiques.
- Recherche des feuilles par marquage de préfixes, à la place du balayage des
  paires de zones. Le test d'équivalence couvre notamment les collections,
  doublons et racines.

Les deux conflits de fusion sont résolus explicitement :

1. `newNode` et `bindServers` utilisent tous deux la fabrique de contacts TLS
   lorsque le nœud est TLS. L'extraction des fonctions ne réintroduit pas HTTP.
2. Les fonctions extraites d'activation sont reprises ; les réservations de
   spawn gardent l'implémentation dédiée de `main`, fondée sur le changement de
   propriétaire du point et non sur l'identité exacte attendue.

Ce lot ne doit pas être présenté comme un nettoyage à comportement constant
ni une baisse globale du nombre de lignes. Les changements de staging et de
format local sont fonctionnels et les compteurs de mesure ont un coût non
qualifié isolément. Le format réseau des lots n'est pas intentionnellement modifié.

## Politique de ressources ajoutée ensuite

La demande utilisateur fixe une cible de 60 % pour garder de la marge.
L’agent mesure aussi le CPU total Linux, en plus de la somme des vnodes, pour
inclure le système, l’agent et le provisionneur. Le CPU hôte est testé sur des
compteurs synthétiques, y compris attente, vol de CPU, invités et remise à zéro. Le CPU
par défaut du contrôleur et l'admission CPU estimée du receveur sont alignés
sur 60 %. Le disque et la mémoire doivent conserver au moins 40 % disponibles
pour l'admission lorsque leurs mesures sont connues. Le signal de disque par
défaut du contrôleur est aligné sur ce plancher.

Un plafond de machines ne permet plus de choisir une destination refusée par
les contrôles de ressources. Le hook reçoit aussi les refus explicites, au lieu
de les remplacer par un slot libre. Si l'inventaire change, il conserve la cible
choisie plutôt que de choisir un autre hôte sans contrôle de ressources.
Les hooks dépourvus de faits gardent leur mode par slots ; il ne constitue pas
une qualification de la cible de 60 %. Ces mécanismes ne sont pas des quotas
matériels instantanés.

`RESOURCE_SIGNALS=true` est désormais transmis jusqu'au JSON des nœuds, même
avec un seuil d'items ou d'écritures. Le prochain dimensionnement prévoit un
vnode par machine, 500k items comme seuil de volume et 500 écritures/s comme
seuil de débit : voir [le plan](aws-16k-sizing-2026-11-26.md).

## Vérifications et limites

La fusion des dix-neuf premiers commits passe la suite normale et `-race`
sur autoscale, provisioner, meshhttp, peer, p2p, node, mesh, storage, summary,
wire, zonelog et indexus. Le correctif de feuilles passe `-race` sur protocol,
autoscale et simulation. La suite normale complète est ensuite relancée sur
l'arbre final, politique de ressources comprise. Les chemins de ressources
passent aussi `-race` sur autoscale, provisioner et experiment ; les tests
Python de spawn et de déploiement et le module QUIC passent. Les logs sont
conservés dans le dossier local de campagne.

Le modèle TLA+ décrit par Claude n'est pas dans le dépôt : son exécution n'est
pas reproduite ici. La documentation de Claude précise que le cas à deux
opérations avec pannes n'est pas exploré jusqu'au bout. Les tests d'intégration
ne remplacent pas cette preuve et ne constituent pas une qualification AWS.

Le [run AWS interrompu](aws-16k-direct-fixed-2026-11-26.md) teste `5e5890d`,
avant ces nouveaux changements. Il reste à qualifier sur AWS les nouveaux
chemins de lot, le correctif du planificateur et la politique de ressources.
Les points signalés par Claude sur le verrou du provisionneur pendant AWS,
le bootstrap sans corps et le classement des erreurs dans les métriques ne
sont pas corrigés par cette fusion ; ils restent à revoir séparément.
