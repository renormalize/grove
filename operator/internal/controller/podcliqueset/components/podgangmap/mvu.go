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

type podGangEntryBuilder func(standalonePCLQPods map[string]int32, pcsgReplicas map[string]int32, isTailPG bool) grovecorev1alpha1.PodGangEntry

// computeNextIteration determines the next set of PodGang entries to create for the current
// coherent update iteration. It checks if a full MVU PodGang can be formed from the remaining
// old pods and PCSG replicas. If yes, it produces exactly one MVU PodGang entry (absorbing
// leftover standalone PCLQ pods if no further MVU can be formed after it). If a full MVU cannot
// be formed, it produces one Tail-PG entry per remaining PCSG replica.
// Returns nil if there are no remaining pods or replicas to process.
//
// Parameters:
//   - template: the fixed MVU template for this update
//   - remainingOldPods: count of old-version pods per updated standalone PCLQ still to be taken down
//   - remainingOldReplicas: count of old-version PCSG replicas per updated PCSG still to be taken down
//   - entryBuilder: a function that creates a PodGangEntry given the composition
//
// The entryBuilder is passed in to decouple naming/hash concerns from the pure computation logic.
func computeNextIteration(
	template mvuTemplate,
	remainingOldPods map[string]int32,
	remainingOldReplicas map[string]int32,
	entryBuilder podGangEntryBuilder) (entries []grovecorev1alpha1.PodGangEntry, done bool) {
	// Check if a MVU PodGang can be created from the remaining old PCLQ pods and old PCSG replicas.
	if canFormMVUPodGang(template, remainingOldPods, remainingOldReplicas) {
		return computeNextMVUPodGang(template, remainingOldPods, remainingOldReplicas, entryBuilder), false
	}
	// If you have reached here, then only Tail-PGs remain. Create PodGangEntries for Tail-PGs.
	return computeTailPodGangs(remainingOldReplicas, entryBuilder)
}

// canFormMVUPodGang checks whether remaining old standalone PCLQ pods and old PCSG replicas can fill a complete MVU.
func canFormMVUPodGang(template mvuTemplate, remainingOldStandalonePCLQPods map[string]int32, remainingOldReplicas map[string]int32) bool {
	for pclqName, minAvailable := range template.standalonePCLQs {
		if remainingOldStandalonePCLQPods[pclqName] < minAvailable {
			return false
		}
	}
	for pcsgName, minAvailable := range template.pcsgs {
		if remainingOldReplicas[pcsgName] < minAvailable {
			return false
		}
	}
	return true
}

// computeNextMVUPodGang computes the constituents of next MVU PodGang, potentially absorbing
// leftover standalone PCLQ pods if another full MVU PodGang cannot be formed after this one.
func computeNextMVUPodGang(
	template mvuTemplate,
	remainingOldStandalonePCLQPodCounts map[string]int32,
	remainingOldPCSGReplicas map[string]int32,
	entryBuilder podGangEntryBuilder) []grovecorev1alpha1.PodGangEntry {

	// clone the standalone PCLQ map from the template as this will be conditionally mutated
	// based on if the remaining old standalone PCLQ pods and PCSG replicas can form another MVU.
	nextMVUStandAlonePCLQs := maps.Clone(template.standalonePCLQs)

	// Deduct from remaining.
	for pclqName, minAvailable := range template.standalonePCLQs {
		remainingOldStandalonePCLQPodCounts[pclqName] -= minAvailable
	}
	for pcsgName, minAvailable := range template.pcsgs {
		remainingOldPCSGReplicas[pcsgName] -= minAvailable
	}

	// Check if another full MVU can be formed after this deduction.
	canFormAnother := canFormMVUPodGang(template, remainingOldStandalonePCLQPodCounts, remainingOldPCSGReplicas)
	if !canFormAnother {
		// Absorb ALL remaining standalone PCLQ pods into this MVU PodGang.
		// PCSG replicas are NOT absorbed — they remain for Tail-PGs.
		for pclq := range template.standalonePCLQs {
			if remainingOldStandalonePCLQPodCounts[pclq] > 0 {
				nextMVUStandAlonePCLQs[pclq] += remainingOldStandalonePCLQPodCounts[pclq]
				remainingOldStandalonePCLQPodCounts[pclq] = 0
			}
		}
	}

	return []grovecorev1alpha1.PodGangEntry{entryBuilder(nextMVUStandAlonePCLQs, template.pcsgs, false)}
}

// computeTailPodGangs create one Tail-PG per remaining PCSG replica.
// Returns the Tail-PG entries and whether the update is done (true if no remaining replicas).
func computeTailPodGangs(remainingOldPCSGReplicas map[string]int32, entryBuilder podGangEntryBuilder) ([]grovecorev1alpha1.PodGangEntry, bool) {
	var entries []grovecorev1alpha1.PodGangEntry
	for pcsg, count := range remainingOldPCSGReplicas {
		for range count {
			entry := entryBuilder(nil, map[string]int32{pcsg: 1}, true)
			entries = append(entries, entry)
		}
		remainingOldPCSGReplicas[pcsg] = 0
	}

	if len(entries) == 0 {
		return nil, true
	}

	return entries, false
}
