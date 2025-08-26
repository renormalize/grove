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

	apicommon "github.com/NVIDIA/grove/operator/api/common"
	"github.com/NVIDIA/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	ctrlcommon "github.com/NVIDIA/grove/operator/internal/controller/common"
	ctrlutils "github.com/NVIDIA/grove/operator/internal/controller/utils"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) reconcileStatus(ctx context.Context, logger logr.Logger, pcsgObjectKey client.ObjectKey) ctrlcommon.ReconcileStepResult {
	// It is important that we re-fetch the PodCliqueScalingGroup. In case rolling update has been started during the spec reconciliation,
	// then UpdateInProgress condition will be set. It is essential that this is checked when computing status.
	// It is a possibility that the informer cache does not reflect the changes that are made to status conditions are not immediately reflected.
	// However, it is currently assumed that eventually this condition will be visible eventually. We will think of alleviating this delay
	// in the future.
	pcsg := &grovecorev1alpha1.PodCliqueScalingGroup{}
	if result := ctrlutils.GetPodCliqueScalingGroup(ctx, r.client, logger, pcsgObjectKey, pcsg); ctrlcommon.ShortCircuitReconcileFlow(result) {
		return result
	}
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
		return k8sutils.IsConditionTrue(pclq.Status.Conditions, constants.ConditionTypePodCliqueScheduled)
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

// computeMinAvailableBreachedCondition computes the MinAvailableBreached condition for the PodCliqueScalingGroup.
// If rolling update is under progress, then gang termination for this PCSG is disabled. This is achieved by marking the status to `Unknown`. This PCSG will not influence
// the gang termination of PGS replica till its update has completed.
// If the number of scheduled replicas is less than the MinAvailable, then it is too pre-mature to set the MinAvailableBreached condition to true.
// If we set MinAvailableBreached condition to true, then it can result in pre-mature gang termination when the PodClique Pods are still starting.
// If there are sufficient scheduled replicas (i.e. scheduledReplicas >= minAvailable), then we can compute the MinAvailableBreached condition based on the number of ready replicas.
func computeMinAvailableBreachedCondition(logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pclqsPerPCSGReplica map[string][]grovecorev1alpha1.PodClique) metav1.Condition {
	if componentutils.IsPCSGUpdateInProgress(pcsg) {
		return metav1.Condition{
			Type:    constants.ConditionTypeMinAvailableBreached,
			Status:  metav1.ConditionUnknown,
			Reason:  constants.ConditionReasonUpdateInProgress,
			Message: "Update is in progress",
		}
	}

	minAvailable := int(*pcsg.Spec.MinAvailable)
	scheduledReplicas := int(pcsg.Status.ScheduledReplicas)
	if scheduledReplicas < minAvailable {
		return metav1.Condition{
			Type:    constants.ConditionTypeMinAvailableBreached,
			Status:  metav1.ConditionFalse,
			Reason:  constants.ConditionReasonInsufficientScheduledPCSGReplicas,
			Message: fmt.Sprintf("Insufficient scheduled replicas. expected at least: %d, found: %d", minAvailable, scheduledReplicas),
		}
	}
	minAvailableBreachedReplicas := computeMinAvailableBreachedReplicas(logger, pclqsPerPCSGReplica)
	availableReplicas := scheduledReplicas - minAvailableBreachedReplicas
	if availableReplicas < minAvailable {
		return metav1.Condition{
			Type:    constants.ConditionTypeMinAvailableBreached,
			Status:  metav1.ConditionTrue,
			Reason:  constants.ConditionReasonInsufficientAvailablePCSGReplicas,
			Message: fmt.Sprintf("Insufficient PodCliqueScalingGroup ready replicas, expected at least: %d, found: %d", minAvailable, availableReplicas),
		}
	}
	return metav1.Condition{
		Type:    constants.ConditionTypeMinAvailableBreached,
		Status:  metav1.ConditionFalse,
		Reason:  constants.ConditionReasonSufficientAvailablePCSGReplicas,
		Message: fmt.Sprintf("Sufficient PodCliqueScalingGroup ready replicas, expected at least: %d, found: %d", minAvailable, availableReplicas),
	}
}

func computeMinAvailableBreachedReplicas(logger logr.Logger, pclqsPerPCSGReplica map[string][]grovecorev1alpha1.PodClique) int {
	var breachedReplicas int
	for pcsgReplicaIndex, pclqs := range pclqsPerPCSGReplica {
		isMinAvailableBreached := lo.Reduce(pclqs, func(agg bool, pclq grovecorev1alpha1.PodClique, _ int) bool {
			return agg || k8sutils.IsConditionTrue(pclq.Status.Conditions, constants.ConditionTypeMinAvailableBreached)
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
		apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgsName),
		map[string]string{
			apicommon.LabelPodCliqueScalingGroup: pcsgObjKey.Name,
			apicommon.LabelComponentKey:          apicommon.LabelComponentNamePodCliqueScalingGroupPodClique,
		},
	)
	pclqs, err := componentutils.GetPCLQsByOwner(ctx,
		r.client,
		constants.KindPodCliqueScalingGroup,
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
		pcsgFQN := apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: pgs.Name, Replica: pgsReplicaIndex}, pcsgConfig.Name)
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
		apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgs.Name),
		map[string]string{
			apicommon.LabelPodCliqueScalingGroup: pcsg.Name,
		},
	)
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: labels})
	if err != nil {
		return fmt.Errorf("%w: failed to create label selector for PodCliqueScalingGroup %v", err, client.ObjectKeyFromObject(pcsg))
	}
	pcsg.Status.Selector = ptr.To(selector.String())
	return nil
}
