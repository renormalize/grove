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

package podcliquescalinggroup

import (
	"context"
	"fmt"

	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	ctrlcommon "github.com/NVIDIA/grove/operator/internal/controller/common"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) reconcileStatus(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) ctrlcommon.ReconcileStepResult {
	pgs, err := componentutils.GetOwnerPodGangSet(ctx, r.client, pcsg.ObjectMeta)
	if err != nil {
		return ctrlcommon.ReconcileWithErrors("failed to get owner PodGangSet", err)
	}

	pclqsPerPCSGReplica, err := r.getPodCliquesPerPCSGReplica(ctx, pgs.Name, client.ObjectKeyFromObject(pcsg))
	if err != nil {
		return ctrlcommon.ReconcileWithErrors(fmt.Sprintf("failed to list PodCliques for PodCliqueScalingGroup: %q", client.ObjectKeyFromObject(pcsg)), err)
	}
	mutateReplicas(logger, pcsg, pclqsPerPCSGReplica)
	mutateMinAvailableBreachedCondition(logger, pcsg, pclqsPerPCSGReplica)

	if err = mutateSelector(pgs, pcsg); err != nil {
		logger.Error(err, "failed to update selector for PodCliqueScalingGroup")
		return ctrlcommon.ReconcileWithErrors("failed to update selector for PodCliqueScalingGroup", err)
	}

	if err = r.client.Status().Update(ctx, pcsg); err != nil {
		return ctrlcommon.ReconcileWithErrors("failed to update the status with label selector and replicas", err)
	}

	return ctrlcommon.ContinueReconcile()
}

func mutateReplicas(logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pclqsPerPCSGReplica map[string][]grovecorev1alpha1.PodClique) {
	pcsg.Status.Replicas = pcsg.Spec.Replicas
	var scheduledReplicas, availableReplicas int32
	for pcsgReplicaIndex, pclqs := range pclqsPerPCSGReplica {
		isScheduled, isAvailable := computeReplicaStatus(logger, len(pcsg.Spec.CliqueNames), pcsgReplicaIndex, pclqs, *pcsg.Spec.MinAvailable)
		if isScheduled {
			scheduledReplicas++
		}
		if isAvailable {
			availableReplicas++
		}
	}
	logger.Info("Mutating PodCliqueScalingGroup replicas",
		"pcsg", client.ObjectKeyFromObject(pcsg),
		"scheduledReplicas", scheduledReplicas, "availableReplicas", availableReplicas)
	pcsg.Status.ScheduledReplicas = scheduledReplicas
	pcsg.Status.AvailableReplicas = availableReplicas
}

// computeReplicaStatus processes a single PodCliqueScalingGroup replica and returns whether it is scheduled and available.
func computeReplicaStatus(logger logr.Logger, expectedPCSGReplicaPCLQSize int, pcsgReplicaIndex string, pclqs []grovecorev1alpha1.PodClique, minAvailable int32) (bool, bool) {
	var isAvailable, isScheduled bool
	nonTerminatedPCSGPodCliques := lo.Filter(pclqs, func(pclq grovecorev1alpha1.PodClique, _ int) bool {
		return !k8sutils.IsResourceTerminating(pclq.ObjectMeta)
	})
	if len(nonTerminatedPCSGPodCliques) != expectedPCSGReplicaPCLQSize {
		logger.V(1).Info("PCSG replica does not have the expected number of PodCliques",
			"pcsgReplicaIndex", pcsgReplicaIndex,
			"expectedPCSGReplicaPCLQSize", expectedPCSGReplicaPCLQSize,
			"actualPCSGReplicaPCLQSize", len(nonTerminatedPCSGPodCliques))
		return false, false
	}
	isScheduled = lo.EveryBy(nonTerminatedPCSGPodCliques, func(pclq grovecorev1alpha1.PodClique) bool {
		return k8sutils.IsConditionTrue(pclq.Status.Conditions, grovecorev1alpha1.ConditionTypePodCliqueScheduled)
	})
	// A PodClique is considered available if it schedules at least MinAvailable pods.
	if isScheduled {
		isAvailable = lo.EveryBy(nonTerminatedPCSGPodCliques, func(pclq grovecorev1alpha1.PodClique) bool {
			return pclq.Status.ReadyReplicas >= minAvailable
		})
	}
	return isScheduled, isAvailable
}

func mutateMinAvailableBreachedCondition(logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pclqsPerPCSGReplica map[string][]grovecorev1alpha1.PodClique) {
	newCondition := computeMinAvailableBreachedCondition(logger, pcsg, pclqsPerPCSGReplica)
	if k8sutils.HasConditionChanged(pcsg.Status.Conditions, newCondition) {
		logger.Info("Updating MinAvailableBreached condition for PodCliqueScalingGroup",
			"pcsg", client.ObjectKeyFromObject(pcsg),
			"conditionType", newCondition.Type,
			"conditionStatus", newCondition.Status,
			"reason", newCondition.Reason)
		meta.SetStatusCondition(&pcsg.Status.Conditions, newCondition)
	}
}

func computeMinAvailableBreachedCondition(logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pclqsPerPCSGReplica map[string][]grovecorev1alpha1.PodClique) metav1.Condition {
	minAvailable := int(*pcsg.Spec.MinAvailable)
	scheduledReplicas := int(pcsg.Status.ScheduledReplicas)
	if scheduledReplicas < minAvailable {
		return metav1.Condition{
			Type:    grovecorev1alpha1.ConditionTypeMinAvailableBreached,
			Status:  metav1.ConditionFalse,
			Reason:  grovecorev1alpha1.ConditionReasonInsufficientScheduledPCSGReplicas,
			Message: fmt.Sprintf("Insufficient scheduled replicas. expected at least: %d, found: %d", minAvailable, scheduledReplicas),
		}
	}
	minAvailableBreachedReplicas := computeMinAvailableBreachedReplicas(logger, pclqsPerPCSGReplica)
	availableReplicas := scheduledReplicas - minAvailableBreachedReplicas
	if availableReplicas < minAvailable {
		return metav1.Condition{
			Type:    grovecorev1alpha1.ConditionTypeMinAvailableBreached,
			Status:  metav1.ConditionTrue,
			Reason:  grovecorev1alpha1.ConditionReasonInsufficientAvailablePCSGReplicas,
			Message: fmt.Sprintf("Insufficient PodCliqueScalingGroup ready replicas, expected at least: %d, found: %d", minAvailable, availableReplicas),
		}
	}
	return metav1.Condition{
		Type:    grovecorev1alpha1.ConditionTypeMinAvailableBreached,
		Status:  metav1.ConditionFalse,
		Reason:  grovecorev1alpha1.ConditionReasonSufficientAvailablePCSGReplicas,
		Message: fmt.Sprintf("Sufficient PodCliqueScalingGroup ready replicas, expected at least: %d, found: %d", minAvailable, availableReplicas),
	}
}

func computeMinAvailableBreachedReplicas(logger logr.Logger, pclqsPerPCSGReplica map[string][]grovecorev1alpha1.PodClique) int {
	var breachedReplicas int
	for pcsgReplicaIndex, pclqs := range pclqsPerPCSGReplica {
		isMinAvailableBreached := lo.Reduce(pclqs, func(agg bool, pclq grovecorev1alpha1.PodClique, _ int) bool {
			return agg || k8sutils.IsConditionTrue(pclq.Status.Conditions, grovecorev1alpha1.ConditionTypeMinAvailableBreached)
		}, false)
		if isMinAvailableBreached {
			breachedReplicas++
		}
		logger.Info("PodCliqueScalingGroup replica has MinAvailableBreached condition set to true", "pcsgReplicaIndex", pcsgReplicaIndex, "isMinAvailableBreached", isMinAvailableBreached)
	}
	return breachedReplicas
}

func (r *Reconciler) getPodCliquesPerPCSGReplica(ctx context.Context, pgsName string, pcsgObjKey client.ObjectKey) (map[string][]grovecorev1alpha1.PodClique, error) {
	selectorLabels := lo.Assign(
		k8sutils.GetDefaultLabelsForPodGangSetManagedResources(pgsName),
		map[string]string{
			grovecorev1alpha1.LabelPodCliqueScalingGroup: pcsgObjKey.Name,
			grovecorev1alpha1.LabelComponentKey:          grovecorev1alpha1.LabelComponentPCSGPodCliqueValue,
		},
	)
	pclqs, err := componentutils.GetPCLQsByOwner(ctx,
		r.client,
		grovecorev1alpha1.PodCliqueScalingGroupKind,
		pcsgObjKey,
		selectorLabels,
	)
	if err != nil {
		return nil, err
	}
	pclqsPerPCSGReplica := componentutils.GroupPCLQsByPCSGReplicaIndex(pclqs)
	return pclqsPerPCSGReplica, nil
}

func mutateSelector(pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
	pgsReplicaIndex, err := k8sutils.GetPodGangSetReplicaIndex(pcsg.ObjectMeta)
	if err != nil {
		return err
	}
	matchingPCSGConfig, ok := lo.Find(pgs.Spec.Template.PodCliqueScalingGroupConfigs, func(pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig) bool {
		pcsgFQN := grovecorev1alpha1.GeneratePodCliqueScalingGroupName(grovecorev1alpha1.ResourceNameReplica{Name: pgs.Name, Replica: pgsReplicaIndex}, pcsgConfig.Name)
		return pcsgFQN == pcsg.Name
	})
	if !ok {
		// This should ideally never happen but if you find a PCSG that is not defined in PGS then just ignore it.
		return nil
	}
	// No ScaleConfig has been defined of this PCSG, therefore there is no need to add a selector in the status.
	if matchingPCSGConfig.ScaleConfig == nil {
		return nil
	}
	labels := lo.Assign(
		k8sutils.GetDefaultLabelsForPodGangSetManagedResources(pgs.Name),
		map[string]string{
			grovecorev1alpha1.LabelPodCliqueScalingGroup: pcsg.Name,
		},
	)
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: labels})
	if err != nil {
		return fmt.Errorf("%w: failed to create label selector for PodCliqueScalingGroup %v", err, client.ObjectKeyFromObject(pcsg))
	}
	pcsg.Status.Selector = ptr.To(selector.String())
	return nil
}
