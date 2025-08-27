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

package podclique

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	apicommon "github.com/NVIDIA/grove/operator/api/common"
	"github.com/NVIDIA/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/NVIDIA/grove/operator/internal/component"
	groveevents "github.com/NVIDIA/grove/operator/internal/component/events"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	groveerr "github.com/NVIDIA/grove/operator/internal/errors"
	"github.com/NVIDIA/grove/operator/internal/utils"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	errCodeListPodClique                          grovecorev1alpha1.ErrorCode = "ERR_LIST_PODCLIQUE"
	errCodeMissingStartupType                     grovecorev1alpha1.ErrorCode = "ERR_UNDEFINED_STARTUP_TYPE"
	errCodeSetPodCliqueOwnerReference             grovecorev1alpha1.ErrorCode = "ERR_SET_PODCLIQUE_OWNER_REFERENCE"
	errCodeBuildPodClique                         grovecorev1alpha1.ErrorCode = "ERR_BUILD_PODCLIQUE"
	errCodeCreatePodCliques                       grovecorev1alpha1.ErrorCode = "ERR_CREATE_PODCLIQUES"
	errCodeDeletePodClique                        grovecorev1alpha1.ErrorCode = "ERR_DELETE_PODCLIQUE"
	errCodeGetPodGangSet                          grovecorev1alpha1.ErrorCode = "ERR_GET_PODGANGSET"
	errCodeMissingPGSReplicaIndex                 grovecorev1alpha1.ErrorCode = "ERR_MISSING_PODGANGSET_REPLICA_INDEX"
	errCodeReplicaIndexIntConversion              grovecorev1alpha1.ErrorCode = "ERR_PODGANGSET_REPLICA_INDEX_CONVERSION"
	errCodeListPodCliquesForPCSG                  grovecorev1alpha1.ErrorCode = "ERR_LIST_PODCLIQUE_FOR_PCSG"
	errCodeCreatePodClique                        grovecorev1alpha1.ErrorCode = "ERR_CREATE_PODCLIQUE"
	errCodeParsePodCliqueScalingGroupReplicaIndex grovecorev1alpha1.ErrorCode = "ERR_PARSE_PODCLIQUESCALINGGROUP_REPLICA_INDEX"
	errCodeUpdateStatus                           grovecorev1alpha1.ErrorCode = "ERR_UPDATE_STATUS"
	errCodeUpdateLastIndexSelectedForUpdate       grovecorev1alpha1.ErrorCode = "ERR_UPDATE_STATUS_LAST_INDEX_SELECTED_FOR_UPDATE"
)

var (
	errPCCGMinAvailableBreached = errors.New("minAvailable has been breached for PodCliqueScalingGroup")
)

type _resource struct {
	client        client.Client
	scheme        *runtime.Scheme
	eventRecorder record.EventRecorder
}

// New creates an instance of PodClique component operator.
func New(client client.Client, scheme *runtime.Scheme, eventRecorder record.EventRecorder) component.Operator[grovecorev1alpha1.PodCliqueScalingGroup] {
	return &_resource{
		client:        client,
		scheme:        scheme,
		eventRecorder: eventRecorder,
	}
}

// GetExistingResourceNames returns the names of all the existing resources that the PodClique Operator manages.
func (r _resource) GetExistingResourceNames(ctx context.Context, logger logr.Logger, pcsgObjMeta metav1.ObjectMeta) ([]string, error) {
	logger.Info("Looking for existing PodCliques managed by PodCliqueScalingGroup")
	pclqPartialObjMetaList, err := k8sutils.ListExistingPartialObjectMetadata(ctx,
		r.client,
		grovecorev1alpha1.SchemeGroupVersion.WithKind("PodClique"),
		pcsgObjMeta,
		getPodCliqueSelectorLabels(pcsgObjMeta))
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodClique,
			component.OperationGetExistingResourceNames,
			fmt.Sprintf("Error listing PodCliques for PodCliqueScalingGroup: %v", k8sutils.GetObjectKeyFromObjectMeta(pcsgObjMeta)),
		)
	}
	return k8sutils.FilterMapOwnedResourceNames(pcsgObjMeta, pclqPartialObjMetaList), nil
}

// Sync synchronizes all resources that the PodClique Operator manages.
func (r _resource) Sync(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
	syncCtx, err := r.prepareSyncContext(ctx, logger, pcsg)

	// If there are excess PodCliques than expected, delete the ones that are no longer expected but existing.
	// This can happen when PCSG replicas have been scaled-in.
	if err = r.triggerDeletionOfExcessPCSGReplicas(ctx, logger, syncCtx, pcsg); err != nil {
		return err
	}
	// Create or update the expected PodCliques as per the PodCliqueScalingGroup configurations defined in the PodGangSet.
	if err = r.createOrUpdateExpectedPCLQs(ctx, logger, syncCtx, pcsg); err != nil {
		return err
	}

	// Only if the rolling update is not in progress, check for a possibility of gang termination and execute it only if
	// the pcsg.spec.minAvailable is not breached.
	if !componentutils.IsPCSGUpdateInProgress(pcsg) {
		if err = r.processMinAvailableBreachedPCSGReplicas(ctx, logger, syncCtx, pcsg); err != nil {
			if errors.Is(err, errPCCGMinAvailableBreached) {
				logger.Info("Skipping further reconciliation as MinAvailable for the PCSG has been breached. This can potentially trigger PGS replica deletion.")
				return nil
			}
			return err
		}
	} else {
		if err = r.orchestrateRollingUpdate(ctx, logger, syncCtx, pcsg); err != nil {
			return err
		}
	}

	// If there are any PCSG replicas which have minAvailableBreached but the terminationDelay has not yet expired, then
	// requeue the event after a fixed delay.
	if len(syncCtx.pcsgIndicesToRequeue) > 0 {
		return groveerr.New(groveerr.ErrCodeRequeueAfter,
			component.OperationSync,
			"Requeuing to re-process PCLQs that have breached MinAvailable but not crossed TerminationDelay",
		)
	}

	return nil
}

// func (r _resource) processPendingUpdates(ctx context.Context, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
// 	if pcsg.Status.RollingUpdateProgress == nil || pcsg.Status.RollingUpdateProgress.UpdateEndedAt != nil {
// 		// There are no pending updates, return early.
// 		return nil
// 	}

// 	if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil {

// 	}

// 	/*
// 		if there is a currently updating replica {
// 			update its progress { update updatedPCLQs and currentlyUpdating }
// 			check if it is complete
// 			if the currently updating replica is now complete {
// 				set currentlyUpdating to nil
// 				increment the updatedReplicas
// 			} else {
// 				requeue
// 			}
// 		} else {
// 			get next replica to update
// 			if there are no more any replicas to update {
// 				mark end of update
// 				set currentlyUpdating to is set to nil
// 				return
// 			} else {
// 				set currentlyUpdating to this replica
// 				trigger update of the next replica
// 				requeue
// 			}
// 		}
// 	*/

// 	pcsgReplicaIndicesPendingUpdate := getPCSGReplicaIndicesPendingUpdate(syncCtx.pgs, pcsg, syncCtx.existingPCLQs)
// 	if len(pcsgReplicaIndicesPendingUpdate) > 0 {
// 		// Progress with rolling update only when any pending update for previously selected PCSG replica has completed.
// 		updatedPCLQFQNs := make([]string, 0)
// 		var allUpdated bool
// 		if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil {
// 			updatedPCLQFQNs, allUpdated = getUpdatedPCLQFQNsForCurrentlyUpdatingIndex(pcsg, expectedPCLQsPerPCSGReplica, existingPCLQs)
// 		}
// 		if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil && !allUpdated {
// 			// update the status with the currenlty updated replicas
// 			// use updatedPCLQFQNs to indicate progress
// 			if err := r.updatePCLQUpdateProgressForReplica(ctx, pcsg, updatedPCLQFQNs); err != nil {
// 				return err
// 			}
// 			return groveerr.New(groveerr.ErrCodeRequeueAfter,
// 				component.OperationSync,
// 				fmt.Sprintf("Requeuing to allow pending PCSG replica index %d to finish update", pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex),
// 			)
// 		}
// 		// Order the PCSG replicas based on a criteria and select the next PCSG index to update.
// 		orderedPCSGReplicaIndicesToUpdate := getOrderedPCSGIndicesPendingUpdate(pcsgReplicaIndicesPendingUpdate, pcsgIndicesToTerminate, pcsgIndicesToRequeue)
// 		replicaPickedForUpdate := orderedPCSGReplicaIndicesToUpdate[0]
// 		logger.Info("Found PCSG replicas pending update, picking up one replica to update", "pcsgReplicaIndicesPendingUpdate", pcsgReplicaIndicesPendingUpdate, "replicaPickedForUpdate", replicaPickedForUpdate)
// 		// TODO: FIX IMMEDIATELY
// 		if err := r.updatePCLQUpdateProgressForReplica(ctx, pcsg, updatedPCLQFQNs); err != nil {
// 			return err
// 		}
// 		if err = r.updatePCSGReplica(ctx, logger, pgs, pcsg, replicaPickedForUpdate); err != nil {
// 			return err
// 		}
// 		// Requeue after a fixed interval.
// 		return groveerr.New(groveerr.ErrCodeRequeueAfter,
// 			component.OperationSync,
// 			"Requeuing to continue PCSG replica updates",
// 		)
// 	} else {
// 		updatedPCLQFQNs := make([]string, 0)
// 		var allUpdated bool
// 		if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil {
// 			updatedPCLQFQNs, allUpdated = getUpdatedPCLQFQNsForCurrentlyUpdatingIndex(pcsg, expectedPCLQsPerPCSGReplica, existingPCLQs)
// 		}
// 		if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil && !allUpdated {
// 			if err := r.updatePCLQUpdateProgressForReplica(ctx, pcsg, updatedPCLQFQNs); err != nil {
// 				return err
// 			}
// 			return groveerr.New(groveerr.ErrCodeRequeueAfter,
// 				component.OperationSync,
// 				fmt.Sprintf("Requeuing to allow pending PCSG replica index %d to finish update", pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex),
// 			)
// 		}
// 		// TODO: FIX IMMEDIATELY
// 		if err := r.updatePCLQUpdateProgressForReplica(ctx, pcsg, updatedPCLQFQNs); err != nil {
// 			return err
// 		}
// 		if err = r.resetUpdateStatus(ctx, pcsg); err != nil {
// 			return err
// 		}
// 	}
// 	return
// }

func (r _resource) processMinAvailableBreachedPCSGReplicas(ctx context.Context, logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
	// If pcsg.spec.minAvailable is breached, then delegate the responsibility to the PodGangSet reconciler which after
	// termination delay terminate the PodGangSet replica. No further processing is required to be done here.
	minAvailableBreachedPCSGReplicas := len(syncCtx.pcsgIndicesToTerminate) + len(syncCtx.pcsgIndicesToRequeue)
	if int(pcsg.Spec.Replicas)-minAvailableBreachedPCSGReplicas < int(*pcsg.Spec.MinAvailable) {
		return errPCCGMinAvailableBreached
	}
	// If pcsg.spec.minAvailable is not breached but if there is one more PCSG replica for which there is at least one PCLQ that has
	// its minAvailable breached for a duration > terminationDelay then gang terminate such PCSG replicas.
	if len(syncCtx.pcsgIndicesToTerminate) > 0 {
		logger.Info("Identified PodCliqueScalingGroup indices for gang termination", "indices", syncCtx.pcsgIndicesToTerminate)
		reason := fmt.Sprintf("Delete PodCliques %v for PodCliqueScalingGroup %v which have breached MinAvailable longer than TerminationDelay: %s", syncCtx.pcsgIndicesToTerminate, client.ObjectKeyFromObject(pcsg), syncCtx.pgs.Spec.Template.TerminationDelay.Duration)
		pclqGangTerminationTasks := r.createDeleteTasks(logger, syncCtx.pgs, pcsg.Name, syncCtx.pcsgIndicesToTerminate, reason)
		if err := r.triggerDeletionOfPodCliques(ctx, logger, client.ObjectKeyFromObject(pcsg), pclqGangTerminationTasks); err != nil {
			return err
		}
		return groveerr.New(groveerr.ErrCodeRequeueAfter,
			component.OperationSync,
			fmt.Sprintf("Requeuing post gang termination of PodCliqueScalingGroup replicas: %v", pclqGangTerminationTasks),
		)
	}
	return nil
}

func (r _resource) updatePCLQUpdateProgressForReplica(ctx context.Context, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, updatedPCLQFQNs []string) error {
	pcsgPatch := client.MergeFrom(pcsg.DeepCopy())
	updatedUniquePCLQNames := lo.Uniq(append(pcsg.Status.RollingUpdateProgress.UpdatedPodCliques, updatedPCLQFQNs...))
	slices.Sort(updatedUniquePCLQNames)
	pcsg.Status.RollingUpdateProgress.UpdatedPodCliques = updatedUniquePCLQNames
	if err := r.client.Status().Patch(ctx, pcsg, pcsgPatch); err != nil {
		return groveerr.WrapError(
			err,
			errCodeUpdateStatus,
			component.OperationSync,
			"failed to update the updated PodCliques in the PodCliqueScalingGroup status",
		)
	}
	return nil
}

// func (r _resource) resetUpdateStatus(ctx context.Context, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
// 	if meta.FindStatusCondition(pcsg.Status.Conditions, constants.ConditionTypeUpdateInProgress) != nil {
// 		meta.RemoveStatusCondition(&pcsg.Status.Conditions, constants.ConditionTypeUpdateInProgress)
// 		// The final replica was updated, which caused the rolling update to end
// 		pcsg.Status.RollingUpdateProgress.UpdatedReplicas++
// 		pcsg.Status.RollingUpdateProgress.CurrentlyUpdating = nil
// 		pcsg.Status.RollingUpdateProgress.UpdateEndedAt = ptr.To(metav1.Now())
// 		if err := r.client.Status().Update(ctx, pcsg); err != nil {
// 			return groveerr.WrapError(
// 				err,
// 				errCodeUpdateStatus,
// 				component.OperationSync,
// 				fmt.Sprintf("failed to remove the %s condition in the PodCliqueScalingGroup status", constants.ConditionReasonInsufficientReadyPCSGReplicas),
// 			)
// 		}
// 	}
// 	return nil
// }

// getUpdatedPCLQFQNsForCurrentlyUpdatingIndex returns all the updated PCLQ FQNs of the currently updating PCSG replica, and whether all the PCLQs have been updated or not
func getUpdatedPCLQFQNsForCurrentlyUpdatingIndex(pcsg *grovecorev1alpha1.PodCliqueScalingGroup, expectedPCLQsPerPCSGReplica map[int][]string, existingPCLQs []grovecorev1alpha1.PodClique) ([]string, bool) {
	updatedPCLQFQNs := make([]string, 0, len(expectedPCLQsPerPCSGReplica))
	lastUpdatingReplica := int(pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex)
	pclqToBeCheckedForAvailabilityFQNs := expectedPCLQsPerPCSGReplica[lastUpdatingReplica]
	for _, pclqFQN := range pclqToBeCheckedForAvailabilityFQNs {
		pclqToBeChecked, ok := lo.Find(existingPCLQs, func(pclq grovecorev1alpha1.PodClique) bool {
			return pclqFQN == pclq.Name
		})
		if ok && pclqToBeChecked.Status.ReadyReplicas >= *pclqToBeChecked.Spec.MinAvailable &&
			pcsg.Status.RollingUpdateProgress.PodGangSetGenerationHash == pclqToBeChecked.Labels[apicommon.LabelPodGangSetGenerationHash] {
			updatedPCLQFQNs = append(updatedPCLQFQNs, pclqToBeChecked.Name)
		}
	}
	return updatedPCLQFQNs, len(pclqToBeCheckedForAvailabilityFQNs) == len(updatedPCLQFQNs)
}

// func (r _resource) updatePCSGReplica(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgIndexToUpdate string) error {
// 	if err := r.updatePCSGStatusWithRollingUpdateProgress(ctx, pcsg, pcsgIndexToUpdate); err != nil {
// 		return err
// 	}
// 	logger.Info("triggering deletion of PCSG replica to update", "pcsgIndexToUpdate", pcsgIndexToUpdate)
// 	deletionTasks := r.createDeleteTasks(logger, pgs, pcsg.Name, []string{pcsgIndexToUpdate}, "deleting PCSG replica to perform update")
// 	return r.triggerDeletionOfPodCliques(ctx, logger, client.ObjectKeyFromObject(pcsg), deletionTasks)
// }

// getOrderedPCSGIndicesPendingUpdate sorts the PCSG indices that require an update.
// It will first consider the PCSG indices that have their MinAvailableBreached condition set to true and the TerminationDelay expired.
// It will then consider the PCSG indices that have their MinAvailableBreached condition set to true and the TerminationDelay yet to expire.
// For the remaining PCSG replicas that require an update it will go in the reverse of their ordinal.
// TODO order the non-MinAvailableBreached indices by numPending pods
func getOrderedPCSGIndicesPendingUpdate(pcsgReplicaIndicesPendingUpdate, pcsgIndicesToTerminate, pcsgIndicesToRequeue []string) []string {
	var (
		pcsgTerminationCandidateIndices []string
		pcsgRequeueCandidateIndices     []string
		remainingPCSGReplicas           []string
		orderedPCSGReplicas             = make([]string, 0, len(pcsgReplicaIndicesPendingUpdate))
	)
	// order the PCSG indices in the descending order
	slices.SortFunc(pcsgReplicaIndicesPendingUpdate, func(a, b string) int {
		return cmp.Compare(b, a)
	})

	for _, pcsgReplicaIndex := range pcsgReplicaIndicesPendingUpdate {
		if slices.Contains(pcsgIndicesToTerminate, pcsgReplicaIndex) {
			pcsgTerminationCandidateIndices = append(pcsgTerminationCandidateIndices, pcsgReplicaIndex)
		} else if slices.Contains(pcsgIndicesToRequeue, pcsgReplicaIndex) {
			pcsgRequeueCandidateIndices = append(pcsgRequeueCandidateIndices, pcsgReplicaIndex)
		} else {
			remainingPCSGReplicas = append(remainingPCSGReplicas, pcsgReplicaIndex)
		}
	}

	orderedPCSGReplicas = append(orderedPCSGReplicas, pcsgTerminationCandidateIndices...)
	orderedPCSGReplicas = append(orderedPCSGReplicas, pcsgRequeueCandidateIndices...)

	// For remaining PCSG indices which do not have MinAvailableBreached condition set to true, order them in descending order.
	slices.SortFunc(remainingPCSGReplicas, func(a, b string) int {
		return cmp.Compare(b, a)
	})

	orderedPCSGReplicas = append(orderedPCSGReplicas, remainingPCSGReplicas...)
	return orderedPCSGReplicas
}

// func (r _resource) updatePCSGStatusWithRollingUpdateProgress(ctx context.Context, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, replicaIndex string) error {
// 	index, err := strconv.Atoi(replicaIndex)
// 	if err != nil {
// 		return groveerr.WrapError(err,
// 			errCodeParsePodCliqueScalingGroupReplicaIndex,
// 			component.OperationSync,
// 			fmt.Sprintf("invalid pcsg replica index: %s", replicaIndex),
// 		)
// 	}
// 	// Increase updated replicas if a previous CurrentlyUpdating replica has finished update.
// 	if pcsg.Status.RollingUpdateProgress.CurrentlyUpdating != nil && index != int(pcsg.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex) {
// 		pcsg.Status.RollingUpdateProgress.UpdatedReplicas++
// 	}
// 	pcsg.Status.RollingUpdateProgress.CurrentlyUpdating = &grovecorev1alpha1.PodCliqueScalingGroupReplicaRollingUpdateProgress{
// 		ReplicaIndex:    int32(index),
// 		UpdateStartedAt: metav1.Now(),
// 	}
// 	if !componentutils.IsPCSGUpdateInProgress(pcsg) {
// 		if pcsg.Status.Conditions == nil {
// 			pcsg.Status.Conditions = []metav1.Condition{}
// 		}
// 		pcsg.Status.Conditions = append(pcsg.Status.Conditions, metav1.Condition{
// 			Type:               constants.ConditionTypeUpdateInProgress,
// 			Status:             metav1.ConditionTrue,
// 			Reason:             constants.ConditionReasonUpdateInProgress,
// 			LastTransitionTime: metav1.Now(),
// 			Message:            "At least one of the constituent PodClique templates have been updated",
// 		})
// 	}
// 	if err := r.client.Status().Update(ctx, pcsg); err != nil {
// 		return groveerr.WrapError(err,
// 			errCodeUpdateStatus,
// 			component.OperationSync,
// 			fmt.Sprintf("could not set %s condition on PCSG: %v", constants.ConditionTypeUpdateInProgress, client.ObjectKeyFromObject(pcsg)),
// 		)
// 	}
// 	return nil
// }

func getPCSGReplicaIndicesPendingUpdate(pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, existingPCLQS []grovecorev1alpha1.PodClique) []string {
	pclqFQNToHash := make(map[string]string)
	pcsgPCLQNames := pcsg.Spec.CliqueNames
	for _, pcsgCliqueName := range pcsgPCLQNames {
		pclqTemplateSpec, ok := lo.Find(pgs.Spec.Template.Cliques, func(pclqTemplateSpec *grovecorev1alpha1.PodCliqueTemplateSpec) bool {
			return pclqTemplateSpec.Name == pcsgCliqueName
		})
		if !ok {
			continue
		}
		podTemplateHash := componentutils.GetPCLQPodTemplateHash(pclqTemplateSpec, pgs.Spec.Template.PriorityClassName)
		for pcsgReplicaIndex := range int(pcsg.Spec.Replicas) {
			cliqueFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{
				Name:    pcsg.Name,
				Replica: pcsgReplicaIndex,
			}, pcsgCliqueName)
			pclqFQNToHash[cliqueFQN] = podTemplateHash
		}
	}

	pcsgReplicaIndicesPendingUpdate := make([]string, 0, pcsg.Spec.Replicas)
	for _, pclq := range existingPCLQS {
		if pclq.Labels[apicommon.LabelPodTemplateHash] != pclqFQNToHash[pclq.Name] {
			pcsgReplicaIndex, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
			if !ok {
				continue
			}
			pcsgReplicaIndicesPendingUpdate = append(pcsgReplicaIndicesPendingUpdate, pcsgReplicaIndex)
		}
	}
	return lo.Uniq(pcsgReplicaIndicesPendingUpdate)
}

func (r _resource) getExistingPCLQs(ctx context.Context, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) ([]grovecorev1alpha1.PodClique, error) {
	existingPCLQs, err := componentutils.GetPCLQsByOwner(ctx, r.client, constants.KindPodCliqueScalingGroup, client.ObjectKeyFromObject(pcsg), getPodCliqueSelectorLabels(pcsg.ObjectMeta))
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodCliquesForPCSG,
			component.OperationSync,
			fmt.Sprintf("Unable to fetch existing PodCliques for PodCliqueScalingGroup: %v", client.ObjectKeyFromObject(pcsg)),
		)
	}
	return existingPCLQs, nil
}

func (r _resource) triggerDeletionOfExcessPCSGReplicas(ctx context.Context, logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
	existingPCSGReplicas := getExistingNonTerminatingPCSGReplicas(syncCtx.existingPCLQs)
	// Check if the number of existing PodCliques is greater than expected, if so, we need to delete the extra ones.
	diff := existingPCSGReplicas - int(pcsg.Spec.Replicas)
	if diff > 0 {
		pcsgObjectKey := client.ObjectKeyFromObject(pcsg)
		logger.Info("Found more PodCliques than expected, triggering deletion of excess PodCliques", "expected", int(pcsg.Spec.Replicas), "existing", existingPCSGReplicas, "diff", diff)
		reason := "Delete excess PodCliqueScalingGroup replicas"
		replicaIndicesToDelete := computePCSGReplicasToDelete(existingPCSGReplicas, int(pcsg.Spec.Replicas))
		deletionTasks := r.createDeleteTasks(logger, syncCtx.pgs, pcsgObjectKey.Name, replicaIndicesToDelete, reason)
		if err := r.triggerDeletionOfPodCliques(ctx, logger, pcsgObjectKey, deletionTasks); err != nil {
			return err
		}
		return syncCtx.refreshExistingPCLQs(pcsg)
	}
	return nil
}

func getExistingNonTerminatingPCSGReplicas(existingPCLQs []grovecorev1alpha1.PodClique) int {
	existingIndices := make([]string, 0, len(existingPCLQs))
	for _, pclq := range existingPCLQs {
		if k8sutils.IsResourceTerminating(pclq.ObjectMeta) {
			continue
		}
		pcsgReplicaIndex, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
		if !ok {
			continue
		}
		existingIndices = append(existingIndices, pcsgReplicaIndex)
	}
	return len(lo.Uniq(existingIndices))
}

func computePCSGReplicasToDelete(existingReplicas, expectedReplicas int) []string {
	indices := make([]string, 0, existingReplicas-expectedReplicas)
	for i := expectedReplicas; i < existingReplicas; i++ {
		indices = append(indices, strconv.Itoa(i))
	}
	return indices
}

func getMinAvailableBreachedPCSGIndices(logger logr.Logger, existingPCLQs []grovecorev1alpha1.PodClique, terminationDelay time.Duration) (pcsgIndicesToTerminate []string, pcsgIndicesToRequeue []string) {
	now := time.Now()
	// group existing PCLQs by PCSG replica index. These are PCLQs that belong to once replica of PCSG.
	pcsgReplicaIndexPCLQs := componentutils.GroupPCLQsByPCSGReplicaIndex(existingPCLQs)
	// For each PCSG replica check if minAvailable for any constituent PCLQ has been violated. Those PCSG replicas should be marked for termination.
	for pcsgReplicaIndex, pclqs := range pcsgReplicaIndexPCLQs {
		pclqNames, minWaitFor := componentutils.GetMinAvailableBreachedPCLQInfo(pclqs, terminationDelay, now)
		if len(pclqNames) > 0 {
			logger.Info("minAvailable breached for PCLQs", "pcsgReplicaIndex", pcsgReplicaIndex, "pclqNames", pclqNames, "minWaitFor", minWaitFor)
			if minWaitFor <= 0 {
				pcsgIndicesToTerminate = append(pcsgIndicesToTerminate, pcsgReplicaIndex)
			} else {
				pcsgIndicesToRequeue = append(pcsgIndicesToRequeue, pcsgReplicaIndex)
			}
		}
	}
	return
}

func (r _resource) createOrUpdateExpectedPCLQs(ctx context.Context, logger logr.Logger, syncCtx *syncContext, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
	var tasks []utils.Task
	for pcsgReplicaIndex, expectedPCLQNames := range syncCtx.expectedPCLQFQNsPerPCSGReplica {
		for _, pclqFQN := range expectedPCLQNames {
			pclqObjectKey := client.ObjectKey{
				Name:      pclqFQN,
				Namespace: pcsg.Namespace,
			}
			createTask := utils.Task{
				Name: fmt.Sprintf("CreateOrUpdatePodClique-%s", pclqObjectKey),
				Fn: func(ctx context.Context) error {
					return r.doCreateOrUpdate(ctx, logger, syncCtx.pgs, pcsg, pcsgReplicaIndex, pclqObjectKey)
				},
			}
			tasks = append(tasks, createTask)
		}
	}
	if runResult := utils.RunConcurrently(ctx, logger, tasks); runResult.HasErrors() {
		return groveerr.WrapError(runResult.GetAggregatedError(),
			errCodeCreatePodCliques,
			component.OperationSync,
			fmt.Sprintf("Error Create of PodCliques for PodCliqueScalingGroup: %v, run summary: %s", client.ObjectKeyFromObject(pcsg), runResult.GetSummary()),
		)
	}
	return nil
}

func (r _resource) triggerDeletionOfPodCliques(ctx context.Context, logger logr.Logger, pcsgObjectKey client.ObjectKey, deletionTasks []utils.Task) error {
	if runResult := utils.RunConcurrently(ctx, logger, deletionTasks); runResult.HasErrors() {
		return groveerr.WrapError(runResult.GetAggregatedError(),
			errCodeDeletePodClique,
			component.OperationSync,
			fmt.Sprintf("Error deleting PodCliques for PodCliqueScalingGroup: %v", pcsgObjectKey),
		)
	}
	logger.Info("Deleted PodCliques of PodCliqueScalingGroup", "pcsgObjectKey", pcsgObjectKey)
	return nil
}

func (r _resource) createDeleteTasks(logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, pcsgName string, pcsgReplicasToDelete []string, reason string) []utils.Task {
	deletionTasks := make([]utils.Task, 0, len(pcsgReplicasToDelete))
	for _, pcsgReplicaIndex := range pcsgReplicasToDelete {
		task := utils.Task{
			Name: "DeletePCSGReplicaPodCliques-" + pcsgReplicaIndex,
			Fn: func(ctx context.Context) error {
				if err := r.client.DeleteAllOf(ctx,
					&grovecorev1alpha1.PodClique{},
					client.InNamespace(pgs.Namespace),
					client.MatchingLabels(getLabelsToDeletePCSGReplicaIndexPCLQs(pgs.Name, pcsgName, pcsgReplicaIndex))); err != nil {
					r.eventRecorder.Eventf(pgs, corev1.EventTypeWarning, groveevents.ReasonPodCliqueScalingGroupReplicaDeletionFailed, "Error deleting PodCliqueScalingGroup %s ReplicaIndex %s : %v", pcsgName, pcsgReplicaIndex, err)
					logger.Error(err, "failed to delete PodCliques for PCSG replica index", "pcsgReplicaIndex", pcsgReplicaIndex, "reason", reason)
					return err
				}
				logger.Info("Deleting PodCliqueScalingGroup replica", "pcsgName", pcsgName, "pcsgReplicaIndex", pcsgReplicaIndex)
				r.eventRecorder.Eventf(pgs, corev1.EventTypeNormal, groveevents.ReasonPodCliqueScalingGroupReplicaDeletionSuccessful, "Deleted PodCliqueScalingGroup %s replicaIndex: %s", pcsgName, pcsgReplicaIndex)
				return nil
			},
		}
		deletionTasks = append(deletionTasks, task)
	}
	return deletionTasks
}

func getLabelsToDeletePCSGReplicaIndexPCLQs(pgsName, pcsgName, pcsgReplicaIndex string) map[string]string {
	return lo.Assign(
		apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgsName),
		map[string]string{
			apicommon.LabelComponentKey:                      apicommon.LabelComponentNamePodCliqueScalingGroupPodClique,
			apicommon.LabelPodCliqueScalingGroup:             pcsgName,
			apicommon.LabelPodCliqueScalingGroupReplicaIndex: pcsgReplicaIndex,
		},
	)
}

// Delete deletes all resources that the PodClique Operator manages.
func (r _resource) Delete(ctx context.Context, logger logr.Logger, pcsgObjectMeta metav1.ObjectMeta) error {
	logger.Info("Triggering deletion of PodCliques managed by PodCliqueScalingGroup")
	existingPCLQNames, err := r.GetExistingResourceNames(ctx, logger, pcsgObjectMeta)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeListPodClique,
			component.OperationDelete,
			fmt.Sprintf("Unable to fetch existing PodClique names for PodCliqueScalingGroup: %v", k8sutils.GetObjectKeyFromObjectMeta(pcsgObjectMeta)),
		)
	}
	deleteTasks := make([]utils.Task, 0, len(existingPCLQNames))
	for _, pclqName := range existingPCLQNames {
		pclqObjectKey := client.ObjectKey{Name: pclqName, Namespace: pcsgObjectMeta.Namespace}
		task := utils.Task{
			Name: "DeletePodClique-" + pclqName,
			Fn: func(ctx context.Context) error {
				if err := client.IgnoreNotFound(r.client.Delete(ctx, emptyPodClique(pclqObjectKey))); err != nil {
					return groveerr.WrapError(err,
						errCodeDeletePodClique,
						component.OperationDelete,
						fmt.Sprintf("Failed to delete PodClique: %v for PodCliqueScalingGroup: %v", pclqObjectKey, k8sutils.GetObjectKeyFromObjectMeta(pcsgObjectMeta)),
					)
				}
				return nil
			},
		}
		deleteTasks = append(deleteTasks, task)
	}
	if runResult := utils.RunConcurrently(ctx, logger, deleteTasks); runResult.HasErrors() {
		logger.Error(runResult.GetAggregatedError(), "Error deleting PodCliques", "run summary", runResult.GetSummary())
		return groveerr.WrapError(runResult.GetAggregatedError(),
			errCodeDeletePodClique,
			component.OperationDelete,
			fmt.Sprintf("Error deleting PodCliques for PodCliqueScalingGroup: %v", k8sutils.GetObjectKeyFromObjectMeta(pcsgObjectMeta)),
		)
	}

	logger.Info("Deleted PodCliques belonging to PodCliqueScalingGroup")
	return nil
}

func (r _resource) getPCSGTemplateNumPods(pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) int {
	var pcsgTemplateNumPods int
	pcMap := make(map[string]*grovecorev1alpha1.PodCliqueTemplateSpec, len(pgs.Spec.Template.Cliques))
	for _, pclqTemplateSpec := range pgs.Spec.Template.Cliques {
		pcMap[pclqTemplateSpec.Name] = pclqTemplateSpec
	}
	for _, pclqTemplateName := range pcsg.Spec.CliqueNames {
		pclqTemplateSpec, ok := pcMap[pclqTemplateName]
		if !ok {
			continue
		}
		pcsgTemplateNumPods += int(pclqTemplateSpec.Spec.Replicas)
	}
	return pcsgTemplateNumPods
}

func (r _resource) doCreateOrUpdate(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex int, pclqObjectKey client.ObjectKey) error {
	logger.Info("Running CreateOrUpdate PodClique", "pclqObjectKey", pclqObjectKey)
	pclq := emptyPodClique(pclqObjectKey)
	pcsgObjKey := client.ObjectKeyFromObject(pclq)

	if _, err := controllerutil.CreateOrPatch(ctx, r.client, pclq, func() error {
		if err := r.buildResource(logger, pgs, pcsg, pcsgReplicaIndex, pclq); err != nil {
			r.eventRecorder.Eventf(pcsg, corev1.EventTypeWarning, groveevents.ReasonPodCliqueCreationOrUpdationFailed, "PodClique %v creation or updation failed: %v", pclqObjectKey, err)
			return groveerr.WrapError(err,
				errCodeCreatePodClique,
				component.OperationSync,
				fmt.Sprintf("Error creating or updating PodClique: %v for PodCliqueScalingGroup: %v", pclqObjectKey, pcsgObjKey),
			)
		}
		return nil
	}); err != nil {
		return err
	}

	r.eventRecorder.Eventf(pcsg, corev1.EventTypeNormal, groveevents.ReasonPodCliqueCreationSuccessful, "PodClique %v created successfully", pclqObjectKey)
	logger.Info("Successfully created or updated PodClique", "pclqObjectKey", pclqObjectKey)
	return nil
}

func (r _resource) buildResource(logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex int, pclq *grovecorev1alpha1.PodClique) error {
	var err error
	pclqObjectKey, pgsObjectKey := client.ObjectKeyFromObject(pclq), client.ObjectKeyFromObject(pgs)
	pclqTemplateSpec, foundAtIndex, ok := lo.FindIndexOf(pgs.Spec.Template.Cliques, func(pclqTemplateSpec *grovecorev1alpha1.PodCliqueTemplateSpec) bool {
		return strings.HasSuffix(pclq.Name, pclqTemplateSpec.Name)
	})
	if !ok {
		logger.Info("Error building PodClique resource, PodClique template spec not found in PodGangSet", "podCliqueObjectKey", pclqObjectKey, "podGangSetObjectKey", pgsObjectKey)
		return groveerr.New(errCodeBuildPodClique,
			component.OperationSync,
			fmt.Sprintf("Error building PodClique resource, PodCliqueTemplateSpec for PodClique: %v not found in PodGangSet: %v", pclqObjectKey, pgsObjectKey),
		)
	}
	// Set PodClique.ObjectMeta
	// ------------------------------------
	if err = controllerutil.SetControllerReference(pcsg, pclq, r.scheme); err != nil {
		return groveerr.WrapError(err,
			errCodeSetPodCliqueOwnerReference,
			component.OperationSync,
			fmt.Sprintf("Error setting controller reference for PodClique: %v", client.ObjectKeyFromObject(pclq)),
		)
	}

	pgsReplicaIndex, err := getPGSReplicaFromPCSG(pcsg)
	if err != nil {
		return err
	}

	podGangName := apicommon.GeneratePodGangNameForPodCliqueOwnedByPCSG(pgs, pgsReplicaIndex, pcsg, pcsgReplicaIndex)

	pclq.Labels = getLabels(pgs, pgsReplicaIndex, pcsg, pcsgReplicaIndex, pclqObjectKey, pclqTemplateSpec, podGangName)
	pclq.Annotations = pclqTemplateSpec.Annotations
	// set PodCliqueSpec
	// ------------------------------------
	pclq.Spec = *pclqTemplateSpec.Spec.DeepCopy()
	pcsgTemplateNumPods := r.getPCSGTemplateNumPods(pgs, pcsg)
	r.addEnvironmentVariablesToPodContainerSpecs(pclq, pcsgTemplateNumPods)
	dependentPCLQNames, err := identifyFullyQualifiedStartupDependencyNames(pgs, pgsReplicaIndex, pcsg, pcsgReplicaIndex, pclq, foundAtIndex)
	if err != nil {
		return err
	}
	pclq.Spec.StartsAfter = dependentPCLQNames
	return nil
}

func (r _resource) addEnvironmentVariablesToPodContainerSpecs(pclq *grovecorev1alpha1.PodClique, pcsgTemplateNumPods int) {
	pcsgEnvVars := []corev1.EnvVar{
		{
			Name: constants.EnvVarPCSGName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: fmt.Sprintf("metadata.labels['%s']", apicommon.LabelPodCliqueScalingGroup),
				},
			},
		},
		{
			Name:  constants.EnvVarPCSGTemplateNumPods,
			Value: strconv.Itoa(pcsgTemplateNumPods),
		},
		{
			Name: constants.EnvVarPCSGIndex,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: fmt.Sprintf("metadata.labels['%s']", apicommon.LabelPodCliqueScalingGroupReplicaIndex),
				},
			},
		},
	}
	pclqObjPodSpec := &pclq.Spec.PodSpec
	componentutils.AddEnvVarsToContainers(pclqObjPodSpec.Containers, pcsgEnvVars)
	componentutils.AddEnvVarsToContainers(pclqObjPodSpec.InitContainers, pcsgEnvVars)
}

// getExpectedPodCliqueFQNsByPCSGReplica computes expected PCLQ names per expected PCSG replica.
// It returns a map with the key being the PCSG replica index and the value is the expected PCLQ FQNs for that replica. In addition
// it also returns the total number of expected PCLQs.
func getExpectedPodCliqueFQNsByPCSGReplica(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) map[int][]string {
	var (
		expectedPCLQFQNs = make(map[int][]string)
	)
	for pcsgReplicaIndex := range int(pcsg.Spec.Replicas) {
		pclqFQNs := lo.Map(pcsg.Spec.CliqueNames, func(cliqueName string, _ int) string {
			return apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{
				Name:    pcsg.Name,
				Replica: pcsgReplicaIndex,
			}, cliqueName)
		})
		expectedPCLQFQNs[pcsgReplicaIndex] = pclqFQNs
	}
	return expectedPCLQFQNs
}

func getPGSReplicaFromPCSG(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) (int, error) {
	pgsReplicaIndex, ok := pcsg.GetLabels()[apicommon.LabelPodGangSetReplicaIndex]
	if !ok {
		return 0, groveerr.New(errCodeMissingPGSReplicaIndex, component.OperationSync, fmt.Sprintf("failed to get the PodGangSet replica ind value from the labels for PodCliqueScalingGroup %s", client.ObjectKeyFromObject(pcsg)))
	}
	pgsReplica, err := strconv.Atoi(pgsReplicaIndex)
	if err != nil {
		return 0, groveerr.WrapError(err,
			errCodeReplicaIndexIntConversion,
			component.OperationSync,
			"failed to convert replica index value from string to integer",
		)
	}
	return pgsReplica, nil
}

func identifyFullyQualifiedStartupDependencyNames(pgs *grovecorev1alpha1.PodGangSet, pgsReplicaIndex int, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex int, pclq *grovecorev1alpha1.PodClique, foundAtIndex int) ([]string, error) {
	cliqueStartupType := pgs.Spec.Template.StartupType
	if cliqueStartupType == nil {
		// Ideally this should never happen as the defaulting webhook should set it v1alpha1.CliqueStartupTypeInOrder as the default value.
		// If it is still nil, then by not returning an error we break the API contract. It is a bug that should be fixed.
		return nil, groveerr.New(errCodeMissingStartupType, component.OperationSync, fmt.Sprintf("PodClique: %v has nil StartupType", client.ObjectKeyFromObject(pclq)))
	}
	switch *cliqueStartupType {
	case grovecorev1alpha1.CliqueStartupTypeInOrder:
		return getInOrderStartupDependencies(pgs, pgsReplicaIndex, pcsg, pcsgReplicaIndex, foundAtIndex), nil
	case grovecorev1alpha1.CliqueStartupTypeExplicit:
		return getExplicitStartupDependencies(pgs, pgsReplicaIndex, pcsg, pcsgReplicaIndex, pclq), nil
	default:
		return nil, nil
	}
}

func getInOrderStartupDependencies(pgs *grovecorev1alpha1.PodGangSet, pgsReplicaIndex int, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex, foundAtIndex int) []string {
	if foundAtIndex == 0 {
		return nil
	}
	parentCliqueName := pgs.Spec.Template.Cliques[foundAtIndex-1].Name

	// Current pcsgReplicaIndex belongs to the base PodGang
	if pcsgReplicaIndex < int(*pcsg.Spec.MinAvailable) {
		return componentutils.GenerateDependencyNamesForBasePodGang(pgs, pgsReplicaIndex, parentCliqueName)
	}

	// Startup ordering is only enforced within a PodGang.
	// PodCliques that belong to the base PodGang are not considered for startsAfter in scaled PodGangs.
	if !slices.Contains(pcsg.Spec.CliqueNames, parentCliqueName) {
		return nil
	}

	return []string{
		apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsg.Name, Replica: pcsgReplicaIndex}, parentCliqueName),
	}
}

func getExplicitStartupDependencies(pgs *grovecorev1alpha1.PodGangSet, pgsReplicaIndex int, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex int, pclq *grovecorev1alpha1.PodClique) []string {
	parentCliqueNames := make([]string, 0, len(pclq.Spec.StartsAfter))
	// Current pcsgReplicaIndex belongs to the base PodGang
	if pcsgReplicaIndex < int(*pcsg.Spec.MinAvailable) {
		for _, dependency := range pclq.Spec.StartsAfter {
			parentCliqueNames = append(parentCliqueNames, componentutils.GenerateDependencyNamesForBasePodGang(pgs, pgsReplicaIndex, dependency)...)
		}
		return parentCliqueNames
	}

	for _, dependency := range pclq.Spec.StartsAfter {
		// Startup ordering is only enforced within the scaled PodCliqueScalingGroup's corresponding PodGang.
		if !slices.Contains(pcsg.Spec.CliqueNames, dependency) {
			continue
		}
		parentCliqueNames = append(parentCliqueNames, apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsg.Name, Replica: pcsgReplicaIndex}, dependency))
	}
	return parentCliqueNames
}

func getPodCliqueSelectorLabels(pcsgObjectMeta metav1.ObjectMeta) map[string]string {
	pgsName := componentutils.GetPodGangSetName(pcsgObjectMeta)
	return lo.Assign(
		apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgsName),
		map[string]string{
			apicommon.LabelComponentKey:          apicommon.LabelComponentNamePodCliqueScalingGroupPodClique,
			apicommon.LabelPodCliqueScalingGroup: pcsgObjectMeta.Name,
		},
	)
}

func getLabels(pgs *grovecorev1alpha1.PodGangSet, pgsReplicaIndex int, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex int, pclqObjectKey client.ObjectKey, pclqTemplateSpec *grovecorev1alpha1.PodCliqueTemplateSpec, podGangName string) map[string]string {
	pclqComponentLabels := map[string]string{
		apicommon.LabelAppNameKey:                        pclqObjectKey.Name,
		apicommon.LabelComponentKey:                      apicommon.LabelComponentNamePodCliqueScalingGroupPodClique,
		apicommon.LabelPodCliqueScalingGroup:             pcsg.Name,
		apicommon.LabelPodGang:                           podGangName,
		apicommon.LabelPodGangSetReplicaIndex:            strconv.Itoa(pgsReplicaIndex),
		apicommon.LabelPodCliqueScalingGroupReplicaIndex: strconv.Itoa(pcsgReplicaIndex),
		apicommon.LabelPodTemplateHash:                   componentutils.GetPCLQPodTemplateHash(pclqTemplateSpec, pgs.Spec.Template.PriorityClassName),
		apicommon.LabelPodGangSetGenerationHash:          *pgs.Status.GenerationHash,
	}

	// Add base-podgang label for scaled PodGang pods (beyond minAvailable)
	basePodGangName := apicommon.GenerateBasePodGangName(
		apicommon.ResourceNameReplica{Name: pgs.Name, Replica: pgsReplicaIndex},
	)
	if podGangName != basePodGangName {
		// This pod belongs to a scaled PodGang - add the base PodGang label
		pclqComponentLabels[apicommon.LabelBasePodGang] = basePodGangName
	}

	return lo.Assign(
		pclqTemplateSpec.Labels,
		apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgs.Name),
		pclqComponentLabels,
	)
}

func emptyPodClique(objKey client.ObjectKey) *grovecorev1alpha1.PodClique {
	return &grovecorev1alpha1.PodClique{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objKey.Name,
			Namespace: objKey.Namespace,
		},
	}
}
