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
	"testing"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComputeInFlightDeficits(t *testing.T) {
	entries := []grovecorev1alpha1.PodGangEntry{
		{Name: "pg-old-0", PodCliques: map[string]int32{"frontend": 3}},
		{Name: "pg-new-0", PodCliques: map[string]int32{"frontend": 2}},
		{Name: "pg-new-1", PodCliques: map[string]int32{"frontend": 3}},
	}

	t.Run("no existing pods for in-flight PodGangs", func(t *testing.T) {
		deficits := computeInFlightDeficits(entries, []string{"pg-new-0", "pg-new-1"}, "frontend", nil)
		assert.Equal(t, map[string]int32{"pg-new-0": 2, "pg-new-1": 3}, deficits)
	})

	t.Run("some pods already exist in in-flight PodGangs", func(t *testing.T) {
		existingPods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-0"}}},
		}
		deficits := computeInFlightDeficits(entries, []string{"pg-new-0", "pg-new-1"}, "frontend", existingPods)
		assert.Equal(t, map[string]int32{"pg-new-0": 1, "pg-new-1": 3}, deficits)
	})

	t.Run("all pods already exist — no deficit", func(t *testing.T) {
		existingPods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-1"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-3", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-1"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-4", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-1"}}},
		}
		deficits := computeInFlightDeficits(entries, []string{"pg-new-0", "pg-new-1"}, "frontend", existingPods)
		assert.Empty(t, deficits)
	})

	t.Run("in-flight PodGang does not reference this clique", func(t *testing.T) {
		pcsgEntries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-tail-0", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		}
		deficits := computeInFlightDeficits(pcsgEntries, []string{"pg-tail-0"}, "frontend", nil)
		assert.Empty(t, deficits)
	})
}

func TestGetPodsFromDecrementedPodGangs(t *testing.T) {
	t.Run("identifies pods from PodGangs with more pods than desired", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliques: map[string]int32{"frontend": 1}},
			{Name: "pg-new-0", PodCliques: map[string]int32{"frontend": 2}},
		}
		existingPods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-0"}}},
		}

		candidates := getPodsFromDecrementedPodGangs(entries, "frontend", existingPods, []string{"pg-new-0"})

		// pg-old-0 has 3 pods but desired is 1 → decremented → all 3 pods are candidates
		candidateNames := getPodNames(candidates)
		assert.ElementsMatch(t, []string{"pod-0", "pod-1", "pod-2"}, candidateNames)
	})

	t.Run("skips PodGangs at or below desired count", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliques: map[string]int32{"frontend": 3}},
		}
		existingPods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-0"}}},
		}

		candidates := getPodsFromDecrementedPodGangs(entries, "frontend", existingPods, nil)
		assert.Empty(t, candidates)
	})

	t.Run("skips in-flight PodGangs", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-new-0", PodCliques: map[string]int32{"frontend": 2}},
		}
		existingPods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Labels: map[string]string{apicommon.LabelPodGang: "pg-new-0"}}},
		}

		candidates := getPodsFromDecrementedPodGangs(entries, "frontend", existingPods, []string{"pg-new-0"})
		assert.Empty(t, candidates)
	})

	t.Run("candidates from multiple decremented PodGangs", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliques: map[string]int32{"frontend": 1}},
			{Name: "pg-old-1", PodCliques: map[string]int32{"frontend": 2}},
			{Name: "pg-new-0", PodCliques: map[string]int32{"frontend": 2}},
		}
		existingPods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-1"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-3", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-1"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-4", Labels: map[string]string{apicommon.LabelPodGang: "pg-old-1"}}},
		}

		candidates := getPodsFromDecrementedPodGangs(entries, "frontend", existingPods, []string{"pg-new-0"})

		// pg-old-0: has 2 pods, desired 1 → decremented → pod-0, pod-1
		// pg-old-1: has 3 pods, desired 2 → decremented → pod-2, pod-3, pod-4
		candidateNames := getPodNames(candidates)
		assert.ElementsMatch(t, []string{"pod-0", "pod-1", "pod-2", "pod-3", "pod-4"}, candidateNames)
	})
}

func TestSelectPodsForDeletion(t *testing.T) {
	t.Run("returns nil for empty candidates", func(t *testing.T) {
		result := selectPodsForDeletion(nil, 3, "hash")
		assert.Nil(t, result)
	})

	t.Run("returns nil for zero count", func(t *testing.T) {
		pods := []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod-0"}}}
		result := selectPodsForDeletion(pods, 0, "hash")
		assert.Nil(t, result)
	})

	t.Run("selects requested number of pods", func(t *testing.T) {
		pods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-2"}},
		}
		result := selectPodsForDeletion(pods, 2, "hash")
		assert.Len(t, result, 2)
	})

	t.Run("does not modify original slice", func(t *testing.T) {
		pods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}},
		}
		original := make([]*corev1.Pod, len(pods))
		copy(original, pods)

		selectPodsForDeletion(pods, 1, "hash")
		assert.Equal(t, original[0].Name, pods[0].Name)
		assert.Equal(t, original[1].Name, pods[1].Name)
	})
}

func TestGroupPodsByPodGang(t *testing.T) {
	pods := []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-0"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-0"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Labels: map[string]string{apicommon.LabelPodGang: "pg-1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-3"}},
	}

	grouped := groupPodsByPodGang(pods)

	assert.Len(t, grouped, 2)
	assert.Len(t, grouped["pg-0"], 2)
	assert.Len(t, grouped["pg-1"], 1)
}

func TestSumDeficits(t *testing.T) {
	assert.Equal(t, 0, sumDeficits(nil))
	assert.Equal(t, 0, sumDeficits(map[string]int32{}))
	assert.Equal(t, 5, sumDeficits(map[string]int32{"pg-0": 2, "pg-1": 3}))
}

func TestCountPodsPerPodGang(t *testing.T) {
	pods := []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-0"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-0"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Labels: map[string]string{apicommon.LabelPodGang: "pg-1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-3"}},
	}

	counts := countPodsPerPodGang(pods)
	assert.Equal(t, map[string]int32{"pg-0": 2, "pg-1": 1}, counts)
}

func TestFindHighestMVUPodGangForClique(t *testing.T) {
	t.Run("returns last entry referencing the clique", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-0", PodCliques: map[string]int32{"frontend": 2}},
			{Name: "pg-1", PodCliques: map[string]int32{"frontend": 3}},
			{Name: "pg-tail-0", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		}
		assert.Equal(t, "pg-1", findHighestMVUPodGangForClique(entries, "frontend"))
	})

	t.Run("returns empty string when no entry references the clique", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-tail-0", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		}
		assert.Equal(t, "", findHighestMVUPodGangForClique(entries, "frontend"))
	})
}

func TestAssignPodsToDeficitPodGangs(t *testing.T) {
	t.Run("distributes pods across entries with deficits", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-0", PodCliques: map[string]int32{"frontend": 2}},
			{Name: "pg-1", PodCliques: map[string]int32{"frontend": 3}},
		}
		assignments := assignPodsToDeficitPodGangs(entries, "frontend", nil, 5)
		assert.Equal(t, []string{"pg-0", "pg-0", "pg-1", "pg-1", "pg-1"}, assignments)
	})

	t.Run("accounts for existing pods", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-0", PodCliques: map[string]int32{"frontend": 2}},
			{Name: "pg-1", PodCliques: map[string]int32{"frontend": 3}},
		}
		existingPods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Labels: map[string]string{apicommon.LabelPodGang: "pg-1"}}},
		}
		// pg-0 full, pg-1 needs 2 more
		assignments := assignPodsToDeficitPodGangs(entries, "frontend", existingPods, 2)
		assert.Equal(t, []string{"pg-1", "pg-1"}, assignments)
	})

	t.Run("scale-out assigns to highest MVU PodGang", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-0", PodCliques: map[string]int32{"frontend": 2}},
			{Name: "pg-1", PodCliques: map[string]int32{"frontend": 3}},
		}
		existingPods := []*corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-0", Labels: map[string]string{apicommon.LabelPodGang: "pg-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: map[string]string{apicommon.LabelPodGang: "pg-0"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Labels: map[string]string{apicommon.LabelPodGang: "pg-1"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-3", Labels: map[string]string{apicommon.LabelPodGang: "pg-1"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-4", Labels: map[string]string{apicommon.LabelPodGang: "pg-1"}}},
		}
		// All PodGangs full, 2 extra pods (scale-out) → go to pg-1 (highest)
		assignments := assignPodsToDeficitPodGangs(entries, "frontend", existingPods, 2)
		assert.Equal(t, []string{"pg-1", "pg-1"}, assignments)
	})

	t.Run("BPG/SPG — single entry gets all pods", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "my-pcs-0", PodCliques: map[string]int32{"frontend": 5}},
		}
		assignments := assignPodsToDeficitPodGangs(entries, "frontend", nil, 5)
		assert.Equal(t, []string{"my-pcs-0", "my-pcs-0", "my-pcs-0", "my-pcs-0", "my-pcs-0"}, assignments)
	})
}

func TestGetPCSReplicaIndexFromPCLQ(t *testing.T) {
	t.Run("extracts valid index", func(t *testing.T) {
		pclq := &grovecorev1alpha1.PodClique{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{apicommon.LabelPodCliqueSetReplicaIndex: "2"},
			},
		}
		idx, err := getPCSReplicaIndexFromPCLQ(pclq)
		require.NoError(t, err)
		assert.Equal(t, 2, idx)
	})

	t.Run("errors when label missing", func(t *testing.T) {
		pclq := &grovecorev1alpha1.PodClique{
			ObjectMeta: metav1.ObjectMeta{Name: "my-pclq", Labels: map[string]string{}},
		}
		_, err := getPCSReplicaIndexFromPCLQ(pclq)
		assert.Error(t, err)
	})

	t.Run("errors when label not an integer", func(t *testing.T) {
		pclq := &grovecorev1alpha1.PodClique{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "my-pclq",
				Labels: map[string]string{apicommon.LabelPodCliqueSetReplicaIndex: "abc"},
			},
		}
		_, err := getPCSReplicaIndexFromPCLQ(pclq)
		assert.Error(t, err)
	})
}

func TestIsStandalonePCLQCoherentUpdate(t *testing.T) {
	t.Run("returns true when all conditions met", func(t *testing.T) {
		sc := &syncContext{
			isStandalonePCLQ: true,
			pcs: &grovecorev1alpha1.PodCliqueSet{
				Spec: grovecorev1alpha1.PodCliqueSetSpec{
					UpdateStrategy: &grovecorev1alpha1.PodCliqueSetUpdateStrategy{
						Type: grovecorev1alpha1.CoherentStrategy,
					},
				},
				Status: grovecorev1alpha1.PodCliqueSetStatus{
					UpdateProgress: &grovecorev1alpha1.PodCliqueSetUpdateProgress{},
				},
			},
			pclq: &grovecorev1alpha1.PodClique{
				Status: grovecorev1alpha1.PodCliqueStatus{
					UpdateProgress: &grovecorev1alpha1.PodCliqueUpdateProgress{},
				},
			},
		}
		assert.True(t, isStandalonePCLQCoherentUpdate(sc))
	})

	t.Run("returns false when not standalone", func(t *testing.T) {
		sc := &syncContext{
			isStandalonePCLQ: false,
			pcs: &grovecorev1alpha1.PodCliqueSet{
				Spec: grovecorev1alpha1.PodCliqueSetSpec{
					UpdateStrategy: &grovecorev1alpha1.PodCliqueSetUpdateStrategy{
						Type: grovecorev1alpha1.CoherentStrategy,
					},
				},
				Status: grovecorev1alpha1.PodCliqueSetStatus{
					UpdateProgress: &grovecorev1alpha1.PodCliqueSetUpdateProgress{},
				},
			},
			pclq: &grovecorev1alpha1.PodClique{
				Status: grovecorev1alpha1.PodCliqueStatus{
					UpdateProgress: &grovecorev1alpha1.PodCliqueUpdateProgress{},
				},
			},
		}
		assert.False(t, isStandalonePCLQCoherentUpdate(sc))
	})

	t.Run("returns false when no coherent update in progress", func(t *testing.T) {
		sc := &syncContext{
			isStandalonePCLQ: true,
			pcs:              &grovecorev1alpha1.PodCliqueSet{},
			pclq:             &grovecorev1alpha1.PodClique{},
		}
		assert.False(t, isStandalonePCLQCoherentUpdate(sc))
	})
}

func getPodNames(pods []*corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	return names
}
