# Timers de pression et qualification du provisioner

## Résultat de cette étape

**Correctif fonctionnel livré :** un niveau de pression déjà soutenu ne se fait
plus oublier par des variations d’un autre signal. Aucun seuil changé ; aucune
modification de l’autorité, du disque ou du format réseau.

**Qualification du transfert utile non terminée :** le vrai provisioner a lancé
un processus sur la réserve et fusionné quatre autres demandes. Le receveur a
ensuite été refusé par le donneur, jamais activé, puis s’est arrêté proprement.
Le banc s’est arrêté en échec ; aucun essai suivant ni AWS n’a été lancé.

## Défaut reproduit à horloge déterministe

La version précédente utilisait deux dates : une pour la montée, une pour le
niveau élevé. Toute nouvelle montée effaçait la date du niveau, même si le CPU
restait constamment au-dessus de 60 %. La montée devait ensuite rester continue
12 s, le niveau 30 s. Des montées brèves répétées pouvaient empêcher les deux.

Trois régressions sont reproduites sur le code initial :

| Situation | Avant | Attendu et vérifié après |
| --- | --- | --- |
| CPU oscillant entre 70 et 80 % pendant 30 s | `held=0s`, attente encore 30 s | niveau CPU qualifié à 30 s |
| CPU élevé depuis 31 s, nouvelle montée au dernier tick | montée fraîche choisie, `held=0s` | niveau déjà qualifié reste éligible |
| CPU chaud 20 s, puis seule la mémoire devient haute | mémoire héritant des 20 s CPU | timer mémoire démarre à zéro |

[Reproduction avant](pressure-placement-20261126/timers-before.log).
Les tests existants gardent leurs assertions ; les échecs ci-dessus concernent
les nouveaux tests de régression exécutés sur l’ancienne logique.

## Implémentation retenue

Une table de quatre prédicats remplace trois fonctions qui choisissaient le
signal puis entretenaient les timers : montée mémoire, montée CPU, niveau
mémoire projeté, niveau CPU. Chaque prédicat entretient sa propre date.

- Une condition fausse remet uniquement sa date à zéro.
- Les quatre dates avancent même si un autre signal est affiché en priorité.
- Un signal qualifié passe avant un signal encore en chauffe. Parmi les
  signaux qualifiés, la priorité reste montée mémoire, montée CPU, niveau
  mémoire, niveau CPU.
- Les lancements réussis et l’arrivée de capacité réinitialisent les dates
  comme auparavant ; les cooldowns et les transferts en cours restent des gates.

La première correction à deux clocks indépendantes ne suffisait pas : des
oscillations mémoire pouvaient encore masquer un CPU constamment élevé. La
version finale suit donc les **quatre conditions**, pas seulement deux familles.
L’état reste constant par vnode (quatre dates), sans parcours du réseau ni I/O.
L’évaluation reste sous le mutex existant du contrôleur, pas celui du writer.

Six tests couvrent ces cas, le refroidissement, une autre ressource fluctuante,
et l’émission réelle d’une demande `host_pressure` par `Tick` à l’échéance CPU.
Les valeurs par défaut restent CPU 60 %, montée 12 s et niveau 30 s.
La qualification du signal n’est pas une borne du délai de spawn : pending,
cooldown, transfert sortant ou absence de plan valide peuvent encore retenir
la demande. Aucun gain de p99 sous charge n’a été mesuré sur cette correction.

## Banc à deux agents : mesuré

Trois conteneurs sur un réseau Docker dédié : contrôle (issuer, provisioner,
loader), donneur (cinq vnodes), réserve (cinq slots vides). Le vrai provisioner
appelle les hooks du dépôt `facts.sh`, `count.sh`, `spawn.sh` ; aucune réponse
`/facts` n’est fabriquée. Les pools de slots, les IP et les répertoires disque
sont distincts. Le CPU et `/proc/stat` restent ceux de la même VM Linux : cela
ne qualifie pas deux machines physiquement indépendantes.

Charge directe : 200 écritures/s, 45 s, 16 collections. À 15 s, cinq demandes
contrôlées `host_pressure` sont adressées au provisioner pour les cinq vnodes
du donneur. Le donneur n’a aucun slot libre ; la réserve est admissible selon
les lectures de ressources réelles.

| Étape | Observation |
| --- | --- |
| Admission des cinq demandes | un spawn, quatre réponses `already_starting`, `coalesced=host_pressure` |
| Placement décidé et appliqué | `kind=new`, cible `http://reserve:22100` |
| Réponse du spawn | environ 339 ms après sa demande |
| Premier receveur observé | t ≈ 15,38 s ; `client_ready=false`, `joining=true` |
| Installation et écritures du receveur | aucune ; maximum `demand.applied=0` |
| Sortie du receveur | code 0 après environ 15 s ; log « join not needed any more » |
| Générateur | 9000 arrivées, 9000 ACK, zéro shed/épuisement définitif |
| Audit des 9000 ACK | **non exécuté**, qualification interrompue avant cette étape |

La coalescence et l’application du placement sont effectivement mesurées.
Le transfert utile, le premier ACK chez le nouveau vnode et sa reprise de charge
**ne sont pas validés** par cet essai. « spawned » veut dire processus lancé,
pas droit installé ou capacité qui soulage un donneur.

## Pourquoi le donneur a refusé

Le scénario contrôlé était incomplet : envoyer une demande à l’orchestrateur ne
met pas le donneur sous pression. Son `Wanted()` ne devenait pas vrai. Dans
`handleHandoff`, un nouveau join `Spawned` qui n’est ni commencé ni activé est
alors refusé avec `ErrNotNeeded`. Le receveur a tenté la reprise en arrière-plan,
puis a quitté proprement. Aucun droit n’a été repris par timeout.

C’est une limite du scénario de qualification, pas une preuve que le protocole
perd les transferts. Le garde du donneur est conservé ; lui faire ignorer sa
propre demande pour rendre le banc vert aurait validé une autre politique.
Le modèle devra représenter séparément demande du contrôleur, lancement du
processus, installation du droit et arrivée du trafic.

Le binaire node de cet essai contenait la correction intermédiaire à deux
clocks. La version finale à quatre clocks a été vérifiée par les suites Go ;
elle n’a pas été redéployée après l’échec. Les empreintes des binaires utilisés
sont conservées, sans présenter le banc comme une qualification live finale.

## Prochaine qualification préparée, non exécutée

`placement.py --trigger demand` configure avant le démarrage un signal de
150 écritures/s par vnode et une charge de 400 écritures/s, dont la moitié sous
un préfixe commun mais sur des zones feuilles distinctes. Ce sont des paramètres
de banc, pas des recommandations de dimensionnement ni des seuils modifiés en
cours de run. Le contrôleur du vnode doit lui-même demander du renfort ; le
provisioner utilise alors son vrai plan et le donneur reste demandeur pendant
l’installation. CPU 60 %, réservations et admission des copies sont conservés.

La réussite exige une capacité réellement lancée sur la réserve, au moins une
écriture appliquée par un receveur, toutes les arrivées acquittées et l’audit
exact final. Le mode contrôlé reste disponible pour diagnostiquer la coalescence,
mais ne fabrique pas la demande du donneur. Le mode demand est préparé et vérifié
en syntaxe seulement : aucune réussite n’est revendiquée à ce stade.

## Vérification et transmission

- `go test -count=1 ./...` : vert, tests lourds opt-in non activés.
- `go test -race -count=1 ./internal/autoscale ./internal/adapter/provisioner ./internal/app/node` : vert.
- `go build ./...` : vert ; gofmt vide.
- Tests Python du lecteur : verts ; syntaxe et CLI du nouveau banc vérifiées.
- Assertions existantes conservées. Aucun essai AWS lancé.

[Résultat structuré](pressure-placement-20261126/result.json),
[traces et compteurs](pressure-placement-20261126/manifest.json),
[mode d’emploi et commande préparée](../../ops/experiment/multiprocess/README.md).
Les stores et clés privées restent dans les outputs locaux ; seuls les logs
et métriques nécessaires sont versionnés. Les conteneurs et le réseau du banc
ont été supprimés. Après l’échec, aucune relance automatique n’a eu lieu.


La [première qualification par demande réelle](demand-qualification-2026-11-26.md)
a maintenant été exécutée : reprise de trafic mesurée, échec d’audit pendant le
dernier transfert, attente et diagnostic du harnais corrigés. Le nouveau passage
avec ces correctifs reste à effectuer.
