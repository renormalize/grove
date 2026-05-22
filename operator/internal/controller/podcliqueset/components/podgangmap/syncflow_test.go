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
	"context"
	"testing"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	groveclientscheme "github.com/ai-dynamo/grove/operator/internal/client"
	testutils "github.com/ai-dynamo/grove/operator/test/utils"
	groveschedulerv1alpha1 "github.com/ai-dynamo/grove/scheduler/api/core/v1alpha1"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	standalonePCLQ := func(mapping map[string]int32) grovecorev1alpha1.PodClique {
		return grovecorev1alpha1.PodClique{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-pcs-0-frontend",
				Namespace: "default",
				Labels: map[string]string{
					apicommon.LabelPartOfKey:                "my-pcs",
					apicommon.LabelPodCliqueSetReplicaIndex: "0",
				},
				OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueSet", Name: "my-pcs"}},
			},
			Status: grovecorev1alpha1.PodCliqueStatus{PodGangMapping: mapping},
		}
	}
	pcsg := func(mapping map[string]int32) grovecorev1alpha1.PodCliqueScalingGroup {
		return grovecorev1alpha1.PodCliqueScalingGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-pcs-0-prefill",
				Namespace: "default",
				Labels:    map[string]string{apicommon.LabelPodCliqueSetReplicaIndex: "0"},
			},
			Status: grovecorev1alpha1.PodCliqueScalingGroupStatus{PodGangMapping: mapping},
		}
	}

	t.Run("standalone PCLQs and PCSGs with PodGangMapping", func(t *testing.T) {
		standalonePCLQs := []grovecorev1alpha1.PodClique{
			standalonePCLQ(map[string]int32{"pg-0": 2, "pg-1": 3}),
		}
		pcsgs := []grovecorev1alpha1.PodCliqueScalingGroup{
			pcsg(map[string]int32{"pg-0": 1, "pg-2": 1}),
		}

		entries := buildEntriesFromStatuses(nil, pcs, standalonePCLQs, pcsgs, 0)

		require.Len(t, entries, 3)
		entryMap := make(map[string]grovecorev1alpha1.PodGangEntry, len(entries))
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
		standalonePCLQs := []grovecorev1alpha1.PodClique{standalonePCLQ(nil)}

		entries := buildEntriesFromStatuses(nil, pcs, standalonePCLQs, nil, 0)
		assert.Empty(t, entries)
	})

	t.Run("preserves DependsOn on entries that already exist in PGM", func(t *testing.T) {
		// Existing PGM has a TailPG with DependsOn=[mpg-0]. The follower must keep that
		// DependsOn intact when the same name reappears in a status mapping.
		existing := []grovecorev1alpha1.PodGangEntry{
			{Name: "mpg-0", PodCliqueSetGenerationHash: "gen-hash-1"},
			{Name: "tail-0", PodCliqueSetGenerationHash: "gen-hash-1", DependsOn: []string{"mpg-0"}},
		}
		standalonePCLQs := []grovecorev1alpha1.PodClique{
			standalonePCLQ(map[string]int32{"mpg-0": 2, "tail-0": 1}),
		}

		entries := buildEntriesFromStatuses(existing, pcs, standalonePCLQs, nil, 0)

		entryMap := make(map[string]grovecorev1alpha1.PodGangEntry, len(entries))
		for _, e := range entries {
			entryMap[e.Name] = e
		}
		assert.Empty(t, entryMap["mpg-0"].DependsOn, "mpg-0 is an anchor — DependsOn stays empty")
		assert.Equal(t, []string{"mpg-0"}, entryMap["tail-0"].DependsOn, "tail-0's DependsOn must be preserved")
	})

	t.Run("net-new Scaled-PG inherits DependsOn from the anchor entries", func(t *testing.T) {
		// PGM has two anchor MPGs (DependsOn empty). PCSG status introduces a new Scaled-PG
		// "spg-new" not yet in PGM. The new entry's DependsOn must list both MPGs so that a
		// future gang-termination recreate enforces "anchors schedule before scale-outs".
		existing := []grovecorev1alpha1.PodGangEntry{
			{Name: "mpg-0", PodCliqueSetGenerationHash: "gen-hash-1"},
			{Name: "mpg-1", PodCliqueSetGenerationHash: "gen-hash-1"},
		}
		pcsgs := []grovecorev1alpha1.PodCliqueScalingGroup{
			pcsg(map[string]int32{"mpg-0": 1, "mpg-1": 1, "spg-new": 1}),
		}

		entries := buildEntriesFromStatuses(existing, pcs, nil, pcsgs, 0)

		entryMap := make(map[string]grovecorev1alpha1.PodGangEntry, len(entries))
		for _, e := range entries {
			entryMap[e.Name] = e
		}
		require.Contains(t, entryMap, "spg-new")
		assert.ElementsMatch(t, []string{"mpg-0", "mpg-1"}, entryMap["spg-new"].DependsOn)
		// Anchor MPGs themselves stay anchors — DependsOn empty.
		assert.Empty(t, entryMap["mpg-0"].DependsOn)
		assert.Empty(t, entryMap["mpg-1"].DependsOn)
	})
}

func TestFilterStandalonePCLQs(t *testing.T) {
	standalone := grovecorev1alpha1.PodClique{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "my-pcs-0-frontend",
			OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueSet", Name: "my-pcs"}},
		},
	}
	pcsgOwned := grovecorev1alpha1.PodClique{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "my-pcs-0-prefill-0-pworker",
			OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueScalingGroup", Name: "my-pcs-0-prefill"}},
		},
	}

	t.Run("returns only PCLQs owned by PodCliqueSet", func(t *testing.T) {
		got := filterStandalonePCLQs([]grovecorev1alpha1.PodClique{standalone, pcsgOwned, standalone})
		require.Len(t, got, 2)
		for _, p := range got {
			assert.Equal(t, "my-pcs-0-frontend", p.Name)
		}
	})

	t.Run("empty input returns empty", func(t *testing.T) {
		assert.Empty(t, filterStandalonePCLQs(nil))
	})
}

func TestAllOwnerMappingsInitialized(t *testing.T) {
	// Default test PCS: 1 standalone PCLQ (frontend) + 1 PCSG (prefill).
	pcs := newTestPCS("my-pcs", "abc12",
		[]grovecorev1alpha1.PodCliqueTemplateSpec{
			{Name: "frontend", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5, MinAvailable: ptr.To(int32(2))}},
			{Name: "pworker", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 3, MinAvailable: ptr.To(int32(2))}},
		},
		[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
			{Name: "prefill", CliqueNames: []string{"pworker"}, Replicas: ptr.To(int32(3)), MinAvailable: ptr.To(int32(1))},
		},
	)

	pclqWith := func(mapping map[string]int32) grovecorev1alpha1.PodClique {
		return grovecorev1alpha1.PodClique{
			Status: grovecorev1alpha1.PodCliqueStatus{PodGangMapping: mapping},
		}
	}
	pcsgWith := func(mapping map[string]int32) grovecorev1alpha1.PodCliqueScalingGroup {
		return grovecorev1alpha1.PodCliqueScalingGroup{
			Status: grovecorev1alpha1.PodCliqueScalingGroupStatus{PodGangMapping: mapping},
		}
	}

	t.Run("returns true when every spec-declared owner is observed and has a non-empty mapping", func(t *testing.T) {
		assert.True(t, allOwnerMappingsInitialized(pcs,
			[]grovecorev1alpha1.PodClique{pclqWith(map[string]int32{"pg-0": 1})},
			[]grovecorev1alpha1.PodCliqueScalingGroup{pcsgWith(map[string]int32{"pg-0": 1})},
		))
	})

	t.Run("returns false when any standalone PCLQ has nil mapping", func(t *testing.T) {
		assert.False(t, allOwnerMappingsInitialized(pcs,
			[]grovecorev1alpha1.PodClique{pclqWith(nil)},
			[]grovecorev1alpha1.PodCliqueScalingGroup{pcsgWith(map[string]int32{"pg-0": 1})},
		))
	})

	t.Run("returns false when any PCSG has empty mapping", func(t *testing.T) {
		assert.False(t, allOwnerMappingsInitialized(pcs,
			[]grovecorev1alpha1.PodClique{pclqWith(map[string]int32{"pg-0": 1})},
			[]grovecorev1alpha1.PodCliqueScalingGroup{pcsgWith(map[string]int32{})},
		))
	})

	t.Run("returns false when no owners observed yet (PCS bootstrap window)", func(t *testing.T) {
		// Spec declares 1 standalone PCLQ + 1 PCSG; cache hasn't seen them yet. The follower
		// must not rebuild PGM during this window — it would wipe entries seeded from spec
		// by createPodGangMapForReplica.
		assert.False(t, allOwnerMappingsInitialized(pcs, nil, nil))
	})

	t.Run("returns false when standalone PCLQ observed but PCSG cache lags", func(t *testing.T) {
		// Spec has 1 standalone PCLQ + 1 PCSG. PCLQ seeded its mapping; PCSG cache hasn't
		// caught up. Rebuilding now would drop the PCSG-side counts from PGM.
		assert.False(t, allOwnerMappingsInitialized(pcs,
			[]grovecorev1alpha1.PodClique{pclqWith(map[string]int32{"pg-0": 1})},
			nil,
		))
	})

	t.Run("returns false during gang-termination window when PCLQs deleted but PCSGs remain", func(t *testing.T) {
		// Gang termination deletes all PCLQs (standalone + PCSG-owned) but leaves PCSGs.
		// Spec still declares 1 standalone PCLQ; cache reports 0. Gate must stay closed
		// until the PCS reconciler recreates the PCLQs and their pod component reseeds.
		assert.False(t, allOwnerMappingsInitialized(pcs,
			nil,
			[]grovecorev1alpha1.PodCliqueScalingGroup{pcsgWith(map[string]int32{"pg-0": 1})},
		))
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

// TestSyncSteadyStateEntries_Integration exercises the full Sync() path with a fake client to
// confirm two follower behaviours end-to-end:
//
//  1. Gate closed (some owner has empty Status.PodGangMapping) → PGM is left as-is.
//  2. Gate open with stale PGM contents → PGM is reconciled to current PCLQ/PCSG status:
//     existing entries' DependsOn is preserved, net-new Scaled-PGs inherit DependsOn from
//     the anchor entries, ghost entries are dropped.
func TestSyncSteadyStateEntries_Integration(t *testing.T) {
	const (
		pcsName    = "my-pcs"
		pcsHash    = "abc12"
		pcsReplica = 0
		pcsUID     = "pcs-test-uid"
	)
	pcsTemplate := func() *grovecorev1alpha1.PodCliqueSet {
		pcs := newTestPCS(pcsName, pcsHash,
			[]grovecorev1alpha1.PodCliqueTemplateSpec{
				{Name: "frontend", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 5}},
				{Name: "pworker", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 2}},
			},
			[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
				{Name: "prefill", CliqueNames: []string{"pworker"}},
			},
		)
		pcs.UID = pcsUID
		pcs.Spec.UpdateStrategy = &grovecorev1alpha1.PodCliqueSetUpdateStrategy{Type: grovecorev1alpha1.CoherentStrategy}
		return pcs
	}
	standalonePCLQ := func(mapping map[string]int32) *grovecorev1alpha1.PodClique {
		return &grovecorev1alpha1.PodClique{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-pcs-0-frontend",
				Namespace: "default",
				Labels: map[string]string{
					apicommon.LabelManagedByKey:             apicommon.LabelManagedByValue,
					apicommon.LabelPartOfKey:                pcsName,
					apicommon.LabelPodCliqueSetReplicaIndex: "0",
				},
				OwnerReferences: []metav1.OwnerReference{{Kind: "PodCliqueSet", Name: pcsName, UID: pcsUID, Controller: ptr.To(true)}},
			},
			Status: grovecorev1alpha1.PodCliqueStatus{PodGangMapping: mapping},
		}
	}
	pcsg := func(mapping map[string]int32) *grovecorev1alpha1.PodCliqueScalingGroup {
		return &grovecorev1alpha1.PodCliqueScalingGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-pcs-0-prefill",
				Namespace: "default",
				Labels: map[string]string{
					apicommon.LabelManagedByKey:             apicommon.LabelManagedByValue,
					apicommon.LabelPartOfKey:                pcsName,
					apicommon.LabelPodCliqueSetReplicaIndex: "0",
				},
			},
			Status: grovecorev1alpha1.PodCliqueScalingGroupStatus{PodGangMapping: mapping},
		}
	}
	pgmWithEntries := func(entries []grovecorev1alpha1.PodGangEntry) *grovecorev1alpha1.PodGangMap {
		return &grovecorev1alpha1.PodGangMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pcsName + "-0",
				Namespace: "default",
				Labels:    getLabels(pcsName, pcsReplica),
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: grovecorev1alpha1.SchemeGroupVersion.String(), Kind: "PodCliqueSet", Name: pcsName, UID: pcsUID, Controller: ptr.To(true)},
				},
			},
			Spec: grovecorev1alpha1.PodGangMapSpec{
				PodCliqueSetReplicaIndex: pcsReplica,
				Entries:                  entries,
			},
		}
	}

	t.Run("gate closed (PCSG mapping nil) leaves PGM untouched", func(t *testing.T) {
		pcs := pcsTemplate()
		pclq := standalonePCLQ(map[string]int32{"my-pcs-0-abc12-0": 5})
		pcsgUninitialised := pcsg(nil) // gate must close on this
		stalePGM := pgmWithEntries([]grovecorev1alpha1.PodGangEntry{
			{
				Name:                       "stale-pg",
				PodCliqueSetGenerationHash: pcsHash,
				PodCliques:                 map[string]int32{"frontend": 999},
			},
		})

		cl := testutils.NewTestClientBuilder().WithObjects(pcs, pclq, pcsgUninitialised, stalePGM).Build()
		r := &_resource{client: cl, scheme: groveclientscheme.Scheme}

		require.NoError(t, r.Sync(context.Background(), logr.Discard(), pcs))

		got := &grovecorev1alpha1.PodGangMap{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: pcsName + "-0"}, got))
		require.Len(t, got.Spec.Entries, 1)
		assert.Equal(t, "stale-pg", got.Spec.Entries[0].Name)
		assert.Equal(t, int32(999), got.Spec.Entries[0].PodCliques["frontend"], "PGM must not be touched while the gate is closed")
	})

	t.Run("gate closed during PCS bootstrap (PCSG cache lags) leaves PGM untouched", func(t *testing.T) {
		// Spec declares 1 standalone PCLQ + 1 PCSG. PCLQ has been observed and seeded its
		// status mapping; the PCSG resource is not yet in the cache (`pcsgs` slice empty
		// because no PCSG object is created in the fake client). The follower must skip
		// this replica until the PCSG is observed — otherwise PGM would be rebuilt from a
		// partial owner set and lose the PCSG-side counts seeded from spec.
		pcs := pcsTemplate()
		pclq := standalonePCLQ(map[string]int32{"my-pcs-0-abc12-0": 5})
		stalePGM := pgmWithEntries([]grovecorev1alpha1.PodGangEntry{
			{
				Name:                       "my-pcs-0-abc12-0",
				PodCliqueSetGenerationHash: pcsHash,
				PodCliques:                 map[string]int32{"frontend": 5},
				PodCliqueScalingGroups:     map[string]int32{"prefill": 2},
			},
		})

		cl := testutils.NewTestClientBuilder().WithObjects(pcs, pclq, stalePGM).Build()
		r := &_resource{client: cl, scheme: groveclientscheme.Scheme}

		require.NoError(t, r.Sync(context.Background(), logr.Discard(), pcs))

		got := &grovecorev1alpha1.PodGangMap{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: pcsName + "-0"}, got))
		require.Len(t, got.Spec.Entries, 1)
		// PCSG-side count must survive — confirms the follower did not rebuild from a partial owner set.
		assert.Equal(t, int32(2), got.Spec.Entries[0].PodCliqueScalingGroups["prefill"])
		assert.Equal(t, int32(5), got.Spec.Entries[0].PodCliques["frontend"])
	})

	t.Run("gate closed during gang termination (PCLQs deleted, PCSG remains) leaves PGM untouched", func(t *testing.T) {
		// Gang termination deletes all PCLQs (standalone + PCSG-owned) but leaves PCSGs.
		// Cache reports zero standalone PCLQs while spec declares one. Gate must stay
		// closed until the PCS reconciler recreates the PCLQs and their pod component
		// reseeds — otherwise PGM would lose its PCLQ-side counts and the recreated
		// PCLQs would seed from a corrupted PGM.
		pcs := pcsTemplate()
		pcsgWithStaleMapping := pcsg(map[string]int32{"my-pcs-0-abc12-0": 1})
		stalePGM := pgmWithEntries([]grovecorev1alpha1.PodGangEntry{
			{
				Name:                       "my-pcs-0-abc12-0",
				PodCliqueSetGenerationHash: pcsHash,
				PodCliques:                 map[string]int32{"frontend": 5},
				PodCliqueScalingGroups:     map[string]int32{"prefill": 1},
			},
		})

		cl := testutils.NewTestClientBuilder().WithObjects(pcs, pcsgWithStaleMapping, stalePGM).Build()
		r := &_resource{client: cl, scheme: groveclientscheme.Scheme}

		require.NoError(t, r.Sync(context.Background(), logr.Discard(), pcs))

		got := &grovecorev1alpha1.PodGangMap{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: pcsName + "-0"}, got))
		require.Len(t, got.Spec.Entries, 1)
		// Standalone PCLQ-side count must survive even though no PCLQ is observed.
		assert.Equal(t, int32(5), got.Spec.Entries[0].PodCliques["frontend"])
		assert.Equal(t, int32(1), got.Spec.Entries[0].PodCliqueScalingGroups["prefill"])
	})

	t.Run("gate open: existing DependsOn preserved, net-new Scaled-PG inherits anchors, ghost dropped", func(t *testing.T) {
		pcs := pcsTemplate()
		// Status mappings: standalone PCLQ contributes one MPG; PCSG contributes the same MPG
		// plus a brand-new Scaled-PG name that is NOT in the existing PGM.
		pclq := standalonePCLQ(map[string]int32{"my-pcs-0-abc12-0": 5})
		pcsgInitialised := pcsg(map[string]int32{
			"my-pcs-0-abc12-0":         1,
			"my-pcs-0-abc12-prefill-0": 1, // freshly minted Scaled-PG
		})
		// Stale PGM:
		//  - real anchor entry "my-pcs-0-abc12-0" with wrong counts (must be overwritten)
		//  - ghost entry "ghost-pg" not referenced by any owner mapping (must be dropped).
		//    DependsOn is set so it is not mistaken for an anchor by collectMPGNamesFromEntries.
		stalePGM := pgmWithEntries([]grovecorev1alpha1.PodGangEntry{
			{
				Name:                       "my-pcs-0-abc12-0",
				PodCliqueSetGenerationHash: pcsHash,
				PodCliques:                 map[string]int32{"frontend": 999},
				PodCliqueScalingGroups:     map[string]int32{"prefill": 999},
			},
			{
				Name:                       "ghost-pg",
				PodCliqueSetGenerationHash: pcsHash,
				PodCliques:                 map[string]int32{"frontend": 7},
				DependsOn:                  []string{"my-pcs-0-abc12-0"},
			},
		})

		cl := testutils.NewTestClientBuilder().WithObjects(pcs, pclq, pcsgInitialised, stalePGM).Build()
		r := &_resource{client: cl, scheme: groveclientscheme.Scheme}

		require.NoError(t, r.Sync(context.Background(), logr.Discard(), pcs))

		got := &grovecorev1alpha1.PodGangMap{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: pcsName + "-0"}, got))

		entryByName := make(map[string]grovecorev1alpha1.PodGangEntry, len(got.Spec.Entries))
		for _, e := range got.Spec.Entries {
			entryByName[e.Name] = e
		}

		// Two entries: the anchor MPG and the new Scaled-PG. Ghost is gone.
		assert.Len(t, got.Spec.Entries, 2, "ghost-pg must be removed")
		_, hasGhost := entryByName["ghost-pg"]
		assert.False(t, hasGhost)

		// my-pcs-0-abc12-0: counts overwritten to status; DependsOn preserved (was nil here).
		anchor := entryByName["my-pcs-0-abc12-0"]
		assert.Equal(t, int32(5), anchor.PodCliques["frontend"])
		assert.Equal(t, int32(1), anchor.PodCliqueScalingGroups["prefill"])
		assert.Empty(t, anchor.DependsOn, "anchor MPG retains its empty DependsOn")

		// my-pcs-0-abc12-prefill-0: new entry, DependsOn inherited from the anchor.
		scaled := entryByName["my-pcs-0-abc12-prefill-0"]
		assert.Equal(t, int32(1), scaled.PodCliqueScalingGroups["prefill"])
		assert.Equal(t, []string{"my-pcs-0-abc12-0"}, scaled.DependsOn)
	})
}
