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

package pod

import (
	"fmt"
	"slices"
	"sort"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	"github.com/ai-dynamo/grove/operator/internal/index"
	"github.com/ai-dynamo/grove/operator/internal/utils"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// processCoherentUpdate handles pod reassignment during a coherent update for standalone PCLQs.
// It reads InFlightPodGangs, determines how many pods this PCLQ needs to contribute to each
// in-flight PodGang, selects pods from decremented old PodGangs for deletion, and creates
// replacement pods assigned to the new PodGangs.
func (r _resource) processCoherentUpdate(logger logr.Logger, sc *syncContext) error {
	inFlightPodGangs := sc.pcs.Status.UpdateProgress.CurrentlyUpdating[0].InFlightPodGangs
	if len(inFlightPodGangs) == 0 {
		return nil
	}

	pgm, err := componentutils.GetPodGangMapForPCSReplica(sc.ctx, r.client, sc.pcs.Name, sc.pclq.Namespace, sc.pcsReplicaIndex)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeGetPodGang,
			component.OperationSync,
			fmt.Sprintf("failed to get PodGangMap for PCS replica %d during coherent update", sc.pcsReplicaIndex),
		)
	}

	podGangDeficits := computeInFlightDeficits(pgm.Spec.Entries, inFlightPodGangs, sc.cliqueName, sc.existingPCLQPods)
	totalDeficit := sumDeficits(podGangDeficits)
	if totalDeficit <= 0 {
		return nil
	}

	candidates := getPodsFromDecrementedPodGangs(pgm.Spec.Entries, sc.cliqueName, sc.existingPCLQPods, inFlightPodGangs)
	podsToDelete := selectPodsForDeletion(candidates, totalDeficit, sc.getExpectedPodTemplateHash())

	if err = r.deletePodsForCoherentUpdate(logger, sc, podsToDelete); err != nil {
		return err
	}
	return r.createPodsForInFlightPodGangs(logger, sc, podGangDeficits)
}

// computeInFlightDeficits determines how many pods each in-flight PodGang still needs
// from this PCLQ, accounting for pods that already exist in those PodGangs.
func computeInFlightDeficits(entries []grovecorev1alpha1.PodGangEntry, inFlightPodGangs []string, cliqueName string, existingPods []*corev1.Pod) map[string]int32 {
	existingPerPodGang := countPodsPerPodGang(existingPods)
	deficits := make(map[string]int32)

	for _, pgName := range inFlightPodGangs {
		for _, entry := range entries {
			if entry.Name != pgName {
				continue
			}
			desired, ok := entry.PodCliques[cliqueName]
			if !ok {
				break
			}
			deficit := desired - existingPerPodGang[pgName]
			if deficit > 0 {
				deficits[pgName] = deficit
			}
			break
		}
	}
	return deficits
}

// getPodsFromDecrementedPodGangs identifies old PodGangs where the actual pod count exceeds
// the PodGangMap's desired count (meaning replicas were taken from them) and returns all pods
// from those PodGangs as deletion candidates.
func getPodsFromDecrementedPodGangs(entries []grovecorev1alpha1.PodGangEntry, cliqueName string, existingPods []*corev1.Pod, inFlightPodGangs []string) []*corev1.Pod {
	inFlightSet := sets.New[string](inFlightPodGangs...)
	podsByPodGang := groupPodsByPodGang(existingPods)

	var candidates []*corev1.Pod
	for _, entry := range entries {
		if inFlightSet.Has(entry.Name) {
			continue
		}
		desired, ok := entry.PodCliques[cliqueName]
		if !ok {
			continue
		}
		podsInPodGang := podsByPodGang[entry.Name]
		if int32(len(podsInPodGang)) > desired {
			candidates = append(candidates, podsInPodGang...)
		}
	}
	return candidates
}

// groupPodsByPodGang groups pods by their PodGang label.
func groupPodsByPodGang(pods []*corev1.Pod) map[string][]*corev1.Pod {
	grouped := make(map[string][]*corev1.Pod)
	for _, pod := range pods {
		if pgName, ok := pod.Labels[apicommon.LabelPodGang]; ok {
			grouped[pgName] = append(grouped[pgName], pod)
		}
	}
	return grouped
}

// selectPodsForDeletion sorts candidates using DeletionSorter and returns the top `count` pods.
func selectPodsForDeletion(candidates []*corev1.Pod, count int, expectedPodTemplateHash string) []*corev1.Pod {
	if len(candidates) == 0 || count <= 0 {
		return nil
	}
	sorter := DeletionSorter{
		Pods:                    slices.Clone(candidates),
		ExpectedPodTemplateHash: expectedPodTemplateHash,
	}
	sort.Sort(sorter)
	return sorter.Pods[:min(count, len(sorter.Pods))]
}

// deletePodsForCoherentUpdate deletes the given pods as part of coherent update reassignment.
func (r _resource) deletePodsForCoherentUpdate(logger logr.Logger, sc *syncContext, pods []*corev1.Pod) error {
	if len(pods) == 0 {
		return nil
	}
	deletionTasks := r.createPodDeletionTasks(logger, sc.pclq, pods, sc.pclqExpectationsStoreKey)
	runResult := utils.RunConcurrentlyWithSlowStart(sc.ctx, logger, 1, deletionTasks)
	if runResult.HasErrors() {
		return runResult.GetAggregatedError()
	}
	return nil
}

// createPodsForInFlightPodGangs creates replacement pods assigned to the in-flight PodGangs.
func (r _resource) createPodsForInFlightPodGangs(logger logr.Logger, sc *syncContext, podGangDeficits map[string]int32) error {
	totalPods := sumDeficits(podGangDeficits)
	if totalPods <= 0 {
		return nil
	}
	availableIndices, err := index.GetAvailableIndices(logger, sc.existingPCLQPods, totalPods)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeGetAvailablePodHostNameIndices,
			component.OperationSync,
			fmt.Sprintf("error getting available indices for Pods in PodClique %v", client.ObjectKeyFromObject(sc.pclq)),
		)
	}

	var taskIdx int
	createTasks := make([]utils.Task, 0, totalPods)
	for pgName, deficit := range podGangDeficits {
		for range deficit {
			createTasks = append(createTasks, r.createPodCreationTask(logger, sc.pcs, sc.pclq, pgName, sc.pclqExpectationsStoreKey, taskIdx, availableIndices[taskIdx]))
			taskIdx++
		}
	}

	runResult := utils.RunConcurrentlyWithSlowStart(sc.ctx, logger, 1, createTasks)
	if runResult.HasErrors() {
		return runResult.GetAggregatedError()
	}
	return nil
}

// sumDeficits returns the total number of pods needed across all PodGang deficits.
func sumDeficits(deficits map[string]int32) int {
	var total int
	for _, d := range deficits {
		total += int(d)
	}
	return total
}
