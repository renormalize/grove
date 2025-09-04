package podclique

//
//import (
//	"context"
//	"fmt"
//	"slices"
//	"strconv"
//
//	apicommon "github.com/NVIDIA/grove/operator/api/common"
//	"github.com/NVIDIA/grove/operator/api/common/constants"
//	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
//	"github.com/NVIDIA/grove/operator/internal/component"
//	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
//	groveerr "github.com/NVIDIA/grove/operator/internal/errors"
//	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"
//
//	"github.com/go-logr/logr"
//	"github.com/samber/lo"
//	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
//	"k8s.io/utils/ptr"
//	"sigs.k8s.io/controller-runtime/pkg/client"
//)
//
//type pendingUpdateWork struct {
//	pendingUpdateReplicaInfos    []pcsgReplicaInfo
//	currentlyUpdatingReplicaInfo *pcsgReplicaInfo
//}
//
//type pcsgReplicaInfo struct {
//	replicaIndex   int
//	pclqs          []grovecorev1alpha1.PodClique
//	updateProgress pcsgReplicaUpdateProgress
//}
//
//type pcsgReplicaUpdateProgress struct {
//	done              bool
//	scheduled         bool
//	updatedPCLQFQNs   []string
//	unhealthyPCLQFQNs []string
//}
//
//func (w *pendingUpdateWork) getNextReplicaToUpdate() *int {
//	slices.SortFunc(w.pendingUpdateReplicaInfos, orderPCSGReplicaInfoForPCSG)
//	if len(w.pendingUpdateReplicaInfos) > 0 {
//		return &w.pendingUpdateReplicaInfos[0].replicaIndex
//	}
//	return nil
//}
//
//// isScheduled checks if all PodCliques in the PCSG replica are scheduled with at least minAvailable pods.
//// Every PCSG replica is gang scheduled. Gang scheduling can only be done if there are sufficient resources for
//// at least minAvailable pods for every PodClique in the PCSG replica.
//func (pri *pcsgReplicaInfo) isScheduled() bool {
//	return lo.EveryBy(pri.pclqs, func(pclq grovecorev1alpha1.PodClique) bool {
//		return pclq.Status.ScheduledReplicas >= *pclq.Spec.MinAvailable
//	})
//}
//
//func orderPCSGReplicaInfoForPCSG(a, b pcsgReplicaInfo) int {
//	// 1. Pick the PCSG replica that has scheduled pods lesser than minAvailable.
//	if a.updateProgress.scheduled != b.updateProgress.scheduled {
//		if a.updateProgress.scheduled {
//			return 1
//		}
//		return -1
//	}
//
//	// 2. Pick the replicas which have the minAvailableBreached condition set to true, but the terminationDelay has not expired yet.
//	// The replicas with minAvailableBreached with terminationDelay expired are deleted before the rolling update is started.
//	if len(a.updateProgress.unhealthyPCLQFQNs) == 0 && len(b.updateProgress.unhealthyPCLQFQNs) != 0 {
//		return 1
//	} else if len(a.updateProgress.unhealthyPCLQFQNs) != 0 && len(b.updateProgress.unhealthyPCLQFQNs) == 0 {
//		return -1
//	}
//
//	// 3. If all replicas are healthy, then pick the replicas in reverse ordinal value.
//	if a.replicaIndex < b.replicaIndex {
//		return 1
//	} else {
//		return -1
//	}
//}
//
//func (pri *pcsgReplicaInfo) computeUpdateProgress(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) {
//	updateProgress := pcsgReplicaUpdateProgress{}
//	allScheduled := true
//	for _, pclq := range pri.pclqs {
//		if pclq.Status.CurrentPodGangSetGenerationHash == nil || *pclq.Status.CurrentPodGangSetGenerationHash != pcsg.Status.RollingUpdateProgress.PodGangSetGenerationHash {
//			continue
//		}
//		if pclq.Status.ScheduledReplicas < *pclq.Spec.MinAvailable {
//			allScheduled = false
//		}
//		if pclq.Status.ReadyReplicas >= *pclq.Spec.MinAvailable {
//			updateProgress.updatedPCLQFQNs = append(updateProgress.updatedPCLQFQNs, pclq.Name)
//		} else {
//			if k8sutils.IsConditionTrue(pclq.Status.Conditions, constants.ConditionTypeMinAvailableBreached) {
//				updateProgress.unhealthyPCLQFQNs = append(updateProgress.unhealthyPCLQFQNs, pclq.Name)
//			}
//		}
//	}
//	updateProgress.scheduled = allScheduled && len(pcsg.Spec.CliqueNames) == len(pri.pclqs)
//	updateProgress.done = len(updateProgress.updatedPCLQFQNs) == len(pcsg.Spec.CliqueNames)
//	pri.updateProgress = updateProgress
//}
//
//func (r _resource) processPendingUpdates(logger logr.Logger, sc *syncContext) error {
//	work := r.computePendingWork(logger, sc, sc.pcsg)
//
//	if sc.pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate != nil && work.currentlyUpdatingReplicaInfo != nil {
//		if err := r.updatePCSGWithReplicaUpdateProgress(sc.ctx, logger, sc, sc.pcsg, work.currentlyUpdatingReplicaInfo.updateProgress); err != nil {
//			return err
//		}
//		if !work.currentlyUpdatingReplicaInfo.updateProgress.done {
//			return groveerr.New(
//				groveerr.ErrCodeContinueReconcileAndRequeue,
//				component.OperationSync,
//				fmt.Sprintf("rolling update of PodCliqueScalingGroup replica index %d is not completed", work.currentlyUpdatingReplicaInfo.replicaIndex),
//			)
//		}
//	}
//
//	// pick the next replica index to update
//	nextReplicaToUpdate := work.getNextReplicaToUpdate()
//	if err := r.updatePCSGRollingUpdateProgress(sc.ctx, logger, sc.pcsg, nextReplicaToUpdate); err != nil {
//		return err
//	}
//
//	if nextReplicaToUpdate != nil {
//		logger.Info("triggering deletion of PodCliqueScalingGroup replica to update", "pcsgIndexToUpdate", *nextReplicaToUpdate)
//		replicaIndexStr := strconv.Itoa(*nextReplicaToUpdate)
//		deletionTasks := r.createDeleteTasks(logger, sc.pgs, sc.pcsg.Name, []string{replicaIndexStr}, "deleting PCSG replica to perform update")
//		if err := r.triggerDeletionOfPodCliques(sc.ctx, logger, client.ObjectKeyFromObject(sc.pcsg), deletionTasks); err != nil {
//			return err
//		}
//		return groveerr.New(
//			groveerr.ErrCodeContinueReconcileAndRequeue,
//			component.OperationSync,
//			fmt.Sprintf("commencing rolling update of PodCliqueScalingGroup replica index %d", *nextReplicaToUpdate),
//		)
//	}
//
//	return nil
//}
//
///*
//	Get all PCSG replicas that are pending update.
//    Categorize each replica as either `Pending` or `Unhealthy` or `Ready`
//*/
//
//func (r _resource) computePendingWork(logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) *pendingUpdateWork {
//	pendingWork := &pendingUpdateWork{}
//	pclqFQNsPendingUpdate := componentutils.GetPCLQsInPCSGPendingUpdate(syncCtx.pgs, pcsg, syncCtx.existingPCLQs)
//	if len(pclqFQNsPendingUpdate) == 0 {
//		logger.Info("No pending updates found for PCSG")
//		return pendingWork
//	}
//	pcsgReplicaInfos := buildPCSGReplicaInfos(logger, syncCtx, pcsg)
//	for _, replicaInfo := range pcsgReplicaInfos {
//		replicaInfo.computeUpdateProgress(pcsg)
//		if pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate != nil &&
//			pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate.ReplicaIndex == int32(replicaInfo.replicaIndex) {
//			pendingWork.currentlyUpdatingReplicaInfo = &replicaInfo
//			continue
//		}
//
//		if !replicaInfo.updateProgress.done {
//			pendingWork.pendingUpdateReplicaInfos = append(pendingWork.pendingUpdateReplicaInfos, replicaInfo)
//		}
//	}
//	return pendingWork
//}
//
//func (r _resource) updatePCSGWithReplicaUpdateProgress(ctx context.Context, logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, currentReplicaUpdateProgress pcsgReplicaUpdateProgress) error {
//	patch := client.MergeFrom(pcsg.DeepCopy())
//
//	updatedCliqueFQNs := lo.Uniq(append(pcsg.Status.RollingUpdateProgress.UpdatedPodCliques, currentReplicaUpdateProgress.updatedPCLQFQNs...))
//	// There is a possibility that the replica that is currently getting updated has been deleted due to scale-in.
//	// We need to clean up the already recorded pcsg.Status.RollingUpdateProgress.UpdatedPodCliques.
//	expectedPCLQFQNs := lo.Flatten(lo.Values(syncCtx.expectedPCLQFQNsPerPCSGReplica))
//	updatedCliqueFQNs = slices.DeleteFunc(updatedCliqueFQNs, func(pclqFQN string) bool {
//		return !slices.Contains(expectedPCLQFQNs, pclqFQN)
//	})
//	slices.Sort(updatedCliqueFQNs)
//	pcsg.Status.RollingUpdateProgress.UpdatedPodCliques = updatedCliqueFQNs
//	pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate.Scheduled = currentReplicaUpdateProgress.scheduled
//
//	if err := r.patchRollingUpdateProgressStatus(ctx, logger, pcsg, patch); err != nil {
//		logger.Error(err, "failed to update rolling update progress", "replicaIndex", pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate.ReplicaIndex)
//		return err
//	}
//	return nil
//}
//
//func (r _resource) patchRollingUpdateProgressStatus(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, patch client.Patch) error {
//	if err := r.client.Status().Patch(ctx, pcsg, patch); err != nil {
//		return groveerr.WrapError(
//			err,
//			errCodeUpdateStatus,
//			component.OperationSync,
//			"could not update rolling update progress",
//		)
//	}
//	logger.Info("Updated the PodCliqueScalingGroup status with rolling update progress")
//	return nil
//}
//
//func (r _resource) updatePCSGRollingUpdateProgress(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, nextToUpdate *int) error {
//	patch := client.MergeFrom(pcsg.DeepCopy())
//	if nextToUpdate == nil {
//		logger.Info("Rolling update has completed for PodCliqueScalingGroup")
//		pcsg.Status.RollingUpdateProgress.UpdateEndedAt = ptr.To(metav1.Now())
//		pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate = nil
//	} else {
//		logger.Info("Initiating rolling update for next replica index", "nextReplicaIndex", *nextToUpdate)
//		pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate = &grovecorev1alpha1.PodCliqueScalingGroupReplicaRollingUpdateProgress{
//			ReplicaIndex:    int32(*nextToUpdate),
//			UpdateStartedAt: metav1.Now(),
//		}
//	}
//	return r.patchRollingUpdateProgressStatus(ctx, logger, pcsg, patch)
//}
//
//func buildPCSGReplicaInfos(logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) []pcsgReplicaInfo {
//	pcsgReplicaInfos := make([]pcsgReplicaInfo, 0, pcsg.Spec.Replicas)
//	pclqsByPCSGIndex := groupPCLQsByPCSGReplicaIndex(logger, syncCtx.existingPCLQs)
//
//	for pcsgReplicaIndex := range int(pcsg.Spec.Replicas) {
//		pcsgReplicaIndexStr := strconv.Itoa(pcsgReplicaIndex)
//		if slices.Contains(syncCtx.pcsgIndicesToTerminate, pcsgReplicaIndexStr) {
//			// skip the PCSG replica index that has been identified for gang termination.
//		}
//		pcsgReplicaInfos = append(pcsgReplicaInfos, pcsgReplicaInfo{
//			replicaIndex: pcsgReplicaIndex,
//			pclqs:        pclqsByPCSGIndex[pcsgReplicaIndexStr],
//		})
//	}
//	return pcsgReplicaInfos
//}
//
//func groupPCLQsByPCSGReplicaIndex(logger logr.Logger, pclqs []grovecorev1alpha1.PodClique) map[string][]grovecorev1alpha1.PodClique {
//	pclqsByPCSGReplicaIndex := make(map[string][]grovecorev1alpha1.PodClique)
//	for _, pclq := range pclqs {
//		pcsgReplicaIndex, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
//		if !ok {
//			logger.Info("skipping PCLQ as it does not have the required label", "pclq", client.ObjectKeyFromObject(&pclq), "label", apicommon.LabelPodCliqueScalingGroupReplicaIndex)
//			continue
//		}
//		pclqsByPCSGReplicaIndex[pcsgReplicaIndex] = append(pclqsByPCSGReplicaIndex[pcsgReplicaIndex], pclq)
//	}
//	return pclqsByPCSGReplicaIndex
//}
