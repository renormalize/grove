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

package podgang

import (
	"context"
	"errors"
	"fmt"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/clustertopology"
	"github.com/ai-dynamo/grove/operator/internal/constants"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	k8sutils "github.com/ai-dynamo/grove/operator/internal/utils/kubernetes"

	groveschedulerv1alpha1 "github.com/ai-dynamo/grove/scheduler/api/core/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// prepareSyncFlow computes the required state for synchronizing PodGang resources.
func (r _resource) prepareSyncFlow(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet) (sc *syncContext, err error) {
	pcsObjectKey := client.ObjectKeyFromObject(pcs)
	sc = &syncContext{
		pcs:                  pcs,
		logger:               logger,
		existingPCLQPods:     make(map[string][]corev1.Pod),
		unassignedPodsByPCLQ: make(map[string][]corev1.Pod),
	}

	sc.existingPCLQs, err = r.getExistingPCLQsForPCS(ctx, pcs)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodCliques,
			component.OperationSync,
			fmt.Sprintf("failed to list PodCliques for PodCliqueSet %v", pcsObjectKey),
		)
	}
	sc.existingPCLQByName = componentutils.PodCliqueByName(sc.existingPCLQs)

	sc.existingPCSGs, err = r.getExistingPCSGsForPCS(ctx, pcs)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodCliqueScalingGroups,
			component.OperationSync,
			fmt.Sprintf("failed to list PodCliqueScalingGroups for PodCliqueSet %v", pcsObjectKey),
		)
	}
	sc.existingPCSGByName = componentutils.PCSGByName(sc.existingPCSGs)

	sc.tasEnabled = r.tasConfig.Enabled
	if r.tasConfig.Enabled && componentutils.HasAnyTopologyConstraint(pcs) {
		topologyName, resolveErr := componentutils.ResolveTopologyNameForPodCliqueSet(pcs)
		if resolveErr == nil && topologyName != "" {
			sc.topologyLevels, err = clustertopology.GetClusterTopologyLevels(ctx, r.client, topologyName)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return nil, groveerr.WrapError(err,
						errCodeGetClusterTopologyLevels,
						component.OperationSync,
						fmt.Sprintf("failed to get cluster topology levels for %q", topologyName))
				}
				sc.logger.Info(
					"ClusterTopology not found while preparing PodGang sync; continuing without translated topology constraints",
					"pcs", pcsObjectKey,
					"topologyName", topologyName,
				)
				sc.topologyLevels = nil
			}
		}
		// If topologyName resolution fails, sc.topologyLevels stays nil — the PCS reconciler
		// handles this via the TopologyNameMissing condition.
	}

	if err = r.computeExpectedPodGangs(ctx, sc); err != nil {
		return nil, groveerr.WrapError(err,
			errCodeComputeExistingPodGangs,
			component.OperationSync,
			fmt.Sprintf("failed to compute existing PodGangs for PodCliqueSet %v", pcsObjectKey),
		)
	}
	sc.expectedPodGangByName = podGangInfoByName(sc.expectedPodGangs)
	sc.expectedPodGangNameSet = podGangInfoNameSet(sc.expectedPodGangs)

	sc.existingPodGangs, err = componentutils.GetExistingPodGangs(ctx, r.client, pcs.ObjectMeta, pcs.Namespace)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodGangs,
			component.OperationSync,
			fmt.Sprintf("Failed to get existing PodGangs for PodCliqueSet: %v", client.ObjectKeyFromObject(sc.pcs)),
		)
	}
	sc.existingPodGangByName = componentutils.PodGangByName(sc.existingPodGangs)

	sc.existingPCLQPods, err = r.getExistingPodsByPCLQForPCS(ctx, pcsObjectKey)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPods,
			component.OperationSync,
			fmt.Sprintf("failed to list Pods for PodCliqueSet %v", pcsObjectKey),
		)
	}
	sc.initializeAssignedAndUnassignedPodsForPCS()

	return sc, nil
}

// getExistingPCLQsForPCS fetches all existing PodCliques managed by the PodCliqueSet.
func (r _resource) getExistingPCLQsForPCS(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet) ([]grovecorev1alpha1.PodClique, error) {
	pclqList := &grovecorev1alpha1.PodCliqueList{}
	if err := r.client.List(ctx, pclqList,
		client.InNamespace(pcs.Namespace),
		client.MatchingLabels(apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name))); err != nil {
		return nil, err
	}

	// Return all PodCliques with matching labels. PodCliques can be owned either:
	// 1. Directly by PCS (standalone pclqs)
	// 2. By PCSG (scaling group member pclqs) - PCSG itself is owned by PCS
	// Label matching ensures they belong to this PCS, no ownership filter needed.
	return pclqList.Items, nil
}

// computeExpectedPodGangs computes expected PodGangs by reading the PodGangMap for each PCS replica.
func (r _resource) computeExpectedPodGangs(ctx context.Context, sc *syncContext) error {
	for replicaIndex := range int(sc.pcs.Spec.Replicas) {
		pgmName := apicommon.GeneratePodGangMapName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: replicaIndex})
		pgm := &grovecorev1alpha1.PodGangMap{}
		if err := r.client.Get(ctx, client.ObjectKey{Namespace: sc.pcs.Namespace, Name: pgmName}, pgm); err != nil {
			return err
		}
		pcsgReplicaOffset := make(map[string]int32)
		for _, entry := range pgm.Spec.Entries {
			pg, err := r.buildPodGangInfoFromEntry(sc, replicaIndex, entry, pcsgReplicaOffset)
			if err != nil {
				return fmt.Errorf("failed to build PodGang info from entry %q in PodGangMap %s: %w", entry.Name, pgmName, err)
			}
			sc.expectedPodGangs = append(sc.expectedPodGangs, pg)
			for pcsgName, count := range entry.PodCliqueScalingGroups {
				pcsgReplicaOffset[pcsgName] += count
			}
		}
	}
	return nil
}

// buildPodGangInfoFromEntry translates a PodGangEntry into a podGangInfo.
// pcsgReplicaOffset carries the cumulative PCSG replica counts from preceding entries,
// allowing the correct PCLQ FQNs and TopologyConstraintGroupConfig names to be derived
// without scanning the full entry list.
func (r _resource) buildPodGangInfoFromEntry(sc *syncContext, pcsReplicaIndex int, pgEntry grovecorev1alpha1.PodGangEntry, pcsgReplicaOffset map[string]int32) (*podGangInfo, error) {
	pg := &podGangInfo{fqn: pgEntry.Name}

	pg.pclqs = buildStandalonePCLQInfos(sc, pcsReplicaIndex, pgEntry)
	pcsgPCLQs, pcsgConstraints, err := buildPCLQInfosAndTopologyConstraintsForPCSGs(sc, pcsReplicaIndex, pgEntry, pcsgReplicaOffset)
	if err != nil {
		return nil, err
	}
	pg.pclqs = append(pg.pclqs, pcsgPCLQs...)
	pg.pcsgTopologyConstraints = pcsgConstraints
	pg.topologyConstraint = resolvePodGangTopologyConstraint(sc, pgEntry)

	return pg, nil
}

// buildStandalonePCLQInfos builds pclqInfo entries for standalone PodCliques referenced in the entry.
// Iterates template cliques in order to keep the result deterministic.
func buildStandalonePCLQInfos(sc *syncContext, pcsReplicaIndex int, pgEntry grovecorev1alpha1.PodGangEntry) []pclqInfo {
	var pclqs []pclqInfo
	for _, cliqueTemplate := range sc.pcs.Spec.Template.Cliques {
		pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: pcsReplicaIndex}, cliqueTemplate.Name)
		desiredPCLQReplicas, ok := pgEntry.PodCliques[pclqFQN]
		if !ok {
			continue
		}
		pi := pclqInfo{
			fqn:          pclqFQN,
			replicas:     desiredPCLQReplicas,
			minAvailable: *cliqueTemplate.Spec.MinAvailable,
		}
		pi.topologyConstraint = createTopologyPackConstraint(sc, types.NamespacedName{Namespace: sc.pcs.Namespace, Name: pclqFQN}, cliqueTemplate.TopologyConstraint)
		pclqs = append(pclqs, pi)
	}
	return pclqs
}

// buildPCLQInfosAndTopologyConstraintsForPCSGs builds pclqInfo entries and TopologyConstraintGroupConfigs for
// PCSG-owned PodCliques referenced in the entry. Iterates template PCSG configs in order to keep the result deterministic.
func buildPCLQInfosAndTopologyConstraintsForPCSGs(sc *syncContext, pcsReplicaIndex int, pgEntry grovecorev1alpha1.PodGangEntry, pcsgReplicaOffset map[string]int32) ([]pclqInfo, []groveschedulerv1alpha1.TopologyConstraintGroupConfig, error) {
	var (
		pclqs           []pclqInfo
		pcsgConstraints []groveschedulerv1alpha1.TopologyConstraintGroupConfig
	)
	for _, pcsgConfig := range sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		pcsgFQN := apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: pcsReplicaIndex}, pcsgConfig.Name)
		desiredPCSGReplicas, ok := pgEntry.PodCliqueScalingGroups[pcsgFQN]
		if !ok {
			continue
		}
		startIndex := pcsgReplicaOffset[pcsgFQN]
		for replicaIdx := startIndex; replicaIdx < startIndex+desiredPCSGReplicas; replicaIdx++ {
			pclqFQNs := make([]string, 0, len(pcsgConfig.CliqueNames))
			for _, cliqueName := range pcsgConfig.CliqueNames {
				pclqTemplateSpec := componentutils.FindPodCliqueTemplateSpecByName(sc.pcs, cliqueName)
				if pclqTemplateSpec == nil {
					return nil, nil, fmt.Errorf("PCSG %q references clique %q that does not exist in PodCliqueSet %v", pcsgFQN, cliqueName, client.ObjectKeyFromObject(sc.pcs))
				}
				pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsgFQN, Replica: int(replicaIdx)}, cliqueName)
				pi := pclqInfo{
					fqn:          pclqFQN,
					replicas:     pclqTemplateSpec.Spec.Replicas,
					minAvailable: *pclqTemplateSpec.Spec.MinAvailable,
				}
				pi.topologyConstraint = createTopologyPackConstraint(sc, types.NamespacedName{Namespace: sc.pcs.Namespace, Name: pclqFQN}, pclqTemplateSpec.TopologyConstraint)
				pclqs = append(pclqs, pi)
				pclqFQNs = append(pclqFQNs, pclqFQN)
			}
			if pgEntry.TopologyAnchor == grovecorev1alpha1.TopologyAnchorPCS {
				pcsgTopologyConstraint := createTopologyPackConstraint(sc, types.NamespacedName{Namespace: sc.pcs.Namespace, Name: pcsgFQN}, pcsgConfig.TopologyConstraint)
				if pcsgTopologyConstraint != nil {
					pcsgConstraints = append(pcsgConstraints, groveschedulerv1alpha1.TopologyConstraintGroupConfig{
						Name:               fmt.Sprintf("%s-%d", pcsgFQN, replicaIdx),
						PodGroupNames:      pclqFQNs,
						TopologyConstraint: pcsgTopologyConstraint,
					})
				}
			}
		}
	}
	return pclqs, pcsgConstraints, nil
}

// resolvePodGangTopologyConstraint determines the PodGang-level topology constraint based on TopologyAnchor.
func resolvePodGangTopologyConstraint(sc *syncContext, pgEntry grovecorev1alpha1.PodGangEntry) *groveschedulerv1alpha1.TopologyConstraint {
	var tc *groveschedulerv1alpha1.TopologyConstraint
	if grovecorev1alpha1.TopologyAnchorPCSG == pgEntry.TopologyAnchor {
		for pcsgFQN := range pgEntry.PodCliqueScalingGroups {
			pcsgConfig := componentutils.FindScalingGroupConfigByFQN(sc.pcs, pcsgFQN)
			if pcsgConfig != nil && pcsgConfig.TopologyConstraint != nil {
				tc = createTopologyPackConstraint(sc, types.NamespacedName{Namespace: sc.pcs.Namespace, Name: pcsgFQN}, pcsgConfig.TopologyConstraint)
			}
		}
	}
	if tc == nil {
		tc = createTopologyPackConstraint(sc, client.ObjectKeyFromObject(sc.pcs), sc.pcs.Spec.Template.TopologyConstraint)
	}
	return tc
}

// createTopologyPackConstraint creates a TopologyPackConstraint based on the sync context and provided parameters for a resource.
// PackConstraints are defined at multiple levels (PodCliqueSet, PodCliqueScalingGroup, PodClique). This function helps create a TopologyPackConstraint for any of these levels.
func createTopologyPackConstraint(sc *syncContext, nsName types.NamespacedName, requiredTopologyConstraint *grovecorev1alpha1.TopologyConstraint) *groveschedulerv1alpha1.TopologyConstraint {
	// If Topology aware scheduling is disabled, return nil even if TopologyConstraint is specified.
	if !sc.tasEnabled || requiredTopologyConstraint == nil {
		return nil
	}
	var pgPackConstraint *groveschedulerv1alpha1.TopologyPackConstraint
	// If requiredTopologyConstraint is specified, set the required topology key accordingly.
	requiredTopologyLevel, found := lo.Find(sc.topologyLevels, func(topologyLevel grovecorev1alpha1.TopologyLevel) bool {
		return topologyLevel.Domain == requiredTopologyConstraint.PackDomain
	})
	if !found {
		// This can only happen if the ClusterTopology CR has been updated and no longer contains a topology level
		// that is being referenced by the resource's TopologyConstraint.
		// In the current version it's been decided to log this occurrence and skip setting the required constraint which is equivalent
		// to nullifying the required constraint.
		sc.logger.Info("required topology domain not found in cluster topology levels, skipping setting required pack constraint", "namespacedName", nsName, "requiredTopologyConstraint", *requiredTopologyConstraint)
	} else {
		pgPackConstraint = &groveschedulerv1alpha1.TopologyPackConstraint{
			Required: ptr.To(requiredTopologyLevel.Key),
		}
	}
	return lo.Ternary(pgPackConstraint != nil, &groveschedulerv1alpha1.TopologyConstraint{PackConstraint: pgPackConstraint}, nil)
}

// getExistingPCSGsForPCS fetches all existing PCSGs for the PodCliqueSet.
func (r _resource) getExistingPCSGsForPCS(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet) ([]grovecorev1alpha1.PodCliqueScalingGroup, error) {
	pcsgList := &grovecorev1alpha1.PodCliqueScalingGroupList{}
	if err := r.client.List(ctx,
		pcsgList,
		client.InNamespace(pcs.Namespace),
		client.MatchingLabels(
			lo.Assign(
				apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name),
			),
		),
	); err != nil {
		return nil, err
	}
	return lo.Filter(pcsgList.Items, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup, _ int) bool {
		return metav1.IsControlledBy(&pcsg, pcs)
	}), nil
}

// getExistingPodsByPCLQForPCS fetches all non-terminating pods grouped by PodClique.
// It returns a map where the key is the PodClique FQN and the value is a slice of Pods belonging to that PodClique.
func (r _resource) getExistingPodsByPCLQForPCS(ctx context.Context, pcsObjectKey client.ObjectKey) (map[string][]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.client.List(ctx,
		podList,
		client.InNamespace(pcsObjectKey.Namespace),
		client.MatchingLabels(apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcsObjectKey.Name)),
	); err != nil {
		return nil, err
	}

	podsByPCLQ := make(map[string][]corev1.Pod)
	for _, pod := range podList.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		pclqFQN := k8sutils.GetFirstOwnerName(pod.ObjectMeta)
		podsByPCLQ[pclqFQN] = append(podsByPCLQ[pclqFQN], pod)
	}

	return podsByPCLQ, nil
}

// runSyncFlow executes the PodGang synchronization workflow.
func (r _resource) runSyncFlow(ctx context.Context, sc *syncContext) syncFlowResult {
	result := syncFlowResult{}
	if err := r.deleteExcessPodGangs(ctx, sc); err != nil {
		result.errs = append(result.errs, err)
		return result
	}
	return r.createOrUpdatePodGangs(ctx, sc)
}

// deleteExcessPodGangs removes PodGangs that are no longer needed.
func (r _resource) deleteExcessPodGangs(ctx context.Context, sc *syncContext) error {
	excessPodGangs := sc.getExcessPodGangNames()
	namespace := sc.pcs.Namespace
	for _, podGangToDelete := range excessPodGangs {
		pgObjectKey := client.ObjectKey{Namespace: namespace, Name: podGangToDelete}
		pg := emptyPodGang(pgObjectKey)
		sc.logger.Info("Delete excess PodGang", "objectKey", client.ObjectKeyFromObject(pg))
		if err := client.IgnoreNotFound(r.client.Delete(ctx, pg)); err != nil {
			r.eventRecorder.Eventf(sc.pcs, corev1.EventTypeWarning, constants.ReasonPodGangDeleteFailed, "Error deleting PodGang %v: %v", pgObjectKey, err)
			return groveerr.WrapError(err,
				errCodeDeleteExcessPodGang,
				component.OperationSync,
				fmt.Sprintf("failed to delete PodGang %v", pgObjectKey),
			)
		}
		r.eventRecorder.Eventf(sc.pcs, corev1.EventTypeNormal, constants.ReasonPodGangDeleteSuccessful, "Deleted PodGang %v", pgObjectKey)
		sc.deletedPodGangNames = append(sc.deletedPodGangNames, podGangToDelete)
		sc.logger.Info("Triggered delete of excess PodGang", "objectKey", client.ObjectKeyFromObject(pg))
	}
	return nil
}

// createOrUpdatePodGangs creates or updates all expected PodGangs.
// PodGangs are created with empty podReferences, Initialized=False.
// Once all pods are created, PodReferences are populated and the PodGang is marked as Initialized=True.
func (r _resource) createOrUpdatePodGangs(ctx context.Context, sc *syncContext) syncFlowResult {
	result := syncFlowResult{}
	for _, expectedPG := range sc.expectedPodGangs {
		// create or update all expected PodGang.
		if err := r.createOrUpdatePodGang(ctx, sc, expectedPG); err != nil {
			sc.logger.Error(err, "failed to create PodGang", "PodGangName", expectedPG.fqn)
			result.recordError(err)
			return result
		}

		// If the PodGang does not exist and the creation succeeded then record the PodGang creation.
		if !sc.isExistingPodGang(expectedPG.fqn) {
			result.recordPodGangCreation(expectedPG.fqn)
		}

		// Verify all pods are created before proceeding
		if err := r.verifyAllPodsCreated(sc, expectedPG); err != nil {
			sc.logger.Info("Not all pods are created or associated to the PodGang yet", "PodGangName", expectedPG.fqn)
			result.recordError(err)
			continue
		}

		// Update status to set Initialized=True (idempotent - no need to check current state)
		if !sc.isPodGangInitialized(expectedPG.fqn) {
			if err := r.patchPodGangInitializedStatus(ctx, sc, expectedPG.fqn, metav1.ConditionTrue, groveschedulerv1alpha1.ConditionReasonPodGangPodsCreated, "PodGang is fully initialized"); err != nil {
				sc.logger.Error(err, "failed to update Initialized condition in PodGang status", "PodGangName", expectedPG.fqn)
				result.recordError(err)
				continue
			}
		}
	}

	return result
}

// patchPodGangInitializedStatus patches the Initialized condition with the given status.
func (r _resource) patchPodGangInitializedStatus(ctx context.Context, sc *syncContext, podGangName string, status metav1.ConditionStatus, reason, message string) error {
	// Create a PodGang object with only the status we want to patch
	statusPatch := &groveschedulerv1alpha1.PodGang{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podGangName,
			Namespace: sc.pcs.Namespace,
		},
	}
	setOrUpdateInitializedCondition(statusPatch, status, reason, message)
	// One could argue why not use Status.Phase to also denote Initialized condition. For now the argument is that
	// current set of phases (Pending, Starting, Running) is influenced by the status of constituent Pods w.r.t their
	// scheduling state, whereas initialized condition is denoting if a PodGang is ready to be scheduled
	// (so it is pre-scheduling phase state). We can always revisit this in future if this reasoning changes.
	statusPatch.Status.Phase = groveschedulerv1alpha1.PodGangPhasePending
	if err := r.client.Status().Patch(ctx, statusPatch, client.Merge); err != nil {
		return err
	}
	sc.logger.Info("Successfully patched PodGang Initialized condition",
		"podGang", podGangName, "status", status)
	return nil
}

// verifyAllPodsCreated checks if all required pods exist before updating PodGang
func (r _resource) verifyAllPodsCreated(sc *syncContext, pgi *podGangInfo) error {
	pclqs := sc.getPodCliques(pgi)
	if len(pclqs) != len(pgi.pclqs) {
		// Not all constituent PCLQs exist yet
		sc.logger.Info("Not all constituent PCLQs exist yet", "podGang", pgi.fqn, "expected", len(pgi.pclqs), "actual", len(pclqs))
		return groveerr.New(groveerr.ErrCodeRequeueAfter,
			component.OperationSync,
			fmt.Sprintf("Waiting for all pods to be created for PodGang %s", pgi.fqn),
		)
	}
	// check the health of each podclique
	numPendingPods := r.getPodsPendingCreationOrAssociation(sc, pgi)
	if numPendingPods > 0 {
		sc.logger.Info("skipping creation of PodGang as all desired replicas have not yet been created or assigned", "podGang", pgi.fqn, "numPendingPodsToCreateOrAssociate", numPendingPods)
		return groveerr.New(groveerr.ErrCodeRequeueAfter,
			component.OperationSync,
			fmt.Sprintf("Waiting for all pods to be created or assigned for PodGang %s", pgi.fqn),
		)
	}
	return nil
}

// getPodsForPodCliquesPendingCreation counts expected pods from non-existent PodCliques.
func (r _resource) getPodsForPodCliquesPendingCreation(sc *syncContext, podGang *podGangInfo) int {
	return lo.Reduce(podGang.pclqs, func(agg int, pclq pclqInfo, _ int) int {
		if _, ok := sc.existingPCLQByName[pclq.fqn]; !ok {
			return agg + int(pclq.replicas)
		}
		return agg
	}, 0)
}

// getPodsPendingCreationOrAssociation counts pods not yet created or labeled for the PodGang.
func (r _resource) getPodsPendingCreationOrAssociation(sc *syncContext, podGang *podGangInfo) int {
	// Find the number of expected pods from PodCliques that are pending creation
	numPodsPendingPCLQCreate := r.getPodsForPodCliquesPendingCreation(sc, podGang)

	// Find the number of pods pending creation of existing PodCliques
	var numPodsPendingCreateOrAssociate int
	pclqs := sc.getPodCliques(podGang)
	for _, pclq := range pclqs {
		existingPCLQPods := sc.existingPCLQPods[pclq.Name]
		// If there is a difference between the expected replicas and the existing pods, we need to account for that.
		// If the difference is positive, it means there are pending pods to create.
		// If the difference is negative, it means there are more existing pods than expected. In this case, we do not need to create any new pods, therefore we can ignore the negative difference.
		numPodsPendingCreateOrAssociate += max(0, int(pclq.Spec.Replicas)-len(existingPCLQPods))

		// For all existing pods in the PCLQ, check if they have the PodGang label set. If that is not set then add them to numPodsPendingCreateOrAssociate.
		for _, existingPod := range existingPCLQPods {
			podGangLabelValue, ok := existingPod.GetLabels()[apicommon.LabelPodGang]
			if !ok {
				sc.logger.Info("Pod does not have a PodGang label yet", "podObjectKey", client.ObjectKeyFromObject(&existingPod), "expectedPodGangName", podGang.fqn)
				numPodsPendingCreateOrAssociate += 1
				continue
			}
			if podGangLabelValue != podGang.fqn {
				sc.logger.Error(nil, "PodGang label does not match expected PodGang name. This should ideally never happen and indicates a coding error", "podObjectKey", client.ObjectKeyFromObject(&existingPod), "expectedPodGangName", podGang.fqn, "podGangLabelValue", podGangLabelValue)
				numPodsPendingCreateOrAssociate += 1
			}
		}
	}
	return numPodsPendingPCLQCreate + numPodsPendingCreateOrAssociate
}

// createOrUpdatePodGang creates or updates a single PodGang resource.
func (r _resource) createOrUpdatePodGang(ctx context.Context, sc *syncContext, pgInfo *podGangInfo) error {
	pgObjectKey := client.ObjectKey{
		Namespace: sc.pcs.Namespace,
		Name:      pgInfo.fqn,
	}
	pg := emptyPodGang(pgObjectKey)
	sc.logger.Info("CreateOrPatch PodGang", "objectKey", pgObjectKey)
	_, err := controllerutil.CreateOrPatch(ctx, r.client, pg, func() error {
		return r.buildResource(sc.pcs, pgInfo, pg)
	})
	if err != nil {
		r.eventRecorder.Eventf(sc.pcs, corev1.EventTypeWarning, constants.ReasonPodGangCreateOrUpdateFailed, "Error Creating/Updating PodGang %v: %v", pgObjectKey, err)
		return groveerr.WrapError(err,
			errCodeCreateOrPatchPodGang,
			component.OperationSync,
			fmt.Sprintf("Failed to CreateOrPatch PodGang %v", pgObjectKey),
		)
	}

	// Update status with Initialized=False condition and Phase if not already set.
	// This needs to be done separately since CreateOrPatch doesn't handle updates/patches to status subresource.
	if !k8sutils.HasCondition(pg.Status.Conditions, string(groveschedulerv1alpha1.PodGangConditionTypeInitialized)) {
		if err = r.patchPodGangInitializedStatus(ctx, sc, pg.Name, metav1.ConditionFalse, groveschedulerv1alpha1.ConditionReasonPodGangPodsCreationPending, "Not all constituent pods have been created yet"); err != nil {
			return err
		}
	}

	r.eventRecorder.Eventf(sc.pcs, corev1.EventTypeNormal, constants.ReasonPodGangCreateOrUpdateSuccessful, "Created/Updated PodGang %v", pgObjectKey)
	sc.logger.Info("Triggered CreateOrPatch of PodGang", "objectKey", pgObjectKey)
	return nil
}

// Convenience types and methods on these types that are used during sync flow run.
// ------------------------------------------------------------------------------------------------

// syncContext holds the relevant state required during the sync flow run. The *ByName / *NameSet
// fields are O(1) views over their corresponding slices and are populated eagerly in
// prepareSyncFlow. Callers must access them as fields, not via getters — there is no lazy
// fallback because lazy mutation of syncContext would race the moment the struct is shared
// across goroutines.
type syncContext struct {
	//ctx                  context.Context
	pcs                    *grovecorev1alpha1.PodCliqueSet
	logger                 logr.Logger
	expectedPodGangs       []*podGangInfo
	existingPodGangs       []groveschedulerv1alpha1.PodGang
	existingPodGangByName  map[string]groveschedulerv1alpha1.PodGang
	deletedPodGangNames    []string
	existingPCLQPods       map[string][]corev1.Pod
	existingPCLQs          []grovecorev1alpha1.PodClique
	existingPCLQByName     map[string]grovecorev1alpha1.PodClique
	existingPCSGs          []grovecorev1alpha1.PodCliqueScalingGroup
	existingPCSGByName     map[string]grovecorev1alpha1.PodCliqueScalingGroup
	expectedPodGangByName  map[string]*podGangInfo
	expectedPodGangNameSet componentutils.Set[string]
	unassignedPodsByPCLQ   map[string][]corev1.Pod
	tasEnabled             bool
	topologyLevels         []grovecorev1alpha1.TopologyLevel
}

// getPodGangNamesPendingCreation identifies PodGangs not yet created.
func (sc *syncContext) getPodGangNamesPendingCreation() []string {
	return lo.FilterMap(sc.expectedPodGangs, func(podGang *podGangInfo, _ int) (string, bool) {
		return podGang.fqn, !sc.isExistingPodGang(podGang.fqn)
	})
}

func (sc *syncContext) isExistingPodGang(podGangName string) bool {
	_, ok := sc.existingPodGangByName[podGangName]
	return ok
}

func (sc *syncContext) getExcessPodGangNames() []string {
	var excessPodGangNames []string
	for _, existingPodGang := range sc.existingPodGangs {
		if !sc.expectedPodGangNameSet.Has(existingPodGang.Name) {
			excessPodGangNames = append(excessPodGangNames, existingPodGang.Name)
		}
	}
	return excessPodGangNames
}

func (sc *syncContext) isPodGangInitialized(podGangName string) bool {
	foundPG, ok := sc.existingPodGangByName[podGangName]
	return ok && k8sutils.IsConditionTrue(foundPG.Status.Conditions, string(groveschedulerv1alpha1.PodGangConditionTypeInitialized))
}

// initializeAssignedAndUnassignedPodsForPCS categorizes pods by PodGang assignment.
// The lookup yields a *podGangInfo that aliases an entry in sc.expectedPodGangs (which stores
// pointers). Mutations via refreshAssociatedPCLQPods therefore propagate back to the slice;
// changing expectedPodGangs to a value-typed slice would silently break this aliasing.
func (sc *syncContext) initializeAssignedAndUnassignedPodsForPCS() {
	for pclqName, pods := range sc.existingPCLQPods {
		for _, pod := range pods {
			if metav1.HasLabel(pod.ObjectMeta, apicommon.LabelPodGang) {
				podGangName := pod.GetLabels()[apicommon.LabelPodGang]
				pgi, ok := sc.expectedPodGangByName[podGangName]
				if !ok {
					continue
				}
				pgi.refreshAssociatedPCLQPods(pclqName, pod.Name)
			} else {
				sc.unassignedPodsByPCLQ[pclqName] = append(sc.unassignedPodsByPCLQ[pclqName], pod)
			}
		}
	}
}

// getPodCliques retrieves PodClique resources for a PodGang.
func (sc *syncContext) getPodCliques(podGang *podGangInfo) []grovecorev1alpha1.PodClique {
	constituentPCLQs := make([]grovecorev1alpha1.PodClique, 0, len(podGang.pclqs))
	for _, podGangConstituentPCLQInfo := range podGang.pclqs {
		if pclq, ok := sc.existingPCLQByName[podGangConstituentPCLQInfo.fqn]; ok {
			constituentPCLQs = append(constituentPCLQs, pclq)
		}
	}
	return constituentPCLQs
}

// podGangInfoByName builds a name-keyed map for O(1) podGangInfo lookups. Kept local because
// podGangInfo is package-private; the public PodCliqueByName/PCSGByName/PodGangByName helpers
// in componentutils cover the cross-package equivalents.
func podGangInfoByName(podGangs []*podGangInfo) map[string]*podGangInfo {
	return componentutils.MapBy(podGangs, func(podGang *podGangInfo) (string, *podGangInfo) {
		return podGang.fqn, podGang
	})
}

// podGangInfoNameSet builds a Set of podGangInfo FQNs. Kept local for the same reason as
// podGangInfoByName.
func podGangInfoNameSet(podGangs []*podGangInfo) componentutils.Set[string] {
	return componentutils.NewSetBy(podGangs, func(podGang *podGangInfo) string {
		return podGang.fqn
	})
}

// syncFlowResult captures the result of a sync flow run.
type syncFlowResult struct {
	// createdPodGangNames are the names of the PodGangs that got created during the sync flow run.
	createdPodGangNames []string
	// errs are the list of errors during the sync flow run.
	errs []error
}

// hasErrors returns true if any errors occurred during sync.
func (sfr *syncFlowResult) hasErrors() bool {
	return len(sfr.errs) > 0
}

// recordError adds an error to the sync flow result.
func (sfr *syncFlowResult) recordError(err error) {
	sfr.errs = append(sfr.errs, err)
}

// recordPodGangCreation adds a PodGang to the created list.
func (sfr *syncFlowResult) recordPodGangCreation(podGangName string) {
	sfr.createdPodGangNames = append(sfr.createdPodGangNames, podGangName)
}

// getAggregatedError combines all errors into a single error.
func (sfr *syncFlowResult) getAggregatedError() error {
	return errors.Join(sfr.errs...)
}

// podGangInfo is a convenience type that holds the information about
// its constituent PodClique names and expected replicas per PodClique for this PodGang.
// Each PodClique constituent is directly mapped to a groveschedulerv1alpha1.PodGroup.
// This struct will be used to check if all pods required by this PodGang are created and determine if this PodGang can be created.
type podGangInfo struct {
	// fqn is a fully qualified name of a PodGang.
	fqn string
	// pclqs holds the relevant information for all constituent PodCliques for this PodGang.
	pclqs []pclqInfo
	// topologyConstraint holds the topology pack constraint applicable at the PodGang level.
	// These will be cleared when TAS is disabled.
	topologyConstraint *groveschedulerv1alpha1.TopologyConstraint
	// pcsgPackConstraints holds the topology pack constraints applicable at the PodCliqueScalingGroup level.
	// These will be cleared when TAS is disabled.
	pcsgTopologyConstraints []groveschedulerv1alpha1.TopologyConstraintGroupConfig
}

// refreshAssociatedPCLQPods adds pod names to a PodClique's associated pod list.
func (pgi *podGangInfo) refreshAssociatedPCLQPods(pclqName string, newlyAssociatedPods ...string) {
	for i := range pgi.pclqs {
		if pgi.pclqs[i].fqn == pclqName {
			pgi.pclqs[i].associatedPodNames = append(pgi.pclqs[i].associatedPodNames, newlyAssociatedPods...)
		}
	}
}

// pclqInfo represents a groveschedulerv1alpha1.PodGroup and captures information relative to the PodGang of which
// this PodClique is a constituent.
type pclqInfo struct {
	// fqn is a fully qualified name for the PodClique
	fqn string
	// replicas is the number of Pods that are assigned to the PodGang for which this PodClique is a constituent.
	replicas int32
	// minAvailable is the minimum number of pods that are required for gang scheduling from this PodClique
	minAvailable int32
	// associatedPodNames are Pod names (having this PodClique as an owner) that have already been associated to this PodGang.
	// This will be updated as and when pods are either deleted or new pods are associated.
	associatedPodNames []string
	// topologyConstraint holds the topology pack constraint for the PodClique.
	// These will be cleared when TAS is disabled.
	topologyConstraint *groveschedulerv1alpha1.TopologyConstraint
}
