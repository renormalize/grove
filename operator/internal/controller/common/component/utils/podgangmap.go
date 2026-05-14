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
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
)

// GetPodGangMapEntriesByGenerationHash returns entries that match the given PodCliqueSetGenerationHash.
func GetPodGangMapEntriesByGenerationHash(entries []grovecorev1alpha1.PodGangEntry, hash string) []grovecorev1alpha1.PodGangEntry {
	result := make([]grovecorev1alpha1.PodGangEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.PodCliqueSetGenerationHash == hash {
			result = append(result, entry)
		}
	}
	return result
}

// GetPodGangMapEntriesForPCLQ returns all entries that reference the given standalone PCLQ FQN.
func GetPodGangMapEntriesForPCLQ(entries []grovecorev1alpha1.PodGangEntry, pclqFQN string) []grovecorev1alpha1.PodGangEntry {
	result := make([]grovecorev1alpha1.PodGangEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := entry.PodCliques[pclqFQN]; ok {
			result = append(result, entry)
		}
	}
	return result
}

// GetPodGangMapEntriesForPCSG returns all entries that reference the given PCSG FQN.
func GetPodGangMapEntriesForPCSG(entries []grovecorev1alpha1.PodGangEntry, pcsgFQN string) []grovecorev1alpha1.PodGangEntry {
	result := make([]grovecorev1alpha1.PodGangEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := entry.PodCliqueScalingGroups[pcsgFQN]; ok {
			result = append(result, entry)
		}
	}
	return result
}
