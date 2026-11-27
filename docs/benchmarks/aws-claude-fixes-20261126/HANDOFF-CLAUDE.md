# Reprise après le run unique de 2a63338

Les quinze commits sont fusionnés en avance rapide et poussés sur main.
Tous les contrôles demandés passent, dont mesh complet sous -race (220 s).
Pas de modification d'assertions. Le labo lourd 64 vnodes n'a pas été activé
sur le portable ; aucun essai multi-machine avec pannes n'est validé ici.

Lire d'abord `../aws-claude-fixes-2026-11-26.md` et `analysis.json`.
Le source réellement exécuté sur AWS est 2a63338 ; les commits documentaires
ultérieurs ne doivent pas être présentés comme un nouveau source mesuré.

## Résultat

- Même générateur 2362b35 et mêmes paramètres que aws-resource-direct-16k-20261126.
- Fenêtre commune 8k, 480–515 s : 44,9 % ACK/offre avant, 90,1 % après.
- Nouveau plateau 8k complet : 88,0 %, file pleine, 95 636 abandons.
- P99 intentionnel 8k commun : 8,39 s avant, 4,19 s après.
- 27 hôtes, un loader ; plafond 32 non atteint. À t620, cinq hôtes vides
  admissibles alors que l'hôte initial est à 96,9 % CPU.
- Signal sentinelle node_down vers t634 ; charge arrêtée, toutes les ressources
  supprimées. Pas de relance. Pas d'audit final ; aucune preuve AWS globale
  de relecture des ACK ou d'écrivain unique revendiquée.

## P0 : sentinelle et abandon normal d'un join

Le nœud 10.77.0.167 quitte avec exit 0 après « donor no longer needs a peer ».
Ses 22 métriques montrent joining=true, client_ready=false, taken=0, applied=0.
Ce n'est pas le crash d'un écrivain en service. Voir failed-host-state.txt.
Qualifier un état explicite de join abandonné et la façon dont l'agent le
rapporte. Ne pas ignorer toutes les disparitions de nœuds non prêts : cela
cacherait de vrais défauts de démarrage. Ne pas changer les assertions pour
faire passer ce cas ; ajouter un scénario déterministe distinct.

## P1 : collecte et comptabilité de réservations

À t376–386, receveur 10.77.8.11:19102 : writer tenu 9,85 s/10, validation
9,15 s, collecte 9,35 s. reservationDelta/reservedBy = 54,7 % du CPU du profil.
L'index a retiré la découverte des anciennes lignes, mais les reçus affectés
sont encore croisés avec les sessions candidates. Le produit reste sous writer.

Commencer par un banc variant indépendamment reçus affectés, sessions actives,
sessions historiques collectées et collisions/rejeux. Compter les lectures
ponctuelles, pas seulement les scans. Étudier une réservation canonique par
requête / référence aux sessions ; conserver exactitude atomique de capacité,
refus de reçus divergents, rollback, crash et réouverture. Mesurer aussi les
petites optimisations de chemins avant de changer le format durable.

## P2 : coût d'export des frontières de résumés

À t531–541, le donneur 10.77.6.216:19101 consacre 38,4 % de son CPU à
summary.boundaryKeys ; ZoneInventory.Each/boundariesIn sont sur ce chemin.
Le cache dépend de la révision du catalogue et de la collection, et ne garde
pas un inventaire trop gros. Mesurer hit/miss par motif, nombre de zones
visitées et coût par page avant de choisir une structure incrémentale.
Ne pas attribuer ce coût à PlanningTotals : celui-ci n'est plus dominant
sur ce donneur. Une collecte et un export coûteux peuvent coexister.

## P3 : ce que le run n'a pas exercé

/proc/stat est bien commun à l'hôte et aux deux vnodes vérifiés.
Mais les logs capturés montrent 84 demandes reason=writes, zéro host_pressure
et zéro queue ; le compteur host_coalesced est absent. ownSignal peut gagner
avant la pression physique : le coalescing par hôte n'est pas qualifié sur AWS.
Le budget de copies compte 369 attentes, 6490 partages, zéro timeout/busy.
Ne pas incriminer sa voie de secours sans mesure ; la collecte et les résumés
ne sont pas tous couverts par ce budget.

Proposer les changements séparément : fonctionnel / ordonnancement / optimisation.
Un essai avant-après local par coût, puis une nouvelle campagne seulement quand
elle est demandée ; ne pas relancer automatiquement cette configuration.
