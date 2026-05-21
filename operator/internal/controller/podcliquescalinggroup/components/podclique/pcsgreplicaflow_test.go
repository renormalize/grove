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
	"sort"
	"testing"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	testutils "github.com/ai-dynamo/grove/operator/test/utils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
)

// Common test fixtures.
//
// Names follow the conventions from final-plan.md §9 ("PCSG reconciler PCLQ component flow"):
//
//   - MPG / TailPG (coherent-update mints): <pcsName>-<pcsReplicaIndex>-<hash>-<counter>
//   - Scaled-PG (new convention, post-upgrade steady-state mints): <pcsName>-<pcsReplicaIndex>-<hash>-<pcsgConfigName>-<counter>
//   - Legacy SPG (pre-upgrade scaled PodGang): <pcsgFQN>-<counter>
//   - BPG (legacy base PodGang): <pcsName>-<pcsReplicaIndex>
const (
	tcsPCSName         = "simple1"
	tcsPCSReplica      = 0
	tcsHash            = "abc123"
	tcsPCSGConfigName  = "sga"
	tcsPCSGFQN         = "simple1-0-sga"
	tcsBPGName         = "simple1-0"
	tcsScaledPGPrefix  = "simple1-0-abc123-sga-"
	tcsLegacySPGPrefix = "simple1-0-sga-"
	// MPGs / TailPGs.
	tcsMPG0    = "simple1-0-abc123-0"
	tcsMPG1    = "simple1-0-abc123-1"
	tcsTailPG2 = "simple1-0-abc123-2"
	tcsTailPG3 = "simple1-0-abc123-3"
	// Scaled-PGs (new convention).
	tcsScaledPG0 = "simple1-0-abc123-sga-0"
	tcsScaledPG1 = "simple1-0-abc123-sga-1"
	// Legacy SPGs.
	tcsLegacySPG0 = "simple1-0-sga-0"
	tcsLegacySPG1 = "simple1-0-sga-1"
)

func TestScaledPodGangNamePrefix(t *testing.T) {
	got := scaledPodGangNamePrefix(tcsPCSName, tcsPCSReplica, tcsHash, tcsPCSGConfigName)
	assert.Equal(t, tcsScaledPGPrefix, got)
}

func TestNextScaledPodGangIndex(t *testing.T) {
	tests := []struct {
		name    string
		mapping map[string]int32
		expect  int
		wantErr bool
	}{
		{
			name:    "empty mapping returns 0",
			mapping: map[string]int32{},
			expect:  0,
		},
		{
			name: "no Scaled-PG entries returns 0",
			mapping: map[string]int32{
				tcsMPG0: 2,
				tcsMPG1: 2,
			},
			expect: 0,
		},
		{
			name: "single Scaled-PG returns next index",
			mapping: map[string]int32{
				tcsMPG0:      2,
				tcsScaledPG0: 1,
			},
			expect: 1,
		},
		{
			name: "multiple Scaled-PGs returns max + 1",
			mapping: map[string]int32{
				tcsScaledPG0: 1,
				tcsScaledPG1: 1,
			},
			expect: 2,
		},
		{
			name: "ignores legacy SPG entries (they don't share the new prefix)",
			mapping: map[string]int32{
				tcsLegacySPG0: 1,
				tcsLegacySPG1: 1,
			},
			expect: 0,
		},
		{
			name: "ignores non-matching MPG/TailPG entries",
			mapping: map[string]int32{
				tcsTailPG2:   1,
				tcsScaledPG0: 1,
			},
			expect: 1,
		},
		{
			name: "errors on unparseable Scaled-PG name",
			mapping: map[string]int32{
				tcsScaledPGPrefix + "notanumber": 1,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextScaledPodGangIndex(tc.mapping, tcsScaledPGPrefix)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expect, got)
		})
	}
}

func TestGenerateScaledPodGangNames(t *testing.T) {
	tests := []struct {
		name           string
		currentMapping map[string]int32
		count          int
		expectNames    []string
	}{
		{
			name:           "first mint starts at 0",
			currentMapping: map[string]int32{},
			count:          2,
			expectNames:    []string{tcsScaledPG0, tcsScaledPG1},
		},
		{
			name: "subsequent mint continues from max+1",
			currentMapping: map[string]int32{
				tcsScaledPG0: 1,
			},
			count:       1,
			expectNames: []string{tcsScaledPG1},
		},
		{
			name: "ignores legacy SPGs when computing next index",
			currentMapping: map[string]int32{
				tcsLegacySPG0: 1,
				tcsLegacySPG1: 1,
			},
			count:       1,
			expectNames: []string{tcsScaledPG0},
		},
		{
			name:           "count zero returns empty slice",
			currentMapping: map[string]int32{},
			count:          0,
			expectNames:    []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := generateScaledPodGangNames(
				tc.currentMapping, tc.count,
				tcsPCSName, tcsPCSReplica, tcsHash, tcsPCSGConfigName, tcsScaledPGPrefix,
			)
			require.NoError(t, err)
			assert.Equal(t, tc.expectNames, got)
		})
	}
}

func TestPartitionPodGangNamesByTier(t *testing.T) {
	tests := []struct {
		name                 string
		mapping              map[string]int32
		expectTierLegacySPG  []string
		expectTierNewScaled  []string
		expectTierMPGTail    []string
	}{
		{
			name: "BPG excluded; new Scaled-PGs in tierNewScaled; MPGs/TailPGs in tierMPGTail",
			mapping: map[string]int32{
				tcsBPGName:   2,
				tcsMPG0:      2,
				tcsTailPG2:   1,
				tcsScaledPG0: 1,
			},
			expectTierLegacySPG: nil,
			expectTierNewScaled: []string{tcsScaledPG0},
			expectTierMPGTail:   []string{tcsMPG0, tcsTailPG2},
		},
		{
			name: "legacy SPGs and new Scaled-PGs partition into their respective tiers",
			mapping: map[string]int32{
				tcsBPGName:    2,
				tcsLegacySPG0: 1,
				tcsLegacySPG1: 1,
				tcsScaledPG0:  1,
			},
			expectTierLegacySPG: []string{tcsLegacySPG0, tcsLegacySPG1},
			expectTierNewScaled: []string{tcsScaledPG0},
			expectTierMPGTail:   nil,
		},
		{
			name:                "empty mapping returns empty tiers",
			mapping:             map[string]int32{},
			expectTierLegacySPG: nil,
			expectTierNewScaled: nil,
			expectTierMPGTail:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tierLegacy, tierNew, tierMPG := partitionPodGangNamesByTier(tc.mapping, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName)
			sort.Strings(tierLegacy)
			sort.Strings(tierNew)
			sort.Strings(tierMPG)
			assert.Equal(t, tc.expectTierLegacySPG, tierLegacy)
			assert.Equal(t, tc.expectTierNewScaled, tierNew)
			assert.Equal(t, tc.expectTierMPGTail, tierMPG)
		})
	}
}

func TestSortDescByPodGangIndex(t *testing.T) {
	t.Run("MPG/TailPG names sorted by trailing counter desc", func(t *testing.T) {
		names := []string{tcsMPG0, tcsTailPG3, tcsMPG1, tcsTailPG2}
		require.NoError(t, sortDescByPodGangIndex(names))
		assert.Equal(t, []string{tcsTailPG3, tcsTailPG2, tcsMPG1, tcsMPG0}, names)
	})

	t.Run("Scaled-PG names sorted by trailing counter desc", func(t *testing.T) {
		names := []string{tcsScaledPG0, tcsScaledPG1}
		require.NoError(t, sortDescByPodGangIndex(names))
		assert.Equal(t, []string{tcsScaledPG1, tcsScaledPG0}, names)
	})

	t.Run("returns error on unparseable name", func(t *testing.T) {
		names := []string{tcsMPG0, "not-a-pg-name"}
		err := sortDescByPodGangIndex(names)
		require.Error(t, err)
	})
}

func TestDecrementPCSGMappingForScaleIn(t *testing.T) {
	t.Run("count=0 is a no-op", func(t *testing.T) {
		mapping := map[string]int32{tcsScaledPG0: 1}
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 0, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, map[string]int32{tcsScaledPG0: 1}, mapping)
	})

	t.Run("tier 1 (legacy SPG) drained before tier 2 (new Scaled-PG)", func(t *testing.T) {
		// Mixed mapping post-Grove-upgrade: budget=1 should drain the highest-counter legacy SPG first.
		mapping := map[string]int32{
			tcsLegacySPG0: 1,
			tcsLegacySPG1: 1,
			tcsScaledPG0:  1,
		}
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		// Highest-counter legacy SPG (LegacySPG1) drained; LegacySPG0 and ScaledPG0 untouched.
		assert.Equal(t, int32(0), mapping[tcsLegacySPG1])
		assert.Equal(t, int32(1), mapping[tcsLegacySPG0])
		assert.Equal(t, int32(1), mapping[tcsScaledPG0])
	})

	t.Run("tier 1 exhausted then tier 2 — Scaled-PGs by counter desc", func(t *testing.T) {
		mapping := map[string]int32{
			tcsLegacySPG0: 1,
			tcsScaledPG0:  1,
			tcsScaledPG1:  1,
		}
		// Need 2: drain LegacySPG0, then highest-counter Scaled-PG (ScaledPG1).
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 2, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, int32(0), mapping[tcsLegacySPG0])
		assert.Equal(t, int32(0), mapping[tcsScaledPG1])
		assert.Equal(t, int32(1), mapping[tcsScaledPG0])
	})

	t.Run("tier 2 only — Scaled-PGs drained highest counter first", func(t *testing.T) {
		mapping := map[string]int32{
			tcsMPG0:      2,
			tcsScaledPG0: 1,
			tcsScaledPG1: 1,
		}
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, int32(0), mapping[tcsScaledPG1])
		assert.Equal(t, int32(1), mapping[tcsScaledPG0])
		assert.Equal(t, int32(2), mapping[tcsMPG0])
	})

	t.Run("tiers 1 and 2 exhausted then tier 3 — TailPG before MPG (higher counter wins)", func(t *testing.T) {
		mapping := map[string]int32{
			tcsMPG0:       2,
			tcsMPG1:       2,
			tcsTailPG2:    1,
			tcsScaledPG0:  1,
			tcsLegacySPG0: 1,
		}
		// Need 3: drain LegacySPG0 (tier 1), drain ScaledPG0 (tier 2), drain TailPG2 (tier 3 highest).
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 3, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, int32(0), mapping[tcsLegacySPG0])
		assert.Equal(t, int32(0), mapping[tcsScaledPG0])
		assert.Equal(t, int32(0), mapping[tcsTailPG2])
		assert.Equal(t, int32(2), mapping[tcsMPG0])
		assert.Equal(t, int32(2), mapping[tcsMPG1])
	})

	t.Run("MPG with count > 1 absorbs multiple decrements", func(t *testing.T) {
		mapping := map[string]int32{
			tcsMPG0: 2,
			tcsMPG1: 3,
		}
		// Tier 3 only. MPG1 has higher counter → decrement it twice to 1.
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 2, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, int32(2), mapping[tcsMPG0])
		assert.Equal(t, int32(1), mapping[tcsMPG1])
	})

	t.Run("BPG never decremented even when budget exhausts every other tier", func(t *testing.T) {
		mapping := map[string]int32{
			tcsBPGName:    2,
			tcsLegacySPG0: 1,
		}
		// Budget=1: tier 1 has LegacySPG0; that drains. BPG must remain at 2.
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, int32(2), mapping[tcsBPGName])
		assert.Equal(t, int32(0), mapping[tcsLegacySPG0])
	})

	t.Run("returns error when tier 1 has unparseable name", func(t *testing.T) {
		mapping := map[string]int32{
			tcsLegacySPGPrefix + "bogus": 1,
		}
		err := decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName)
		require.Error(t, err)
	})

	t.Run("returns error when tier 2 has unparseable name", func(t *testing.T) {
		mapping := map[string]int32{
			tcsScaledPGPrefix + "bogus": 1,
		}
		err := decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName)
		require.Error(t, err)
	})
}

func TestBuildLiveIndexToPodGang(t *testing.T) {
	pclqWith := func(name, podGang, idx string) grovecorev1alpha1.PodClique {
		return *testutils.NewPCSGPodCliqueBuilder(name, "default", tcsPCSName, tcsPCSGFQN, tcsPCSReplica, 0).
			WithLabels(map[string]string{
				apicommon.LabelPodGang:                           podGang,
				apicommon.LabelPodCliqueScalingGroupReplicaIndex: idx,
			}).Build()
	}

	t.Run("happy path — multiple PCLQs at multiple indices", func(t *testing.T) {
		pclqs := []grovecorev1alpha1.PodClique{
			pclqWith("simple1-0-sga-0-pca", tcsMPG0, "0"),
			pclqWith("simple1-0-sga-1-pca", tcsMPG0, "1"),
			pclqWith("simple1-0-sga-2-pca", tcsMPG1, "2"),
		}
		got, err := buildLiveIndexToPodGang(pclqs)
		require.NoError(t, err)
		assert.Equal(t, map[int]string{0: tcsMPG0, 1: tcsMPG0, 2: tcsMPG1}, got)
	})

	t.Run("multiple PCLQs at same index (one per CliqueName) collapse to one entry", func(t *testing.T) {
		// Index 0 with two CliqueNames — both share the same LabelPodGang.
		pclqs := []grovecorev1alpha1.PodClique{
			pclqWith("simple1-0-sga-0-pca", tcsMPG0, "0"),
			pclqWith("simple1-0-sga-0-pcb", tcsMPG0, "0"),
		}
		got, err := buildLiveIndexToPodGang(pclqs)
		require.NoError(t, err)
		assert.Equal(t, map[int]string{0: tcsMPG0}, got)
	})

	t.Run("terminating PCLQs are skipped", func(t *testing.T) {
		now := metav1.Now()
		terminating := pclqWith("simple1-0-sga-0-pca", tcsMPG0, "0")
		terminating.DeletionTimestamp = &now
		terminating.Finalizers = []string{"grove.io/test-finalizer"}
		live := pclqWith("simple1-0-sga-1-pca", tcsMPG0, "1")
		got, err := buildLiveIndexToPodGang([]grovecorev1alpha1.PodClique{terminating, live})
		require.NoError(t, err)
		assert.Equal(t, map[int]string{1: tcsMPG0}, got)
	})

	t.Run("missing LabelPodGang surfaces an error", func(t *testing.T) {
		pclq := pclqWith("simple1-0-sga-0-pca", tcsMPG0, "0")
		delete(pclq.Labels, apicommon.LabelPodGang)
		_, err := buildLiveIndexToPodGang([]grovecorev1alpha1.PodClique{pclq})
		require.Error(t, err)
	})

	t.Run("unparseable replica-index label surfaces an error", func(t *testing.T) {
		pclq := pclqWith("simple1-0-sga-0-pca", tcsMPG0, "not-a-number")
		_, err := buildLiveIndexToPodGang([]grovecorev1alpha1.PodClique{pclq})
		require.Error(t, err)
	})
}

func TestComputePCSGCountDeltas(t *testing.T) {
	t.Run("steady state — no deltas", func(t *testing.T) {
		desired := map[string]int32{tcsMPG0: 2, tcsMPG1: 2}
		live := map[int]string{0: tcsMPG0, 1: tcsMPG0, 2: tcsMPG1, 3: tcsMPG1}
		dels, creates := computePCSGCountDeltas(desired, live)
		assert.Empty(t, dels)
		assert.Empty(t, creates)
	})

	t.Run("Case 1 — gap fill: live short by 1 in MPG1", func(t *testing.T) {
		desired := map[string]int32{tcsMPG0: 2, tcsMPG1: 2}
		live := map[int]string{0: tcsMPG0, 1: tcsMPG0, 2: tcsMPG1} // index 3 missing
		dels, creates := computePCSGCountDeltas(desired, live)
		assert.Empty(t, dels)
		assert.Equal(t, map[string]int{tcsMPG1: 1}, creates)
	})

	t.Run("Case 2 — scale-in: excess in MPG1 deletes highest-index PCLQ in MPG1", func(t *testing.T) {
		desired := map[string]int32{tcsMPG0: 2, tcsMPG1: 1}
		live := map[int]string{0: tcsMPG0, 1: tcsMPG0, 2: tcsMPG1, 3: tcsMPG1}
		dels, creates := computePCSGCountDeltas(desired, live)
		assert.Equal(t, []int{3}, dels)
		assert.Empty(t, creates)
	})

	t.Run("Case 3 — scale-out: new Scaled-PG not yet in live", func(t *testing.T) {
		desired := map[string]int32{tcsMPG0: 2, tcsMPG1: 2, tcsScaledPG0: 1}
		live := map[int]string{0: tcsMPG0, 1: tcsMPG0, 2: tcsMPG1, 3: tcsMPG1}
		dels, creates := computePCSGCountDeltas(desired, live)
		assert.Empty(t, dels)
		assert.Equal(t, map[string]int{tcsScaledPG0: 1}, creates)
	})

	t.Run("obsolete PodGang — live PCLQs whose PodGang is gone from desired are deleted", func(t *testing.T) {
		desired := map[string]int32{tcsMPG0: 2}
		live := map[int]string{0: tcsMPG0, 1: tcsMPG0, 2: tcsMPG1, 3: tcsMPG1}
		dels, creates := computePCSGCountDeltas(desired, live)
		// Indices 2 and 3 in obsolete MPG1 should be deleted.
		sort.Ints(dels)
		assert.Equal(t, []int{2, 3}, dels)
		assert.Empty(t, creates)
	})

	t.Run("excess deletion picks the highest indices, not arbitrary", func(t *testing.T) {
		// MPG0 holds {0, 5, 9}; desired count is 1. Should delete 9 and 5, keep 0.
		desired := map[string]int32{tcsMPG0: 1}
		live := map[int]string{0: tcsMPG0, 5: tcsMPG0, 9: tcsMPG0}
		dels, _ := computePCSGCountDeltas(desired, live)
		sort.Ints(dels)
		assert.Equal(t, []int{5, 9}, dels)
	})
}

func TestAssignFreeIndicesToPodGangs(t *testing.T) {
	t.Run("no creations returns nil", func(t *testing.T) {
		got := assignFreeIndicesToPodGangs(nil, map[string]int32{}, sets.New[int](), 4)
		assert.Nil(t, got)
	})

	t.Run("draws lowest free indices in alphabetical PodGang order", func(t *testing.T) {
		// kept = {0,1,2,3} (live); spec=5 → free=[4]. Only ScaledPG0 needs 1 → assign 4.
		creations := map[string]int{tcsScaledPG0: 1}
		desired := map[string]int32{tcsMPG0: 2, tcsMPG1: 2, tcsScaledPG0: 1}
		kept := sets.New[int](0, 1, 2, 3)
		got := assignFreeIndicesToPodGangs(creations, desired, kept, 5)
		assert.Equal(t, map[int]string{4: tcsScaledPG0}, got)
	})

	t.Run("Case 1 gap fill — fills the hole, not the tail", func(t *testing.T) {
		// Live held {0,1,2}; index 3 was deleted externally. Spec=4 → free=[3].
		// MPG1 short by 1.
		creations := map[string]int{tcsMPG1: 1}
		desired := map[string]int32{tcsMPG0: 2, tcsMPG1: 2}
		kept := sets.New[int](0, 1, 2)
		got := assignFreeIndicesToPodGangs(creations, desired, kept, 4)
		assert.Equal(t, map[int]string{3: tcsMPG1}, got)
	})

	t.Run("multiple creations across multiple PodGangs — alphabetical order, ascending indices", func(t *testing.T) {
		// Spec=4, kept={}. PodGangs visited alphabetically:
		// MPG0 (counter=0), MPG1 (counter=1), ScaledPG0. MPG0 wants 2 → indices 0,1; MPG1 wants 2 → indices 2,3.
		creations := map[string]int{tcsMPG0: 2, tcsMPG1: 2}
		desired := map[string]int32{tcsMPG0: 2, tcsMPG1: 2}
		kept := sets.New[int]()
		got := assignFreeIndicesToPodGangs(creations, desired, kept, 4)
		expect := map[int]string{0: tcsMPG0, 1: tcsMPG0, 2: tcsMPG1, 3: tcsMPG1}
		assert.Equal(t, expect, got)
	})

	t.Run("exhausts free pool early — partial assignment returned", func(t *testing.T) {
		// Spec=2, kept={0}. Free=[1]. Creations want 2; only index 1 is available.
		creations := map[string]int{tcsMPG0: 2}
		desired := map[string]int32{tcsMPG0: 2}
		kept := sets.New[int](0)
		got := assignFreeIndicesToPodGangs(creations, desired, kept, 2)
		assert.Equal(t, map[int]string{1: tcsMPG0}, got)
	})
}

// newSyncContextForMappingTests builds a syncContext usable by computeDesiredPCSGReplicaMapping
// and buildMappingFromPodGangMap. The PCS spec drives IsCoherentUpdateInProgress: when
// coherentUpdateInProgress=true, UpdateProgress is set with no UpdateEndedAt (mirrors the live
// definition in IsCoherentUpdateInProgress).
func newSyncContextForMappingTests(
	pcsgSpecReplicas int32,
	pcsgStatusMapping map[string]int32,
	pgmEntries []grovecorev1alpha1.PodGangEntry,
	coherentUpdateInProgress bool,
) *syncContext {
	pcsg := &grovecorev1alpha1.PodCliqueScalingGroup{
		ObjectMeta: metav1.ObjectMeta{Name: tcsPCSGFQN, Namespace: "default"},
		Spec:       grovecorev1alpha1.PodCliqueScalingGroupSpec{Replicas: pcsgSpecReplicas},
		Status:     grovecorev1alpha1.PodCliqueScalingGroupStatus{PodGangMapping: pcsgStatusMapping},
	}
	pcs := &grovecorev1alpha1.PodCliqueSet{
		ObjectMeta: metav1.ObjectMeta{Name: tcsPCSName, Namespace: "default"},
		Spec: grovecorev1alpha1.PodCliqueSetSpec{
			UpdateStrategy: &grovecorev1alpha1.PodCliqueSetUpdateStrategy{Type: grovecorev1alpha1.CoherentStrategy},
		},
		Status: grovecorev1alpha1.PodCliqueSetStatus{
			CurrentGenerationHash: ptr.To(tcsHash),
		},
	}
	if coherentUpdateInProgress {
		pcs.Status.UpdateProgress = &grovecorev1alpha1.PodCliqueSetUpdateProgress{}
	}
	pgm := &grovecorev1alpha1.PodGangMap{
		Spec: grovecorev1alpha1.PodGangMapSpec{Entries: pgmEntries},
	}
	return &syncContext{
		pcs:             pcs,
		pcsg:            pcsg,
		pcsReplicaIndex: tcsPCSReplica,
		podGangMap:      pgm,
	}
}

func TestBuildMappingFromPodGangMap(t *testing.T) {
	r := _resource{}

	t.Run("entries referencing this PCSG are picked up; others ignored", func(t *testing.T) {
		sc := newSyncContextForMappingTests(0, nil, []grovecorev1alpha1.PodGangEntry{
			{Name: tcsMPG0, PodCliqueScalingGroups: map[string]int32{tcsPCSGConfigName: 2}},
			{Name: "irrelevant-pg", PodCliqueScalingGroups: map[string]int32{"other-pcsg": 5}},
			{Name: tcsMPG1, PodCliqueScalingGroups: map[string]int32{tcsPCSGConfigName: 3}},
		}, false)
		got := r.buildMappingFromPodGangMap(sc)
		assert.Equal(t, map[string]int32{tcsMPG0: 2, tcsMPG1: 3}, got)
	})

	t.Run("zero-count entries are skipped", func(t *testing.T) {
		sc := newSyncContextForMappingTests(0, nil, []grovecorev1alpha1.PodGangEntry{
			{Name: tcsMPG0, PodCliqueScalingGroups: map[string]int32{tcsPCSGConfigName: 2}},
			{Name: tcsMPG1, PodCliqueScalingGroups: map[string]int32{tcsPCSGConfigName: 0}},
		}, false)
		got := r.buildMappingFromPodGangMap(sc)
		assert.Equal(t, map[string]int32{tcsMPG0: 2}, got)
	})

	t.Run("empty PGM yields empty mapping", func(t *testing.T) {
		sc := newSyncContextForMappingTests(0, nil, nil, false)
		got := r.buildMappingFromPodGangMap(sc)
		assert.Empty(t, got)
	})
}

func TestComputeDesiredPCSGReplicaMapping(t *testing.T) {
	r := _resource{}

	t.Run("coherent update in progress — overwrites from PGM regardless of status", func(t *testing.T) {
		// Status mapping says one thing; PGM says another. Coherent-update flow should pick PGM.
		sc := newSyncContextForMappingTests(
			4,
			map[string]int32{tcsMPG0: 100, tcsMPG1: 100}, // bogus status, should be ignored
			[]grovecorev1alpha1.PodGangEntry{
				{Name: tcsMPG0, PodCliqueScalingGroups: map[string]int32{tcsPCSGConfigName: 2}},
				{Name: tcsMPG1, PodCliqueScalingGroups: map[string]int32{tcsPCSGConfigName: 2}},
			},
			true, // coherent update in progress
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, map[string]int32{tcsMPG0: 2, tcsMPG1: 2}, got)
	})

	t.Run("fresh PCSG (empty status) — seeds from PGM", func(t *testing.T) {
		sc := newSyncContextForMappingTests(
			4,
			nil,
			[]grovecorev1alpha1.PodGangEntry{
				{Name: tcsMPG0, PodCliqueScalingGroups: map[string]int32{tcsPCSGConfigName: 2}},
				{Name: tcsMPG1, PodCliqueScalingGroups: map[string]int32{tcsPCSGConfigName: 2}},
			},
			false,
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, map[string]int32{tcsMPG0: 2, tcsMPG1: 2}, got)
	})

	t.Run("steady state, no drift — returns clone of status mapping", func(t *testing.T) {
		statusMapping := map[string]int32{tcsMPG0: 2, tcsMPG1: 2}
		sc := newSyncContextForMappingTests(4, statusMapping, nil, false)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, statusMapping, got)
		// Mutating the result should not affect the input — verifies clone.
		got[tcsMPG0] = 99
		assert.Equal(t, int32(2), statusMapping[tcsMPG0])
	})

	t.Run("scale-out — appends new Scaled-PG entries with count 1 each", func(t *testing.T) {
		// Spec=6, status sums to 4 → diff=+2 → mint 2 new Scaled-PGs.
		sc := newSyncContextForMappingTests(6, map[string]int32{tcsMPG0: 2, tcsMPG1: 2}, nil, false)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, int32(2), got[tcsMPG0])
		assert.Equal(t, int32(2), got[tcsMPG1])
		assert.Equal(t, int32(1), got[tcsScaledPG0])
		assert.Equal(t, int32(1), got[tcsScaledPG1])
		assert.Len(t, got, 4)
	})

	t.Run("scale-out preserves Scaled-PG counter continuity from existing entries", func(t *testing.T) {
		// Status already has ScaledPG0; one more mint should produce ScaledPG1 (counter 1).
		sc := newSyncContextForMappingTests(
			6,
			map[string]int32{tcsMPG0: 2, tcsMPG1: 2, tcsScaledPG0: 1},
			nil,
			false,
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, int32(1), got[tcsScaledPG0])
		assert.Equal(t, int32(1), got[tcsScaledPG1])
	})

	t.Run("scale-in — decrements via Tier 1/2/3 walk; BPG never touched", func(t *testing.T) {
		// Status sums to 6 (BPG=2, MPG0=2, LegacySPG0=1, ScaledPG0=1); spec=4 → diff=-2.
		// Tier 1 drains LegacySPG0; Tier 2 drains ScaledPG0; MPG0 and BPG untouched.
		sc := newSyncContextForMappingTests(
			4,
			map[string]int32{
				tcsBPGName:    2,
				tcsMPG0:       2,
				tcsLegacySPG0: 1,
				tcsScaledPG0:  1,
			},
			nil,
			false,
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, int32(2), got[tcsBPGName])
		assert.Equal(t, int32(2), got[tcsMPG0])
		assert.Equal(t, int32(0), got[tcsLegacySPG0])
		assert.Equal(t, int32(0), got[tcsScaledPG0])
	})

	t.Run("scale-in does not mutate the input status mapping (clone)", func(t *testing.T) {
		statusMapping := map[string]int32{tcsMPG0: 2, tcsMPG1: 2}
		sc := newSyncContextForMappingTests(3, statusMapping, nil, false)
		_, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		// Original status mapping unchanged.
		assert.Equal(t, int32(2), statusMapping[tcsMPG0])
		assert.Equal(t, int32(2), statusMapping[tcsMPG1])
	})
}
