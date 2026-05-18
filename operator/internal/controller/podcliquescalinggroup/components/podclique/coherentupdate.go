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

package podclique

import (
	"context"
	"fmt"
	"strconv"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/constants"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	"github.com/ai-dynamo/grove/operator/internal/utils"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// processCoherentUpdate handles PCSG replica reassignment during a coherent update.
// It reads InFlightPodGangs, determines how many replicas this PCSG needs to contribute
// to each in-flight PodGang, selects replicas from decremented old PodGangs for deletion,
// and creates replacement PCLQs assigned to the new PodGangs.
func (r _resource) processCoherentUpdate(logger logr.Logger, sc *syncContext) error {
	// Early exit: if the orchestrator has not yet selected a PCS replica to update
	// or has not populated InFlightPodGangs for the current iteration, there is
	// nothing for the PCSG to act on.
	if len(sc.pcs.Status.UpdateProgress.CurrentlyUpdating) == 0 ||
		len(sc.pcs.Status.UpdateProgress.CurrentlyUpdating[0].InFlightPodGangs) == 0 {
		return nil
	}

	inFlightPodGangs := sc.pcs.Status.UpdateProgress.CurrentlyUpdating[0].InFlightPodGangs
	pgm, err := componentutils.GetPodGangMapForPCSReplica(sc.ctx, r.client, sc.pcs.Name, sc.pcsg.Namespace, sc.pcsReplicaIndex)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeGetPodGangMap,
			component.OperationSync,
			fmt.Sprintf("failed to get PodGangMap for PCS replica %d during coherent update", sc.pcsReplicaIndex),
		)
	}

	pcsgName := apicommon.ExtractScalingGroupNameFromPCSGFQN(sc.pcsg.Name, apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: sc.pcsReplicaIndex})

	deficits, err := computeInFlightPCSGDeficits(pgm.Spec.Entries, inFlightPodGangs, pcsgName, sc.existingPCLQs)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeParsePodCliqueScalingGroupReplicaIndex,
			component.OperationSync,
			fmt.Sprintf("failed to compute in-flight PCSG deficits for %v during coherent update", client.ObjectKeyFromObject(sc.pcsg)),
		)
	}
	totalDeficit := sumPCSGDeficits(deficits)
	if totalDeficit <= 0 {
		return nil
	}

	replicaIndicesToDelete, err := getReplicaIndicesFromDecrementedPodGangs(pgm.Spec.Entries, pcsgName, sc.existingPCLQs, inFlightPodGangs)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeParsePodCliqueScalingGroupReplicaIndex,
			component.OperationSync,
			fmt.Sprintf("failed to identify decremented PodGang replicas for %v during coherent update", client.ObjectKeyFromObject(sc.pcsg)),
		)
	}
	indicesToDelete := replicaIndicesToDelete[:min(totalDeficit, len(replicaIndicesToDelete))]

	if err := r.deleteReplicasForCoherentUpdate(logger, sc, indicesToDelete); err != nil {
		return err
	}

	return r.createReplicasForInFlightPodGangs(logger, sc, deficits, indicesToDelete)
}

// computeInFlightPCSGDeficits determines how many PCSG replicas each in-flight PodGang
// still needs, accounting for replicas that already exist in those PodGangs.
func computeInFlightPCSGDeficits(entries []grovecorev1alpha1.PodGangEntry, inFlightPodGangs []string, pcsgName string, existingPCLQs []grovecorev1alpha1.PodClique) (map[string]int32, error) {
	existingPerPodGang, err := countExistingPCSGReplicasPerPodGang(existingPCLQs)
	if err != nil {
		return nil, err
	}
	deficits := make(map[string]int32)

	for _, pgName := range inFlightPodGangs {
		for _, entry := range entries {
			if entry.Name != pgName {
				continue
			}
			desired, ok := entry.PodCliqueScalingGroups[pcsgName]
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
	return deficits, nil
}

// getReplicaIndicesFromDecrementedPodGangs identifies PCSG replica indices from old PodGangs
// where the actual replica count exceeds the PodGangMap's desired count.
func getReplicaIndicesFromDecrementedPodGangs(entries []grovecorev1alpha1.PodGangEntry, pcsgName string, existingPCLQs []grovecorev1alpha1.PodClique, inFlightPodGangs []string) ([]int, error) {
	inFlightSet := sets.New[string](inFlightPodGangs...)
	existingPCSGReplicasByPodGang, err := groupPCSGReplicaIndicesByPodGang(existingPCLQs)
	if err != nil {
		return nil, err
	}

	var indices []int
	for _, entry := range entries {
		if inFlightSet.Has(entry.Name) {
			continue
		}
		desired, ok := entry.PodCliqueScalingGroups[pcsgName]
		if !ok {
			continue
		}
		replicaIndices := existingPCSGReplicasByPodGang[entry.Name]
		if int32(len(replicaIndices)) > desired {
			indices = append(indices, replicaIndices...)
		}
	}
	return indices, nil
}

// groupPCSGReplicaIndicesByPodGang groups unique PCSG replica indices by their PodGang label.
// Returns an error if any PCLQ has a non-numeric PCSG replica index label — this label is
// controller-managed and an unparseable value indicates a corrupt resource.
func groupPCSGReplicaIndicesByPodGang(pclqs []grovecorev1alpha1.PodClique) (map[string][]int, error) {
	seen := make(map[string]sets.Set[int])
	for _, pclq := range pclqs {
		pgName, ok := pclq.Labels[apicommon.LabelPodGang]
		if !ok {
			continue
		}
		replicaIdxStr, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
		if !ok {
			continue
		}
		replicaIdx, err := strconv.Atoi(replicaIdxStr)
		if err != nil {
			return nil, fmt.Errorf("%s label on PodClique %s is not a valid integer: %q", apicommon.LabelPodCliqueScalingGroupReplicaIndex, pclq.Name, replicaIdxStr)
		}
		if seen[pgName] == nil {
			seen[pgName] = sets.New[int]()
		}
		seen[pgName].Insert(replicaIdx)
	}
	grouped := make(map[string][]int, len(seen))
	for pgName, uniqueIndices := range seen {
		grouped[pgName] = uniqueIndices.UnsortedList()
	}
	return grouped, nil
}

// countExistingPCSGReplicasPerPodGang counts unique PCSG replica indices per PodGang.
func countExistingPCSGReplicasPerPodGang(pclqs []grovecorev1alpha1.PodClique) (map[string]int32, error) {
	grouped, err := groupPCSGReplicaIndicesByPodGang(pclqs)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int32, len(grouped))
	for pgName, replicaIndices := range grouped {
		counts[pgName] = int32(len(replicaIndices))
	}
	return counts, nil
}

// deleteReplicasForCoherentUpdate deletes PCLQs for the given PCSG replica indices.
func (r _resource) deleteReplicasForCoherentUpdate(logger logr.Logger, sc *syncContext, replicaIndices []int) error {
	if len(replicaIndices) == 0 {
		return nil
	}
	deletionTasks := r.createDeleteTasks(logger, sc.pcs, sc.pcsg.Name, replicaIndices, "delete PCSG replicas for coherent update")
	return r.triggerDeletionOfPodCliques(sc.ctx, logger, client.ObjectKeyFromObject(sc.pcsg), deletionTasks)
}

// createReplicasForInFlightPodGangs creates new PCLQs for PCSG replicas assigned to in-flight PodGangs.
// It reuses the freed replica indices from deleted replicas to maintain index continuity within the PCSG.
func (r _resource) createReplicasForInFlightPodGangs(logger logr.Logger, sc *syncContext, podGangDeficits map[string]int32, freedReplicaIndices []int) error {
	replicaAssignments := buildReplicaAssignments(podGangDeficits, freedReplicaIndices)
	if len(replicaAssignments) == 0 {
		return nil
	}

	tasks := r.buildPCLQCreationTasks(logger, sc, replicaAssignments)
	if runResult := utils.RunConcurrently(sc.ctx, logger, tasks); runResult.HasErrors() {
		return groveerr.WrapError(runResult.GetAggregatedError(),
			errCodeCreatePodCliques,
			component.OperationSync,
			fmt.Sprintf("Error creating PCLQs for coherent update in PodCliqueScalingGroup: %v", client.ObjectKeyFromObject(sc.pcsg)),
		)
	}
	return nil
}

// buildReplicaAssignments pairs each in-flight PodGang's deficit with a freed replica index.
// Returns a map of PCSG replica index to PodGang name.
// If there are fewer freed indices than the total deficit, only the available indices are assigned —
// the remaining deficit will be resolved in a subsequent reconcile after more replicas are deleted.
func buildReplicaAssignments(podGangDeficits map[string]int32, freedReplicaIndices []int) map[int]string {
	assignments := make(map[int]string)
	var freedIdx int
	for pgName, deficit := range podGangDeficits {
		for range deficit {
			if freedIdx >= len(freedReplicaIndices) {
				return assignments
			}
			assignments[freedReplicaIndices[freedIdx]] = pgName
			freedIdx++
		}
	}
	return assignments
}

// buildPCLQCreationTasks creates tasks for creating all PCLQs needed for the given replica assignments.
func (r _resource) buildPCLQCreationTasks(logger logr.Logger, sc *syncContext, assignments map[int]string) []utils.Task {
	var tasks []utils.Task
	for pcsgReplicaIndex, podGangName := range assignments {
		for _, cliqueName := range sc.pcsgConfig.CliqueNames {
			pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: sc.pcsg.Name, Replica: pcsgReplicaIndex}, cliqueName)
			pclqObjectKey := client.ObjectKey{Name: pclqFQN, Namespace: sc.pcsg.Namespace}
			pgName := podGangName
			replicaIdx := pcsgReplicaIndex
			tasks = append(tasks, utils.Task{
				Name: fmt.Sprintf("CreatePCLQ-%s", pclqFQN),
				Fn: func(ctx context.Context) error {
					return r.doCreateWithPodGangName(ctx, logger, sc.pcs, sc.pcsg, replicaIdx, pclqObjectKey, pgName)
				},
			})
		}
	}
	return tasks
}

// doCreateWithPodGangName creates a PCLQ with a specific PodGang name for coherent update.
// It builds the resource using the standard buildResource and then overrides the PodGang label.
func (r _resource) doCreateWithPodGangName(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex int, pclqObjectKey client.ObjectKey, podGangName string) error {
	pclq := emptyPodClique(pclqObjectKey)
	if err := r.buildResource(logger, pcs, pcsg, pcsgReplicaIndex, pclq, false); err != nil {
		return err
	}
	pclq.Labels[apicommon.LabelPodGang] = podGangName

	if err := r.client.Create(ctx, pclq); err != nil {
		r.eventRecorder.Eventf(pcsg, corev1.EventTypeWarning, constants.ReasonPodCliqueCreateOrUpdateFailed, "PodClique %v creation failed: %v", pclqObjectKey, err)
		return groveerr.WrapError(err,
			errCodeCreatePodCliques,
			component.OperationSync,
			fmt.Sprintf("Error creating PodClique %v for coherent update: %v", pclqObjectKey, client.ObjectKeyFromObject(pcsg)),
		)
	}
	r.eventRecorder.Eventf(pcsg, corev1.EventTypeNormal, constants.ReasonPodCliqueCreateOrUpdateSuccessful, "PodClique %v created for coherent update", pclqObjectKey)
	return nil
}

// sumPCSGDeficits returns the total number of replicas needed across all PodGang deficits.
func sumPCSGDeficits(deficits map[string]int32) int {
	var total int
	for _, d := range deficits {
		total += int(d)
	}
	return total
}
