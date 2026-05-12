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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName={pgm}

// PodGangMap is the desired-state mapping between PodGangs and their constituent
// PodClique and PodCliqueScalingGroup pod counts for a single PodCliqueSet replica.
// One PodGangMap resource exists per PodCliqueSet replica, named <pcs-name>-<pcs-replica-index>.
type PodGangMap struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec defines the desired PodGang-to-pod-count mapping for this PodCliqueSet replica.
	Spec PodGangMapSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodGangMapList is a list of PodGangMap resources.
type PodGangMapList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items is the list of PodGangMap resources.
	Items []PodGangMap `json:"items"`
}

// PodGangMapSpec defines the desired PodGang composition for a PodCliqueSet replica.
type PodGangMapSpec struct {
	// PodCliqueSetReplicaIndex is the index of the PodCliqueSet replica this map belongs to.
	PodCliqueSetReplicaIndex int32 `json:"podCliqueSetReplicaIndex"`
	// Entries is the ordered list of desired PodGangs for this PodCliqueSet replica.
	// Each entry corresponds to one PodGang and specifies its pod and replica counts.
	// +listType=map
	// +listMapKey=name
	Entries []PodGangEntry `json:"entries"`
}

// TopologyAnchor determines how multi-level topology constraints (PCS, PCSG, PCLQ) are
// mapped onto a PodGang resource derived from this entry.
//
// PodGang resources support topology constraints at three levels:
//   - PodGang.Spec.TopologyConstraint: broadest scope, applies to all PodGroups
//   - PodGang.Spec.TopologyConstraintGroupConfigs: intermediate scope, groups PodGroups
//     belonging to the same PCSG replica under a shared PCSG-level constraint
//   - PodGang.Spec.PodGroups[].TopologyConstraint: narrowest scope, per-PodClique constraint
//
// TopologyAnchor controls which source constraint populates the PodGang-level field and
// whether TopologyConstraintGroupConfigs are emitted. PodGroup-level constraints are always
// derived from the PodClique template and are unaffected by this field.
//
// +kubebuilder:validation:Enum=pcs;pcsg
type TopologyAnchor string

const (
	// TopologyAnchorPCS indicates the PodGang is anchored to the PodCliqueSet. The PCS-level
	// topology constraint is used as the PodGang-level constraint, and PCSG-level constraints
	// are emitted as TopologyConstraintGroupConfigs grouping each PCSG replica's PodCliques.
	TopologyAnchorPCS TopologyAnchor = "pcs"
	// TopologyAnchorPCSG indicates the PodGang represents a single PodCliqueScalingGroup replica
	// that is not anchored to the PodCliqueSet. The PCSG-level topology constraint is promoted
	// to the PodGang-level constraint. No TopologyConstraintGroupConfigs are emitted since the
	// entire PodGang IS the PCSG replica — a subgroup constraint would be redundant.
	TopologyAnchorPCSG TopologyAnchor = "pcsg"
)

// PodGangEntry describes the desired composition of a single PodGang.
type PodGangEntry struct {
	// Name is the name of the PodGang this entry corresponds to.
	Name string `json:"name"`
	// PodCliqueSetGenerationHash is the PodCliqueSet generation hash that pods in this PodGang
	// must match. Used by PodClique and PodCliqueScalingGroup reconcilers to create pods at the
	// correct spec version and to distinguish old pods from new pods during a coherent update.
	PodCliqueSetGenerationHash string `json:"podCliqueSetGenerationHash"`
	// TopologyAnchor determines how topology constraints are assigned to the PodGang
	// derived from this entry.
	TopologyAnchor TopologyAnchor `json:"topologyAnchor"`
	// PodCliques maps standalone PodClique name to the number of pods that belong to this PodGang.
	// Only standalone PodCliques (not owned by a PodCliqueScalingGroup) are listed here.
	// PodCliques owned by a PodCliqueScalingGroup derive their PodGang association via
	// PodCliqueScalingGroups below.
	// +optional
	PodCliques map[string]int32 `json:"podCliques,omitempty"`
	// PodCliqueScalingGroups maps PodCliqueScalingGroup name to the number of replicas of that
	// PodCliqueScalingGroup that belong to this PodGang. A PodClique reconciler for a
	// PodCliqueScalingGroup-owned PodClique uses this field to find its target PodGang by looking
	// up its owning PodCliqueScalingGroup name here.
	// +optional
	PodCliqueScalingGroups map[string]int32 `json:"podCliqueScalingGroups,omitempty"`
}
