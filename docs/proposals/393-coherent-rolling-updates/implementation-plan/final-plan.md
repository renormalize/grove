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
| `PodGangEntry` | `operator/api/core/v1alpha1/podgangmap.go:61` | One desired PodGang: name, generation hash, standalone PCLQ pod counts, PCSG replica counts, dependency list. |
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
| `grove.io/podcliqueset-replica-index` (`LabelPodCliqueSetReplicaIndex`) | `operator/api/common/labels.go:43` | Set on every PodGang resource at creation/patch (`podgang.go:buildResource`) to record the owning PCS replica index. Read by the PodGangMap component to attribute existing PodGangs to a replica when bootstrapping old entries. Legacy PodGangs (created before this label was stamped) fall back to name parsing via `getPodGangPCSReplicaIndex`. |
| `grove.io/podcliqueset-generation-hash` (`LabelPodCliqueSetGenerationHash`) | `operator/api/common/labels.go:46` | Set on every PodGang resource at creation/patch (`podgang.go:buildResource`) from `pcs.Status.CurrentGenerationHash`. Lets `buildOldEntriesFromExistingPodGangs` distinguish old-hash from current-hash PodGangs without depending on PodGangMap state — old means "label absent or differs from current". |
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
- `PodGangEntryBuilder` — closure that emits `PodGangEntry` values with sequentially-numbered names; takes optional `dependsOn` names that are stamped onto the produced entry. (line 36)
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
  DependsOn:                   []string{"<other-pg-name>", ...},    // PodGangs in this replica that must be scheduled first
}
```

Multiple entries can exist with the same generation hash. During an active update both old-hash and new-hash entries coexist — the new-hash entries grow as the update progresses, old-hash entries shrink and disappear.

`DependsOn` encodes the gate-removal contract uniformly across all PodGang kinds:

| Kind | DependsOn |
|---|---|
| BPG | `nil` |
| SPG | `[BPG name]` |
| MPG | `nil` |
| TailPG | `[all sibling MPG names]` |

Pods in a PodGang with empty `DependsOn` may have their scheduling gates removed once the PodGang resource lists them in `PodGroups[].PodReferences`. Pods in a PodGang with non-empty `DependsOn` additionally wait until each named PodGang is scheduled.

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
  - For each PCS replica, calls `computeCoherentUpdateEntries` (`syncflow.go:168`) which reads existing PodGangMap if present (`getOldAndNewEntries`, line 200), bootstraps from live PodGangs on the first reconcile (`buildOldEntriesFromExistingPodGangs`, line 240, filtering by `getPodGangPCSReplicaIndex` + generation-hash mismatch), runs one MVU iteration via `computeNextPodGangMapState` (mvu.go:124), and emits the merged set of entries. `populateDependsOnForReconstructedEntries` (line 271) then populates `DependsOn` on bootstrapped entries: entries with empty `PodCliques` (SPG/Tail-PG shape) depend on every sibling that has non-empty `PodCliques` (BPG/MPG shape).
  - Deletes excess PodGangMaps for replica indices removed by scale-in.
- Otherwise → `syncSteadyStateEntries` (`syncflow.go:335`):
  - For each PCS replica, if no PodGangMap exists, calls `createPodGangMapForReplica` (line 356) which chooses between BPG/SPG-shape (`buildBaseAndScaledPodGangEntries`, line 372) and MVU-shape (`computeMVUEntriesFromSpec`, line 438) based on `hasMVUPodGangs(pcs)` (true iff `Status.PodGangCounter` is non-empty). Both builders set `DependsOn` directly: SPG entries point at the BPG name; Tail-PG entries point at all sibling MPG names emitted in the same call.
  - If a PodGangMap exists, it is only updated from status when `isPodGangMappingSettled` returns true — that is, when every PCLQ and PCSG in the replica has `PodGangMapping != nil` and `sum(PodGangMapping values) == Spec.Replicas`. When settled, `buildEntriesFromStatuses` (line 514) reconstructs entries from PCLQ and PCSG `Status.PodGangMapping` fields. When not settled (initial deployment, scale-in/out in flight), the existing PGM entries are read back unchanged via `getExistingPGMEntries`, preventing the PGM from being silently emptied.

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
- `buildEntriesFromStatuses` is only invoked when `isPodGangMappingSettled` returns true for all PCLQs and PCSGs in the replica (every `PodGangMapping` is non-nil and its total equals `Spec.Replicas`). When this condition is not met, existing PGM entries are preserved unchanged. This prevents the PGM from being silently emptied during initial deployment or while scale-in/out is in flight, which would corrupt pod-label assignments.
- Steady-state recomputation is idempotent: when all mappings are settled, recomputed BPG/SPG/MVU entry counts converge to the actual pod distribution across PodGangs.

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
| `operator/api/core/v1alpha1/podgangmap.go` | New CRD types: `PodGangMap`, `PodGangMapSpec`, `PodGangEntry` (with `DependsOn`), `PodGangMapList`. |
| `operator/api/core/v1alpha1/podcliqueset.go` | `CoherentStrategy` constant, kubebuilder default `Coherent`, `PodGangCounter`, `InFlightPodGangs`, `ErrorMessage`. |
| `operator/api/core/v1alpha1/podclique.go` | `PodCliqueStatus.PodGangMapping`. |
| `operator/api/core/v1alpha1/scalinggroup.go` | `PodCliqueScalingGroupStatus.PodGangMapping`. |
| `operator/api/common/namegen.go` | `GeneratePodGangName`, `GeneratePodGangMapName`, `ExtractScalingGroupNameFromPCSGFQN`, `podGangNameShortHashLength` constant. |
| `operator/api/common/labels.go` | `LabelMinimumViableUnit`, `LabelComponentNamePodGangMap`, `LabelPodCliqueSetGenerationHash`. |
| `scheduler/api/core/v1alpha1/podgang.go` | `PodGangConditionTypeAvailable` and reason constants. |
| `operator/internal/controller/common/component/utils/mvu.go` | `MVUTemplate`, `PodGangEntryBuilder` (takes `dependsOn` arg), spec helpers (`ComputeMVUTemplateFromPCS`, `GetStandalonePCLQReplicasFromPCS`, `GetPCSGReplicasFromPCS`, etc.), `NewPodGangEntryBuilder`. |
| `operator/internal/controller/common/component/utils/podgangmap.go` | `GetPodGangMap`, `GetPodGangMapForPCSReplica`, `FilterPodGangMapEntriesByGenerationHash`, entry filters by PCLQ / PCSG. |
| `operator/internal/controller/common/component/utils/podcliqueset.go` | `IsCoherentStrategy`, `IsCoherentUpdateInProgress`, `IsRollingRecreateUpdateInProgress` (and existing helpers). |
| `operator/internal/controller/common/component/utils/podclique.go` | `GroupPCLQsByPCSGReplicaIndex` returns `(map[int][]PodClique, error)` with strict label parsing. |
| `operator/internal/controller/podcliqueset/reconcilespec.go` | `initUpdateProgress` Coherent branch; `getKindSyncGroups` orders PodGangMap into G0. |
| `operator/internal/controller/podcliqueset/reconcilestatus.go` | `mutatePodGangCounter` recomputes counters from PodGangMap each reconcile. |
| `operator/internal/controller/podcliqueset/components/podgangmap/podgangmap.go` | Component scaffolding: `Sync`/`Delete`/`GetExistingResourceNames`, `buildResource`, label helpers. |
| `operator/internal/controller/podcliqueset/components/podgangmap/syncflow.go` | `prepareSyncFlow`, `runSyncFlow`, `syncCoherentUpdateEntries`, `computeCoherentUpdateEntries`, `getOldAndNewEntries`, `buildOldEntriesFromExistingPodGangs`, `populateDependsOnForReconstructedEntries`, `getPodGangPCSReplicaIndex`, `collectMPGNamesFromEntries`, `syncSteadyStateEntries`, `createPodGangMapForReplica`, BPG/SPG entry builders, `computeMVUEntriesFromSpec`, `computeAllMVUPodGangEntries`, `buildEntriesFromStatuses`. |
| `operator/internal/controller/podcliqueset/components/podgangmap/mvu.go` | `mvuTemplate`, `computeMVUTemplate`, `findUpdatedPodCliques`, `computeNextPodGangMapState`, `computeNextMVUPodGang`, `computeTailPodGangs`, deduction/empty-removal helpers. |
| `operator/internal/controller/podcliqueset/components/podcliquesetreplica/podcliquesetreplica.go` | Strategy dispatch in `Sync`. |
| `operator/internal/controller/podcliqueset/components/podcliquesetreplica/coherentupdate.go` | `orchestrateCoherentUpdate`, `checkAndAdvanceCoherentUpdate`, `computeCoherentPendingWork`, `populateInFlightPodGangs`, `coherentPendingWork`. |
| `operator/internal/controller/podcliqueset/components/podcliquesetreplica/updateprogress.go` | Shared replica-progress helpers: `pcsReplicaInfo`, `computeUpdateProgress`, `getPCSReplicaInfos`, `patchUpdateProgressStatus`, `markCurrentReplicaUpdateDone`. |
| `operator/internal/controller/podcliqueset/components/podgang/syncflow.go` | `computeExpectedPodGangs` reads PodGangMap; `buildPodGangInfoFromEntry` records `pcsReplicaIndex` on `podGangInfo`; `buildStandalonePCLQInfos`, `buildPCLQInfosAndTopologyConstraintsForPCSGs`; `arePodGangMinReplicasReady`, `releaseMinReplicasConstraint`; `Initialized` and `Available` condition patches. |
| `operator/internal/controller/podcliqueset/components/podgang/podgang.go` | `buildResource` stamps `LabelPodCliqueSetGenerationHash` and `LabelPodCliqueSetReplicaIndex` on every PodGang. |
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

---

## 9. PGM Scale-Out/In Livelock Fix

### Context

The PodGangMap (PGM) component has two related bugs:

1. **Steady-state scale-out livelock**: `isPodGangMappingSettled` blocks PGM updates when `sum(PodGangMapping) < Spec.Replicas`, but pods cannot be created without a resolvable PGM entry. The PCSG/PCLQ reconcilers requeue forever.
2. **Scale-in/out during coherent update** is fundamentally complex (stale entries, in-flight unsatisfiable PodGangs, deletion non-determinism, orphaned pods, mapping staleness). Every in-place handling approach introduces new edge cases.

### Decision

**Block scale-in and scale-out via validating admission webhooks while a coherent update is in progress.** The block applies to PCSG and PCLQ `Spec.Replicas` only; PCS `Spec.Replicas` (top-level replica scaling) is allowed at any time. Within steady state, redefine `PodGangMapping` on both PCLQ and PCSG status to be a **decision-driven, persistent map** of PodGang→count owned by the PCLQ pod component / PCSG PCLQ component. PGM is the source of truth during a coherent update; the PCLQ/PCSG status mappings are the source of truth in steady state.

**Direction of authority by mode** (mode = `IsCoherentUpdateInProgress(pcs)`):
- **Coherent update iteration in progress** (`InFlightPodGangs` non-empty): PGM drives. PCLQ/PCSG reconcilers overwrite their status mappings from PGM each reconcile. PGM is used to orchestrate the migration — entries are recomputed iteratively (one MPG per reconcile, then TailPGs).
- **Steady state and RollingRecreate** (`InFlightPodGangs` empty): PCLQ/PCSG status mappings drive. PGM component follows: per-entry `PodCliques[cliqueName]` and `PodCliqueScalingGroups[pcsgName]` are derived from the mappings. PGM is kept up-to-date but **not recomputed structurally** — existing PodGang entries are preserved. RollingRecreate updates pods in place via individual PCLQ/PCSG resource deletion; PGM entries don't move.

**`pcs.Status.PodGangCounter` ownership and naming conventions**:

PodGangCounter on PCS status serves two purposes:
1. Source of the counter segment in PodGang names minted **during a coherent update** (`<pcs-name>-<pcs-replica-index>-<pcshash>-<counter>`).
2. Observability of total PodGang count per PCS replica.

By mode:
- **Coherent update**: PGM component mints MPG and TailPG names using `pcs.Status.PodGangCounter`. The PCS reconciler's `mutatePodGangCounter` keeps it consistent with PGM contents.
- **Steady state PCSG scale-out**: PCSG reconciler mints **Scaled-PG** entries (one per new replica). Names use a different convention: `<pcs-name>-<pcs-replica-index>-<pcshash>-<pcsgname>-<counter>`. The next counter value is **derived on the fly** from existing keys in `pcsg.Status.PodGangMapping` matching the prefix `<pcs-name>-<pcs-replica-index>-<pcshash>-<pcsgname>-`: parse the trailing integer of each, take max+1. No separate counter field is stored. `pcs.Status.PodGangCounter` is updated for observability by `mutatePodGangCounter` recounting PGM, but **not** consulted to mint Scaled-PG names.

This convention also distinguishes Scaled-PGs (steady-state mint, `<pcsgname>` segment in name) from MPGs/TailPGs (coherent-update mint, no `<pcsgname>` segment).

Eliminates from earlier designs: trim functions during coherent update, `PodGangAssignment` typing, `ObservedGeneration` guard, in-flight entry patching, orphaned-pod cleanup, deduction-order rules, scale-out delta in coherent update path.

### Design

#### Reconciler ownership

| Resource | Component | Role |
|----------|-----------|------|
| PGM | PGM component (PCS reconciler, G0) | Coherent update: mint entries iteratively. Steady state: follower — per-entry counts come from PCLQ/PCSG status mappings; new Tail-PG entries appear by mirroring PCSG status. |
| `pclq.Status.PodGangMapping` | PCLQ reconciler pod component | Per-PodGang pod count for this PCLQ. Maintained on scale-in/out decisions. |
| `pcsg.Status.PodGangMapping` | PCSG reconciler PCLQ component | Per-PodGang replica count for this PCSG. Maintained on scale-in/out decisions. New Tail-PG names minted here in steady state. |
| PodGang resource | PodGang component (PCS reconciler, G2) | Created from PGM entries. Unchanged. |

#### `PodGangMapping` semantics (both PCLQ and PCSG)

```go
PodGangMapping map[string]int32   // key: PodGang name, value: pod/replica count
```

Invariant when settled: `sum(values) == Spec.Replicas`. The PGM component's settled guard checks this before consuming the mapping.

The mapping persists across reconciles. It is **not** derived from live pods — it represents the latest decision. Pod crashes do not perturb it (live count drops below mapped value → reconciler creates a replacement pod under the same PodGang).

#### PCLQ reconciler pod component flow (standalone PCLQs only)

`pclq.Status.PodGangMapping` captures the desired state for pod→PodGang assignment maintained by the PCLQ reconciler. It is the source of truth in every situation except when a coherent update iteration is in progress (i.e. `IsCoherentUpdateInProgress(pcs)`).

```
Reconcile() {
    if coherent update is in progress (IsCoherentUpdateInProgress(pcs)) {
        // update pclq.Status.PodGangMapping from ALL PGM entries (both old-hash and new-hash);
        // sets entry name → entry.PodCliques[cliqueName] for each entry that has the clique
    } else {
        // Steady state
        if len(pclq.Status.PodGangMapping) == 0 {
            // initialize pclq.Status.PodGangMapping from ALL PGM entries (initial seed)
        }
        diff := pclq.Spec.Replicas - sum of pod counts across all PodGangs in pclq.Status.PodGangMapping
        if diff > 0 {  // scale-out
            // add `diff` to the highest-index PodGang in pclq.Status.PodGangMapping
        } else if diff < 0 {  // scale-in
            // run the deletion sorter to choose `|diff|` pods to delete;
            // decrement pclq.Status.PodGangMapping per chosen pod's LabelPodGang
        }
    }

    // At this point pclq.Status.PodGangMapping holds the desired pod distribution.
    // Reconcile actual pods to desired:
    currentPodGangMapping := pod-PodGang mapping built from live (non-terminating, DeletionTimestamp == nil) pods using LabelPodGang
    desiredPodGangMapping := pclq.Status.PodGangMapping
    // For each PodGang in desired:
    //   delta = desired[PG] - current[PG]
    //   delta > 0: create `delta` pods labeled PG
    //   delta < 0: delete `|delta|` pods from PG (via deletion sorter scoped to PG)
}
```

Notes:
- No function names are prescribed; the comments express intent.
- Pod crashes don't perturb status: live count drops below desired for the affected PG → reconciler creates a replacement pod under the same PG.

#### PCSG reconciler PCLQ component flow

`pcsg.Status.PodGangMapping` captures the desired state for PCSG replica→PodGang assignment maintained by the PCSG reconciler. It is the source of truth in every situation except when a coherent update iteration is in progress.

The next Scaled-PG counter is **derived on the fly** from existing keys in `pcsg.Status.PodGangMapping` matching the Scaled-PG prefix `<pcs-name>-<pcs-replica-index>-<pcshash>-<pcsgname>-` — parse trailing integer from each, take max+1. No separate counter field is stored on PCSG status; the mapping itself carries the necessary state. Format: `<pcs-name>-<pcs-replica-index>-<pcshash>-<pcsgname>-<counter>`. Each PCSG owns its own naming space (PCSG name is part of the prefix), so multiple PCSGs in the same PCS replica derive their counters independently without races.

It is essential that the desired state is computed and patched into `pcsg.Status.PodGangMapping` **before** the create/delete reconciliation step runs, so that the desired state is the single source of truth for that step.

```
Reconcile() {
    if coherent update is in progress (IsCoherentUpdateInProgress(pcs)) {
        // update pcsg.Status.PodGangMapping from ALL PGM entries (both old-hash and new-hash);
        // sets entry name → entry.PodCliqueScalingGroups[pcsgName] for each entry that has the PCSG
    } else {
        // Steady state
        if len(pcsg.Status.PodGangMapping) == 0 {
            // initialize pcsg.Status.PodGangMapping from ALL PGM entries
        }
        // Update the desired state in pcsg.Status.PodGangMapping before the reconciliation step.
        diff := pcsg.Spec.Replicas - sum of replica counts across all PodGangs in pcsg.Status.PodGangMapping
        if diff > 0 {  // scale-out
            // PCSG scale-out in steady state mints Scaled-PG entries (one per new replica).
            // Each Scaled-PG name: <pcs-name>-<pcs-replica-index>-<pcshash>-<pcsgname>-<counter>
            // Derive next counter: scan existing keys in pcsg.Status.PodGangMapping with the
            // Scaled-PG prefix, parse trailing integer, take max+1. No separate counter field.
            // Each entry: {newScaledPGName: 1}; add to pcsg.Status.PodGangMapping.
        } else if diff < 0 {  // scale-in
            // Two-tier deletion order to pick `|diff|` PCSG replica indices to remove:
            //
            // Tier 1 — ScaledPGs (steady-state mints, name contains <pcsgname> segment).
            //          Sort descending by trailing-integer counter parsed from the Scaled-PG name; each holds 1 replica → decrement entry to 0.
            // Tier 2 — MPGs and TailPGs (coherent-update mints, no <pcsgname> segment in name).
            //          Sort descending by counter; each step decrements that entry's count by 1.
            //          TailPGs (1 replica each) zero out first; MPGs decrement by 1 per step.
            //
            // Walk Tier 1 first; when exhausted, walk Tier 2 until `|diff|` is satisfied.
            // No per-PodGang MinReplicas constraint to honor — once a PodGang is Available,
            // its MinReplicas is set to 0 (already implemented). Breach of MinAvailable is
            // checked at the PCS level, not at individual PodGang level.
            // Empty entries (count = 0) are dropped by removeEmptyEntries when PGM is rewritten.
        }
    }

    // At this point pcsg.Status.PodGangMapping holds the desired PodGang→PCSG-replica distribution.
    // Reconcile actual PCLQs to desired:
    currentPodGangMapping := PodGang→PCSG-replica mapping built from live (non-terminating, DeletionTimestamp == nil) PCLQs (counted by LabelPodGang on each PCLQ)
    desiredPodGangMapping := pcsg.Status.PodGangMapping
    // For each PodGang in desired:
    //   delta = desired[PG] - current[PG]
    //   delta > 0: create `delta` PCLQs labeled PG (one per PCSG replica index covered by this PG)
    //   delta < 0: delete `|delta|` PCLQs from PG
}
```

Notes:
- No separate counter field on PCSG status. The next Scaled-PG counter is derived on the fly from existing keys in `pcsg.Status.PodGangMapping` matching the Scaled-PG prefix.
- `pcs.Status.PodGangCounter` is **not** consulted in steady state for minting Scaled-PG names. It continues to be updated by the PCS reconciler's `mutatePodGangCounter` for observability (recomputed from PGM).
- The new Scaled-PG names appear in `pcsg.Status.PodGangMapping` first; the PGM component, on its next sync, adds matching entries to PGM.
- Status patch happens before the create/delete reconciliation step — no chicken-and-egg between status and PCLQ creation.

#### PGM component `syncSteadyStateEntries` (follower)

```
for each PCS replica:
    entries = read existing PGM (or empty list if first time)
    
    // 1. Mirror new entries from PCSG status mappings (Scaled-PGs minted by PCSG reconciler that aren't in PGM yet).
    for each PCSG:
        for each name in pcsg.Status.PodGangMapping not present in entries:
            create new entry:
                Name = name
                PodCliqueSetGenerationHash = pcs.Status.CurrentGenerationHash
                PodCliques = nil
                PodCliqueScalingGroups = {pcsgName: pcsg.Status.PodGangMapping[name]}
                DependsOn = collectMPGNamesFromEntries(entries)  // Scaled-PG depends on existing MPGs (or BasePG for non-MVU PGMs)
            append to entries
    
    // 2. Apply status authority for per-entry counts.
    //    No settled guard needed: PodGangMapping is the desired state, written atomically by the
    //    reconciler in a single status patch with sum == Spec.Replicas. Empty mapping (nil/len==0)
    //    means the reconciler hasn't initialized yet for that resource → skip that source.
    for each entry:
        for each standalone PCLQ:
            if pclq.Status.PodGangMapping is non-empty:
                entry.PodCliques[cliqueName] = pclq.Status.PodGangMapping[entry.Name]   // 0 if absent
        for each PCSG:
            if pcsg.Status.PodGangMapping is non-empty:
                entry.PodCliqueScalingGroups[pcsgName] = pcsg.Status.PodGangMapping[entry.Name]   // 0 if absent
    
    // 3. Drop entries with all-zero counts (handles scale-in cleanup).
    entries = removeEmptyEntries(entries)
    
    write PGM
```

For initial PCS replica creation (no PGM exists, no PCLQ/PCSG status yet), fall through to existing `createPodGangMapForReplica` which builds entries from spec.

For non-MVU base PGMs (initial creation via `buildBaseAndScaledPodGangEntries`), Scaled-PGs depend on the BasePG (already the case today). For MVU PGMs (post-coherent-update), Scaled-PGs depend on all MPGs. `collectMPGNamesFromEntries` correctly returns either case (entries with empty `DependsOn` — BasePG or MPG).

#### Coherent update path: unchanged

`computeCoherentUpdateEntries`, `computeNextPodGangMapState`, `computeNextMVUPodGang`, `computeTailPodGangs`, `InFlightPodGangs` lifecycle — all unchanged. PGM mints names via `NewPodGangEntryBuilder` with `pcs.Status.PodGangCounter`. The PCLQ/PCSG reconcilers reconcile their status to PGM in step 3 of their flows.

#### Webhook block on scaling during coherent update

Validating admission webhooks reject `Spec.Replicas` changes on **PCSG and PCLQ** when `IsCoherentUpdateInProgress(pcs)`. PCS `Spec.Replicas` is not blocked — top-level PCS replica scaling is orthogonal to the coherent-update migration within a single PCS replica. Reuses existing webhook infrastructure.

#### Delete the broken machinery

- `isPodGangMappingSettled` / `isPCLQMappingSettled` / `isPCSGMappingSettled` — replaced by inline `sum == Spec.Replicas` check in PGM consumer.
- `buildEntriesFromStatuses` / `getOrCreateEntry` — replaced by the new follower logic.
- `mutatePodGangMapping` in both PCSG `reconcilestatus.go` and PCLQ `reconcilestatus.go` (was live-pod-derived; replaced by decision-driven writes in pod/PCLQ components).

---

### Scenario Walkthrough

Baseline:
- PCS replica 0
- 1 standalone PCLQ `pca`, `Spec.Replicas=5`, `MinAvailable=2`
- 1 PCSG `sg`, `Spec.Replicas=4`, `MinAvailable=2`
- All MPGs have `nil` DependsOn. Tail-PGs have `DependsOn=[mpgNames...]`.

Steady-state PGM (hash `xyz`):
```
MPG-xyz-0: PodCliques={pca:2}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
MPG-xyz-1: PodCliques={pca:3}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
```

Status mappings (settled):
```
pca.Status.PodGangMapping = {MPG-xyz-0: 2, MPG-xyz-1: 3}
sg.Status.PodGangMapping  = {MPG-xyz-0: 2, MPG-xyz-1: 2}
```

---

#### Case 1 — PCSG scale-out in steady state (4 → 6 replicas)

`sg.Spec.Replicas = 6`. Webhook allows (steady state).

**PCSG reconciler PCLQ component**:
1. Reads live PCLQs (4 PCLQs covering replica indices 0–3).
2. Reads `sg.Status.PodGangMapping = {MPG-xyz-0: 2, MPG-xyz-1: 2}`. sum=4.
3. totalDeficit = 6 - 4 = 2.
4. sum(4) < Spec.Replicas(6). Mint 2 new Tail-PG names using `pcs.Status.PodGangCounter` (assume next is 2):
   ```
   TailPG-xyz-2, TailPG-xyz-3
   sg.Status.PodGangMapping = {MPG-xyz-0: 2, MPG-xyz-1: 2, TailPG-xyz-2: 1, TailPG-xyz-3: 1}
   pcs.Status.PodGangCounter[0] = 4
   ```
   Patch.
5. diff per entry: TailPG-xyz-2 needs 1 PCLQ for sg replica index 4; TailPG-xyz-3 needs 1 for index 5.
6. Creates PCLQs for indices 4 and 5, labeled `LabelPodGang=TailPG-xyz-2` and `TailPG-xyz-3`.

**PGM component** on next sync:
1. Sees TailPG-xyz-2 and TailPG-xyz-3 in `sg.Status.PodGangMapping` not in PGM → adds them with `PodCliqueScalingGroups={sg:1}`, `DependsOn=[MPG-xyz-0, MPG-xyz-1]`, hash=`xyz`.
2. Existing entries' counts unchanged (mappings unchanged for them).

**Final PGM:**
```
MPG-xyz-0:    PodCliques={pca:2}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
MPG-xyz-1:    PodCliques={pca:3}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
TailPG-xyz-2: PodCliqueScalingGroups={sg:1}, DependsOn=[MPG-xyz-0, MPG-xyz-1]
TailPG-xyz-3: PodCliqueScalingGroups={sg:1}, DependsOn=[MPG-xyz-0, MPG-xyz-1]
```

**Outcome**: New entries exist in PGM; PodGang component creates `TailPG-xyz-2` and `TailPG-xyz-3` resources; pods can be scheduled. No livelock.

---

#### Case 2 — Standalone PCLQ scale-out in steady state (5 → 7 pods)

`pca.Spec.Replicas = 7`. Webhook allows.

**PCLQ reconciler pod component**:
1. Reads live pods (5).
2. Reads `pca.Status.PodGangMapping = {MPG-xyz-0: 2, MPG-xyz-1: 3}`. sum=5.
3. totalDeficit = 7 - 5 = 2.
4. sum(5) < Spec.Replicas(7). Highest-index PG = MPG-xyz-1. Increment by 2:
   ```
   pca.Status.PodGangMapping = {MPG-xyz-0: 2, MPG-xyz-1: 5}
   ```
   Patch.
5. diff: MPG-xyz-1 has 3 live, status says 5 → create 2 pods labeled `MPG-xyz-1`.

**PGM component** on next sync:
1. No new entries in mappings.
2. Per-entry counts: `MPG-xyz-1.PodCliques["pca"] = pca.Status.PodGangMapping["MPG-xyz-1"] = 5`. Updated.

**Final PGM:**
```
MPG-xyz-0: PodCliques={pca:2}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
MPG-xyz-1: PodCliques={pca:5}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
```

**Outcome**: PGM matches actual distribution. No livelock.

---

#### Case 3 — PCSG scale-in in steady state (4 → 2 replicas)

`sg.Spec.Replicas = 2`. Webhook allows.

**PCSG reconciler PCLQ component**:
1. Reads live PCLQs (4).
2. Reads `sg.Status.PodGangMapping = {MPG-xyz-0: 2, MPG-xyz-1: 2}`. sum=4.
3. totalDeficit = 2 - 4 = -2.
4. sum(4) > Spec.Replicas(2). Trim from highest-index entries: MPG-xyz-1 (replica indices 2, 3 are highest). Decrement by 2:
   ```
   sg.Status.PodGangMapping = {MPG-xyz-0: 2, MPG-xyz-1: 0}
   ```
   Patch.
5. diff: MPG-xyz-1 has 2 live, status says 0 → delete 2 PCLQs from MPG-xyz-1.

**PGM component** on next sync:
1. Per-entry counts: `MPG-xyz-1.PodCliqueScalingGroups["sg"] = 0`.
2. After update: MPG-xyz-1 has `pca:3, sg:0`. Not all zero → kept by `removeEmptyEntries`.

**Final PGM:**
```
MPG-xyz-0: PodCliques={pca:2}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
MPG-xyz-1: PodCliques={pca:3}, PodCliqueScalingGroups={sg:0}, DependsOn=nil
```

**Outcome**: PGM reflects the 2 PCSG replicas. `resolvePodGangNameFromPGM` for indices 0, 1 walks: MPG-xyz-0 covers both. Index ≥ 2 is not consulted.

---

#### Case 4 — Standalone PCLQ scale-in in steady state (5 → 3 pods)

`pca.Spec.Replicas = 3`. Webhook allows.

**PCLQ reconciler pod component**:
1. Reads live pods (5).
2. Reads `pca.Status.PodGangMapping = {MPG-xyz-0: 2, MPG-xyz-1: 3}`. sum=5.
3. totalDeficit = 3 - 5 = -2.
4. sum(5) > Spec.Replicas(3). Run deletion sorter, suppose 1 pod from MPG-xyz-0 and 1 from MPG-xyz-1 selected.
   ```
   pca.Status.PodGangMapping = {MPG-xyz-0: 1, MPG-xyz-1: 2}
   ```
   Patch.
5. diff: MPG-xyz-0 (2 live → 1) delete 1; MPG-xyz-1 (3 live → 2) delete 1.

**PGM component** on next sync:
1. Per-entry counts: `MPG-xyz-0.PodCliques["pca"] = 1`, `MPG-xyz-1.PodCliques["pca"] = 2`.

**Final PGM:**
```
MPG-xyz-0: PodCliques={pca:1}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
MPG-xyz-1: PodCliques={pca:2}, PodCliqueScalingGroups={sg:2}, DependsOn=nil
```

**Outcome**: PGM reflects post-scale-in distribution.

**Pod-crash robustness**: if a pod dies after the patch, kubelet recreates it via PCLQ reconciler. `local-PGMapping` shows fewer pods than `status-PGMapping`; diff > 0 for the affected PG; reconciler creates a replacement labeled with the same PG. Status mapping unchanged.

---

#### Case 5 — Scale-in or scale-out during coherent update (rejected for PCSG/PCLQ; allowed for PCS)

User attempts `Spec.Replicas` change on PCSG or PCLQ while `IsCoherentUpdateInProgress(pcs)`.

**Validating webhook** rejects:
```
spec.replicas changes are not allowed while a coherent update is in progress on PodCliqueSet <name>; complete the update before scaling
```

---

#### Case 6 — Coherent update: unchanged behavior

PGM component runs `computeCoherentUpdateEntries` as today (one MPG per reconcile, gated by `InFlightPodGangs`). PCLQ/PCSG reconcilers see `IsCoherentUpdateInProgress=true` and overwrite their `status.PodGangMapping` from PGM at the start of their reconcile, then run their flow. No new code in the coherent update path itself.

---

### Implementation Steps

#### Step 1 — API: redefine `PodGangMapping` semantics on both PCLQ and PCSG status

- `operator/api/core/v1alpha1/scalinggroup.go`:
  - Update `PodGangMapping` godoc to describe its new desired-state semantics: source of truth in steady state; reconciled from PGM during a coherent update.
- `operator/api/core/v1alpha1/podclique.go` — same godoc update for `PodGangMapping`.

No new fields. Scaled-PG counter is derived from existing keys in `pcsg.Status.PodGangMapping` matching the Scaled-PG prefix.

Run `make generate` + `make generate-api-docs`.

#### Step 2 — Remove `mutatePodGangMapping` from both reconcilers

- `operator/internal/controller/podcliquescalinggroup/reconcilestatus.go` — remove call (line 75) and function (line 143)
- `operator/internal/controller/podcliquescalinggroup/reconcilestatus_test.go` — remove related tests
- `operator/internal/controller/podclique/reconcilestatus.go` — remove call (line 72) and function (line 167)
- `operator/internal/controller/podclique/reconcilestatus_test.go` — remove related tests

#### Step 3 — Validating admission webhooks to block scaling during coherent update

**Scope**: only PCSG and PCLQ `Spec.Replicas` changes are blocked during a coherent update. PCS `Spec.Replicas` is allowed at any time — adding/removing top-level PCS replicas is orthogonal to the coherent-update migration that operates within a single PCS replica's PGM.

- **3a. New PCSG validating webhook** at `operator/internal/webhook/admission/pcsg/validation/{handler,register}.go`. Resolves owning PCS via owner reference. Path: `/webhooks/validate-podcliquescalinggroup`.
- **3b. New PCLQ validating webhook** at `operator/internal/webhook/admission/pclq/validation/{handler,register}.go`. Owner-reference resolution (PCLQ → PCSG → PCS for PCSG-owned, PCLQ → PCS for standalone). Path: `/webhooks/validate-podclique`.
- **3c. Register**: extend `operator/internal/webhook/register.go`.
- **3d. Helm manifests**: add new validating webhook configurations under `operator/charts/`.

#### Step 4 — PCLQ reconciler pod component: implement new flow (standalone PCLQs)

**File**: `operator/internal/controller/podclique/components/pod/syncflow.go`

Replace the standalone-PCLQ portion of `runSyncFlow` with the flow described in the Design section. Key changes:
- Read `pclq.Status.PodGangMapping`; seed from PGM if empty.
- If coherent update is in progress (`IsCoherentUpdateInProgress(pcs)`): overwrite status mapping from PGM entries; patch.
- In steady state, compute spec-diff and update status: scale-out increments the highest-index PodGang in the mapping; scale-in runs the deletion sorter and decrements per chosen pod's `LabelPodGang`. Patch.
- Build `currentPodGangMapping` from live (non-terminating, `DeletionTimestamp == nil`) pods counted by `LabelPodGang`.
- For each PodGang in the desired mapping, compute `delta = desired[PG] - current[PG]` and create/delete pods accordingly.

Helpers (new):
- `seedPodGangMappingFromPGM(pgm, cliqueName) map[string]int32` — build mapping from all PGM entries where `entry.PodCliques[cliqueName] > 0`.
- `decrementMappingForChosenPods(mapping, chosenPods) map[string]int32` — decrement per `LabelPodGang` of each chosen pod.
- `findHighestIndexEntryName(mapping) string` — scan keys, parse trailing counter, return name with highest counter (handles both MPG/TailPG counter and ScaledPG counter — same trailing integer position).
- `computePerPGDelta(currentPodGangMapping, desiredPodGangMapping) map[string]int32` — `desired - current` per PG.

**Retired** (replaced by status-driven per-PG diff):
- `assignPodsToDeficitPodGangs` (line 294) — deficit-fill against PGM. No longer needed; the deficit is computed against `pclq.Status.PodGangMapping`.
- `findHighestMVUPodGangForClique` (line 338) — overflow assignment to highest-index MPG. No longer needed; the highest-index decision happens in the status-update step (scale-out increments the highest-index PG), and pod placement follows the per-PG diff.
- `resolvePodGangNamesForNewPods` for standalone PCLQs simplifies to: iterate `currentPodGangMapping`/`desiredPodGangMapping`, produce a list of PodGang names per pod to create. PCSG-owned PCLQ path (`!sc.isStandalonePCLQ`) is unchanged.

#### Step 5 — PCSG reconciler PCLQ component: implement new flow

**File**: `operator/internal/controller/podcliquescalinggroup/components/podclique/syncflow.go` and `podclique.go`

Replace the steady-state portion of `runSyncFlow` with the flow described in the Design section. Key changes:
- Read `pcsg.Status.PodGangMapping`; seed from PGM if empty.
- If coherent update is in progress (`IsCoherentUpdateInProgress(pcs)`): overwrite status mapping from PGM entries; patch.
- In steady state, compute spec-diff and update status:
  - **Scale-out**: derive next Scaled-PG counter on the fly (max trailing integer over keys with the Scaled-PG prefix in `pcsg.Status.PodGangMapping`, +1). Mint new Scaled-PG names with format `<pcs>-<replica>-<hash>-<pcsgname>-<counter>`. Add `{newName: 1}` to mapping per new replica.
  - **Scale-in**: walk Tier 1 (ScaledPGs by trailing-integer counter parsed from name, descending) then Tier 2 (MPGs+TailPGs by their counter descending), decrementing entries until `|diff|` is satisfied.
- Patch `pcsg.Status.PodGangMapping`.
- Build `currentPodGangMapping` from live (non-terminating, `DeletionTimestamp == nil`) PCLQs counted by `LabelPodGang`.
- For each PodGang in the desired mapping, compute `delta = desired[PG] - current[PG]` and create/delete PCLQs accordingly.

Helpers (new):
- `seedPodGangMappingFromPGMForPCSG(pgm, pcsgName) map[string]int32`
- `mintScaledPGName(pcs, pcsg, counter) string` — generates Scaled-PG name with the steady-state naming convention.
- `partitionEntriesForScaleIn(mapping) (tier1ScaledPGs []string, tier2Others []string)` — split by name pattern; sort each by counter descending.
- `computePerPGDelta(currentPodGangMapping, desiredPodGangMapping) map[string]int32` — shared helper with PCLQ pod component.

**Retired**:
- `resolvePodGangNameFromPGM` (podclique.go:380) — positional walk over PGM. No longer needed; PCSG reconciler reads `pcsg.Status.PodGangMapping` directly and creates PCLQs labeled with PodGang names from the mapping.
- `triggerDeletionOfExcessPCSGReplicas` and `computePCSGReplicasToDelete` (syncflow.go:329, 365) — replaced by the status-driven scale-in walk that decrements entries directly in the mapping; PCLQ deletion is then driven by the per-PG diff.

#### Step 6 — PGM component `syncSteadyStateEntries`: implement follower logic

**File**: `operator/internal/controller/podcliqueset/components/podgangmap/syncflow.go`

Replace the settled/unsettled branch with:

```go
entries, _ := r.getExistingPGMEntries(ctx, sc.pcs, pgmName)

// Mirror new Scaled-PG entries from PCSG status mappings.
addPCSGStatusEntries(&entries, sc.pcs, sc.pcsgsByReplica[pcsReplicaIndex])

// Apply per-entry counts from status mappings (with settled guard).
applyStatusMappingsToEntries(entries, sc.pclqsByReplica[pcsReplicaIndex], sc.pcsgsByReplica[pcsReplicaIndex])

// Remove entries with all-zero counts.
entries = removeEmptyEntries(entries)

return r.createOrPatchPodGangMap(ctx, sc.pcs, pgmName, pcsReplicaIndex, entries)
```

Helpers (new):
- `addPCSGStatusEntries(entries, pcs, pcsgs)` — for each Scaled-PG name in any PCSG status mapping not present in entries, append a new entry. Set: `Name=name`, `PodCliqueSetGenerationHash=*pcs.Status.CurrentGenerationHash`, `PodCliques=nil`, `PodCliqueScalingGroups={pcsgName: 1}`, `DependsOn=collectMPGNamesFromEntries(entries before adding)`.
- `applyStatusMappingsToEntries(entries, pclqs, pcsgs)` — for each entry, set per-PCLQ and per-PCSG counts from status mappings. Skip a status mapping that is empty (nil/len==0; means the owning reconciler hasn't initialized yet). For non-empty mappings, values default to 0 when entry name not in the mapping (drives `removeEmptyEntries` to drop scaled-in entries).

Delete:
- `isPodGangMappingSettled` (line 439), `isPCLQMappingSettled` (line 456), `isPCSGMappingSettled` (line 467) — no settled guard needed; replaced by simple non-empty check.
- `buildEntriesFromStatuses` (line 658), `getOrCreateEntry` (line 691).

Keep:
- `getExistingPGMEntries`, `createPodGangMapForReplica` (initial PGM creation), `removeEmptyEntries` (mvu.go), `collectMPGNamesFromEntries`.
- `mutatePodGangCounter` in `podcliqueset/reconcilestatus.go` (used for observability; still recomputes PodGangCounter from PGM).

Coherent update path (`computeCoherentUpdateEntries` and below) is unchanged.

#### Step 7 — Tests

- **`syncflow_test.go`** (PGM component): remove `TestBuildEntriesFromStatuses`. Add `TestAddPCSGStatusEntries`, `TestApplyStatusMappingsToEntries`, `TestSyncSteadyStateEntries` covering all 4 steady-state cases.
- **`syncflow_test.go`** (PCLQ pod component): add tests for the new flow on standalone PCLQs (scale-out, scale-in, coherent realignment, status seeding from PGM).
- **`syncflow_test.go`** (PCSG PCLQ component): add tests for new flow (Tail-PG minting, trim, coherent realignment, status seeding).
- **`reconcilestatus_test.go`** (PCSG and PCLQ): remove `mutatePodGangMapping` tests.
- **Webhook tests** per package: replica changes rejected during coherent update; allowed in steady state.

---

### Files Modified

| File | Change |
|------|--------|
| `operator/api/core/v1alpha1/scalinggroup.go` | Update `PodGangMapping` godoc (semantics) |
| `operator/api/core/v1alpha1/podclique.go` | Update `PodGangMapping` godoc (semantics) |
| `operator/internal/controller/podcliquescalinggroup/reconcilestatus.go` | Remove `mutatePodGangMapping` |
| `operator/internal/controller/podcliquescalinggroup/reconcilestatus_test.go` | Remove related tests |
| `operator/internal/controller/podclique/reconcilestatus.go` | Remove `mutatePodGangMapping` |
| `operator/internal/controller/podclique/reconcilestatus_test.go` | Remove related tests |
| `operator/internal/controller/podclique/components/pod/syncflow.go` | New decision-driven flow for standalone PCLQs |
| `operator/internal/controller/podclique/components/pod/syncflow_test.go` | Add tests for new flow |
| `operator/internal/controller/podcliquescalinggroup/components/podclique/syncflow.go` | New decision-driven flow with Tail-PG minting |
| `operator/internal/controller/podcliquescalinggroup/components/podclique/syncflow_test.go` | Add tests for new flow |
| `operator/internal/webhook/admission/pcsg/validation/{handler,register}.go` | New validating webhook |
| `operator/internal/webhook/admission/pclq/validation/{handler,register}.go` | New validating webhook |
| `operator/internal/webhook/register.go` | Register new PCSG and PCLQ webhooks |
| `operator/charts/...` | Helm manifests for new webhooks |
| `operator/internal/controller/podcliqueset/components/podgangmap/syncflow.go` | Rewrite `syncSteadyStateEntries` as follower; delete dead functions |
| `operator/internal/controller/podcliqueset/components/podgangmap/syncflow_test.go` | Update + add tests |

---

### Verification

```bash
cd operator
make generate && make generate-api-docs
go test ./internal/webhook/...
go test ./internal/controller/podcliqueset/components/podgangmap/...
go test ./internal/controller/podcliquescalinggroup/...
go test ./internal/controller/podclique/...
```

Manual verification:
1. **Initial deployment** → PGM created from spec; PCLQ/PCSG status mappings seeded from PGM; pods/PCLQs created.
2. **Steady-state PCSG scale-out** → PCSG reconciler mints Tail-PG names in status; PGM follows; new entries appear; PodGang resources created.
3. **Steady-state PCLQ scale-out** → status mapping increments highest-index MPG; PGM follows.
4. **Steady-state PCSG scale-in** → status mapping trims highest indices; PGM follows; entries with zero counts dropped if all clique/PCSG counts are zero.
5. **Steady-state PCLQ scale-in** → deletion sorter picks pods; status decrements; PGM follows.
6. **During coherent update** → webhook rejects PCSG and PCLQ replica changes; PCS replica changes allowed; PCLQ/PCSG status mappings realign from PGM each reconcile.
7. **Pod crash mid-flight** → kubelet recreates; PCLQ pod component sees `local < status` for that PG; creates replacement labeled the same PG; status mapping unchanged.
