# Analyse CPU, transferts et snapshots — run du 26 novembre 2026

Source examinée : `indexus/core` commit `420061d`. Analyse en lecture seule du
protocole et des ressources ; aucun changement de code ni mutation AWS.

## Résultat principal

Sur un receveur, le verrou d'écriture est pratiquement occupé en permanence par
la comptabilité et la validation des transferts. Le coût ne se résume ni aux
octets à copier, ni au fsync. Un scan caché des sessions de staging représente
environ un tiers du CPU du receveur profilé. Le même contrôle de capacité est
exécuté deux fois pendant chaque `PreloadCausal`.

Cela ne prouve pas une erreur de sûreté du protocole. Cela prouve que son
implémentation de transfert/comptabilité n'est pas encore légère ni bornée par
les seuls octets du lot. Le placement et la régulation par hôte peuvent
aggraver cette charge ; déplacer davantage de vnodes sans traiter ce travail
ne garantit pas que les écritures reprennent leur débit.

## Ce que les profils établissent

Trois profils CPU de dix secondes, même binaire `499515d…`, sont conservés dans
`profiles/`. Les temps CPU sont cumulés entre goroutines : 19,04 s CPU sur dix
secondes signifient environ 1,904 cœur utilisé, pas 190 % d'une machine quatre
cœurs. Les pourcentages cumulatifs des fonctions s'emboîtent : ne pas les sommer.

| Profil | Moment | CPU total | Chemins observés |
| --- | --- | ---: | --- |
| `donor-t4k-cpu.pprof` | 15:27:37 UTC, sous charge 4k | 11,96 s | `scanPaged` 35,87 %, handoff 22,91 %, `PlanningTotals` via autoscale 14,38 %, `Snapshots` 4,18 %, `gcDrain` 11,87 % |
| `post-stop-10.77.1.145-slot1.pprof` | 15:33:49 UTC, écritures arrêtées | 19,04 s | `scanPaged` 43,43 %, `reservationDelta` 33,35 %, `PreloadCausal` 22,16 %, `gcDrain` 27,42 %, `PlanningTotals` 10,08 % |
| `post-stop-10.77.11.141-slot0.pprof` | 15:33:49 UTC | 21,97 s | `FlushPendingEdges` 22,39 %, `gcDrain` 19,07 %, `reservationDelta` 15,25 %, `PlanningTotals` 10,06 %, installation 8,24 % |

Les profils post-arrêt coïncident avec un audit et des lectures de contrôle.
Ils ne constituent donc pas une mesure de repos sans aucune requête. Le second
contient notamment environ 5,1 % de CPU dans l'énumération `/ownership` de
l'audit. Les appels explicitement attribués à preload, installation et
comptabilité sont néanmoins identifiés, pas supposés.

Reproduction :

```bash
go tool pprof -top -cum -nodecount=100 profiles/post-stop-10.77.1.145-slot1.pprof
go tool pprof -peek='reservationDelta|stagingSessions|PreloadCausal' profiles/post-stop-10.77.1.145-slot1.pprof
go tool pprof -peek='checkCapacity|prepareSnapshots|writeCausalChecked' profiles/post-stop-10.77.1.145-slot1.pprof
```

## 1. Le « delta incrémental » cache une énumération chère

Chemin constaté dans le profil du receveur :

```
checkCapacity → usageAfter → reservationDelta → stagingSessions → Tx.scanRows
```

`reservationDelta` consomme 6,35 s CPU, dont **6,31 s (99,37 %) dans
`stagingSessions`**, contre 0,04 s dans les recherches de reçus avant/après.
La première optimisation à viser est donc l'énumération des sessions, pas une
micro-optimisation de la comparaison des reçus.

Source : `internal/storage/usage_delta.go:109` et `:202`. Le calcul commence
par les chemins modifiés, puis appelle `stagingSessions(tx)` pour découvrir
toutes les sessions. Chaque recherche ouvre `Tx.scanRows`, qui appelle
`scanPaged` sur la collection de contrôle à la location `@`.

Le dépôt possède déjà `TestScanPagedStartsWithASmallPage` : il borne la
première page à huit lignes visibles au lieu de 512. Ce test utilise un
`countingReader` artificiel ne contenant que des lignes visibles.

**Hypothèse forte restant à reproduire : amplification par anciennes versions
et tombstones.** `zonelog.Snapshot.ScanWith` traverse les clés et versions avec
`eachKey`, puis saute les tombstones avant d'appeler le visiteur. Huit lignes
visibles peuvent donc demander beaucoup plus de huit clés physiques. Les
anciennes sessions collectées peuvent rester dans les segments avant leur
compaction. Le profil prouve le coût de l'énumération ; il ne prouve pas à lui
seul quel mélange de tombstones, segments et sessions actives le provoque.

À mesurer : appels de seek, clés physiques examinées, tombstones/versions
écartées, lignes visibles, pages décodées, sessions actives et octets décodés.
Un index exact des sessions actives, maintenu dans la même transaction, évite
de découvrir cet ensemble dans les lignes de données à chaque contrôle.
Il doit rester cohérent au crash, à la reprise et à la collecte partielle.

## 2. Deux contrôles de capacité pour un même preload

Chemin de `internal/storage/causal_data.go:436` :

```
PreloadCausal
  prepareSnapshots → checkCapacity
  writeCausalChecked → checkCapacity
```

Dans le profil 10.77.1.145, les deux branches coûtent respectivement **2,12 s
et 2,08 s CPU**. Elles appellent essentiellement la même `usageAfter` et le
même scan de sessions, dans la même transaction.

`prepareSnapshots` n'a qu'un appelant de production, `PreloadCausal`, plus un
appelant de test dans `disk_digest_test.go`. Une simplification à faible
surface consiste à rendre ce helper limité au staging et à garder **un seul
contrôle final** après les modifications du contrôle causal. Ne jamais enlever
le contrôle final de capacité ni déplacer une décision d'autorité vers un
compteur éventuellement périmé. Vérifier les tests de refus atomique, de
capacité et de rejeu avant d'en déduire un gain.

## 3. Préparer le montage hors verrou ne suffit pas

`MountZones` prépare déjà le staging avec une vue MVCC et le valide à la
publication. Mais `PreloadCausal` utilise toujours `updateCausal`, qui prend
`writeMu` dans `updateWroteKind` avant d'exécuter `prepareSnapshots`, les lectures
des headers et les contrôles de capacité. Le coût élevé de preload se trouve
donc bien dans la section critique qui exclut les écritures clientes.

Les deltas des métriques confirment ce mécanisme, au-delà du seul profil CPU.
Sur `https://10.77.1.145:19101` :

| Fenêtre observateur | Durée | Temps tenu collect/control/mount/write | Validation | Temps CPU | GC |
| --- | ---: | --- | ---: | ---: | ---: |
| t400 → t468 | 68 s | 34,08 / 20,66 / 10,73 / 2,29 s | 63,95 s | 1,76 cœur moyen | 459 |
| t630 → t688 | 58 s | 8,44 / 30,70 / 15,10 / 0 s | 53,23 s | 2,02 cœurs moyens | 559 |

Dans la première fenêtre, les temps tenus totalisent **67,76 s sur 68 s**.
La validation en représente environ **94 %**. L'attente cumulée des commits
de writes vaut 64,06 s ; le temps cumulé de queue par écriture vaut 1 862,99 s,
ce dernier additionnant les attentes simultanées et n'étant pas une pause
murale de trente minutes.

Les moyennes de tenue sont alors 144 ms pour control, 87 ms pour mount et
79 ms pour collect, contre 6,76 ms pour le commit write. Après arrêt, elles
montent à 218 / 127 / 103 ms. La phase « validation » inclut les lectures et
le calcul de comptabilité ; ce n'est pas une mesure de CPU pur. En revanche,
le fsync final n'y est pas inclus et ne peut pas expliquer seul cette tenue.

Préparer le preload hors verrou avec une version de staging contrôlée au
commit est une piste, en reprenant les garanties de préparation de mount.
Mais sortir un scan coûteux du verrou sans supprimer sa répétition ne résout
pas la consommation CPU ni le retard du transfert.

## 4. Le planificateur borné laisse une autre boucle complète

`PlanningSample(512)` ne supprime pas le chemin suivant :

```
Worker autoscale → AutoscaleTick → retainedOwnedMetrics
  → OwnershipStats → PlanningTotals → scanZonePages
```

Ce chemin vaut 14,38 % du CPU du donneur profilé. `PlanningTotals` recompte
zones et items, mais `retainedOwnedMetrics` n'en retient que le nombre de
zones et utilise séparément `n.Items()`. Son cache n'est réutilisé que si la
version n'a pas changé ; des écritures ou transferts continus le rendent peu
efficace précisément pendant la pression. Le worker vise un tick rapide.

Le compteur d'items déjà publié et une mesure bornée/maintenue des zones
pourraient remplacer ce parcours dans la décision d'élasticité. Toute valeur
de planification doit rester explicitement consultative et distincte de
l'autorité durable. Une solution à tester d'abord est de réutiliser la mesure
de fond plutôt que de refaire le décompte sur chaque tick.

## 5. Le pire gel observé attend surtout le receveur

Agrégation des lignes `zone transfer progress` dans les journaux capturés,
avec maximum de chaque champ par `(fichier source, operation)` pour ne pas
perdre les valeurs lors d'une reprise qui réinitialise ses compteurs :

- 1 924 opérations ont un progrès non nul ; 838 191 zones et 26 972 unités
  sont observées. Ce ne sont pas des nombres de records uniques ni une preuve
  de transfert exactement une fois : reprises et déplacements sont possibles.
- Les maxima `hold_max` par opération : médiane 53 ms, p95 206 ms, max 974 ms.
- Maxima `freeze_max` : médiane 23 ms, p95 120 ms, max 503 ms.
- Maxima `install_max` : médiane 38 ms, p95 162 ms, max 973 ms.

Le pire exemple est `host0/slots/0`, opération
`3230f96cc6b86271994a571b16dea1f2:285` : `hold_max=974ms`,
`freeze_max=38ms`, `install_max=973ms`. Le gel complet du lot est donc surtout
le temps de l'installation distante et de son attente, pas le fence local.
Ces quantiles sont ceux des **maxima par opération journalisée**, pas les
quantiles clients ni ceux de tous les lots.

Les unités observées portent en moyenne environ 31 zones : il ne faut pas
raisonner comme si chaque commit amortissait toujours 512 zones. Les frontières
de droits et les découpages produisent des unités plus petites. Le coût fixe
des contrôles répétés et du writer devient alors déterminant.

Fichier de détail : `transfer-progress-derived.json`.

## 6. CPU, disque et contention : limites des preuves

Le relevé OS de l'hôte initial à 15:30:19 UTC montre le vrai NVMe local monté
en XFS sur `/var/lib/indexus`, aucune swap, environ 32 Gio de RAM et beaucoup
de mémoire disponible. Sur l'intervalle de deux secondes : environ 20,3 % de
capacité CPU indisponible et 1,5 % de temps d'activité du NVMe. Cet hôte avait
alors terminé une partie de son travail ; ce relevé **n'exclut pas** une
saturation antérieure, ni celle d'un autre receveur simultané.

Les profils CPU ne mesurent ni le temps bloqué sur un mutex, ni le temps passé
en attente d'I/O. Les métriques `Wait` et `Hold` complètent ce manque et
établissent la contention du writer sur le receveur désigné. Les chronos de
validation, la consommation CPU et le GC montrent qu'une part majeure de la
section critique est du travail logiciel. Un profil mutex/blocage et des
mesures disque simultanées sur les receveurs chauds restent utiles.

## Ordre proposé pour la suite, sans toucher aux garanties

1. Supprimer le contrôle de capacité intermédiaire du preload, conserver le
   contrôle final atomique et mesurer la baisse des scans.
2. Rendre l'énumération des sessions indépendante des tombstones et de
   l'historique, avec une maintenance atomique d'un index de sessions actives.
3. Éviter le recomptage complet de `PlanningTotals` à chaque tick d'autoscale.
4. Préparer le preload hors `writeMu`, valider les versions au commit et
   borner le retry. Garder les preuves/receipts/mutations installés dans une
   publication durable atomique.
5. Réguler ensemble émission, réception, collecte et demandes d'élasticité au
   niveau de la machine. Un receveur peut devenir donneur dès qu'il a un droit
   délégable ; ses rôles s'additionnent. Un budget pour « un join entrant »
   ne borne pas le travail total de ses vnodes.

Tests à demander à Claude :

- vrai zonelog avec nombre fixe de sessions actives et historique de sessions
  mortes croissant ; compactions retardées, puis autorisées ; mesurer clés
  physiques/visibles, allocations et durée de stagingSessions ;
- différentiels exacts des compteurs de capacité après preload, doublon,
  installation, collecte partielle, crash/reopen et conflits de reçus ;
- barrière déterministe pendant la préparation d'un preload : une écriture
  sur une zone indépendante doit pouvoir committer avant sa publication ;
- même lot sur des stores contenant 10k/40k/160k zones étrangères : coût du
  lot borné par ses lignes et les sessions actives, pas par toute la donnée ;
- un hôte à deux vnodes dont chacun est sous le seuil individuel mais dont
  la somme dépasse la cible : déclenchement sur la ressource physique et
  demande non masquée par la baisse des ACK pendant la saturation ;
- mesure directe de freeze→installation, plus attente/tenue du writer et
  délai de queue clients, avant de réévaluer le p99 du cluster.

Les garanties à conserver sont l'écrivain unique, le refus des replays de
droits consommés, l'idempotence des requêtes acquittées, les compteurs de
capacité exacts et la lecture des états installés. Aucune amélioration de
placement ne dispense de tester les lectures et les ACK pendant les bascules.
