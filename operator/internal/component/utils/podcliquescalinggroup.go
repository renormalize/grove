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
	"slices"
	"strconv"
	"time"

	apicommon "github.com/NVIDIA/grove/operator/api/common"
	"github.com/NVIDIA/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	pcsgList, err := doGetPCSGsForPGS(ctx, cl, pgsObjKey, map[string]string{
		apicommon.LabelPodGangSetReplicaIndex: strconv.Itoa(pgsReplicaIndex),
	})
	if err != nil {
		return nil, err
	}
	return pcsgList.Items, nil
}

// GetPCSGsForPGS fetches all PodCliqueScalingGroups for a PodGangSet.
func GetPCSGsForPGS(ctx context.Context, cl client.Client, pgsObjKey client.ObjectKey) ([]grovecorev1alpha1.PodCliqueScalingGroup, error) {
	pcsgList, err := doGetPCSGsForPGS(ctx, cl, pgsObjKey, nil)
	if err != nil {
		return nil, err
	}
	return pcsgList.Items, nil
}

func doGetPCSGsForPGS(ctx context.Context, cl client.Client, pgsObjKey client.ObjectKey, matchingLabels map[string]string) (*grovecorev1alpha1.PodCliqueScalingGroupList, error) {
	pcsgList := &grovecorev1alpha1.PodCliqueScalingGroupList{}
	if err := cl.List(ctx,
		pcsgList,
		client.InNamespace(pgsObjKey.Namespace),
		client.MatchingLabels(lo.Assign(
			apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgsObjKey.Name),
			matchingLabels,
		)),
	); err != nil {
		return nil, err
	}
	return pcsgList, nil
}

// GetMinAvailableBreachedPCSGInfo filters PodCliqueScalingGroups that have grovecorev1alpha1.ConditionTypeMinAvailableBreached set to true.
// It returns the names of all such PodCliqueScalingGroups and minimum of all the waitDurations.
func GetMinAvailableBreachedPCSGInfo(pcsgs []grovecorev1alpha1.PodCliqueScalingGroup, terminationDelay time.Duration, since time.Time) ([]string, time.Duration) {
	pcsgCandidateNames := make([]string, 0, len(pcsgs))
	waitForDurations := make([]time.Duration, 0, len(pcsgs))
	for _, pcsg := range pcsgs {
		cond := meta.FindStatusCondition(pcsg.Status.Conditions, constants.ConditionTypeMinAvailableBreached)
		if cond == nil {
			continue
		}
		if cond.Status == metav1.ConditionTrue {
			pcsgCandidateNames = append(pcsgCandidateNames, pcsg.Name)
			waitFor := terminationDelay - since.Sub(cond.LastTransitionTime.Time)
			waitForDurations = append(waitForDurations, waitFor)
		}
	}
	if len(waitForDurations) == 0 {
		return pcsgCandidateNames, 0
	}
	slices.Sort(waitForDurations)
	return pcsgCandidateNames, waitForDurations[0]
}

// IsPCSGUpdateInProgress checks if PCSG is under rolling update.
func IsPCSGUpdateInProgress(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) bool {
	return k8sutils.IsConditionTrue(pcsg.Status.Conditions, constants.ConditionTypeUpdateInProgress)
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

// GroupPCSGsByPGSReplicaIndex filters PCSGs that have a PodGangSetReplicaIndex label and groups them by the PGS replica.
func GroupPCSGsByPGSReplicaIndex(pcsgs []grovecorev1alpha1.PodCliqueScalingGroup) map[string][]grovecorev1alpha1.PodCliqueScalingGroup {
	return groupPCSGsByLabel(pcsgs, apicommon.LabelPodGangSetReplicaIndex)
}

func groupPCSGsByLabel(pcsgs []grovecorev1alpha1.PodCliqueScalingGroup, label string) map[string][]grovecorev1alpha1.PodCliqueScalingGroup {
	result := make(map[string][]grovecorev1alpha1.PodCliqueScalingGroup)
	for _, pcsg := range pcsgs {
		labelValue, exists := pcsg.Labels[label]
		if !exists {
			continue
		}
		result[labelValue] = append(result[labelValue], pcsg)
	}
	return result
}

// GetPCSGsByPGSReplicaIndex groups the PodCliqueScalingGroups per PodGangSet replica index and returns a map with the key being the PodGangSet replica index and the value
// being the slice of PodCliqueScalingGroup objects.
func GetPCSGsByPGSReplicaIndex(ctx context.Context, cl client.Client, pgsObjKey client.ObjectKey) (map[string][]grovecorev1alpha1.PodCliqueScalingGroup, error) {
	pcsgList := &grovecorev1alpha1.PodCliqueScalingGroupList{}
	if err := cl.List(ctx,
		pcsgList,
		client.InNamespace(pgsObjKey.Namespace),
		client.MatchingLabels(apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgsObjKey.Name)),
	); err != nil {
		return nil, err
	}
	pcsgsByPGSReplicaIndex := make(map[string][]grovecorev1alpha1.PodCliqueScalingGroup)
	for _, pcsg := range pcsgList.Items {
		pgsReplicaIndex, ok := pcsg.Labels[apicommon.LabelPodGangSetReplicaIndex]
		if !ok {
			continue
		}
		pcsgsByPGSReplicaIndex[pgsReplicaIndex] = append(pcsgsByPGSReplicaIndex[pgsReplicaIndex], pcsg)
	}
	return pcsgsByPGSReplicaIndex, nil
}
