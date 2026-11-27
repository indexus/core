# Réutiliser les machines sous 30 % avant d’étoffer la réserve chaude

Sur la flotte observée du run `aws-place-100-direct-1127`, le plancher calculé sur le seul excédent de charge au-dessus de 30 % demande **1,45 offre-minute**, contre **20,08** avec le plancher actuel. Il conserve la demande explicite des vnodes. Ce rejeu modifie la décision de garder une réserve chaude. Le budget CPU temporaire d’une copie continue de déterminer quand un placement peut démarrer.

## Trois planchers, à demande identique

Pour chaque enquête, `C` est la somme des vCPU des hôtes avec leurs faits CPU, RAM et disque, et `U` la somme de leurs cœurs occupés (`cores × cpu_pct / 100`, même convention que le provisioner). Un receveur coûte un budget provisionnel de 1,75 cœur. Les trois règles donnent un plancher de zéro, `ceil(0,15 × U / 1,75)` et `ceil(0,15 × max(0, U − 0,30 × C) / 1,75)`. À chaque décision, la demande opérationnelle reste `max(0, pending + plancher − ready − warming)`.

| 44 enquêtes complètes, 657,7 secondes couvertes | Plancher zéro | 15 % de tout le CPU occupé | 15 % de l’excédent au-dessus de 30 % |
| --- | ---: | ---: | ---: |
| Enquêtes où le plancher est positif | 0 | 44 | 6 |
| Plancher intégré sur la durée (offres-minutes) | 0 | 20,08 | 1,45 |
| Sur huit enquêtes du palier 16k, 117,3 s (offres-minutes) | 0 | 6,57 | 0,45 |

La moyenne de CPU de la flotte pondérée par les cœurs atteint au maximum 38,0 %. Un hôte isolé peut être saturé alors que cette moyenne est basse. La demande du donneur et ses délais de transfert doivent donc continuer à déclencher le provisionnement. La réserve se rajoute à cette demande.

## Capacité présente avec un slot libre

Comptage des machines déjà occupées, sous 30 % de CPU, avec au moins un slot libre, aucun join entrant et au moins 40 % de RAM et de disque libres. Le test matériel ajoute 1,75 cœur au CPU et accepte un receveur si le projeté reste au plus à 60 %. Sur les i4i.xlarge à quatre vCPU du run, le seuil de démarrage immédiat est donc 16,25 %.

| État de la machine existante | Enquêtes avec au moins une | Hôtes-minutes observées | Dont au palier 16k |
| --- | ---: | ---: | ---: |
| En dessous de 30 %, slot libre et autres filtres satisfaits | 43/44 | 167,42 | 36,31 |
| Copie immédiatement admissible | 43/44 | 133,47 | 16,87 |
| Au-dessus de 16,25 % et sous 30 %, démarrage différé | 28/44 | 33,95 | 19,44 |

Une machine qui dispose de trois slots ne compte qu’une fois. En parallèle, 73,56 hôtes-minutes ont été observées sur des machines avec zéro processus. Une machine vide peut héberger une réservation ou se trouver en chauffe. Pour choisir une destination distante il faut encore vérifier le soulagement du donneur, la validité des observations et les réservations déjà attribuées. L’intérêt du créneau 16,25–30 % serait de prévoir un placement différé avec réévaluation de l’admission au lancement.

## Rejeu sur décisions enregistrées

Le collecteur a conservé 235 décisions distinctes munies des termes de réserve pendant la charge. Le calcul rapproche chaque décision de la dernière enquête **antérieure**, à effectif identique, distante d’au plus 16 s. Cela couvre 205 décisions. Pour 172 d’entre elles, le plancher fixe recalculé est exactement celui de la décision. Les deux séries et le provisioner échantillonnent le CPU à des instants différents.

| Mode (205 décisions comparables) | Décisions avec déficit d’offres positif | Somme des déficits réévalués |
| --- | ---: | ---: |
| Plancher zéro | 93 | 1 244 |
| 15 % réellement journalisé | 106 | 1 445 |
| 15 % du surplus de CPU | 93 | 1 286 |

Sur les 172 cas où la reconstruction du plancher fixe est exacte, les nombres de décisions à déficit positif sont respectivement 92, 102 et 92. Chaque scénario conserve exactement les termes `pending`, `ready` et `warming` journalisés. L’excédent peut accroître une demande réelle, sans jamais descendre sous le déficit calculé à plancher zéro. Les 205 décisions peuvent recompter les mêmes demandeurs pendant les secondes de démarrage EC2. La convention de concurrence du run reste celle de ce binaire, antérieure à la nouvelle borne par copies source.

Le rejeu tient constants la flotte, les files, les durées et les mesures de CPU. Il n’attribue aucun coût facturé, aucun temps de réponse ou nombre de machines évité au changement de plancher. Une qualification comparative mesurerait les délais demande → admission → premier ACK et le nombre maximal d’hôtes avant d’adopter la règle. Les enquêtes et décisions détaillées sont dans `reuse30-surveys.csv` et `reuse30-decisions.csv`; les chiffres peuvent être recalculés en sommant le produit de chaque valeur par la durée de son enquête et en divisant par 60.

## Le même voisin devient-il admissible si on attend ?

Les 28 enquêtes ayant un voisin entre 16,25 et 30 % contiennent 137 couples (instant, hôte différé), pour 25 machines distinctes. Pour chacun, on suit **la même machine** à l’enquête suivante (~15 s), puis deux enquêtes plus tard (~30 s). Tous les critères du receveur sont réévalués, avant la fin de l’injection. Les fenêtres se chevauchent.

| Horizon | Hôtes-instants suivis | Admissibles | Encore bloqués par le budget CPU | Autre critère devenu bloquant | Fin de trace |
| --- | ---: | ---: | ---: | ---: | ---: |
| +15 s | 130 | 38 (29,2 %) | 87 | 5 | 7 |
| +30 s | 119 | 34 (28,6 %) | 78 | 7 | 18 |

Parmi les 119 fenêtres ayant les deux sondes, **43 (36,1 %)** montrent une admission possible à au moins un des deux instants, dont 29 aux deux. Une disponibilité brève entre les sondes n’est pas observable. À chacun des 137 instants de départ, au moins une autre machine déjà utilisée satisfaisait immédiatement le filtre de ressources ; sa compatibilité avec un donneur particulier et sa réservation restaient à vérifier.

Ces résultats favorisent la recherche d’une cible immédiatement admissible pour une demande réelle, tout en réévaluant le voisin différé aux ticks suivants. Un délai systématique de 15 ou 30 s pour ce voisin ferait patienter le donneur alors que le cas le plus fréquent reste l’absence de marge CPU pour copier. La diminution du seul plancher de réserve reste possible indépendamment. Détail par hôte et horizon dans `reuse30-wait.csv`.
