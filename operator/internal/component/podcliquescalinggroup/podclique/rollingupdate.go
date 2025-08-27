package podclique

import (
	"context"
	"fmt"
	apicommon "github.com/NVIDIA/grove/operator/api/common"
	"github.com/NVIDIA/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/NVIDIA/grove/operator/internal/component"
	groveerr "github.com/NVIDIA/grove/operator/internal/errors"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
	"strconv"
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
	unhealthyPCLQFQNs []string
}

func (w *pendingUpdateWork) getNextReplicaToUpdate() *int {
	// TODO
	//slices.SortFunc(w.pendingUpdateReplicaInfos, orderPCSGReplicaInfos())
	return nil
}

func (pri *pcsgReplicaInfo) getUpdateProgress(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) pcsgReplicaUpdateProgress {
	updateProgress := pcsgReplicaUpdateProgress{scheduled: true}
	for _, pclq := range pri.pclqs {
		if pclq.Labels[apicommon.LabelPodGangSetGenerationHash] != pcsg.Status.RollingUpdateProgress.PodGangSetGenerationHash {
			// PodClique not recreated yet to match the PodGangSet GenerationHash.
			continue
		}
		if pclq.Status.ScheduledReplicas < *pclq.Spec.MinAvailable {
			updateProgress.scheduled = false
		}
		if pclq.Status.ReadyReplicas >= *pclq.Spec.MinAvailable {
			updateProgress.done = true
		} else {
			if k8sutils.IsConditionTrue(pclq.Status.Conditions, constants.ConditionTypeMinAvailableBreached) {
				updateProgress.unhealthyPCLQFQNs = append(updateProgress.unhealthyPCLQFQNs, pclq.Name)
			}
		}
	}
	return updateProgress
}

func (r _resource) orchestrateRollingUpdate(ctx context.Context, logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
	updateWork := r.computePendingWork(ctx, logger, syncCtx, pcsg)

	var lastUpdatedPCSGReplicaIndex *int32
	if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil {
		lastUpdatedPCSGReplicaIndex = &pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex
		if !updateWork.currentlyUpdatingReplicaInfo.updateProgress.done {
			if err := r.updatePCSGWithReplicaUpdateProgress(ctx, logger, pcsg, updateWork.currentlyUpdatingReplicaInfo.updateProgress); err != nil {
				return err
			}
			return groveerr.New(
				groveerr.ErrCodeContinueReconcileAndRequeue,
				component.OperationSync,
				fmt.Sprintf("rolling update of PodCliqueScalingGroup replica index %d is not completed", updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
			)
		}
	}

	// pick the next replica index to update
	nextReplicaToUpdate := updateWork.getNextReplicaToUpdate()
	if err := r.updatePCSGWithNextSelectedReplica(ctx, logger, pcsg, lastUpdatedPCSGReplicaIndex, nextReplicaToUpdate); err != nil {
		return err
	}

	if nextReplicaToUpdate != nil {
		return groveerr.New(
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("commencing rolling update of PodCliqueScalingGroup replica index %d", nextReplicaToUpdate),
		)
	}

	return nil
}

func (r _resource) updatePCSGWithReplicaUpdateProgress(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, currentReplicaUpdateProgress pcsgReplicaUpdateProgress) error {
	// TODO
	return nil
}

func (r _resource) updatePCSGWithNextSelectedReplica(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, index *int32, update *int) error {
	// TODO
	return nil
}

func (r _resource) computePendingWork(ctx context.Context, logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) *pendingUpdateWork {
	pcsgReplicaInfos := buildPCSGReplicaInfos(logger, syncCtx, pcsg)
	pendingWork := &pendingUpdateWork{}
	for _, replicaInfo := range pcsgReplicaInfos {
		replicaInfo.updateProgress = replicaInfo.getUpdateProgress(pcsg)
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
