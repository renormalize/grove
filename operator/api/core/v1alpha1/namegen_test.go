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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestExtractScalingGroupNameFromPCSGFQN(t *testing.T) {
	tests := []struct {
		name           string
		pcsgName       string
		pgsNameReplica ResourceNameReplica
		expected       string
	}{
		{
			name:     "simple scaling group name",
			pcsgName: "simple1-0-sga",
			pgsNameReplica: ResourceNameReplica{
				Name:    "simple1",
				Replica: 0,
			},
			expected: "sga",
		},
		{
			name:     "scaling group with different replica index",
			pcsgName: "simple1-2-sga",
			pgsNameReplica: ResourceNameReplica{
				Name:    "simple1",
				Replica: 2,
			},
			expected: "sga",
		},
		{
			name:     "complex scaling group name",
			pcsgName: "test-workload-1-gpu-workers",
			pgsNameReplica: ResourceNameReplica{
				Name:    "test-workload",
				Replica: 1,
			},
			expected: "gpu-workers",
		},
		{
			name:     "scaling group with hyphens in name",
			pcsgName: "my-app-0-data-processing-group",
			pgsNameReplica: ResourceNameReplica{
				Name:    "my-app",
				Replica: 0,
			},
			expected: "data-processing-group",
		},
		{
			name:     "single character scaling group",
			pcsgName: "app-5-x",
			pgsNameReplica: ResourceNameReplica{
				Name:    "app",
				Replica: 5,
			},
			expected: "x",
		},
		{
			name:     "numeric scaling group name",
			pcsgName: "workload-0-123",
			pgsNameReplica: ResourceNameReplica{
				Name:    "workload",
				Replica: 0,
			},
			expected: "123",
		},
		{
			name:     "long PGS name with scaling group",
			pcsgName: "very-long-podgangset-name-0-sg",
			pgsNameReplica: ResourceNameReplica{
				Name:    "very-long-podgangset-name",
				Replica: 0,
			},
			expected: "sg",
		},
		{
			name:     "scaling group name with numbers and hyphens",
			pcsgName: "app-3-worker-group-v2",
			pgsNameReplica: ResourceNameReplica{
				Name:    "app",
				Replica: 3,
			},
			expected: "worker-group-v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractScalingGroupNameFromPCSGFQN(tt.pcsgName, tt.pgsNameReplica)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGenerateBasePodGangName(t *testing.T) {
	tests := []struct {
		name           string
		pgsNameReplica ResourceNameReplica
		expected       string
	}{
		{
			name:           "simple base PodGang name",
			pgsNameReplica: ResourceNameReplica{Name: "simple1", Replica: 0},
			expected:       "simple1-0",
		},
		{
			name:           "base PodGang with different replica",
			pgsNameReplica: ResourceNameReplica{Name: "test-app", Replica: 2},
			expected:       "test-app-2",
		},
		{
			name:           "complex PGS name",
			pgsNameReplica: ResourceNameReplica{Name: "my-complex-workload", Replica: 5},
			expected:       "my-complex-workload-5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateBasePodGangName(tt.pgsNameReplica)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractScalingGroupNameFromPCSGFQN_Consistency(t *testing.T) {
	// Test that ExtractScalingGroupNameFromPCSGFQN is the inverse of GeneratePodCliqueScalingGroupName
	testCases := []struct {
		pgsNameReplica   ResourceNameReplica
		scalingGroupName string
	}{
		{
			pgsNameReplica:   ResourceNameReplica{Name: "simple1", Replica: 0},
			scalingGroupName: "sga",
		},
		{
			pgsNameReplica:   ResourceNameReplica{Name: "test-app", Replica: 2},
			scalingGroupName: "worker-group",
		},
		{
			pgsNameReplica:   ResourceNameReplica{Name: "my-workload", Replica: 1},
			scalingGroupName: "gpu-nodes",
		},
	}

	for _, tc := range testCases {
		t.Run("consistency_test", func(t *testing.T) {
			// Generate PCSG name
			generatedPCSGName := GeneratePodCliqueScalingGroupName(tc.pgsNameReplica, tc.scalingGroupName)

			// Extract scaling group name back
			extractedScalingGroupName := ExtractScalingGroupNameFromPCSGFQN(generatedPCSGName, tc.pgsNameReplica)

			// They should match
			assert.Equal(t, tc.scalingGroupName, extractedScalingGroupName)
		})
	}
}

func TestGeneratePodGangNameForPodCliqueOwnedByPodGangSet(t *testing.T) {
	// Create a PodGangSet for testing
	pgs := &PodGangSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "simple1",
		},
	}

	tests := []struct {
		name                string
		pgsReplicaIndex     int
		expectedPodGangName string
	}{
		{
			name:                "PGS replica 0",
			pgsReplicaIndex:     0,
			expectedPodGangName: "simple1-0",
		},
		{
			name:                "PGS replica 1",
			pgsReplicaIndex:     1,
			expectedPodGangName: "simple1-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GeneratePodGangNameForPodCliqueOwnedByPodGangSet(pgs, tt.pgsReplicaIndex)
			assert.Equal(t, tt.expectedPodGangName, result)
		})
	}
}

func TestGeneratePodGangNameForPodCliqueOwnedByPCSG(t *testing.T) {
	// Create a PodGangSet for testing
	pgs := &PodGangSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "simple1",
		},
	}

	tests := []struct {
		name                string
		pgsReplicaIndex     int
		pcsg                *PodCliqueScalingGroup
		pcsgReplicaIndex    int
		expectedPodGangName string
	}{
		{
			name:            "PCSG PodClique within minAvailable",
			pgsReplicaIndex: 0,
			pcsg: &PodCliqueScalingGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: "simple1-0-sga",
				},
				Spec: PodCliqueScalingGroupSpec{
					MinAvailable: ptr.To[int32](3),
				},
			},
			pcsgReplicaIndex:    1, // Within minAvailable (< 3)
			expectedPodGangName: "simple1-0",
		},
		{
			name:            "PCSG PodClique beyond minAvailable - first scaled",
			pgsReplicaIndex: 0,
			pcsg: &PodCliqueScalingGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: "simple1-0-sga",
				},
				Spec: PodCliqueScalingGroupSpec{
					MinAvailable: ptr.To[int32](3),
				},
			},
			pcsgReplicaIndex:    3,                 // Beyond minAvailable (>= 3)
			expectedPodGangName: "simple1-0-sga-0", // First scaled PodGang (3-3=0)
		},
		{
			name:            "PCSG PodClique beyond minAvailable - second scaled",
			pgsReplicaIndex: 0,
			pcsg: &PodCliqueScalingGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: "simple1-0-sga",
				},
				Spec: PodCliqueScalingGroupSpec{
					MinAvailable: ptr.To[int32](3),
				},
			},
			pcsgReplicaIndex:    4,                 // Beyond minAvailable (>= 3)
			expectedPodGangName: "simple1-0-sga-1", // Second scaled PodGang (4-3=1)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GeneratePodGangNameForPodCliqueOwnedByPCSG(pgs, tt.pgsReplicaIndex, tt.pcsg, tt.pcsgReplicaIndex)
			assert.Equal(t, tt.expectedPodGangName, result)
		})
	}
}

func TestCreatePodGangNameFromPCSGFQN(t *testing.T) {
	tests := []struct {
		name             string
		pcsgFQN          string
		pcsgReplicaIndex int
		expected         string
	}{
		{
			name:             "scaled PodGang name from FQN",
			pcsgFQN:          "simple1-0-sga",
			pcsgReplicaIndex: 1,
			expected:         "simple1-0-sga-1",
		},
		{
			name:             "scaled PodGang name from FQN with different replica",
			pcsgFQN:          "simple1-0-sga",
			pcsgReplicaIndex: 2,
			expected:         "simple1-0-sga-2",
		},
		{
			name:             "complex scaling group name",
			pcsgFQN:          "test-2-complex-sg",
			pcsgReplicaIndex: 0,
			expected:         "test-2-complex-sg-0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CreatePodGangNameFromPCSGFQN(tt.pcsgFQN, tt.pcsgReplicaIndex)
			assert.Equal(t, tt.expected, result)
		})
	}
}
