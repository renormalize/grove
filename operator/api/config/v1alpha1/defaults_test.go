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
)

func TestSetDefaults_ClusterTopologyConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		input    ClusterTopologyConfiguration
		expected ClusterTopologyConfiguration
	}{
		{
			name: "enabled with no name defaults to grove-topology",
			input: ClusterTopologyConfiguration{
				Enabled: true,
				Name:    "",
			},
			expected: ClusterTopologyConfiguration{
				Enabled: true,
				Name:    "grove-topology",
			},
		},
		{
			name: "enabled with custom name preserves name",
			input: ClusterTopologyConfiguration{
				Enabled: true,
				Name:    "custom-topology",
			},
			expected: ClusterTopologyConfiguration{
				Enabled: true,
				Name:    "custom-topology",
			},
		},
		{
			name: "disabled with no name remains empty",
			input: ClusterTopologyConfiguration{
				Enabled: false,
				Name:    "",
			},
			expected: ClusterTopologyConfiguration{
				Enabled: false,
				Name:    "",
			},
		},
		{
			name: "disabled with name preserves name",
			input: ClusterTopologyConfiguration{
				Enabled: false,
				Name:    "some-topology",
			},
			expected: ClusterTopologyConfiguration{
				Enabled: false,
				Name:    "some-topology",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.input
			SetDefaults_ClusterTopologyConfiguration(&cfg)
			assert.Equal(t, tt.expected, cfg)
		})
	}
}
