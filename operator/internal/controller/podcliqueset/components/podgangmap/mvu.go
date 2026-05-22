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

package podgangmap

import (
	"maps"
	"slices"

	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	"github.com/ai-dynamo/grove/operator/internal/utils"
)

// mvuTemplate describes the composition of one MVU PodGang.
// It is computed once at update start and remains fixed for the duration of the update.
type mvuTemplate struct {
	// standalonePCLQs maps updated standalone PCLQ name → minAvailable pod count.
	standalonePCLQs map[string]int32
	// pcsgs maps updated PCSG name → minAvailable replica count.
	pcsgs map[string]int32
}

// computeMVUTemplate determines which components have been updated and computes the MVU template.
// To determine which components of a PCS has been updated it compares the new expected pod template
// hash (from the PCS spec) and compares it against the live PCLQ pod template hash.
// For each updated standalone PCLQ, include its minAvailable in the template.
// For each PCSG that has at least one updated constituent PCLQ, include its minAvailable in the template.
func computeMVUTemplate(pcs *grovecorev1alpha1.PodCliqueSet, existingPCLQs []grovecorev1alpha1.PodClique) (*mvuTemplate, error) {
	updatedStandalonePCLQTuples, updatedPCSGPCLQNames, err := findUpdatedPodCliques(pcs, existingPCLQs)
	if err != nil {
		return nil, err
	}

	// Find the PCSGs that have been updated using the updatedPCSGPCLQNames computed earlier.
	updatedPCSGTuples := make(map[string]int32, len(pcs.Spec.Template.PodCliqueScalingGroupConfigs))
	for _, pcsgConfig := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		for _, cliqueName := range pcsgConfig.CliqueNames {
			if slices.Contains(updatedPCSGPCLQNames, cliqueName) {
				updatedPCSGTuples[pcsgConfig.Name] = *pcsgConfig.MinAvailable
				break
			}
		}
	}

	return &mvuTemplate{
		standalonePCLQs: updatedStandalonePCLQTuples,
		pcsgs:           updatedPCSGTuples,
	}, nil
}

func findUpdatedPodCliques(pcs *grovecorev1alpha1.PodCliqueSet, existingPCLQs []grovecorev1alpha1.PodClique) (updatedStandalonePCLQTuples map[string]int32, updatedPCSGPCLQNames []string, err error) {
	updatedStandalonePCLQTuples = make(map[string]int32)

	for _, cliqueTemplate := range pcs.Spec.Template.Cliques {
		// This should never happen. Since the API allows it as it is []*PodCliqueTemplateSpec this check is added.
		if cliqueTemplate == nil {
			continue
		}
		newPCLQHash := componentutils.ComputePCLQPodTemplateHash(cliqueTemplate, pcs.Spec.Template.PriorityClassName)
		// Check any live PCLQ with this clique name — we only need one to determine if the spec changed.
		// All PCLQs with the same clique name share the same template, so checking one suffices.
		for _, pclq := range existingPCLQs {
			var cliqueName string
			cliqueName, err = utils.GetPodCliqueNameFromPodCliqueFQN(pclq.ObjectMeta)
			if err != nil {
				return
			}
			if cliqueName != cliqueTemplate.Name {
				continue
			}
			// found matching PCLQ, check the hash, if it is different then this PCLQ has been updated.
			if pclq.Status.CurrentPodTemplateHash == nil || *pclq.Status.CurrentPodTemplateHash != newPCLQHash {
				// identify if this is a standalone PCLQ or it belongs to a PCSG.
				if componentutils.IsStandalonePCLQ(&pclq) {
					updatedStandalonePCLQTuples[cliqueName] = *pclq.Spec.MinAvailable
				} else {
					updatedPCSGPCLQNames = append(updatedPCSGPCLQNames, cliqueName)
				}
			}
			break
		}
	}
	return
}

// podGangMapState captures the state of the PodGangMap after a computation step:
// the updated old entries (with decremented counts) and newly introduced entries.
type podGangMapState struct {
	// oldEntries are existing old-hash PodGang entries with decremented pod/replica counts.
	// Entries that have reached zero across all counts are removed.
	oldEntries []grovecorev1alpha1.PodGangEntry
	// newEntries are the newly computed MVU PodGang or Tail-PG entries for this iteration.
	newEntries []grovecorev1alpha1.PodGangEntry
	// done is true when there are no remaining old pods/replicas to process.
	done bool
}

// computeNextPodGangMapState determines the next state of the PodGangMap for the current
// coherent update. It checks if a full MVU PodGang can be formed from the pods and
// replicas available in the old-hash entries. If yes, it produces exactly one MVU PodGang entry
// (absorbing leftover standalone PCLQ pods if no further MVU can be formed after it). If a full
// MVU cannot be formed, it produces one Tail-PG entry per remaining PCSG replica.
// It deducts the allocated pods/replicas from old entries (lowest-indexed first) and removes
// old entries that reach zero.
//
// Parameters:
//   - template: the fixed MVU template for this update
//   - oldEntries: existing old-hash PodGang entries ordered by index (lowest first)
//   - mpgNames: names of MVU PodGangs already created in previous iterations of this update
//     (Tail-PGs created in the current iteration depend on them)
//   - entryBuilder: a function that creates a PodGangEntry given the composition
func computeNextPodGangMapState(
	template mvuTemplate,
	oldEntries []grovecorev1alpha1.PodGangEntry,
	mpgNames []string,
	entryBuilder componentutils.PodGangEntryBuilder,
) podGangMapState {
	if canFormMVUPodGang(template, oldEntries) {
		oldEntries, newEntries := computeNextMVUPodGang(template, oldEntries, entryBuilder)
		return podGangMapState{oldEntries: oldEntries, newEntries: newEntries, done: false}
	}
	// Only Tail-PGs remain. Create PodGangEntries for Tail-PGs.
	oldEntries, newEntries, done := computeTailPodGangs(template, oldEntries, mpgNames, entryBuilder)
	return podGangMapState{oldEntries: oldEntries, newEntries: newEntries, done: done}
}

// canFormMVUPodGang checks whether the old entries collectively have enough pods and replicas
// to fill a complete MVU.
func canFormMVUPodGang(template mvuTemplate, oldEntries []grovecorev1alpha1.PodGangEntry) bool {
	for pclqName, minAvailable := range template.standalonePCLQs {
		if sumPCLQPodsInEntries(oldEntries, pclqName) < minAvailable {
			return false
		}
	}
	for pcsgName, minAvailable := range template.pcsgs {
		if sumPCSGReplicasInEntries(oldEntries, pcsgName) < minAvailable {
			return false
		}
	}
	return true
}

// computeNextMVUPodGang computes the next MVU PodGang entry and deducts the allocated
// pods/replicas from old entries (lowest-indexed first). If another MVU cannot be formed
// after this one, all remaining standalone PCLQ pods are absorbed into this MVU.
func computeNextMVUPodGang(
	template mvuTemplate,
	oldEntries []grovecorev1alpha1.PodGangEntry,
	entryBuilder componentutils.PodGangEntryBuilder,
) (updatedOldEntries []grovecorev1alpha1.PodGangEntry, newEntries []grovecorev1alpha1.PodGangEntry) {
	// Clone standalone PCLQ counts from template — may grow if absorption happens.
	nextMVUStandalonePCLQs := maps.Clone(template.standalonePCLQs)

	// Deduct standalone PCLQ pods from old entries (lowest index first).
	for pclqName, needed := range template.standalonePCLQs {
		oldEntries = deductPCLQPodsFromOldEntries(oldEntries, pclqName, needed)
	}

	// Pop PCSG replica indices from old entries (lowest index first). The returned slice is
	// the set of indices absorbed into this MVU PodGang.
	nextMVUPCSGIndices := make(map[string][]int32, len(template.pcsgs))
	for pcsgName, needed := range template.pcsgs {
		var taken []int32
		oldEntries, taken = popPCSGIndicesFromOldEntries(oldEntries, pcsgName, needed)
		nextMVUPCSGIndices[pcsgName] = taken
	}

	// Check if another full MVU can be formed after this deduction.
	if !canFormMVUPodGang(template, oldEntries) {
		// Absorb ALL remaining standalone PCLQ pods into this MVU PodGang.
		// PCSG replicas are NOT absorbed — they remain for Tail-PGs.
		for pclqName := range template.standalonePCLQs {
			remaining := sumPCLQPodsInEntries(oldEntries, pclqName)
			if remaining > 0 {
				nextMVUStandalonePCLQs[pclqName] += remaining
				oldEntries = deductPCLQPodsFromOldEntries(oldEntries, pclqName, remaining)
			}
		}
	}

	// Remove old entries that have no pods and no replicas left.
	updatedOldEntries = removeEmptyEntries(oldEntries)
	newEntries = []grovecorev1alpha1.PodGangEntry{entryBuilder(nextMVUStandalonePCLQs, nextMVUPCSGIndices, nil)}
	return
}

// computeTailPodGangs creates one Tail-PG per remaining PCSG replica in the old entries.
// Pops indices from old entries (lowest-indexed first). Each Tail-PG depends on the
// supplied MVU PodGang names and carries exactly one PCSG replica index.
func computeTailPodGangs(
	template mvuTemplate,
	oldEntries []grovecorev1alpha1.PodGangEntry,
	mpgNames []string,
	entryBuilder componentutils.PodGangEntryBuilder,
) (updatedOldEntries []grovecorev1alpha1.PodGangEntry, newEntries []grovecorev1alpha1.PodGangEntry, done bool) {
	for pcsgName := range template.pcsgs {
		remaining := sumPCSGReplicasInEntries(oldEntries, pcsgName)
		for range remaining {
			var taken []int32
			oldEntries, taken = popPCSGIndicesFromOldEntries(oldEntries, pcsgName, 1)
			newEntries = append(newEntries, entryBuilder(nil, map[string][]int32{pcsgName: taken}, mpgNames))
		}
	}

	updatedOldEntries = removeEmptyEntries(oldEntries)
	if len(newEntries) == 0 {
		return updatedOldEntries, nil, true
	}
	return updatedOldEntries, newEntries, false
}

// sumPCLQPodsInEntries sums the pod count for a given standalone PCLQ across all entries.
func sumPCLQPodsInEntries(entries []grovecorev1alpha1.PodGangEntry, pclqName string) int32 {
	var total int32
	for _, entry := range entries {
		total += entry.PodCliques[pclqName]
	}
	return total
}

// sumPCSGReplicasInEntries sums the replica count for a given PCSG across all entries.
// Computed as the total length of index slices since each index is exactly one replica.
func sumPCSGReplicasInEntries(entries []grovecorev1alpha1.PodGangEntry, pcsgName string) int32 {
	var total int32
	for _, entry := range entries {
		total += int32(len(entry.PCSGReplicaIndices[pcsgName]))
	}
	return total
}

// deductPCLQPodsFromOldEntries deducts the specified number of pods for a PCLQ from old
// entries, starting from the lowest-indexed entry.
func deductPCLQPodsFromOldEntries(entries []grovecorev1alpha1.PodGangEntry, pclqName string, toDeduct int32) []grovecorev1alpha1.PodGangEntry {
	for i := range entries {
		available := entries[i].PodCliques[pclqName]
		if available <= 0 {
			continue
		}
		take := min(available, toDeduct)
		entries[i].PodCliques[pclqName] -= take
		toDeduct -= take
		if toDeduct == 0 {
			break
		}
	}
	return entries
}

// popPCSGIndicesFromOldEntries pops the smallest `count` PCSG replica indices for the given
// PCSG across `entries`, walking entries in slice order. It mutates each entry's index slice
// in place, removing the smallest indices first within an entry. The two-level traversal —
// entries in order, smallest index within each — preserves the existing "lowest first"
// deterministic deduction order.
//
// Returns:
//   - mutatedEntries: the same `entries` slice with the popped indices removed from each
//     entry's PCSGReplicaIndices[pcsgName]. Returned for call-site clarity; the input slice
//     is mutated in place.
//   - takenIndices: the union of indices popped across all entries, sorted ascending. Length
//     is min(count, total available). The caller assigns this to a new entry's
//     PCSGReplicaIndices[pcsgName] so the popped replicas land in their new home.
func popPCSGIndicesFromOldEntries(entries []grovecorev1alpha1.PodGangEntry, pcsgName string, count int32) (mutatedEntries []grovecorev1alpha1.PodGangEntry, takenIndices []int32) {
	takenIndices = make([]int32, 0, count)
	for i := range entries {
		if count == 0 {
			break
		}
		available := entries[i].PCSGReplicaIndices[pcsgName]
		if len(available) == 0 {
			continue
		}
		sorted := append([]int32(nil), available...)
		slices.Sort(sorted)
		take := int32(min(int(count), len(sorted)))
		takenIndices = append(takenIndices, sorted[:take]...)
		entries[i].PCSGReplicaIndices[pcsgName] = sorted[take:]
		count -= take
	}
	slices.Sort(takenIndices)
	mutatedEntries = entries
	return
}

// removeEmptyEntries removes entries where all PodClique pod counts are zero and all
// PCSG index slices are empty.
func removeEmptyEntries(entries []grovecorev1alpha1.PodGangEntry) []grovecorev1alpha1.PodGangEntry {
	return slices.DeleteFunc(entries, func(entry grovecorev1alpha1.PodGangEntry) bool {
		for _, count := range entry.PodCliques {
			if count > 0 {
				return false
			}
		}
		for _, indices := range entry.PCSGReplicaIndices {
			if len(indices) > 0 {
				return false
			}
		}
		return true
	})
}
