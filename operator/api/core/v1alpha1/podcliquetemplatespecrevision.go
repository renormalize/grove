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
// +kubebuilder:resource:shortName={pctsr}

// PodCliqueTemplateSpecRevision is an immutable snapshot of a PodClique's PodCliqueTemplateSpec
// at a specific revision. It is created whenever a PodClique's PodCliqueTemplateSpec changes as
// part of a PodCliqueSet update, and is never modified after creation.
//
// Naming convention: <pcs-name>-<pclq-name>-r<revision-number>
//
// PodCliqueTemplateSpecRevision has no scale-subresource and plays no active role in pod lifecycle
// management — it is purely a snapshot for audit and recovery purposes.
type PodCliqueTemplateSpecRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec holds the PodCliqueTemplateSpec captured at the time this revision was created.
	Spec PodCliqueTemplateSpecRevisionSpec `json:"spec"`
}

// PodCliqueTemplateSpecRevisionSpec holds the PodCliqueTemplateSpec captured at the time this
// revision was created.
type PodCliqueTemplateSpecRevisionSpec struct {
	// PodCliqueTemplateSpec is the exact PodCliqueTemplateSpec of the PodClique at this revision.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	PodCliqueTemplateSpec PodCliqueTemplateSpec `json:"podCliqueTemplateSpec"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodCliqueTemplateSpecRevisionList is a list of PodCliqueTemplateSpecRevision resources.
type PodCliqueTemplateSpecRevisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items is a slice of PodCliqueTemplateSpecRevisions.
	Items []PodCliqueTemplateSpecRevision `json:"items"`
}
