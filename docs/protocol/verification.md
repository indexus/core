# Verification

**Statut : registre de qualification locale, 25 novembre 2026.**
La campagne `e65dfd3` est un repère antérieur. Les
[correctifs de reprise](../benchmarks/recovery-cleanup-20261125.md), puis
[le nettoyage et les coûts de contrôle](../benchmarks/cleanup-scale-20261125.md)
consignent leurs commits, tests et limites séparément. Aucun de ces résultats
locaux ne qualifie automatiquement AWS à 16k/s.

This register names existing tests for the current working tree. The former
[qualification record](../archive/verification-frontier-20261125.md) is historical;
its passing runs do not automatically qualify the replacement. The
[target evidence map](local-permissions-verification.md) distinguishes tested pure
kernels, bounded TLA+ models and the final local storage/HTTP campaigns.

A named test is scoped engineering evidence, not a general mathematical proof.
`PARTIAL` below means that a relevant test exists, often retained from the previous
runtime, with the final local integration campaign reported in the evidence map.
`GAP` names an obligation without sufficient current evidence. No `PROVED` claim
is carried over merely because a test kept its name. The guard
`TestVerificationDocMatchesTests` still rejects references to missing tests and
requires normative test IDs to remain represented.

## Current test map

| ID | Status | Method | Tests | Limits |
| --- | --- | --- | --- | --- |
| CRDT-2 | PARTIAL | pure/kernel and retained scenarios | `TestConcurrentJoinsStaleRoutingAndPartitionHeal` `TestMembership64PeersRepartitionAndDrain` `TestMembershipRetirementBeforeActivationAndRedirectMove` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| AUTH-1 | PARTIAL | pure/kernel and retained scenarios | `TestCausalDivergentViewsExplore` `TestLocalPermissionsTicketLifecycle` `TestZONE1_WriteAckedByOneSideDuringMigration` `TestLocalTicketsGranularEarlyACKAndReplay` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| AUTH-3 | PARTIAL | pure/kernel and retained scenarios | `TestBulkIngestReachesTheParentOnce` `TestBulkIntoAZoneOwnedElsewhereIsRefused` `TestConcurrentJoinsStaleRoutingAndPartitionHeal` `TestLocalPermissionsTentativeCancelAndValidation` `TestLocalPermissionsTicketLifecycle` `TestMoveHostKeepsXORAndFencesOldDisk` `TestLocalTicketsNoFenceBeforeConfirmation` `TestLocalTicketsPhysicalPointBeforePlacementPublication` `TestPhysicalMoveRefusesIncomingPointsBeforeClosingIntake` `TestMovedEndpointRejectsNewOldEpochAdmissionButAcceptsPhysicalReturn` `TestMovePrunesTentativeSlotBeforeClosingIntake` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| PLAN-CRDT-1 | PARTIAL | retained scenarios; final local campaign passed | `TestValidatePlanRejectsExternalStealWithoutExistingRows` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| MOVE-2 | PARTIAL | retained scenarios; final local campaign passed | `TestMoveHostKeepsXORAndFencesOldDisk` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| ROUTE-1 | PARTIAL | pure/kernel and retained scenarios | `TestCausalRuntimeHTTPJoinStaleRouteAndRestart` `TestConcurrentJoinsStaleRoutingAndPartitionHeal` `TestMembershipFutureEndpointAndPhysicalReturn` `TestMoveHostKeepsXORAndFencesOldDisk` `TestWriterLearnsJoinAndRoutesNextWriteDirectly` `TestJoinActivatesOnFirstInstallAndForwardsTheRest` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| HND-2 | PARTIAL | retained scenarios; final local campaign passed | `TestHandoffCarriesBulkZones` `TestJoinAfterBulkMovesItsZones` `TestMirrorConcurrentWritesAndLostInstallACK` `TestShadowReceiverResumesDroppedInstallWithoutDonorWorker` `TestLocalTicketsReturnOldCollectorAndAdmissionReplay` `TestLostInstallReplyClosesAfterPhysicalReceiverDeparture` `TestLostInstallReplyClosesAfterLogicalReceiverRetirement` `TestAddressChangeCannotCloseUninstalledReceiverTicket` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| ZONE-1 | PARTIAL | retained scenarios; final local campaign passed | `TestUnchangedWritesAndControlRecords` `TestZONE1_WriteAckedByOneSideDuringMigration` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| ZONE-2 | PARTIAL | retained scenarios; final local campaign passed | `TestFlipUnitsPutChildrenBeforeParentsInBoundedUnits` `TestModel_ZoneTreeHandoff` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| MIR-1 | PARTIAL | retained scenarios; final local campaign passed | `TestDivergentMirrorKeepsSourceWritable` `TestShrinkingAggregateMirrorCanFreezeWithStablePerZoneLag` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| MOVE-1 | PARTIAL | retained scenarios; final local campaign passed | `TestMoveHostKeepsXORAndFencesOldDisk` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| REC-1 | PARTIAL | retained scenarios; final local campaign passed | `TestCausalStartupRefusesReplacementDatabase` `TestIdentityAndFormat` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| AGG-1 | PARTIAL | retained scenarios; final local campaign passed | `TestAcceptChildIgnoresDuplicateAndOlderRevision` `TestBulkIngestReachesTheParentOnce` `TestCausalAggregateRefreshAcrossChildrenAndRestart` `TestCausalJoinerAggregatesFreshWithoutUpdate` `TestCausalSummaryDuplicateContributeDoesNotDoubleCount` `TestCausalSummaryParentRestartPollReconstructs` `TestCausalSummaryPushBatchesDistinctRemoteEdges` `TestCausalSummaryPushCoalescesRemoteParent` `TestCausalSummarySameVNodeFoldsOnUpdate` `TestJoinAfterBulkMovesItsZones` `TestRandomMutationsMatchModel` `TestRepushBoundariesMarksOnlyMovedParentEdges` `TestSummaryRootConvergesAfterConcurrentMeshWrites` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| WIRE-1 | PARTIAL | retained scenarios; final local campaign passed | `TestGetDecodesBlockWithEveryMetric` `TestGetRecordsServesBinaryPayloadsWhenAsked` `TestGetRefusesAnythingButIXB2` `TestGetSetsAnswersIXB2` `TestGetSetsBlockFailureIsNotABody` `TestReadsAndSingleWriterThroughHandoffAndLostMessages` `TestRelayedBlockReadEqualsTheOwners` `TestSetsBlockEdges` `TestSetsBlockKeyPacking` `TestSetsBlockMatchesReferenceGolden` `TestZoneKeysAreTheSameFromOwnerAndRelay` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| DESC-1 | PARTIAL | retained scenarios; final local campaign passed | `TestBulkBatchesFollowTheDescription` `TestCoveredByFindsTheRemoteChildOfALocation` `TestSparseStorageGivesTheSameValuesWhereverItemsAreHeld` `TestTypedCollectionAcrossVnodesMatchesASequentialFold` `TestWritesFollowTheDescription` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| DESC-2 | PARTIAL | retained scenarios; final local campaign passed | `FuzzDecode` `TestCatalogAdmitsVerifiesAndSurvivesARestart` `TestCatalogRefusesASecondDescriptionForAHeldName` `TestDescriptionIsAdmittedOnEveryHostBeforeItIsAcknowledged` `TestNameIsDeterministicAndRoundTrips` `TestVerifyRejectsTamperingAndWrongNames` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| DESC-3 | PARTIAL | retained scenarios; final local campaign passed | `TestAJoiningDiskReceivesTheCatalogWithItsZones` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| DESC-4 | PARTIAL | retained scenarios; final local campaign passed | `TestADiskFetchesAnUnknownDescriptionFromAPeer` `TestContributionsAreFoldedOnlyWithTheirDescription` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| DESC-5 | PARTIAL | retained scenarios; final local campaign passed | `TestCellOrderSortsPrefixFirstWithoutRotation` `TestDescriptionVectorsForTheClient` `TestSchemaRunsAreCompactAndCanonical` `TestSmallBasesRotateWithinTheirOwnCharacters` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| ID-1 | PARTIAL | retained scenarios; final local campaign passed | `TestID1_ChildSharesParentPrefix` `TestID1_EmptyLocationIsRoot` `TestID1_VNodeEncodeDecodeRoundTrip` `TestID1_ZoneIDIs96Bits` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| OWN-1 | PARTIAL | retained scenarios; final local campaign passed | `FuzzViewOwner` `TestOWN1_OwnerIsArgminXOR` `TestOWN1_OwnerMatchesOracleExhaustive8Bit` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| TOPO-1 | PARTIAL | retained scenarios; final local campaign passed | `TestTOPO1_SameActiveSetSameOwner` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| NODE-TOPO-1 | PARTIAL | retained scenarios; final local campaign passed | `TestNODETOPO1_RegistryDoesNotChangeOwner` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| PLAN-1 | PARTIAL | retained scenarios; final local campaign passed | `TestPLAN1_IgnorePlannerPredictions` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| HOT-1 | PARTIAL | retained scenarios; final local campaign passed | `FuzzHotTrie_NoExternalSteal` `TestHOT1_NoExternalSteal` `TestHotSplitAddressFlipsExactlyOneBit` `TestHotSplitAddressRejectsReservedZero` `TestHotTrie_NoExternalStealExhaustive8Bit` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| HOT-2 | PARTIAL | retained scenarios; final local campaign passed | `TestHOT2_ImpossibleSplitHasNoSideEffect` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| REG-1 | PARTIAL | retained scenarios; final local campaign passed | `TestREG1_EveryKeyOutsideRegionUnchanged` `TestREG1_NoChangeOutsideRegion` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| HND-3 | PARTIAL | retained scenarios; final local campaign passed | `TestHND3_SingleWriteLeader` `TestZONE1_WriteAckedByOneSideDuringMigration` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| ACT-1 | PARTIAL | retained scenarios; final local campaign passed | `TestJoinActivatesOnFirstInstallAndForwardsTheRest` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| ACT-2 | PARTIAL | retained scenarios; final local campaign passed | `FuzzACT2_IndependentComponentActivation` `TestACT2_ComponentsCoverValidatedPlan` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| MIG-1 | PARTIAL | retained scenarios; final local campaign passed | `TestModel_ZoneTreeHandoff` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| AUTO-1 | PARTIAL | retained scenarios; final local campaign passed | `TestAUTO1_MoveBeforeHotWhenDiskOnly` `TestAUTO1_NoPressureIsNoop` `TestAUTO1_WritePrefersHotTrieNotMove` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| AUTO-2 | PARTIAL | retained scenarios; final local campaign passed | `TestAUTO2_FarExhaustionWaits` `TestAUTO2_NegativeGrowthDoesNotScale` `TestAUTO2_PositiveGrowthScalesNow` `TestAUTO2_SingleSpikeDoesNotPinGrowth` `TestAUTO2_UnknownGrowthUsesThreshold` `TestAUTO2_ZeroGrowthDoesNotScale` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| NODE-OWN-1 | PARTIAL | retained scenarios; final local campaign passed | `TestNodeRequiresDurableRuntime` `TestReadsAndSingleWriterThroughHandoffAndLostMessages` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| PLAN-2 | PARTIAL | retained scenarios; final local campaign passed | `TestPLAN2_NoPlanDoesNotSpawnSplit` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| READ-1 | PARTIAL | retained scenarios; final local campaign passed | `TestBulkChecksZoneSumsAndFileNames` `TestBulkCorruptBatchLeavesNothing` `TestBulkOverlapIsRefusedUnlessReplace` `TestBulkReplaceMatchesModel` `TestContinuousReadsKeepEarlierACKsDuringJoinsAndPartition` `TestQueryKeepsEarlierACKWhenTransferCompletesDuringSourceInventory` `TestReadsAndSingleWriterThroughHandoffAndLostMessages` `TestSummaryOwnershipRefreshAfterPlacementOnlyMove` `TestWriteIsInvisibleUntilSynced` `TestQueryCoverageFailedHintCannotSatisfyMandatoryReferral` `TestQueryCoverageJSONDistinguishesKnownEmptyFromUnknown` `TestQueryCoverageIgnoresOutOfScopeJSONReferral` `TestQueryCoverageJSONFollowsMandatoryLatestACK` `TestQueryNeverReturnsEmptyDuringPhysicalReturnBeforePlacementPublication` `TestPhysicalMoveCarriesEmptyDelegationToUnawareReader` `TestQueryFollowsAllSourcesOfUninstalledRegions` `TestCancelledWarmReturnKeepsCurrentSourceWithoutOldReceiver` `TestLostInstallReplyClosesAfterLogicalReceiverRetirement` `TestLocalSummaryReadinessCachesControlChanges` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| SCALE-WIRE-1 | PARTIAL | retained scenarios; final local campaign passed | `TestSCALEWIRE1_AutoscaleChangeComesFromValidatedPlan` `TestSCALEWIRE1_UnvalidatedPlanDoesNotChangeActiveSet` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| AUTH-4 | PARTIAL | pure/kernel and retained scenarios | `FuzzLocalPermissionsReplay` `TestBulkCutWithoutControlReceiptIsReconciledAtOpen` `TestKillDuringBulkIsAllOrNothing` `TestKillDuringWritesKeepsAcknowledgedBatches` `TestLocalPermissionsReturnAndCompactedReplay` `TestPowerLossDuringBulkIsAllOrNothing` `TestPowerLossKeepsAcknowledgedBatches` `TestTornLogKeepsAcknowledgedBatches` `TestLocalTicketsReturnOldCollectorAndAdmissionReplay` `TestLocalTicketsCancelledCleanupSurvivesLostReplyAndRestart` `TestReceiverCancellationCrashCutsRetainCleanup` `TestCancelledDiscoveryLeavesNoHistoricalRows` `TestPhysicalReturnUsesOriginalClosedTicketNamespace` `TestOldIssuedFenceCannotBlockReacquiredPointOrNativePermission` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| RETRY-1 | PARTIAL | retained scenarios; final local campaign passed | `TestBulkIngestInstallsOnceAndCountsLikeWrites` `TestBulkStreamAnswersAKnownBatchEarly` `TestUnchangedWritesAndControlRecords` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| CAP-1 | PARTIAL | retained scenarios; final local campaign passed | `TestBulkCutWithoutControlReceiptIsReconciledAtOpen` `TestBulkIngestInstallsOnceAndCountsLikeWrites` `TestBulkOverlapIsRefusedUnlessReplace` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| GC-1 | PARTIAL | retained scenarios; final local campaign passed | `TestCollectTransferredWithoutStagedBytesDoesNotCommit` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| COLD-1 | PARTIAL | retained scenarios; final local campaign passed | `TestAutomaticDurableCompactionAfterInsertions` `TestCausalHTTPCompactionAndRetiredRestart` `TestColdFoldHeatPreemptsAndLostReleaseResumes` `TestColdFoldRecoversEveryMessageCut` `TestColdFoldSingleWriterAtTransferBoundaries` `TestDurableColdFoldAllowsAnotherGrowthCycle` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| COLD-2 | PARTIAL | retained scenarios; final local campaign passed | `TestCOLD2_CoolMeterMeasuresRateNotSilence` `TestCOLD2_FoldRespectsTheItemBound` `TestCOLD2_HeatDuringLastPieceCompletesFold` `TestCOLD2_HeatUnderBudgetStillFolds` `TestCOLD2_SurvivorFractionBoundary` `TestCOLD2_TepidClusterStillCompacts` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| REPL-1 | PARTIAL | retained scenarios; final local campaign passed | `TestREPL1_ReplicaSetOnTheRoutingView` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| REPL-3 | PARTIAL | retained scenarios; final local campaign passed | `TestREPL3_ReplicateSyncsShadowCopies` `TestREPL3_WriteWaitsForOneMember` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| REPL-4 | PARTIAL | retained scenarios; final local campaign passed | `TestREPL4_FallbackWriteRepairAndConvergence` `TestREPL4_TwoReplicasRepairConvergeInEitherOrder` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| GOS-1 | PARTIAL | retained scenarios; final local campaign passed | `TestGOS1_BucketGossipCrossesNetworkWithinTrieDepth` `TestGOS1_EveryRoundContactsCloserToEveryTarget` `TestGOS1_RealJoinsConvergeByBucketGossip` `TestGossipRotationCoversEveryMember` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| SUM-2 | PARTIAL | retained scenarios; final local campaign passed | `TestSUM2_BoundedPollRotatesOverEveryHost` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| PREFIX-1 | PARTIAL | retained scenarios; final local campaign passed | `TestPREFIX1_DescendantsStayInPrefixRegion` `TestPREFIX1_ScopedPrefixReadsMatchAskingEveryone` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| JOIN-1 | PARTIAL | retained scenarios; final local campaign passed | `TestJOIN1_HintedJoinsKeepCellsWithinFactorTwo` `TestJOIN1_RecentHintsForgetJoinedAndExpired` `TestJOIN1_StaleViewHintsKeepCellsWithinFactorTwo` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| NET-2 | PARTIAL | retained scenarios; final local campaign passed | `TestHTTP3OneConnectionCarriesManyConcurrentRequests` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| NET-1 | PARTIAL | retained scenarios; final local campaign passed | `TestInstallFillsTheTransportHooks` `TestMeshCallsHTTPSPeersOverHTTP3` `TestMeshFallsBackToTCPWithoutHTTP3` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| CAP-2 | PARTIAL | retained scenarios; final local campaign passed | `TestCAP2_CutoverRefundsSourceAndChargesReceiver` `TestCAP2_WriteRateCapsAppliedWrites` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| STORE-1 | PARTIAL | retained scenarios; final local campaign passed | `TestMVCCViewDoesNotBlockUpdates` `TestSnapshotAtomicMultiKeyConsistency` `TestSnapshotPointInTimeIsolation` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| STORE-2 | PARTIAL | retained scenarios; final local campaign passed | `TestApplyRecordAdmissionCancellationAndReplay` `TestWriteAdmissionCapacityFailureReleasesBudgetAndReopensReplay` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| XFER-LOT-1 | PARTIAL | retained scenarios; final local campaign passed | `TestAbortCausalRefusesAfterEmit` `TestCutoverWaitWakesOnEndCutoverAndRespectsContext` `TestGranularHandoffResumesFrozenAndLostACKWithoutOverwritingLiveWrites` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| WRITE-HINT-1 | PARTIAL | retained scenarios; final local campaign passed | `TestEntryWriteFollowsAppliedByHint` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| SUM-3 | PARTIAL | retained scenarios; final local campaign passed | `TestSummaryOldContributionPausedBeforeLockCannotOverwriteNewHolder` `TestSummaryPollFloorRejectsDelayedCompleteAcrossReceiverRestart` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| SUM-4 | PARTIAL | pure/kernel and retained scenarios | `TestJoinAfterBulkMovesItsZones` `TestRemoteCessionDoesNotRefoldUnchangedLocalData` `TestRootTotalReadsOneVersionWhileCollectionDeletesHandedRows` `TestZoneSumsMatchTheStore` `TestDerivedReadsFollowOffPlacementNativeDelegation` `TestDerivedParentInvalidatesOffPlacementChildContribution` `TestIncompleteContributionPropagatesAndNewerCompleteRepairs` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| AUTO-3 | PARTIAL | retained scenarios; final local campaign passed | `TestHoldForTransfersFollowsStepsNotTotalDuration` `TestLastTransferStepCountsOnlyCommitsThatWrite` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| SCALE-1 | PARTIAL | retained scenarios; final local campaign passed | `TestSCALE1_DonorNearTheTargetDoesNotLandIt` `TestSCALE1_OneScaleUpInFlightPerVNode` | Existence is checked; final commit and campaign scope are recorded in the evidence map |
| CRDT-1 | PARTIAL | pure/kernel and retained scenarios | `TestMembershipConvergesWithoutHandoffHistory` `TestMembershipRejectsConflictsAtomically` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| CRDT-3 | PARTIAL | pure/kernel and retained scenarios | `TestMembershipConcurrentPublication` `TestMembershipDetachedViewsAndXORRouting` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| AUTH-2 | PARTIAL | pure/kernel and retained scenarios | `TestLocalPermissionsCoalesceAndOverflow` `TestCoalesceLocalRightsAfterReturnPreservesACKsAndClosedTickets` `TestCoalesceLocalRightsRespectsPinnedSourceInputs` `TestPhysicalReceiverCannotRunSourcePointAPIs` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| KNOW-FAST | PARTIAL | pure/kernel and retained scenarios | `TestMembershipWireScheduleNeverWaitsAndClosesFlights` `TestLocalTicketsWritesUseCachedLocalPermissions` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |
| KNOW-LAZY | PARTIAL | pure/kernel and retained scenarios | `TestMembershipWireLostReplyAndReceiverRestart` | See evidence map for passed local disk/HTTP campaigns; no universal proof or latency bound |

## Explicit outstanding obligations

- New ticket storage transactions must pass durable cut/reopen, replay after
  return, first-fence cancellation and old-collector scenarios.
- Fresh reads must require actual regional coverage and every mandatory referral,
  including initial hinted failures that become mandatory later. JSON transport
  must retain coverage/referrals; in-process pointer passing is insufficient.
- The current unit/race suites, vet, finite fuzz and local storage/HTTP campaigns
  passed on the source commit named in the evidence map. These runs remain
  scoped engineering evidence, not a proof of every execution or a latency SLA.
- The 64-peer membership schedule checks compact routing convergence. Actual
  64-disk transfer/fault schedules are separate and must identify their binary,
  seed, faults and assertions.
- No historical AWS throughput, freeze distribution or cost figure is assigned
  to the new runtime. No new AWS run is authorized by this map.

Reproduction and model bounds are in [spec/handoff](../../spec/handoff/README.md).
The [failure model](failure-model.md) retains honest peers, stable storage
incarnations, durable counters and no automatic takeover of lost owning disks.

## Contrôle local et progression des réparations — passe du 25 novembre

Les tests suivants qualifient des propriétés ciblées, sans preuve universelle
de latence ou de vivacité sous un pair définitivement absent :

- `TestLocalControlReusesImmutableMembership` : un changement local ne relit ni
  ne réécrit le blob réseau inchangé.
- `TestMembershipExportsAreDetachedFromInternalSnapshots`,
  `TestMembershipReadsKeepTheirMVCCVersionAcrossMerge`,
  `TestAbortedMembershipDoesNotReplaceCommittedReference` et
  `TestMembershipBlobDigestIsCheckedAfterReopen` : isolation, MVCC et validation
  à froid des versions de membership.
- `TestCellMatchesPairwiseOracleAcrossIndexRebuilds` et
  `TestCellMatchesPairwiseOracleOnDeepAndLiteralViews` : les cellules du trie
  correspondent aux contraintes géométriques indépendamment calculées.
- `TestPollHostInheritsCallerBudgetAndRejectsLateSuccess` et
  `TestCancelledPollRoundsDoNotStarveTheirUnattemptedHosts` : annulation des
  polls et progression des pairs après tours partiels.

Les modèles historiques déplacés dans les tests ne constituent pas une nouvelle
preuve du runtime. Le planificateur réserve les noms du plan validé complet ;
l'ancien DAG ne s'interpose pas avant les tickets de transfert.
