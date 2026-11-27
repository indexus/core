> Archive de la carte antérieure à la passe de nettoyage du 25 novembre 2026.
> Le fonctionnement actif est décrit dans [la carte actuelle](../architecture/pannes.md).

# Matrice des pannes

> **Transition du 25 novembre 2026.** Cette carte décrit le prédécesseur.
> Les appels liés aux preuves globales, aux checkpoints et au placement doivent
> être relus avec le [nouveau contrat](../protocol/local-permissions.md).
> La [qualification](../protocol/local-permissions-verification.md) distingue
> remplacement branché et qualification distincte ; aucun ancien benchmark ne qualifie celle-ci.

**Statut : actuel.** Pour chaque point où un transfert ou une écriture peut
être interrompu, cette page dit qui accepte les écritures, qui attend, relaie
ou refuse, et comment la reprise se fait **sans rendre l'autorité à la
source**. C'est cette matrice qui justifie le code de reprise, même quand il
paraît inutile dans un parcours sans panne.

Modèle de panne : [../protocol/failure-model.md](../protocol/failure-model.md)
(crash et redémarrage, pertes et retards de messages, partitions ; pas de
participant byzantin). Règles : AUTH-1, AUTH-4, HND-2, HND-3, REC-1, RETRY-1,
MOVE-2, STORE-3 ([registre](../protocol/invariants.md)).

## Le moteur des reprises

À chaque tick de maintenance, `Service.Resume` (`internal/mesh/service.go`) :

- reprend un transfert entrant interrompu (`resumeIncoming`) ;
- relance, pour chaque transfert sortant non installé, **la même opération
  persistée**. Il ne la replanifie pas contre une topologie plus récente, et ne
  crée pas de second successeur ;
- annule un transfert non émis dont la cible a été retirée (`AbortCausal`) ;
- termine la collecte d'un transfert installé (`CollectTransferred`).

Aucun délai ne rouvre une zone ou une région gelée : seule une reprise ou une
annulation durable le fait.

## Transfert de zone

| Point d'interruption | Qui accepte les écritures | Qui attend, relaie ou refuse | Comment on reprend | Tests |
| --- | --- | --- | --- | --- |
| Copie ou rattrapage en cours, rien de gelé | la source | personne | `Resume` relance l'opération ; le receveur garde sa progression par zone (`known`) | `TestMirrorConcurrentWritesAndLostInstallACK` |
| Lot gelé (`FreezeZones` commité), cession non émise | personne pour ces zones | la source refuse (`ErrFrozen`) ; l'écriture attend au plus 500 ms si la barrière du même processus est active | le gel est durable ; `Resume` reprend le lot, ou `AbortCausal` dégèle si la cible est retirée | `TestGranularHandoffResumesFrozenAndLostACKWithoutOverwritingLiveWrites`, `TestResumeAbortsUnemittedTransferToRetiredTarget` |
| Cession émise (`EmitZoneTransfers`), `install-zone` pas encore fait | personne pour ces zones | la source relaie au receveur, qui ne peut pas acquitter : refus, et le client réessaie | une cession émise ne s'annule plus (`TestAbortCausalRefusesAfterEmit`) ; `resumeZoneInstalls` rejoue la même preuve | `TestCausalZoneCycleAndCrashCuts` |
| Installation faite, ACK perdu | le receveur | la source relaie au receveur | `resumeZoneInstalls` rejoue `install-zone` : l'installation est idempotente et préserve les écritures reçues depuis | `TestGranularHandoffResumesFrozenAndLostACKWithoutOverwritingLiveWrites`, `TestMirrorConcurrentWritesAndLostInstallACK` |
| Certificat de région émis, `install` pas fait ou ACK perdu | le receveur, pour les zones déjà basculées | les autres zones attendent l'installation régionale | `Resume` relance l'opération, puis `install`, puis `CompleteCausal`, qui est idempotent | `TestCausalCrashCutsPreserveSingleWriterAndAcknowledgedRows` |
| Transfert installé, collecte interrompue | le receveur | — | `CollectTransferred` reprend à chaque tick ; l'étape finale se décide sur l'état durable | `TestCollectTransferredFinishesAfterRestartBeforeFinalStep` |
| `install` régional perdu, côté receveur | selon la ligne ci-dessus | idem | le receveur rappelle `Join` (ou `resumeIncoming` au tick suivant) : le staging est durable, sans dépendre d'un worker côté donneur | `TestShadowReceiverResumesDroppedInstallWithoutDonorWorker` |

**Garantie commune (AUTH-1, HND-3).** Dans chaque ligne, au plus un disque peut
acquitter une zone. Une écriture acquittée avant l'interruption reste visible
après la reprise (HND-2).

## Disques et redémarrages

| Situation | Effet | Tests |
| --- | --- | --- |
| Redémarrage sur le même disque | l'autorité consommée ne revient pas (AUTH-4) ; les reçus reconnaissent les rejeux (RETRY-1) | `TestCausalAuthorityDiskFenceRestartAndReplay`, `TestCompactReceiptsPreserveOldResultsAcrossHandoffAndRestart` |
| Le vnode change de disque (MOVE) | le nouveau disque reçoit l'autorité ; l'ancien est clôturé et n'acquitte plus (MOVE-2) | `TestMoveHostKeepsXORAndFencesOldDisk` |
| Disque d'un ancien format | `Open` refuse avec `ErrUnsupportedFormat` (STORE-3) : rien n'est lu de travers | `TestOpenRefusesRetiredControlShapes` |
| Crash entre le segment bulk et son reçu | les lignes sont là ; `reconcileBulk` recompte à l'ouverture | `TestBulkIngestInstallsOnceAndCountsLikeWrites` |

## Réseau

| Situation | Ce qui reste garanti | Ce qui ne l'est plus | Tests |
| --- | --- | --- | --- |
| Un pair injoignable | aucune écriture acquittée par un autre que le détenteur | les écritures vers ce détenteur échouent ; le client réessaie | `TestConcurrentJoinsStaleRoutingAndPartitionHeal` |
| Partition, réplication activée | lectures dégradées depuis la copie shadow et le journal de secours (READ-1) | une écriture de secours n'est pas un ACK du propriétaire tant qu'elle n'est pas réparée chez lui ; aucun code de ce dépôt n'en émet ([chemin-ecriture.md](../architecture/chemin-ecriture.md#écritures-de-secours-fallback)) | `TestFaultUnderLoad_PartitionCrashRestartZeroLostACKs`, `TestDegradedReadOverlaysFallbackOnShadow` |
| Knowledge perdu ou en retard (cache évincé, redémarrage) | aucune autorité n'est déduite (ROUTE-1) | des routes fausses : relais ou refus en plus ; l'appelant renvoie le paquet complet | `TestKnowledgeWireFullThenReferencesAndReceiverRestart`, `TestKnowledgeWireLostACKRetriesIdempotently` |

## Résumés (totaux)

| Situation | Effet | Tests |
| --- | --- | --- |
| La source garde les lignes cédées jusqu'à leur collecte | le total lit ses lignes locales et celles de chaque enfant distant dans **une seule version** (SUM-4) : une collecte ou une installation qui commite entre les deux ne compte rien deux fois et n'oublie rien | `TestRootTotalReadsOneVersionWhileCollectionDeletesHandedRows`, `TestJoinAfterBulkMovesItsZones` |
| Changement de holder d'un enfant | une contribution n'est retenue que si son holder est encore le bon sous `Tree.Mu` (SUM-3) | `TestSummaryOldContributionPausedBeforeLockCannotOverwriteNewHolder` |

## Ce que cette matrice ne couvre pas encore

- La convergence des résumés après une perte de messages, décrite par SUM-2
  et AGG-1.
- Des mesures de durée par ligne (combien de temps une zone reste sans
  écrivain dans chaque cas). Seule la ligne « lot gelé » est mesurée
  aujourd'hui (`transferProgress.hold`).
