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
	"testing"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComputeInFlightPCSGDeficits(t *testing.T) {
	entries := []grovecorev1alpha1.PodGangEntry{
		{Name: "pg-old-0", PodCliqueScalingGroups: map[string]int32{"prefill": 3}},
		{Name: "pg-new-0", PodCliqueScalingGroups: map[string]int32{"prefill": 2}},
		{Name: "pg-new-1", PodCliqueScalingGroups: map[string]int32{"prefill": 3}},
	}

	t.Run("no existing PCLQs in in-flight PodGangs", func(t *testing.T) {
		deficits, err := computeInFlightPCSGDeficits(entries, []string{"pg-new-0", "pg-new-1"}, "prefill", nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]int32{"pg-new-0": 2, "pg-new-1": 3}, deficits)
	})

	t.Run("partial existing replicas in in-flight PodGangs", func(t *testing.T) {
		// Two PCLQs sharing replica index "0" in pg-new-0 → counts as one PCSG replica.
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-a", "pg-new-0", "0"),
			pclqWithLabels("pclq-b", "pg-new-0", "0"),
		}
		deficits, err := computeInFlightPCSGDeficits(entries, []string{"pg-new-0", "pg-new-1"}, "prefill", existingPCLQs)
		require.NoError(t, err)
		assert.Equal(t, map[string]int32{"pg-new-0": 1, "pg-new-1": 3}, deficits)
	})

	t.Run("all replicas already exist — no deficit", func(t *testing.T) {
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0", "pg-new-0", "0"),
			pclqWithLabels("pclq-1", "pg-new-0", "1"),
			pclqWithLabels("pclq-2", "pg-new-1", "2"),
			pclqWithLabels("pclq-3", "pg-new-1", "3"),
			pclqWithLabels("pclq-4", "pg-new-1", "4"),
		}
		deficits, err := computeInFlightPCSGDeficits(entries, []string{"pg-new-0", "pg-new-1"}, "prefill", existingPCLQs)
		require.NoError(t, err)
		assert.Empty(t, deficits)
	})

	t.Run("in-flight PodGang does not reference this PCSG", func(t *testing.T) {
		pclqEntries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-tail-0", PodCliques: map[string]int32{"frontend": 1}},
		}
		deficits, err := computeInFlightPCSGDeficits(pclqEntries, []string{"pg-tail-0"}, "prefill", nil)
		require.NoError(t, err)
		assert.Empty(t, deficits)
	})

	t.Run("existing replicas exceed desired — no deficit recorded", func(t *testing.T) {
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0", "pg-new-0", "0"),
			pclqWithLabels("pclq-1", "pg-new-0", "1"),
			pclqWithLabels("pclq-2", "pg-new-0", "2"),
		}
		deficits, err := computeInFlightPCSGDeficits(entries, []string{"pg-new-0"}, "prefill", existingPCLQs)
		require.NoError(t, err)
		assert.Empty(t, deficits)
	})

	t.Run("empty inputs", func(t *testing.T) {
		deficits, err := computeInFlightPCSGDeficits(nil, nil, "prefill", nil)
		require.NoError(t, err)
		assert.Empty(t, deficits)
		deficits, err = computeInFlightPCSGDeficits(entries, nil, "prefill", nil)
		require.NoError(t, err)
		assert.Empty(t, deficits)
	})

	t.Run("non-numeric replica index label propagates error", func(t *testing.T) {
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-bad", "pg-new-0", "abc"),
		}
		_, err := computeInFlightPCSGDeficits(entries, []string{"pg-new-0"}, "prefill", existingPCLQs)
		assert.Error(t, err)
	})
}

func TestGetReplicaIndicesFromDecrementedPodGangs(t *testing.T) {
	t.Run("identifies replicas from decremented old PodGangs", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
			{Name: "pg-new-0", PodCliqueScalingGroups: map[string]int32{"prefill": 2}},
		}
		// pg-old-0 has 3 unique replica indices but desired is 1 → all 3 are candidates.
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0", "pg-old-0", "0"),
			pclqWithLabels("pclq-1", "pg-old-0", "1"),
			pclqWithLabels("pclq-2", "pg-old-0", "2"),
		}
		indices, err := getReplicaIndicesFromDecrementedPodGangs(entries, "prefill", existingPCLQs, []string{"pg-new-0"})
		require.NoError(t, err)
		assert.ElementsMatch(t, []int{0, 1, 2}, indices)
	})

	t.Run("dedups replica indices across multiple PCLQs of the same replica", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		}
		// Replica index "0" appears in two PCLQs (different cliques) → counted once.
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0a", "pg-old-0", "0"),
			pclqWithLabels("pclq-0b", "pg-old-0", "0"),
			pclqWithLabels("pclq-1a", "pg-old-0", "1"),
			pclqWithLabels("pclq-1b", "pg-old-0", "1"),
		}
		indices, err := getReplicaIndicesFromDecrementedPodGangs(entries, "prefill", existingPCLQs, nil)
		require.NoError(t, err)
		assert.ElementsMatch(t, []int{0, 1}, indices)
	})

	t.Run("skips PodGangs at or below desired count", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliqueScalingGroups: map[string]int32{"prefill": 3}},
		}
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0", "pg-old-0", "0"),
			pclqWithLabels("pclq-1", "pg-old-0", "1"),
		}
		indices, err := getReplicaIndicesFromDecrementedPodGangs(entries, "prefill", existingPCLQs, nil)
		require.NoError(t, err)
		assert.Empty(t, indices)
	})

	t.Run("skips in-flight PodGangs", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-new-0", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		}
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0", "pg-new-0", "0"),
			pclqWithLabels("pclq-1", "pg-new-0", "1"),
		}
		indices, err := getReplicaIndicesFromDecrementedPodGangs(entries, "prefill", existingPCLQs, []string{"pg-new-0"})
		require.NoError(t, err)
		assert.Empty(t, indices)
	})

	t.Run("skips entries that do not reference this PCSG", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliques: map[string]int32{"frontend": 3}},
		}
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0", "pg-old-0", "0"),
		}
		indices, err := getReplicaIndicesFromDecrementedPodGangs(entries, "prefill", existingPCLQs, nil)
		require.NoError(t, err)
		assert.Empty(t, indices)
	})

	t.Run("aggregates replicas from multiple decremented PodGangs", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
			{Name: "pg-old-1", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
			{Name: "pg-new-0", PodCliqueScalingGroups: map[string]int32{"prefill": 2}},
		}
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0", "pg-old-0", "0"),
			pclqWithLabels("pclq-1", "pg-old-0", "1"),
			pclqWithLabels("pclq-2", "pg-old-1", "2"),
			pclqWithLabels("pclq-3", "pg-old-1", "3"),
		}
		indices, err := getReplicaIndicesFromDecrementedPodGangs(entries, "prefill", existingPCLQs, []string{"pg-new-0"})
		require.NoError(t, err)
		assert.ElementsMatch(t, []int{0, 1, 2, 3}, indices)
	})

	t.Run("empty inputs", func(t *testing.T) {
		indices, err := getReplicaIndicesFromDecrementedPodGangs(nil, "prefill", nil, nil)
		require.NoError(t, err)
		assert.Empty(t, indices)
	})

	t.Run("non-numeric replica index label propagates error", func(t *testing.T) {
		entries := []grovecorev1alpha1.PodGangEntry{
			{Name: "pg-old-0", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		}
		existingPCLQs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-bad", "pg-old-0", "not-a-number"),
		}
		_, err := getReplicaIndicesFromDecrementedPodGangs(entries, "prefill", existingPCLQs, nil)
		assert.Error(t, err)
	})
}

func TestGroupPCSGReplicaIndicesByPodGang(t *testing.T) {
	t.Run("groups unique replica indices per PodGang", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0a", "pg-0", "0"),
			pclqWithLabels("pclq-0b", "pg-0", "0"),
			pclqWithLabels("pclq-1", "pg-0", "1"),
			pclqWithLabels("pclq-2", "pg-1", "2"),
		}
		grouped, err := groupPCSGReplicaIndicesByPodGang(pclqs)
		require.NoError(t, err)
		assert.Len(t, grouped, 2)
		assert.ElementsMatch(t, []int{0, 1}, grouped["pg-0"])
		assert.ElementsMatch(t, []int{2}, grouped["pg-1"])
	})

	t.Run("skips PCLQs missing PodGang label", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			{ObjectMeta: metav1.ObjectMeta{Name: "pclq-no-pg", Labels: map[string]string{
				apicommon.LabelPodCliqueScalingGroupReplicaIndex: "0",
			}}},
			pclqWithLabels("pclq-0", "pg-0", "0"),
		}
		grouped, err := groupPCSGReplicaIndicesByPodGang(pclqs)
		require.NoError(t, err)
		assert.Len(t, grouped, 1)
		assert.ElementsMatch(t, []int{0}, grouped["pg-0"])
	})

	t.Run("skips PCLQs missing replica index label", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			{ObjectMeta: metav1.ObjectMeta{Name: "pclq-no-idx", Labels: map[string]string{
				apicommon.LabelPodGang: "pg-0",
			}}},
			pclqWithLabels("pclq-0", "pg-0", "0"),
		}
		grouped, err := groupPCSGReplicaIndicesByPodGang(pclqs)
		require.NoError(t, err)
		assert.Len(t, grouped, 1)
		assert.ElementsMatch(t, []int{0}, grouped["pg-0"])
	})

	t.Run("non-numeric replica index label returns error", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-bad", "pg-0", "abc"),
		}
		_, err := groupPCSGReplicaIndicesByPodGang(pclqs)
		assert.Error(t, err)
	})

	t.Run("empty input returns empty map", func(t *testing.T) {
		grouped, err := groupPCSGReplicaIndicesByPodGang(nil)
		require.NoError(t, err)
		assert.Empty(t, grouped)
	})
}

func TestCountExistingPCSGReplicasPerPodGang(t *testing.T) {
	t.Run("counts unique replica indices per PodGang", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-0a", "pg-0", "0"),
			pclqWithLabels("pclq-0b", "pg-0", "0"),
			pclqWithLabels("pclq-1", "pg-0", "1"),
			pclqWithLabels("pclq-2", "pg-1", "2"),
		}
		counts, err := countExistingPCSGReplicasPerPodGang(pclqs)
		require.NoError(t, err)
		assert.Equal(t, map[string]int32{"pg-0": 2, "pg-1": 1}, counts)
	})

	t.Run("empty input returns empty map", func(t *testing.T) {
		counts, err := countExistingPCSGReplicasPerPodGang(nil)
		require.NoError(t, err)
		assert.Empty(t, counts)
	})

	t.Run("non-numeric replica index label propagates error", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			pclqWithLabels("pclq-bad", "pg-0", "abc"),
		}
		_, err := countExistingPCSGReplicasPerPodGang(pclqs)
		assert.Error(t, err)
	})
}

func TestBuildReplicaAssignments(t *testing.T) {
	t.Run("pairs each deficit slot with a freed index", func(t *testing.T) {
		assignments := buildReplicaAssignments(map[string]int32{"pg-new-0": 2}, []int{3, 4})
		assert.Equal(t, map[int]string{3: "pg-new-0", 4: "pg-new-0"}, assignments)
	})

	t.Run("stops when freed indices are exhausted", func(t *testing.T) {
		// Total deficit 4 but only 2 freed indices available.
		assignments := buildReplicaAssignments(map[string]int32{"pg-new-0": 4}, []int{0, 1})
		assert.Equal(t, map[int]string{0: "pg-new-0", 1: "pg-new-0"}, assignments)
	})

	t.Run("returns empty when no freed indices", func(t *testing.T) {
		assignments := buildReplicaAssignments(map[string]int32{"pg-new-0": 2}, nil)
		assert.Empty(t, assignments)
	})

	t.Run("returns empty when no deficits", func(t *testing.T) {
		assignments := buildReplicaAssignments(nil, []int{0, 1})
		assert.Empty(t, assignments)
	})

	t.Run("distributes across multiple PodGangs", func(t *testing.T) {
		assignments := buildReplicaAssignments(map[string]int32{"pg-new-0": 1, "pg-new-1": 1}, []int{0, 1})
		assert.Len(t, assignments, 2)
		// Map iteration order is unspecified — assert that both PodGangs are assigned to distinct indices.
		gangs := make(map[string]bool)
		for _, pg := range assignments {
			gangs[pg] = true
		}
		assert.True(t, gangs["pg-new-0"])
		assert.True(t, gangs["pg-new-1"])
	})
}

func TestSumPCSGDeficits(t *testing.T) {
	assert.Equal(t, 0, sumPCSGDeficits(nil))
	assert.Equal(t, 0, sumPCSGDeficits(map[string]int32{}))
	assert.Equal(t, 5, sumPCSGDeficits(map[string]int32{"pg-0": 2, "pg-1": 3}))
	assert.Equal(t, 1, sumPCSGDeficits(map[string]int32{"pg-0": 1}))
}

func pclqWithLabels(name, podGang, pcsgReplicaIndex string) grovecorev1alpha1.PodClique {
	return grovecorev1alpha1.PodClique{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				apicommon.LabelPodGang:                           podGang,
				apicommon.LabelPodCliqueScalingGroupReplicaIndex: pcsgReplicaIndex,
			},
		},
	}
}
