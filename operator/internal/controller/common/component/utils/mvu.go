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

package utils

import (
	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
)

// MVUTemplate describes the composition of one Minimum Viable Unit (MVU) PodGang.
// It specifies the minimum number of pods per standalone PodClique and the minimum
// number of replicas per PodCliqueScalingGroup that must be gang-scheduled together.
type MVUTemplate struct {
	// StandalonePCLQs maps standalone PodClique name to the minAvailable pod count.
	StandalonePCLQs map[string]int32
	// PCSGs maps PodCliqueScalingGroup name to the minAvailable replica count.
	PCSGs map[string]int32
}

// PodGangEntryBuilder is a function that creates a PodGangEntry given standalone PCLQ replicas
// and PCSG replicas for the PodGang.
type PodGangEntryBuilder func(standalonePCLQReplicas map[string]int32, pcsgReplicas map[string]int32) grovecorev1alpha1.PodGangEntry

// ComputeMVUTemplateFromPCS computes the MVU template for a PCS from its spec.
func ComputeMVUTemplateFromPCS(pcs *grovecorev1alpha1.PodCliqueSet) MVUTemplate {
	return MVUTemplate{
		StandalonePCLQs: GetStandalonePCLQMinAvailableFromPCS(pcs),
		PCSGs:           GetPCSGMinAvailableFromPCS(pcs),
	}
}

// GetStandalonePCLQMinAvailableFromPCS returns the minAvailable pod count per standalone PCLQ from the PCS spec.
func GetStandalonePCLQMinAvailableFromPCS(pcs *grovecorev1alpha1.PodCliqueSet) map[string]int32 {
	result := make(map[string]int32)
	for _, cliqueTemplate := range pcs.Spec.Template.Cliques {
		pcsgConfig := FindScalingGroupConfigForClique(pcs.Spec.Template.PodCliqueScalingGroupConfigs, cliqueTemplate.Name)
		if pcsgConfig == nil {
			result[cliqueTemplate.Name] = *cliqueTemplate.Spec.MinAvailable
		}
	}
	return result
}

// GetStandalonePCLQReplicasFromPCS returns the total replica count per standalone PCLQ from the PCS spec.
func GetStandalonePCLQReplicasFromPCS(pcs *grovecorev1alpha1.PodCliqueSet) map[string]int32 {
	result := make(map[string]int32)
	for _, cliqueTemplate := range pcs.Spec.Template.Cliques {
		pcsgConfig := FindScalingGroupConfigForClique(pcs.Spec.Template.PodCliqueScalingGroupConfigs, cliqueTemplate.Name)
		if pcsgConfig == nil {
			result[cliqueTemplate.Name] = cliqueTemplate.Spec.Replicas
		}
	}
	return result
}

// GetPCSGMinAvailableFromPCS returns the minAvailable replica count per PCSG from the PCS spec.
func GetPCSGMinAvailableFromPCS(pcs *grovecorev1alpha1.PodCliqueSet) map[string]int32 {
	result := make(map[string]int32)
	for _, pcsgConfig := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		result[pcsgConfig.Name] = *pcsgConfig.MinAvailable
	}
	return result
}

// GetPCSGReplicasFromPCS returns the total replica count per PCSG from the PCS spec.
func GetPCSGReplicasFromPCS(pcs *grovecorev1alpha1.PodCliqueSet) map[string]int32 {
	result := make(map[string]int32)
	for _, pcsgConfig := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		result[pcsgConfig.Name] = *pcsgConfig.Replicas
	}
	return result
}

// NewPodGangEntryBuilder returns a closure that creates PodGangEntry values with
// sequentially-numbered names. The counter is incremented on each call.
func NewPodGangEntryBuilder(pcsName string, pcsReplicaIndex int32, pcsGenerationHash string, podGangIndex *int32) PodGangEntryBuilder {
	return func(standalonePCLQReplicas map[string]int32, pcsgReplicas map[string]int32) grovecorev1alpha1.PodGangEntry {
		name := apicommon.GeneratePodGangName(pcsName, pcsReplicaIndex, pcsGenerationHash, *podGangIndex)
		*podGangIndex++
		return grovecorev1alpha1.PodGangEntry{
			Name:                       name,
			PodCliqueSetGenerationHash: pcsGenerationHash,
			PodCliques:                 standalonePCLQReplicas,
			PodCliqueScalingGroups:     pcsgReplicas,
		}
	}
}
