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

package utils

import (
	"context"
	apicommon "github.com/NVIDIA/grove/operator/api/common"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
	"strconv"
)

// FindScalingGroupConfigForClique searches through the scaling group configurations to find
// the one that contains the specified clique name in its CliqueNames list.
//
// Returns the matching PodCliqueScalingGroupConfig and true if found, or an empty config and false if not found.
func FindScalingGroupConfigForClique(scalingGroupConfigs []grovecorev1alpha1.PodCliqueScalingGroupConfig, cliqueName string) *grovecorev1alpha1.PodCliqueScalingGroupConfig {
	pcsgConfig, ok := lo.Find(scalingGroupConfigs, func(pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig) bool {
		return slices.Contains(pcsgConfig.CliqueNames, cliqueName)
	})
	if !ok {
		return nil
	}
	return &pcsgConfig
}

// GetPCSGsForPGSReplicaIndex fetches all PodCliqueScalingGroups for a PodGangSet replica index.
func GetPCSGsForPGSReplicaIndex(ctx context.Context, cl client.Client, pgsObjKey client.ObjectKey, pgsReplicaIndex int) ([]grovecorev1alpha1.PodCliqueScalingGroup, error) {
	pcsgList := &grovecorev1alpha1.PodCliqueScalingGroupList{}
	if err := cl.List(ctx,
		pcsgList,
		client.InNamespace(pgsObjKey.Namespace),
		client.MatchingLabels(lo.Assign(
			apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgsObjKey.Name),
			map[string]string{
				apicommon.LabelPodGangSetReplicaIndex: strconv.Itoa(pgsReplicaIndex),
			},
		)),
	); err != nil {
		return nil, err
	}
	return pcsgList.Items, nil
}

// GenerateDependencyNamesForBasePodGang generates the FQNs of all PodCliques that would qualify as a dependency.
func GenerateDependencyNamesForBasePodGang(pgs *grovecorev1alpha1.PodGangSet, pgsReplicaIndex int, parentCliqueName string) []string {
	parentPCLQNames := make([]string, 0)
	pcsgConfig := FindScalingGroupConfigForClique(pgs.Spec.Template.PodCliqueScalingGroupConfigs, parentCliqueName)
	if pcsgConfig != nil {
		// Generate FQNs of minAvailable number of PodCliques that belong to a PodCliueScalingGroup.
		pcsgFQN := apicommon.GeneratePodCliqueScalingGroupName(apicommon.ResourceNameReplica{Name: pgs.Name, Replica: pgsReplicaIndex}, pcsgConfig.Name)
		for pcsgReplicaIndex := range int(*pcsgConfig.MinAvailable) {
			parentPCLQNames = append(parentPCLQNames, apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsgFQN, Replica: pcsgReplicaIndex}, parentCliqueName))
		}
	} else {
		parentPCLQNames = append(parentPCLQNames, apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pgs.Name, Replica: pgsReplicaIndex}, parentCliqueName))
	}
	return parentPCLQNames
}
