# Reprise pour Claude — élasticité et répartition

Base commune : `907ae4e`. La politique d'autorité reste celle des permissions
locales ; aucune validation globale ni chaîne historique n'est réintroduite.
Voir [le rapport et les limites](../elastic-anticipation-2026-11-26.md).

## Corrections Codex

- `a7e4395`, `a5d80ff`, `7c7e0c0` : les curseurs d'inventaire suivent les données
  de la collection et la couverture/délégation interrogée. La maintenance,
  le staging et une autre collection ne les invalident plus. Une mutation
  réelle les invalide toujours. Le marqueur de collection rejoint le commit
  existant, bulk compris : pas de fsync supplémentaire. La dernière version
  utilise la séquence locale du cut et évite de relire un compteur ou de
  reconstruire l'autorité d'une ligne que la transaction vient de modifier.
- `2e19dca` : une copie sortante non gelée n'outrepasse plus le budget machine
  après dix secondes. Elle diffère la même décision durable. L'attente est
  visible sans compter comme une nouvelle tentative de handoff. Concurrence
  par défaut : quart des cœurs, minimum un. Sous pression CPU, la source cède
  du temps **entre les lots installés et acquittés**, jamais dans le gel.
  Reprise gelée et clôture scellée prioritaires. Métriques `copy_paced_lots/ms`.
- `d138319` : chaleur récente, bornée à 4 096 entrées, décroissance exponentielle
  sur dix secondes et deux niveaux de préfixes. Un relais ne chauffe plus
  artificiellement le nœud d'entrée. La chaleur de préfixe classe les choix ;
  elle ne devient ni autorité ni capacité atomique. Un petit déplacement chaud
  peut soulager un processus, sans exiger la moitié des items. La validation
  géométrique et le refus d'une chaîne de donneurs vides restent actifs.
- `91981f0` : le scénario AWS steps respectait toujours quatre vnodes. Il
  respecte maintenant `STEP_VNODES`, par défaut le premier `SIZES`, et vérifie
  les slots initiaux avant de provisionner. Préparation : quatre hôtes,
  cinq slots chacun, seize vnodes initiaux ; aucun nouveau run AWS lancé.
- `c3b26f7` : annulation des requêtes et backoffs d'audit, comptage des lectures
  réellement terminées, rapport interrompu explicitement incomplet et en
  échec. Plus de seconde relecture complète si chaque ACK a déjà été vérifié ;
  diagnostic échantillonné à 32 workers. Un désaccord de total reste un échec.
- `2bbfa2b` : attente d'une ligne JSON complète dans le faux AWS du test de
  spawn ; ses assertions sont inchangées.
- `532d017` : le résumé de placement est prélevé après l'audit. Avant cela,
  `spawned=2` pouvait côtoyer un placement final à trois applications.

## Reprise du nouveau `claude/next`

`052ca47` est repris intégralement par cherry-pick (`b1a69b7`). Le problème
fondamental est confirmé dans le code : un receveur sur la machine source
ne lui retire pas de CPU. Le placement local passe donc en dernier recours
lorsque rien ne peut démarrer ailleurs, sous admission inchangée.

`43a546a` complète cette direction :

1. Réutiliser un slot sur une **autre** machine avant une réserve vide, si les
   comptages des deux participants sont connus et si le critère de soulagement
   et les ressources passent. Cela permet quatre/cinq vnodes par machine,
   sans transformer chaque split en nouvelle machine.
2. Ne pas publier un zéro jamais compté comme une charge connue. Les comptages
   âgés de plus de trente secondes expirent, même si les observations HTTP
   cessent ; la cadence de recomptage et l'expiration partagent la constante.
3. Un processus vivant sans identité rend la charge de son hôte inconnue.
4. La connaissance d'un hôte sans rapport n'est pas requise pour choisir
   entre deux hôtes connus. Le test des items reste un indice local à la paire.
5. Le comparateur n'alterne pas entre ordre par items et ordre par CPU selon
   les deux éléments comparés : cela pouvait former un cycle à trois hôtes.
   Chaque ensemble de candidats utilise un seul ordre. Test sur six permutations.

`target_items + vnode_items < donor_items` est démontré pour le modèle uniforme
à moitiés. Les petits déplacements chauds ne sont pas nécessairement des
moitiés : on conserve ce critère comme heuristique conservatrice, pas comme
preuve de soulagement CPU. Aucun seuil physique n'est relevé.

## Ce qu'il faut conserver et mesurer ensuite

- L'ACK de lot précède toute temporisation de copie. Ne pas déplacer l'attente
  dans `cutoverLot`, ni attendre un jeton pour terminer un gel existant.
- La collecte physique de données déjà cédées ne doit pas invalider un curseur
  dont la réponse visible est inchangée. Une vraie mutation doit le faire.
- Les comptages d'items restent consultatifs : le banc a observé leur retard
  dans les deux sens. Afficher leur âge ; utiliser l'inventaire exact pour
  l'audit, sans imposer un parcours complet à chaque décision de scale.
- Le budget de copies n'est pas un plafond dur sur réceptions et reprises ;
  la régulation CPU n'est pas un quota OS à 60 %.
- L'estimation receveur de 1,75 cœur reste conservatrice et fixe. Ne pas la
  réduire pour faire passer des petites instances avant de mesurer le nouveau
  chemin réel. Les tests locaux ne qualifient pas une rampe H3 à 16k/s.
- Le replay fluide de placement passe (pic modélisé 47,5 %, zéro split local).
  Ce sont les hypothèses du modèle, pas une mesure AWS.

Les résultats, sources exactes, scénarios à 64 vnodes et contrôles entre
processus sont détaillés dans le rapport et ses preuves JSON. Les clés d'issuer
et les disques bruts restent dans les résultats locaux, hors dépôt.
