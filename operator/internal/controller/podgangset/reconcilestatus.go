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

package podgangset

import (
	"context"
	"fmt"
	"strconv"

	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	ctrlcommon "github.com/NVIDIA/grove/operator/internal/controller/common"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) reconcileStatus(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet) ctrlcommon.ReconcileStepResult {
	// Calculate available replicas using PCSG-inspired approach
	err := r.mutateReplicas(ctx, logger, pgs)
	if err != nil {
		return ctrlcommon.ReconcileWithErrors("failed to mutate replicas status", err)
	}

	// Update the PodGangSet status
	if err = r.client.Status().Update(ctx, pgs); err != nil {
		return ctrlcommon.ReconcileWithErrors("failed to update PodGangSet status", err)
	}
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) mutateReplicas(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet) error {
	// Set basic replica count
	pgs.Status.Replicas = pgs.Spec.Replicas
	availableReplicas, err := r.computeAvailableReplicas(ctx, logger, pgs)
	if err != nil {
		return fmt.Errorf("could not compute available replicas: %w", err)
	}
	pgs.Status.AvailableReplicas = availableReplicas
	return nil
}

// computeAvailableReplicas calculates the number of available replicas for a PodGangSet.
// It checks both standalone PodCliques and PodCliqueScalingGroups to determine availability.
// A replica is considered available if it has all its required components (PCSGs and standalone PCLQs) available.
func (r *Reconciler) computeAvailableReplicas(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet) (int32, error) {
	var availableReplicas int32
	// Calculate expected resource counts per replica (same for all replicas)
	expectedStandalonePCLQs, expectedPCSGs := r.computeExpectedResourceCounts(pgs)

	// Fetch all PCSGs for this PGS
	pcsgs, err := componentutils.GetPCSGsForPGS(ctx, r.client, client.ObjectKeyFromObject(pgs))
	if err != nil {
		return -1, err
	}

	// Fetch all standalone PodCliques for this PGS
	standalonePCLQs, err := componentutils.GetPodCliquesWithParentPGS(ctx, r.client, client.ObjectKeyFromObject(pgs))
	if err != nil {
		return -1, err
	}

	// Group both resources by PGS replica index
	standalonePCLQsByReplica := componentutils.GroupPCLQsByPGSReplicaIndex(standalonePCLQs)
	pcsgsByReplica := componentutils.GroupPCSGsByPGSReplicaIndex(pcsgs)

	for replicaIndex := 0; replicaIndex < int(pgs.Spec.Replicas); replicaIndex++ {
		replicaIndexStr := strconv.Itoa(replicaIndex)
		replicaStandalonePCLQs := standalonePCLQsByReplica[replicaIndexStr]
		replicaPCSGs := pcsgsByReplica[replicaIndexStr]
		// Check if this PGS replica is available based on all its components
		isReplicaAvailable := r.isPGSReplicaAvailable(logger, pgs, replicaIndex, replicaPCSGs,
			replicaStandalonePCLQs, expectedPCSGs, expectedStandalonePCLQs)

		if isReplicaAvailable {
			availableReplicas++
		}
	}

	logger.Info("Calculated available replicas for PGS", "pgs", client.ObjectKeyFromObject(pgs), "availableReplicas", availableReplicas, "totalReplicas", pgs.Spec.Replicas)
	return availableReplicas, nil
}

func (r *Reconciler) computeExpectedResourceCounts(pgs *grovecorev1alpha1.PodGangSet) (expectedStandalonePCLQs, expectedPCSGs int) {
	// Count expected PCSGs - this is the number of unique scaling group configs
	expectedPCSGs = len(pgs.Spec.Template.PodCliqueScalingGroupConfigs)

	// Count expected standalone PodCliques - cliques that are NOT part of any scaling group
	expectedStandalonePCLQs = 0
	for _, cliqueSpec := range pgs.Spec.Template.Cliques {
		pcsgConfig := componentutils.FindScalingGroupConfigForClique(pgs.Spec.Template.PodCliqueScalingGroupConfigs, cliqueSpec.Name)
		if pcsgConfig == nil {
			// This clique is not part of any scaling group, so it's standalone
			expectedStandalonePCLQs++
		}
	}

	return expectedStandalonePCLQs, expectedPCSGs
}

func (r *Reconciler) isPGSReplicaAvailable(logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, replicaIndex int, replicaPCSGs []grovecorev1alpha1.PodCliqueScalingGroup, standalonePCLQs []grovecorev1alpha1.PodClique, expectedPCSGs int, expectedStandalonePCLQs int) bool {
	if !r.checkPGSReplicaStandalonePCLQsAvailability(logger, pgs, expectedStandalonePCLQs, replicaIndex, standalonePCLQs) {
		logger.Info("PGS replica is not available due to insufficient standalone PodCliques",
			"pgs", client.ObjectKeyFromObject(pgs), "replicaIndex", replicaIndex,
			"expectedStandalonePCLQs", expectedStandalonePCLQs, "actualPCLQsCount", len(standalonePCLQs))
		return false
	}

	if !r.checkPGSCReplicaPCSGsAvailability(logger, pgs, expectedPCSGs, replicaIndex, replicaPCSGs) {
		logger.Info("PGS replica is not available due to insufficient PodCliqueScalingGroups",
			"pgs", client.ObjectKeyFromObject(pgs), "replicaIndex", replicaIndex,
			"expectedPCSGs", expectedPCSGs, "actualPCSGsCount", len(replicaPCSGs))
		return false
	}

	logger.Info("PGS replica is available", "pgs", client.ObjectKeyFromObject(pgs), "replicaIndex", replicaIndex)
	return true
}

func (r *Reconciler) checkPGSReplicaStandalonePCLQsAvailability(logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, expectedStandalonePCLQs, replicaIndex int, pclqs []grovecorev1alpha1.PodClique) bool {
	nonTerminatedPCLQs := lo.Filter(pclqs, func(pclq grovecorev1alpha1.PodClique, _ int) bool {
		return !k8sutils.IsResourceTerminating(pclq.ObjectMeta)
	})
	if len(nonTerminatedPCLQs) < expectedStandalonePCLQs {
		logger.Info("PGS replica does not have all expected PodCliques",
			"pgs", client.ObjectKeyFromObject(pgs), "replicaIndex", replicaIndex,
			"expectedStandalonePCLQs", expectedStandalonePCLQs, "actualPCLQsCount", len(pclqs))
		return false
	}

	isAvailable := lo.EveryBy(nonTerminatedPCLQs, func(pclq grovecorev1alpha1.PodClique) bool {
		return pclq.Status.ReadyReplicas >= *pclq.Spec.MinAvailable
	})

	logger.Info("PGS replica availability status", "pgs",
		client.ObjectKeyFromObject(pgs), "replicaIndex", replicaIndex, "isAvailable", isAvailable)
	return isAvailable
}

func (r *Reconciler) checkPGSCReplicaPCSGsAvailability(logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, expectedPCSGs, replicaIndex int, pcsgs []grovecorev1alpha1.PodCliqueScalingGroup) bool {
	nonTerminatedPCSGs := lo.Filter(pcsgs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup, _ int) bool {
		return !k8sutils.IsResourceTerminating(pcsg.ObjectMeta)
	})
	if len(nonTerminatedPCSGs) < expectedPCSGs {
		logger.Info("PGS replica does not have all expected PodCliqueScalingGroups",
			"pgs", client.ObjectKeyFromObject(pgs), "replicaIndex", replicaIndex,
			"expectedPCSGs", expectedPCSGs, "actualPCSGsCount", len(pcsgs))
		return false
	}
	isAvailable := lo.EveryBy(nonTerminatedPCSGs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup) bool {
		return pcsg.Status.AvailableReplicas >= *pcsg.Spec.MinAvailable
	})

	logger.Info("PGS replica PCSG availability status", "pgs",
		client.ObjectKeyFromObject(pgs), "replicaIndex", replicaIndex, "isAvailable", isAvailable)
	return isAvailable
}
