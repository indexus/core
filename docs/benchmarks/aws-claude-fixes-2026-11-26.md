# AWS : les correctifs améliorent le débit, mais 8k/s reste instable

Run unique `aws-claude-fixes-16k-20261126`, le 26 novembre 2026. Source
`2a63338`, générateur figé à `2362b35`. Comparaison avec
[le run `420061d`](aws-resource-direct-2026-11-26.md).

**Résultat : 16k/s non atteint, 8k/s non validé.** Sur la fenêtre commune
480–515 s, le ratio ACK/offre passe de 44,9 % à 90,1 %. Sur le plateau complet
8k du nouveau run, il vaut 88,0 %, avec une file pleine et des abandons.
Les ressources du run sont supprimées ; aucune relance n'a été effectuée.

La sentinelle a déclenché `node_down https://10.77.0.167:19100 (3 polls)`.
Le nœud concerné s'était arrêté normalement sans avoir reçu de droit ni appliqué
d'écriture. Ce signal ne prouve donc pas un crash du protocole. La dégradation
de débit et de latence, observée avant ce départ, reste bien réelle.

## Intégration et vérifications

`main` (`d63cf15`) était un ancêtre de `claude/next`. Fusion en avance rapide
vers `2a63338`, sans conflit, rebase ni commit de merge, puis push sur
`origin/main`. Les assertions n'ont pas été modifiées. L'essai utilise une
copie isolée de ce commit ; les fichiers non suivis du checkout principal
ont été laissés intacts.

| Contrôle | Résultat |
| --- | --- |
| `gofmt -l .` | Sortie vide |
| `go build ./...` | Succès |
| `cd quic && go build ./...` | Succès |
| `go test -count=1 ./...` | Succès, environ 100 s |
| `go test -race -count=1` sur storage, autoscale, provisioner, app/node, mesh | Succès ; mesh complet 220,4 s |
| `python3 ops/experiment/elastic/test_spawn.py` | Succès |
| `cd ops/experiment/aws && python3 test_deployment.py` | Succès |

Le laboratoire lourd opt-in de 64 vnodes n'a pas été activé. Le portable
possède 16 Gio de RAM et environ 25 Gio libres ; la qualification précédente
y avait déjà relevé du swap. La suite normale ne remplace pas cette validation.
Aucun test multi-machine avec pannes injectées n'a été ajouté à ce run.
Les [journaux de validation](aws-claude-fixes-20261126/validation/results.json)
sont conservés.

### Nature des changements intégrés

- **Fonctionnel :** mesure des arrivées avant service, observation CPU physique,
  coalescing des demandes `host_pressure`, calcul de réserve avec demandeurs
  distincts, démarrage anticipé des jobs compatibles, index durable des sessions
  de staging avec reprise à l'ouverture.
- **Ordonnancement :** budget de copies partagé entre processus d'un même hôte,
  activé par défaut dans l'agent d'expérience.
- **Optimisation :** contrôle de capacité unique par preload, comptage des zones
  depuis l'index, lecture unique d'une ligne inchangée dans la comptabilité.

Ce lot n'est pas une réduction de code : Go de production +1 678 / −244,
soit **+1 434 lignes nettes** ; tests Go +2 341 nettes. Il ajoute des mécanismes
et des preuves de correction. La refonte de l'autorité n'est pas dans ce lot.

## Configuration et instrumentation

Configuration conservée : H3/direct, six `i4i.xlarge` initiaux, quatre vnodes
initiaux, cinq slots par machine, plafond 32 hôtes ; un `c6i.4xlarge` de charge,
4 096 workers. Paliers 2k → 4k → 8k → 16k, rampes et plateaux 120 s,
tail 180 s. Items 500 000, demande 500/s/vnode, capacité logique 4 Gio,
write-cap désactivé, signaux ressources actifs, réserve 15 %, budget 8 USD,
TTL 30 minutes, `CPU_PROFILE=1`. Voir [le plan](aws-claude-fixes-20261126/plan.json).
Les constantes et seuils n'ont pas été ajustés pendant l'essai. Les identités
des vnodes et préfixes de collections ne sont pas identiques entre les runs.
Il s'agit d'une comparaison unique du lot complet, pas d'une estimation statistique
ni de l'effet isolé de chacun des quinze commits.

Le mode `steps` ne lançait pas lui-même la sentinelle. Une observation séparée
utilise le binaire `experiment` de la version testée, avec les règles par défaut
et une liste dynamique des slots. Le premier signal arrête le générateur,
sauvegarde les preuves, puis entraîne le teardown. Le code testé et les templates
du harnais n'ont pas été modifiés pour cela. Le prélèvement de profils ajoute
une surcharge diagnostique ; ses fenêtres sont identifiées ci-dessous.

Le compteur de placement demande une authentification : les premières tentatives
de lecture de l'observateur étaient refusées, puis corrigées avec un jeton de
mesure, sans toucher au service. Son historique conservait les premières décisions.

**Vérification Linux effective :** sur l'hôte initial, les deux vnodes et l'hôte
partagent le namespace de montage `mnt:[4026531832]` et le même device/inode
pour `/proc/stat`, avec `cpu0` à `cpu3`. Le périmètre machine est donc vérifié
sur cet hôte, pas seulement supposé. Noyau relevé : AL2023
`6.18.62-127.179.amzn2023.x86_64`. Cela ne mesure pas encore l'exactitude temporelle
de chaque échantillon CPU. [Capture](aws-claude-fixes-20261126/host0-runtime.txt).

## Mesures de débit et de latence

Les quantiles ci-dessous viennent de l'agrégation des histogrammes des fenêtres,
pas de la moyenne des p99 du dashboard. Les valeurs sont des bornes de buckets
logarithmiques. ACK/offre est un ratio de compteurs de fenêtre : un ACK peut
provenir d'une arrivée précédente, particulièrement lorsqu'une file se draine.

| Fenêtre commune | ACK/offre avant → après | P99 depuis l'envoi avant → après | P99 depuis l'arrivée prévue avant → après |
| --- | ---: | ---: | ---: |
| 2k, 5–120 s | 100 % → 100 % | 16,4 → 8,2 ms | 16,4 → 16,4 ms |
| 4k, 240–360 s | 99,99 % → 99,98 % | 131,1 → 131,1 ms | 131,1 → 131,1 ms |
| 8k, 480–515 s | 44,9 % → 90,1 % | 2 097 → 1 049 ms | 8 389 → 4 194 ms |

À 4k, le p95 passe de 32,8 à 65,5 ms : aucune amélioration uniforme de latence
n'est démontrée. À 8k, les 35 secondes communes représentent 3 594 ACK/s avant
contre 7 205 après. La nouvelle fenêtre complète 480–600 s atteint 7 038 ACK/s,
95 636 abandons du générateur et une file maximale de 16 384. L'ancien run
n'a que 38 échantillons dans ce plateau ; on ne compare pas ses 38 secondes
aux 120 secondes nouvelles comme s'il s'agissait de fenêtres identiques.

La rampe suivante est interrompue vers 634 s, après une consigne de charge
d'environ 10,2k/s. Ni 16k/s ni son plateau ne sont atteints. Le générateur
reste autour de 3–3,5 de ses 16 cœurs, mais ses 4 096 workers et sa file sont
saturés : la latence limite le débit qu'il peut effectivement présenter au réseau.
Ce n'est pas une saturation CPU du loader.

## Placement, demande et copies

### Ce qui a effectivement été observé

- 27 hôtes et un loader créés ; environ 30 vnodes vus dans le dashboard final.
  Le plafond 32 hôtes n'a pas été atteint. Une adresse de nœud apprise ne signifie
  pas qu'il a reçu un droit ou commencé à servir.
- À environ 620 s, cinq hôtes vides et admissibles coexistent avec un hôte à
  96,9 % CPU. Celui-ci porte deux processus dont les dernières pressions CPU
  individuelles sont 46,1 % et 45,0 %. Les fenêtres d'observation ne sont pas
  parfaitement synchronisées ; leur somme n'est pas une identité instantanée.
- Les logs capturés contiennent 84 demandes émises, toutes `reason=writes`.
  Aucune `host_pressure` ni `queue` n'y est observée. Le compteur
  `decided.host_coalesced` est absent, donc aucun coalescing compté dans la
  capture finale. Le mécanisme est testé localement, **pas qualifié par ce run**.
- La capture finale de placement compte 7 décisions locales, 18 sur une autre
  machine, 40 attentes de nouvelle machine et 2 destinations déjà existantes.
  Les décisions réussies ne sont pas toutes des receveurs distincts.
- La réserve produit bien des vagues multiples : une vague atteint cinq boots
  en vol, avec `floor=2,pending=3,ready=0,warming=0`. La limitation ancienne
  à un seul boot n'explique donc plus cette période.
- Le bloc `demand` reste observable : pic par vnode de 337 écritures en cours
  et 564 ms pour l'âge suivi. Au-delà des 256 emplacements suivis, cet âge est
  une borne inférieure. Il ne mesure ni la file du loader ni toute la latence
  avant l'entrée dans le détenteur. Le seuil de queue de deux secondes n'a
  pas déclenché de demande observée.

Voir les [machines](aws-claude-fixes-20261126/machines-at-620.csv), les
[vnodes et leurs refus](aws-claude-fixes-20261126/vnodes-at-620.csv) et
[les compteurs/fenêtres complets](aws-claude-fixes-20261126/analysis.json).
Les compteurs `arrived/applied/acknowledged` sont cumulatifs ; les inventaires
items restent consultatifs, pas un audit global exact.

### Budget de copies : portée réelle

Compteurs cumulés sur les derniers échantillons disponibles : 369 attentes,
0 timeout, 6 490 partages de slot, 0 acquisition immédiate occupée.
Les compteurs sont distribués et leurs derniers instants diffèrent légèrement.

Le code prévoit deux jetons sur quatre CPU. Une source attend au plus dix
secondes avant de continuer sans jeton ; un receveur n'attend pas. La voie de
secours n'a pas produit de timeout observé ici. Ce budget ne couvre pas tous
les coûts de collecte, de résumés ou de maintenance après transfert.

Un regroupement des observations sur deux secondes voit jusqu'à trois jobs
sortants `running`, hors phase `copy-slot`, sur le même hôte. C'est un indicateur
de jobs et non une mesure exacte des copies CPU simultanées : les observations
sont asynchrones et des reprises/collectes ne consomment pas le même budget.
Aucune jauge ne permet d'affirmer un maximum physique exact de copies dans ce run.

### Activation et compteurs

Sur 29 nœuds observés avec `taken>0` et `client_ready=true`, la première pression
autoscale est présente au même sondage que la disponibilité dans le cas médian,
et au plus environ deux secondes après. Le premier inventaire d'items positif
arrive au même sondage dans le cas médian, mais jusqu'à **28 secondes après**.
Les sondages ont une résolution d'environ deux secondes.

Les logs montrent aussi deux démarrages de jobs après la première écriture
appliquée, au plus 168 ms après ; d'autres démarrent avant cette écriture.
Aucun événement horodaté n'expose exactement le premier droit installé : ces
mesures ne sont pas un délai précis installation→tick. Elles montrent que
le retard systématique jusqu'à la fin du join a reculé, sans garantir des
inventaires immédiatement à jour.

## Profils et verrou d'écriture

Deux couples de profils de dix secondes sont conservés. Les snapshots complets
avant/après sont compressés en `start.json.gz` et `end.json.gz` dans leurs dossiers. Ils décrivent des
phases différentes ; on ne compare pas leurs pourcentages comme des gains
normalisés par record ou par octet.

**Vers 376–386 s, pendant les joins à environ 4,5k/s :** sur le receveur
`10.77.8.11:19102`, la somme des tenues successives du writer vaut **9,85 s
sur 10**, dont 9,15 s de validation. La collecte représente 9,35 s de tenue.
Ce n'est pas un gel continu de dix secondes. Le profil attribue 54,7 % du
CPU à `reservationDelta` / `reservedBy` et 58,4 % au chemin `compactStaging` ;
ces pourcentages sont imbriqués et ne s'additionnent pas.
L'ancien run montrait 67,76 s de tenue sur 68 : l'occupation presque continue
reste donc reproductible, même si les fenêtres et volumes diffèrent.

**Vers 531–541 s, pendant le déficit à 8k/s :** le donneur profilé consomme
38,4 % de son CPU dans `summary.boundaryKeys`, notamment `ZoneInventory.Each`
et `boundariesIn`. Le receveur de ce couple consacre encore 15,9 % à
`reservationDelta`. Les deux processus du donneur initial tiennent chacun
le writer environ 6,5 s sur dix. Le goulot varie donc avec la phase et le rôle.

Dans les listings publiés, `stagingSessions` n'apparaît plus parmi les fonctions
coûteuses affichées ; cela ne signifie pas qu'elle n'est plus appelée.
`PlanningTotals` représente environ 0,76 % du second profil receveur, et n'est
pas visible au seuil d'affichage des profils donneurs. `checkCapacity` n'est
pas visible à ce seuil non plus. L'absence d'échantillon affiché ne prouve
pas un coût nul. Le gain de leur retrait ne suffit pas à expliquer tout le run.

Profils et fenêtres du writer : [premier couple](aws-claude-fixes-20261126/profiles/),
[second couple](aws-claude-fixes-20261126/profiles-8k/).

## Ce qui est lu dans le code, et ce qui reste une hypothèse

**Lu dans le code :** `reservationDelta` croise les reçus affectés avec les
sessions de staging. L'index évite le parcours des anciennes lignes, mais
`reservedBy` continue de consulter les copies d'un reçu dans les sessions.
L'ordre de grandeur reste dépendant des reçus affectés × sessions candidates.
La collecte appelle cette comptabilité sous le writer.

**Lu dans le code :** `summary.boundaryKeys` partage un inventaire en cache,
mais le reconstruit si sa révision, sa cellule ou sa collection ne conviennent
plus, ou si sa taille dépasse le budget. `boundariesIn` parcourt l'inventaire.
La création continue de nouvelles zones peut donc invalider le cache.

**Hypothèses à tester séparément :** un index plus direct des réservations par
requête, une collecte avec unités de travail bornées, et un inventaire de
frontières mis à jour sans reconstruction complète pourraient supprimer ces
coûts. Le profil établit les fonctions coûteuses, pas encore la fréquence de
chaque motif d'invalidation ni un correctif sûr. Les tests devront conserver
collision de reçus, capacité atomique, rejeu, écrivain unique et exactitude des
lectures/résumés. Aucun de ces changements n'a été réalisé pendant le run.

Le choix prioritaire de `ownSignal` explique aussi pourquoi les demandes
`writes` peuvent précéder `host_pressure`. Voir du CPU physique dans une
métrique n'implique donc pas qu'une demande ait exercé le coalescing par hôte.

## Arrêt, conservation et limites des garanties

Le nœud `10.77.0.167` est lancé à 17:26:52 UTC. Son log indique ensuite
« donor no longer needs a peer », puis « join not needed any more, leaving »
à 17:27:37 ; l'agent enregistre une sortie normale après 45 s. Les 22
échantillons de métriques disponibles ont tous `joining=true`,
`client_ready=false`, zéro `taken` et zéro écriture appliquée.
La machine a encore beaucoup de RAM et de disque disponibles ; aucun OOM
n'a été trouvé dans la capture noyau. [État capturé](aws-claude-fixes-20261126/failed-host-state.txt).

La sentinelle traite un endpoint précédemment joignable puis disparu comme
un nœud en panne. Elle ne distingue pas ici l'abandon normal d'un join jamais
activé. Il faut qualifier ce cas avant une prochaine campagne ; ignorer toutes
les disparitions de nœuds non prêts masquerait d'autres défauts.

Le premier signal a arrêté la charge. Le ledger final, recapturé après drainage
avec une taille stable, contient **2 829 479 lignes ACK**, dont 2 827 818 écritures
principales et 1 661 sondes. Le résumé du loader rapporte 221 940 abandons,
4 125 requêtes épuisées et 12 283 travaux encore en file à l'arrêt.
Le ledger initial comprimé pendant le drainage pouvait être incomplet ; c'est
`acked-final.jsonl.gz` qui sert de référence.

Les audits de fin n'ont pas tourné, puisque le run est interrompu au premier
signal. **Aucune validation AWS complète de relecture de tous les ACK, de
l'écrivain unique, des totaux ou du rejeu n'est revendiquée.** Les suites locales
passent ; elles ne remplacent pas ces audits. Le ledger conserve les écritures à vérifier ; il ne prouve pas leur présence
sur les disques. Aucune image des disques NVMe des machines terminées n'a été
archivée pour ce run, donc leur relecture ne peut plus être effectuée.

## Nettoyage et suite

Les 28 instances sont terminées. Absence du VPC, du groupe de sécurité, du
launch template, du rôle/profil IAM et du bucket vérifiée indépendamment :
[preuve de nettoyage](aws-claude-fixes-20261126/cleanup-verification.json).
Estimation EC2 lancement→transition de terminaison : **1,36 USD** ; borne
jusqu'à la vérification : **1,80 USD**, hors EBS, S3, transfert, API et taxes.
Ce n'est pas une facture. [Calcul et horodatages](aws-claude-fixes-20261126/cost-estimate.json).

Les preuves brutes, le dashboard et le ledger sont conservés localement dans
`outputs/aws-claude-fixes-2a63338-20261126/` du dossier de travail Codex.
Le [manifeste](aws-claude-fixes-20261126/evidence-manifest.json) en donne les SHA-256.
Le script d'analyse attend ce dossier brut à côté de lui ; sa copie dans cette
annexe documente la reproduction et n'est pas un benchmark autonome.

Prochaine étape proposée, sans relance automatique : reproduire le coût
`reçus × sessions` pendant la collecte et les reconstructions d'inventaire de
résumés, puis mesurer leurs correctifs isolément. Qualifier aussi la sortie
normale d'un join abandonné dans la sentinelle. Une nouvelle campagne nécessite
ces vérifications et un scénario dont les critères de réussite restent inchangés.
