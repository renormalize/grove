package podclique

import (
	"fmt"
	apicommon "github.com/NVIDIA/grove/operator/api/common"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/NVIDIA/grove/operator/internal/component"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	groveerr "github.com/NVIDIA/grove/operator/internal/errors"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"
	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
)

// updateWork encapsulates the information needed to perform a rolling update of a PodCliqueScalingGroup.
type updateWork struct {
	oldPendingReplicaIndices     []int
	oldUnavailableReplicaIndices []int
	oldReadyReplicaIndices       []int
	newReadyReplicaIndices       []int
}

type replicaState int

const (
	replicaStatePending replicaState = iota
	replicaStateUnAvailable
	replicaStateReady
)

func (r _resource) processPendingUpdates(logger logr.Logger, sc *syncContext) error {
	work, err := computePendingUpdateWork(sc)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeComputePendingPodCliqueScalingGroupUpdateWork,
			component.OperationSync,
			fmt.Sprintf("failed to compute pending update work for PodCliqueScalingGroup %v", client.ObjectKeyFromObject(sc.pcsg)))
	}
	// always delete PCSG replicas that are either pending or unavailable
	if err = r.deleteOldPendingAndUnavailableReplicas(logger, sc, work); err != nil {
		return err
	}

	if isAnyReadyReplicaSelectedForUpdate(sc.pcsg) && !isCurrentReplicaUpdateComplete(sc) {
		return groveerr.New(
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("rolling update of currently selected PCSG replica index: %d is not complete, requeuing", sc.pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate.Current),
		)
	}

	return nil
}

func computePendingUpdateWork(sc *syncContext) (*updateWork, error) {
	work := &updateWork{}
	existingPCLQsByReplicaIndex := componentutils.GroupPCLQsByPCSGReplicaIndex(sc.existingPCLQs)
	for pcsgReplicaIndex := range int(sc.pcsg.Spec.Replicas) {
		pcsgReplicaIndexStr := strconv.Itoa(pcsgReplicaIndex)
		existingPCSGReplicaPCLQs := existingPCLQsByReplicaIndex[pcsgReplicaIndexStr]
		if isReplicaDeletedOrMarkedForDeletion(existingPCSGReplicaPCLQs) {
			continue
		}
		state := getReplicaState(existingPCSGReplicaPCLQs)
		isUpdated, err := isReplicaUpdated(sc.expectedPCLQPodTemplateHashMap, existingPCSGReplicaPCLQs)
		if err != nil {
			return nil, err
		}
		if isUpdated {
			if state == replicaStateReady {
				work.newReadyReplicaIndices = append(work.newReadyReplicaIndices, pcsgReplicaIndex)
			}
			continue
		}
		switch state {
		case replicaStatePending:
			work.oldPendingReplicaIndices = append(work.oldPendingReplicaIndices, pcsgReplicaIndex)
		case replicaStateUnAvailable:
			work.oldUnavailableReplicaIndices = append(work.oldUnavailableReplicaIndices, pcsgReplicaIndex)
		case replicaStateReady:
			work.oldReadyReplicaIndices = append(work.oldReadyReplicaIndices, pcsgReplicaIndex)
		}
	}
	return work, nil
}

func (r _resource) deleteOldPendingAndUnavailableReplicas(logger logr.Logger, sc *syncContext, work *updateWork) error {
	// TODO
	return nil
}

func isAnyReadyReplicaSelectedForUpdate(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) bool {
	return pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate != nil
}

func isCurrentReplicaUpdateComplete(sc *syncContext) bool {
	currentlyUpdatingReplicaIndex := int(sc.pcsg.Status.RollingUpdateProgress.ReadyReplicaIndicesSelectedToUpdate.Current)
	existingPCLQsByReplicaIndex := componentutils.GroupPCLQsByPCSGReplicaIndex(sc.existingPCLQs)
	// Get the expected PCLQ PodTemplateHash and compare it against all existing PCLQs for the currently updating replica index.
	expectedPCLQFQNs := sc.expectedPCLQFQNsPerPCSGReplica[currentlyUpdatingReplicaIndex]
	existingPCSGReplicaPCLQs := existingPCLQsByReplicaIndex[strconv.Itoa(currentlyUpdatingReplicaIndex)]
	if len(expectedPCLQFQNs) != len(existingPCSGReplicaPCLQs) {
		return false
	}
	return lo.EveryBy(existingPCSGReplicaPCLQs, func(pclq grovecorev1alpha1.PodClique) bool {
		return pclq.Labels[apicommon.LabelPodTemplateHash] == sc.expectedPCLQPodTemplateHashMap[pclq.Name]
	})
}

func isReplicaUpdated(expectedPCLQPodTemplateHashes map[string]string, pcsgReplicaPCLQs []grovecorev1alpha1.PodClique) (bool, error) {
	for _, pclq := range pcsgReplicaPCLQs {
		podTemplateHash, ok := pclq.Labels[apicommon.LabelPodTemplateHash]
		if !ok {
			return false, groveerr.ErrMissingPodTemplateHashLabel
		}
		if podTemplateHash != expectedPCLQPodTemplateHashes[apicommon.LabelPodGangSetReplicaIndex] {
			return false, nil
		}
	}
	return true, nil
}

func isReplicaDeletedOrMarkedForDeletion(pcsgReplicaPCLQs []grovecorev1alpha1.PodClique) bool {
	if len(pcsgReplicaPCLQs) == 0 {
		return true
	}
	return lo.EveryBy(pcsgReplicaPCLQs, func(pclq grovecorev1alpha1.PodClique) bool {
		return k8sutils.IsResourceTerminating(pclq.ObjectMeta)
	})
}

func getReplicaState(pcsgReplicaPCLQs []grovecorev1alpha1.PodClique) replicaState {
	// TODO
	return replicaStatePending
}
