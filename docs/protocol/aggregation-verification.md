# Agrégations et croissance locale — 2 novembre 2026

**Statut : historique.** Revue datée du 2 novembre 2026. Le fonctionnement actuel est décrit dans [README.md](../architecture/README.md).

Cette qualification porte sur le runtime causal local, le chemin complet
`indexus.Node`, des machines simulées dans un même processus et Bolt sur le
même disque physique. Elle ne mesure pas le débit de plusieurs serveurs physiques.
Les scripts, commandes, datasets et rapports sont conservés dans
`/Users/remihenriot/Documents/Cockpit/outputs/indexus-aggregations-2026-11-02`.

## Ce que vérifie l'oracle

L'audit des données contrôle séparément les ACK, les payloads et les doublons
sous leur identité `(collection, chemin, ID du record)`. L'audit des agrégations
repart des inventaires durables complets des détenteurs autoritaires : une
écriture appliquée malgré un timeout client doit aussi figurer dans les sommes.
Un inventaire incomplet rend le résultat indisponible, jamais implicitement vide.

Pour chaque préfixe de chaque collection, l'oracle compare :

- le nombre de records vivants, exactement ;
- chaque dimension métrique ;
- le nombre de vnodes répondant pour ce préfixe, exactement un.

Les préfixes des tentatives non appliquées et des records supprimés sont aussi
contrôlés avec une valeur attendue nulle. Une branche fantôme ne peut donc pas
sortir de l'univers testé simplement parce qu'elle n'a plus de record vivant.
Cet univers couvre les chemins de la charge et des inventaires, pas une
énumération exhaustive de tous les chemins possibles de l'espace 96 bits.

L'oracle utilise des accumulateurs scalaires avec compensation de Neumaier,
sans appeler `Abelian.Sum`. Les sommes de métriques entières sont comparées
exactement lorsque la somme des valeurs absolues reste au plus à 2^53. Les
sommes flottantes sont comparées avec une tolérance explicite
`1e-10 + 1e-12 × somme des valeurs absolues` ; elles ne sont pas qualifiées
bit à bit. NaN et les infinis sont refusés. La tolérance précédente de 1e-6
relative pouvait masquer une erreur entière de 1 sur une somme de 10^9.

La convergence des racines vérifie désormais les comptes **et** les métriques
séparément pour chaque collection. Deux erreurs de collections différentes
ne peuvent plus se compenser dans le total global. L'audit complet de tous les
préfixes reste nécessaire après ce signal de convergence des racines.
Pendant les insertions et les handoffs, les résumés restent à convergence
éventuelle : le retard observé est mesuré, sans promesse de lecture linéarisable.

Les tests négatifs couvrent une dérive du durable sans delta de résumé, une
branche supprimée, une contribution fantôme sans record durable, une erreur
entière de 1, NaN/infini et des erreurs de racines qui se compensent globalement.
Un test HTTP avec join vérifie aussi des ancêtres d'adresse XOR identique :
les chemins `@`, `0`, `00`, `000` exposent respectivement les comptes
`3,3,2,1` et sommes `6,6,5,3`, depuis un seul owner chacun.

## Paramètres de croissance

Le banc possède maintenant `-max-acquiring`. Zéro conserve le comportement
antérieur : les activations sont sérialisées, mais les acquisitions peuvent
se chevaucher. Avec 1, le provisionneur attend la fin de la couverture précédente
avant de démarrer une acquisition supplémentaire. Les écritures continuent.
Ce budget relève du provisionnement du banc ; il ne crée aucun validateur global
et ne change ni la validation P2P, ni les règles d'autorité durable du protocole.
Une acquisition terminée en erreur libère ce budget, mais son échec reste signalé.

Le seuil `items-per-vnode` déclenche une tentative de split à partir d'une
observation fraîche. Le fork doit rester limité au donneur dans la vue fournie.
Les slots libres sont remplis avant d'ouvrir une machine supplémentaire.
Le banc arrête les nouvelles décisions après traitement de la charge, attend
les acquisitions en cours, puis audite. Un vnode peut donc encore dépasser le
seuil à la fin ; cette campagne ne recherche pas un équilibre optimal hors charge.
Le nombre maximal de machines est un plafond ; atteindre le seuil ne garantit
pas de revenir sous ce seuil lorsque tous les slots sont occupés.

La campagne complète à 60k utilise : 2 vnodes sur 2 machines initiales,
2 vnodes au plus par machine, 4 machines au plus, seuil 10000 records/vnode,
une acquisition simultanée, décision toutes les 5 s, Update toutes les 1 s,
latence inter-machine de 2 ms et débit proposé par paliers 150/300/450 insertions/s.
Le stockage est `durable-batch`, avec synchronisation disque, délai de group
commit maximal de 10 ms, 96 workers et 12 writers. Les fichiers Bolt temporaires
sont supprimés après fermeture ; les preuves de qualification et datasets restent.

## Calibration et premiers résultats

À 16 workers et un délai de batch de 2 ms, le chemin durable local tient environ
110 insertions/s avant les migrations. Une campagne de 30000 propositions à
200/400/600 par seconde sature donc sa file : 13824 ACK, 16176 refus de file,
zéro timeout et zéro erreur inattendue. Le réseau passe de 2 à 3 machines/vnodes.
L'activation prend 4,25 s, la couverture complète 100,68 s. Après stabilisation,
444875 préfixes sont contrôlés sans écart, sans perte d'ACK ni doublon autoritaire.
Ce résultat qualifie les agrégations des données appliquées, pas l'acceptation
intégrale de 30000 écritures.

Un premier départ à 60000 avec cette concurrence insuffisante a été arrêté
explicitement pour recalibration ; il ne qualifie pas la charge complète.
Un contrôle à 3000 records, 96 workers et batch de 10 ms accepte les 3000 en
10,03 s et vérifie 16014 préfixes sans écart. Il motive la nouvelle concurrence,
sans fournir une comparaison isolée de la taille du batch et du nombre de workers.

La seconde charge comporte 60000 IDs distincts dans 1024 zones de profondeur 16,
réparties sur 16 collections. Les métriques incluent l'index entier de chaque
record et des coordonnées flottantes. La somme entière globale attendue vaut
1799970000. Le dataset est synthétique et réutilise les zones ; il ne remplace
pas le test précédent où chaque record crée une zone nouvelle.

## Contre-exemple trouvé pendant la campagne complète

La première exécution à 60000 a acquitté toutes les écritures sans refus ni
timeout et a atteint 8 vnodes sur 4 machines. Elle **échoue** à la qualification :
94 ACK sont absents de l'inventaire autoritaire final, deux acquisitions refusent
un `frozen-mirror` incohérent et l'audit complet des résumés est annulé parce que
l'autorité ne s'est pas stabilisée. Le fait que la racine corresponde aux
59906 records visibles ne valide pas la conservation des 60000 ACK.
Cette exécution ne permet pas de conclure à la destruction physique des 94 rows.

Le test `TestPreviouslyInstalledZoneCannotBypassNextHandoffFence` reproduit
la cause sur trois nœuds : une zone reçue lors d'un premier join est transférée
à nouveau. L'ancien lookup choisissait une entrée de transfert arbitraire dans
une map ; son flag `Installed` pouvait masquer le gel du nouveau départ. Une
écriture acceptée après le snapshot figé rompt ensuite la séquence du miroir.
Le prédicat transactionnel vérifie maintenant **tous les gels sortants avant
une autorisation**, puis exige l'installation du grant actuel ou du transfert
par zone correspondant à l'opération actuelle. Un ancien flag installé ne suffit
plus. Les preuves restent locales et durables ; aucun arbitre global n'est ajouté.

Un second test montre qu'un transfert émis sans installation confirmée pouvait
retourner un inventaire déclaré complet mais vide. `CausalInventory` refuse
maintenant toute sortie gelée non confirmée, y compris pendant une migration
partielle par zones. Cette indisponibilité est une incertitude de lecture, pas
une preuve de perte. Les deux tests échouent avant correction et passent après.

## Argument local de sûreté

Pour une zone, la transaction de gel pose la fence et capture le snapshot à la
séquence n. Une mutation peut être acquittée avant cette transaction, ou après
l'installation autoritaire suivante ; aucune ancienne installation ne peut
rouvrir l'intervalle gelé. Le miroir doit donc retrouver exactement les mutations
jusqu'à n, puis l'installation ouvre la suite n+1 sur le détenteur suivant.
L'émission et l'installation restent des transactions locales reliées par leurs
preuves ; le provisionneur ne valide pas leur sûreté.

Pour les résumés, le compte d'un préfixe est son compte direct plus les comptes
de ses enfants dans la même collection. Les contributions absolues versionnées
sont remplacées de façon idempotente ; les folds durables et les polls réparent
les manques. Cet argument de convergence suppose la livraison éventuelle des
messages et des ressources disponibles après stabilisation. Il ne promet aucun
délai borné ni ordre strict des lectures pendant les transferts. Les comptes
entiers et les sommes flottantes ont les qualifications distinctes précisées
plus haut.

## Rejeu après correction

Même dataset et mêmes paramètres, nouvelle exécution `60k-fixed-fences` :

| Contrôle | Résultat |
| --- | --- |
| Propositions / ACK | 60000 / 60000 |
| Refus / timeouts / erreurs inattendues | 0 / 0 / 0 |
| ACK absents, payloads altérés, doublons autoritaires | aucun |
| Préfixes audités / écarts | 16014 / 0 |
| Racines par collection : comptes et métriques | toutes conformes |
| Somme entière globale de la métrique 0 | 1799970000 |
| Acquisitions | 5 terminées sans erreur |
| Vnodes / machines | 2 → 7 / 2 → 4 |
| Traitement des écritures | 290,51 s, soit 206,53 ACK/s moyens |
| Convergence des racines après les dernières écritures | 1,34 s |
| Fin de qualification, stabilisation et audits inclus | 302,26 s |

Les paliers 150/300/450 sont les débits **proposés**. Ils ne constituent pas une
preuve de 450 ACK/s soutenus : la file absorbe une partie de la dernière montée
puis se vide. Les acquisitions durent de 14,69 à 29,80 s ; l'activation seule
est plus rapide et ne signifie pas que toute la cellule est déjà acquise.

| Décision à t (s) | Vnodes après ajout | Machines | Couverture (s) |
| ---: | ---: | ---: | ---: |
| 135 | 3 | 2 | 14,69 |
| 150 | 4 | 2 | 29,80 |
| 205 | 5 | 3 | 26,73 |
| 235 | 6 | 3 | 26,46 |
| 265 | 7 | 4 | 26,01 |

Cette réussite qualifie ce scénario d'insertions après convergence. Elle ne
qualifie ni les débits extrêmes antérieurs à 4000/s, ni un scale linéaire sur
plusieurs serveurs physiques. Le premier run défectueux reste conservé dans le
rapport comparatif ; le rejeu ne répare pas rétroactivement ses données.

Les suites `cluster`, `mesh`, `indexus`, `cmd/node` et `benchmark` ont passé
`-race`, hors les deux tests de débit extrême antérieurs. Après le durcissement
d'inventaire, les scénarios concernés de `cluster`, `mesh` et `cmd/node` ont été
requalifiés avec `-race`. Une race dans le booléen d'instrumentation du nouveau
test a été corrigée par un flag atomique et le test repassé. Les contrôles de
formatage et `go vet ./...` ne signalent aucun problème.

## Zone atomique à 72 012 lignes — 2 novembre 2026

La campagne de pics à 120 000 insertions a reproduit une dérive de somme sur une
zone concentrant 72 012 lignes. Les payloads, les comptes et la somme entière de
la métrique 0 étaient exacts, mais seize préfixes dépassaient la tolérance flottante
existante. L'écart atteignait environ 1,14e-5 sur la longitude cumulée. Ce run est
conservé comme échec ; il n'a pas été reclassé en réussite.

`Abelian` conserve maintenant une compensation de Neumaier par dimension. Les
lectures, l'égalité et le JSON utilisent les sommes corrigées ; Clone et les
fusions locales conservent aussi la compensation. Un Unmarshal remplace cette
mémoire avec la nouvelle valeur reçue. Le schéma JSON reste `count` + `metrics` ;
les données durables et les règles d'autorité ne changent pas.

Le test indépendant multiplie les entrées binaires à 256 bits avec `math/big`,
puis compare leur arrondi float64 à la somme des 72 012 entrées. Un autre couvre
l'annulation `1e16 + 1 - 1e16`, la copie, la fusion, la soustraction et le JSON.
Ces tests échouent avant correction. Aucune tolérance d'audit n'a été élargie.
La compensation réduit la dérive ; elle ne transforme pas les flottants en
arithmétique réelle exacte. Le wire conserve un float64 par dimension.
