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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// errCodeAddPodSchedulingGate is used when addition of a scheduling gate to a pod fails
	errCodeAddPodSchedulingGate grovecorev1alpha1.ErrorCode = "ERR_ADD_POD_SCHEDULING_GATE"
	errCodeGetPodCliqueTemplate grovecorev1alpha1.ErrorCode = "ERR_GET_PODCLIQUE_TEMPLATE"
	errCodeGetPCSReplicaPods    grovecorev1alpha1.ErrorCode = "ERR_GET_PCS_REPLICA_PODS"
	errCodeGetPodGang           grovecorev1alpha1.ErrorCode = "ERR_GET_PODGANG"
)

const (
	mvuRollingUpdateSchedulingGate = "grove.io/gate-old-pending-pods"
)

type coherentSyncContext struct {
	updateWork         *pendingUpdateWork
	pcsReplicaPods     []corev1.Pod
	expectedHashByPCLQ map[string]string
}

func (c *coherentSyncContext) prepareSyncContext(ctx context.Context, logger logr.Logger, cl client.Client, pcs *grovecorev1alpha1.PodCliqueSet, updateWork *pendingUpdateWork) error {
	pcsReplicaPods, err := getPCSReplicaPods(ctx, cl, pcs, updateWork.currentlyUpdatingReplicaInfo.replicaIndex)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeGetPCSReplicaPods,
			component.OperationSync,
			fmt.Sprintf("failed to list pods of PodCliqueSet replica %d", updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
		)
	}
	c.pcsReplicaPods = pcsReplicaPods

	// Build a map of PCLQ name -> expected pod template hash for all PodCliques in this replica.
	expectedHashByPCLQ := make(map[string]string, len(updateWork.currentlyUpdatingReplicaInfo.pclqs))
	for _, pclq := range updateWork.currentlyUpdatingReplicaInfo.pclqs {
		expectedHash, err := componentutils.GetExpectedPCLQPodTemplateHash(pcs, pclq.ObjectMeta)
		if err != nil {
			return groveerr.WrapError(err,
				errCodeGetPodCliqueTemplate,
				component.OperationSync,
				fmt.Sprintf("failed to compute expected pod template hash for PodClique %s", pclq.Name),
			)
		}
		expectedHashByPCLQ[pclq.Name] = expectedHash
	}
	c.expectedHashByPCLQ = expectedHashByPCLQ
	c.updateWork = updateWork
	return nil
}

func (c *coherentSyncContext) getOutdatedResources() ([]string, []string) {
	// Check the standalone PodCliques with their CurrentPodTemplateHash that does not match the expected value
	return nil, nil
}

func (r _resource) orchestrateCoherentUpdate(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsIndicesToTerminate, minAvailableBreachedPCSReplicaIndices []int) error {
	updateWork, err := r.computePendingUpdateWork(ctx, pcs, pcsIndicesToTerminate)
	if err != nil {
		return err
	}

	if len(pcs.Status.UpdateProgress.CurrentlyUpdating) > 0 && updateWork.currentlyUpdatingReplicaInfo != nil {
		sc := &coherentSyncContext{}
		sc.prepareSyncContext(ctx, logger, r.client, pcs, updateWork)

		if err := r.scheduleGateOldPendingPodsInPCSReplica(ctx, logger, sc, pcs); err != nil {
			return err
		}

		if pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate != nil {
			// Coherent update has already been initiated, we check the availability of the MPG corresponding with the current iteration index
			// Check that the latest MPG is available. If it is not, requeue.
			// TODO: @renormalize the name will always be set here? If it is set here then we can get rid of this.
			// If the name is set in the PodGang component, then we will have to check since the patch of the status might fail and this field might be empty
			if pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate.LatestMPGName != nil {
				// If current MPG is not available, the ContinueReconcileAndRequeue will cause the function to return here
				if err := r.isCurrentMPGAvailable(ctx, logger, sc, pcs); err != nil {
					return err
				}
			}
		}

		// Signal the start of coherent update OR finish of last iteration index by bumping the iteration index
		if err := r.bumpCoherentUpdateIterationIndex(ctx, logger, sc, pcs); err != nil {
			return err
		}

		// Find the take down set

		// Update the PCS status with the latest update progress
		if err = r.updatePCSWithReplicaUpdateProgress(ctx, logger, pcs, updateWork.currentlyUpdatingReplicaInfo.updateProgress); err != nil {
			return err
		}

		if !updateWork.currentlyUpdatingReplicaInfo.updateProgress.done {
			return groveerr.New(
				groveerr.ErrCodeContinueReconcileAndRequeue,
				component.OperationSync,
				fmt.Sprintf("coherent update of PodCliqueSet replica index %d is not completed", updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
			)
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

func (r _resource) bumpCoherentUpdateIterationIndex(ctx context.Context, logger logr.Logger, sc *coherentSyncContext, pcs *grovecorev1alpha1.PodCliqueSet) error {
	originalPCS := pcs.DeepCopy()
	iterationIndex := int32(0)
	if pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate != nil {
		iterationIndex = pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate.IterationIndex + 1
	}
	pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate = &grovecorev1alpha1.CoherentUpdateProgress{
		IterationIndex: iterationIndex,
		// TODO: @renormalize PCS Revision counter needs to start being maintained
		LatestMPGName: ptr.To(apicommon.GenerateMPGName(pcs.Name, sc.updateWork.currentlyUpdatingReplicaInfo.replicaIndex, 0, int(iterationIndex))),
	}
	logger.Info("Bumping coherent update iteration index", "iterationIndex", iterationIndex)
	return r.patchUpdateProgressStatus(ctx, logger, pcs, originalPCS)
}

func (r _resource) scheduleGateOldPendingPodsInPCSReplica(ctx context.Context, logger logr.Logger, sc *coherentSyncContext, pcs *grovecorev1alpha1.PodCliqueSet) error {
	var oldPendingPods []corev1.Pod
	for _, pod := range sc.pcsReplicaPods {
		pclqName := pod.Labels[apicommon.LabelPodClique]
		expectedHash, ok := sc.expectedHashByPCLQ[pclqName]
		if !ok {
			continue
		}
		if pod.Labels[apicommon.LabelPodTemplateHash] != expectedHash && pod.Status.Phase == corev1.PodPending {
			oldPendingPods = append(oldPendingPods, pod)
		}
	}

	// schedule gate old pending pods
	for _, pendingPod := range oldPendingPods {
		result, err := controllerutil.CreateOrPatch(ctx, r.client, &pendingPod, func() error {
			pendingPod.Spec.SchedulingGates = append(pendingPod.Spec.SchedulingGates, corev1.PodSchedulingGate{Name: mvuRollingUpdateSchedulingGate})
			return nil
		})
		if err != nil {
			return groveerr.New(
				errCodeAddPodSchedulingGate,
				component.OperationSync,
				fmt.Sprintf("failed to schedule gate old pending pods of PodCliqueSet replica %d", sc.updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
			)
		}
		logger.Info("Schedule gated an old pending pod as a part of the ongoing coherent update", "pcsReplicaIndex", sc.updateWork.currentlyUpdatingReplicaInfo.replicaIndex, "result", result)
	}
	return nil
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

// isCurrentMPGAvailable checks if the MPG (Minimal Pod Gang) PodGang is available.
// An MPG is considered available when, for each PodGroup in the PodGang,
// the number of referenced pods in Running phase >= PodGroup.MinReplicas.
func (r _resource) isCurrentMPGAvailable(ctx context.Context, logger logr.Logger, sc *coherentSyncContext, pcs *grovecorev1alpha1.PodCliqueSet) error {
	mpgName := *pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate.LatestMPGName
	mpg, err := componentutils.GetPodGang(ctx, r.client, mpgName, pcs.Namespace)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeGetPodGang,
			component.OperationSync,
			fmt.Sprintf("failed to get MPG PodGang %s", mpgName),
		)
	}

	for _, podGroup := range mpg.Spec.PodGroups {
		runningCount := int32(0)
		for _, podRef := range podGroup.PodReferences {
			pod, ok := lo.Find(sc.pcsReplicaPods, func(pod corev1.Pod) bool {
				return podRef.Name == pod.Name && podRef.Namespace == pod.Namespace
			})
			// If referred pod is not found, then the pod references are stale. Wait for PodGang sync to happen to refresh the pod references
			if !ok {
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
				"minReplicas", podGroup.MinReplicas,
			)
			return groveerr.New(
				groveerr.ErrCodeContinueReconcileAndRequeue,
				component.OperationSync,
				fmt.Sprintf("latest MPG %s is not yet available for PodCliqueSet replica %d", mpgName, sc.updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
			)
		}
	}
	logger.Info("Latest MPG is available, continuing coherent update orchestration", "mpgName", mpgName)
	return nil
}
