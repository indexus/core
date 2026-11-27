# Verification

**Statut : historique — qualification du prédécesseur, non transférée au nouveau protocole.** Les preuves de chaque règle du registre.

The causal runtime evidence below is separate from the retained kernel tests. A named test is bounded engineering evidence, never a claim about all executions.

Checklist for [invariants.md](../protocol/invariants.md).

A row is one of `PROVED`, `PARTIAL`, `GAP`, `OPERATIONAL`, `EXPERIMENTAL`.

`PROVED` means an engineering proof: a named test checks the safety statement and accepts zero counter-examples. It is not a formal mathematical proof. `PARTIAL` means one piece of the chain is tested, not the whole chain. `GAP` means there is no sufficient proof. `OPERATIONAL` means a behavioural condition, not a safety theorem. `EXPERIMENTAL` means the mechanism is unwired, so the runtime claims nothing.

`Method` says why that status holds: oracle, property, exhaustive-small, fuzz, scenario, race, decision table, wiring, or simulation. A scenario is not an exhaustive proof.

`INDEXUS_LONG=1` widens a `PROVED` row. It never adds a row. Simulation numbers are not rows.

`Blocks` is the claim that cannot be made while the row is not `PROVED`. A `PROVED` row blocks nothing (`—`).

## Causal runtime

| ID | Status | Method | Tests | Limits |
| --- | --- | --- | --- | --- |
| CRDT-1 | PARTIAL | deterministic laws, dependency holes and independent dots | `TestCRDTMergeLawsAndReverseDelivery` `TestFrontierDoesNotAcknowledgeSequenceHoles` `TestConcurrentActivationDotsConvergeWithoutSharedView` | compatible valid histories; not Byzantine equivocation |
| CRDT-2 | PARTIAL | partition/reunion and reordered delivery | `TestCRDTPartitionHealAndDuplicateDelivery` `TestConcurrentJoinsStaleRoutingAndPartitionHeal` `TestCRDT2_LeanActivationsConvergeInAnyOrder` `TestFulfilledIntentsAreDropped` | eventual delivery is an assumption |
| AUTH-1 | PARTIAL | actual disks at every durable cut + independent bounded scheduler | `TestCausalCrashCutsPreserveSingleWriterAndAcknowledgedRows` `TestCausalDivergentViewsExplore` | six disk cuts; 398 model states, not unbounded proof |
| AUTH-2 | PARTIAL | complete partitions and fork rejection | `TestAUTHConcurrentSuccessorAndImpersonationRejected` `TestRegionSubtractConservesDisjointCoverage` | trusted crash-faulty participants |
| AUTH-3 | PARTIAL | divergent routes and shadow refusal | `TestConcurrentJoinsStaleRoutingAndPartitionHeal` `TestCausalInventoryRefusesInstalledShadow` | no cloned concurrently serving disks |
| AUTH-4 | PARTIAL | reopen disks and replay | `TestCausalAuthorityDiskFenceRestartAndReplay` `TestCausalCrashCutsPreserveSingleWriterAndAcknowledgedRows` | no silent rollback |
| ADD-1 | PARTIAL | seeded finite-space property | `TestADD1_ConcurrentAddsOnlyShrinkCells` | 64 sets, 256 keys per set |
| PLAN-CRDT-1 | PARTIAL | current changed-domain inclusion | `TestADD1_ConcurrentAddsOnlyShrinkCells` `TestValidatePlanRejectsExternalStealWithoutExistingRows` | singleton mesh activations; current-holder rebase also exercised by concurrent joins |
| CRDT-3 | PROVED | mutation isolation | `TestCRDT3_CurrentCannotMutateReplica` | — |
| MOVE-2 | PARTIAL | disk fencing and empty incoming reservation | `TestMoveHostKeepsXORAndFencesOldDisk` `TestCausalEmptyPreloadReservesAgainstMove` | same disk incarnation assumptions as AUTH-4 |
| GC-1 | PARTIAL | delayed ACK after authority returns | `TestCausalLateGCDoesNotDeleteReturnedAuthority` | local disk schedule; bounded evidence |
| ROUTE-1 | PARTIAL | stale HTTP route, topology-aware writer, stale entry after a MOVE and piggybacked knowledge | `TestCausalRuntimeHTTPJoinStaleRouteAndRestart` `TestConcurrentJoinsStaleRoutingAndPartitionHeal` `TestWriterLearnsJoinAndRoutesNextWriteDirectly` `TestMoveHostKeepsXORAndFencesOldDisk` | bounded forwarding, unavailable holders cause errors |
| HND-2 | PARTIAL | writes during copy, replay, lost install ACK and late per-zone messages after compaction | `TestMirrorConcurrentWritesAndLostInstallACK` `TestCompletedTransferIsCompactedAndLateZoneMessagesAreNoOps` `TestShadowReceiverResumesDroppedInstallWithoutDonorWorker` `TestCausalCrashCutsPreserveSingleWriterAndAcknowledgedRows` | process restart and message faults, not hardware failure |
| ZONE-1 | PARTIAL | per-zone ACK exclusion during an in-flight MigrationSet | `TestZONE1_HolderOfFollowsZoneTransfer` `TestZONE1_HolderOfZoneIsCollectionScoped` `TestZONE1_WriteAckedByOneSideDuringMigration` `TestZONE1_RegionCertKeepsReceiverWrites` `TestCausalZoneCycleAndCrashCuts` | one ACK authority per zone; not all crash interleavings |
| ZONE-2 | PARTIAL | flip units, deepest first, and parent blocked on descendants | `TestZONE2_ParentFlipsAfterChildren` `TestFlipUnitsPutChildrenBeforeParentsInBoundedUnits` `TestModel_ZoneTreeHandoff` | small trees |
| MIG-1 | PARTIAL | certificate and GC gated on MigrationSet | `TestMIG1_EmitCausalRequiresTransferredZones` `TestMIG1_CollectTransferredAfterMigrationSet` | local disk schedule |
| MIR-1 | OPERATIONAL | refuse freeze on a nondecreasing mirror backlog above the freeze floor; a bounded small backlog freezes and is caught up frozen | `TestDivergentMirrorKeepsSourceWritable` | four-pass progress heuristic with a 256-mutation / 1/20-region floor, not a throughput theorem |
| MOVE-1 | PARTIAL | physical successor and data replay | `TestMoveHostKeepsXORAndFencesOldDisk` | explicit MOVE; logical retirement uses the separate cold fold protocol |
| REC-1 | PARTIAL | disk identity and reopened stores | `TestCausalStartupRefusesReplacementDatabase` `TestCausalAuthorityDiskFenceRestartAndReplay` | total loss of both disk and external identity has no automatic recovery |
| AGG-1 | PARTIAL | versioned child replace, owned sums read from the store at each read (no fold/delta pair to reconcile since the radix went), same-vnode fold on Update, batched remote push/poll, join handoff, partition retains stale totals | `TestAcceptChildIgnoresDuplicateAndOlderRevision` `TestCausalSummarySameVNodeFoldsOnUpdate` `TestCausalAggregateRefreshAcrossChildrenAndRestart` `TestCausalSummaryPushCoalescesRemoteParent` `TestCausalSummaryPushBatchesDistinctRemoteEdges` `TestCausalJoinerAggregatesFreshWithoutUpdate` `TestRepushBoundariesMarksOnlyMovedParentEdges` `TestCausalSummaryDuplicateContributeDoesNotDoubleCount` `TestCausalSummaryParentRestartPollReconstructs` `TestSummaryRootConvergesAfterConcurrentMeshWrites` | advisory cache, float64 metrics, no bounded freshness under partition; without partition the three-node concurrent-write test bounds root convergence at 10 s, but it is probabilistic (it caught the stale-fold race in about half of its runs before the fix) and a write whose summary hook fails before applying is repaired only by the quiet five-minute inventory fold; no MachineID locality |

## Wire

| ID | Status | Method | Tests | Limits |
| --- | --- | --- | --- | --- |
| WIRE-1 | PARTIAL | independent oracle (`testdata/sets_block/reference.py`) byte for byte on both sides, IXB2 the only /sets answer, IXRB records | `TestSetsBlockMatchesReferenceGolden` `TestSetsBlockKeyPacking` `TestSetsBlockEdges` `TestReadsAndSingleWriterThroughHandoffAndLostMessages` `TestGetSetsAnswersIXB2` `TestGetRecordsServesBinaryPayloadsWhenAsked` `TestGetSetsBlockFailureIsNotABody` `TestRelayedBlockReadEqualsTheOwners` (two HTTP nodes: relayed f64 block equals the owner's byte for byte, every metric) `TestZoneKeysAreTheSameFromOwnerAndRelay` (three HTTP nodes: owner summary, owner scan and relays send the same keys and bytes for 17 named zones and the 1- and 2-character zones above 40 random lone records, deep keys included, some naming a location held by another node) `TestGetDecodesBlockWithEveryMetric` `TestGetRefusesAnythingButIXB2`; sdk `src/wire/block.test.mjs` (`golden file basic/edges decodes to its expectation`) | golden vectors `basic`, `edges` (self-key k=0, keys outside the parent, k=2 straddling and k=6 keys, truncation folding, several parents, metric widths 0-5, float32 rounding), `dvf`, and `basic-f64`, `align-f64` (metrics=f64, column padding from the body start), Go encodes them and sdk decodes them from the same files; v1 and IXS1 were removed on 2026-11-14 (no parity check left); IXRB has no golden file yet; float32 metrics lose precision on sums wider than 24 bits |

## Collections

Collection descriptions and typed columns (`docs/protocol/collection-description.md`, `docs/design/typed-columns.md`).

| ID | Status | Method | Tests | Limits |
| --- | --- | --- | --- | --- |
| DESC-1 | PARTIAL | no typed fold without the verified description: writes and IXP1 batches checked against it (width, record depth); the zone value folds live records and remote children in cell order; three injected defects detected (character order, sums, ignored remote children) | `TestWritesFollowTheDescription` `TestBulkBatchesFollowTheDescription` `TestTypedCollectionAcrossVnodesMatchesASequentialFold` `TestSparseStorageGivesTheSameValuesWhereverItemsAreHeld` `TestCoveredByFindsTheRemoteChildOfALocation` | a record written before the admission is legacy and left out of the typed fold if its width differs; a typed zone is read record by record (O(n log n)); three vnodes, 600 records |
| DESC-2 | PARTIAL | admission verified (hash = name), kept on disk, broadcast to every host of the routing view before it is acknowledged; a second description for a held name refused | `TestCatalogAdmitsVerifiesAndSurvivesARestart` `TestCatalogRefusesASecondDescriptionForAHeldName` `TestDescriptionIsAdmittedOnEveryHostBeforeItIsAcknowledged` `TestNameIsDeterministicAndRoundTrips` `TestVerifyRejectsTamperingAndWrongNames` `FuzzDecode` | a host that joins during a broadcast receives it by DESC-3; 96-bit names (collision about 2^48) |
| DESC-3 | PARTIAL | the donor sends its whole catalog before any zone (handoff copy, shadow preload); the test fails without it | `TestAJoiningDiskReceivesTheCatalogWithItsZones` | covered by a test for the join handoff only, not for cold drains or shadow copies |
| DESC-4 | PARTIAL | contributions carry the description digest: fetched from the child when missing, dropped when different, dropped with the description sent to the child when a typed collection arrives without one | `TestContributionsAreFoldedOnlyWithTheirDescription` `TestADiskFetchesAnUnknownDescriptionFromAPeer` | a remote refusal arrives as text (503, not 409) |
| DESC-5 | PARTIAL | readers verify the bytes against the name; Go and the JS client share golden vectors (bytes, names, cell order keys, folds), bit for bit on amd64 and arm64 | `TestDescriptionVectorsForTheClient` `TestSchemaRunsAreCompactAndCanonical` `TestCellOrderSortsPrefixFirstWithoutRotation` `TestSmallBasesRotateWithinTheirOwnCharacters`; sdk `src/tree/description.test.mjs`, `src/walk/columns.test.mjs` | the SDK walk folds ordered columns in cell order inside a zone and reports NaN for a viewport; the heat reads sum columns only |

## Kernel and historical local-model regressions

These tests retain their original narrow statements. They do not define the
causal mesh's distributed authority.

| ID | Status | Method | Tests | Blocks |
| --- | --- | --- | --- | --- |
| ID-1 | PROVED | property | `TestID1_ZoneIDIs96Bits` `TestID1_VNodeEncodeDecodeRoundTrip` `TestID1_EmptyLocationIsRoot` `TestID1_ChildSharesParentPrefix` | — |
| OWN-1 | PROVED | oracle + exhaustive-small + fuzz | `TestOWN1_OwnerIsArgminXOR` `TestOWN1_OwnerMatchesOracleExhaustive8Bit` `FuzzViewOwner` | — |
| TOPO-1 | PROVED | property | `TestTOPO1_SameActiveSetSameOwner` | — |
| NODE-TOPO-1 | PROVED | wiring | `TestNODETOPO1_RegistryDoesNotChangeOwner` | — |
| PLAN-1 | PROVED | property | `TestPLAN1_IgnorePlannerPredictions` | — |
| HOT-1 | PROVED | exhaustive-small + fuzz + canonical address | `TestHOT1_NoExternalSteal` `TestHotTrie_NoExternalStealExhaustive8Bit` `FuzzHotTrie_NoExternalSteal` `TestHotSplitAddressFlipsExactlyOneBit` `TestHotSplitAddressRejectsReservedZero` | — |
| HOT-2 | PROVED | scenario | `TestHOT2_ImpossibleSplitHasNoSideEffect` | — |
| REG-1 | PROVED | exhaustive-small | `TestREG1_NoChangeOutsideRegion` `TestREG1_EveryKeyOutsideRegionUnchanged` | — |
| HND-3 | PARTIAL | scenario | `TestHND3_SingleWriteLeader` `TestZONE1_WriteAckedByOneSideDuringMigration` | mesh-wide single writer per zone |
| MIR-1 | OPERATIONAL | simulation/state | `TestShrinkingAggregateMirrorCanFreezeWithStablePerZoneLag` | universal mirror safety |
| ACT-1 | PARTIAL | property + scenario | `TestACT1_EmptyContextAcceptsEveryHonestProof` `TestACT1_EmptyContextBoundary` `TestACT1_PartialProofActivatesEmptyOrForeignProofDoesNot` `TestACT1_ZoneProofActivates` `TestJoinActivatesOnFirstInstallAndForwardsTheRest` | zone-proof activation through an in-flight MigrationSet under crash |
| ACT-2 | PROVED | property + fuzz | `TestACT2_ComponentsCoverValidatedPlan` `FuzzACT2_IndependentComponentActivation` | — |
| ZONE-1 | PARTIAL | property + scenario | `TestZONE1_HolderOfFollowsZoneTransfer` `TestZONE1_HolderOfZoneIsCollectionScoped` `TestZONE1_WriteAckedByOneSideDuringMigration` `TestZONE1_RegionCertKeepsReceiverWrites` `TestCausalZoneCycleAndCrashCuts` | mesh-wide single writer per zone under arbitrary interleavings |
| ZONE-2 | PARTIAL | scenario | `TestZONE2_ParentFlipsAfterChildren` `TestFlipUnitsPutChildrenBeforeParentsInBoundedUnits` `TestModel_ZoneTreeHandoff` | unbounded trees |
| MIG-1 | PARTIAL | scenario | `TestMIG1_EmitCausalRequiresTransferredZones` `TestMIG1_CollectTransferredAfterMigrationSet` `TestModel_ZoneTreeHandoff` | residual GC across delayed ACKs of concurrent MigrationSets |
| AUTO-1 | PROVED | decision table | `TestAUTO1_NoPressureIsNoop` `TestAUTO1_MoveBeforeHotWhenDiskOnly` `TestAUTO1_WritePrefersHotTrieNotMove` | — |
| AUTO-2 | PROVED | decision table | `TestAUTO2_PositiveGrowthScalesNow` `TestAUTO2_FarExhaustionWaits` `TestAUTO2_ZeroGrowthDoesNotScale` `TestAUTO2_NegativeGrowthDoesNotScale` `TestAUTO2_UnknownGrowthUsesThreshold` `TestAUTO2_SingleSpikeDoesNotPinGrowth` | — |
| NODE-OWN-1 | PARTIAL | durable disk phase probes | `TestReadsAndSingleWriterThroughHandoffAndLostMessages` `TestNodeRequiresDurableRuntime` | bounded causal schedules; legacy local ownership engine removed |
| PLAN-2 | PROVED | wiring | `TestPLAN2_NoPlanDoesNotSpawnSplit` | — |
| READ-1 | PARTIAL | reads through joins, partitions and physical moves | `TestReadsAndSingleWriterThroughHandoffAndLostMessages` `TestContinuousReadsKeepEarlierACKsDuringJoinsAndPartition` `TestSummaryOwnershipRefreshAfterPlacementOnlyMove` `TestQueryKeepsEarlierACKWhenTransferCompletesDuringSourceInventory` | bounded schedules, including source completion during a page and an unavailable receiver; no global instantaneous snapshot or availability guarantee |
| SCALE-WIRE-1 | PARTIAL | wiring | `TestSCALEWIRE1_AutoscaleChangeComesFromValidatedPlan` `TestSCALEWIRE1_UnvalidatedPlanDoesNotChangeActiveSet` | every autoscale mutation of V_active comes from a validated plan |


## Bulk ingest

A staged batch installed in one cut ([storage/bulk.md](../storage/bulk.md)).
It writes no history, no per-record receipt and no sequence, so it leans on
the invariants below instead of on the write path. Every test here runs on
the zonelog engine, which a batch needs.

| ID | Status | Method | Tests | Limits |
| --- | --- | --- | --- | --- |
| AUTH-3 | PARTIAL | the writable authority of the disk for every zone of the batch, under the write lock, against one causal state no transition can change meanwhile; an active vnode after handing its region away, a disk without authority | `TestBulkIntoAZoneOwnedElsewhereIsRefused` `TestBulkIngestReachesTheParentOnce` | one snapshot of the causal state per batch: a long batch delays every transition |
| HND-4 / ZONE-1 | PARTIAL | refused while a transfer, a move or a cold fold is in progress; the fence check above for the rest | `TestBulkIntoAZoneOwnedElsewhereIsRefused` | refusal, not ordering: a batch never races a freeze |
| AUTH-4 | PARTIAL | power cut at any disk operation of a bulk replace on a modelled disk (40 cuts, both outcomes reached); a killed ingesting process with real fsync; the window between the cut and its receipt (a second batch) | `TestPowerLossDuringBulkIsAllOrNothing` `TestKillDuringBulkIsAllOrNothing` `TestBulkCutWithoutControlReceiptIsReconciledAtOpen` | the disk is a model, eight kills; removing the segment sync makes the first fail |
| READ-1 | PARTIAL | nothing of a refused, corrupt, overlapping or mis-summed batch is visible; a replace is exactly the batch, against a brute-force model through later writes, merges and reopen | `TestBulkCorruptBatchLeavesNothing` `TestBulkOverlapIsRefusedUnlessReplace` `TestBulkChecksZoneSumsAndFileNames` `TestBulkReplaceMatchesModel` | readers wait only for the cut, not for the pass |
| RETRY-1 | PARTIAL | the same batch again is a no-op, known from its manifest alone; by content hash, not by sender | `TestBulkIngestInstallsOnceAndCountsLikeWrites` `TestBulkStreamAnswersAKnownBatchEarly` | a batch with any other byte is another batch |
| CAP-1 | PARTIAL | rows counted as writes count theirs, checked before the cut; stored usage equals a recount after ingest, replace, a write over a bulk record, and reconcile | `TestBulkIngestInstallsOnceAndCountsLikeWrites` `TestBulkOverlapIsRefusedUnlessReplace` `TestBulkCutWithoutControlReceiptIsReconciledAtOpen` | a bound one batch may exceed by its spool |
| HND-2 | PARTIAL | a handoff after a batch carries its zones in the frozen snapshot (sequence 0, no history), with a later write; a node that joins after a batch takes its share through the mesh | `TestHandoffCarriesBulkZones` `TestJoinAfterBulkMovesItsZones` | the receiver of the storage test is not an active vnode: its installed zones are read directly |
| AGG-1 | PARTIAL | one fold after the cut; every boundary reaches the remote parent once, in one push round; the root sums the batch once after a join | `TestBulkIngestReachesTheParentOnce` `TestJoinAfterBulkMovesItsZones` | the fold re-reads the boundaries of the node from the store |

## Storage engines

Record rows and control state are in zonelog since 2026-11-14, with no bbolt
file: one zonelog batch per update ([storage/zonelog.md](../storage/zonelog.md)).
The suite runs on zonelog alone. The tests that read bbolt's record layout
went with it (listed in [storage/bulk.md](../storage/bulk.md), engine cut).
The rows below say what the zonelog store alone shows, on one machine
([storage/zonelog.md](../storage/zonelog.md)); with the engine they back the
rows above only for the record rows it now holds.

| ID | Status | Method | Tests | Limits |
| --- | --- | --- | --- | --- |
| AUTH-4 | EXPERIMENTAL | power cut at a random disk operation on a modelled disk (unsynced writes lost or torn, renames and creations undone without a directory sync), three cuts in a row; process kill with real fsync; log cut at any byte | `TestPowerLossKeepsAcknowledgedBatches` `TestKillDuringWritesKeepsAcknowledgedBatches` `TestTornLogKeepsAcknowledgedBatches` | the disk is a model: an fsync that lies, bit rot in synced data and a full disk are not covered |
| HND-2 | EXPERIMENTAL | handoff of a zone by `internal/storage` (staged copy, seal, install) at every durable cut | `TestAuthorityAndCompleteReadsAtEveryDurableHandoffCut` `TestSealedStageRejectsNonEquivalentReplaysBeforeInstall` | zonelog has no handoff calls of its own since November 2026 |
| RETRY-1 | EXPERIMENTAL | receipts of `internal/storage` across restart and a handoff; in zonelog, a write of equal bytes is a no-op | `TestCompactReceiptsPreserveOldResultsAcrossHandoffAndRestart` `TestUnchangedWritesAndControlRecords` | zonelog keeps no request ids |
| ZONE-1 | EXPERIMENTAL | a fence is a control record of the same frame as data | `TestUnchangedWritesAndControlRecords` | the fence rules themselves, read under the write lock, stay in `internal/storage` |
| READ-1 | EXPERIMENTAL | a held fsync: the write is in no get, scan or sum until it returns | `TestWriteIsInvisibleUntilSynced` | one writer |
| AGG-1 | EXPERIMENTAL | zone sums, counts and the one-location rule against a brute-force model through inserts, replaces, deletes, flushes, merges and reopen | `TestRandomMutationsMatchModel` | one disk; float64 sums; remote contributions are outside the store |
| REC-1 | EXPERIMENTAL | identity kept across reopen, another format refused | `TestIdentityAndFormat` | — |
| GC-1 | EXPERIMENTAL | a transferred zone is collected by `internal/storage` and stays out of the sums | `TestCollectTransferredWithoutStagedBytesDoesNotCommit` `TestMIG1_CollectTransferredAfterMigrationSet` | the gate (when a drop is allowed) stays in `internal/storage` |

## Qualification record

See the runtime audit (archived outside the repository) for passing and failing runs at their
actual code snapshots. The simulation fixture now uses the causal serving path
by default. Historical quorum runs are not evidence for CRDT behavior. The old
refactor LONG run had failures; its earlier blanket twenty-scenario claim is not
carried forward. No power-loss, Byzantine, global-snapshot or automatic disk-failover
claim is made here.

## Bounded durable cold compaction

| ID | Status | Scope | Evidence | Remaining scope |
|---|---|---|---|---|
| COLD-1 | PARTIAL | durable pair fold, no global validator | `TestAutomaticDurableCompactionAfterInsertions`, `TestColdFoldRecoversEveryMessageCut`, `TestColdFoldHeatPreemptsAndLostReleaseResumes`, `TestColdFoldSingleWriterAtTransferBoundaries`, `TestCausalHTTPCompactionAndRetiredRestart`, `TestDurableColdFoldAllowsAnotherGrowthCycle` | finite local tests; general balloon executor, permanent failure recovery, mixed versions and extreme-volume latency unqualified |
| COLD-2 | PARTIAL | boundary and defect-injection tests, end-to-end tepid cycle | `TestCOLD2_HeatDuringLastPieceCompletesFold`, `TestCOLD2_SurvivorFractionBoundary`, `TestCOLD2_HeatUnderBudgetStillFolds`, `TestCOLD2_CoolMeterMeasuresRateNotSilence`, `TestCOLD2_TepidClusterStillCompacts` | two to four vnodes; the 45% hysteresis assumes the split trigger (90% of machine capacity) and the survivor's `data_bytes` measure the same disk, as with one vnode per machine; no benchmark scenario yet |

See [cold reconfiguration](../protocol/cold-reconfiguration.md) for admission, permanent ID
exclusion, witness/pin lifecycle and conservative hot preemption. The node binary
enables it by default; skeleton W1 and general automatic balloon execution
remain unwired.

## Causal-only facade refactor, 2026-11-02

`Node` no longer has a local queue/WAL/collection ownership fallback. The node
constructor accepts settings, a contact factory and bootstrap contacts; attach
`mesh.Service` with `EnableCausalCluster` before data APIs. Unconfigured data
operations return `ErrNotReady`. `NewSettings` no longer takes a cache expiration.
`/copy`, `/transfer`, `/leave`, `/queue`, `/checkpoint` and S3 snapshot management
are removed. Durable handoffs use `/cluster/`; logical cold folds were opt-in until COLD-2 (2026-11-15).
Stopping a process preserves its durable vnode state for restart; it does not
claim to drain or safely terminate a physical host.

Tests specific to the removed local engine and WAL were retired, not passed or
translated into causal guarantees. Current causal disk, message cut, partition,
read, aggregation, no-steal and compaction tests remain. Pure kernel and historical
mathematical models are still tests of their own statements, not executable modes.

## Retired 2026-11-13

These rows checked kernel mechanisms that were never wired to the runtime. The
code and its tests were removed; the rows claim nothing any more.

| ID | Former status | Reason |
| --- | --- | --- |
| EXP-1 | PARTIAL | Cold balloon planner (`PlanBalloon`, `BuildTransition`, `ColdReconfiguration`): mécanisme jamais branché au runtime, retiré. |
| HND-2 (kernel row) | PARTIAL | Local `Durable` ACK ledger: mécanisme jamais branché au runtime, retiré; the causal HND-2 row above stays. |
| REC-1 (kernel row) | PROVED | Local topology intent and `ResolveIntent` recovery over `TopologyStore`: mécanisme jamais branché au runtime, retiré; the causal REC-1 row above stays. |
| FWD-1 | PROVED | Kernel forwarding table (`Table`, `Trace`, `RoleFromView`): mécanisme jamais branché au runtime, retiré. |
| FWD-2 | PARTIAL | Kernel forwarding hop limit and cycle check: mécanisme jamais branché au runtime, retiré. |
| TOPO-2 | PARTIAL | `TopologyStore.CAS` stale-digest and generation checks: mécanisme jamais branché au runtime, retiré. |
| TOPO-3 | PROVED | `TopologyStore` copy isolation and CAS-only mutation: mécanisme jamais branché au runtime, retiré; CRDT-3 covers the runtime replica. |
| MOVE-1 (kernel row) | PROVED | Kernel `MoveHost` over the `Machine` ledger and `VNode` states: mécanisme jamais branché au runtime, retiré; the causal MOVE-1 row above stays. |
| HND-1 | PROVED | Kernel `ZoneHandoff` pipeline (`Ready()` is not ownership): mécanisme jamais branché au runtime, retiré. |
| HND-4 | PROVED | Kernel `ZoneHandoff` freeze and `FreezeGate`: mécanisme jamais branché au runtime, retiré. |
| DAG-1 | PROVED | `DAG.ReadyToStart` over the kernel pipeline: mécanisme jamais branché au runtime, retiré; the runtime order (flip units, deepest first) stays covered by ZONE-2. |
| HIER-1 | PROVED | Kernel materialize windows (`AllowMaterialize`, `OnMaterialize`): mécanisme jamais branché au runtime, retiré. |
| MUT-1 | PROVED | Kernel mirror log (`Mutation`, `MirrorApply`, `CoveredThroughF`): mécanisme jamais branché au runtime, retiré. |
| MUT-2 | PROVED | Kernel mirror replay idempotence: mécanisme jamais branché au runtime, retiré. |
| CAP-1 | PROVED | Kernel `Machine` reservation/staging/residual ledger: mécanisme jamais branché au runtime, retiré; the runtime counts capacity in storage. |

Two rows keep their status and lose only their kernel-model tests, for the same reason:
MIR-1 (kernel) drops `TestMIR1_ConvergesWhenDrainExceedsArrival` and
`TestMIR1_DoesNotFreezeWhilePersistentlyDiverging` (`MirrorStats`), ACT-1 drops
`TestACT1_ChangedZonesMustBeReady` (kernel model).
## Linear knowledge target model

Reference model only, not wired into the runtime. See
[linear-knowledge.md](../protocol/linear-knowledge.md) for the statements and the
implementation steps that would make them runtime claims.

| ID | Status | Method | Tests | Limits |
| --- | --- | --- | --- | --- |
| MAP-1 | EXPERIMENTAL | property against the certificate graph | `TestMAP1_FrontierJoinEqualsHistoryUnion` | 8-bit space, 300 random histories, three partial replicas; test-only model |
| MAP-2 | EXPERIMENTAL | scenario | `TestMAP2_ForkIsRefusedByJoin` | one fork at genesis; forks against pruned ancestors are not detected |

## Convergent replication

In progress (`docs/design/convergent-replication.md`); the rows state what the
tests hold today, not a qualified guarantee.

| Invariant | Status | Evidence | Tests | Limit |
| --- | --- | --- | --- | --- |
| REPL-1 | EXPERIMENTAL | after joins, R(z) on every node is the owner then the next closest vnodes, one per machine, and agrees across nodes | `TestREPL1_ReplicaSetOnTheRoutingView` | in-process mesh; no fault during the joins |
| REPL-3 | EXPERIMENTAL | shadow copies on the other members of R(z) reach the owner's sequence and follow later writes; a write acknowledged "replicated" is held by another member, "local" after the timeout when all are cut off | `TestREPL3_ReplicateSyncsShadowCopies` `TestREPL3_WriteWaitsForOneMember` | in-process mesh; no AWS run with replication on |
| REPL-4 | EXPERIMENTAL | non-owner replicas in R(z) gate fallback writes on owner heartbeat suspicion (`T_suspect`), serve degraded reads (`fallback` overlaid on `shadow`), repair onto the owner through deterministic `convergent.Wins` merge regardless of repair order, clear acked fallback logs, and resync shadows | `TestREPL4_FallbackWriteRepairAndConvergence` `TestREPL4_TwoReplicasRepairConvergeInEitherOrder` | in-process mesh; node HTTP degraded-read endpoint and AWS fault campaign not wired yet |

## Anti-entropy partners

Wired in the runtime maintenance loop. Gossip carries knowledge only: no
authority, ACK or data movement depends on who is contacted, so these rows
are liveness and cost claims, not safety claims. See [gossip](../protocol/gossip.md).

| ID | Status | Method | Tests | Limits |
| --- | --- | --- | --- | --- |
| GOS-1 | OPERATIONAL | structural property each round, push-pull spread model, real stores and joins | `TestGOS1_EveryRoundContactsCloserToEveryTarget`, `TestGOS1_BucketGossipCrossesNetworkWithinTrieDepth`, `TestGossipRotationCoversEveryMember`, `TestGOS1_RealJoinsConvergeByBucketGossip` | up to 1024 simulated vnodes, random and spike-shaped tries; 6 real vnodes; bound assumes exchanges succeed; full bundles still sent when a partner's version differs |
| SUM-2 | OPERATIONAL | rotation coverage; A/B on 16 real processes | `TestSUM2_BoundedPollRotatesOverEveryHost` | child push stays the primary path; after an authority change the poll still asks every host for a few ticks |
| PREFIX-1 | EXPERIMENTAL | 20 000 random prefixes; scoped read equal to asking every holder on joined real stores; defect injection | `TestPREFIX1_DescendantsStayInPrefixRegion`, `TestPREFIX1_ScopedPrefixReadsMatchAskingEveryone` | relies on zone ids starting with the location bits; open zone transfers inside the region are asked too |
| JOIN-1 | OPERATIONAL | sequential joins up to 100 vnodes against random ids; 16 real processes; joins faster than gossip, on a view 4 joins behind, keep cells within a factor 2 because a bootstrap remembers the hints it gave (REM-120) | `TestJOIN1_HintedJoinsKeepCellsWithinFactorTwo`, `TestJOIN1_StaleViewHintsKeepCellsWithinFactorTwo`, `TestJOIN1_RecentHintsForgetJoinedAndExpired` | advisory: a joiner that cannot reach a bootstrap falls back to a random cell; joiners asking different bootstraps within one gossip round can still share a cell |
| HANDOFF-STREAM | PARTIAL | paging bound, round resend count, pending retry with one job; defect injection for each; scaling benchmarks | `TestHandoffStreamsPreloadInBoundedPages`, `TestHandoffRoundsResendOnlyMissingZones`, `TestHandoffRetriesJoinTheRunningJob`, `TestScalingSpawnsAreDonorOnly` | the last `stage` still carries the frozen offer in one request (zones mostly moved by then); an abandoned operation keeps its progress map until restart |
| KNOW-LAZY | OPERATIONAL | compact writes preserve ACKs without building or merging global metadata; authority still checked | `TestWriteDoesNotBuildKnowledgeOnCacheMiss` `TestWritePreservesResultWithoutMergingLegacyReply` `TestCompactWriteDoesNotAuthorizeUninstalledReceiver` `TestKnowledgeSyncConcurrencyAndShutdownAreBounded` | full control bundles, legacy peers and background convergence remain; no new AWS result |
| KNOW-FAST | OPERATIONAL | a version request completes while a rebuild holds the lock; defect injection (lock first deadlocks); goroutine dump of a saturated node before and after | `TestKnowledgeVersionDoesNotWaitForARebuild` | the frame is still rebuilt whole when facts change |
| NET-2 | OPERATIONAL | 300 requests held open on one connection; defect injection (the default limit deadlocks); goroutine dump of a saturated load before and after | `TestHTTP3OneConnectionCarriesManyConcurrentRequests` (gateway module) | 4 096 per connection is a constant; flow-control windows stay at quic-go defaults |
| NET-1 | OPERATIONAL | the core/quic build: real QUIC over loopback, TCP-only peer fallback for POST and GET, hooks filled by `meshquic.Install`; 3-node TLS cluster: all mesh calls HTTP/3, audit green | `TestMeshCallsHTTPSPeersOverHTTP3`, `TestMeshFallsBackToTCPWithoutHTTP3`, `TestInstallFillsTheTransportHooks` | only the node built from `quic/` (module of its own); the default node serves HTTPS over TCP without Alt-Svc and calls peers over TCP (`TestNativeHTTPAndTLSKeepAuthenticationAndShutdown`); a peer whose QUIC fails costs one 2 s handshake before TCP; no WAN measurement |
| CAP-2 | OPERATIONAL | applied writes capped within the burst, relays not charged, refill | `TestCAP2_WriteRateCapsAppliedWrites` `TestCAP2_LocalRetryAfterAbortedCutoverSpendsOneToken` | experiments only (`limits.writes_per_second`); one second of burst |
| KNOW-CARRY-1 | OPERATIONAL | concurrent calls through a slow transport; bounded follower wait | `TestKnowledgeCarriedOncePerPeerUnderConcurrentCalls` `TestKnowledgeCarrierWaitIsBounded` | a follower waits for the carrier's whole call, not only its merge; writes and prepared lot steps bypass it (`knowledge_decoupling_test.go`) |
| STORE-1 | PARTIAL | isolation under concurrent commits, merges and flushes; race detector | `TestSnapshotPointInTimeIsolation` `TestSnapshotAtomicMultiKeyConsistency` `TestMVCCViewDoesNotBlockUpdates` | no metric for the age of the oldest snapshot |
| STORE-2 | PARTIAL | group commit with a failing callback, admission refusals | `TestWriteAdmissionCapacityFailureReleasesBudgetAndReopensReplay` `TestApplyRecordAdmissionCancellationAndReplay` | fsync durability is the disk's; `NoSync` stores are test-only |
| STORE-3 | PROVED | decision table over every retired shape, and a disk of this build | `TestOpenRefusesRetiredControlShapes` | — |
| XFER-LOT-1 | PARTIAL | lot cutover with frozen writes, lost ACK and aborted freeze | `TestGranularHandoffResumesFrozenAndLostACKWithoutOverwritingLiveWrites` `TestCutoverWaitWakesOnEndCutoverAndRespectsContext` `TestAbortCausalRefusesAfterEmit` | the 150 ms target is not a bound; no per-case duration of the window without writer |
| WRITE-HINT-1 | PROVED | scripted ACKs: learned, followed, verified by a disk holding nothing, retired by the holder's ACK, unreachable hint dropped with the holder reached in the same call, `Hops > 0` ignores hints | `TestEntryWriteFollowsAppliedByHint` | only the routing; authority is HND-3 |
| SUM-3 | PROVED | deterministic interleaving: old holder paused between its check and the lock while the child moves | `TestSummaryOldContributionPausedBeforeLockCannotOverwriteNewHolder` | — |
| SUM-4 | PROVED | deterministic interleaving: the source's collection commits between the two local reads of the root total; property against the store | `TestRootTotalReadsOneVersionWhileCollectionDeletesHandedRows` `TestZoneSumsMatchTheStore` `TestJoinAfterBulkMovesItsZones` | typed collections: pages of one scan may see several versions (no row twice) |
| AUTO-3 | PROVED | decision table on a fixed clock; empty commits do not count | `TestHoldForTransfersFollowsStepsNotTotalDuration` `TestLastTransferStepCountsOnlyCommitsThatWrite` | — |
| SCALE-1 | OPERATIONAL | controller unit test with defect injection; elastic runs on real processes | `TestSCALE1_OneScaleUpInFlightPerVNode` | patience bound (3 min) is a constant; a definite refusal clears the hold, a timeout keeps it |
