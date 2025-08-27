package podclique

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	apicommon "github.com/NVIDIA/grove/operator/api/common"
	"github.com/NVIDIA/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/NVIDIA/grove/operator/internal/component"
	groveerr "github.com/NVIDIA/grove/operator/internal/errors"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"
	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type pendingUpdateWork struct {
	pendingUpdateReplicaInfos    []pcsgReplicaInfo
	currentlyUpdatingReplicaInfo *pcsgReplicaInfo
}

type pcsgReplicaInfo struct {
	replicaIndex   int
	pclqs          []grovecorev1alpha1.PodClique
	updateProgress pcsgReplicaUpdateProgress
}

type pcsgReplicaUpdateProgress struct {
	done              bool
	scheduled         bool
	updatedPCLQFQNs   []string
	unhealthyPCLQFQNs []string
}

func (w *pendingUpdateWork) getNextReplicaToUpdate() *int {
	slices.SortFunc(w.pendingUpdateReplicaInfos, orderPCSGReplicaInfoForPCSG)
	if len(w.pendingUpdateReplicaInfos) > 0 {
		return &w.pendingUpdateReplicaInfos[0].replicaIndex
	}
	return nil
}

func orderPCSGReplicaInfoForPCSG(a, b pcsgReplicaInfo) int {
	// 1. Pick the PCSG replica that has scheduled pods lesser than minAvailable.
	if a.updateProgress.scheduled != b.updateProgress.scheduled {
		if a.updateProgress.scheduled {
			return 1
		}
		return -1
	}

	// 2. Pick the replicas which have the minAvailableBreached condition set to true, but the terminationDelay has not expired yet.
	// The replicas with minAvailableBreached with terminationDelay expired are deleted before the rolling update is started.
	if len(a.updateProgress.unhealthyPCLQFQNs) == 0 && len(b.updateProgress.unhealthyPCLQFQNs) != 0 {
		return 1
	} else if len(a.updateProgress.unhealthyPCLQFQNs) != 0 && len(b.updateProgress.unhealthyPCLQFQNs) == 0 {
		return -1
	}

	// 3. If all replicas are healthy, then pick the replicas in reverse ordinal value.
	if a.replicaIndex < b.replicaIndex {
		return 1
	} else {
		return -1
	}
}

func (pri *pcsgReplicaInfo) computeUpdateProgress(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) {
	updateProgress := pcsgReplicaUpdateProgress{scheduled: true}
	for _, pclq := range pri.pclqs {
		if pclq.Labels[apicommon.LabelPodGangSetGenerationHash] != pcsg.Status.RollingUpdateProgress.PodGangSetGenerationHash {
			// PodClique not recreated yet to match the PodGangSet GenerationHash.
			continue
		}
		if pclq.Status.ScheduledReplicas >= *pclq.Spec.MinAvailable {
			updateProgress.scheduled = true
		}
		if pclq.Status.ReadyReplicas >= *pclq.Spec.MinAvailable {
			updateProgress.updatedPCLQFQNs = append(updateProgress.updatedPCLQFQNs, pclq.Name)
			updateProgress.done = true
		} else {
			if k8sutils.IsConditionTrue(pclq.Status.Conditions, constants.ConditionTypeMinAvailableBreached) {
				updateProgress.unhealthyPCLQFQNs = append(updateProgress.unhealthyPCLQFQNs, pclq.Name)
			}
		}
	}
	pri.updateProgress = updateProgress
}

func (r _resource) orchestrateRollingUpdate(ctx context.Context, logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
	updateWork := r.computePendingWork(logger, syncCtx, pcsg)

	var lastUpdatedPCSGReplicaIndex *int32
	if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil {
		lastUpdatedPCSGReplicaIndex = &pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex
		if err := r.updatePCSGWithReplicaUpdateProgress(ctx, logger, pcsg, updateWork.currentlyUpdatingReplicaInfo.updateProgress); err != nil {
			return err
		}
		if !updateWork.currentlyUpdatingReplicaInfo.updateProgress.done {
			return groveerr.New(
				groveerr.ErrCodeContinueReconcileAndRequeue,
				component.OperationSync,
				fmt.Sprintf("rolling update of PodCliqueScalingGroup replica index %d is not completed", updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
			)
		}
	}

	// pick the next replica index to update
	nextReplicaToUpdate := updateWork.getNextReplicaToUpdate()
	if err := r.updatePCSGRollingUpdateProgress(ctx, logger, pcsg, lastUpdatedPCSGReplicaIndex, nextReplicaToUpdate); err != nil {
		return err
	}

	if nextReplicaToUpdate != nil {
		logger.Info("triggering deletion of PodCliqueScalingGroup replica to update", "pcsgIndexToUpdate", *nextReplicaToUpdate)
		replicaIndexStr := strconv.Itoa(*nextReplicaToUpdate)
		deletionTasks := r.createDeleteTasks(logger, syncCtx.pgs, pcsg.Name, []string{replicaIndexStr}, "deleting PCSG replica to perform update")
		if err := r.triggerDeletionOfPodCliques(ctx, logger, client.ObjectKeyFromObject(pcsg), deletionTasks); err != nil {
			return err
		}
		return groveerr.New(
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("commencing rolling update of PodCliqueScalingGroup replica index %d", *nextReplicaToUpdate),
		)
	}

	return nil
}

func (r _resource) updatePCSGWithReplicaUpdateProgress(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, currentReplicaUpdateProgress pcsgReplicaUpdateProgress) error {
	patch := client.MergeFrom(pcsg.DeepCopy())
	updatedCliqueFQNs := lo.Uniq(append(pcsg.Status.RollingUpdateProgress.UpdatedPodCliques, currentReplicaUpdateProgress.updatedPCLQFQNs...))
	slices.Sort(updatedCliqueFQNs)
	pcsg.Status.RollingUpdateProgress.UpdatedPodCliques = updatedCliqueFQNs
	pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.Scheduled = currentReplicaUpdateProgress.scheduled
	pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.UnhealthyPodCliques = currentReplicaUpdateProgress.unhealthyPCLQFQNs
	logger.Info("Updating PodCliqueScalingGroup status with newly updaed PodCliques")
	if err := r.patchRollingUpdateProgressStatus(ctx, logger, pcsg, patch); err != nil {
		logger.Error(err, "failed to update rolling update progress", "replicaIndex", pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex)
		return err
	}
	return r.patchRollingUpdateProgressStatus(ctx, logger, pcsg, patch)
}

func (r _resource) patchRollingUpdateProgressStatus(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, patch client.Patch) error {
	if err := r.client.Status().Patch(ctx, pcsg, patch); err != nil {
		return groveerr.WrapError(
			err,
			errCodeUpdateStatus,
			component.OperationSync,
			"could not update rolling update progress",
		)
	}
	logger.Info("Updated the PodCliqueScalingGroup status with rolling update progress")
	return nil
}

func (r _resource) updatePCSGRollingUpdateProgress(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, previouslyUpdated *int32, nextToUpdate *int) error {
	patch := client.MergeFrom(pcsg.DeepCopy())
	if nextToUpdate == nil {
		logger.Info("Rolling update has completed for PodCliqueScalingGroup")
		meta.RemoveStatusCondition(&pcsg.Status.Conditions, constants.ConditionTypeUpdateInProgress)
		pcsg.Status.RollingUpdateProgress.UpdateEndedAt = ptr.To(metav1.Now())
		pcsg.Status.RollingUpdateProgress.CurrentlyUpdating = nil
	} else {
		logger.Info("Initiating rolling update for next replica index", "nextReplicaIndex", *nextToUpdate)
		pcsg.Status.RollingUpdateProgress.CurrentlyUpdating = &grovecorev1alpha1.PodCliqueScalingGroupReplicaRollingUpdateProgress{
			ReplicaIndex:    int32(*nextToUpdate),
			UpdateStartedAt: metav1.Now(),
		}
	}
	if previouslyUpdated != nil {
		pcsg.Status.RollingUpdateProgress.UpdatedReplicas++
	}
	return r.patchRollingUpdateProgressStatus(ctx, logger, pcsg, patch)
}

func (r _resource) computePendingWork(logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) *pendingUpdateWork {
	pcsgReplicaInfos := buildPCSGReplicaInfos(logger, syncCtx, pcsg)
	pendingWork := &pendingUpdateWork{}
	for _, replicaInfo := range pcsgReplicaInfos {
		replicaInfo.computeUpdateProgress(pcsg)
		if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil &&
			pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex == int32(replicaInfo.replicaIndex) {
			pendingWork.currentlyUpdatingReplicaInfo = &replicaInfo
			continue
		}

		if !replicaInfo.updateProgress.done {
			pendingWork.pendingUpdateReplicaInfos = append(pendingWork.pendingUpdateReplicaInfos, replicaInfo)
		}
	}
	return pendingWork
}

func buildPCSGReplicaInfos(logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) []pcsgReplicaInfo {
	pcsgReplicaInfos := make([]pcsgReplicaInfo, 0, pcsg.Spec.Replicas)
	pclqsByPCSGIndex := groupPCLQsByPCSGReplicaIndex(logger, syncCtx.existingPCLQs)

	for pcsgReplicaIndex := range int(pcsg.Spec.Replicas) {
		pcsgReplicaIndexStr := strconv.Itoa(pcsgReplicaIndex)
		if slices.Contains(syncCtx.pcsgIndicesToTerminate, pcsgReplicaIndexStr) {
			// skip the PCSG replica index that has been identified for gang termination.
			continue
		}
		pcsgReplicaInfos = append(pcsgReplicaInfos, pcsgReplicaInfo{
			replicaIndex: pcsgReplicaIndex,
			pclqs:        pclqsByPCSGIndex[pcsgReplicaIndexStr],
		})
	}
	return pcsgReplicaInfos
}

func groupPCLQsByPCSGReplicaIndex(logger logr.Logger, pclqs []grovecorev1alpha1.PodClique) map[string][]grovecorev1alpha1.PodClique {
	pclqsByPCSGReplicaIndex := make(map[string][]grovecorev1alpha1.PodClique)
	for _, pclq := range pclqs {
		pcsgReplicaIndex, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
		if !ok {
			logger.Info("skipping PCLQ as it does not have the required label", "pclq", client.ObjectKeyFromObject(&pclq), "label", apicommon.LabelPodCliqueScalingGroupReplicaIndex)
			continue
		}
		pclqsByPCSGReplicaIndex[pcsgReplicaIndex] = append(pclqsByPCSGReplicaIndex[pcsgReplicaIndex], pclq)
	}
	return pclqsByPCSGReplicaIndex
}

func isRollingUpdateInProgress(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) bool {
	return pcsg.Status.RollingUpdateProgress != nil && pcsg.Status.RollingUpdateProgress.UpdateEndedAt == nil
}
