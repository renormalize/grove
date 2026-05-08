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

package common

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
)

// ResourceNameReplica is a type that holds a resource name and its replica index.
type ResourceNameReplica struct {
	// Name is the name of the resource.
	Name string
	// Replica is the index of the replica within the resource.
	Replica int
}

// GenerateHeadlessServiceName generates a headless service name based on the PodCliqueSet name and replica index.
func GenerateHeadlessServiceName(pcsNameReplica ResourceNameReplica) string {
	return fmt.Sprintf("%s-%d", pcsNameReplica.Name, pcsNameReplica.Replica)
}

// GenerateHeadlessServiceAddress generates a headless service address based on the PodCliqueSet name, replica index, and namespace.
// The address is in the format: <headless-service-name>.<namespace>.svc.cluster.local
func GenerateHeadlessServiceAddress(pcsNameReplica ResourceNameReplica, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", GenerateHeadlessServiceName(pcsNameReplica), namespace)
}

// GeneratePodRoleName generates a Pod role name.
// This role will be associated to an init container within each Pod for a PodCliqueSet.
// The init container is created by the operator and is responsible for ensuring start-up order amongst PodCliques.
func GeneratePodRoleName(pcsName string) string {
	return fmt.Sprintf("%s:pcs:%s", v1alpha1.SchemeGroupVersion.Group, pcsName)
}

// GeneratePodRoleBindingName generates a role binding name. The role binding will bind the
// role to the service account that are created for the init container responsible for ensuring start-up order amongst PodCliques.
func GeneratePodRoleBindingName(pcsName string) string {
	return fmt.Sprintf("%s:pcs:%s", v1alpha1.SchemeGroupVersion.Group, pcsName)
}

// GeneratePodServiceAccountName generates a Pod service account used by all the init containers
// within the PodCliqueSet (one per pod) that are responsible for ensuring start-up order amongst PodCliques.
func GeneratePodServiceAccountName(pcsName string) string {
	return pcsName
}

// GenerateInitContainerSATokenSecretName generates a Secret name containing a service account token that will be mounted onto the init container
// responsible for ensuring start-up order amongst PodCliques.
func GenerateInitContainerSATokenSecretName(pcsName string) string {
	return fmt.Sprintf("%s-initc-sa-token-secret", pcsName)
}

// GeneratePodCliqueName generates a PodClique name based on the PodCliqueSet name, replica index, and PodCliqueTemplate name.
func GeneratePodCliqueName(ownerNameReplica ResourceNameReplica, pclqTemplateName string) string {
	return fmt.Sprintf("%s-%d-%s", ownerNameReplica.Name, ownerNameReplica.Replica, pclqTemplateName)
}

// GeneratePodCliqueScalingGroupName generates a PodCliqueScalingGroup name based on the PodCliqueSet name, replica index and PodCliqueScalingGroup name.
// PodCliqueScalingGroup name is only guaranteed to be unique within the PodCliqueSet, so it is prefixed with the PodCliqueSet name and its replica index.
func GeneratePodCliqueScalingGroupName(pcsNameReplica ResourceNameReplica, pclqScalingGroupName string) string {
	return fmt.Sprintf("%s-%d-%s", pcsNameReplica.Name, pcsNameReplica.Replica, pclqScalingGroupName)
}

// GeneratePodGangName generates a PodGang name using the unified naming convention:
// <pcs-name>-<pcs-replica-index>-<global-counter>.
// The global counter is a monotonically increasing integer scoped to the PCS replica,
// assigned at PodGang creation time and never reused.
func GeneratePodGangName(pcsName string, pcsReplicaIndex int, globalCounter int) string {
	return fmt.Sprintf("%s-%d-%d", pcsName, pcsReplicaIndex, globalCounter)
}

// GenerateBasePodGangName generates a base PodGang name for a PodCliqueSet replica.
// Deprecated: Use GeneratePodGangName with counter=0 instead.
// TODO: @renormalize this has to be removed
func GenerateBasePodGangName(pcsNameReplica ResourceNameReplica) string {
	return GeneratePodGangName(pcsNameReplica.Name, pcsNameReplica.Replica, 0)
}

// ExtractPodGangGlobalCounter extracts the global counter (last numeric segment) from the currently present PodGangs
// formatted as <pcs-name>-<pcs-replica-index>-<global-counter>.
func ExtractPodGangGlobalCounter(names []string) (*int, error) {
	if len(names) == 0 {
		return nil, nil
	}
	name := names[len(names)-1]
	lastDash := strings.LastIndex(name, "-")
	if lastDash < 0 {
		return nil, fmt.Errorf("invalid PodGang name: %s", name)
	}
	counter, err := strconv.Atoi(name[lastDash+1:])
	if err != nil {
		return nil, fmt.Errorf("invalid globalCounter in PodGang name %s: %w", name, err)
	}
	return &counter, nil
}

// ExtractScalingGroupNameFromPCSGFQN extracts the scaling group name from a PodCliqueScalingGroup FQN.
// For example, "simple1-0-sga" with pcsNameReplica="simple1-0" returns "sga".
func ExtractScalingGroupNameFromPCSGFQN(pcsgFQN string, pcsNameReplica ResourceNameReplica) string {
	prefix := fmt.Sprintf("%s-%d-", pcsNameReplica.Name, pcsNameReplica.Replica)
	return pcsgFQN[len(prefix):]
}
