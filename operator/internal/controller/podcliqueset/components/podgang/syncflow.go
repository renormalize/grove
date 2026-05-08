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
	"slices"
	"sort"
	"strconv"
	"strings"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	apiconstants "github.com/ai-dynamo/grove/operator/api/common/constants"
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

	sc.existingPCSGs, err = r.getExistingPCSGsForPCS(ctx, pcs)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodCliqueScalingGroups,
			component.OperationSync,
			fmt.Sprintf("failed to list PodCliqueScalingGroups for PodCliqueSet %v", pcsObjectKey),
		)
	}

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

	// if err = r.computeExpectedPodGangs(sc); err != nil {
	// 	return nil, groveerr.WrapError(err,
	// 		errCodeComputeExistingPodGangs,
	// 		component.OperationSync,
	// 		fmt.Sprintf("failed to compute existing PodGangs for PodCliqueSet %v", pcsObjectKey),
	// 	)
	// }

	sc.existingPodGangs, err = componentutils.GetExistingPodGangs(ctx, r.client, pcs.ObjectMeta, pcs.Namespace)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodGangs,
			component.OperationSync,
			fmt.Sprintf("Failed to get existing PodGangs for PodCliqueSet: %v", client.ObjectKeyFromObject(sc.pcs)),
		)
	}

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

// computeExpectedPodGangs computes expected PodGangs based on PCS replicas and scaling groups.
// func (r _resource) computeExpectedPodGangs(sc *syncContext) error {
// 	var expectedPodGangs []*podGangInfo

// 	// For each PodCliqueSet replica, a base PodGang is expected to be created.
// 	// A base PodGang constitutes the minimum viable set of PodCliques that must be scheduled together.
// 	basePodGangs, err := buildExpectedBasePodGangForPCSReplicas(sc)
// 	if err != nil {
// 		return err
// 	}
// 	expectedPodGangs = append(expectedPodGangs, basePodGangs...)

// 	// For each replica of PodCliqueSet, get the PodGangs associated to PodCliqueScalingGroup replicas above MinAvailable.
// 	// These are also commonly called "scaled PodGangs" which refer to replica indexes for PCSG above MinAvailable.
// 	// Each scaled replica of a PCSG is gang scheduled as is represented by its own PodGang resource.
// 	if len(sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs) > 0 {
// 		for pcsReplica := range sc.pcs.Spec.Replicas {
// 			expectedPodGangsForPCSG, err := r.buildExpectedScaledPodGangsForPCSG(sc, int(pcsReplica))
// 			if err != nil {
// 				return err
// 			}
// 			expectedPodGangs = append(expectedPodGangs, expectedPodGangsForPCSG...)
// 		}
// 	}
// 	sc.expectedPodGangs = expectedPodGangs
// 	return nil
// }

// buildExpectedBasePodGangForPCSReplicas builds the BASE PodGangs for each PodCliqueSet replica.
// These are the foundational PodGangs that contain:
// 1. Standalone PodCliques (not part of any scaling group)
// 2. PodCliques that are part of PodCliqueScalingGroup replicas [0, minAvailable-1]
func buildExpectedBasePodGangForPCSReplicas(sc *syncContext) ([]*podGangInfo, error) {
	expectedPodGangs := make([]*podGangInfo, 0, int(sc.pcs.Spec.Replicas))
	for pcsReplica := range int(sc.pcs.Spec.Replicas) {
		basePodGang, err := buildExpectedBasePodGangForPCSReplica(sc, pcsReplica)
		if err != nil {
			return nil, err
		}
		expectedPodGangs = append(expectedPodGangs, basePodGang)
	}
	return expectedPodGangs, nil
}

// buildExpectedBasePodGangForPCSReplica builds the base PodGang info for a given PodCliqueSet replica.
func buildExpectedBasePodGangForPCSReplica(sc *syncContext, pcsReplica int) (*podGangInfo, error) {
	podGangFQN := apicommon.GenerateBasePodGangName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: pcsReplica})
	pg := &podGangInfo{
		fqn: podGangFQN,
		// TopologyConstraint for the base PodGang comes from the topology constraint defined at the PCS level.
		topologyConstraint: createTopologyPackConstraint(sc, client.ObjectKeyFromObject(sc.pcs), sc.pcs.Spec.Template.TopologyConstraint),
	}
	pclqInfos := make([]pclqInfo, 0, len(sc.pcs.Spec.Template.Cliques))

	// Add all standalone PodCliques to the base PodGang PCLQs
	pclqInfos = append(pclqInfos, buildStandalonePCLQInfosForBasePodGang(sc, pcsReplica)...)
	// Compute PCSG PodCliques and TopologyConstraintGroupConfig's that are part of the base PodGang
	pcsgPackConstraints, pcsgPodCliques, err := buildPCSGPackConstraintsAndPCLQsForBasePodGang(sc, pcsReplica)
	if err != nil {
		return nil, fmt.Errorf("failed to build PCSG TopologyConstraintGroupConfigs and PodClique infos for base PodGang %q: %w", podGangFQN, err)
	}
	pclqInfos = append(pclqInfos, pcsgPodCliques...)
	pg.pcsgTopologyConstraints = pcsgPackConstraints
	pg.pclqs = pclqInfos

	return pg, nil
}

func buildStandalonePCLQInfosForBasePodGang(sc *syncContext, pcsReplica int) []pclqInfo {
	pclqInfos := make([]pclqInfo, 0, len(sc.pcs.Spec.Template.Cliques))
	for _, pclqTemplateSpec := range sc.pcs.Spec.Template.Cliques {
		// Check if this PodClique belongs to a scaling group
		pcsgConfig := componentutils.FindScalingGroupConfigForClique(sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs, pclqTemplateSpec.Name)
		if pcsgConfig == nil { // Standalone PodClique
			pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: pcsReplica}, pclqTemplateSpec.Name)
			pclqInfos = append(pclqInfos, buildPodCliqueInfo(sc, pclqTemplateSpec, pclqFQN, false))
		}
	}
	return pclqInfos
}

func buildPCSGPackConstraintsAndPCLQsForBasePodGang(sc *syncContext, pcsReplica int) ([]groveschedulerv1alpha1.TopologyConstraintGroupConfig, []pclqInfo, error) {
	var (
		pclqInfos           []pclqInfo
		pcsgPackConstraints []groveschedulerv1alpha1.TopologyConstraintGroupConfig
	)
	for _, pcsgConfig := range sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		// Iterate through replicas of the PCSG that belong to the base PodGang [0, minAvailable-1]
		minAvailable := int(*pcsgConfig.MinAvailable)
		pcsgPodCliqueInfos, pcsgTopologyConstraints, err := doBuildBasePodGangPCLQsAndPCSGPackConstraints(sc, pcsReplica, pcsgConfig, minAvailable)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to build PCSG TopologyConstraintGroupConfigs and PodClique infos for base PodGang for PCSG %q: %w", pcsgConfig.Name, err)
		}
		pclqInfos = append(pclqInfos, pcsgPodCliqueInfos...)
		pcsgPackConstraints = append(pcsgPackConstraints, pcsgTopologyConstraints...)
	}
	return pcsgPackConstraints, pclqInfos, nil
}

// doBuildBasePodGangPCLQsAndPCSGPackConstraints builds pclqInfos and TopologyConstraintGroupConfigs for a PCSG within a base PodGang.
func doBuildBasePodGangPCLQsAndPCSGPackConstraints(sc *syncContext, pcsReplica int, pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig, minAvailable int) ([]pclqInfo, []groveschedulerv1alpha1.TopologyConstraintGroupConfig, error) {
	var (
		pclqInfos           []pclqInfo
		pcsgPackConstraints []groveschedulerv1alpha1.TopologyConstraintGroupConfig
	)

	pcsgFQN := apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: pcsReplica}, pcsgConfig.Name)
	for replicaIndex := 0; replicaIndex < minAvailable; replicaIndex++ {
		// Iterate through each PCLQ within the PCSG
		pclqFQNs := make([]string, 0, len(pcsgConfig.CliqueNames))
		for _, pclqName := range pcsgConfig.CliqueNames {
			pclqTemplateSpec := componentutils.FindPodCliqueTemplateSpecByName(sc.pcs, pclqName)
			if pclqTemplateSpec == nil {
				return nil, nil, fmt.Errorf("PodCliqueScalingGroup %q references a PodClique %q that does not exist in the PodCliqueSet: %v", pcsgConfig.Name, pclqName, client.ObjectKeyFromObject(sc.pcs))
			}
			pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsgFQN, Replica: replicaIndex}, pclqName)
			pclqInfos = append(pclqInfos, buildPodCliqueInfo(sc, pclqTemplateSpec, pclqFQN, true))
			pclqFQNs = append(pclqFQNs, pclqFQN)
		}
		if sc.tasEnabled && pcsgConfig.TopologyConstraint != nil {
			// For every PCSG a TopologyConstraintGroupConfig is created which has its own TopologyConstraint that is
			// defined for PCLQs within the PCSG. For each PCSG replica there is a separate TopologyConstraintGroupConfig.
			pcsgPackConstraints = append(pcsgPackConstraints, groveschedulerv1alpha1.TopologyConstraintGroupConfig{
				Name:               fmt.Sprintf("%s-%d", pcsgFQN, replicaIndex),
				PodGroupNames:      pclqFQNs,
				TopologyConstraint: createTopologyPackConstraint(sc, types.NamespacedName{Namespace: sc.pcs.Namespace, Name: pcsgFQN}, pcsgConfig.TopologyConstraint),
			})
		}
	}

	return pclqInfos, pcsgPackConstraints, nil
}

func (r _resource) buildExpectedScaledPodGangsForPCSG(sc *syncContext, pcsReplica int) ([]*podGangInfo, error) {
	var expectedPodGangs []*podGangInfo
	for _, pcsgConfig := range sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		pcsgFQN := apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: pcsReplica}, pcsgConfig.Name)
		replicas := sc.determinePCSGReplicas(pcsgFQN, pcsgConfig)
		minAvailable := int(*pcsgConfig.MinAvailable)
		scaledReplicas := replicas - minAvailable
		for podGangIndex, pcsgReplica := 0, minAvailable; podGangIndex < scaledReplicas; podGangIndex, pcsgReplica = podGangIndex+1, pcsgReplica+1 {
			pg, err := doBuildExpectedScaledPodGangForPCSG(sc, pcsgFQN, pcsgConfig, pcsgReplica, podGangIndex)
			if err != nil {
				return nil, fmt.Errorf("failed to build expected scaled PodGang for PCSG %q replica %d: %w", pcsgFQN, pcsgReplica, err)
			}
			expectedPodGangs = append(expectedPodGangs, pg)
		}
	}
	return expectedPodGangs, nil
}

func doBuildExpectedScaledPodGangForPCSG(sc *syncContext, pcsgFQN string, pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig, pcsgReplica int, podGangIndex int) (*podGangInfo, error) {
	var (
		pclqInfos          = make([]pclqInfo, 0, len(pcsgConfig.CliqueNames))
		topologyConstraint *groveschedulerv1alpha1.TopologyConstraint
	)

	// Iterate through each PCLQ within the PCSG
	for _, pclqName := range pcsgConfig.CliqueNames {
		pclqTemplateSpec := componentutils.FindPodCliqueTemplateSpecByName(sc.pcs, pclqName)
		if pclqTemplateSpec == nil {
			return nil, fmt.Errorf("PodCliqueScalingGroup %q references a PodClique %q that does not exist in the PodCliqueSet: %v", pcsgConfig.Name, pclqName, client.ObjectKeyFromObject(sc.pcs))
		}
		pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsgFQN, Replica: pcsgReplica}, pclqName)
		pclqInfos = append(pclqInfos, buildPodCliqueInfo(sc, pclqTemplateSpec, pclqFQN, true))
	}

	// For scaled PodGangs, the TopologyConstraint is determined as follows:
	// 1. If PCSG has a TopologyConstraint defined, use that for the PodGang's TopologyConstraint
	// 2. Else, fall back to PCS-level TopologyConstraint
	// no need to set pcsg topology constraint
	if sc.tasEnabled {
		if pcsgConfig.TopologyConstraint != nil {
			topologyConstraint = createTopologyPackConstraint(sc,
				types.NamespacedName{Namespace: sc.pcs.Namespace, Name: pcsgFQN}, pcsgConfig.TopologyConstraint)
		} else {
			// Fall back to PCS-level constraints
			topologyConstraint = createTopologyPackConstraint(sc, client.ObjectKeyFromObject(sc.pcs),
				sc.pcs.Spec.Template.TopologyConstraint)
		}
	}

	pg := &podGangInfo{
		// fqn:                apicommon.CreatePodGangNameFromPCSGFQN(pcsgFQN, podGangIndex),
		topologyConstraint: topologyConstraint,
		pclqs:              pclqInfos,
	}

	return pg, nil
}

// buildPodCliqueInfo creates pclqInfo with appropriate replica counts.
func buildPodCliqueInfo(sc *syncContext, pclqTemplateSpec *grovecorev1alpha1.PodCliqueTemplateSpec, pclqFQN string, belongsToPCSG bool) pclqInfo {
	replicas := determinePodCliqueReplicas(sc, pclqTemplateSpec, pclqFQN, belongsToPCSG)
	expectedPCLQ := pclqInfo{
		fqn:          pclqFQN,
		replicas:     replicas,
		minAvailable: *pclqTemplateSpec.Spec.MinAvailable,
	}
	expectedPCLQ.topologyConstraint = createTopologyPackConstraint(sc, types.NamespacedName{Namespace: sc.pcs.Namespace, Name: pclqFQN}, pclqTemplateSpec.TopologyConstraint)
	return expectedPCLQ
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

// determinePodCliqueReplicas determines replica count considering HPA mutations.
func determinePodCliqueReplicas(sc *syncContext, pclqTemplateSpec *grovecorev1alpha1.PodCliqueTemplateSpec, pclqFQN string, belongsToPCSG bool) int32 {
	if belongsToPCSG || pclqTemplateSpec.Spec.ScaleConfig == nil {
		return pclqTemplateSpec.Spec.Replicas
	}
	matchingPCLQ, found := lo.Find(sc.existingPCLQs, func(pclq grovecorev1alpha1.PodClique) bool {
		return pclqFQN == pclq.Name
	})
	if !found {
		// PodClique resource not found - might be during initial creation
		// Fall back to template replicas but log warning for visibility
		sc.logger.Info("[WARN]: PodClique resource not found, using template replicas",
			"podCliqueFQN", pclqFQN,
			"templateReplicas", pclqTemplateSpec.Spec.Replicas)
		return pclqTemplateSpec.Spec.Replicas
	}
	return matchingPCLQ.Spec.Replicas
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
	// TODO: @renormalize the excess PodGangs are deleted once the PodGangs do not have any pod references.
	// This can be safely done when all pods in the cluster have been assigned to some or the other PodGang already.
	if err := r.deleteExcessPodGangs(ctx, sc); err != nil {
		result.errs = append(result.errs, err)
		return result
	}
	return r.createOrUpdatePodGangs(ctx, sc)
}

// deleteExcessPodGangs removes PodGangs that are no longer needed.
func (r _resource) deleteExcessPodGangs(ctx context.Context, sc *syncContext) error {
	// TODO: @renormalize excess PGs are deleted based on whether they have no pod references
	// excessPodGangs := sc.getExcessPodGangNames()
	// namespace := sc.pcs.Namespace
	// for _, podGangToDelete := range excessPodGangs {
	// 	pgObjectKey := client.ObjectKey{Namespace: namespace, Name: podGangToDelete}
	// 	pg := emptyPodGang(pgObjectKey)
	// 	sc.logger.Info("Delete excess PodGang", "objectKey", client.ObjectKeyFromObject(pg))
	// 	if err := client.IgnoreNotFound(r.client.Delete(ctx, pg)); err != nil {
	// 		r.eventRecorder.Eventf(sc.pcs, corev1.EventTypeWarning, constants.ReasonPodGangDeleteFailed, "Error deleting PodGang %v: %v", pgObjectKey, err)
	// 		return groveerr.WrapError(err,
	// 			errCodeDeleteExcessPodGang,
	// 			component.OperationSync,
	// 			fmt.Sprintf("failed to delete PodGang %v", pgObjectKey),
	// 		)
	// 	}
	// 	r.eventRecorder.Eventf(sc.pcs, corev1.EventTypeNormal, constants.ReasonPodGangDeleteSuccessful, "Deleted PodGang %v", pgObjectKey)
	// 	sc.deletedPodGangNames = append(sc.deletedPodGangNames, podGangToDelete)
	// 	sc.logger.Info("Triggered delete of excess PodGang", "objectKey", client.ObjectKeyFromObject(pg))
	// }
	return nil
}

// createOrUpdatePodGangs reactively creates PodGangs for unassigned pods.
// 1. List out all existing PodGangs. This is already present in the syncContext: sc.existingPodGangs
// 2. Check if there are pods that need to be assigned a new PodGang: these pods will not have a PodGang label. All pods are fetched already and are present
//    in sc.existingPCLQPods. Unassigned pods are categorized in sc.unassignedPodsByPCLQ.
// 3. Create a PodGang for these pods based on the MVU template logic. While creating, add the selected pod names as pod references.
//    The name of the PodGang will be one index higher than the highest index PodGang that is existing.
// 4. Label the pods with the PodGang label name as the value to LabelPodGang, and remove the scheduling gate at the same time.
func (r _resource) createOrUpdatePodGangs(ctx context.Context, sc *syncContext) syncFlowResult {
	result := syncFlowResult{}

	// Step 2: If there are no unassigned pods, nothing to do.
	if len(sc.unassignedPodsByPCLQ) == 0 {
		return result
	}

	// Wait until all pods of every PodClique have been created before forming PodGangs.
	// This prevents partial assignment where early pods get lumped into a single PodGang
	// because later pods haven't been created yet.
	for _, pclq := range sc.existingPCLQs {
		expectedPods := int(pclq.Spec.Replicas)
		actualPods := len(sc.existingPCLQPods[pclq.Name])
		if actualPods < expectedPods {
			sc.logger.Info("Not all pods created yet, requeuing",
				"pclq", pclq.Name, "expected", expectedPods, "actual", actualPods)
			result.recordError(groveerr.New(groveerr.ErrCodeRequeueAfter,
				component.OperationSync,
				fmt.Sprintf("Waiting for all pods of PodClique %s to be created (%d/%d)", pclq.Name, actualPods, expectedPods),
			))
			return result
		}
	}

	// Group unassigned pods by PCS replica index.
	podsByReplica := groupUnassignedPodsByReplica(sc.unassignedPodsByPCLQ)

	for replicaIndex, pclqPods := range podsByReplica {
		// Step 3: Determine the next PodGang counter for this replica.
		nextCounter := getNextCounterForReplica(sc.existingPodGangs, sc.pcs.Name, replicaIndex)

		// Build PodGangs from unassigned pods. Each PodGang gets MinAvailable pods from
		// each standalone PCLQ and MinAvailable PCSG replicas. The loop repeats until all
		// unassigned pods are consumed (creating a tail PodGang if a full MVU can't be formed).
		for {
			pgInfo, assignedPods := r.buildPodGangFromUnassignedPods(sc, replicaIndex, nextCounter, pclqPods)
			if pgInfo == nil {
				break
			}

			if err := r.createOrUpdatePodGang(ctx, sc, pgInfo); err != nil {
				sc.logger.Error(err, "failed to create PodGang", "PodGangName", pgInfo.fqn)
				result.recordError(err)
				return result
			}
			result.recordPodGangCreation(pgInfo.fqn)

			// Step 4: Label pods and remove the scheduling gate.
			if err := r.assignPodsAndRemoveGates(ctx, sc, assignedPods, pgInfo.fqn); err != nil {
				sc.logger.Error(err, "failed to assign pods to PodGang", "PodGangName", pgInfo.fqn)
				result.recordError(err)
				return result
			}

			if err := r.patchPodGangInitializedStatus(ctx, sc, pgInfo.fqn, metav1.ConditionTrue, groveschedulerv1alpha1.ConditionReasonPodGangPodsCreated, "PodGang is fully initialized"); err != nil {
				sc.logger.Error(err, "failed to update Initialized condition in PodGang status", "PodGangName", pgInfo.fqn)
				result.recordError(err)
			}
			nextCounter++
		}
	}

	return result
}

// groupUnassignedPodsByReplica groups unassigned pods by their PCS replica index.
// Returns a map of replicaIndex -> pclqFQN -> pods.
func groupUnassignedPodsByReplica(unassignedPodsByPCLQ map[string][]corev1.Pod) map[int]map[string][]corev1.Pod {
	result := make(map[int]map[string][]corev1.Pod)
	for pclqFQN, pods := range unassignedPodsByPCLQ {
		for _, pod := range pods {
			replicaStr, ok := pod.GetLabels()[apicommon.LabelPodCliqueSetReplicaIndex]
			if !ok {
				continue
			}
			replicaIndex, err := strconv.Atoi(replicaStr)
			if err != nil {
				continue
			}
			if result[replicaIndex] == nil {
				result[replicaIndex] = make(map[string][]corev1.Pod)
			}
			result[replicaIndex][pclqFQN] = append(result[replicaIndex][pclqFQN], pod)
		}
	}
	return result
}

// getNextCounterForReplica computes the next PodGang counter for a PCS replica
// by examining existing PodGang names matching the prefix <pcsName>-<replica>-.
func getNextCounterForReplica(existingPodGangs []groveschedulerv1alpha1.PodGang, pcsName string, replica int) int {
	prefix := fmt.Sprintf("%s-%d-", pcsName, replica)
	var matchingNames []string
	for _, pg := range existingPodGangs {
		if strings.HasPrefix(pg.Name, prefix) {
			matchingNames = append(matchingNames, pg.Name)
		}
	}
	if len(matchingNames) == 0 {
		return 0
	}
	sort.Strings(matchingNames)
	counter, err := apicommon.ExtractPodGangGlobalCounter(matchingNames)
	if err != nil || counter == nil {
		return 0
	}
	return *counter + 1
}

// countPCSGReplicasInExistingPodGangs counts how many distinct PCSG replicas are already
// represented in existing PodGangs for the given PCS replica. It checks PodGroup names
// matching the PCSG's constituent PCLQ naming pattern.
func countPCSGReplicasInExistingPodGangs(existingPodGangs []groveschedulerv1alpha1.PodGang, pcsNameReplica apicommon.ResourceNameReplica, pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig) int {
	pcsgFQN := apicommon.GeneratePodCliqueScalingGroupName(pcsNameReplica, pcsgConfig.Name)
	totalReplicas := int(*pcsgConfig.Replicas)
	consumed := 0
	for replicaIdx := 0; replicaIdx < totalReplicas; replicaIdx++ {
		prefix := fmt.Sprintf("%s-%d-", pcsgFQN, replicaIdx)
		found := false
		for _, pg := range existingPodGangs {
			for _, podGroup := range pg.Spec.PodGroups {
				if strings.HasPrefix(podGroup.Name, prefix) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			consumed++
		}
	}
	return consumed
}

// buildPodGangFromUnassignedPods builds a podGangInfo by selecting pods from the unassigned pool.
// For standalone PCLQs: MinAvailable pods are taken. If the remainder after taking would be less
// than MinAvailable, the remainder is also absorbed (no standalone-only tail PodGangs).
// For PCSGs: MinAvailable replicas are taken. Each chosen replica contributes ALL pods from its
// constituent PodCliques.
// If PCSGs are configured but no PCSG replicas remain, no PodGang is created (standalone pods
// must have been absorbed by the previous PodGang via the remainder rule).
// Returns nil only when no unassigned pods remain.
// It also removes consumed pods from pclqPods.
func (r _resource) buildPodGangFromUnassignedPods(sc *syncContext, replicaIndex int, counter int, pclqPods map[string][]corev1.Pod) (*podGangInfo, []corev1.Pod) {
	pcsNameReplica := apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: replicaIndex}
	var allPclqInfos []pclqInfo
	var assignedPods []corev1.Pod

	// PCSGs: take up to MinAvailable replicas (the next available ones) from each PCSG.
	// ALL pods of each chosen PCSG replica's constituent PodCliques are included.
	// Replicas are consumed in order — if a replica has no pods yet, stop (don't skip ahead).
	// Already-consumed replicas (empty slice from prior call) are skipped.
	var pcsgPackConstraints []groveschedulerv1alpha1.TopologyConstraintGroupConfig
	pcsgReplicasTaken := 0
	for _, pcsgConfig := range sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		pcsgFQN := apicommon.GeneratePodCliqueScalingGroupName(pcsNameReplica, pcsgConfig.Name)
		totalReplicas := int(*pcsgConfig.Replicas)
		minAvailable := int(*pcsgConfig.MinAvailable)
		replicasTaken := 0
		for replicaIdx := 0; replicaIdx < totalReplicas && replicasTaken < minAvailable; replicaIdx++ {
			var pclqFQNs []string
			replicaHasPods := false
			replicaAlreadyConsumed := true
			for _, pclqName := range pcsgConfig.CliqueNames {
				pclqTemplateSpec := componentutils.FindPodCliqueTemplateSpecByName(sc.pcs, pclqName)
				if pclqTemplateSpec == nil {
					continue
				}
				pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsgFQN, Replica: replicaIdx}, pclqName)
				available, exists := pclqPods[pclqFQN]
				if exists {
					replicaAlreadyConsumed = false
				}
				if len(available) == 0 {
					continue
				}
				selected := available
				delete(pclqPods, pclqFQN)
				podNames := lo.Map(selected, func(p corev1.Pod, _ int) string { return p.Name })
				pi := buildPodCliqueInfo(sc, pclqTemplateSpec, pclqFQN, true)
				pi.associatedPodNames = podNames
				allPclqInfos = append(allPclqInfos, pi)
				assignedPods = append(assignedPods, selected...)
				pclqFQNs = append(pclqFQNs, pclqFQN)
				replicaHasPods = true
			}
			if replicaAlreadyConsumed {
				continue
			}
			if !replicaHasPods {
				break
			}
			replicasTaken++
			pcsgReplicasTaken++
			if sc.tasEnabled && pcsgConfig.TopologyConstraint != nil {
				pcsgPackConstraints = append(pcsgPackConstraints, groveschedulerv1alpha1.TopologyConstraintGroupConfig{
					Name:               fmt.Sprintf("%s-%d", pcsgFQN, replicaIdx),
					PodGroupNames:      pclqFQNs,
					TopologyConstraint: createTopologyPackConstraint(sc, types.NamespacedName{Namespace: sc.pcs.Namespace, Name: pcsgFQN}, pcsgConfig.TopologyConstraint),
				})
			}
		}
	}

	// If PCSGs are configured but no replicas were taken, don't create a PodGang.
	hasPCSGs := len(sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs) > 0
	if hasPCSGs && pcsgReplicasTaken == 0 {
		return nil, nil
	}

	// Determine if this is the last PodGang for this PCS replica by comparing total
	// PCSG replicas consumed (existing PodGangs + this call) against total expected from spec.
	// We cannot just check what remains in pclqPods because pods that haven't been created yet
	// won't appear in the map — that would make lastIteration=true prematurely.
	lastIteration := !hasPCSGs
	if hasPCSGs {
		totalExpectedPCSGReplicas := 0
		totalConsumedPCSGReplicas := pcsgReplicasTaken
		for _, pcsgConfig := range sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs {
			totalExpectedPCSGReplicas += int(*pcsgConfig.Replicas)
			totalConsumedPCSGReplicas += countPCSGReplicasInExistingPodGangs(sc.existingPodGangs, pcsNameReplica, pcsgConfig)
		}
		lastIteration = totalConsumedPCSGReplicas >= totalExpectedPCSGReplicas
	}

	// Standalone PCLQs: take MinAvailable pods. If the remainder after taking is less than
	// MinAvailable, absorb the remainder. If this is the last iteration, take all.
	for _, pclqTemplateSpec := range sc.pcs.Spec.Template.Cliques {
		pcsgConfig := componentutils.FindScalingGroupConfigForClique(sc.pcs.Spec.Template.PodCliqueScalingGroupConfigs, pclqTemplateSpec.Name)
		if pcsgConfig != nil {
			continue
		}
		pclqFQN := apicommon.GeneratePodCliqueName(pcsNameReplica, pclqTemplateSpec.Name)
		available := pclqPods[pclqFQN]
		if len(available) == 0 {
			continue
		}
		var take int
		if lastIteration {
			take = len(available)
		} else {
			minAvail := int(*pclqTemplateSpec.Spec.MinAvailable)
			take = min(minAvail, len(available))
			if len(available)-take < minAvail {
				take = len(available)
			}
		}
		selected := available[:take]
		pclqPods[pclqFQN] = available[take:]
		podNames := lo.Map(selected, func(p corev1.Pod, _ int) string { return p.Name })
		pi := buildPodCliqueInfo(sc, pclqTemplateSpec, pclqFQN, false)
		pi.associatedPodNames = podNames
		allPclqInfos = append(allPclqInfos, pi)
		assignedPods = append(assignedPods, selected...)
	}

	if len(allPclqInfos) == 0 {
		return nil, nil
	}

	podGangName := apicommon.GeneratePodGangName(sc.pcs.Name, replicaIndex, counter)
	pgInfo := &podGangInfo{
		fqn:                     podGangName,
		pclqs:                   allPclqInfos,
		topologyConstraint:      createTopologyPackConstraint(sc, client.ObjectKeyFromObject(sc.pcs), sc.pcs.Spec.Template.TopologyConstraint),
		pcsgTopologyConstraints: pcsgPackConstraints,
	}
	return pgInfo, assignedPods
}

// assignPodsAndRemoveGates patches each pod to set the PodGang label and remove the scheduling gate.
func (r _resource) assignPodsAndRemoveGates(ctx context.Context, sc *syncContext, pods []corev1.Pod, podGangName string) error {
	for i := range pods {
		pod := &pods[i]
		basePod := pod.DeepCopy()

		// Set the PodGang label.
		labels := pod.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[apicommon.LabelPodGang] = podGangName
		pod.SetLabels(labels)

		// Remove the scheduling gate.
		pod.Spec.SchedulingGates = lo.Filter(pod.Spec.SchedulingGates, func(gate corev1.PodSchedulingGate, _ int) bool {
			return gate.Name != apiconstants.PodGangSchedulingGate
		})

		if err := r.client.Patch(ctx, pod, client.MergeFrom(basePod)); err != nil {
			if apierrors.IsNotFound(err) {
				sc.logger.Info("Pod not found during assignment, skipping", "pod", pod.Name)
				continue
			}
			return groveerr.WrapError(err,
				errCodeAssignPodsToPodGang,
				component.OperationSync,
				fmt.Sprintf("failed to assign pod %s to PodGang %s", pod.Name, podGangName),
			)
		}
		sc.logger.Info("Assigned pod to PodGang and removed scheduling gate", "pod", pod.Name, "podGang", podGangName)
	}
	return nil
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
	existingPCLQNames := lo.Map(sc.existingPCLQs, func(pclq grovecorev1alpha1.PodClique, _ int) string {
		return pclq.Name
	})

	return lo.Reduce(podGang.pclqs, func(agg int, pclq pclqInfo, _ int) int {
		if !slices.Contains(existingPCLQNames, pclq.fqn) {
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

// syncContext holds the relevant state required during the sync flow run.
type syncContext struct {
	//ctx                  context.Context
	pcs    *grovecorev1alpha1.PodCliqueSet
	logger logr.Logger
	// expectedPodGangs     []*podGangInfo
	existingPodGangs     []groveschedulerv1alpha1.PodGang
	deletedPodGangNames  []string
	existingPCLQPods     map[string][]corev1.Pod
	existingPCLQs        []grovecorev1alpha1.PodClique
	existingPCSGs        []grovecorev1alpha1.PodCliqueScalingGroup
	unassignedPodsByPCLQ map[string][]corev1.Pod
	tasEnabled           bool
	topologyLevels       []grovecorev1alpha1.TopologyLevel
}

// getPodGangNamesPendingCreation identifies PodGangs not yet created.
// func (sc *syncContext) getPodGangNamesPendingCreation() []string {
// 	return lo.FilterMap(sc.expectedPodGangs, func(podGang *podGangInfo, _ int) (string, bool) {
// 		return podGang.fqn, !sc.isExistingPodGang(podGang.fqn)
// 	})
// }

func (sc *syncContext) isExistingPodGang(podGangName string) bool {
	return slices.ContainsFunc(sc.existingPodGangs, func(pg groveschedulerv1alpha1.PodGang) bool {
		return podGangName == pg.Name
	})
}

// func (sc *syncContext) getExcessPodGangNames() []string {
// 	var excessPodGangNames []string
// 	expectedPodGangNames := lo.Map(sc.expectedPodGangs, func(pg *podGangInfo, _ int) string {
// 		return pg.fqn
// 	})
// 	for _, existingPodGang := range sc.existingPodGangs {
// 		if !slices.Contains(expectedPodGangNames, existingPodGang.Name) {
// 			excessPodGangNames = append(excessPodGangNames, existingPodGang.Name)
// 		}
// 	}
// 	return excessPodGangNames
// }

func (sc *syncContext) isPodGangInitialized(podGangName string) bool {
	foundPG, ok := lo.Find(sc.existingPodGangs, func(pg groveschedulerv1alpha1.PodGang) bool {
		return podGangName == pg.Name
	})
	return ok && k8sutils.IsConditionTrue(foundPG.Status.Conditions, string(groveschedulerv1alpha1.PodGangConditionTypeInitialized))
}

// initializeAssignedAndUnassignedPodsForPCS categorizes pods by PodGang assignment.
func (sc *syncContext) initializeAssignedAndUnassignedPodsForPCS() {
	for pclqName, pods := range sc.existingPCLQPods {
		for _, pod := range pods {
			if metav1.HasLabel(pod.ObjectMeta, apicommon.LabelPodGang) {
				// podGangName := pod.GetLabels()[apicommon.LabelPodGang]
				// // Find the index to work with the original slice element, not a copy
				// pgiIndex := slices.IndexFunc(sc.expectedPodGangs, func(pgi *podGangInfo) bool {
				// 	return podGangName == pgi.fqn
				// })
				// if pgiIndex == -1 {
				// 	continue
				// }
				// // Work with the original element in the slice, not a copy
				// sc.expectedPodGangs[pgiIndex].refreshAssociatedPCLQPods(pclqName, pod.Name)
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
		for _, pclq := range sc.existingPCLQs {
			if pclq.Name == podGangConstituentPCLQInfo.fqn {
				constituentPCLQs = append(constituentPCLQs, pclq)
			}
		}
	}
	return constituentPCLQs
}

// determinePCSGReplicas retrieves the number of replicas for a PCSG for a given PCS and PCS replica index.
// If the PCSG exists then it will return the pcsg.Spec.Replicas value, else it will return the template replicas
// as defined in grovecorev1alpha1.PodCliqueScalingGroupConfig.Replicas
func (sc *syncContext) determinePCSGReplicas(pcsgFQN string, pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig) int {
	foundExistingPCSG, ok := lo.Find(sc.existingPCSGs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup) bool {
		return pcsg.Name == pcsgFQN
	})
	if ok {
		return int(foundExistingPCSG.Spec.Replicas)
	}
	return int(*pcsgConfig.Replicas)
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
