// /*
// Copyright 2026 The Grove Authors.
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
	"sort"
	"strconv"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// errCodeAddPodSchedulingGate is used when addition of a scheduling gate to a pod fails
	errCodeAddPodSchedulingGate         grovecorev1alpha1.ErrorCode = "ERR_ADD_POD_SCHEDULING_GATE"
	errCodeGetPodCliqueTemplate         grovecorev1alpha1.ErrorCode = "ERR_GET_PODCLIQUE_TEMPLATE"
	errCodeGetPCSReplicaPods            grovecorev1alpha1.ErrorCode = "ERR_GET_PCS_REPLICA_PODS"
	errCodeGetPodGang                   grovecorev1alpha1.ErrorCode = "ERR_GET_PODGANG"
	errCodeDeleteTakeDownSetPod         grovecorev1alpha1.ErrorCode = "ERR_DELETE_TAKEDOWN_SET_POD"
	errCodeDeleteTakeDownSetPCSGReplica grovecorev1alpha1.ErrorCode = "ERR_DELETE_TAKEDOWN_SET_PCSG_REPLICA"
)

const (
	mvuRollingUpdateSchedulingGate = "grove.io/gate-old-pending-pods"
)

type coherentSyncContext struct {
	updateWork       *pendingUpdateWork
	pcsReplicaPods   []corev1.Pod
	pcsReplicaPCLQs  []grovecorev1alpha1.PodClique
	pcsReplicaPCSGs  []grovecorev1alpha1.PodCliqueScalingGroup
	expectedPCLQHash map[string]string
}

func (c *coherentSyncContext) prepareCoherentContext(ctx context.Context, logger logr.Logger, cl client.Client, pcs *grovecorev1alpha1.PodCliqueSet, updateWork *pendingUpdateWork) error {
	pcsReplicaPods, err := getPCSReplicaPods(ctx, cl, pcs, updateWork.currentlyUpdatingReplicaInfo.replicaIndex)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeGetPCSReplicaPods,
			component.OperationSync,
			fmt.Sprintf("failed to list pods of PodCliqueSet replica %d", updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
		)
	}
	c.pcsReplicaPods = pcsReplicaPods

	c.pcsReplicaPCLQs, err = componentutils.GetPCLQsMatchingLabels(ctx, cl, pcs.Namespace, lo.Assign(
		map[string]string{
			apicommon.LabelPodCliqueSetReplicaIndex: strconv.Itoa(updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
		},
	))
	if err != nil {
		return groveerr.WrapError(err,
			errCodeListPCLQs,
			component.OperationSync,
			fmt.Sprintf("failed to list all PCLQs in PCS replica %s", pcs.Name),
		)
	}

	// Build a map of PCLQ name -> expected pod template hash for all PodCliques in this replica.
	expectedHashByPCLQ := make(map[string]string, len(c.pcsReplicaPCLQs))
	for _, pclq := range c.pcsReplicaPCLQs {
		expectedHash, err := componentutils.GetExpectedPCLQPodTemplateHash(pcs, pclq.ObjectMeta)
		if err != nil {
			return groveerr.WrapError(err,
				errCodeGetPodCliqueTemplate,
				component.OperationSync,
				fmt.Sprintf("failed to compute expected pod template hash for PodClique %s", pclq.Name),
			)
		}
		expectedHashByPCLQ[pclq.Name] = expectedHash
	}
	c.expectedPCLQHash = expectedHashByPCLQ
	c.pcsReplicaPCSGs = updateWork.currentlyUpdatingReplicaInfo.pcsgs
	c.updateWork = updateWork
	return nil
}

// mvuTemplateEntry represents a single component in the MVU template.
type mvuTemplateEntry struct {
	// fqn is the fully qualified name of the resource (e.g. "my-pcs-0-frontend" for a standalone PCLQ,
	// "my-pcs-0-prefill" for a PCSG).
	fqn string
	// minAvailable is the number of replicas of this component that form one MVU.
	minAvailable int32
}

// mvuTemplate defines the shape of a single MVU based on which components are outdated.
type mvuTemplate struct {
	// standalonePCLQs lists standalone PodClique template specs that are outdated.
	standalonePCLQs []mvuTemplateEntry
	// pcsgConfigs lists PCSG configs that have at least one outdated constituent PCLQ.
	pcsgConfigs []mvuTemplateEntry
}

func (t *mvuTemplate) isEmpty() bool {
	return len(t.standalonePCLQs) == 0 && len(t.pcsgConfigs) == 0
}

// computeMVUTemplate identifies which standalone PCLQs and PCSGs need updating in this replica
// and builds the MVU template from them. A standalone PCLQ is outdated when its
// Status.CurrentPodTemplateHash does not match the expected hash computed from the PCS spec.
// A PCSG is outdated when any of its constituent PCLQs is outdated.
func computeMVUTemplate(logger logr.Logger, cc *coherentSyncContext) mvuTemplate {
	template := mvuTemplate{}

	// Build a lookup from PCSG FQN (ObjectMeta.Name) to its MinAvailable.
	pcsgMinAvailable := make(map[string]int32, len(cc.pcsReplicaPCSGs))
	for _, pcsg := range cc.pcsReplicaPCSGs {
		pcsgMinAvailable[pcsg.Name] = *pcsg.Spec.MinAvailable
	}

	// Track which PCSG FQNs have already been added to the template so each PCSG is included at most once.
	outdatedPCSGFQNs := make(map[string]bool)

	for _, pclq := range cc.pcsReplicaPCLQs {
		expectedHash := cc.expectedPCLQHash[pclq.Name]
		if !isPCLQOutdated(&pclq, expectedHash) {
			continue
		}

		pcsgFQN, isPCSGOwned := pclq.Labels[apicommon.LabelPodCliqueScalingGroup]
		if !isPCSGOwned {
			// Standalone PCLQ is outdated — add it to the template.
			template.standalonePCLQs = append(template.standalonePCLQs, mvuTemplateEntry{
				fqn:          pclq.Name,
				minAvailable: *pclq.Spec.MinAvailable,
			})
			continue
		}

		// PCSG-owned PCLQ is outdated — mark the entire PCSG as outdated.
		if outdatedPCSGFQNs[pcsgFQN] {
			continue
		}
		outdatedPCSGFQNs[pcsgFQN] = true
		template.pcsgConfigs = append(template.pcsgConfigs, mvuTemplateEntry{
			fqn:          pcsgFQN,
			minAvailable: pcsgMinAvailable[pcsgFQN],
		})
	}

	logger.Info("Computed MVU template",
		"standalonePCLQs", lo.Map(template.standalonePCLQs, func(e mvuTemplateEntry, _ int) string { return fmt.Sprintf("%s(%d)", e.fqn, e.minAvailable) }),
		"pcsgConfigs", lo.Map(template.pcsgConfigs, func(e mvuTemplateEntry, _ int) string { return fmt.Sprintf("%s(%d)", e.fqn, e.minAvailable) }),
	)
	return template
}

// takeDownSet captures the pods and PCSG replicas selected for take-down in this iteration.
type takeDownSet struct {
	// standalonePods maps each standalone PCLQ FQN to the pods selected for take-down.
	standalonePods map[string][]*corev1.Pod
	// pcsgReplicaIndices maps each PCSG FQN to the PCSG replica indices selected for take-down.
	pcsgReplicaIndices map[string][]int
}

// computeTakeDownSet selects the old pods and PCSG replicas to take down for one MVU iteration.
// For standalone PCLQs: selects MinAvailable old-hash pods per template, preferring scheduled over pending.
// If the remaining old pods wouldn't fill another full MVU, includes them as tail.
// For PCSGs: selects MinAvailable old PCSG replica indices, preferring replicas with more scheduled pods.
// If the remaining old replicas wouldn't fill another full MVU, includes them as tail.
func computeTakeDownSet(logger logr.Logger, template mvuTemplate, pcsReplicaPods []corev1.Pod, allPCLQsInReplica []grovecorev1alpha1.PodClique, expectedHashByPCLQ map[string]string) takeDownSet {
	tds := takeDownSet{
		standalonePods:     make(map[string][]*corev1.Pod),
		pcsgReplicaIndices: make(map[string][]int),
	}

	// Select standalone PCLQ pods for take-down.
	for _, entry := range template.standalonePCLQs {
		expectedHash := expectedHashByPCLQ[entry.fqn]

		// Collect all old-hash pods belonging to this standalone PCLQ.
		var oldPods []*corev1.Pod
		for i := range pcsReplicaPods {
			pod := &pcsReplicaPods[i]
			if pod.Labels[apicommon.LabelPodClique] != entry.fqn {
				continue
			}
			if pod.Labels[apicommon.LabelPodTemplateHash] != expectedHash {
				oldPods = append(oldPods, pod)
			}
		}

		// Sort: scheduled pods first (prefer taking down scheduled pods ahead of pending).
		sort.Slice(oldPods, func(i, j int) bool {
			return oldPods[i].Spec.NodeName != "" && oldPods[j].Spec.NodeName == ""
		})

		// Select MinAvailable pods. If remaining wouldn't fill another full MVU, take them all (tail).
		selectCount := int(entry.minAvailable)
		if len(oldPods)-selectCount < int(entry.minAvailable) {
			selectCount = len(oldPods)
		}
		tds.standalonePods[entry.fqn] = oldPods[:selectCount]
	}

	// Select PCSG replicas for take-down.
	for _, entry := range template.pcsgConfigs {
		// Group PCSG-owned PCLQs in this replica by their PCSG replica index.
		// A PCSG replica is "old" if any of its constituent PCLQs is outdated.
		pcsgReplicaOld := make(map[int]bool)
		pcsgReplicaScheduledPodCount := make(map[int]int)
		for i := range allPCLQsInReplica {
			pclq := &allPCLQsInReplica[i]
			if pclq.Labels[apicommon.LabelPodCliqueScalingGroup] != entry.fqn {
				continue
			}
			pcsgReplicaIdxStr, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
			if !ok {
				continue
			}
			pcsgReplicaIdx, _ := strconv.Atoi(pcsgReplicaIdxStr)
			if isPCLQOutdated(pclq, expectedHashByPCLQ[pclq.Name]) {
				pcsgReplicaOld[pcsgReplicaIdx] = true
			}
			// Count scheduled pods for this PCSG replica to prioritize take-down ordering.
			for j := range pcsReplicaPods {
				pod := &pcsReplicaPods[j]
				if pod.Labels[apicommon.LabelPodClique] == pclq.Name && pod.Spec.NodeName != "" {
					pcsgReplicaScheduledPodCount[pcsgReplicaIdx]++
				}
			}
		}

		// Collect old replica indices and sort: prefer replicas with more scheduled pods.
		var oldReplicaIndices []int
		for idx, isOld := range pcsgReplicaOld {
			if isOld {
				oldReplicaIndices = append(oldReplicaIndices, idx)
			}
		}
		sort.Slice(oldReplicaIndices, func(i, j int) bool {
			return pcsgReplicaScheduledPodCount[oldReplicaIndices[i]] > pcsgReplicaScheduledPodCount[oldReplicaIndices[j]]
		})

		// Select MinAvailable replicas. If remaining wouldn't fill another full MVU, take them all (tail).
		selectCount := int(entry.minAvailable)
		if len(oldReplicaIndices)-selectCount < int(entry.minAvailable) {
			selectCount = len(oldReplicaIndices)
		}
		tds.pcsgReplicaIndices[entry.fqn] = oldReplicaIndices[:selectCount]
	}

	logger.Info("Computed take-down set",
		"standalonePods", lo.MapValues(tds.standalonePods, func(pods []*corev1.Pod, _ string) []string {
			return lo.Map(pods, func(p *corev1.Pod, _ int) string { return p.Name })
		}),
		"pcsgReplicaIndices", tds.pcsgReplicaIndices,
	)
	return tds
}

// deleteTakeDownSet deletes the pods and PCSG replica PodCliques selected in the take-down set.
// For standalone PCLQs: deletes each selected pod individually.
// For PCSGs: deletes all PodCliques belonging to each selected PCSG replica.
func (r _resource) deleteTakeDownSet(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, tds takeDownSet) error {
	// TODO: @renormalize this could be improved by creating tasks and using concurrent with slow start.
	// Delete standalone PCLQ pods.
	for pclqFQN, pods := range tds.standalonePods {
		for _, pod := range pods {
			logger.Info("Deleting standalone PCLQ pod from take-down set", "pclqFQN", pclqFQN, "pod", pod.Name)
			if err := client.IgnoreNotFound(r.client.Delete(ctx, pod)); err != nil {
				return groveerr.WrapError(err,
					errCodeDeleteTakeDownSetPod,
					component.OperationSync,
					fmt.Sprintf("failed to delete pod %s of standalone PCLQ %s", pod.Name, pclqFQN),
				)
			}
		}
	}

	// Delete PCSG replica PodCliques. Each replica index's constituent PodCliques are deleted
	// through a single DeleteAllOf call using the PCSG FQN and replica index as label selectors.
	for pcsgFQN, replicaIndices := range tds.pcsgReplicaIndices {
		for _, replicaIdx := range replicaIndices {
			logger.Info("Deleting PCSG replica PodCliques from take-down set", "pcsgFQN", pcsgFQN, "pcsgReplicaIndex", replicaIdx)
			if err := r.client.DeleteAllOf(ctx,
				&grovecorev1alpha1.PodClique{},
				client.InNamespace(pcs.Namespace),
				client.MatchingLabels(
					lo.Assign(
						apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name),
						map[string]string{
							apicommon.LabelPodCliqueScalingGroup:             pcsgFQN,
							apicommon.LabelPodCliqueScalingGroupReplicaIndex: strconv.Itoa(replicaIdx),
						},
					),
				),
			); err != nil {
				return groveerr.WrapError(err,
					errCodeDeleteTakeDownSetPCSGReplica,
					component.OperationSync,
					fmt.Sprintf("failed to delete PodCliques for PCSG %s replica %d", pcsgFQN, replicaIdx),
				)
			}
		}
	}

	return nil
}

func (r _resource) orchestrateCoherentUpdate(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsIndicesToTerminate, minAvailableBreachedPCSReplicaIndices []int) error {
	updateWork, err := r.computePendingUpdateWork(ctx, pcs, pcsIndicesToTerminate)
	if err != nil {
		return err
	}

	if len(pcs.Status.UpdateProgress.CurrentlyUpdating) > 0 && updateWork.currentlyUpdatingReplicaInfo != nil {
		cc := &coherentSyncContext{}
		if err := cc.prepareCoherentContext(ctx, logger, r.client, pcs, updateWork); err != nil {
			return err
		}

		if err := r.scheduleGateOldPendingPodsInPCSReplica(ctx, logger, cc, pcs); err != nil {
			return err
		}

		if pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate != nil {
			// Coherent update has already been initiated, we check the availability of the MPG corresponding with the current iteration
			// Check that the latest MPG is available. If it is not, requeue.
			// TODO: @renormalize the name will always be set here? If it is set here then we can get rid of this.
			// If the name is set in the PodGang component, then we will have to check since the patch of the status might fail and this field might be empty
			if pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate.LatestMPGName != nil {
				// If current MPG is not available, the ContinueReconcileAndRequeue will cause the function to return here
				if err := r.isCurrentMPGAvailable(ctx, logger, cc, pcs); err != nil {
					return err
				}
			}
		}

		// Signal the start of coherent update OR finish of last iteration index by bumping the iteration index
		if err := r.bumpCoherentUpdateIterationIndex(ctx, logger, cc, pcs); err != nil {
			return err
		}

		// Compute the MVU template and take-down set for this iteration.
		mvuTmpl := computeMVUTemplate(logger, cc)
		if !mvuTmpl.isEmpty() {
			tds := computeTakeDownSet(logger, mvuTmpl, cc.pcsReplicaPods, cc.pcsReplicaPCLQs, cc.expectedPCLQHash)
			if err := r.deleteTakeDownSet(ctx, logger, pcs, tds); err != nil {
				return err
			}
		}

		// Update the PCS status with the latest update progress
		if err = r.updatePCSWithReplicaUpdateProgress(ctx, logger, pcs, updateWork.currentlyUpdatingReplicaInfo.updateProgress); err != nil {
			return err
		}

		if !updateWork.currentlyUpdatingReplicaInfo.updateProgress.done {
			return groveerr.New(
				groveerr.ErrCodeContinueReconcileAndRequeue,
				component.OperationSync,
				fmt.Sprintf("coherent update of PodCliqueSet replica index %d is not completed", updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
			)
		}
	}
	// pick the next replica index to update.
	nextReplicaToUpdate := updateWork.getNextReplicaToUpdate(pcs, minAvailableBreachedPCSReplicaIndices)
	if err = r.updatePCSWithNextSelectedReplica(ctx, logger, pcs, nextReplicaToUpdate); err != nil {
		return err
	}

	if nextReplicaToUpdate != nil {
		return groveerr.New(
			groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			fmt.Sprintf("commencing update of PodCliqueSet replica index %d", *nextReplicaToUpdate),
		)
	}
	return nil
}

func (r _resource) bumpCoherentUpdateIterationIndex(ctx context.Context, logger logr.Logger, cc *coherentSyncContext, pcs *grovecorev1alpha1.PodCliqueSet) error {
	originalPCS := pcs.DeepCopy()
	iterationIndex := int32(0)
	if pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate != nil {
		iterationIndex = pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate.Counter + 1
	}
	pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate = &grovecorev1alpha1.CoherentUpdateProgress{
		Counter:       iterationIndex,
		LatestMPGName: ptr.To(apicommon.GenerateMPGName(pcs.Name, cc.updateWork.currentlyUpdatingReplicaInfo.replicaIndex, int(ptr.Deref(pcs.Status.CurrentRevision, 0)), int(iterationIndex))),
	}
	logger.Info("Bumping coherent update iteration index", "iterationIndex", iterationIndex)
	return r.patchUpdateProgressStatus(ctx, logger, pcs, originalPCS)
}

func (r _resource) scheduleGateOldPendingPodsInPCSReplica(ctx context.Context, logger logr.Logger, cc *coherentSyncContext, pcs *grovecorev1alpha1.PodCliqueSet) error {
	var oldPendingPods []corev1.Pod
	for _, pod := range cc.pcsReplicaPods {
		pclqName := pod.Labels[apicommon.LabelPodClique]
		expectedHash, ok := cc.expectedPCLQHash[pclqName]
		if !ok {
			continue
		}
		if pod.Labels[apicommon.LabelPodTemplateHash] != expectedHash && pod.Status.Phase == corev1.PodPending {
			oldPendingPods = append(oldPendingPods, pod)
		}
	}

	// schedule gate old pending pods
	for _, pendingPod := range oldPendingPods {
		if lo.Contains(pendingPod.Spec.SchedulingGates, corev1.PodSchedulingGate{Name: mvuRollingUpdateSchedulingGate}) {
			return nil
		}
		result, err := controllerutil.CreateOrPatch(ctx, r.client, &pendingPod, func() error {
			pendingPod.Spec.SchedulingGates = append(pendingPod.Spec.SchedulingGates, corev1.PodSchedulingGate{Name: mvuRollingUpdateSchedulingGate})
			return nil
		})
		if err != nil {
			return groveerr.New(
				errCodeAddPodSchedulingGate,
				component.OperationSync,
				fmt.Sprintf("failed to schedule gate old pending pods of PodCliqueSet replica %d", cc.updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
			)
		}
		logger.Info("Schedule gated an old pending pod as a part of the ongoing coherent update", "pcsReplicaIndex", cc.updateWork.currentlyUpdatingReplicaInfo.replicaIndex, "result", result)
	}
	return nil
}

// getPCSReplicaPods lists all Pods that belong to the passed PodCliqueSet replica.
func getPCSReplicaPods(ctx context.Context, cl client.Client, pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := cl.List(ctx,
		podList,
		client.InNamespace(pcs.Namespace),
		client.MatchingLabels(
			lo.Assign(
				apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name),
				map[string]string{
					apicommon.LabelPodCliqueSetReplicaIndex: strconv.Itoa(replicaIndex),
				},
			),
		)); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// isCurrentMPGAvailable checks if the MPG (Minimal Pod Gang) PodGang is available.
// An MPG is considered available when, for each PodGroup in the PodGang,
// the number of referenced pods in Running phase >= PodGroup.MinReplicas.
func (r _resource) isCurrentMPGAvailable(ctx context.Context, logger logr.Logger, cc *coherentSyncContext, pcs *grovecorev1alpha1.PodCliqueSet) error {
	mpgName := *pcs.Status.UpdateProgress.CurrentlyUpdating[0].CoherentUpdate.LatestMPGName
	mpg, err := componentutils.GetPodGang(ctx, r.client, mpgName, pcs.Namespace)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeGetPodGang,
			component.OperationSync,
			fmt.Sprintf("failed to get MPG PodGang %s", mpgName),
		)
	}

	for _, podGroup := range mpg.Spec.PodGroups {
		runningCount := int32(0)
		for _, podRef := range podGroup.PodReferences {
			pod, ok := lo.Find(cc.pcsReplicaPods, func(pod corev1.Pod) bool {
				return podRef.Name == pod.Name && podRef.Namespace == pod.Namespace
			})
			// If referred pod is not found, then the pod references are stale. Wait for PodGang sync to happen to refresh the pod references
			if !ok {
				continue
			}
			if pod.Status.Phase == corev1.PodRunning {
				runningCount++
			}
		}
		if runningCount < podGroup.MinReplicas {
			logger.Info("MPG PodGroup has insufficient running pods",
				"mpgName", mpgName,
				"podGroupName", podGroup.Name,
				"runningCount", runningCount,
				"minReplicas", podGroup.MinReplicas,
			)
			return groveerr.New(
				groveerr.ErrCodeContinueReconcileAndRequeue,
				component.OperationSync,
				fmt.Sprintf("latest MPG %s is not yet available for PodCliqueSet replica %d", mpgName, cc.updateWork.currentlyUpdatingReplicaInfo.replicaIndex),
			)
		}
	}
	logger.Info("Latest MPG is available, continuing coherent update orchestration", "mpgName", mpgName)
	return nil
}

// isPCLQOutdated checks if a PCLQ's status hash does not match the expected hash.
func isPCLQOutdated(pclq *grovecorev1alpha1.PodClique, expectedHash string) bool {
	return pclq.Status.CurrentPodTemplateHash == nil || *pclq.Status.CurrentPodTemplateHash != expectedHash
}
