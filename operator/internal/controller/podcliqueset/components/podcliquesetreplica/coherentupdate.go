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
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// orchestrateCoherentUpdate manages the coherent update process for PodCliqueSet replicas.
func (r _resource) orchestrateCoherentUpdate(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsIndicesToTerminate []int) error {
	updateWork, err := r.computeCoherentPendingWork(ctx, pcs, pcsIndicesToTerminate)
	if err != nil {
		return err
	}

	if len(pcs.Status.UpdateProgress.CurrentlyUpdating) > 0 {
		// TODO: check InFlightPodGangs availability, advance iteration
		return groveerr.New(
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("coherent update of PodCliqueSet replica index %d in progress", pcs.Status.UpdateProgress.CurrentlyUpdating[0].ReplicaIndex),
		)
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
