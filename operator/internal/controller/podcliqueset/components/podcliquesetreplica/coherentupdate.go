// /*
// Copyright 2026 The Grove Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */

package podcliquesetreplica

import (
	"context"
	"fmt"
	"strconv"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	mvuRollingUpdateSchedulingGate = "grove.io/gate-old-pending-pods"
)

func (r _resource) orchestrateCoherentUpdate(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsIndicesToTerminate, minAvailableBreachedPCSReplicaIndices []int) error {
	updateWork, err := r.computePendingUpdateWork(ctx, pcs, pcsIndicesToTerminate)
	if err != nil {
		return err
	}

	// Schedule gate all old pending pods that belong to this PCS replica before starting the update
	if updateWork.currentlyUpdatingReplicaInfo != nil {
		oldPendingPods, err := r.getOldPendingPodsForReplica(ctx, pcs, updateWork.currentlyUpdatingReplicaInfo)
		if err != nil {
			return err
		}
		// schedule gate the old pending pods
		for _, pendingPod := range oldPendingPods {
			result, err := controllerutil.CreateOrPatch(ctx, r.client, &pendingPod, func() error {
				pendingPod.Spec.SchedulingGates = append(pendingPod.Spec.SchedulingGates, corev1.PodSchedulingGate{Name: mvuRollingUpdateSchedulingGate})
				return nil
			})
			if err != nil {
				return groveerr.New(
					groveerr.ErrCodeContinueReconcileAndRequeue,
					component.OperationSync,
					fmt.Sprintf("failed to schedule gate old pending pods of PodCliqueSet replica %d", updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
				)
			}
			logger.Info("Schedule gated an old pending pod as a part of the coherent update for currently updating PodCliqueSetReplica", "pcsReplicaIndex", updateWork.currentlyUpdatingReplicaInfo.replicaIndex, "result", result)
		}
	}

	if len(pcs.Status.UpdateProgress.CurrentlyUpdating) > 0 && updateWork.currentlyUpdatingReplicaInfo != nil {
		if err = r.updatePCSWithReplicaUpdateProgress(ctx, logger, pcs, updateWork.currentlyUpdatingReplicaInfo.updateProgress); err != nil {
			return err
		}
		// Unlike RollingRecreate, the orchestration is fully handled here.
		// Check that the latest MPG is available. If it is not, requeue.
		if pcs.Status.UpdateProgress.CoherentUpdate != nil && pcs.Status.UpdateProgress.CoherentUpdate.LatestMPGName != nil {
			mpgName := *pcs.Status.UpdateProgress.CoherentUpdate.LatestMPGName
			available, err := r.isMPGAvailable(ctx, logger, mpgName, pcs.Namespace)
			if err != nil {
				return err
			}
			if !available {
				return groveerr.New(
					groveerr.ErrCodeContinueReconcileAndRequeue,
					component.OperationSync,
					fmt.Sprintf("latest MPG %s is not yet available for PodCliqueSet replica %d", mpgName, updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
				)
			}
			logger.Info("Latest MPG is available, continuing coherent update orchestration", "mpgName", mpgName)
		}
	}

	// pick the next replica index to update.
	nextReplicaToUpdate := updateWork.getNextReplicaToUpdate(pcs, minAvailableBreachedPCSReplicaIndices)
	if err = r.updatePCSWithNextSelectedReplica(ctx, logger, pcs, nextReplicaToUpdate); err != nil {
		return err
	}

	if nextReplicaToUpdate != nil {
		return groveerr.New(
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("commencing update of PodCliqueSet replica index %d", *nextReplicaToUpdate),
		)
	}
	return nil
}

// getOldPendingPodsForReplica lists all pods for the PCS replica with a single List call, then
// filters to only those that are pending and whose pod template hash does not match the expected
// hash for their owning PodClique.
func (r _resource) getOldPendingPodsForReplica(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet, replicaInfo *pcsReplicaInfo) ([]corev1.Pod, error) {
	// Build a map of PCLQ name -> expected pod template hash for all PodCliques in this replica.
	expectedHashByPCLQ := make(map[string]string, len(replicaInfo.pclqs))
	for _, pclq := range replicaInfo.pclqs {
		expectedHash, err := componentutils.GetExpectedPCLQPodTemplateHash(pcs, pclq.ObjectMeta)
		if err != nil {
			return nil, groveerr.WrapError(err,
				groveerr.ErrCodeContinueReconcileAndRequeue,
				component.OperationSync,
				fmt.Sprintf("failed to compute expected pod template hash for PodClique %s", pclq.Name),
			)
		}
		expectedHashByPCLQ[pclq.Name] = expectedHash
	}

	pods, err := getPCSReplicaPods(ctx, r.client, pcs, replicaInfo.replicaIndex)
	if err != nil {
		return nil, groveerr.WrapError(err,
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("failed to list pods of PodCliqueSet replica %d", replicaInfo.replicaIndex),
		)
	}

	var oldPendingPods []corev1.Pod
	for _, pod := range pods {
		pclqName := pod.Labels[apicommon.LabelPodClique]
		expectedHash, ok := expectedHashByPCLQ[pclqName]
		if !ok {
			continue
		}
		if pod.Labels[apicommon.LabelPodTemplateHash] != expectedHash && pod.Status.Phase == corev1.PodPending {
			oldPendingPods = append(oldPendingPods, pod)
		}
	}
	return oldPendingPods, nil
}

// getPCSReplicaPods lists all Pods that belong to the passed PodCliqueSet replica.
func getPCSReplicaPods(ctx context.Context, cl client.Client, pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := cl.List(ctx,
		podList,
		client.InNamespace(pcs.Namespace),
		client.MatchingLabels(
			lo.Assign(
				apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name),
				map[string]string{
					apicommon.LabelPodCliqueSetReplicaIndex: strconv.Itoa(replicaIndex),
				},
			),
		)); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// isMPGAvailable checks if the MPG (Minimal Pod Gang) PodGang is available.
// An MPG is considered available when, for each PodGroup in the PodGang,
// the number of referenced pods in Running phase >= PodGroup.MinReplicas.
func (r _resource) isMPGAvailable(ctx context.Context, logger logr.Logger, mpgName, namespace string) (bool, error) {
	mpg, err := componentutils.GetPodGang(ctx, r.client, mpgName, namespace)
	if err != nil {
		return false, groveerr.WrapError(err,
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("failed to get MPG PodGang %s", mpgName),
		)
	}

	for _, podGroup := range mpg.Spec.PodGroups {
		runningCount := int32(0)
		for _, podRef := range podGroup.PodReferences {
			pod := &corev1.Pod{}
			podKey := client.ObjectKey{Namespace: podRef.Namespace, Name: podRef.Name}
			if err := r.client.Get(ctx, podKey, pod); err != nil {
				logger.V(4).Info("Could not get pod referenced in MPG PodGroup", "podKey", podKey, "error", err)
				continue
			}
			if pod.Status.Phase == corev1.PodRunning {
				runningCount++
			}
		}
		if runningCount < podGroup.MinReplicas {
			logger.Info("MPG PodGroup has insufficient running pods",
				"mpgName", mpgName,
				"podGroupName", podGroup.Name,
				"runningCount", runningCount,
				"minReplicas", podGroup.MinReplicas)
			return false, nil
		}
	}

	return true, nil
}
