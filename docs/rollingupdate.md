# Rolling Update

## Overview

`Grove` offers a hierarchical and a flexible API to define AI inference workloads. There are primarily three groupings, namely `PodGangSet`, `PodCliqueScalingGroup` and `PodClique` as depicted below.

![`PodGangSet` Composition Diagram](./assets/pgs-composition.excalidraw.png)

Grove's rolling update mechanism is designed to maintain service availability while updating components of the AI inference workload. It implements an opinionated, hierarchical update strategy that ensures minimal disruption to running services. The rolling update process pauses the gang termination behavior, and halts updates if the updated `Pod`s do not become available as `Deplyoment`s or `StatefulSet`s do.

Rolling updates are triggered when there's a change in the specification of the `PodGangSet`. The updates are performed one replica at a time all hierarchies, ensuring that a failure in an update does not progress to other replicas, ensuring further degradation of service availability does not occur.

## Abbreviations

We will be using the following `short-names` in the document for brevity:

| Abbreviation / Short Name | Long Form / Description                                      |
| ------------------------- | ------------------------------------------------------------ |
| PGS                       | `PodGangSet`                                                   |
| PCLQ                      | `PodClique`                                                    |
| PCSG                      | `PodCliqueScalingGroup`                                        |
| PCLQ-S                    | Standalone `PodClique`, is a `PodClique` that is not associated to any `PodCliqueScalingGroup` |
| PCLQ-G                    | `PodClique` that is associated to a `PodCliqueScalingGroup`      |

## Requirements

Rolling update is an evolving feature. In the initial version of rolling updates following are the requirements:

* `Scale` subresource has been exposed for PGS, PCLQ-S and PCSG. Scale-in and Scale-out at all levels should be supported during rolling update.
* Availability is paramount for deployed AI workloads. It is therefore advised that deployments for each of PGS, PCLQ and PCSG have more than one replica. Keeping availability in view following requirements are defined:
  * Only one PGS replica should be updated at a time. If update of the currently updating replica is not complete then the rolling update will get paused till the time the criteria for update completion is met.
  * For PCSG and PCLQ:
    * During the rolling update if there are any replicas in `Pending` or in `Unhealthy` state then they should be force updated and rolling update should not wait for them to be ready.
    * Only one ready replica should be updated at a time. The update of each replica is done by recreating the entire replica. Rolling update must progress to the next replica only when the criteria for update completion for a replica is met.
* Partial rolling updates for the PGS should be supported. Since each `PodClique` can have a different `PodSpec`, it is possible that updates are only available for a subset of `PodClique`s in a `PodGangSet`. Since each `PodClique` can utilize numerous GPUs (e.g. wide-EP deployment), relinquishing GPU resources (which are scarce resources) for `PodClique`s that have no updates is not desirable as it can lead to unexpected and longer unavailability.
* It can take a long time (from seconds to several minutes) to update each constituent `Pod`.  Thus, it is important that the update progress be indicated via appropriate custom resource status fields.

## How to identify if there are pending updates?

Grove uses a generation-based approach to track changes and identify when resources need to be updated. This is implemented through generation hashes that are computed and propagated through the resource hierarchy.

### Generation Hash

When a change is made to a `PodGangSet` (PGS) specification, a new `CurrentGenerationHash` is computed and stored in the PGS status. This hash represents the current desired state of the PGS and all its child resources.

The generation hash is propagated down the resource hierarchy:

1. PodGangSet (PGS) computes and stores its `CurrentGenerationHash` based on its specification.
2. PodClique (PCLQ) and PodCliqueScalingGroup (PCSG) receive this hash through their `CurrentPodGangSetGenerationHash` field when the resources are in sync with the `PodGangSet` specification.
3. Additionally, each resource computes its own template hash:
   * PCLQs have a `CurrentPodTemplateHash` that reflects their pod template specification.
   * PCSGs track the pod template hash for each PCLQ they manage.

### Identifying Pending Updates

An update is considered pending for a resource in the following cases:

1. For a PGS: When its specification changes, causing a new `CurrentGenerationHash` to be generated.
2. For a PCSG: When the PGS generation hash changes, along with a change to the pod templates of the `PodClique`s that the PCSG manages.
3. For a PCLQ-S: When the PGS generation hash changes, along with a change to the PCLQ's pod template.

The system identifies resources requiring updates by comparing their current hash present in the status with the expected hash, which is computed from the specification defined in the PGS. A mismatch indicates that the resource is pending an update, and a rolling update is started.

This hashing mechanism ensures that only resources with actual changes are updated, allowing for partial updates within the resource hierarchy.

## Tracking Rolling Update Progress

Grove tracks rolling update progress through the `RollingUpdateProgress` field added to the status of each resource type (PGS, PCSG, and PCLQ). This allows users to monitor the update progress and helps the controllers manage the update process.

In each case, the `UpdateStartedAt` timestamp is set in the status of the resource to indicate that the rolling update has initiated.

### RollingUpdateProgress Structure

Each resource type implements its own variation of the `RollingUpdateProgress` structure:

#### PodGangSet RollingUpdateProgress

When viewing the status of a `PodGangSet` with `kubectl get podgangset <name> -o yaml`, the `RollingUpdateProgress` will appear as follows:

```yaml
status:
  # ... other status fields ...
  rollingUpdateProgress:
    updateStartedAt: "2025-09-11T14:30:00Z"
    # updateEndedAt would not be set until the update ends
    currentlyUpdating:
      replicaIndex: 2
      updateStartedAt: "2025-09-11T14:30:00Z"
    updatedPodCliques:
      - "podclique-1-fqn"
      - "podclique-2-fqn"
    updatedPodCliqueScalingGroups:
      - "pcsg-1-fqn"
  updatedReplicas: 0
```

#### PodCliqueScalingGroup RollingUpdateProgress

When viewing the status of a `PodCliqueScalingGroup` with `kubectl get pcsg <name> -o yaml`, the `RollingUpdateProgress` will appear as follows:

```yaml
status:
  # ... other status fields ...
  rollingUpdateProgress:
    updateStartedAt: "2025-09-11T14:30:00Z"
    # updateEndedAt would not be set until the update ends
    podGangSetGenerationHash: "abc123def456" # the new hash of the PGS which triggered the rolling update
    updatedPodCliques:
      - "pcsg-0-pclq-0-fqn"
      - "pcsg-0-pclq-1-fqn"
      - "pcsg-2-pclq-0-fqn"
      - "pcsg-2-pclq-1-fqn"
    readyReplicaIndicesSelectedToUpdate:
      current: 1
      completed:
        - 0
        - 2
```

#### PodClique RollingUpdateProgress

When viewing the status of a `PodClique` with `kubectl get pclq <name> -o yaml`, the `RollingUpdateProgress` will appear as follows:

```yaml
status:
  # ... other status fields ...
  rollingUpdateProgress:
    updateStartedAt: "2025-09-11T14:30:00Z"
    # updateEndedAt would not be set until the update ends
    podGangSetGenerationHash: "abc123def456"
    podTemplateHash: "xyz789uvw012"
    readyPodsSelectedToUpdate:
      current: "pod-name-3"
      completed:
      - "pod-name-1"
      - "pod-name-2"
```

### When Updates Are Considered Complete

The rolling update process in general follows the philosophy that the update would not reduce the availability of any resource in the resource hierarchy by more than one replica.

Therefore, a rolling update is considered complete when the number of updated and available replicas the number of available replicas prior to the update.

In each case, the `UpdateEndedAt` timestamp is set in the status of the resource to indicate completion.

#### PGS Update Completion

A PGS replica update is considered complete when all of its constituent PCLQ-S and PCSGs have been updated.

A PGS update is considered complete, when all its constituent replicas have been updated.

#### PCSG Update Completion

A PCSG replica update is considered complete when all of its constituent PCLQs (PCLQ-Gs) have been updated to the new template, by deleting the PCLQ-Gs first, and recreating them from the new template.

A PCSG update is considered complete, when the number of available replicas of the PCSG is greater than equal to the number of available replicas prior to the start of the rolling update.

#### PCLQ-S Update Completion

A PCLQ-S update is considered complete when the number of ready pods with the new template hash is greater than or equal to the number of ready pods with the old template hash prior to the start of the rolling update.

It could be the case not all updated `Pod`s of the PCLQ-S are `Ready` for the rolling update be considered as ended.

### Detecting Stuck Updates

A rolling update may become stuck in the following scenarios:

1. **Resource Unavailability**: When there aren't enough resources (e.g., GPUs) to create new pods while old ones are still running.
2. **MinAvailable Constraint**: When the system can't delete older pods/replicas because the number of available pods/replicas (old and new combined) would fall below `MinAvailable`.
3. **Pod Scheduling Issues**: When new pods fail to schedule due to node affinity, taints, or other constraints.

You can identify stuck updates by:

1. Checking if `RollingUpdateProgress.UpdateEndedAt` remains `nil` for an extended period.
2. Examining if `CurrentlyUpdating` (PGS) or `ReadyReplicaIndicesSelectedToUpdate` (PCSG) or `ReadyPodsSelectedToUpdate` (PCLQ) remains unchanged for a long time.
3. Checking pod events and logs to identify scheduling or resource issues.

## Order in which resources are selected for update

Grove applies specific prioritization logic at each level of the hierarchy to determine the optimal order for updating replicas. This ensures that the system maintains maximum availability while efficiently progressing through the update.

### PodGangSet (PGS) Replica Selection

When selecting which PGS replica to update next, the system follows this priority order:

1. **Replicas with no scheduled pods**: These are updated first since they're not serving traffic.
2. **Replicas with MinAvailable breached**: Replicas that don't meet their minimum availability requirements but haven't reached their termination delay are updated next.
3. **Healthy replicas**: The healthy replicas are updated in reverse ordinal order (highest index first).

### PodCliqueScalingGroup (PCSG) Replica Selection

For PCSG replicas, the update prioritization follows:

1. **Pending replicas**: Replicas in pending state are immediately deleted for update.
2. **Unavailable replicas**: Replicas that are scheduled but not ready are also immediately deleted for update along with the pending replicas.
3. **Ready replicas**: Only one ready replica is updated at a time, in ascending ordinal order. (This is done to ensure the base `PodGang` becomes available which is necessary for scheduling scaled `PodGang`s which are created during the update)

The controller ensures that the PCSG's `MinAvailable` constraint is satisfied before selecting a ready replica for update.

### Standalone PodClique (PCLQ-S) Pod Selection

Within a `PodClique`, pods are selected for update in the following order:

1. **Pods with old template hash in Pending state**: These are immediately deleted.
2. **Pods with old template hash in Unhealthy state**: These are also immediately deleted along with old template hash `Pending` pods.
3. **Pods with old template hash in Ready state**: Only one ready pod is updated at a time.

Similar to PCSG, the PCLQ controller ensures the `MinAvailable` constraint is satisfied before updating a ready pod.

For ready pods, the controller selects the oldest pod first (based on creation timestamp) to ensure consistent update behavior.

## Flow

### Actors Participating in Rolling Update

The rolling update process in Grove involves several controller components working together:

1. **PodGangSet Controller**: Manages the top-level update orchestration.
2. **PodCliqueScalingGroup Controller**: Handles updates of PCLQs within a scaling group.
3. **PodClique Controller**: Manages updates of pods within a clique.
4. **Pod Lifecycle Management**: Kubernetes core controllers that handle pod creation and termination.

### Update Flow Control

The rolling update follows a cascading pattern from top to bottom in the resource hierarchy:

#### 1. PodGangSet Update Initiated

When a PGS specification is updated:

The controller computes the generation hash from the spec and compares it with the value in the status.

* If the computed hash matches the hash in the status, there is no need for a rolling update.
* If the computed hash does not match with the hash in the status, an update is needed:
    1. The controller initializes `RollingUpdateProgress` with `UpdateStartedAt` timestamp.
    2. It selects the first PGS replica to update based on the prioritization logic.
    3. The selected replica index is stored in `RollingUpdateProgress.CurrentlyUpdating`.
    4. Updates the hash in the status.

The hash in the status must always be up-to-date, since it is used by the PCSG and PCLQ controllers to determine if a rolling update is necessary.

#### 2. PGS Replica Update Process

For each PGS replica being updated:

1. The controller identifies all PCLQ-S and PCSGs belonging to the replica.
2. It tracks the update progress of all PCLQ-S and PCSGs within the replica.
3. The update is considered complete when all PCLQ-S and PCSGs are updated.
4. Once the current replica update is complete, the next replica is selected.
5. When all replicas are updated, `UpdateEndedAt` is set in the PGS status.

#### 3. PCSG Update Process

When a PCSG needs to be updated:

1. The controller identifies all replicas needing updates.
2. It immediately deletes any pending or unavailable replicas.
3. It selects one ready replica for update if available.
4. The selected replica index is stored in `RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate.Current`.
5. The selected replica's PCLQ-Gs are deleted to trigger recreation with the new template.
6. The controller waits until the replica is recreated and ready before selecting the next one.

#### 4. PCLQ Update Process

When a PCLQ needs to be updated:

1. The controller identifies pods with old template hash.
2. It immediately deletes pending and unhealthy pods.
3. It selects one ready pod for update if available.
4. The selected pod name is stored in `RollingUpdateProgress.ReadyPodsSelectedToUpdate.Current`.
5. The selected pod is deleted to trigger recreation with the new template.
6. The controller waits until enough new pods are ready before selecting the next pod.
7. The update is marked complete when all pods have the new template hash.

This cascading update mechanism ensures that changes propagate through the system in a controlled manner while maintaining the availability requirements at each level of the hierarchy.
