# Distribution des records et de la charge — run AWS du 26 novembre 2026

Source testée : `420061d`, loader `2362b35`. Run : `aws-resource-direct-16k-20261126`.
Analyse en lecture seule des fichiers `captured/steps-1/{metrics,machines,load}.jsonl`, de `captured/live.jsonl` et du ledger `captured/operator/acked.jsonl.gz`.
Calcul reproductible : `python3 analyse_distribution.py`. Sorties : `distribution.json`, `distribution-nodes.csv`, `distribution-machines.csv`, `distribution-summary.txt`.

## Conclusion

La charge est équilibrée à 2k/s et devient sensiblement concentrée pendant les joins. Ce n'est pas un hotspot volontaire du générateur. Mais les zéros d'items des nouveaux vnodes sont trompeurs : ils peuvent déjà posséder des données et appliquer des écritures sans avoir publié leur premier comptage. La concentration **des écritures appliquées**, indépendante de ces compteurs d'items, est bien réelle.

À t515, deux anciennes machines hébergeant quatre des vingt vnodes portent environ **34,3 % des items affichés et 32,8 % des nouvelles applications**, alors que leur part souhaitée de l'espace XOR est **18,75 %**. Les transferts ne sont pas encore convergés. En même temps cinq machines restent vides. La quantité de capacité créée n'est donc pas la capacité effectivement utile au bon endroit et au bon instant.

Le CPU chaud est aussi du travail de déplacement : après arrêt des applications, un receveur reste à 86,6 % CPU hôte / 88,7 % CPU des processus de nœuds. RAM et espace disque sont très loin de leurs seuils. Cela n'établit pas l'absence de contention ou de coût algorithmique dans le protocole de transfert.

## Sémantique et précautions

- `storage.items` = `Node.Items()` = dernier `PlanningTotals`. Celui-ci parcourt les zones **writable** par le vnode, en pages pouvant observer des versions différentes. Le comptage est advisory et publié en arrière-plan. Ce n'est ni le total de collection racine ni un compte physique de toutes les copies/staging.
- `scanZonePages → eachInventoryZoneFrom → writableIn` exclut le staging et les zones cédées au moment de chaque page. Des compteurs de nœuds pris à des moments différents pendant un transfert ne constituent pas un snapshot global.
- `Count` et les tâches du worker ne commencent qu'après la première tentative de join. Un vnode partiellement actif peut donc afficher `items=0, zones=0` pendant plus longtemps que le délai habituel de rafraîchissement, tout en écrivant déjà. À t430, neuf vnodes en join affichent zéro item mais ont de 0,8 à 27,5 Mio d'`owned_bytes` et de 0,45 à 127 applications/s. À t515 trois zéros cachent encore 6,8 à 18,9 Mio et 18 à 64 applications/s.
- `storage.owned_bytes` = compteur d'occupation `usage.Owned` : lignes, live, historique et reçus d'opérations. Ce n'est pas le seul payload logique. Le staging et le résiduel ont leurs propres compteurs internes, non exposés ici.
- Les débits ci-dessous sont recalculés : `Δwrites.applied / Δuptime_seconds`, sur environ vingt secondes par processus. Le dashboard original utilise le temps des points d'agrégation (5 s), alors que les prélèvements des nœuds sont espacés de 4 ou 6 s : cela crée une oscillation artificielle de ±20 %. Ce défaut de mesure ne change pas le déficit d'ACK client.
- CPU des processus = `Δprocess.cpu_seconds / Δuptime_seconds`, normalisé aux quatre cœurs. Le CPU hôte est le fait d'agent le plus récent ; les fenêtres diffèrent légèrement et ne doivent pas être soustraites comme des mesures simultanées exactes.

## Évolution

| Instant (s) | Vnodes observés | Hôtes avec faits frais | Hôtes vides | Applications/s corrigées | Max/moy applications par vnode | Items affichés (somme advisory) | Max/moy items par vnode | CPU hôte max |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 105, 2k stable | 8 | 6 | 1 | 2 003 | 1,02 | 176 492 | 1,09 | 13,8 % |
| 330, 4k | 12 | 8 | 0 | 4 010 | 1,52 | 779 702 | 1,68 | 100 % |
| 430, rampe ~6,2k | 20 | 18 | 2 | 3 988 | 2,33 | 1 170 365 | 2,62 | 96,2 % |
| 515, fin de charge 8k | 20 | 21 | 5 | 3 589 | 1,98 | 1 602 861 | 2,05 | 95,4 % |
| 735, après arrêt | 20 | 21 | 5 | 0 | — | 1 800 036 | 1,73 | 86,6 % |

Les applications corrigées sont une moyenne d'environ 20 s ; les ACK du graphique sont une moyenne d'environ 5 s et incluent un autre point d'entrée. Ainsi à t515 le point client est à 3 317 ACK/s, et la moyenne des applications à 3 589/s : ce n'est pas une conservation violée.

À t735 il reste sept opérations ouvertes selon le point voisin t733, et onze offres de réception admissibles. Les applications sont nulles depuis bien plus de la fenêtre de débit : le CPU restant ne peut pas être attribué au trafic client courant.

## Hôtes à t515

`items` reste advisory ; les octets sont les octets Owned. « entrants » est `facts.receiving` (vnode encore en join), pas un nombre de flux simultanés. « sortants » est le nombre d'opérations ouvertes observées sur les nœuds, à confirmer par leur champ `running` pour la concurrence exacte.

| Machine | CPU hôte | Items affichés | Owned Mio | Applications/s corrigées | Vnodes | Vnodes en join / entrants | Opérations sortantes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 10.77.1.145 | 95,4 % | 205 263 | 161,3 | 449,8 | 2 | 1 | 0 |
| 10.77.3.17 | 85,6 % | 262 491 | 224,4 | 555,8 | 2 | 0 | 2 |
| 10.77.7.69 | 76,6 % | 287 236 | 234,9 | 620,7 | 2 | 0 | 1 |
| 10.77.14.157 | 54,6 % | 67 795 | 41,4 | 147,8 | 1 | 1 | 0 |
| 10.77.7.182 | 47,8 % | 56 272 | 35,5 | 121,0 | 1 | 1 | 0 |
| 10.77.11.141 | 37,8 % | 42 326 | 28,2 | 93,8 | 1 | 1 | 0 |
| 10.77.6.29 | 37,1 % | 46 669 | 28,1 | 100,7 | 1 | 1 | 0 |
| 10.77.5.149 | 35,1 % | 28 946 | 17,8 | 62,4 | 1 | 1 | 0 |
| 10.77.5.123 | 31,6 % | 249 858 | 209,4 | 531,7 | 2 | 0 | 1 |
| 10.77.0.177 | 31,2 % | 101 600 | 68,6 | 225,8 | 1 | 0 | 0 |
| 10.77.6.186 | 30,4 % | 81 347 | 56,0 | 180,3 | 1 | 1 | 0 |
| 10.77.0.56 | 27,7 % | 34 280 | 21,6 | 71,8 | 1 | 1 | 0 |
| 10.77.10.28 | 26,3 % | 138 778 | 113,4 | 291,5 | 1 | 0 | 1 |
| 10.77.15.164 | 20,2 % | **0 (non mesuré)** | 18,9 | 64,2 | 1 | 1 | 0 |
| 10.77.12.229 | 12,2 % | **0 (non mesuré)** | 16,1 | 53,1 | 1 | 1 | 0 |
| 10.77.1.96 | 11,0 % | **0 (non mesuré)** | 6,8 | 18,4 | 1 | 1 | 0 |
| Cinq hôtes vides | 0–0,8 % | 0 | 0 | 0 | 0 | 0 | 0 |

Les cinq vides sont 10.77.14.202, 10.77.12.133, 10.77.2.124, 10.77.14.180 et 10.77.4.185. Il n'y a aucun hôte portant cinq vnodes : vingt vnodes se répartissent sur seize hôtes, douze avec un vnode et quatre avec deux.

## Le générateur impose-t-il une concentration ?

Le scénario `steps` appelle le générateur uniforme : collection tirée parmi 256, location de seize caractères indépendants dans l'alphabet à 64 symboles. Il n'y a pas de hot-prefix injecté.

Le ledger **des écritures acquittées**, pas l'intégralité des arrivées offertes, contient exactement **1 715 230 lignes** :

- 1 713 679 écritures principales, ce qui égale `load-summary.json.acked` ;
- 1 551 sondes dans `G0Thgp0000000001`, IDs `pN`, confirmées par le code du probe. Elles ne sont pas incluses dans le compteur ACK du générateur principal.

Par premier caractère de **location** (pas par ID d'item), les 64 effectifs vont de 26 451 à 27 126, moyenne 26 800,5 ; coefficient de variation **0,61 %**. Les 256 collections principales vont de 6 492 à 7 040, CV **1,13 %**. Ces statistiques portent sur la population qui a obtenu un ACK ; elles ne reconstruisent pas les clés rejetées ou restées en file. Le code et les ACK ne montrent pas de hotspot intentionnel expliquant un facteur deux ou trois.

## La géométrie XOR n'implique pas 1/N exactement

Les vingt adresses à t515/t735 définissent des cellules souhaitées de 1/16, 1/32 et 1/64 de l'espace. Le maximum vaut 6,25 %, la moyenne 5 %, donc le max/moy théorique n'est que **1,25**. Les extrêmes sont bien dans un rapport quatre, mais ce rapport quatre ne justifie pas un max/moy du débit de **2,33** pendant la rampe.

Ces cellules sont définies par des **masques de bits discriminants**, parfois avec des trous ; la probabilité n'est pas simplement 2^(-position du dernier bit). Le script compte les niveaux de branchement, vérifie que les parts somment à un et les vérifie aussi par énumération des 2^15 préfixes qui distinguent ici tous les membres.

En replaçant les 1 715 230 ACK selon cette membership finale, les cellules 1/16 recevraient ~106 800–107 800 records, celles 1/32 ~53 100–53 800, et les 1/64 ~26 700–26 900. Ce sont des **destinations souhaitées**, pas les détenteurs installés pendant le join. Exemples à t735 : Rwn… affiche 155 997 items contre 106 817 souhaités ; 52G… 68 422 contre 26 686 ; 5IG… 65 356 contre 107 571. Certaines cellules ont achevé leur mouvement (dai… 107 406 observés et souhaités, 53G… 53 381, 52O… 26 933), d'autres restent à converger ou ont un compteur ancien.

On ne peut pas déduire de l'écart des compteurs une perte ou une duplication : la somme advisory t735 est 1 800 036, supérieure au ledger de 84 806 ; les nouvelles applications cumulées de tous les détenteurs valent 1 715 352, soit seulement 122 de plus que le ledger ACK. Ces 122 peuvent notamment correspondre à des opérations appliquées dont la réponse n'a pas été reçue à l'arrêt. Aucun audit complet n'a certifié cette explication ni l'exactitude des totaux.

## Concurrence de transfert et saturation réelle

À t428 sur 10.77.3.17, les deux vnodes ont **trois opérations sortantes `running=true`** :

- sLL… → 10.77.15.164, phase install ;
- sLL… → 10.77.7.182, phase freeze ;
- tLL… → 10.77.11.141, phase install.

Donc même un vnode peut donner à deux destinations en même temps. À ce moment le CPU hôte est 96,2 %, et le CPU des deux processus recalculé sur ~20 s est 95,5 %. Ce n'est pas seulement une anomalie du compteur hôte. À t735, sans application client, 10.77.1.145 reste à 86,6 % / 88,7 % CPU des processus et 10.77.6.186 à 63,2 % / 62,6 % : le travail de transfert/arrière-plan mérite une qualification séparée du coût des écritures.

La RAM utilisée maximale observée dans ces instantanés est inférieure à 4 %, et le disque occupé à 0,76 %. Cela exclut un manque de RAM/espace comme explication de ce run ; cela ne mesure ni l'utilisation du disque en IOPS ni les latences de fsync.

## Pistes et critères avant le prochain essai

1. Mesurer et borner le travail **par machine** : copies entrantes et sortantes, coût CPU et bytes de snapshots, et concurrence de tous les vnodes. Une limite par receveur ne protège pas les donneurs co-résidents.
2. Compter les premières données et publications de pression dès l'activation partielle. Pour le dashboard, `items non mesurés` doit être distinct de zéro. Publier un âge de mesure et les compteurs Owned/Staging/Residual plutôt que surcharger « items total ».
3. Distinguer le droit installé de la cellule XOR souhaitée ; afficher par machine le volume restant à déplacer et le trafic déjà basculé. Faire converger les mouvements existants avant de multiplier les nouveaux receveurs si le donneur est le goulot.
4. Valider le placement sur CPU partagé et coût de déplacement, pas sur la disponibilité de cinq slots. Préserver l'ordre sûr des autorités ; cette analyse ne démontre pas qu'une optimisation du transfert suffit ni que son protocole est exempt de défaut.
5. Corriger la mesure des applications/s avec les timestamps propres aux prélèvements ; garder les trois totaux distincts : ACK client principal, applications (sondes et issues inconnues comprises), inventaire advisory.
