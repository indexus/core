# Qualification des permissions locales

**Statut : campagne locale finale verte sur `e65dfd3` : tests globaux, détecteur de races, vet, fuzz, PowerFS64, HTTP64 strict et 31 configurations TLC.** Cette page
n'attribue pas aux nouvelles permissions les résultats d'un ancien binaire.
Une ligne de test pur ou de TLC n'est pas une preuve du chemin HTTP et du disque.

## Correctifs et nouvelle qualification après AWS

La [passe de reprise et de nettoyage](../benchmarks/recovery-cleanup-20261125.md)
est distincte de la campagne verte historique ci-dessous. L'essai AWS sur
`a15284d` a échoué à 16k/s et a laissé des transferts ouverts ; les résultats de
`e65dfd3` ne doivent donc pas être présentés comme une qualification AWS.
Les nouveaux tests ajoutent une dépendance entrant/sortant absente des anciens
modèles, la préparation disque froide concurrente, le routage des morceaux
installés et le retrait de la seconde copie régionale. Les résultats et limites
du laboratoire enrichi sont consignés dans cette passe.

## Nettoyage et coûts de contrôle

La [passe de contrôle](../benchmarks/cleanup-scale-20261125.md) retire les anciens
modèles des fichiers de production, sépare la membership des commits locaux,
réutilise le trie pour les cellules et corrige budget/rotation du polling.
Ses microbenchmarks ne prouvent pas un p99 client constant. La fermeture des
transferts sous écritures et les limites du laboratoire sont consignées séparément.

## Passe globale confirmée au checkpoint antérieur

Sur le checkout propre `e65dfd3`, `go test -race ./...` a passé pour l'ensemble
du dépôt. La passe finale rapporte notamment `internal/mesh` en 184,153 s et
`indexus` en 30,583 s ; certains autres packages ont réutilisé des résultats de
test valides du cache Go. `go vet ./...` a également passé. Le journal de la
première campagne rejetée est conservé : les correctifs de fixtures gardent les
assertions de sûreté et observent explicitement les conditions concurrentes,
sans simplement allonger les délais.

Le runner TLC complet a terminé les **31 configurations** : **11 résultats
`configured_checks_passed` et 20 `expected_counterexample`**. Le résultat JSON
est produit par TLC 2.19, jar v1.7.4 de SHA-256
`936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88`.
Cela inclut les nouveaux modèles de tickets et reçus terminaux ; les limites
bornées et l'absence de raffinement Go restent applicables.

Les artefacts locaux sont `target-final-race-v2.log`, `target-final-vet-v2.log`
et `target-final-tlc/results.json`, conservés dans le répertoire parent du
worktree de qualification. Cette passe globale complète les sélections décrites
plus bas ; les durées de ces tests ne sont pas des temps de gel utilisateur.
Les campagnes fuzz, PowerFS64 et HTTP64 strict sont détaillées ci-dessous. Aucun nouveau test AWS n'a été
lancé dans cette phase.

## Campagnes finales Go, fuzz et PowerFS64

Le manifeste `target-final-go-checks.json` identifie le commit complet
`e65dfd38356f97b00df78af6aa20b285748caf17`, les commandes, sorties et durées.
Tous ses codes de sortie valent zéro :

| Campagne | Résultat | Limite |
| --- | --- | --- |
| `go test ./...` | PASS, commande en 53,107 s | Certains packages peuvent réutiliser le cache Go |
| `FuzzHandoffSchedule`, 30 s, deux workers | 3 489 cas ; commande en 34,611 s | Exploration aléatoire finie, pas tous les ordonnancements |
| `FuzzDirectoryVersionRankPreservesPossibleUpdates`, 30 s, deux workers | 1 247 800 cas ; commande en 30,882 s | Fonction de rang et ses assertions, pas réseau complet |
| `TestHandoffSchedule64`, deux exécutions | PASS en 8,98 s puis 9,10 s | 64 disques simulés PowerFS, pas 64 machines |

Les deux exécutions PowerFS64 produisent le même transcript SHA-256
`11037c2e738668fd2e60730ebd707c5169e3df8304d76ce9126adcf8ca49aa3a`.
**Par exécution** : 3 522 étapes, 284 écritures dont 23 précoces, 51 transferts,
43 messages perdus, 25 réponses perdues, 167 crashes et 167 réouvertures,
10 coupures I/O, 153 vrais paquets rejoués et 171 collectes/compactions.
Le contrôle réaudite l'écrivain unique et les ACK aux étapes prévues du scénario.
Ce sont les fautes exercées du dispositif, pas des interruptions AWS ou une
preuve de comportement de tout système de fichiers réel.

Une erreur de l'ancien harnais `wireClone` pouvait transformer un paquet en `{}`
en ignorant un échec JSON. La campagne actuelle exige une vraie sérialisation et
rejoue effectivement les 153 anciens paquets. Les comptes historiques affectés
par cette erreur ne prouvent pas le rejeu de leur contenu original ; ils ne sont
pas additionnés aux nouvelles passes.

Les logs sont `target-final-unit-v3.log`, `target-final-fuzz-v3.log`,
`target-final-directory-fuzz-v3.log` et `target-final-schedule64-v3.log`.

## Laboratoire HTTP64 : disponibilité stricte

Le run local `target-http64-v6` sur `e65dfd3` a passé son assertion stricte :
**1 480 offres, 1 480 ACK, zéro erreur et zéro rejet** ; l'audit constate zéro
ACK perdu ou dupliqué et zéro état gelé restant. Avec 128 enregistrements seed,
il observe 1 608 records. Les 64 joins ont terminé vers 65 s ; l'audit de
rééquilibrage prend 76,739 s, l'audit final est émis à 103,809 s et le test
termine en 108,310 s.

Il s'agit de 64 services HTTP réels sur un seul hôte, avec fichiers/fsync réels,
32 devices sérialisés simulés à 1 ms, réseau simulé à 1 ms, partition de 750 ms,
traîne I/O de 25 ms pendant 750 ms et pertes de réponses d'installation. Aucune
autre campagne de test ne tournait pendant la mesure. Le p99 d'ACK vaut
566,489 ms, le maximum 752,746 ms et le p99 intentionnel 572,225 ms : **ce ne
sont pas des mesures de p99 de freeze** ni le débit de 64 machines AWS.

Les sources sont `target-http64-v6/events.jsonl` (événement `audit`) et
`target-http64-v6.log` dans le répertoire local de qualification. La version v5
avait conservé 1 817 ACK mais refusé 124 offres sur 1 941 ; sa conservation des
ACK ne satisfaisait pas la disponibilité. La nouvelle assertion exige aussi
zéro échec/rejet et ACK = offres : seule v6 est rapportée comme réussite de ce
scénario strict. Ce scénario fini ne prouve pas la disponibilité sous toute
partition ni une durée universelle de gel.

## Ce qui a été exécuté

| Sujet | Preuve présente | Portée et limite |
| --- | --- | --- |
| Ticket, confirmation, clôture et rejeu | `TestLocalPermissionsTicketLifecycle`, `TestLocalPermissionsTentativeCancelAndValidation` | Transitions pures ; pas un fsync ni une transaction de stockage |
| Retour de région et vieux install | `TestLocalPermissionsReturnAndCompactedReplay` | Permissions locales et anciens paquets ; le montage des octets doit avoir ses propres tests |
| Consommer les deux frères ; compteur épuisé | `TestLocalPermissionsCoalesceAndOverflow` | Noyau pur ; complété par les tests de coalescence/pins du stockage cités plus bas |
| Membership indépendante de l'autorité | `TestMembershipConvergesWithoutHandoffHistory`, `TestMembershipRejectsConflictsAtomically` | Annonces honnêtes compatibles ; pas de convergence universelle en présence de forks |
| Destinations futures et retour physique | `TestMembershipFutureEndpointAndPhysicalReturn` | Résolution des routes, sans acquisition de droit |
| Publication concurrente | `TestMembershipConcurrentPublication` sous `-race` | Concurrence exercée, pas preuve de tous les ordonnancements |
| 64 caches, partitions et oubli des déplacements | `TestMembership64PeersRepartitionAndDrain` sous `-race` | 768 déplacements, 32 768 messages, 64 records/copie, JSON final 9 348 octets ; aucun disque réel ni SLA |
| Transport membership | `TestMembershipWireConvergesAndUsesOneReceiptPerPeer`, `TestMembershipWireLostReplyAndReceiverRestart` | Contrôle seul, callbacks de test ; ne qualifie pas à lui seul le chemin complet d'écriture |
| Couverture et renvois après sérialisation JSON | `TestQueryCoverageFailedHintCannotSatisfyMandatoryReferral`, `TestQueryCoverageJSONDistinguishesKnownEmptyFromUnknown`, `TestQueryCoverageIgnoresOutOfScopeJSONReferral`, `TestQueryCoverageJSONFollowsMandatoryLatestACK` | Quatre tests contre DTO JSON, dont échec initial/découverte devenu obligatoire ; pas un serveur HTTP réel |
| Saturation du transport asynchrone | `TestMembershipWireScheduleNeverWaitsAndClosesFlights`, `TestMembershipWireBoundsPeerReceiptsAndRetries` | Soumission bornée et non bloquante dans les scénarios exercés |

Les tests sont dans `internal/causal/*_test.go` et
`internal/mesh/membership_wire_test.go`. Leurs noms n'impliquent aucune preuve
mathématique générale.

## Premier pont vers les vraies méthodes du protocole

Les **12 scénarios de `internal/mesh/service_test.go`** ont passé sous `-race`
contre les fichiers de production, dans une compilation ciblée pendant le portage
des autres tests (commit de tests `176ad61`). Cette sélection historique était distincte de la passe globale finale rapportée
ci-dessus. Le transport est en mémoire et les
stores de cette fixture ont `NoSync: true` : ces résultats vérifient le parcours
et les assertions, pas une durée réseau réelle ou la durabilité sur panne électrique.

Les assertions conservées couvrent notamment :

- `TestConcurrentJoinsStaleRoutingAndPartitionHeal` : conservation des lignes,
  convergence après partition et un écrivain parmi chaque sonde ;
- `TestMoveHostKeepsXORAndFencesOldDisk` : A→B→A sur les disques d'origine,
  refus de l'ancien disque, ACK et données conservés, endpoint 1 puis 2 ;
- `TestMirrorConcurrentWritesAndLostInstallACK` : mutation pendant copie,
  réponse d'installation perdue et rejeu sans résurrection ;
- `TestCompetingJoinFinishesPersistedTokenReservation` : une réservation
  abandonnée ne bloque pas un autre join sans worker de reprise séparé ;
- `TestJoinActivatesOnFirstInstallAndForwardsTheRest` : R sert sa première région
  et relaie vers S la région non installée avant la fin du join ;
- `TestZONE1_WriteAckedByOneSideDuringMigration` : lots forcés à un point,
  premier point écrivain sur R, deuxième encore sur S, exactement un ACK direct.

Cette passe a exposé de vrais défauts du remplacement : publication d'activation
sur une migration physique, attribution de numéro d'endpoint, résolution des
sources entrantes et reprise d'une réservation épinglée. Les corrections sont
dans les chemins de production ; les assertions n'ont pas été remplacées par
de simples tests de compilation.

Les tests de couverture JSON ont également passé sous `-race`. Un contrôle
négatif par overlay externe retire la vérification d'échec d'un pair devenu
obligatoire : les deux sous-cas (candidat initial et découverte ultérieure)
renvoient alors un succès partiel, détecté par le test. Aucun fichier de production
partagé n'est muté pour cette expérience. Le test positif avec données plus récentes
vérifie séparément que les renvois survivent à la sérialisation JSON.

## Transactions de stockage et lectures adversariales

Les scénarios `internal/storage/local_tickets_test.go` utilisent `Open` avec
synchronisation durable activée, contrairement à la fixture réseau `NoSync`.
Leur auteur rapporte une passe `-race` des huit scénarios (stockage `ff4bfa9`) : confirmation avant
fence, installation granulaire et rejeu, retour avec ancien collecteur, premier
point physique avant publication, budget d'admission, chemin d'écriture en cache,
annulation avec réponse perdue et réouverture, renvois limités aux points basculés.
Ces tests de transactions et de réouverture ne sont pas, à eux seuls, une campagne
exhaustive de coupures électriques.

Les noms correspondants sont `TestLocalTicketsGranularEarlyACKAndReplay`,
`TestLocalTicketsNoFenceBeforeConfirmation`,
`TestLocalTicketsReturnOldCollectorAndAdmissionReplay`,
`TestLocalTicketsPhysicalPointBeforePlacementPublication`,
`TestLocalTicketsAdmissionBudgetPreservesConfirmedSlots`,
`TestLocalTicketsWritesUseCachedLocalPermissions`,
`TestLocalTicketsCancelledCleanupSurvivesLostReplyAndRestart` et
`TestLocalTicketsPointReferralsDoNotBlockUnmovedScopes`.

Les tests `TestCoalesceLocalRightsAfterReturnPreservesACKsAndClosedTickets`,
`TestCoalesceLocalRightsRespectsPinnedSourceInputs`,
`TestPhysicalReceiverCannotRunSourcePointAPIs` et
`TestCancelledDiscoveryLeavesNoHistoricalRows` couvrent en plus les entrées
épinglées, la distinction des disques d'un même vnode et les lignes de découverte
qui doivent disparaître lors d'une annulation. Leur passe ciblée ne remplace pas
la qualification complète du dépôt.

Les **sept scénarios de lecture** des fichiers `query_delegation_test.go` et
`query_handoff_cut_test.go` ont passé sous `-race` avec les méthodes de production
(compilation ciblée, commit `c0de9e3`, transport mémoire et stores `NoSync`). Ils
conservent les ACK à travers A→B→C après collecte, lisent à travers deux sources
non installées, rejettent le succès partiel si R disparaît, et terminent un transfert
pendant le snapshot MVCC d'une lecture. Le retour physique A→B→A exige désormais
une **lecture complète avant publication du nouvel endpoint** ; l'ancien échappatoire
« `ErrNotReady` est acceptable » a été retiré.

`TestPhysicalMoveCarriesEmptyDelegationToUnawareReader` migre une délégation
sans aucune ligne, coupe l'ancien hôte et garde un lecteur ignorant la membership
du délégataire. La lecture vide réussit grâce à la couverture et au répertoire
migrés ; une première écriture ultérieure acquittée est ensuite visible par ce même
lecteur, sans réparation globale.

`TestPhysicalMoveRefusesIncomingPointsBeforeClosingIntake` a d'abord reproduit
un défaut : un MOVE refusé persistait pourtant `Moving`, fermant l'admission d'un
receveur au milieu de son transfert entrant. Après la correction de stockage,
le test a passé sous `-race` (commit de test `d2d67bc`) : refus avant cette mutation,
deuxième point et consolidation possibles, trois records conservés dont un ACK
antérieur, puis le même MOVE accepté une fois les droits consolidés.

`TestReceiverCancellationCrashCutsRetainCleanup` a aussi passé sous `-race`
avec le dispositif PowerFS : 32 positions d'injection sont essayées, au moins une
interruption effective est exigée par le test, puis crash, réouverture et retry
vérifient l'absence de slot et de garbage de staging/découverte. Les droits et
ACK sont réaudités après chaque scénario. Ce sont des pannes du dispositif de
stockage modélisé, pas 32 arrêts de machines AWS ou une preuve universelle du fsync.

Les **six scénarios de `receiver_drain_test.go`** ont passé ensemble sous
`-race` en compilation ciblée des sources de production et fixtures (commit
`d9f1041`, transport mémoire, stores `NoSync`, 1,715 s pour cette passe) :

- `TestLostInstallReplyClosesAfterPhysicalReceiverDeparture` : réponse native
  perdue, premier point physique non root écrivain avant publication, ancien hôte
  coupé, reçu terminal appris puis dette source collectée ;
- `TestLostInstallReplyClosesAfterLogicalReceiverRetirement` : retraite avec
  droits clairsemés et délégation vide migrée ; un lecteur ignorant le délégataire
  voit aussi sa première écriture ultérieure, sans ancien hôte ;
- `TestAddressChangeCannotCloseUninstalledReceiverTicket` : changer l'adresse du
  même disque ne clôt jamais une admission non installée ; le même ticket doit
  être rejoué à la nouvelle adresse ;
- `TestPhysicalReturnUsesOriginalClosedTicketNamespace` : retour au disque
  initial, ancien ticket acquitté sans remonter le droit consommé ;
- `TestOldIssuedFenceCannotBlockReacquiredPointOrNativePermission` : S→R→C→S,
  ancien ACK S perdu, R inaccessible ; point puis natif revenus restent écrivains
  et lisibles, les anciennes fences ne visent que l'ancien droit ;
- `TestCancelledWarmReturnKeepsCurrentSourceWithoutOldReceiver` : copie de retour
  confirmée puis annulée avant fence, source actuelle C conservée dans le
  répertoire ; lecture exacte sans ressusciter l'ancienne destination R.

Le scénario de retour sous ancienne fence a d'abord échoué avec `ErrFrozen`,
ce qui a conduit au correctif sur l'identité du droit et au répertoire courant
unique. Ces tests vérifient des transitions et des ACK ; leur durée locale n'est
ni une distribution de gel ni un résultat HTTP/AWS.

Les laboratoires de 64 disques et HTTP sont rapportés séparément en tête de
page, avec leurs binaires et limites. Aucun résultat de caches seuls n'est
transformé en qualification de stockage ou de réseau réel.

## Régression des résumés hors placement

La qualification complète a révélé une erreur réelle après joins concurrents :
Query voyait quatre ACK, mais un ancien détenteur continuait de servir trois
records depuis son résumé. Des droits actuels et le placement XOR pouvaient
légitimement différer ; les polls n'exportaient aucune frontière permettant de
réparer ce total. Allonger le délai ou ajouter un rebalance dans le test aurait
masqué ce défaut.

Le correctif `62acc85` conserve le test d'origine et ajoute :

- `TestDerivedReadsFollowOffPlacementNativeDelegation` : cession volontairement
  hors placement, nouvel ACK, lectures fraîches/ordinaires exactes et un seul
  répondeur physique d'agrégat ;
- `TestDerivedParentInvalidatesOffPlacementChildContribution` : parent toujours
  dans sa cellule, descendant déplacé, troisième ACK après invalidation, retour
  au placement puis restauration du cache ; la suppression des trois dernières
  lignes exige ensuite un zéro complet, pas une dépendance incomplète éternelle ;
- `TestSummaryPollFloorRejectsDelayedCompleteAcrossReceiverRestart` : ancien
  message complet retardé après invalidation, fermeture/réouverture du vrai store,
  nouvelle incarnation persistante et refus d'un ancien statut à grande révision ;
- `TestIncompleteContributionPropagatesAndNewerCompleteRepairs` : propagation
  par frontière, sérialisation JSON des flags, puis réparation par valeur nouvelle ;
- `TestLocalSummaryReadinessCachesControlChanges` : vingt lectures et une écriture
  ordinaire réutilisent le cache de contrôle ; une partition native le reconstruit.

Le paquet `internal/summary` entier a passé sous `-race` (1,808 s) ; le paquet
`indexus` entier a passé en test ordinaire (23,715 s). Une sélection comprenant
la phase réseau originale et les nouvelles régressions a passé sous `-race`
(12,981 s) ; concurrent-joins et les deux régressions ont aussi passé trois fois
(15,927 s). Ces durées décrivent ces passes locales, pas des gels utilisateur.
La dernière assertion de zéro complet a été ajoutée ensuite et passée sous
`-race` dans sa sélection (2,116 s), avant la passe ordinaire complète.

Les stores des deux nouvelles fixtures façade sont ouverts avec sync activée ;
le transport reste en mémoire. La propagation d'incomplétude et le cache de
readiness ne sont pas dans les modèles TLA ci-dessous : leurs preuves sont ici
des tests fonctionnels, pas un raffinement formel ni une nouvelle garantie de
linéarisabilité des lectures ordinaires.

## Modèles TLA+ et contre-exemples obligatoires

| Modèle positif | Graphe exploré | Propriété et contrôle négatif |
| --- | ---: | --- |
| `ReceiverTickets` | 501 888 états distincts | Écrivain unique, ACK de fermeture justifié, slot confirmé conservé ; trois mutations réinstallent un ticket clos, oublient un slot ou omettent la fence |
| `TicketData` | 1 472 états distincts | Conservation des ACK et dernière révision au détenteur ; mutations : pas de copie, collecte précoce, ancien collecteur effaçant le retour |
| `FirstFenceCancellation` | 20 états distincts | Annulation seulement avant la première fence, slot conservé ; `CancelAfterPointFence` produit deux écrivains dans 20 états visités |
| `ReceiptAfterDeparture` | 336 états distincts | Reçu terminal et dernière révision conservée après réponse perdue, MOVE/retraite et retour ; `DepartureBeforeDrain` (49 états) et `AddressAsDrain` (31 états) échouent sur `SoundReceipt` |

`ReceiverTickets` est limité à deux disques, deux cellules, trois allocations par
disque et deux offres. Sa vérification temporelle concerne ce graphe contraint ;
elle n'est pas une preuve non bornée de vivacité. `TicketData` traite une région,
une révision et A→B→A. Le petit modèle d'annulation isole un point dans une région :
un point est déjà émis/installable avant la clôture régionale. Il vérifie la sûreté,
**sans propriété temporelle ni mesure de durée**.

`ReceiptAfterDeparture` isole une admission originale et une révision ultérieure,
avec pertes de réponse, arrêt/reprise et départ honnête. Il vérifie la sûreté,
sans fairness ni mesure de durée ; la fermeture permanente des anciennes
admissions est une précondition du producteur, pas un accord global. Il ne
modélise pas le répertoire multi-région ni de nouvelles admissions après départ.

Ces modèles ne sont pas une composition formelle entre eux et ne raffinent pas
les transactions Go. Les contrôles négatifs doivent échouer avec l'invariant
attendu ; un timeout, une erreur de syntaxe ou un autre invariant ne compte pas.
Le [runner](../../spec/handoff/check.py) vérifie le code de retour et le message
TLC. Ses résultats positifs signifient seulement que les vérifications configurées
ont passé ; certains anciens modèles n'ont aucune propriété temporelle.

## Obligations de l'intégration avant livraison

| Garantie à garder | Vérification d'intégration exigée |
| --- | --- |
| Écrivain unique | Sur les vrais prédicats de commit, source interdite après fence ; cible interdite avant installation atomique |
| ACK durable | Coupures aux frontières de commit, réouverture, dernier delta et reçus conservés |
| Annulation sûre | Rejet après **première fence**, même avant émission régionale ; reprise persistée de la même opération |
| Rejeu sans effet | Après collecte et retour A→B→A, ancien point/région ACK sans montage d'octets anciens |
| Admission sans attente globale | Builder/diffusion de membership indisponibles pendant le lot ; écritures installées et reprise restent possibles |
| Lecture fraîche exacte | Pages MVCC + couverture de l'espace vide + sources/successeurs obligatoires ; trou ou pair indisponible refuse le succès partiel |
| Totaux exacts pendant join | Pas de double contribution ni perte d'ACK après transfert, partition et déplacement physique |
| Décharge immédiate | Distinguer première installation, premier ACK cible par relais, apprentissage de route directe et clôture entière |
| État borné par le présent | Après de vrais cycles disque, aucun graphe de certificats/anciens endpoints ; restent fragmentation actuelle, slots réels et tombstones |

Les résultats finaux des campagnes disque/HTTP seront ajoutés avec leurs
assertions, leurs logs et le commit effectivement exercé. Cette liste n'est pas une déclaration
de réussite. Une simulation de caches ne remplace pas ces tests.

## Mesurer le gel sans confondre les horloges

Il faut enregistrer, par lot et opération : admission confirmée, début de fence,
fin de montage durable, premier ACK utilisateur chez la cible, premier accès
direct, clôture et collecte. Le gel du lot n'est ni la durée du join ni le délai
d'apprentissage du routage. Un dispositif en mémoire ou une horloge simulée ne
prédit pas le fsync, le réseau ou la saturation sur AWS. Aucun nouveau budget AWS
n'est engagé par cette qualification locale.
