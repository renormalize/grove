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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.hpaPodSelector
// +kubebuilder:resource:shortName={pclq}

// PodClique is a set of pods running the same image. TODO: @renormalize expand on this.
type PodClique struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec defines the specification of a PodClique.
	Spec PodCliqueSpec `json:"spec"`
	// Status defines the status of a PodClique.
	Status PodCliqueStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodCliqueList is a list of PodClique's.
type PodCliqueList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items is a slice of PodClique.
	Items []PodClique `json:"items"`
}

// PodCliqueSpec defines the specification of a PodClique.
type PodCliqueSpec struct {
	// RoleName is the name of the role that this PodClique will assume.
	RoleName string `json:"roleName"`
	// Spec is the spec of the pods in the clique.
	PodSpec corev1.PodSpec `json:"podSpec"`
	// Replicas is the number of replicas of the pods in the clique. It cannot be less than 1.
	Replicas int32 `json:"replicas"`
	// MinAvailable serves two purposes:
	// 1. It defines the minimum number of pods that are guaranteed to be gang scheduled.
	// 2. It defines the minimum requirement of available pods in a PodClique. Violation of this threshold will result in termination of the PodGang that it belongs to.
	// If MinAvailable is not set, then it will default to the template Replicas.
	// +optional
	MinAvailable *int32 `json:"minAvailable,omitempty"`
	// StartsAfter provides you a way to explicitly define the startup dependencies amongst cliques.
	// If CliqueStartupType in PodGang has been set to 'CliqueStartupTypeExplicit', then to create an ordered start amongst PodClique's StartsAfter can be used.
	// A forest of DAG's can be defined to model any start order dependencies. If there are more than one PodClique's defined and StartsAfter is not set for any of them,
	// then their startup order is random at best and must not be relied upon.
	// Validations:
	// 1. If a StartsAfter has been defined and one or more cycles are detected in DAG's then it will be flagged as validation error.
	// 2. If StartsAfter is defined and does not identify any PodClique then it will be flagged as a validation error.
	// +optional
	StartsAfter []string `json:"startsAfter,omitempty"`
	// ScaleConfig is the horizontal pod autoscaler configuration for a PodClique.
	// +optional
	ScaleConfig *AutoScalingConfig `json:"autoScalingConfig,omitempty"`
}

// AutoScalingConfig defines the configuration for the horizontal pod autoscaler.
type AutoScalingConfig struct {
	// MinReplicas is the lower limit for the number of replicas for the target resource.
	// It will be used by the horizontal pod autoscaler to determine the minimum number of replicas to scale-in to.
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// maxReplicas is the upper limit for the number of replicas to which the autoscaler can scale up.
	// It cannot be less that minReplicas.
	MaxReplicas int32 `json:"maxReplicas"`
	// Metrics contains the specifications for which to use to calculate the
	// desired replica count (the maximum replica count across all metrics will
	// be used).  The desired replica count is calculated multiplying the
	// ratio between the target value and the current value by the current
	// number of pods.  Ergo, metrics used must decrease as the pod count is
	// increased, and vice versa.  See the individual metric source types for
	// more information about how each type of metric must respond.
	// If not set, the default metric will be set to 80% average CPU utilization.
	// +listType=atomic
	// +optional
	Metrics []autoscalingv2.MetricSpec `json:"metrics,omitempty" protobuf:"bytes,4,rep,name=metrics"`
}

// PodCliqueStatus defines the status of a PodClique.
type PodCliqueStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
	// LastOperation captures the last operation done by the respective reconciler on the PodClique.
	LastOperation *LastOperation `json:"lastOperation,omitempty"`
	// LastErrors captures the last errors observed by the controller when reconciling the PodClique.
	LastErrors []LastError `json:"lastErrors,omitempty"`
	// Replicas is the total number of non-terminated Pods targeted by this PodClique.
	Replicas int32 `json:"replicas,omitempty"`
	// ReadyReplicas is the number of ready Pods targeted by this PodClique.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// UpdatedReplicas is the number of Pods that have been updated and are at the desired revision of the PodClique.
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`
	// ScheduleGatedReplicas is the number of Pods that have been created with one or more scheduling gate(s) set.
	// Sum of ReadyReplicas and ScheduleGatedReplicas will always be <= Replicas.
	ScheduleGatedReplicas int32 `json:"scheduleGatedReplicas,omitempty"`
	// Selector is the label selector that determines which pods are part of the PodClique.
	// PodClique is a unit of scale and this selector is used by HPA to scale the PodClique based on metrics captured for the pods that match this selector.
	Selector *string `json:"hpaPodSelector,omitempty"`
	// Conditions represents the latest available observations of the clique by its controller.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SetLastErrors sets the last errors observed by the controller when reconciling the PodClique.
func (pclq *PodClique) SetLastErrors(lastErrs ...LastError) {
	pclq.Status.LastErrors = lastErrs
}

// SetLastOperation sets the last operation done by the respective reconciler on the PodClique.
func (pclq *PodClique) SetLastOperation(operation *LastOperation) {
	pclq.Status.LastOperation = operation
}
