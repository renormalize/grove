# Coherent Rolling Updates — Implementation

This document describes the Coherent update strategy as implemented on the `coherent-updates` branch. It assumes the reader has read [GREP-393](../README.md) and is familiar with PodCliqueSet (PCS), PodClique (PCLQ), PodCliqueScalingGroup (PCSG), PodGang, MVU PodGang (MPG), Tail PodGang, BasePodGang/ScaledPodGang, MinAvailable, and gang scheduling.

## 1. Overview

Coherent updates replace pods one Minimal Viable Unit (MVU) at a time. The mechanism is a state machine in the PCS-replica orchestrator (`podcliquesetreplica` component), driven off two artefacts:

- A **PodGangMap** custom resource — one per PCS replica — that holds the desired PodGang composition for that replica. Every other component reads it; only the PodGangMap component writes to it.
- A new **`PodGangConditionTypeAvailable`** condition on PodGang, set by the PCS PodGang component once the gang scheduling contract is satisfied and `MinReplicas=0` has been released. The orchestrator advances iterations on this condition.

Five components participate per reconcile:

| Component | Owner | Role |
|---|---|---|
| PodGangMap | PCS controller | Computes desired PodGang composition for each PCS replica into `PodGangMap.Spec.Entries`. |
| PodCliqueSetReplica orchestrator | PCS controller | State machine: picks next replica, populates `InFlightPodGangs`, waits for Available, advances. |
| PodGang | PCS controller | Materialises PodGangs from PodGangMap entries, sets `Initialized` and `Available` conditions, releases `MinReplicas`. |
| PCLQ pod | PodClique controller | Creates/deletes pods per PodGangMap entries; handles standalone PCLQ coherent-update reassignment. |
| PCSG podclique | PodCliqueScalingGroup controller | Creates/deletes PCLQs per PodGangMap entries; handles PCSG-replica coherent-update reassignment. |

The PodGang component, PCLQ pod component, and PCSG podclique component all read the PodGangMap and react to it; the orchestrator never mutates PodGang resources or pods directly.

## 2. API Surface

### 2.1 New CRD

| Type | File | Purpose |
|---|---|---|
| `PodGangMap` | `operator/api/core/v1alpha1/podgangmap.go:31` | Per-PCS-replica desired PodGang composition. Cluster-scoped name: `<pcs-name>-<pcs-replica-index>`. |
| `PodGangMapSpec` | `operator/api/core/v1alpha1/podgangmap.go:50` | `PodCliqueSetReplicaIndex` + ordered list of `Entries`. |
| `PodGangEntry` | `operator/api/core/v1alpha1/podgangmap.go:61` | One desired PodGang: name, generation hash, standalone PCLQ pod counts, PCSG replica counts. |
| `PodGangMapList` | `operator/api/core/v1alpha1/podgangmap.go:42` | List type for PodGangMap. |

`PodGangEntry.PodCliques` and `PodGangEntry.PodCliqueScalingGroups` use **unqualified** clique/PCSG names (e.g. `frontend`, not `simple1-0-frontend`).

### 2.2 PCS spec / status additions

| Field | File:line | Purpose |
|---|---|---|
| `CoherentStrategy` constant | `operator/api/core/v1alpha1/podcliqueset.go:131` | New `UpdateStrategyType` value, default for `PodCliqueSetUpdateStrategy.Type` (kubebuilder default `Coherent`). |
| `PodCliqueSetStatus.PodGangCounter` | `operator/api/core/v1alpha1/podcliqueset.go:118` | `map[string]int32` — number of PodGangs created per PCS replica. Key is stringified replica index. Recomputed each reconcile from PodGangMap (`reconcilestatus.go:mutatePodGangCounter`). |
| `PodCliqueSetReplicaUpdateProgress.InFlightPodGangs` | `operator/api/core/v1alpha1/podcliqueset.go:206` | Names of PodGangs the orchestrator is waiting to become `Available`. Drives both observability and the orchestrator's "current iteration" detection. |
| `PodCliqueSetReplicaUpdateProgress.ErrorMessage` | `operator/api/core/v1alpha1/podcliqueset.go:209` | Optional stalled-update reason. |

### 2.3 Per-child status additions

| Field | File:line | Purpose |
|---|---|---|
| `PodCliqueStatus.PodGangMapping` | `operator/api/core/v1alpha1/podclique.go:143` | `map[PodGangName]podCount` — counts pods of this PCLQ per PodGang via the `grove.io/podgang` label. |
| `PodCliqueScalingGroupStatus.PodGangMapping` | `operator/api/core/v1alpha1/scalinggroup.go:111` | `map[PodGangName]replicaCount` — counts PCSG replicas per PodGang. |

Both are populated by their respective `reconcilestatus.go` (`mutatePodGangMapping`) and read by the PodGangMap component during steady-state syncs.

### 2.4 Scheduler API additions

| Symbol | File:line | Purpose |
|---|---|---|
| `PodGangConditionTypeAvailable` | `scheduler/api/core/v1alpha1/podgang.go:169` | Set True after `MinReplicas` is released and all min-replica pods are ready. The orchestrator's advancement gate. |
| `ConditionReasonPodGangAvailable` | `scheduler/api/core/v1alpha1/podgang.go:184` | Reason string for True. |
| `ConditionReasonPodGangNotAvailable` | `scheduler/api/core/v1alpha1/podgang.go:186` | Reason string for not-yet-Available. |

### 2.5 Labels

| Label | File:line | Purpose |
|---|---|---|
| `grove.io/podgang` (`LabelPodGang`) | `operator/api/common/labels.go:34` | Existing per-pod label naming the PodGang the pod belongs to. Set at pod creation. |
| `grove.io/minimum-viable-unit` (`LabelMinimumViableUnit`) | `operator/api/common/labels.go:41` | Marks MVU-shaped PodGangs (containing `minAvailable` of all updated standalone PCLQs/PCSGs). Reserved for future scale-in/out targeting. |
| `LabelComponentNamePodGangMap` | `operator/api/common/labels.go:96` | Component value for PodGangMap resources. |

### 2.6 Naming functions

All in `operator/api/common/namegen.go`:

| Function | Line | Output |
|---|---|---|
| `GeneratePodGangName(pcsName, replicaIndex, pcsGenerationHash, createdPodGangCount)` | 146 | `<pcsName>-<replicaIndex>-<short5HashChars>-<createdPodGangCount>` — unified PodGang name used for new MPGs/Tail-PGs. |
| `GeneratePodGangMapName(pcsNameReplica)` | 153 | `<pcsName>-<replicaIndex>` — PodGangMap resource name. |
| `GenerateBasePodGangName(pcsNameReplica)` | 82 | Legacy BasePodGang name `<pcsName>-<replicaIndex>`, retained for steady-state on existing PCSes. |
| `CreatePodGangNameFromPCSGFQN(pcsgFQN, scaledPodGangIndex)` | 88 | Legacy ScaledPodGang name `<pcsgFQN>-<scaledIndex>`. |
| `GeneratePodGangNameForPodCliqueOwnedByPodCliqueSet` / `…ByPCSG` | 94 / 100 | Legacy lookup used in pre-update topology. |
| `ExtractScalingGroupNameFromPCSGFQN(pcsgFQN, pcsNameReplica)` | 119 | Strips `<pcsName>-<replicaIndex>-` from a PCSG FQN to recover the unqualified PCSG name used as a PodGangMap entry key. |

`podGangNameShortHashLength = 5` (`namegen.go:127`) — the prefix length taken from `Status.CurrentGenerationHash`.

### 2.7 Shared utilities

`operator/internal/controller/common/component/utils/mvu.go`:

- `MVUTemplate` — `{StandalonePCLQs, PCSGs}` map of unqualified-name → minAvailable. (line 27)
- `PodGangEntryBuilder` — closure that emits `PodGangEntry` values with sequentially-numbered names. (line 36)
- `ComputeMVUTemplateFromPCS` (line 39), `GetStandalonePCLQReplicasFromPCS` (line 59), `GetStandalonePCLQMinAvailableFromPCS` (line 47), `GetPCSGReplicasFromPCS` (line 80), `GetPCSGMinAvailableFromPCS` (line 71), `NewPodGangEntryBuilder` (line 90).

`operator/internal/controller/common/component/utils/podgangmap.go`:

- `GetPodGangMap(ctx, cl, name, ns)` (line 28).
- `GetPodGangMapForPCSReplica(ctx, cl, pcsName, ns, replicaIndex)` (line 37).
- `FilterPodGangMapEntriesByGenerationHash(entries, hash)` (line 43).
- `GetPodGangMapEntriesForPCLQ(entries, pclqName)` (line 54).
- `GetPodGangMapEntriesForPCSG(entries, pcsgName)` (line 65).

`operator/internal/controller/common/component/utils/podcliqueset.go`:

- `IsCoherentStrategy` (line 151), `IsCoherentUpdateInProgress` (line 159), `IsRollingRecreateUpdateInProgress` (line 166), `IsOnDeleteStrategy` (line 143), `IsAutoUpdateStrategy` (line 135, deprecated).

## 3. PodGangMap CRD

### 3.1 Role

PodGangMap is the single source of truth for PodGang composition in all three regimes:

1. Steady-state on a new PCS that started under Coherent → MVU-shaped entries from spec.
2. Steady-state on an existing PCS that pre-dates Coherent → BPG/SPG-shaped entries reconstructed from live PodGangs.
3. Coherent update in progress → mixed old-hash and new-hash entries reflecting the current take-down/take-up state.

In all cases, downstream components (PodGang, PCLQ pod, PCSG podclique) derive their work by reading `PodGangMap.Spec.Entries`. They never recompute composition themselves.

### 3.2 Lifecycle

- Created the first time the PCS is reconciled (one PodGangMap per PCS replica), as part of dependency group G0 (see §5).
- Persists across updates and steady state — never deleted unless the PCS itself is deleted (cascade via `controllerutil.SetControllerReference` in `podgangmap.go:buildResource`) or the PCS replica count shrinks (`syncflow.go:syncCoherentUpdateEntries` deletes excess maps for indices ≥ current replica count).
- `Delete()` (`podgangmap.go:96`) is invoked by the finalizer flow and uses `DeleteAllOf` keyed by component selector labels.

### 3.3 Entry structure

```go
PodGangEntry{
  Name:                        "<pcs>-<replica>-<short>-<count>",   // or legacy BPG/SPG name
  PodCliqueSetGenerationHash:  "<hash>",                            // identifies which spec version this PodGang serves
  PodCliques:                  {"frontend": 2, ...},                // unqualified name → pod count (standalone only)
  PodCliqueScalingGroups:      {"prefill": 1, ...},                 // unqualified name → replica count
}
```

Multiple entries can exist with the same generation hash. During an active update both old-hash and new-hash entries coexist — the new-hash entries grow as the update progresses, old-hash entries shrink and disappear.

## 4. Component Flow

Each component below uses a `prepareSyncFlow` (gather state) / `runSyncFlow` (act) split. Entry points are cited; deeper file-level inspection is recommended for full detail.

### 4.1 PodGangMap component

`operator/internal/controller/podcliqueset/components/podgangmap/`

**prepareSyncFlow** (`syncflow.go:51`)
Lists all PCLQs and PCSGs for the PCS, groups them by PCS replica index, and lists existing PodGangMap names. Stores `pclqsByReplica`, `pcsgsByReplica`, `existingPGMNames` on `syncContext`.

**runSyncFlow** (`syncflow.go:118`)
Top-level guard in `Sync` (`podgangmap.go:78`): if `hasInFlightPodGangs(pcs)` is true, return without touching the PodGangMap — the orchestrator is mid-iteration and the entries must remain frozen until `InFlightPodGangs` are Available.

Otherwise dispatches:

- `IsCoherentUpdateInProgress(pcs)` → `syncCoherentUpdateEntries` (`syncflow.go:126`):
  - For each PCS replica, calls `computeCoherentUpdateEntries` (`syncflow.go:168`) which reads existing PodGangMap if present (`getOldAndNewEntries`, line 200), bootstraps from live PodGangs on the first reconcile (`buildOldEntriesFromExistingPodGangs`, line 228), runs one MVU iteration via `computeNextPodGangMapState` (mvu.go:124), and emits the merged set of entries.
  - Deletes excess PodGangMaps for replica indices removed by scale-in.
- Otherwise → `syncSteadyStateEntries` (`syncflow.go:335`):
  - For each PCS replica, if no PodGangMap exists, calls `createPodGangMapForReplica` (line 356) which chooses between BPG/SPG-shape (`buildBaseAndScaledPodGangEntries`, line 372) and MVU-shape (`computeMVUEntriesFromSpec`, line 438) based on `hasMVUPodGangs(pcs)` (true iff `Status.PodGangCounter` is non-empty).
  - If a PodGangMap exists, `buildEntriesFromStatuses` (line 514) reconstructs entries from PCLQ and PCSG `Status.PodGangMapping` fields.

**MVU iteration** (`mvu.go`)

- `computeMVUTemplate` (line 42) computes `mvuTemplate{standalonePCLQs, pcsgs}` by comparing the new template hash against live PCLQ `Status.CurrentPodTemplateHash`. PCSGs are marked updated if any constituent PCLQ changed.
- `computeNextPodGangMapState` (line 124) is the rules engine described in the GREP — at each call it produces either:
  - Exactly one new MVU entry (with optional absorption of remaining standalone PCLQ pods if no further full MVU can form), and decremented old entries (`computeNextMVUPodGang`, line 157), or
  - Zero or more Tail-PG entries (one PCSG replica each) plus emptied old entries (`computeTailPodGangs`, line 196).

**Runtime invariants**

- `InFlightPodGangs != nil` ⇒ PodGangMap is frozen for that reconcile.
- One MVU entry per orchestrator iteration; tail-MVU iteration may create N entries simultaneously.
- Pods/replicas are deducted from the lowest-indexed old entry first (`deductPCLQPodsFromOldEntries` / `deductPCSGReplicasFromOldEntries`).
- Old entries with all counts zero are removed (`removeEmptyEntries`).
- Steady-state recomputation is idempotent: if a PCLQ scaled out between reconciles, the recomputed BPG/SPG/MVU entry counts grow on the next pass.

### 4.2 PodGang component

`operator/internal/controller/podcliqueset/components/podgang/syncflow.go`

**prepareSyncFlow** (line 48)
Lists existing PCLQs, PCSGs, PodGangs, and pods. For every PCS replica reads its PodGangMap via `componentutils.GetPodGangMap` and translates each entry into a `podGangInfo` via `buildPodGangInfoFromEntry` (line 197). The translation:

- `buildStandalonePCLQInfos` (line 214) — for each unqualified standalone clique referenced in the entry, derives the FQN `<pcs>-<replica>-<clique>` and emits a `pclqInfo` with the entry's pod count and the clique-template's `MinAvailable`.
- `buildPCLQInfosAndTopologyConstraintsForPCSGs` (line 235) — for each PCSG referenced in the entry, walks `desiredPCSGReplicas` starting at the running offset (`pcsgReplicaOffset`) so the same PCSG split across two entries gets disjoint replica indices. Emits one `pclqInfo` per constituent clique per replica plus a `TopologyConstraintGroupConfig` per PCSG replica.

The entry-name → `podGangInfo` mapping populates `expectedPodGangByName` and `expectedPodGangNameSet`.

**runSyncFlow** (line 328)

1. `deleteExcessPodGangs` (line 338) — any existing PodGang not in `expectedPodGangNameSet` is deleted.
2. `createOrUpdatePodGangs` (line 363) — for each expected `podGangInfo`:
   - `createOrUpdatePodGang` (line 418) creates/patches the PodGang resource. On creation, `Initialized=False`.
   - `verifyAllPodsCreated` (line 451) ensures all constituent PCLQs exist and all pods carry the right `grove.io/podgang` label.
   - Once verified, `Initialized` is patched to True.
   - `arePodGangMinReplicasReady` (line 520) checks that for each PodGroup, `MinReplicas` pods that are *associated to this PodGang* (membership tracked by `pclqInfo.associatedPodNames`) are ready. This is the load-bearing check that handles standalone-PCLQ pods spread across multiple PodGangs during a coherent update.
   - When ready: `releaseMinReplicasConstraint` (line 546) sets `MinReplicas=0` on every PodGroup of this PodGang via merge patch, then `patchPodGangCondition` sets `PodGangConditionTypeAvailable=True`.

**Runtime invariants**

- The PodGang component is the sole mutator of PodGang `Spec` and `Status`.
- `MinReplicas=0` is released *after* the gang is healthy, not before pod deletion. This protects against the scheduler tearing down the gang when in-flight pod replacement causes a transient breach.
- Excess detection works trivially: PodGangMap is the single source of truth, anything not in it is excess.

### 4.3 PCLQ pod component

`operator/internal/controller/podclique/components/pod/`

**prepareSyncFlow** (`syncflow.go:48`)
Resolves: owning PCS, `isStandalonePCLQ`, `pcsReplicaIndex` (from `LabelPodCliqueSetReplicaIndex`), unqualified `cliqueName`, expected pod template hash, the PCLQ's `associatedPodGangName` from the `grove.io/podgang` label, the PodGang resource (if any), and the PCLQ's existing pods.

**runSyncFlow** (`syncflow.go:186`)

1. `syncExpectationsAndComputeDifference` produces a delta vs `Spec.Replicas`.
2. If `diff < 0` (under-replicated) AND **not** in standalone-PCLQ coherent update → `createPods` → `resolvePodGangNamesForNewPods` → assign each pod to a target PodGang.
3. If `diff > 0` → `deleteExcessPods` (uses `DeletionSorter` on `selectExcessPodsToDelete`, which prioritises pods at the wrong template hash).
4. If standalone-PCLQ coherent update is active (`isStandalonePCLQCoherentUpdate` line 259) → `processCoherentUpdate` (`coherentupdate.go:42`).
5. Else if RollingRecreate update in progress → `processPendingUpdates` (`rollingupdate.go`).
6. `checkAndRemovePodSchedulingGates` (line 425) — only releases the gate once the pod has been added to its PodGang's `PodGroup.PodReferences` (`podNamesUpdatedInPCLQPodGangSet`).

**`resolvePodGangNamesForNewPods`** (`syncflow.go:295`)

- PCSG-owned PCLQ → all new pods go to `sc.associatedPodGangName` (the PCLQ-resource label, set by the PCSG reconciler at creation).
- Standalone PCLQ → reads PodGangMap, calls `assignPodsToDeficitPodGangs` (line 318) which fills entries with deficit > 0 in PodGangMap order. Excess (scale-out beyond the map's totals) goes to the highest-indexed entry referencing this clique (`findHighestMVUPodGangForClique`, line 362).

**`processCoherentUpdate`** (`coherentupdate.go:42`)

1. Reads `InFlightPodGangs` from PCS status; nil → exit.
2. `computeInFlightDeficits` (line 74) — for each in-flight PodGang, `desired (entry) - existing (label-counted)`.
3. `getPodsFromDecrementedPodGangs` (line 100) — every old PodGang where `existing > desired` contributes its pods as deletion candidates.
4. `selectPodsForDeletion` (line 133) — applies `DeletionSorter` to choose `totalDeficit` candidates.
5. Deletes selected pods, then `createPodsForInFlightPodGangs` (line 159) creates replacements with `grove.io/podgang` set per-pod to the assigned in-flight PodGang.

**Runtime invariants**

- Standalone-PCLQ creation logic is bypassed during coherent update — `processCoherentUpdate` owns the lifecycle.
- Replacement pods carry the correct `grove.io/podgang` label at creation time. There is no post-hoc label patching.
- Scheduling gate removal waits for the pod to appear in the PodGang's `PodReferences` — the PodGang component's `Initialized=True` step is the gate.

### 4.4 PCSG podclique component

`operator/internal/controller/podcliquescalinggroup/components/podclique/`

**prepareSyncContext** (`syncflow.go:57`)
Resolves owner PCS, PCS replica index, expected PCLQ FQNs per PCSG replica, existing PCLQs grouped by PCSG replica index (`GroupPCLQsByPCSGReplicaIndex` returns `map[int][]PodClique` and errors on a non-numeric label), MinAvailable-breached indices split by termination delay, and a per-PCLQ expected pod template hash.

**runSyncFlow** (`syncflow.go:184`)
Three phases gated by `isPCSGCoherentUpdateInProgress = IsCoherentUpdateInProgress(pcs) && IsPCSGUpdateInProgress(pcsg)`:

1. **Steady-state replica management** (skipped when coherent update is in progress):
   - `triggerDeletionOfExcessPCSGReplicas` (line 311): replica scale-down by index range.
   - On `OnDelete` strategy → `createOrUpdatePCLQs`, otherwise → `createExpectedPCLQs`.
2. **Update handling**:
   - Coherent update in progress → `processCoherentUpdate` (`coherentupdate.go:42`).
   - RollingRecreate update in progress → `processPendingUpdates`.
3. **Gang termination** (only when no PCSG update is in progress) → `processMinAvailableBreachedPCSGReplicas` (line 446).

**`processCoherentUpdate`** (`coherentupdate.go:42`)

1. Early exit if `CurrentlyUpdating` empty or `InFlightPodGangs` empty.
2. Loads PodGangMap for the PCS replica via `GetPodGangMapForPCSReplica`.
3. Extracts unqualified PCSG name via `ExtractScalingGroupNameFromPCSGFQN`.
4. `computeInFlightPCSGDeficits` (line 95) — `desired - existingReplicaCount` per in-flight PodGang. Existing replicas counted by unique `LabelPodCliqueScalingGroupReplicaIndex` per PodGang via `groupPCSGReplicaIndicesByPodGang` (line 150).
5. `getReplicaIndicesFromDecrementedPodGangs` (line 123) — non-in-flight PodGang entries whose actual replica count > desired contribute their replica indices as deletion candidates.
6. Trim candidates to `min(totalDeficit, len(candidates))`. `deleteReplicasForCoherentUpdate` (line 191) cascades delete of all PCLQs for those PCSG replicas.
7. `createReplicasForInFlightPodGangs` (line 201) reuses the freed indices — `buildReplicaAssignments` (line 222) pairs each in-flight deficit slot with a freed `pcsgReplicaIndex`. For each pairing, `buildPCLQCreationTasks` (line 238) creates one PCLQ per `pcsgConfig.CliqueNames` via `doCreateWithPodGangName` (line 259), which builds the standard PCLQ resource and overrides `Labels[grove.io/podgang]` to the assigned PodGang name.

**Runtime invariants**

- Reused replica indices preserve PCSG replica-index continuity; no need to allocate fresh indices.
- The unqualified PCSG name is the lookup key into `PodGangEntry.PodCliqueScalingGroups` — the FQN-stripping is centralised in `ExtractScalingGroupNameFromPCSGFQN`.
- A non-numeric `LabelPodCliqueScalingGroupReplicaIndex` is treated as a hard error (`errCodeParsePodCliqueScalingGroupReplicaIndex`); the label is controller-managed and corruption indicates a bug.

### 4.5 PodCliqueSetReplica orchestrator

`operator/internal/controller/podcliqueset/components/podcliquesetreplica/`

**prepareSyncFlow** (no separate file — work happens inline in `Sync`)
Computes `delWork` via `getPCSReplicaDeletionWork` (gang-termination/scale-in candidates).

**runSyncFlow** (`podcliquesetreplica.go:64`)
After `delWork.deletionTasks` runs, dispatches by strategy:

- `IsCoherentUpdateInProgress(pcs)` → `orchestrateCoherentUpdate(ctx, logger, pcs, delWork.pcsIndicesToTerminate)` (`coherentupdate.go:47`).
- `IsRollingRecreateUpdateInProgress(pcs)` → `orchestrateRollingRecreateUpdate(...)` (`rollingrecreateupdate.go`).

**`orchestrateCoherentUpdate`** (`coherentupdate.go:47`)

1. `computeCoherentPendingWork` (line 129) lists PCS replicas as `done` or `pending` based on whether all standalone PCLQs and PCSG PCLQs report the new generation hash with sufficient ready replicas (`computeUpdateProgress` in `updateprogress.go:45`).
2. If a replica is `CurrentlyUpdating` → `checkAndAdvanceCoherentUpdate` (line 81).
3. Else pick the lowest-indexed `pending` replica, set it as `CurrentlyUpdating`, patch status, requeue.
4. If no pending, no current → set `UpdateProgress.UpdateEndedAt`, clear `CurrentlyUpdating`, patch status.

**`checkAndAdvanceCoherentUpdate`** (`coherentupdate.go:81`)

- `InFlightPodGangs` empty → `populateInFlightPodGangs` (line 164): reads PodGangMap, takes new-hash entries, picks those whose PodGang is missing or not yet `Available`, writes to status. Returns `ErrCodeContinueReconcileAndRequeue`.
- Else iterates `InFlightPodGangs`, fetches each PodGang, checks `PodGangConditionTypeAvailable=True`. Any not-yet-Available → `ErrCodeContinueReconcileAndRequeue`.
- All Available + replica in `doneReplicaIndices` → `markCurrentReplicaUpdateDone` (`updateprogress.go:121`) sets `UpdateEndedAt`, clears `InFlightPodGangs`.
- All Available but replica not done → clear `InFlightPodGangs`, patch status; next reconcile re-runs the PodGangMap component (now unblocked) to compute the next iteration.

**Runtime invariants**

- The orchestrator never creates pods, never mutates PodGangs, never computes MVU shape. Its only writes are to `Status.UpdateProgress`.
- `InFlightPodGangs` is the synchronisation point between the orchestrator and the PodGangMap component: PodGangMap freezes entries while it is non-empty; orchestrator populates it; PodGangMap unblocks when the orchestrator clears it.
- One PCS replica is updated at a time. The code carries a NOTE about this assumption (`coherentupdate.go:82-84`).

## 5. Coherent Update Lifecycle

### 5.1 Reconcile dependency groups

`reconcilespec.go:getKindSyncGroups` (line 281) groups components into three sequential groups, run in order:

- **G0**: PodGangMap (along with RBAC, headless service, HPA, etc.). PodGangMap must be ready before G1/G2 read it.
- **G1**: PodCliqueSetReplica orchestrator. Reads PodGangMap to populate `InFlightPodGangs`.
- **G2**: PodClique, PodCliqueScalingGroup, PodGang components — run concurrently. All read PodGangMap.

### 5.2 State machine walk-through

Initial state: PCS exists, `Status.UpdateProgress = nil`, `Status.CurrentGenerationHash` set, all replicas at the current hash, PodGangMap entries reflect steady state.

1. **Spec change**: user edits PCS template. PCS reconciler computes new hash in `processGenerationHashChange` (`reconcilespec.go:75`); if the hash differs, `initUpdateProgress` (line 138) sets `Status.UpdateProgress = {UpdateStartedAt: now}`, persists new hash. (`OnDelete` also sets `UpdateEndedAt = now`; `Coherent` and `RollingRecreate` leave it nil.)

2. **Reconcile group G0**: PodGangMap component runs. `IsCoherentUpdateInProgress(pcs) == true` and `InFlightPodGangs` empty (no `CurrentlyUpdating` yet) → `syncCoherentUpdateEntries` runs. For each replica, `computeCoherentUpdateEntries` reads existing PodGangMap entries, classifies them by hash (`getOldAndNewEntries`, line 200), and runs `computeNextPodGangMapState` (`mvu.go:124`) producing the first new-hash MVU entry plus decremented old-hash entries.

3. **Reconcile group G1**: orchestrator runs. `CurrentlyUpdating` empty → `getNextPendingReplicaByIndex` returns lowest-indexed pending replica. Sets `CurrentlyUpdating[0] = {ReplicaIndex, UpdateStartedAt: now}`, patches, requeues.

4. **Next reconcile, G0**: `InFlightPodGangs` still empty → PodGangMap is recomputed (idempotent — same entries).

5. **Next reconcile, G1**: orchestrator sees `CurrentlyUpdating` populated, `InFlightPodGangs` empty → `checkAndAdvanceCoherentUpdate` calls `populateInFlightPodGangs`. It reads PodGangMap, filters by new hash, picks entries whose PodGang is missing or not Available, writes them to `InFlightPodGangs`, requeues.

6. **Next reconcile, G0**: `hasInFlightPodGangs == true` → `Sync` returns early. PodGangMap is frozen.

7. **Reconcile group G2** (runs concurrently each pass):
   - PodGang component: sees the new entries, creates the new PodGang resource (`Initialized=False`), and (for old PodGangs that were decremented) waits for pods to disappear before `verifyAllPodsCreated` permits initialisation.
   - PCLQ pod component: `processCoherentUpdate` for each standalone PCLQ — deletes pods from decremented old PodGangs, creates replacements assigned to in-flight PodGangs.
   - PCSG podclique component: `processCoherentUpdate` for each PCSG — deletes PCSG replicas from decremented entries, creates new PCLQs assigned to in-flight PodGangs.

8. **PodGang reaches Available**: as new pods come up and become ready, the PodGang component's `arePodGangMinReplicasReady` returns true. It calls `releaseMinReplicasConstraint` (sets `MinReplicas=0` on all PodGroups) and patches `PodGangConditionTypeAvailable=True`.

9. **Orchestrator advances**: `checkAndAdvanceCoherentUpdate` sees all `InFlightPodGangs` Available. If the replica is fully updated (every PCLQ/PCSG at the new hash with min-available reached) → `markCurrentReplicaUpdateDone`. Otherwise → clear `InFlightPodGangs`, patch, requeue.

10. **Next iteration**: `InFlightPodGangs` empty unblocks PodGangMap. Steps 2–9 repeat. Once `canFormMVUPodGang` returns false the PodGangMap component switches to `computeTailPodGangs` and emits all remaining tail entries in one pass; the orchestrator then puts all of them in `InFlightPodGangs` simultaneously.

11. **Replica complete**: `markCurrentReplicaUpdateDone` sets `UpdateEndedAt` on the replica, clears `InFlightPodGangs`, leaves `CurrentlyUpdating` populated. Next orchestrator run sees no replica is currently updating *with `UpdateEndedAt == nil`* (`coherentupdate.go:53`) and picks the next pending replica (or finishes).

12. **All replicas complete**: orchestrator sets `Status.UpdateProgress.UpdateEndedAt = now`, clears `CurrentlyUpdating`. The PodGangMap component returns to steady-state mode.

### 5.3 Reconcile-status side effect

`reconcilestatus.go:mutatePodGangCounter` (`podcliqueset/reconcilestatus.go:273`) recomputes `Status.PodGangCounter` from PodGangMap entries matching the current generation hash on every PCS reconcile. This counter feeds `getCreatedPodGangCount` in the PodGangMap component (`syncflow.go:320`), which seeds the `PodGangEntryBuilder` so new PodGang names continue the per-replica numbering across reconciles and updates.

## 6. Watches and Triggers

| Watcher | Triggers on | Enqueues | Source |
|---|---|---|---|
| PCLQ controller, PodGangMap watch | Create + spec Update of PodGangMap (delete skipped) | One reconcile request per standalone PCLQ named in `Entries[*].PodCliques` keys, computed by `mapPodGangMapToPCLQs` | `podclique/register.go:84` |
| PCSG controller, PodGangMap watch | Create + spec Update of PodGangMap (delete skipped) | One reconcile request per PCSG named in `Entries[*].PodCliqueScalingGroups` keys, computed by `mapPodGangMapToPCSGs` | `podcliquescalinggroup/register.go:68` |
| PCLQ controller, PodGang watch | PodGang spec or condition change | Reconcile owning PCLQs (existing) | `podclique/register.go:79` |
| PCSG controller, PCS watch | PCS update — when `CurrentlyUpdating[0].ReplicaIndex` changes | All PCSGs of the currently-updating PCS replica via `mapPCSToPCSG` | `podcliquescalinggroup/register.go:60` |

Both PodGangMap predicates use `ctrlutils.IsManagedPodGangMap` and skip deletes — PodGangMap deletion is driven by PCS deletion, which already cascades to PCLQs/PCSGs/PodGangs via owner references.

## 7. Naming Examples

For a PCS named `my-pcs` with replica index `0`, currentGenerationHash `abcdefg…`, PCSG named `prefill`:

| Function | Output |
|---|---|
| `GeneratePodGangMapName({"my-pcs", 0})` | `my-pcs-0` |
| `GeneratePodGangName("my-pcs", 0, "abcdefg...", 0)` | `my-pcs-0-abcde-0` |
| `GeneratePodGangName("my-pcs", 0, "abcdefg...", 3)` | `my-pcs-0-abcde-3` |
| `GenerateBasePodGangName({"my-pcs", 0})` | `my-pcs-0` (legacy BPG) |
| `GeneratePodCliqueScalingGroupName({"my-pcs", 0}, "prefill")` | `my-pcs-0-prefill` |
| `CreatePodGangNameFromPCSGFQN("my-pcs-0-prefill", 2)` | `my-pcs-0-prefill-2` (legacy SPG) |
| `ExtractScalingGroupNameFromPCSGFQN("my-pcs-0-prefill", {"my-pcs", 0})` | `prefill` |

The legacy `GenerateBasePodGangName` collides numerically with `GeneratePodGangMapName` — the former is a PodGang resource, the latter a PodGangMap; same name, different kinds, no conflict.

## 8. File Change Summary

| File | Role |
|---|---|
| `operator/api/core/v1alpha1/podgangmap.go` | New CRD types: `PodGangMap`, `PodGangMapSpec`, `PodGangEntry`, `PodGangMapList`. |
| `operator/api/core/v1alpha1/podcliqueset.go` | `CoherentStrategy` constant, kubebuilder default `Coherent`, `PodGangCounter`, `InFlightPodGangs`, `ErrorMessage`. |
| `operator/api/core/v1alpha1/podclique.go` | `PodCliqueStatus.PodGangMapping`. |
| `operator/api/core/v1alpha1/scalinggroup.go` | `PodCliqueScalingGroupStatus.PodGangMapping`. |
| `operator/api/common/namegen.go` | `GeneratePodGangName`, `GeneratePodGangMapName`, `ExtractScalingGroupNameFromPCSGFQN`, `podGangNameShortHashLength` constant. |
| `operator/api/common/labels.go` | `LabelMinimumViableUnit`, `LabelComponentNamePodGangMap`. |
| `scheduler/api/core/v1alpha1/podgang.go` | `PodGangConditionTypeAvailable` and reason constants. |
| `operator/internal/controller/common/component/utils/mvu.go` | `MVUTemplate`, `PodGangEntryBuilder`, spec helpers (`ComputeMVUTemplateFromPCS`, `GetStandalonePCLQReplicasFromPCS`, `GetPCSGReplicasFromPCS`, etc.), `NewPodGangEntryBuilder`. |
| `operator/internal/controller/common/component/utils/podgangmap.go` | `GetPodGangMap`, `GetPodGangMapForPCSReplica`, `FilterPodGangMapEntriesByGenerationHash`, entry filters by PCLQ / PCSG. |
| `operator/internal/controller/common/component/utils/podcliqueset.go` | `IsCoherentStrategy`, `IsCoherentUpdateInProgress`, `IsRollingRecreateUpdateInProgress` (and existing helpers). |
| `operator/internal/controller/common/component/utils/podclique.go` | `GroupPCLQsByPCSGReplicaIndex` returns `(map[int][]PodClique, error)` with strict label parsing. |
| `operator/internal/controller/podcliqueset/reconcilespec.go` | `initUpdateProgress` Coherent branch; `getKindSyncGroups` orders PodGangMap into G0. |
| `operator/internal/controller/podcliqueset/reconcilestatus.go` | `mutatePodGangCounter` recomputes counters from PodGangMap each reconcile. |
| `operator/internal/controller/podcliqueset/components/podgangmap/podgangmap.go` | Component scaffolding: `Sync`/`Delete`/`GetExistingResourceNames`, `buildResource`, label helpers. |
| `operator/internal/controller/podcliqueset/components/podgangmap/syncflow.go` | `prepareSyncFlow`, `runSyncFlow`, `syncCoherentUpdateEntries`, `computeCoherentUpdateEntries`, `getOldAndNewEntries`, `buildOldEntriesFromExistingPodGangs`, `syncSteadyStateEntries`, `createPodGangMapForReplica`, BPG/SPG entry builders, `computeMVUEntriesFromSpec`, `computeAllMVUPodGangEntries`, `buildEntriesFromStatuses`. |
| `operator/internal/controller/podcliqueset/components/podgangmap/mvu.go` | `mvuTemplate`, `computeMVUTemplate`, `findUpdatedPodCliques`, `computeNextPodGangMapState`, `computeNextMVUPodGang`, `computeTailPodGangs`, deduction/empty-removal helpers. |
| `operator/internal/controller/podcliqueset/components/podcliquesetreplica/podcliquesetreplica.go` | Strategy dispatch in `Sync`. |
| `operator/internal/controller/podcliqueset/components/podcliquesetreplica/coherentupdate.go` | `orchestrateCoherentUpdate`, `checkAndAdvanceCoherentUpdate`, `computeCoherentPendingWork`, `populateInFlightPodGangs`, `coherentPendingWork`. |
| `operator/internal/controller/podcliqueset/components/podcliquesetreplica/updateprogress.go` | Shared replica-progress helpers: `pcsReplicaInfo`, `computeUpdateProgress`, `getPCSReplicaInfos`, `patchUpdateProgressStatus`, `markCurrentReplicaUpdateDone`. |
| `operator/internal/controller/podcliqueset/components/podgang/syncflow.go` | `computeExpectedPodGangs` reads PodGangMap; `buildPodGangInfoFromEntry`, `buildStandalonePCLQInfos`, `buildPCLQInfosAndTopologyConstraintsForPCSGs`; `arePodGangMinReplicasReady`, `releaseMinReplicasConstraint`; `Initialized` and `Available` condition patches. |
| `operator/internal/controller/podclique/register.go` | PodGangMap watch + `mapPodGangMapToPCLQs` + `podGangMapPredicate`. |
| `operator/internal/controller/podclique/reconcilestatus.go` | `mutatePodGangMapping` derives `Status.PodGangMapping` from pod labels. |
| `operator/internal/controller/podclique/components/pod/syncflow.go` | `prepareSyncFlow` resolves `isStandalonePCLQ`/`pcsReplicaIndex`/`cliqueName`; coherent-update dispatch; `resolvePodGangNamesForNewPods`, `assignPodsToDeficitPodGangs`, `findHighestMVUPodGangForClique`, `countPodsPerPodGang`. |
| `operator/internal/controller/podclique/components/pod/coherentupdate.go` | `processCoherentUpdate`, `computeInFlightDeficits`, `getPodsFromDecrementedPodGangs`, `groupPodsByPodGang`, `selectPodsForDeletion`, `deletePodsForCoherentUpdate`, `createPodsForInFlightPodGangs`, `sumDeficits`. |
| `operator/internal/controller/podcliquescalinggroup/register.go` | PodGangMap watch + `mapPodGangMapToPCSGs` + `podGangMapPredicate`. |
| `operator/internal/controller/podcliquescalinggroup/reconcilestatus.go` | `mutatePodGangMapping` aggregates PodGang labels of constituent PCLQs by PCSG replica; `map[int][]PodClique` keying. |
| `operator/internal/controller/podcliquescalinggroup/components/podclique/syncflow.go` | Three-phase `runSyncFlow` (steady-state / update / gang-termination), strict int-keyed PCSG replica indices, coherent-update guard. |
| `operator/internal/controller/podcliquescalinggroup/components/podclique/podclique.go` | `createDeleteTasks` takes `[]int`; `errCodeGetPodGangMap`. |
| `operator/internal/controller/podcliquescalinggroup/components/podclique/coherentupdate.go` | `processCoherentUpdate`, `computeInFlightPCSGDeficits`, `getReplicaIndicesFromDecrementedPodGangs`, `groupPCSGReplicaIndicesByPodGang`, `countExistingPCSGReplicasPerPodGang`, `deleteReplicasForCoherentUpdate`, `createReplicasForInFlightPodGangs`, `buildReplicaAssignments`, `buildPCLQCreationTasks`, `doCreateWithPodGangName`, `sumPCSGDeficits`. |
| `operator/internal/controller/podcliquescalinggroup/components/podclique/rollingupdate.go` | Drops int↔string round-trips; `isCurrentReplicaUpdateComplete` returns `(bool, error)`. |
