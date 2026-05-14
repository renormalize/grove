// /*
// Copyright 2025 The Grove Authors.
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
	"slices"

	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	k8sutils "github.com/ai-dynamo/grove/operator/internal/utils/kubernetes"

	groveschedulerv1alpha1 "github.com/ai-dynamo/grove/scheduler/api/core/v1alpha1"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// orchestrateCoherentUpdate manages the coherent update process for PodCliqueSet replicas.
// The orchestrator's responsibilities are narrowed to state machine progression:
//   - Pick the next replica to update
//   - Wait for InFlightPodGangs to become Available (PodGangConditionTypeAvailable)
//   - Advance to the next iteration or mark replica/update as complete
//
// It does NOT compute PodGangMap entries (PodGangMap component does that),
// does NOT mutate PodGang resources (PodGang component does that),
// and does NOT delete pods (PCLQ reconciler handles that).
func (r _resource) orchestrateCoherentUpdate(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsIndicesToTerminate []int) error {
	updateWork, err := r.computeCoherentPendingWork(ctx, pcs, pcsIndicesToTerminate)
	if err != nil {
		return err
	}

	if len(pcs.Status.UpdateProgress.CurrentlyUpdating) > 0 && pcs.Status.UpdateProgress.CurrentlyUpdating[0].UpdateEndedAt == nil {
		return r.checkAndAdvanceCoherentUpdate(ctx, logger, pcs, updateWork)
	}

	nextReplica := updateWork.getNextPendingReplicaByIndex()
	if nextReplica != nil {
		logger.Info("Initiating coherent update for next replica index", "nextReplicaIndex", *nextReplica)
		original := pcs.DeepCopy()
		pcs.Status.UpdateProgress.CurrentlyUpdating = []grovecorev1alpha1.PodCliqueSetReplicaUpdateProgress{
			{
				ReplicaIndex:    int32(*nextReplica),
				UpdateStartedAt: metav1.Now(),
			},
		}
		return r.patchUpdateProgressStatus(ctx, logger, pcs, original)
	}

	logger.Info("Coherent update has completed")
	original := pcs.DeepCopy()
	pcs.Status.UpdateProgress.UpdateEndedAt = ptr.To(metav1.Now())
	pcs.Status.UpdateProgress.CurrentlyUpdating = nil
	return r.patchUpdateProgressStatus(ctx, logger, pcs, original)
}

// checkAndAdvanceCoherentUpdate checks if the current in-flight PodGangs are Available.
// If they are not yet Available, it re-queues. If they are Available and the replica is fully
// updated, it marks the replica as done. Otherwise, it clears InFlightPodGangs and re-queues
// so that the PodGangMap component can compute the next iteration's entries.
func (r _resource) checkAndAdvanceCoherentUpdate(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, updateWork *coherentPendingWork) error {
	// NOTE: While the API allows more than one PCS replica to be updated concurrently, none of the update strategies
	// currently supported allow more than one PCS replica to be updated. Therefore, we only check for index 0 of `CurrentlyUpdating`
	// If and when concurrent PCS replica update is supported then we should iterate over all currently updating replicas.
	currentProgress := &pcs.Status.UpdateProgress.CurrentlyUpdating[0]
	replicaIndex := currentProgress.ReplicaIndex

	if len(currentProgress.InFlightPodGangs) == 0 {
		return groveerr.New(
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("waiting for PodGangMap to compute next update intent for replica %d", replicaIndex),
		)
	}

	// Check if all in-flight PodGangs have become Available.
	for _, pgName := range currentProgress.InFlightPodGangs {
		pg, err := componentutils.GetPodGang(ctx, r.client, pgName, pcs.Namespace)
		if err != nil {
			return groveerr.WrapError(err,
				errCodeListPCLQs,
				component.OperationSync,
				fmt.Sprintf("failed to get PodGang %s to check availability", pgName),
			)
		}
		if !k8sutils.IsConditionTrue(pg.Status.Conditions, string(groveschedulerv1alpha1.PodGangConditionTypeAvailable)) {
			logger.Info("Waiting for in-flight PodGangs to become Available",
				"replicaIndex", replicaIndex,
				"inFlightPodGangs", currentProgress.InFlightPodGangs)
			return groveerr.New(
				groveerr.ErrCodeContinueReconcileAndRequeue,
				component.OperationSync,
				fmt.Sprintf("coherent update of PodCliqueSet replica %d in progress, waiting for PodGang %s to become Available", replicaIndex, pgName),
			)
		}
	}

	// All in-flight PodGangs are Available. Check if this replica is fully updated.
	if slices.Contains(updateWork.doneReplicaIndices, int(replicaIndex)) {
		logger.Info("Coherent update for replica completed", "replicaIndex", replicaIndex)
		return r.markCurrentReplicaUpdateDone(ctx, logger, pcs)
	}

	// Replica not fully updated — clear InFlightPodGangs and requeue.
	// The PodGangMap component will compute the next iteration's entries on the next reconcile.
	logger.Info("Current iteration complete, seeking next update target", "replicaIndex", replicaIndex)
	original := pcs.DeepCopy()
	pcs.Status.UpdateProgress.CurrentlyUpdating[0].InFlightPodGangs = nil
	return r.patchUpdateProgressStatus(ctx, logger, pcs, original)
}

// computeCoherentPendingWork identifies which replicas still need updating vs. which are done.
func (r _resource) computeCoherentPendingWork(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet, pcsIndicesToTerminate []int) (*coherentPendingWork, error) {
	replicaInfos, err := r.getPCSReplicaInfos(ctx, pcs, pcsIndicesToTerminate)
	if err != nil {
		return nil, err
	}
	work := &coherentPendingWork{}
	for _, ri := range replicaInfos {
		ri.computeUpdateProgress(pcs)
		if ri.updateDone {
			work.doneReplicaIndices = append(work.doneReplicaIndices, ri.replicaIndex)
		} else {
			work.pendingReplicaIndices = append(work.pendingReplicaIndices, ri.replicaIndex)
		}
	}
	return work, nil
}

// coherentPendingWork tracks PCS replicas that have been updated and
// replicas that are pending updates.
type coherentPendingWork struct {
	pendingReplicaIndices []int
	doneReplicaIndices    []int
}

func (w *coherentPendingWork) getNextPendingReplicaByIndex() *int {
	if len(w.pendingReplicaIndices) == 0 {
		return nil
	}
	slices.Sort(w.pendingReplicaIndices)
	return &w.pendingReplicaIndices[0]
}
