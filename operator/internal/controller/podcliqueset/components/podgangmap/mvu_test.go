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
	"fmt"
	"testing"

	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComputeNextPodGangMapState_SingleStandalonePCLQUpdated tests a coherent update where only
// one standalone PCLQ is updated.
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {5 Frontend pods, 1 Prefill replica, 1 Decode replica}
//	SPGs (old hash): 3x {1 Prefill replica}, 2x {1 Decode replica}
//	Only Frontend has been updated. PCSGs are unchanged.
//
// MVU template: {Frontend: 2 pods}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2}. Old BPG reduced to {F:3, P:1, D:1}.
//	Iteration 2: MVU {F:3}. Absorbs 1F (3-2=1 < minAvail=2). Old BPG reduced to {F:0, P:1, D:1}.
//	             BPG removed (F=0, but P and D still present so NOT removed — only F is in template).
//	Iteration 3: Done. No Tail-PGs (no PCSGs in template).
func TestComputeNextPodGangMapState_SingleStandalonePCLQUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 5}, PodCliqueScalingGroups: map[string]int32{"prefill": 1, "decode": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	assert.Empty(t, state.newEntries[0].PodCliqueScalingGroups)
	require.Len(t, state.oldEntries, 1)
	assert.Equal(t, int32(3), state.oldEntries[0].PodCliques["frontend"])

	// Iteration 2: absorbs remaining 1F
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(3), state.newEntries[0].PodCliques["frontend"])
	// BPG still has P:1, D:1 so it's not removed
	require.Len(t, state.oldEntries, 1)
	assert.Equal(t, int32(0), state.oldEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), state.oldEntries[0].PodCliqueScalingGroups["prefill"])

	// Iteration 3: done (no PCSGs in template, remaining F=0)
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_MultipleStandalonePCLQsUpdated tests a coherent update where
// multiple standalone PCLQs are updated simultaneously.
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {5 Frontend pods, 3 Backend pods}
//	Only Frontend and Backend have been updated. PCSGs unchanged.
//
// MVU template: {Frontend: 2 pods, Backend: 1 pod}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2, B:1}. Old BPG: {F:3, B:2}.
//	Iteration 2: MVU {F:3, B:2}. Absorbs F=1, B=1. Old BPG removed (all zero).
//	Iteration 3: Done.
func TestComputeNextPodGangMapState_MultipleStandalonePCLQsUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2, "backend": 1},
		pcsgs:           map[string]int32{},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 5, "backend": 3}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliques["backend"])
	require.Len(t, state.oldEntries, 1)
	assert.Equal(t, int32(3), state.oldEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(2), state.oldEntries[0].PodCliques["backend"])

	// Iteration 2: absorbs remaining F=1, B=1
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(3), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["backend"])
	assert.Empty(t, state.oldEntries)

	// Iteration 3: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_SinglePCSGUpdated tests a coherent update where only one PCSG
// is updated (no standalone PCLQs changed).
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {5 Frontend pods, 1 Prefill replica}
//	SPGs (old hash): "spg-1" {P:1}, "spg-2" {P:1}, "spg-3" {P:1}
//	Only Prefill PCSG updated. Total old Prefill replicas: 4.
//
// MVU template: {Prefill: 1 replica}
//
// Expected: 4 MVU iterations {P:1} each, deducting from lowest-indexed entry first.
func TestComputeNextPodGangMapState_SinglePCSGUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 5}, PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-1", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-2", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-3", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1: takes from bpg-0 (lowest index)
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])
	// bpg-0 has P:0 but still has F:5, so it remains. SPGs still have P:1 each.
	require.Len(t, state.oldEntries, 4)

	// Iterations 2-4: each takes from next lowest SPG
	for i := range 3 {
		state = computeNextPodGangMapState(template, state.oldEntries, builder)
		require.False(t, state.done, "iteration %d should not be done", i+2)
		require.Len(t, state.newEntries, 1)
		assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])
	}

	// After 4 iterations: only bpg-0 with F:5 remains (no prefill left anywhere)
	require.Len(t, state.oldEntries, 1)
	assert.Equal(t, int32(5), state.oldEntries[0].PodCliques["frontend"])

	// Iteration 5: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_MultiplePCSGsUpdated tests a coherent update where multiple PCSGs
// are updated (no standalone PCLQs changed).
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {5 Frontend pods, 1 Prefill replica, 1 Decode replica}
//	SPGs (old hash): "spg-1" {P:1}, "spg-2" {P:1}, "spg-3" {P:1}, "spg-4" {D:1}, "spg-5" {D:1}
//	Both Prefill and Decode PCSGs updated.
//	Total: Prefill=4, Decode=3
//
// MVU template: {Prefill: 1 replica, Decode: 1 replica}
//
// Expected:
//
//	Iterations 1-3: MVU {P:1, D:1} each.
//	Iteration 4: Tail-PG {P:1}.
//	Iteration 5: Done.
func TestComputeNextPodGangMapState_MultiplePCSGsUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{},
		pcsgs:           map[string]int32{"prefill": 1, "decode": 1},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 5}, PodCliqueScalingGroups: map[string]int32{"prefill": 1, "decode": 1}},
		{Name: "spg-1", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-2", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-3", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-4", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"decode": 1}},
		{Name: "spg-5", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"decode": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iterations 1-3: MVU {P:1, D:1}
	state := podGangMapState{oldEntries: oldEntries}
	for i := range 3 {
		state = computeNextPodGangMapState(template, state.oldEntries, builder)
		require.False(t, state.done, "iteration %d should not be done", i+1)
		require.Len(t, state.newEntries, 1)
		assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])
		assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["decode"])
	}

	// Iteration 4: cannot form MVU (D=0 < 1). Tail-PG: {P:1}
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])

	// Iteration 5: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_SingleStandalonePCLQAndSinglePCSGUpdated tests a coherent update
// where one standalone PCLQ and one PCSG are both updated.
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {5 Frontend pods, 1 Prefill replica}
//	SPGs (old hash): "spg-1" {P:1}, "spg-2" {P:1}, "spg-3" {P:1}
//	Frontend (standalone) and Prefill (PCSG) updated.
//	Total: Frontend=5 pods, Prefill=4 replicas.
//
// MVU template: {Frontend: 2 pods, Prefill: 1 replica}
//
// Expected:
//
//	Iteration 1: MVU {F:2, P:1}. BPG: {F:3, P:0}.
//	Iteration 2: MVU {F:3, P:1}. Absorbs F=1. SPG-1 consumed.
//	Iteration 3: Tail-PGs: {P:1}, {P:1}.
//	Iteration 4: Done.
func TestComputeNextPodGangMapState_SingleStandalonePCLQAndSinglePCSGUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 5}, PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-1", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-2", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-3", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])

	// Iteration 2: absorbs remaining 1F
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(3), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])

	// Iteration 3: Tail-PGs
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 2)
	for _, e := range state.newEntries {
		assert.Equal(t, int32(1), e.PodCliqueScalingGroups["prefill"])
		assert.Empty(t, e.PodCliques)
	}

	// Iteration 4: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_SingleStandalonePCLQAndMultiplePCSGsUpdated tests a coherent
// update where one standalone PCLQ and multiple PCSGs are all updated.
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {5 Frontend pods, 1 Prefill replica, 1 Decode replica}
//	SPGs (old hash): "spg-1" {P:1}, "spg-2" {P:1}, "spg-3" {P:1}, "spg-4" {D:1}, "spg-5" {D:1}
//	Frontend, Prefill PCSG, and Decode PCSG all updated.
//	Total: Frontend=5, Prefill=4, Decode=3.
//
// MVU template: {Frontend: 2 pods, Prefill: 1 replica, Decode: 1 replica}
//
// Expected:
//
//	Iteration 1: MVU {F:2, P:1, D:1}.
//	Iteration 2: MVU {F:3, P:1, D:1}. Absorbs 1F.
//	Iteration 3: Tail-PGs: {P:1}, {P:1}, {D:1}.
//	Iteration 4: Done.
func TestComputeNextPodGangMapState_SingleStandalonePCLQAndMultiplePCSGsUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{"prefill": 1, "decode": 1},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 5}, PodCliqueScalingGroups: map[string]int32{"prefill": 1, "decode": 1}},
		{Name: "spg-1", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-2", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-3", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-4", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"decode": 1}},
		{Name: "spg-5", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"decode": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["decode"])

	// Iteration 2: absorbs remaining 1F
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(3), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["decode"])

	// Iteration 3: Tail-PGs
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 3)
	prefillTails := 0
	decodeTails := 0
	for _, e := range state.newEntries {
		if e.PodCliqueScalingGroups["prefill"] == 1 {
			prefillTails++
		}
		if e.PodCliqueScalingGroups["decode"] == 1 {
			decodeTails++
		}
	}
	assert.Equal(t, 2, prefillTails)
	assert.Equal(t, 1, decodeTails)

	// Iteration 4: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_MultipleStandalonePCLQsAndSinglePCSGUpdated tests a coherent
// update where multiple standalone PCLQs and one PCSG are updated.
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {5 Frontend pods, 3 Backend pods, 1 Prefill replica}
//	SPGs (old hash): "spg-1" {P:1}, "spg-2" {P:1}, "spg-3" {P:1}
//	Frontend, Backend (standalone), and Prefill PCSG all updated.
//	Total: Frontend=5, Backend=3, Prefill=4.
//
// MVU template: {Frontend: 2 pods, Backend: 1 pod, Prefill: 1 replica}
//
// Expected:
//
//	Iteration 1: MVU {F:2, B:1, P:1}.
//	Iteration 2: MVU {F:3, B:2, P:1}. Absorbs F=1, B=1.
//	Iteration 3: Tail-PGs: {P:1}, {P:1}.
//	Iteration 4: Done.
func TestComputeNextPodGangMapState_MultipleStandalonePCLQsAndSinglePCSGUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2, "backend": 1},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 5, "backend": 3}, PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-1", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-2", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-3", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliques["backend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])

	// Iteration 2: absorbs F=1, B=1
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(3), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["backend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])

	// Iteration 3: Tail-PGs
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 2)
	for _, e := range state.newEntries {
		assert.Equal(t, int32(1), e.PodCliqueScalingGroups["prefill"])
		assert.Empty(t, e.PodCliques)
	}

	// Iteration 4: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_MultipleStandalonePCLQsAndMultiplePCSGsUpdated tests a coherent
// update where multiple standalone PCLQs and multiple PCSGs are all updated.
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {5 Frontend, 3 Backend, 1 Prefill, 1 Decode}
//	SPGs (old hash): "spg-1" {P:1}, "spg-2" {P:1}, "spg-3" {P:1}, "spg-4" {D:1}, "spg-5" {D:1}
//	All components updated. Total: F=5, B=3, P=4, D=3.
//
// MVU template: {Frontend: 2, Backend: 1, Prefill: 1, Decode: 1}
//
// Expected:
//
//	Iteration 1: MVU {F:2, B:1, P:1, D:1}.
//	Iteration 2: MVU {F:3, B:2, P:1, D:1}. Absorbs F=1, B=1.
//	Iteration 3: Tail-PGs: {P:1}, {P:1}, {D:1}.
//	Iteration 4: Done.
func TestComputeNextPodGangMapState_MultipleStandalonePCLQsAndMultiplePCSGsUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2, "backend": 1},
		pcsgs:           map[string]int32{"prefill": 1, "decode": 1},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 5, "backend": 3}, PodCliqueScalingGroups: map[string]int32{"prefill": 1, "decode": 1}},
		{Name: "spg-1", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-2", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-3", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-4", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"decode": 1}},
		{Name: "spg-5", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"decode": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliques["backend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["decode"])

	// Iteration 2: absorbs F=1, B=1
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(3), state.newEntries[0].PodCliques["frontend"])
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["backend"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(1), state.newEntries[0].PodCliqueScalingGroups["decode"])

	// Iteration 3: Tail-PGs
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 3)
	prefillTails := 0
	decodeTails := 0
	for _, e := range state.newEntries {
		if e.PodCliqueScalingGroups["prefill"] == 1 {
			prefillTails++
		}
		if e.PodCliqueScalingGroups["decode"] == 1 {
			decodeTails++
		}
	}
	assert.Equal(t, 2, prefillTails)
	assert.Equal(t, 1, decodeTails)

	// Iteration 4: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_ExactMultipleOfMinAvailable tests a coherent update where
// pod count divides evenly by minAvailable. No absorption, no tail PGs.
//
// Starting state:
//
//	BPG "bpg-0" (old hash): {4 Frontend pods}
//
// MVU template: {Frontend: 2 pods}
//
// Expected:
//
//	Iteration 1: MVU {F:2}. Old BPG: {F:2}.
//	Iteration 2: MVU {F:2}. Old BPG removed.
//	Iteration 3: Done.
func TestComputeNextPodGangMapState_ExactMultipleOfMinAvailable(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "bpg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 4}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	require.Len(t, state.oldEntries, 1)
	assert.Equal(t, int32(2), state.oldEntries[0].PodCliques["frontend"])

	// Iteration 2
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	assert.Empty(t, state.oldEntries)

	// Iteration 3: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_NothingRemaining tests the edge case where the function is
// called with no old entries (update already completed).
func TestComputeNextPodGangMapState_NothingRemaining(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	state := computeNextPodGangMapState(template, nil, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_OnlyTailPGsFromStart tests the edge case where canFormMVU is
// false from the first call because standalone PCLQ pods are missing in old entries but
// PCSG replicas remain.
//
// Starting state:
//
//	"spg-1" (old hash): {P:1}
//	"spg-2" (old hash): {P:1}
//	No Frontend pods in old entries.
//
// MVU template: {Frontend: 2 pods, Prefill: 1 replica}
//
// Expected: immediately produces Tail-PGs.
func TestComputeNextPodGangMapState_OnlyTailPGsFromStart(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "spg-1", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
		{Name: "spg-2", PodCliqueSetGenerationHash: "old-hash", PodCliqueScalingGroups: map[string]int32{"prefill": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Cannot form MVU (F=0 < 2). Directly produces Tail-PGs.
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 2)
	for _, e := range state.newEntries {
		assert.Equal(t, int32(1), e.PodCliqueScalingGroups["prefill"])
		assert.Empty(t, e.PodCliques)
	}
	assert.Empty(t, state.oldEntries)

	// Next call: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// TestComputeNextPodGangMapState_DeductsFromLowestIndexFirst verifies that pods and replicas
// are always deducted from the lowest-indexed old entry first.
//
// Starting state:
//
//	"pg-0" (old hash): {F:1}
//	"pg-1" (old hash): {F:3}
//	"pg-2" (old hash): {F:1}
//
// MVU template: {Frontend: 2 pods}
// Total F=5. Iterations: MVU{F:2}, MVU{F:3}(absorbs 1), done.
//
// Expected:
//
//	Iteration 1: Takes F:1 from pg-0 and F:1 from pg-1. MVU {F:2}. pg-0 removed. Old: pg-1{F:2}, pg-2{F:1}.
//	Iteration 2: Takes F:2 from pg-1. Remaining F=1 < minAvail=2. Absorbs F:1 from pg-2.
//	             MVU {F:3}. pg-1 removed, pg-2 removed. Old: empty.
//	Iteration 3: Done.
func TestComputeNextPodGangMapState_DeductsFromLowestIndexFirst(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{},
	}
	oldEntries := []grovecorev1alpha1.PodGangEntry{
		{Name: "pg-0", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 1}},
		{Name: "pg-1", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 3}},
		{Name: "pg-2", PodCliqueSetGenerationHash: "old-hash", PodCliques: map[string]int32{"frontend": 1}},
	}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1: takes 1 from pg-0 (exhausts it) and 1 from pg-1
	state := computeNextPodGangMapState(template, oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(2), state.newEntries[0].PodCliques["frontend"])
	// pg-0 removed (F=0), pg-1 has F:2, pg-2 has F:1
	require.Len(t, state.oldEntries, 2)
	assert.Equal(t, "pg-1", state.oldEntries[0].Name)
	assert.Equal(t, int32(2), state.oldEntries[0].PodCliques["frontend"])
	assert.Equal(t, "pg-2", state.oldEntries[1].Name)
	assert.Equal(t, int32(1), state.oldEntries[1].PodCliques["frontend"])

	// Iteration 2: takes 2 from pg-1, remaining F=1 < 2. Absorbs 1 from pg-2.
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	require.False(t, state.done)
	require.Len(t, state.newEntries, 1)
	assert.Equal(t, int32(3), state.newEntries[0].PodCliques["frontend"])
	assert.Empty(t, state.oldEntries)

	// Iteration 3: done
	state = computeNextPodGangMapState(template, state.oldEntries, builder)
	assert.True(t, state.done)
	assert.Nil(t, state.newEntries)
}

// makeTestEntryBuilder creates a simple entryBuilder that produces entries with an incrementing
// name and the given composition.
func makeTestEntryBuilder(counter *int) componentutils.PodGangEntryBuilder {
	return func(standalonePCLQPods map[string]int32, pcsgReplicas map[string]int32) grovecorev1alpha1.PodGangEntry {
		name := fmt.Sprintf("mvu-%d", *counter)
		*counter++
		return grovecorev1alpha1.PodGangEntry{
			Name:                       name,
			PodCliqueSetGenerationHash: "new-hash",
			PodCliques:                 standalonePCLQPods,
			PodCliqueScalingGroups:     pcsgReplicas,
		}
	}
}
