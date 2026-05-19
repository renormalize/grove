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
	"testing"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	groveschedulerv1alpha1 "github.com/ai-dynamo/grove/scheduler/api/core/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestBuildEntriesFromStatuses(t *testing.T) {
	pcs := newTestPCS("my-pcs", "gen-hash-1",
		[]grovecorev1alpha1.PodCliqueTemplateSpec{
			{Name: "frontend", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5, MinAvailable: ptr.To(int32(2))}},
			{Name: "pworker", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 3, MinAvailable: ptr.To(int32(2))}},
		},
		[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
			{Name: "prefill", CliqueNames: []string{"pworker"}, Replicas: ptr.To(int32(3)), MinAvailable: ptr.To(int32(1))},
		},
	)

	t.Run("standalone PCLQs and PCSGs with PodGangMapping", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-pcs-0-frontend",
					Namespace: "default",
					Labels: map[string]string{
						apicommon.LabelPartOfKey:                "my-pcs",
						apicommon.LabelPodCliqueSetReplicaIndex: "0",
					},
					OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueSet", Name: "my-pcs"}},
				},
				Status: grovecorev1alpha1.PodCliqueStatus{
					PodGangMapping: map[string]int32{
						"pg-0": 2,
						"pg-1": 3,
					},
				},
			},
		}
		pcsgs := []grovecorev1alpha1.PodCliqueScalingGroup{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-pcs-0-prefill",
					Namespace: "default",
					Labels: map[string]string{
						apicommon.LabelPodCliqueSetReplicaIndex: "0",
					},
				},
				Status: grovecorev1alpha1.PodCliqueScalingGroupStatus{
					PodGangMapping: map[string]int32{
						"pg-0": 1,
						"pg-2": 1,
					},
				},
			},
		}

		entries := buildEntriesFromStatuses(pcs, 0, pclqs, pcsgs)

		require.Len(t, entries, 3)
		entryMap := make(map[string]grovecorev1alpha1.PodGangEntry)
		for _, e := range entries {
			entryMap[e.Name] = e
		}

		// pg-0 has both frontend pods and prefill replicas
		assert.Equal(t, int32(2), entryMap["pg-0"].PodCliques["frontend"])
		assert.Equal(t, int32(1), entryMap["pg-0"].PodCliqueScalingGroups["prefill"])
		assert.Equal(t, "gen-hash-1", entryMap["pg-0"].PodCliqueSetGenerationHash)

		// pg-1 has only frontend pods
		assert.Equal(t, int32(3), entryMap["pg-1"].PodCliques["frontend"])
		assert.Nil(t, entryMap["pg-1"].PodCliqueScalingGroups)

		// pg-2 has only prefill replicas
		assert.Nil(t, entryMap["pg-2"].PodCliques)
		assert.Equal(t, int32(1), entryMap["pg-2"].PodCliqueScalingGroups["prefill"])
	})

	t.Run("empty PodGangMapping returns no entries", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-pcs-0-frontend",
					Namespace: "default",
					Labels: map[string]string{
						apicommon.LabelPartOfKey:                "my-pcs",
						apicommon.LabelPodCliqueSetReplicaIndex: "0",
					},
					OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueSet", Name: "my-pcs"}},
				},
				Status: grovecorev1alpha1.PodCliqueStatus{},
			},
		}

		entries := buildEntriesFromStatuses(pcs, 0, pclqs, nil)
		assert.Empty(t, entries)
	})
}

func TestBuildBasePodGangEntry(t *testing.T) {
	pcs := newTestPCS("my-pcs", "gen-hash-1",
		[]grovecorev1alpha1.PodCliqueTemplateSpec{
			{Name: "frontend", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5, MinAvailable: ptr.To(int32(2))}},
			{Name: "pworker", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 3, MinAvailable: ptr.To(int32(2))}},
		},
		[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
			{Name: "prefill", CliqueNames: []string{"pworker"}, Replicas: ptr.To(int32(3)), MinAvailable: ptr.To(int32(1))},
		},
	)
	pcsNameReplica := apicommon.ResourceNameReplica{Name: "my-pcs", Replica: 0}

	pclqs := []grovecorev1alpha1.PodClique{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-pcs-0-frontend",
				Namespace: "default",
				Labels: map[string]string{
					apicommon.LabelPartOfKey:                "my-pcs",
					apicommon.LabelPodCliqueSetReplicaIndex: "0",
				},
				OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueSet", Name: "my-pcs"}},
			},
			Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-pcs-0-prefill-0-pworker",
				Namespace: "default",
				Labels: map[string]string{
					apicommon.LabelPodCliqueScalingGroup:             "my-pcs-0-prefill",
					apicommon.LabelPodCliqueScalingGroupReplicaIndex: "0",
					apicommon.LabelPodCliqueSetReplicaIndex:          "0",
				},
				OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueScalingGroup", Name: "my-pcs-0-prefill"}},
			},
			Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 3},
		},
	}

	entry := buildBasePodGangEntry(pcs, pcsNameReplica, "gen-hash-1", pclqs)

	assert.Equal(t, "my-pcs-0", entry.Name)
	assert.Equal(t, "gen-hash-1", entry.PodCliqueSetGenerationHash)
	// Only standalone PCLQ (frontend) should be in PodCliques
	assert.Equal(t, map[string]int32{"frontend": 5}, entry.PodCliques)
	// PCSG minAvailable
	assert.Equal(t, map[string]int32{"prefill": 1}, entry.PodCliqueScalingGroups)
}

func TestBuildScaledPodGangEntries(t *testing.T) {
	pcs := newTestPCS("my-pcs", "gen-hash-1",
		[]grovecorev1alpha1.PodCliqueTemplateSpec{
			{Name: "pworker", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 3, MinAvailable: ptr.To(int32(2))}},
		},
		[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
			{Name: "prefill", CliqueNames: []string{"pworker"}, Replicas: ptr.To(int32(4)), MinAvailable: ptr.To(int32(1))},
		},
	)
	pcsNameReplica := apicommon.ResourceNameReplica{Name: "my-pcs", Replica: 0}

	t.Run("creates SPG entries for replicas above minAvailable", func(t *testing.T) {
		pcsgs := []grovecorev1alpha1.PodCliqueScalingGroup{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "my-pcs-0-prefill", Namespace: "default"},
				Spec:       grovecorev1alpha1.PodCliqueScalingGroupSpec{Replicas: 4},
			},
		}

		entries := buildScaledPodGangEntries(pcs, pcsNameReplica, "gen-hash-1", pcsgs)

		// 4 replicas - 1 minAvailable = 3 scaled PodGangs
		require.Len(t, entries, 3)
		assert.Equal(t, "my-pcs-0-prefill-0", entries[0].Name)
		assert.Equal(t, "my-pcs-0-prefill-1", entries[1].Name)
		assert.Equal(t, "my-pcs-0-prefill-2", entries[2].Name)
		for _, entry := range entries {
			assert.Equal(t, "gen-hash-1", entry.PodCliqueSetGenerationHash)
			assert.Equal(t, map[string]int32{"prefill": 1}, entry.PodCliqueScalingGroups)
			assert.Nil(t, entry.PodCliques)
		}
	})

	t.Run("no SPG entries when replicas equals minAvailable", func(t *testing.T) {
		pcsgs := []grovecorev1alpha1.PodCliqueScalingGroup{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "my-pcs-0-prefill", Namespace: "default"},
				Spec:       grovecorev1alpha1.PodCliqueScalingGroupSpec{Replicas: 1},
			},
		}

		entries := buildScaledPodGangEntries(pcs, pcsNameReplica, "gen-hash-1", pcsgs)
		assert.Empty(t, entries)
	})

	t.Run("falls back to config replicas when PCSG resource not found", func(t *testing.T) {
		// No PCSGs passed — falls back to config Replicas=4
		entries := buildScaledPodGangEntries(pcs, pcsNameReplica, "gen-hash-1", nil)

		// 4 replicas - 1 minAvailable = 3 scaled PodGangs
		require.Len(t, entries, 3)
	})
}

func TestGetPCSGCurrentReplicas(t *testing.T) {
	pcsgConfig := grovecorev1alpha1.PodCliqueScalingGroupConfig{
		Name:     "prefill",
		Replicas: ptr.To(int32(3)),
	}

	t.Run("returns spec replicas when PCSG exists", func(t *testing.T) {
		pcsgs := []grovecorev1alpha1.PodCliqueScalingGroup{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "my-pcs-0-prefill"},
				Spec:       grovecorev1alpha1.PodCliqueScalingGroupSpec{Replicas: 5},
			},
		}
		assert.Equal(t, 5, getPCSGCurrentReplicas(pcsgs, "my-pcs-0-prefill", pcsgConfig))
	})

	t.Run("falls back to config replicas when PCSG not found", func(t *testing.T) {
		assert.Equal(t, 3, getPCSGCurrentReplicas(nil, "my-pcs-0-prefill", pcsgConfig))
	})
}

func TestComputeMVUEntriesFromSpec(t *testing.T) {
	t.Run("standalone PCLQs only", func(t *testing.T) {
		pcs := newTestPCS("my-pcs", "gen-hash-1",
			[]grovecorev1alpha1.PodCliqueTemplateSpec{
				{Name: "frontend", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5, MinAvailable: ptr.To(int32(2))}},
			},
			nil,
		)

		entries := computeMVUEntriesFromSpec(pcs, 0)

		// 5 replicas, minAvailable=2: 2 full MVUs (2+3 with absorption), 0 Tail-PGs
		require.Len(t, entries, 2)
		assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])
		assert.Equal(t, int32(3), entries[1].PodCliques["frontend"])
		assert.Equal(t, "gen-hash-1", entries[0].PodCliqueSetGenerationHash)
	})

	t.Run("PCSGs only", func(t *testing.T) {
		pcs := newTestPCS("my-pcs", "gen-hash-1",
			[]grovecorev1alpha1.PodCliqueTemplateSpec{
				{Name: "worker", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 2, MinAvailable: ptr.To(int32(2))}},
			},
			[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
				{Name: "sg", CliqueNames: []string{"worker"}, Replicas: ptr.To(int32(4)), MinAvailable: ptr.To(int32(1))},
			},
		)

		entries := computeMVUEntriesFromSpec(pcs, 0)

		// No standalone PCLQs. PCSG: 4 replicas, minAvail=1.
		// 1 MVU with 1 PCSG replica, then 3 Tail-PGs
		require.Len(t, entries, 4)
		assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["sg"])
		for i := 1; i < 4; i++ {
			assert.Equal(t, int32(1), entries[i].PodCliqueScalingGroups["sg"])
			assert.Empty(t, entries[i].PodCliques)
		}
	})

	t.Run("mixed standalone PCLQs and PCSGs", func(t *testing.T) {
		pcs := newTestPCS("my-pcs", "gen-hash-1",
			[]grovecorev1alpha1.PodCliqueTemplateSpec{
				{Name: "frontend", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5, MinAvailable: ptr.To(int32(2))}},
				{Name: "pworker", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 3, MinAvailable: ptr.To(int32(2))}},
			},
			[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
				{Name: "prefill", CliqueNames: []string{"pworker"}, Replicas: ptr.To(int32(4)), MinAvailable: ptr.To(int32(1))},
			},
		)

		entries := computeMVUEntriesFromSpec(pcs, 0)

		// Frontend: 5 replicas, minAvail=2. PCSG prefill: 4 replicas, minAvail=1.
		// MVU template: {frontend: 2, prefill: 1}
		// Iteration 1: canForm=true (5>=2, 4>=1). Create MVU {F:2, P:1}. Remaining: F:3, P:3.
		//   canFormAnother=true (3>=2, 3>=1).
		// Iteration 2: canForm=true (3>=2, 3>=1). Create MVU {F:2, P:1}. Remaining: F:1, P:2.
		//   canFormAnother=false (1<2). Absorb F: MVU becomes {F:3, P:1}. Remaining: F:0, P:2.
		// Tail-PGs: P:2 → 2 Tail-PGs.
		// Total: 2 MVUs + 2 Tail-PGs = 4 entries.
		require.Len(t, entries, 4)
		assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])
		assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
		assert.Equal(t, int32(3), entries[1].PodCliques["frontend"])
		assert.Equal(t, int32(1), entries[1].PodCliqueScalingGroups["prefill"])
		assert.Empty(t, entries[2].PodCliques)
		assert.Equal(t, int32(1), entries[2].PodCliqueScalingGroups["prefill"])
		assert.Empty(t, entries[3].PodCliques)
		assert.Equal(t, int32(1), entries[3].PodCliqueScalingGroups["prefill"])
	})

	t.Run("empty PCS spec produces no entries", func(t *testing.T) {
		pcs := newTestPCS("my-pcs", "gen-hash-1", nil, nil)

		entries := computeMVUEntriesFromSpec(pcs, 0)
		assert.Empty(t, entries)
	})
}

func TestHasMVUPodGangs(t *testing.T) {
	t.Run("returns true when PodGangCounter has entries", func(t *testing.T) {
		pcs := &grovecorev1alpha1.PodCliqueSet{
			Status: grovecorev1alpha1.PodCliqueSetStatus{
				PodGangCounter: map[string]int32{"0": 2},
			},
		}
		assert.True(t, hasMVUPodGangs(pcs))
	})

	t.Run("returns false when PodGangCounter is nil", func(t *testing.T) {
		pcs := &grovecorev1alpha1.PodCliqueSet{}
		assert.False(t, hasMVUPodGangs(pcs))
	})

	t.Run("returns false when PodGangCounter is empty", func(t *testing.T) {
		pcs := &grovecorev1alpha1.PodCliqueSet{
			Status: grovecorev1alpha1.PodCliqueSetStatus{
				PodGangCounter: map[string]int32{},
			},
		}
		assert.False(t, hasMVUPodGangs(pcs))
	})
}

func TestHasInFlightPodGangs(t *testing.T) {
	t.Run("returns false when UpdateProgress is nil", func(t *testing.T) {
		pcs := &grovecorev1alpha1.PodCliqueSet{}
		assert.False(t, hasInFlightPodGangs(pcs))
	})

	t.Run("returns false when CurrentlyUpdating is empty", func(t *testing.T) {
		pcs := &grovecorev1alpha1.PodCliqueSet{
			Status: grovecorev1alpha1.PodCliqueSetStatus{
				UpdateProgress: &grovecorev1alpha1.PodCliqueSetUpdateProgress{},
			},
		}
		assert.False(t, hasInFlightPodGangs(pcs))
	})

	t.Run("returns false when InFlightPodGangs is empty", func(t *testing.T) {
		pcs := &grovecorev1alpha1.PodCliqueSet{
			Status: grovecorev1alpha1.PodCliqueSetStatus{
				UpdateProgress: &grovecorev1alpha1.PodCliqueSetUpdateProgress{
					CurrentlyUpdating: []grovecorev1alpha1.PodCliqueSetReplicaUpdateProgress{
						{ReplicaIndex: 0},
					},
				},
			},
		}
		assert.False(t, hasInFlightPodGangs(pcs))
	})

	t.Run("returns true when InFlightPodGangs is populated", func(t *testing.T) {
		pcs := &grovecorev1alpha1.PodCliqueSet{
			Status: grovecorev1alpha1.PodCliqueSetStatus{
				UpdateProgress: &grovecorev1alpha1.PodCliqueSetUpdateProgress{
					CurrentlyUpdating: []grovecorev1alpha1.PodCliqueSetReplicaUpdateProgress{
						{ReplicaIndex: 0, InFlightPodGangs: []string{"pg-0"}},
					},
				},
			},
		}
		assert.True(t, hasInFlightPodGangs(pcs))
	})
}

func TestBuildBaseAndScaledPodGangEntries(t *testing.T) {
	pcs := newTestPCS("my-pcs", "gen-hash-1",
		[]grovecorev1alpha1.PodCliqueTemplateSpec{
			{Name: "frontend", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5, MinAvailable: ptr.To(int32(2))}},
			{Name: "pworker", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 3, MinAvailable: ptr.To(int32(2))}},
		},
		[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
			{Name: "prefill", CliqueNames: []string{"pworker"}, Replicas: ptr.To(int32(3)), MinAvailable: ptr.To(int32(1))},
		},
	)

	pclqs := []grovecorev1alpha1.PodClique{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-pcs-0-frontend",
				Namespace: "default",
				Labels: map[string]string{
					apicommon.LabelPartOfKey:                "my-pcs",
					apicommon.LabelPodCliqueSetReplicaIndex: "0",
				},
				OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueSet", Name: "my-pcs"}},
			},
			Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5},
		},
	}

	pcsgs := []grovecorev1alpha1.PodCliqueScalingGroup{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "my-pcs-0-prefill", Namespace: "default"},
			Spec:       grovecorev1alpha1.PodCliqueScalingGroupSpec{Replicas: 3},
		},
	}

	entries := buildBaseAndScaledPodGangEntries(pcs, 0, pclqs, pcsgs)

	// 1 BPG + 2 SPGs (3 replicas - 1 minAvailable = 2 scaled)
	require.Len(t, entries, 3)

	// BPG
	assert.Equal(t, "my-pcs-0", entries[0].Name)
	assert.Equal(t, int32(5), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])

	// SPGs
	assert.Equal(t, "my-pcs-0-prefill-0", entries[1].Name)
	assert.Equal(t, map[string]int32{"prefill": 1}, entries[1].PodCliqueScalingGroups)
	assert.Equal(t, "my-pcs-0-prefill-1", entries[2].Name)
	assert.Equal(t, map[string]int32{"prefill": 1}, entries[2].PodCliqueScalingGroups)
}

func TestGetPodGangPCSReplicaIndex(t *testing.T) {
	const pcsName = "my-pcs"
	tests := []struct {
		name        string
		pgName      string
		labels      map[string]string
		expectIndex int
		expectOK    bool
	}{
		{
			name:        "label present and valid",
			pgName:      "my-pcs-3-abc12-0",
			labels:      map[string]string{apicommon.LabelPodCliqueSetReplicaIndex: "3"},
			expectIndex: 3,
			expectOK:    true,
		},
		{
			name:        "legacy BPG without label",
			pgName:      "my-pcs-2",
			labels:      nil,
			expectIndex: 2,
			expectOK:    true,
		},
		{
			name:        "legacy SPG without label",
			pgName:      "my-pcs-2-prefill-1",
			labels:      nil,
			expectIndex: 2,
			expectOK:    true,
		},
		{
			name:        "label has invalid integer falls back to name",
			pgName:      "my-pcs-4-prefill-1",
			labels:      map[string]string{apicommon.LabelPodCliqueSetReplicaIndex: "not-a-number"},
			expectIndex: 4,
			expectOK:    true,
		},
		{
			name:     "name does not start with pcs name prefix",
			pgName:   "other-pcs-0",
			labels:   nil,
			expectOK: false,
		},
		{
			name:     "name has pcs prefix but non-numeric replica segment",
			pgName:   "my-pcs-x-prefill-0",
			labels:   nil,
			expectOK: false,
		},
		{
			name:     "name equals pcs name with no replica segment",
			pgName:   "my-pcs",
			labels:   nil,
			expectOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pg := groveschedulerv1alpha1.PodGang{
				ObjectMeta: metav1.ObjectMeta{
					Name:   tc.pgName,
					Labels: tc.labels,
				},
			}
			idx, ok := getPodGangPCSReplicaIndex(pg, pcsName)
			assert.Equal(t, tc.expectOK, ok)
			if tc.expectOK {
				assert.Equal(t, tc.expectIndex, idx)
			}
		})
	}
}
