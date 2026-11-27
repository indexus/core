# Combien de demandes de renfort arrivent à temps ?

Dans le scénario historique, toutes les deux secondes, un donneur peut saisir immédiatement le meilleur receveur admissible. Cette hypothèse change le résultat. À la fin du palier de 16 000 écritures/s, le banc compte 126 vnodes prêts. Le scénario historique en attendait 314.

## Calibrage

Le rejeu conserve la rampe, les ressources, le démarrage des hôtes, le plafond de 32 machines et les contraintes d’admission du scénario `50-voies-strict`. Les paramètres ajoutés viennent des secondes 0 à 300 du run `aws-place-50-direct-1127`. Le générateur de requêtes utilise les événements suivants.

- Le provisioner termine 321 décisions, dont 61 avec placement. Parmi ces décisions, 246 peuvent être appariées sans ambiguïté à une demande du même donneur pour la même cible. Les temps de réponse sont tirés séparément selon l’issue. Le serveur horodate à la seconde. Les refus HTTP 429, antérieurs au hook, sont exclus du calibrage. Les nombres de demandes modélisés portent donc sur celles qui atteignent ce chemin.
- Un donneur garde une seule demande en cours. Quand son délai HTTP de 15 s expire, la décision peut encore aboutir côté serveur. Une réponse tardive positive lance une copie au plus une fois. Avec le comportement synchrone, une réponse tardive négative laisse le donneur attendre jusqu’à 180 s depuis l’envoi. Avec un ticket, cette issue devient consultable dès que le serveur la connaît.
- Les 64 premiers bits de la cible identifient le nouveau vnode. Pour 48 placements, les vitesses appliquées des deux partenaires sont mesurables sur un même relevé, 15 à 30 s après placement, sans nouvelle délégation identifiable par un des deux entre-temps. Les parts du receveur ont un p10 de 46,1 %, une médiane de 49,65 % et un p90 de 53,3 %. Le simulateur tire parmi ces 48 observations à chaque copie autorisée. Cette sélection omet les cas où l’un des deux n’applique encore aucune écriture.

## Validation temporelle

Les 72 relevés de 301 à 660 s servent uniquement à la comparaison. Les valeurs simulées sont les médianes de 20 graines. Les bornes issues des graines mesurent la dispersion interne. Un second essai physique reste nécessaire pour évaluer la reproductibilité.

| Erreur absolue moyenne après 300 s | Instantané historique | Demandes avec délai |
|---|---:|---:|
| Nombre de vnodes prêts | 94,6 | 21,3 |
| Nombre de machines prêtes | 4,9 | 6,0 |
| Débit du vnode le plus chargé, écritures/s | 270 | 359 |
| CPU du plus chaud des hôtes, points de pourcentage | 44,8 | 26,4 |

À 660 s, le nouveau modèle donne 139,5 vnodes prêts (p10–p90 de 104 à 162), sur 32 machines. AWS en a 126 sur 30 machines. L’amélioration des effectifs s’accompagne d’une surestimation du point chaud. Le modèle peut maintenir trop longtemps une part indivisée. La probabilité de placement marginale inclut déjà les contraintes réelles du parc. Vérifier ensuite les ressources simulées peut les compter deux fois. Les deux classes de refus sont exportées séparément. Le coût CPU des sondes d’hôtes, des lectures, des résumés, des processus du système et de la contraction reste à qualifier. La courbe CPU simulée additionne les écritures, le fond des vnodes et la copie. Entre 590 et 650 s, la médiane temporelle du maximum des hôtes passe de 29,4 % dans le modèle historique à 66,0 % dans le modèle synchrone, contre 99,9 % mesurés.

## Expérience sur les demandes

| Variante à 660 s, médianes de 20 graines | Demandes émises | Refus du serveur ou de la réservation | Admissions simulées invalidées | Vnodes prêts |
|---|---:|---:|---:|---:|
| Demande synchrone | 868 | 681 | 38 | 139,5 |
| Ticket avec remise en concurrence immédiate | 3 834 | 2 935 | 452 | 269,5 |
| Une intention conservée, offre réservée | 485 | 31 | 5 | 265,5 |

Le ticket seul laisse les donneurs redemander dès la décision finale. Il quadruple les demandes dans ce modèle. La dernière variante conserve chaque besoin et recherche une offre seulement quand les deux hôtes disposent de capacité. Le modèle examine les emplacements toutes les deux secondes. Le coût de cette recherche du simulateur n’est pas ajouté au CPU prédit. Les donneurs attendent par ancienneté, avec priorité au plus chaud à ancienneté égale. Le donneur et le receveur restent réservés jusqu’au résultat du hook. Avant copie, les budgets sont vérifiés de nouveau. Une offre refusée ou périmée conserve la demande tant que la pression persiste.

Les 485 intentions ont donné lieu à 292 tentatives de réservation internes en médiane. À 660 s, les médianes donnent encore 228,5 intentions en attente hors réservations, quatre en réservation, et 158 s pour la plus ancienne demande en attente. La baisse des refus explicites déplace une partie de la pression vers cette file. Chaque indicateur est agrégé séparément sur les 20 graines. Pour une offre déjà associée à une machine, le tirage est conditionné sur les 48 décisions réelles qui avaient choisi une machine existante dans la fenêtre de calibrage, dont 43 ont abouti. Il suppose des faits frais et des réservations exclusives. Les 31 échecs concernent ces réservations. Un refus initial au donneur et une offre interne infructueuse ont des effets différents. Les bénéfices projetés doivent être vérifiés avec des redémarrages, des notifications perdues, des changements de charge et une mesure du coût des décisions. La simulation ne traite ni le stockage durable des intentions, ni la contraction après la fin de charge.

Au dernier tick, les motifs de l’attente sont ventilés dans le simulateur. En médiane, 91,5 intentions attendent que la machine source termine une réservation ou une copie, 134,5 disposent d’une source libre et cherchent encore une destination admissible, et 0 attendent uniquement à cause du budget CPU du donneur. Les médianes par motif ne s’additionnent pas nécessairement. Un test vérifie l’égalité des comptes pour chaque décision de son scénario. Ce diagnostic est propre aux règles de concurrence simulées. Il suggère de mesurer les mêmes trois motifs sur le prochain banc avant de modifier le nombre de copies simultanées ou la marge CPU de réception.

## Reproduire

Dans ce dossier avec les dépendances de `projection.py` installées, les commandes suivantes exécutent les tests déterministes et écrivent les résultats dans un dossier neuf.

```sh
python3 -m unittest -v test_request_lifecycle.py
python3 compare_control.py observations50.csv --output /tmp/indexus-control-50
```

Le dossier contient `control-replay.pdf`, les séries de comparaison, les erreurs hors calibrage et les compteurs par politique. Les entrées autonomes sont [les délais et fractions de trafic](control-training.json), [les relevés synchronisés](observations50.csv) et les entrées du scénario historique. Pour reconstituer les agrégats depuis l’archive privée, exécuter `python3 calibrate_control.py /chemin/vers/aws-place-50-1127`. Les 8 410 lignes et 15 colonnes des dix projections historiques ont été comparées aux séries archivées et reproduites à l’identique.

## Quand un placement commence-t-il à soulager le donneur ?

Une demande désigne ici le couple (donneur, cible) à sa première apparition. Ses nouveaux appels comptent comme des tentatives. Un placement devient utile au premier message « first write applied » du vnode né pour cette cible. Cette mesure inclut les écritures relayées. Elle ne renseigne ni sur leur débit ni sur la baisse du CPU du donneur. On relie le hook positif au receveur par les 64 bits de sa cible et une naissance unique à moins de 30 s du hook. Sur les 660 s d'injection, cette méthode confirme 110 premières écritures pour 111 placements dans le témoin, et 174 pour 174 dans le run feedback.

| Essai AWS | Tentatives | Demandes distinctes | Placements confirmés par le hook | Première écriture en ≤ 30 s | Première écriture en ≤ 120 s |
|---|---:|---:|---:|---:|---:|
| aws-place-50 | 867 | 211 | 111 | 69/209 (33 %) | 92/200 (46 %) |
| aws-elastic-feedback | 8 802 | 318 | 174 | 92/317 (29 %) | 163/302 (54 %) |

L'horloge part de la première demande du donneur pour la cible. Le dénominateur inclut seulement les demandes dont toute l'échéance tient dans les 660 s. À 120 s, cela écarte respectivement 11 et 16 demandes, même si leur placement est déjà visible. Sous cette échéance, le coût de contrôle mesuré passe de 7,66 à 32,12 tentatives par demande utile. Ce coût compte les tentatives des demandes servies et de celles qui attendent encore. Le témoin et le feedback diffèrent sur plusieurs comportements.

### Prédire le palier suivant avec le début du run

On apprend sur les demandes ouvertes et entièrement suivies entre 0 et 300 s. Sur les demandes ouvertes et entièrement suivies entre 300 et 660 s, on fournit seulement leur nombre à la prévision. Les nombres attendus de placements utiles et de tentatives sont ce nombre multiplié par les moyennes de début de run. L'hypothèse testée est celle d'un régime de service constant.

| Essai et horizon | Demandes évaluables après 300 s | Utiles prévus / mesurés | Tentatives prévues / mesurées |
|---|---:|---:|---:|
| Témoin 30 s | 109 | 37,1 / 35 | 325,9 / 258 |
| Témoin 120 s | 100 | 50,6 / 33 | 574,1 / 208 |
| Feedback 30 s | 205 | 98,8 / 38 | 2 377,6 / 1 902 |
| Feedback 120 s | 190 | 188,3 / 52 | 3 944,2 / 2 911 |

Pour le feedback à 120 s, le service tombe de 111/112 au calibrage à 52/190 en validation. Malgré une initialisation favorable, extrapoler une probabilité constante de placement surestime donc très fortement le soulagement aux paliers élevés.

### Quel travail de contrôle un délai entre les tentatives pourrait-il éviter ?

Parmi les 8 802 appels du feedback, on lit 6 130 réponses « reconciliation in progress », 2 052 délais API, 120 attentes de démarrage, 43 verrous de calcul occupés, 19 absences d'emplacement et 106 expirations réseau. Les 332 autres appels ont une issue différente ou sans échec journalisé. Une réconciliation déjà en cours regroupe des demandes. Elle ne mesure pas une pénurie de machines.

Le rejeu événementiel conserve chaque tentative à son instant réel si le donneur est autorisé à émettre. Seule la réponse d'une tentative conservée peut retarder le donneur. La politique livrée ajoute 5 à 10 s après un refus explicite avant l'envoi au receveur, pour les demandes à une seule cible. Le candidat conserve un demandeur à la réouverture de l'API et étale les suivants sur au plus deux fenêtres de 2 s. L'expiration d'une requête au résultat incertain conserve son traitement. Les logs permettent d'inférer une réouverture depuis le délai annoncé mais ne donnent ni son identifiant ni toute la table d'attente du serveur. Le modèle approxime le plafonnement à 10 s après l'inscription en attente avec les décisions de démarrage observées. Il ignore les cooldowns individuels masqués par l'ancien ordre des contrôles.

| Feedback, médianes de 30 graines | Tentatives conservées | Tentatives sans échec journalisé qui auraient été supprimées |
|---|---:|---:|
| Trace observée | 8 802 | 0 |
| Délai déjà livré (4ca6bbe) | 2 783 | 178,5 |
| Délai livré et étalement API | 2 624 | 203 |

La tolérance de rapprochement des réouvertures est de 50 ms. Entre 5 et 100 ms, la médiane des appels conservés avec étalement varie entre 2 624 et 2 629. Le témoin ne journalisait aucun délai API en millisecondes, ce qui empêche d'y évaluer cet étalement. Le rejeu retire des appels de la trace initiale. Un vrai donneur pourrait réessayer à une date absente des logs, ou manquer une offre libérée entre-temps. Les compteurs du tableau ne prédisent donc ni le nombre de machines lancées ni le débit effectivement servi.

Pour isoler la règle d'ordonnancement, une simulation distincte fait arriver 32 donneurs ensemble, chacun demandant un receveur. Une seule admission devient possible toutes les 2 s, avec une machine immédiatement disponible. Sur 100 graines, l'étalement réduit la médiane de 560 appels à 395. Les deux règles servent 15 demandes dans les 30 premières secondes et les 32 en 64 s. Ce résultat concerne une rafale fermée, sans transfert ni délai de démarrage. Les grandeurs à contrôler sur AWS restent les premières écritures à 30 et 120 s, les tentatives par placement utile et la part des demandes qui attendent effectivement une machine.

Pour reproduire les tableaux sur les deux archives anonymisées depuis ce dossier :

```sh
python3 -m unittest -v test_demand_control
python3 demand_control.py demand-before.json.gz demand-after.json.gz
```

Pour reconstruire une archive de 400 s, utiliser la durée prévue et les mesures du générateur :

```sh
python3 extract_request_episodes.py /chemin/vers/captured archive.json.gz --window 400 --load /chemin/vers/load.jsonl
python3 demand_control.py archive.json.gz
```

La fenêtre enregistrée est la plus courte des deux durées, celle demandée et celle observée. Sans les données du générateur, seul le dernier relevé de charge frais du dashboard confirme la durée. La comparaison entre deux essais prend la plus courte de leurs fenêtres. Une fin de télémétrie manquante ne prolonge pas artificiellement le suivi des demandes. Les logs des donneurs et des receveurs doivent couvrir cette même fenêtre. Un refus HTTP 429 `waiting_for_capacity` prouve que le receveur n'a pas été sollicité. Son texte ne distingue plus un boot, un verrou de calcul et une réconciliation en cours.

La [comparaison AWS à 16 000 écritures/s](aws-pair.md) suit ces mêmes étapes jusqu’à la première écriture du receveur.
