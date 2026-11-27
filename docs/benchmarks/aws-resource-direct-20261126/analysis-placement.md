# AWS 16k interrompu — diagnostic élasticité / placement

Analyse en lecture seule du commit `420061d`, run `aws-resource-direct-16k-20261126`.
Les ressources sont arrêtées par le harnais principal ; aucun changement de code ni mutation AWS réalisé dans cette analyse.

**Conclusion prioritaire : le contrôleur ne regarde pas la même unité que le placeur.** Le placeur refuse de charger une machine au-delà de 60 %, mais chaque vnode déclenche le scale d'après son seul processus, divisé par les quatre cœurs physiques. Deux processus à 45 % chacun saturent donc l'hôte à 90 % sans déclencher le seuil CPU de 60 %. La demande mesurée baisse avec le débit servi et la file côté contrôleur vaut toujours zéro. C'est observé pendant le décrochage, pas seulement une possibilité théorique.

Cela s'ajoute à des copies coûteuses, des donneurs concurrents sur le même hôte, une réserve qui ne couvre pas les demandes en attente et un délai de démarrage des workers des nouveaux vnodes. Ces éléments expliquent une mauvaise utilisation de capacité disponible. Ils **ne prouvent pas** que le protocole de transfert est sans défaut ; son travail de copie et les scans restent à analyser séparément. L'audit interrompu ne permet pas de conclure à l'absence de perte d'ACK.

## 1. Données du run et occupation réelle

Configuration : six hôtes `i4i.xlarge`, cinq slots chacun, quatre vnodes initiaux, deux machines vides au départ ; maximum 32 machines, CPU/RAM/disque cible 60 %, réserve de réception 15 %, seuil d'écritures 500/s/vnode, items 500 000, mode direct, H3, rampes 120 s et plateaux 120 s. Générateur `2362b35`, nœuds/agents `420061d`.

Les i4i.xlarge observés ont **4 vCPU et environ 32 Gio de RAM**, pas les 16 Gio utilisés comme hypothèse du modèle de dimensionnement. RAM et espace libre sont très loin de leurs seuils sur cette campagne. Le modèle ne doit pas être présenté comme une prédiction calibrée sur ces machines.

Points du `live.jsonl` complet :

| t phase (s) | Demande/s | ACK/s | Vnodes observés* | Machines | Machines portant des vnodes | Machines chaudes | CPU max | P99 écriture | Items max/moyenne affiché** |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 105 | 2 000 | 2 000 | 8 | 6 | 5 | 1 | 15,8 % | 8,2 ms | 1,09 |
| 242 | 3 985 | 3 987 | 8 | 6 | 5 | 1 | 24,5 % | 16,4 ms | 1,03 |
| 302 | 4 000 | 4 005 | 11 | 7 | 7 | 0 | 83 % | 65,5 ms | 1,51 |
| 399 | 5 217 | 4 564 | 17 | 14 | 13 | 0 | 99 % | 1 048,6 ms | 2,42 |
| 434 | 6 384 | 4 113 | 20 | 18 | 16 | 0 | 96,2 % | 2 097 ms | 2,59 |
| 490 | 8 000 | 3 643 | 20 | 21 | 16 | 5 | 97,6 % | 2 097 ms | 2,17 |
| 515 | 8 000 | 3 317 | 20 | 21 | 16 | 5 | 95,4 % | 2 097 ms | 2,06 |

* Nombre des instances répondant au monitoring ; le maximum `known_vnodes` de gossip peut retarder d'une observation. Aucun retour d'un droit d'écrire n'est déduit de ce tableau.

** Le compteur items est consultatif et peut être incomplet pendant les joins : voir section 5. Ne pas traiter ce skew comme une preuve de concentration physique exacte.

À t515, 20 vnodes occupent seulement 16 hôtes : **4 hôtes portent deux vnodes, 12 en portent un, 5 n'en portent aucun**. Aucun hôte ne dépasse deux des cinq slots configurés. Le plafond 32 n'est pas atteint. Les cinq hôtes vides sont mesurés admissibles et disponibles à la fin de la montée. La panne observée n'est donc ni une limite de slots, ni le plafond d'instances, ni une saturation uniforme de toute la flotte.

## 2. Mismatch entre CPU vnode et CPU machine — confirmé

### Chemin de code

- `indexus/autoscale.go:112` : `Node.AutoscaleTick`.
- `indexus/autoscale.go:135` environ : `Resources: autoscale.SampleResources(diskPath)`.
- `internal/autoscale/resources.go:58–79` : `cpuMeter.sample()` utilise `Getrusage(RUSAGE_SELF)` puis divise par `runtime.NumCPU()`.
- `cmd/experiment/agent.go:103` : la vue machine additionne les cœurs des slots.
- `cmd/experiment/agent.go:489` : la vue machine prend aussi le maximum avec `/proc/stat`.
- `internal/autoscale/placement.go:145` : l'admission du receveur utilise la vue **machine**.

Un CPU de vnode à 45 % signifie donc 1,8 cœur sur une machine à quatre cœurs. Deux vnodes à 45 % ne sont pas à 45 % de leur petite allocation respective : ils consomment ensemble 90 % de la machine.

### Preuve temporelle

J'ai rapproché chaque échantillon de `machines.jsonl` du dernier monitoring de chaque slot (âge maximal cinq secondes). Les taux de CPU par processus utilisent `Δprocess.cpu_seconds / Δuptime_seconds` sur environ douze secondes, puis sont divisés par quatre. Script reproductible : `analyze-placement.py` ; résultats détaillés : `placement-samples.json`.

Sur `10.77.1.145`, **sept échantillons successifs** de t422,5 à t512,7, couvrant 90 secondes, montrent simultanément :

- CPU hôte ≥ 85 % ;
- deux vnodes dont le CPU propre reste inférieur à 60 % ;
- leurs débits appliqués individuels inférieurs à 500/s.

À t497,6 :

| Mesure | Hôte 10.77.1.145 | Vnode 52GJ… | Vnode 7YGJ… |
| --- | ---: | ---: | ---: |
| CPU corrigé | 97,78 % observé | 53,94 % | 42,27 % |
| Cœurs consommés | ≈ 3,91 | 2,158 | 1,691 |
| Applications/s | — | 299,5 | 151,6 |
| Dernier CPU vu par son contrôleur | — | 56,05 % | 40,58 % |
| EWMA d'écritures vue par le contrôleur | — | 307,3/s | 148,9/s |
| Appel scale actuellement en vol | — | non | non |
| Initial join en cours | — | non | oui |

La somme des CPU processus (96,22 %) est proche du CPU hôte (97,78 %). Ici la saturation est réelle ; il ne s'agit pas simplement d'un mauvais indicateur incluant beaucoup d'attente disque.

À t512,7 : hôte 95,36 %, processus 51,91 % et 44,05 %, applications 297,5/s et 159,0/s. Sur `10.77.3.17`, huit observations entre t362,4 et t512,7 montrent le même motif. À t512,7, ses processus sont à 36,30 % et 47,25 % alors que l'hôte est à 85,64 %.

**Conséquence : activer `RESOURCE_SIGNALS=true` ne suffit pas. Le signal CPU qui peut déclencher une demande reste à la portée du processus. La décision d'admission, elle, voit correctement la machine entière.**

## 3. La demande mesurée diminue précisément lorsque le service bloque

Chemin :

- `indexus/autoscale.go:36–47` : débit issu de `Service.Demand`.
- `internal/mesh/write.go:71` : incrément sur refus au plafond explicite `WriteRate`.
- `internal/mesh/write.go:81–104` : incrément normal après le retour réussi d'`ApplyRecordContext` ; attente de cutover et erreurs reviennent avant.
- `indexus/autoscale.go:131` : `Queue: 0`.

Le run a `WRITE_CAP=0`. Une écriture en attente d'un verrou ou d'une installation n'est pas encore comptée par `Demand`. Les arrivées abandonnées par le générateur n'arrivent pas au serveur non plus. Le contrôleur n'observe donc pas toute la demande externe. Quand le service ralentit, ses signaux "writes" peuvent redescendre sous 500/s, alors que la demande globale continue d'augmenter.

À t434 : 6 384 demandées, 4 113 acquittées, file générateur 16 384 et 2 271 abandonnées/s. À t515 : 8 000 demandées, 3 317 acquittées, 4 683 abandonnées/s. Ce sont des abandons côté générateur ; ils ne sont pas des réponses 503 du serveur.

**La combinaison de CPU par processus + débit servi + file toujours nulle peut ne plus produire aucun signal de renfort pour une machine saturée.** Des transferts encore ouverts ajoutent un deuxième filtre, mais ne sont pas nécessaires pour construire ce contre-exemple.

## 4. Copies : budget du receveur, absence de budget partagé du donneur

`internal/autoscale/placement.go:65–87` conserve le budget mesuré de 1,75 cœur pour un receveur. Sur quatre cœurs, il ajoute 43,75 points : une machine n'est donc admissible que si son CPU avant réception est ≤ 16,25 %.

Ce budget :

- inclut le transfert et une partie de la charge reprise ; ce n'est pas un simple débit de copie ;
- n'impose aucun rate limiter ;
- ne réserve pas le coût supplémentaire sur le donneur ;
- ne garantit pas le coût total d'une copie locale où donneur et receveur partagent le même hôte ;
- limite les vnodes **recevant leur join initial**, pas tous les transferts sortants de plusieurs vnodes co-résidents.

Preuve complémentaire relevée par l'analyse de distribution : à t≈428, `10.77.3.17` porte deux vnodes mais **trois opérations sortantes `running=true`** : deux depuis sLL… (install vers 10.77.15.164 et freeze vers 10.77.7.182), une depuis tLL… (install vers 10.77.11.141). CPU hôte ≈ 96,2 %, somme CPU processus corrigée sur vingt secondes ≈ 95,5 %.

L'hôte paie donc plusieurs copies simultanées, sans arbitrage de budget physique côté source. L'admission par destination ne peut pas, à elle seule, respecter une cible globale de 60 %. Ne pas simplement réduire `receiverCores` pour remplir les cinq slots : cela accepterait davantage de copies sans résoudre ce coût.

## 5. Vnode actif avant les workers : 120 secondes possibles sans autoscale ni compteur items actualisé

`internal/app/node/main.go:198–206` crée `jobs`, puis attend `servers.startCluster(...)`, et ne démarre `jobs.Start()` qu'après son retour.

`startCluster`, lignes 285–299, ouvre bien le monitoring avant `boot.Join`, mais ce premier join peut durer jusqu'à deux minutes avant d'être poursuivi en arrière-plan.

Pendant cet intervalle, le nouveau vnode peut recevoir des fragments, être `client_ready=true`, appliquer des écritures et consommer du CPU, mais ses jobs de fond ne tournent pas encore. Le mécanisme `HasDelegatableRight` qui autorise théoriquement un fragment natif installé à aider un autre vnode ne sert pas si aucun `AutoscaleTick` n'est exécuté.

Exemple brut vers t485 : un nouveau vnode donne :

```
joining=true, client_ready=true
storage.items=0, storage.zones=0, storage.collections=0
storage.owned_bytes=24 848 345
process.cpu_seconds=71,35
autoscale.pressure={}
```

Il a pourtant déjà des applications mesurées. `Items()` (`indexus/monitoring.go:73`) lit un cache atomique actualisé par `MeasureItems()` ; le membership worker effectue ce rafraîchissement. Les zéros ne représentent pas nécessairement l'absence de données installées.

Même après démarrage des jobs, la mesure est consultative et peut avoir jusqu'à environ 30 secondes de retard (`itemsCountMaxAge`), davantage si le worker est occupé. Pour le dashboard, il faut afficher l'âge/la portée du comptage, et distinguer items possédés, staging et copies conservées. Additionner des copies physiques puis les comparer à des ACK n'est pas un test de perte ou de duplication logique.

## 6. Réserve de 15 % : elle ignore les demandes en attente

`internal/autoscale/reserve.go:19–34` convertit le CPU actuellement observé en offres de réception :

```
want = ceil(used_cores * 15% / 1,75)
deficit = max(0, want - offres_admissibles)
```

À environ quatre cœurs consommés par la flotte stable, cela ne demande qu'une offre. Plusieurs vnodes franchissent pourtant le seuil 500/s au même moment ; leurs besoins de réception ne figurent pas dans cette formule.

`ops/experiment/elastic/spawn.sh:89–114` soustrait les boots puis, quand aucune destination n'est disponible, force un boot uniquement s'il n'y en a déjà aucun. Avec un boot en vol qui couvre la petite réserve, plusieurs donneurs attendent donc le même prochain hôte ; le pool ne grossit pas selon le nombre de demandes distinctes.

Extrait des journaux : dès 15:26:42 UTC, `booting=1 deficit=0` se répète pendant que plusieurs requesters reçoivent des refus. Les demandes retentées parcourent à nouveau les faits de toutes les machines. Les logs disponibles contiennent 101 décisions : 3 locales appliquées, 12 sur hôtes vides appliquées, 1 sur hôte existant appliquée, **85 `new` sans destination**.

Les hôtes vides ne sont pas toujours immédiatement admissibles : le CPU du démarrage peut être 22–25 %, au-dessus du seuil pré-réception de 16,25 %. À 15:29:24, deux hôtes sont comptés vides mais la décision reste "no machine has headroom" ; cela est compatible avec les faits `unavailable` du dashboard, pas une preuve que le placeur ignore des hôtes déjà prêts.

Après leur stabilisation, cinq hôtes sont bien prêts à t490/515. Ils restent inutilisés alors que 20 vnodes ne suffisent pas à servir la charge : les signaux de renfort et le travail de transfert deviennent alors le goulot, pas le pool de machines.

## 7. Ce que les métriques ne permettent pas encore de trancher

- `holdAutoscaleForTransfers()` (`indexus/autoscale.go:75–109`) bloque tant qu'il existe des opérations ouvertes sans droit natif disponible. Il peut relâcher après trois minutes sans progrès, mais pas si des étapes continuent. Son résultat et sa raison ne sont pas exportés. Les opérations ouvertes ne prouvent donc pas à elles seules que le scale a été bloqué par ce filtre.
- `up_in_flight=false` ne suffit pas non plus à exclure l'attente de `pendingSince`, distincte et non exposée. Le code `settlePending` reste à vérifier avec les identités réellement activées.
- Un âge court de l'opération courante ne donne pas l'âge du join complet : les opérations changent au fil des morceaux.
- CPU hôte et CPU processus sont mesurés à des fenêtres différentes. Les rapprochements ci-dessus indiquent la méthode et évitent de comparer un snapshot ancien à un profil ultérieur.
- Un profil de CPU ne mesure pas l'attente de verrou ou de disque. Ne pas prendre son pourcentage cumulé comme une part exclusive de temps mural.
- Les débits par vnode du premier dashboard utilisent `Δcounter/Δwall_agrégateur`, alors que les observations des nœuds arrivent à des instants différents. Pour l'analyse, j'ai utilisé `Δuptime` sur fenêtres longues. Une correction du taux affiché est à prévoir.

## 8. Travail proposé à Claude — sans modifier l'autorité

### A. Test déterministe du mismatch de portée, puis signal hôte coalescé

Fixture minimale : quatre cœurs, deux vnodes chacun à 1,8 cœur, hôte à 90 %, mémoire/disque disponibles ; chaque vnode applique 350/s, donc reste sous le seuil 500. Aujourd'hui le CPU par vnode vaut 45 %, et `Queue=0` : aucun ne demande de renfort malgré l'hôte à 90 %.

Le test doit d'abord constater le comportement actuel, puis vérifier la cible : le contrôleur **physique** détecte la pression et choisit une seule action utile. Diffuser le CPU hôte à chacun des cinq vnodes sans arbitrage provoquerait cinq demandes et davantage de copies.

Proposition de câblage :

1. Faits hôte horodatés, identifiant physique stable et publication d'une pression commune ; conserver séparément le coût propre des vnodes.
2. Une génération de pression / réservation d'opération par machine, coalesçant les demandes concurrentes de ses vnodes. Au plus une nouvelle action de soulagement par réservation, pas une par processus.
3. Choix du vnode/fragment qui réduit le plus la charge réelle de cet hôte, avec admissibilité source **et** destination. Une séparation locale qui ajoute un processus ne crée pas de CPU physique.
4. Fin de réservation liée à l'opération réellement terminée/échouée, récupérable depuis les faits. Un timeout d'ordonnancement ne rend jamais un droit d'écrire.
5. Test avec deux puis cinq vnodes qui reçoivent le même événement hôte : exactement une décision, demandes suivantes fusionnées, pas cinq nouveaux transferts.

Attention : si la pression vient essentiellement des copies, commencer par ralentir/ordonnancer celles en cours peut être préférable à une nouvelle copie. Le test doit couvrir cette distinction pour éviter la rétroaction "CPU de copie → spawn → plus de copies → plus de CPU".

### B. Mesurer une demande indépendante de l'ACK et une vraie file

Séparer arrivées adressées au détenteur, applications, ACK, refus et attente. Exporter les attentes avant `Apply`, le nombre de requêtes en vol et l'âge de file. Ne pas incrémenter deux fois une écriture relayée ou un retry lors du calcul de demande globale. Tester un store volontairement bloqué : le signal de demande ne doit pas tomber à zéro parce que les ACK s'arrêtent.

### C. Limiter les copies par hôte aux deux extrémités

Une réservation machine couvre tous les vnodes sources et destinataires. Tester deux donneurs co-résidents et un troisième receveur local : le budget est partagé au niveau physique. Préserver une voie de progrès ; admission et borne du delta **avant** la fence, pas une validation globale pendant le gel. Les droits d'écrire restent décidés par le store.

### D. Faire vivre le premier fragment avant la clôture du join

Tester un join dont le premier fragment est installé et le reste est suspendu. Avant expiration des deux minutes, le vnode doit publier les compteurs et pouvoir exécuter les tâches compatibles avec l'état partiel (dont autoscale sur un droit natif non épinglé). Séparer les jobs dangereux de reprise/rééquilibrage des jobs qui peuvent démarrer dès `Open`, avec tests de concurrence et d'arrêt. Éviter une deuxième boucle spéciale qui dupliquerait les workers.

### E. Réserve fondée sur le backlog et le temps de réaction

Conserver un plancher en pourcentage, mais tenir compte des demandes distinctes de renfort déjà en attente et de la charge prévue pendant le délai de boot. Distinguer offre prête, boot accepté et destination déjà réservée. Une machine prête ne doit pas être comptée encore comme un boot futur. Borner la croissance par le budget/quota, pas par un stock fixe de machines.

Remplacer le 500 répétitif "waiting for a new machine" par une demande en attente identifiée et dédupliquée, suivie sans reparcourir toutes les machines à chaque tick. Le contrôle physique AWS peut conserver un inventaire récent ; la couche d'autorité du protocole n'a pas besoin de cet inventaire global pour céder un fragment.

## Artefacts

- `captured/steps-1/metrics.jsonl` : CPU cumulatif, transitions joining, counters et opérations par vnode.
- `captured/steps-1/machines.jsonl` : faits machine, slots et admissibilité, toutes les ≈15 s.
- `captured/logs/host0/{machines,provisioner}.log` : décisions et demandes en attente.
- `placement-samples.json` : rapprochement machine / processus, conservant les valeurs intermédiaires.
- `analyze-placement.py` : reproduction de ces rapprochements.
- `profiles/` : profils recueillis par le harnais principal. Ils complètent cette analyse ; leur attribution détaillée relève de l'analyse des transferts.

Aucun essai à 16k/s n'est validé par cette campagne. Les résultats démontrent que des ressources disponibles ne sont pas absorbées assez vite et que le seuil machine n'est pas le seuil effectivement utilisé par les contrôleurs de vnodes.
