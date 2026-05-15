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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComputeNextIteration_SingleStandalonePCLQUpdated tests a coherent update where only
// one standalone PCLQ is updated.
//
// Starting state:
//
//	BPG (old hash): {5 Frontend pods, 1 Prefill replica, 1 Decode replica}
//	SPGs (old hash): 3x {1 Prefill replica}, 2x {1 Decode replica}
//	Only Frontend has been updated. PCSGs are unchanged.
//	Remaining old Frontend pods to replace: 5
//
// MVU template: {Frontend: 2 pods}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2}. Remaining: F=3.
//	Iteration 2: MVU {F:3}. Absorbs 1F (3-2=1 < minAvail=2). Remaining: F=0.
//	Iteration 3: Done. No Tail-PGs (no PCSGs in template).
func TestComputeNextIteration_SingleStandalonePCLQUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{},
	}
	remainingOldPods := map[string]int32{"frontend": 5}
	remainingOldReplicas := map[string]int32{}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])
	assert.Empty(t, entries[0].PodCliqueScalingGroups)
	assert.Equal(t, int32(3), remainingOldPods["frontend"])

	// Iteration 2: absorbs remaining 1F
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(3), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(0), remainingOldPods["frontend"])

	// Iteration 3: done
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_MultipleStandalonePCLQsUpdated tests a coherent update where
// multiple standalone PCLQs are updated simultaneously.
//
// Starting state:
//
//	BPG (old hash): {5 Frontend pods, 3 Backend pods, 1 Prefill replica, 1 Decode replica}
//	Only Frontend and Backend have been updated. PCSGs unchanged.
//	Remaining: Frontend=5, Backend=3
//
// MVU template: {Frontend: 2 pods, Backend: 1 pod}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2, B:1}. Remaining: F=3, B=2.
//	Iteration 2: MVU {F:2, B:1}. Remaining: F=1, B=1.
//	             Cannot form another (F=1 < 2). Absorb: F+=1, B+=1. MVU becomes {F:3, B:2}.
//	             Remaining: F=0, B=0.
//	Iteration 3: Done.
func TestComputeNextIteration_MultipleStandalonePCLQsUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2, "backend": 1},
		pcsgs:           map[string]int32{},
	}
	remainingOldPods := map[string]int32{"frontend": 5, "backend": 3}
	remainingOldReplicas := map[string]int32{}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), entries[0].PodCliques["backend"])
	assert.Equal(t, int32(3), remainingOldPods["frontend"])
	assert.Equal(t, int32(2), remainingOldPods["backend"])

	// Iteration 2: absorbs remaining F=1, B=1
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(3), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(2), entries[0].PodCliques["backend"])
	assert.Equal(t, int32(0), remainingOldPods["frontend"])
	assert.Equal(t, int32(0), remainingOldPods["backend"])

	// Iteration 3: done
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_SinglePCSGUpdated tests a coherent update where only one PCSG
// is updated (no standalone PCLQs changed).
//
// Starting state:
//
//	BPG (old hash): {5 Frontend pods, 1 Prefill replica, 1 Decode replica}
//	SPGs (old hash): 3x {1 Prefill replica}
//	Only Prefill PCSG updated. Total old Prefill replicas: 4.
//
// MVU template: {Prefill: 1 replica}
//
// Expected iterations:
//
//	Iterations 1-4: MVU {P:1} each. Remaining after each: P=3, P=2, P=1, P=0.
//	Iteration 5: Done.
func TestComputeNextIteration_SinglePCSGUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	remainingOldPods := map[string]int32{}
	remainingOldReplicas := map[string]int32{"prefill": 4}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	for i := range 4 {
		entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
		require.False(t, done, "iteration %d should not be done", i+1)
		require.Len(t, entries, 1)
		assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
		assert.Empty(t, entries[0].PodCliques)
	}
	assert.Equal(t, int32(0), remainingOldReplicas["prefill"])

	// Done
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_MultiplePCSGsUpdated tests a coherent update where multiple PCSGs
// are updated (no standalone PCLQs changed).
//
// Starting state:
//
//	BPG (old hash): {5 Frontend pods, 1 Prefill replica, 1 Decode replica}
//	SPGs (old hash): 3x {1 Prefill replica}, 2x {1 Decode replica}
//	Both Prefill and Decode PCSGs updated.
//	Remaining: Prefill=4, Decode=3
//
// MVU template: {Prefill: 1 replica, Decode: 1 replica}
//
// Expected iterations:
//
//	Iterations 1-3: MVU {P:1, D:1} each.
//	Iteration 4: Cannot form MVU (D=0 < 1). Tail-PG: {P:1}.
//	Iteration 5: Done.
func TestComputeNextIteration_MultiplePCSGsUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{},
		pcsgs:           map[string]int32{"prefill": 1, "decode": 1},
	}
	remainingOldPods := map[string]int32{}
	remainingOldReplicas := map[string]int32{"prefill": 4, "decode": 3}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	for i := range 3 {
		entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
		require.False(t, done, "iteration %d should not be done", i+1)
		require.Len(t, entries, 1)
		assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
		assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["decode"])
	}
	assert.Equal(t, int32(1), remainingOldReplicas["prefill"])
	assert.Equal(t, int32(0), remainingOldReplicas["decode"])

	// Iteration 4: Tail-PG
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])

	// Iteration 5: done
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_SingleStandalonePCLQAndSinglePCSGUpdated tests a coherent update
// where one standalone PCLQ and one PCSG are both updated.
//
// Starting state:
//
//	BPG (old hash): {5 Frontend pods, 1 Prefill replica}
//	SPGs (old hash): 3x {1 Prefill replica}
//	Both Frontend (standalone) and Prefill (PCSG) updated.
//	Remaining: Frontend=5 pods, Prefill=4 replicas.
//
// MVU template: {Frontend: 2 pods, Prefill: 1 replica}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2, P:1}. Remaining: F=3, P=3.
//	Iteration 2: MVU {F:2, P:1}. Remaining: F=1, P=2.
//	             Cannot form another (F=1 < 2). Absorb F: MVU becomes {F:3, P:1}.
//	             Remaining: F=0, P=2.
//	Iteration 3: Cannot form MVU (F=0 < 2). Tail-PGs: {P:1}, {P:1}.
//	Iteration 4: Done.
func TestComputeNextIteration_SingleStandalonePCLQAndSinglePCSGUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	remainingOldPods := map[string]int32{"frontend": 5}
	remainingOldReplicas := map[string]int32{"prefill": 4}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(3), remainingOldPods["frontend"])
	assert.Equal(t, int32(3), remainingOldReplicas["prefill"])

	// Iteration 2: absorbs remaining 1F
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(3), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(0), remainingOldPods["frontend"])
	assert.Equal(t, int32(2), remainingOldReplicas["prefill"])

	// Iteration 3: Tail-PGs
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 2)
	for _, e := range entries {
		assert.Equal(t, int32(1), e.PodCliqueScalingGroups["prefill"])
		assert.Empty(t, e.PodCliques)
	}

	// Iteration 4: done
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_SingleStandalonePCLQAndMultiplePCSGsUpdated tests a coherent update
// where one standalone PCLQ and multiple PCSGs are all updated.
//
// Starting state:
//
//	BPG (old hash): {5 Frontend pods, 1 Prefill replica, 1 Decode replica}
//	SPGs (old hash): 3x {1 Prefill replica}, 2x {1 Decode replica}
//	Frontend, Prefill PCSG, and Decode PCSG all updated.
//	Remaining: Frontend=5 pods, Prefill=4 replicas, Decode=3 replicas.
//
// MVU template: {Frontend: 2 pods, Prefill: 1 replica, Decode: 1 replica}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2, P:1, D:1}. Remaining: F=3, P=3, D=2.
//	Iteration 2: MVU {F:3, P:1, D:1}. Absorbs 1F. Remaining: F=0, P=2, D=1.
//	Iteration 3: Tail-PGs: {P:1}, {P:1}, {D:1}. Remaining: all zero.
//	Iteration 4: Done.
func TestComputeNextIteration_SingleStandalonePCLQAndMultiplePCSGsUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{"prefill": 1, "decode": 1},
	}
	remainingOldPods := map[string]int32{"frontend": 5}
	remainingOldReplicas := map[string]int32{"prefill": 4, "decode": 3}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["decode"])

	// Iteration 2: absorbs remaining 1F
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(3), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["decode"])
	assert.Equal(t, int32(0), remainingOldPods["frontend"])
	assert.Equal(t, int32(2), remainingOldReplicas["prefill"])
	assert.Equal(t, int32(1), remainingOldReplicas["decode"])

	// Iteration 3: Tail-PGs
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 3)
	prefillTails := 0
	decodeTails := 0
	for _, e := range entries {
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
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_MultipleStandalonePCLQsAndSinglePCSGUpdated tests a coherent update
// where multiple standalone PCLQs and one PCSG are updated.
//
// Starting state:
//
//	BPG (old hash): {5 Frontend pods, 3 Backend pods, 1 Prefill replica}
//	SPGs (old hash): 3x {1 Prefill replica}
//	Frontend, Backend (standalone), and Prefill PCSG all updated.
//	Remaining: Frontend=5 pods, Backend=3 pods, Prefill=4 replicas.
//
// MVU template: {Frontend: 2 pods, Backend: 1 pod, Prefill: 1 replica}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2, B:1, P:1}. Remaining: F=3, B=2, P=3.
//	Iteration 2: MVU {F:2, B:1, P:1}. Remaining: F=1, B=1, P=2.
//	             Cannot form another (F=1 < 2). Absorb: MVU becomes {F:3, B:2, P:1}.
//	             Remaining: F=0, B=0, P=2.
//	Iteration 3: Tail-PGs: {P:1}, {P:1}. Remaining: all zero.
//	Iteration 4: Done.
func TestComputeNextIteration_MultipleStandalonePCLQsAndSinglePCSGUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2, "backend": 1},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	remainingOldPods := map[string]int32{"frontend": 5, "backend": 3}
	remainingOldReplicas := map[string]int32{"prefill": 4}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), entries[0].PodCliques["backend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(3), remainingOldPods["frontend"])
	assert.Equal(t, int32(2), remainingOldPods["backend"])
	assert.Equal(t, int32(3), remainingOldReplicas["prefill"])

	// Iteration 2: absorbs remaining F=1, B=1
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(3), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(2), entries[0].PodCliques["backend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(0), remainingOldPods["frontend"])
	assert.Equal(t, int32(0), remainingOldPods["backend"])
	assert.Equal(t, int32(2), remainingOldReplicas["prefill"])

	// Iteration 3: Tail-PGs
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 2)
	for _, e := range entries {
		assert.Equal(t, int32(1), e.PodCliqueScalingGroups["prefill"])
		assert.Empty(t, e.PodCliques)
	}

	// Iteration 4: done
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_MultipleStandalonePCLQsAndMultiplePCSGsUpdated tests a coherent
// update where multiple standalone PCLQs and multiple PCSGs are all updated.
//
// Starting state:
//
//	BPG (old hash): {5 Frontend pods, 3 Backend pods, 1 Prefill replica, 1 Decode replica}
//	SPGs (old hash): 3x {1 Prefill replica}, 2x {1 Decode replica}
//	All components updated.
//	Remaining: Frontend=5 pods, Backend=3 pods, Prefill=4 replicas, Decode=3 replicas.
//
// MVU template: {Frontend: 2 pods, Backend: 1 pod, Prefill: 1 replica, Decode: 1 replica}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2, B:1, P:1, D:1}. Remaining: F=3, B=2, P=3, D=2.
//	Iteration 2: MVU {F:3, B:2, P:1, D:1}. Absorbs F=1,B=1. Remaining: F=0, B=0, P=2, D=1.
//	Iteration 3: Tail-PGs: {P:1}, {P:1}, {D:1}. Remaining: all zero.
//	Iteration 4: Done.
func TestComputeNextIteration_MultipleStandalonePCLQsAndMultiplePCSGsUpdated(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2, "backend": 1},
		pcsgs:           map[string]int32{"prefill": 1, "decode": 1},
	}
	remainingOldPods := map[string]int32{"frontend": 5, "backend": 3}
	remainingOldReplicas := map[string]int32{"prefill": 4, "decode": 3}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(1), entries[0].PodCliques["backend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["decode"])

	// Iteration 2: absorbs F=1, B=1
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(3), entries[0].PodCliques["frontend"])
	assert.Equal(t, int32(2), entries[0].PodCliques["backend"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["prefill"])
	assert.Equal(t, int32(1), entries[0].PodCliqueScalingGroups["decode"])
	assert.Equal(t, int32(0), remainingOldPods["frontend"])
	assert.Equal(t, int32(0), remainingOldPods["backend"])

	// Iteration 3: Tail-PGs
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 3)
	prefillTails := 0
	decodeTails := 0
	for _, e := range entries {
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
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_ExactMultipleOfMinAvailable tests a coherent update where the
// pod count divides evenly by minAvailable. No absorption or tail PGs needed.
//
// Starting state:
//
//	BPG (old hash): {4 Frontend pods}
//	Only Frontend updated. Remaining: Frontend=4.
//
// MVU template: {Frontend: 2 pods}
//
// Expected iterations:
//
//	Iteration 1: MVU {F:2}. Remaining: F=2. Another MVU can be formed (2 >= 2).
//	Iteration 2: MVU {F:2}. Remaining: F=0. Cannot form another, but nothing to absorb.
//	Iteration 3: Done.
func TestComputeNextIteration_ExactMultipleOfMinAvailable(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{},
	}
	remainingOldPods := map[string]int32{"frontend": 4}
	remainingOldReplicas := map[string]int32{}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Iteration 1
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])

	// Iteration 2
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(2), entries[0].PodCliques["frontend"])

	// Iteration 3: done
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_NothingRemaining tests the edge case where the function is
// called with no remaining pods or replicas (update already completed or nothing to do).
func TestComputeNextIteration_NothingRemaining(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	remainingOldPods := map[string]int32{"frontend": 0}
	remainingOldReplicas := map[string]int32{"prefill": 0}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// TestComputeNextIteration_OnlyTailPGsFromStart tests the edge case where canFormMVU is
// false from the very first call because standalone PCLQ pods are already exhausted but
// PCSG replicas remain.
//
// Starting state:
//
//	All standalone PCLQ pods have already been processed in prior iterations.
//	Only 2 Prefill PCSG replicas remain.
//
// MVU template: {Frontend: 2 pods, Prefill: 1 replica}
//
// Expected: immediately produces Tail-PGs without attempting an MVU.
func TestComputeNextIteration_OnlyTailPGsFromStart(t *testing.T) {
	template := mvuTemplate{
		standalonePCLQs: map[string]int32{"frontend": 2},
		pcsgs:           map[string]int32{"prefill": 1},
	}
	remainingOldPods := map[string]int32{"frontend": 0}
	remainingOldReplicas := map[string]int32{"prefill": 2}
	counter := 0
	builder := makeTestEntryBuilder(&counter)

	// Cannot form MVU (F=0 < 2). Directly produces Tail-PGs.
	entries, done := computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	require.False(t, done)
	require.Len(t, entries, 2)
	for _, e := range entries {
		assert.Equal(t, int32(1), e.PodCliqueScalingGroups["prefill"])
		assert.Empty(t, e.PodCliques)
	}

	// Next call: done
	entries, done = computeNextIteration(template, remainingOldPods, remainingOldReplicas, builder)
	assert.True(t, done)
	assert.Nil(t, entries)
}

// makeTestEntryBuilder creates a simple entryBuilder that produces entries with an incrementing
// name and the given composition. It captures a counter to generate unique names across iterations.
func makeTestEntryBuilder(counter *int) podGangEntryBuilder {
	return func(standalonePCLQPods map[string]int32, pcsgReplicas map[string]int32, _ bool) grovecorev1alpha1.PodGangEntry {
		name := fmt.Sprintf("pg-%d", *counter)
		*counter++
		return grovecorev1alpha1.PodGangEntry{
			Name:                       name,
			PodCliqueSetGenerationHash: "new-hash",
			PodCliques:                 standalonePCLQPods,
			PodCliqueScalingGroups:     pcsgReplicas,
		}
	}
}
