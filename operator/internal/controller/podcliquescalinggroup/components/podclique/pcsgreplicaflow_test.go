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
	"strconv"
	"testing"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		mapping map[string][]int32
		expect  int
		wantErr bool
	}{
		{
			name:    "empty mapping returns 0",
			mapping: map[string][]int32{},
			expect:  0,
		},
		{
			name: "no Scaled-PG entries returns 0",
			mapping: map[string][]int32{
				tcsMPG0: {0, 1},
				tcsMPG1: {2, 3},
			},
			expect: 0,
		},
		{
			name: "single Scaled-PG returns next index",
			mapping: map[string][]int32{
				tcsMPG0:      {0, 1},
				tcsScaledPG0: {2},
			},
			expect: 1,
		},
		{
			name: "multiple Scaled-PGs returns max + 1",
			mapping: map[string][]int32{
				tcsScaledPG0: {0},
				tcsScaledPG1: {1},
			},
			expect: 2,
		},
		{
			name: "ignores legacy SPG entries (they don't share the new prefix)",
			mapping: map[string][]int32{
				tcsLegacySPG0: {0},
				tcsLegacySPG1: {1},
			},
			expect: 0,
		},
		{
			name: "ignores non-matching MPG/TailPG entries",
			mapping: map[string][]int32{
				tcsTailPG2:   {0},
				tcsScaledPG0: {1},
			},
			expect: 1,
		},
		{
			name: "errors on unparseable Scaled-PG name",
			mapping: map[string][]int32{
				tcsScaledPGPrefix + "notanumber": {0},
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
		currentMapping map[string][]int32
		count          int
		expectNames    []string
	}{
		{
			name:           "first mint starts at 0",
			currentMapping: map[string][]int32{},
			count:          2,
			expectNames:    []string{tcsScaledPG0, tcsScaledPG1},
		},
		{
			name: "subsequent mint continues from max+1",
			currentMapping: map[string][]int32{
				tcsScaledPG0: {0},
			},
			count:       1,
			expectNames: []string{tcsScaledPG1},
		},
		{
			name: "ignores legacy SPGs when computing next index",
			currentMapping: map[string][]int32{
				tcsLegacySPG0: {0},
				tcsLegacySPG1: {1},
			},
			count:       1,
			expectNames: []string{tcsScaledPG0},
		},
		{
			name:           "count zero returns empty slice",
			currentMapping: map[string][]int32{},
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
		name                string
		mapping             map[string][]int32
		expectTierLegacySPG []string
		expectTierNewScaled []string
		expectTierMPGTail   []string
	}{
		{
			name: "BPG excluded; new Scaled-PGs in tierNewScaled; MPGs/TailPGs in tierMPGTail",
			mapping: map[string][]int32{
				tcsBPGName:   {0, 1},
				tcsMPG0:      {2, 3},
				tcsTailPG2:   {4},
				tcsScaledPG0: {5},
			},
			expectTierLegacySPG: nil,
			expectTierNewScaled: []string{tcsScaledPG0},
			expectTierMPGTail:   []string{tcsMPG0, tcsTailPG2},
		},
		{
			name: "legacy SPGs and new Scaled-PGs partition into their respective tiers",
			mapping: map[string][]int32{
				tcsBPGName:    {0, 1},
				tcsLegacySPG0: {2},
				tcsLegacySPG1: {3},
				tcsScaledPG0:  {4},
			},
			expectTierLegacySPG: []string{tcsLegacySPG0, tcsLegacySPG1},
			expectTierNewScaled: []string{tcsScaledPG0},
			expectTierMPGTail:   nil,
		},
		{
			name:                "empty mapping returns empty tiers",
			mapping:             map[string][]int32{},
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
		mapping := map[string][]int32{tcsScaledPG0: {0}}
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 0, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, map[string][]int32{tcsScaledPG0: {0}}, mapping)
	})

	t.Run("tier 1 (legacy SPG) drained before tier 2 (new Scaled-PG)", func(t *testing.T) {
		// Mixed mapping post-Grove-upgrade: budget=1 should drain the highest-counter legacy SPG first.
		mapping := map[string][]int32{
			tcsLegacySPG0: {1},
			tcsLegacySPG1: {2},
			tcsScaledPG0:  {3},
		}
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		// Highest-counter legacy SPG (LegacySPG1) drained; LegacySPG0 and ScaledPG0 untouched.
		assert.Empty(t, mapping[tcsLegacySPG1])
		assert.Equal(t, []int32{1}, mapping[tcsLegacySPG0])
		assert.Equal(t, []int32{3}, mapping[tcsScaledPG0])
	})

	t.Run("tier 1 exhausted then tier 2 — Scaled-PGs by counter desc", func(t *testing.T) {
		mapping := map[string][]int32{
			tcsLegacySPG0: {1},
			tcsScaledPG0:  {2},
			tcsScaledPG1:  {3},
		}
		// Need 2: drain LegacySPG0, then highest-counter Scaled-PG (ScaledPG1).
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 2, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Empty(t, mapping[tcsLegacySPG0])
		assert.Empty(t, mapping[tcsScaledPG1])
		assert.Equal(t, []int32{2}, mapping[tcsScaledPG0])
	})

	t.Run("tier 2 only — Scaled-PGs drained highest counter first", func(t *testing.T) {
		mapping := map[string][]int32{
			tcsMPG0:      {0, 1},
			tcsScaledPG0: {2},
			tcsScaledPG1: {3},
		}
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Empty(t, mapping[tcsScaledPG1])
		assert.Equal(t, []int32{2}, mapping[tcsScaledPG0])
		assert.Equal(t, []int32{0, 1}, mapping[tcsMPG0])
	})

	t.Run("tiers 1 and 2 exhausted then tier 3 — TailPG before MPG (higher counter wins)", func(t *testing.T) {
		mapping := map[string][]int32{
			tcsMPG0:       {0, 1},
			tcsMPG1:       {2, 3},
			tcsTailPG2:    {4},
			tcsScaledPG0:  {5},
			tcsLegacySPG0: {6},
		}
		// Need 3: drain LegacySPG0 (tier 1), drain ScaledPG0 (tier 2), drain TailPG2 (tier 3 highest).
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 3, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Empty(t, mapping[tcsLegacySPG0])
		assert.Empty(t, mapping[tcsScaledPG0])
		assert.Empty(t, mapping[tcsTailPG2])
		assert.Equal(t, []int32{0, 1}, mapping[tcsMPG0])
		assert.Equal(t, []int32{2, 3}, mapping[tcsMPG1])
	})

	t.Run("MPG with multiple indices absorbs multiple pops", func(t *testing.T) {
		mapping := map[string][]int32{
			tcsMPG0: {0, 1},
			tcsMPG1: {2, 3, 4},
		}
		// Tier 3 only. MPG1 has higher counter → pop its highest index twice (4, then 3).
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 2, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, []int32{0, 1}, mapping[tcsMPG0])
		assert.Equal(t, []int32{2}, mapping[tcsMPG1])
	})

	t.Run("BPG never decremented even when budget exhausts every other tier", func(t *testing.T) {
		mapping := map[string][]int32{
			tcsBPGName:    {0, 1},
			tcsLegacySPG0: {2},
		}
		// Budget=1: tier 1 has LegacySPG0; that drains. BPG must remain at [0,1].
		require.NoError(t, decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName))
		assert.Equal(t, []int32{0, 1}, mapping[tcsBPGName])
		assert.Empty(t, mapping[tcsLegacySPG0])
	})

	t.Run("returns error when tier 1 has unparseable name", func(t *testing.T) {
		mapping := map[string][]int32{
			tcsLegacySPGPrefix + "bogus": {0},
		}
		err := decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName)
		require.Error(t, err)
	})

	t.Run("returns error when tier 2 has unparseable name", func(t *testing.T) {
		mapping := map[string][]int32{
			tcsScaledPGPrefix + "bogus": {0},
		}
		err := decrementPCSGMappingForScaleIn(mapping, 1, tcsScaledPGPrefix, tcsLegacySPGPrefix, tcsBPGName)
		require.Error(t, err)
	})
}

// newSyncContextForMappingTests builds a syncContext usable by computeDesiredPCSGReplicaMapping
// and buildMappingFromPodGangMap. The PCS spec drives IsCoherentUpdateInProgress: when
// coherentUpdateInProgress=true, UpdateProgress is set with no UpdateEndedAt (mirrors the live
// definition in IsCoherentUpdateInProgress).
func newSyncContextForMappingTests(
	pcsgSpecReplicas int32,
	pcsgStatusMapping map[string][]int32,
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
			{Name: tcsMPG0, PCSGReplicaIndices: map[string][]int32{tcsPCSGConfigName: {0, 1}}},
			{Name: "irrelevant-pg", PCSGReplicaIndices: map[string][]int32{"other-pcsg": {0, 1, 2, 3, 4}}},
			{Name: tcsMPG1, PCSGReplicaIndices: map[string][]int32{tcsPCSGConfigName: {2, 3, 4}}},
		}, false)
		got := r.buildMappingFromPodGangMap(sc)
		assert.Equal(t, map[string][]int32{tcsMPG0: {0, 1}, tcsMPG1: {2, 3, 4}}, got)
	})

	t.Run("entries with empty index slices are skipped", func(t *testing.T) {
		sc := newSyncContextForMappingTests(0, nil, []grovecorev1alpha1.PodGangEntry{
			{Name: tcsMPG0, PCSGReplicaIndices: map[string][]int32{tcsPCSGConfigName: {0, 1}}},
			{Name: tcsMPG1, PCSGReplicaIndices: map[string][]int32{tcsPCSGConfigName: {}}},
		}, false)
		got := r.buildMappingFromPodGangMap(sc)
		assert.Equal(t, map[string][]int32{tcsMPG0: {0, 1}}, got)
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
			map[string][]int32{tcsMPG0: {99}, tcsMPG1: {98}}, // bogus status, should be ignored
			[]grovecorev1alpha1.PodGangEntry{
				{Name: tcsMPG0, PCSGReplicaIndices: map[string][]int32{tcsPCSGConfigName: {0, 1}}},
				{Name: tcsMPG1, PCSGReplicaIndices: map[string][]int32{tcsPCSGConfigName: {2, 3}}},
			},
			true, // coherent update in progress
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, map[string][]int32{tcsMPG0: {0, 1}, tcsMPG1: {2, 3}}, got)
	})

	t.Run("fresh PCSG (empty status) — seeds from PGM", func(t *testing.T) {
		sc := newSyncContextForMappingTests(
			4,
			nil,
			[]grovecorev1alpha1.PodGangEntry{
				{Name: tcsMPG0, PCSGReplicaIndices: map[string][]int32{tcsPCSGConfigName: {0, 1}}},
				{Name: tcsMPG1, PCSGReplicaIndices: map[string][]int32{tcsPCSGConfigName: {2, 3}}},
			},
			false,
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, map[string][]int32{tcsMPG0: {0, 1}, tcsMPG1: {2, 3}}, got)
	})

	t.Run("steady state, no drift — returns clone of status mapping", func(t *testing.T) {
		statusMapping := map[string][]int32{tcsMPG0: {0, 1}, tcsMPG1: {2, 3}}
		sc := newSyncContextForMappingTests(4, statusMapping, nil, false)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, statusMapping, got)
		// Mutating the result should not affect the input — verifies clone.
		got[tcsMPG0] = append(got[tcsMPG0], 99)
		assert.Equal(t, []int32{0, 1}, statusMapping[tcsMPG0])
	})

	t.Run("scale-out — mints one Scaled-PG per new replica with the smallest free indices", func(t *testing.T) {
		// Spec=6, status sums to 4 (MPG0:[0,1], MPG1:[2,3]) → diff=+2 → mint 2 new Scaled-PGs
		// claiming the smallest free indices (4 and 5).
		sc := newSyncContextForMappingTests(6, map[string][]int32{tcsMPG0: {0, 1}, tcsMPG1: {2, 3}}, nil, false)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, []int32{0, 1}, got[tcsMPG0])
		assert.Equal(t, []int32{2, 3}, got[tcsMPG1])
		assert.Equal(t, []int32{4}, got[tcsScaledPG0])
		assert.Equal(t, []int32{5}, got[tcsScaledPG1])
		assert.Len(t, got, 4)
	})

	t.Run("scale-out preserves Scaled-PG counter continuity from existing entries", func(t *testing.T) {
		// Status already has ScaledPG0 holding index 4; one more mint should produce ScaledPG1
		// (next name index) holding free replica index 5.
		sc := newSyncContextForMappingTests(
			6,
			map[string][]int32{tcsMPG0: {0, 1}, tcsMPG1: {2, 3}, tcsScaledPG0: {4}},
			nil,
			false,
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, []int32{4}, got[tcsScaledPG0])
		assert.Equal(t, []int32{5}, got[tcsScaledPG1])
	})

	t.Run("scale-in — pops via Tier 1/2/3 walk; BPG never touched; emptied entries pruned", func(t *testing.T) {
		// Status sums to 6 (BPG=[0,1], MPG0=[2,3], LegacySPG0=[4], ScaledPG0=[5]); spec=4 → diff=-2.
		// Tier 1 drains LegacySPG0; Tier 2 drains ScaledPG0. Both empty entries are pruned
		// from the output so subsequent scale-out can reuse the freed replica indices.
		// MPG0 and BPG retain their indices.
		sc := newSyncContextForMappingTests(
			4,
			map[string][]int32{
				tcsBPGName:    {0, 1},
				tcsMPG0:       {2, 3},
				tcsLegacySPG0: {4},
				tcsScaledPG0:  {5},
			},
			nil,
			false,
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, map[string][]int32{tcsBPGName: {0, 1}, tcsMPG0: {2, 3}}, got)
	})

	t.Run("scale-out after scale-in reuses the freed replica index", func(t *testing.T) {
		// Reproduces the bug fixed by the prune step:
		//   1. Initial scale-out minted ScaledPG-0 holding replica index 0.
		//   2. Scale-in popped that index; without pruning, the empty ScaledPG-0 entry would survive.
		//   3. Scale-out again should reuse replica index 0 under a fresh ScaledPG-0 mint.
		// The test simulates the post-step-2 state and confirms the next scale-out picks index 0.
		sc := newSyncContextForMappingTests(
			1,
			// Status mapping after scale-in: only the anchor MPG remains with an empty slice;
			// previous Scaled-PG is gone. Spec.Replicas grows from 0 to 1 → diff=+1.
			map[string][]int32{tcsMPG0: {}},
			nil,
			false,
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		// MPG0 was empty and gets pruned. The next Scaled-PG mint reuses replica index 0.
		assert.Equal(t, map[string][]int32{tcsScaledPG0: {0}}, got)
	})

	t.Run("orphan empty-slice entries are pruned", func(t *testing.T) {
		// A pre-existing empty-slice entry sitting in PCSG.Status.PodGangMapping must be removed
		// even when no scale-in/scale-out runs in this reconcile. Otherwise nextScaledPodGangIndex
		// would treat the orphan's trailing index as occupied.
		sc := newSyncContextForMappingTests(
			2,
			map[string][]int32{tcsMPG0: {0, 1}, tcsScaledPG1: {}}, // ScaledPG1 is the orphan
			nil,
			false,
		)
		got, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		assert.Equal(t, map[string][]int32{tcsMPG0: {0, 1}}, got)
	})

	t.Run("scale-in does not mutate the input status mapping (clone)", func(t *testing.T) {
		statusMapping := map[string][]int32{tcsMPG0: {0, 1}, tcsMPG1: {2, 3}}
		sc := newSyncContextForMappingTests(3, statusMapping, nil, false)
		_, err := r.computeDesiredPCSGReplicaMapping(sc)
		require.NoError(t, err)
		// Original status mapping unchanged.
		assert.Equal(t, []int32{0, 1}, statusMapping[tcsMPG0])
		assert.Equal(t, []int32{2, 3}, statusMapping[tcsMPG1])
	})
}

func TestComputePCSGCountDeltas(t *testing.T) {
	// Test PCSG has two cliques. The "covered" check requires both cliques present at an index.
	cliqueNames := []string{"pcb", "pcc"}

	t.Run("no desired and no live PCLQs is a no-op", func(t *testing.T) {
		dels, creates, err := computePCSGCountDeltas(map[int]string{}, nil, cliqueNames)
		require.NoError(t, err)
		assert.Empty(t, dels)
		assert.Empty(t, creates)
	})

	t.Run("desired is empty: all live indices are obsolete and flagged for deletion", func(t *testing.T) {
		// 2 replicas live under tcsMPG0; desired is empty (e.g. PCSG was deleted in spec).
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG0, false),
			pcsgPCLQ("pcc", 0, tcsMPG0, false),
			pcsgPCLQ("pcb", 1, tcsMPG0, false),
			pcsgPCLQ("pcc", 1, tcsMPG0, false),
		}
		dels, creates, err := computePCSGCountDeltas(map[int]string{}, live, cliqueNames)
		require.NoError(t, err)
		sort.Ints(dels)
		assert.Equal(t, []int{0, 1}, dels)
		assert.Empty(t, creates)
	})

	t.Run("fully populated steady state: no deltas", func(t *testing.T) {
		desired := map[int]string{0: tcsMPG0, 1: tcsMPG1}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG0, false),
			pcsgPCLQ("pcc", 0, tcsMPG0, false),
			pcsgPCLQ("pcb", 1, tcsMPG1, false),
			pcsgPCLQ("pcc", 1, tcsMPG1, false),
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		assert.Empty(t, dels)
		assert.Empty(t, creates)
	})

	t.Run("missing replica entirely: index stays in creations, no deletion", func(t *testing.T) {
		// Index 1 is desired but no live PCLQs at index 1.
		desired := map[int]string{0: tcsMPG0, 1: tcsMPG1}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG0, false),
			pcsgPCLQ("pcc", 0, tcsMPG0, false),
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		assert.Empty(t, dels)
		assert.Equal(t, map[int]string{1: tcsMPG1}, creates)
	})

	t.Run("wrong PodGang label: index flagged for deletion AND stays in creations", func(t *testing.T) {
		// Live label at index 0 says tcsMPG1, but desired says tcsMPG0. The whole replica
		// gets deleted; the next reconcile will recreate under the correct PodGang.
		desired := map[int]string{0: tcsMPG0}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG1, false),
			pcsgPCLQ("pcc", 0, tcsMPG1, false),
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		assert.Equal(t, []int{0}, dels)
		assert.Equal(t, map[int]string{0: tcsMPG0}, creates)
	})

	t.Run("obsolete index (live but not in desired): flagged for deletion, no creation", func(t *testing.T) {
		desired := map[int]string{0: tcsMPG0}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG0, false),
			pcsgPCLQ("pcc", 0, tcsMPG0, false),
			pcsgPCLQ("pcb", 5, tcsMPG1, false),
			pcsgPCLQ("pcc", 5, tcsMPG1, false),
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		assert.Equal(t, []int{5}, dels)
		assert.Empty(t, creates)
	})

	t.Run("half-populated replica (one clique missing): stays in creations, NOT deleted", func(t *testing.T) {
		// Index 0 has only pcc; pcb is missing entirely. The lone live PCLQ has the correct
		// PodGang label. Without the per-clique presence check, this would be wrongly flagged
		// as "covered" and the missing pcb would never be created.
		desired := map[int]string{0: tcsMPG0}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcc", 0, tcsMPG0, false),
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		assert.Empty(t, dels, "the existing pcc must not be deleted")
		assert.Equal(t, map[int]string{0: tcsMPG0}, creates, "pcb must be re-emitted for creation")
	})

	t.Run("terminating PCLQs are ignored (whole replica terminating): index stays in creations", func(t *testing.T) {
		// Both PCLQs at index 0 are terminating with the OLD PodGang label. desiredIndexToPG
		// says the index should belong to tcsMPG0 now. Without the terminating filter, the
		// stale OLD label would mark the index as live-with-wrong-label, but no actual delete
		// would happen (already terminating) and creations would be retained — but the whole
		// replica is in flux. The right behavior: ignore the terminators entirely so the
		// covered check sees an empty index → emit creations, no spurious deletes.
		desired := map[int]string{0: tcsMPG0}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG1, true), // terminating, old label
			pcsgPCLQ("pcc", 0, tcsMPG1, true),
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		assert.Empty(t, dels, "terminating PCLQs must not be re-flagged for deletion")
		assert.Equal(t, map[int]string{0: tcsMPG0}, creates)
	})

	t.Run("one clique terminating, sibling live with correct label: still treated as half-populated", func(t *testing.T) {
		// pcb is terminating (old hash, finalizer slow); pcc is fresh with the correct new
		// PodGang label. The terminator is ignored, so liveCliquesByIndex[0] = {pcc}, which
		// fails the equality check against {pcb, pcc} → creations[0] retained. Once pcb
		// finishes terminating, the next reconcile creates a new pcb under tcsMPG0; pcc's
		// re-create attempt hits AlreadyExists and is swallowed.
		desired := map[int]string{0: tcsMPG0}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG1, true),  // terminating, old label
			pcsgPCLQ("pcc", 0, tcsMPG0, false), // fresh, new label
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		assert.Empty(t, dels, "the live pcc must not be deleted")
		assert.Equal(t, map[int]string{0: tcsMPG0}, creates, "pcb must be re-emitted for creation")
	})

	t.Run("all cliques present, one terminating with the correct label: still half-populated", func(t *testing.T) {
		// Edge case: pcb is terminating with the desired PodGang label (e.g. it was deleted
		// externally during a stable period). The terminating filter excludes it, so the
		// index looks half-populated and pcb gets re-emitted. doCreate will swallow
		// AlreadyExists if the terminator hasn't finalized yet, retrying next reconcile.
		desired := map[int]string{0: tcsMPG0}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG0, true), // terminating, correct label
			pcsgPCLQ("pcc", 0, tcsMPG0, false),
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		assert.Empty(t, dels)
		assert.Equal(t, map[int]string{0: tcsMPG0}, creates)
	})

	t.Run("multiple indices with mixed states", func(t *testing.T) {
		// idx 0: fully covered under tcsMPG0 → no delta.
		// idx 1: half-populated under tcsMPG0 → stays in creations.
		// idx 2: wrong label (live=tcsMPG1, desired=tcsMPG0) → delete + retain creations.
		// idx 3: not live at all → stays in creations.
		// idx 7: obsolete (live but not desired) → delete only.
		desired := map[int]string{
			0: tcsMPG0,
			1: tcsMPG0,
			2: tcsMPG0,
			3: tcsMPG1,
		}
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG0, false),
			pcsgPCLQ("pcc", 0, tcsMPG0, false),
			pcsgPCLQ("pcc", 1, tcsMPG0, false), // half-populated
			pcsgPCLQ("pcb", 2, tcsMPG1, false), // wrong label
			pcsgPCLQ("pcc", 2, tcsMPG1, false),
			pcsgPCLQ("pcb", 7, tcsMPG0, false), // obsolete (idx 7 not desired)
			pcsgPCLQ("pcc", 7, tcsMPG0, false),
		}
		dels, creates, err := computePCSGCountDeltas(desired, live, cliqueNames)
		require.NoError(t, err)
		sort.Ints(dels)
		assert.Equal(t, []int{2, 7}, dels)
		assert.Equal(t, map[int]string{1: tcsMPG0, 2: tcsMPG0, 3: tcsMPG1}, creates)
	})

	// --- Error paths ---

	t.Run("error on PCLQ missing PodGang label", func(t *testing.T) {
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG0, false),
			pcsgPCLQWithoutLabels("pcc", 0, apicommon.LabelPodGang), // strip podgang label
		}
		_, _, err := computePCSGCountDeltas(map[int]string{0: tcsMPG0}, live, cliqueNames)
		require.Error(t, err)
		assert.Contains(t, err.Error(), apicommon.LabelPodGang)
	})

	t.Run("error on PCLQ missing replica-index label", func(t *testing.T) {
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQWithoutLabels("pcb", 0, apicommon.LabelPodCliqueScalingGroupReplicaIndex),
		}
		_, _, err := computePCSGCountDeltas(map[int]string{0: tcsMPG0}, live, cliqueNames)
		require.Error(t, err)
		assert.Contains(t, err.Error(), apicommon.LabelPodCliqueScalingGroupReplicaIndex)
	})

	t.Run("error on divergent PodGang labels at the same index", func(t *testing.T) {
		// Contract violation: two cliques at one PCSG replica must share the same PodGang label.
		live := []grovecorev1alpha1.PodClique{
			pcsgPCLQ("pcb", 0, tcsMPG0, false),
			pcsgPCLQ("pcc", 0, tcsMPG1, false),
		}
		_, _, err := computePCSGCountDeltas(map[int]string{0: tcsMPG0}, live, cliqueNames)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "divergent")
	})
}

// pcsgPCLQ constructs a non-/terminating PCSG-owned PodClique fixture with the labels the
// production code requires. cliqueName is unqualified (e.g. "pcb"), pcsgReplicaIndex is the
// PCSG replica the PCLQ belongs to, and podGangName is the value of the LabelPodGang label.
// terminating=true sets a non-nil DeletionTimestamp so IsResourceTerminating returns true.
func pcsgPCLQ(cliqueName string, pcsgReplicaIndex int, podGangName string, terminating bool) grovecorev1alpha1.PodClique {
	pclq := grovecorev1alpha1.PodClique{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tcsPCSGFQN + "-" + strconv.Itoa(pcsgReplicaIndex) + "-" + cliqueName,
			Namespace: "default",
			Labels: map[string]string{
				apicommon.LabelPodCliqueScalingGroup:             tcsPCSGFQN,
				apicommon.LabelPodCliqueScalingGroupReplicaIndex: strconv.Itoa(pcsgReplicaIndex),
				apicommon.LabelPodGang:                           podGangName,
			},
		},
	}
	if terminating {
		now := metav1.Now()
		pclq.DeletionTimestamp = &now
		pclq.Finalizers = []string{"grove.io/test"}
	}
	return pclq
}

// pcsgPCLQWithoutLabels constructs a PCLQ fixture where the named label is missing. Used by
// error-path tests that exercise the contract-violation branches of computePCSGCountDeltas.
func pcsgPCLQWithoutLabels(cliqueName string, pcsgReplicaIndex int, missingLabel string) grovecorev1alpha1.PodClique {
	pclq := pcsgPCLQ(cliqueName, pcsgReplicaIndex, tcsMPG0, false)
	delete(pclq.Labels, missingLabel)
	return pclq
}
