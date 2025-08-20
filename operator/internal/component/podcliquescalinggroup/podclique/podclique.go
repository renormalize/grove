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
	"context"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	errCodeListPodClique                   grovecorev1alpha1.ErrorCode = "ERR_LIST_PODCLIQUE"
	errCodeMissingStartupType              grovecorev1alpha1.ErrorCode = "ERR_UNDEFINED_STARTUP_TYPE"
	errCodeSetPodCliqueOwnerReference      grovecorev1alpha1.ErrorCode = "ERR_SET_PODCLIQUE_OWNER_REFERENCE"
	errCodeBuildPodClique                  grovecorev1alpha1.ErrorCode = "ERR_BUILD_PODCLIQUE"
	errCodeCreatePodCliques                grovecorev1alpha1.ErrorCode = "ERR_CREATE_PODCLIQUES"
	errCodeDeletePodClique                 grovecorev1alpha1.ErrorCode = "ERR_DELETE_PODCLIQUE"
	errCodeGetPodGangSet                   grovecorev1alpha1.ErrorCode = "ERR_GET_PODGANGSET"
	errCodeMissingPGSReplicaIndex          grovecorev1alpha1.ErrorCode = "ERR_MISSING_PODGANGSET_REPLICA_INDEX"
	errCodeReplicaIndexIntConversion       grovecorev1alpha1.ErrorCode = "ERR_PODGANGSET_REPLICA_INDEX_CONVERSION"
	errCodeListPodCliquesForPCSG           grovecorev1alpha1.ErrorCode = "ERR_LIST_PODCLIQUE_FOR_PCSG"
	errCodeCreatePodClique                 grovecorev1alpha1.ErrorCode = "ERR_CREATE_PODCLIQUE"
	errCodeParsePodCliqueScalingGroupIndex grovecorev1alpha1.ErrorCode = "ERR_PARSE_PODCLIQUESCALINGGROUP_REPLICA_INDEX"
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

/*
	reconcileSpec:
		* Sync the components.
			* Check if there was a scale-in, if yes delete any pending excess PCLQs
			* Check if minAvailable is breached for at least MinAvailable replicas. If yes skip any other work.
			* Check if there was a scale-out, if yes create additional PCLQs.
			* Check if there are pending updates. If there, then skip the updates if the above condition is true, else:
				* Update Status of PCSG and set update-in-progress to true.
				* Order PCSG replicas by health
				* Update PCSG replicas by re-creating one PCSG replica at a time.
*/

// Sync synchronizes all resources that the PodClique Operator manages.
func (r _resource) Sync(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) error {
	pgs, err := componentutils.GetOwnerPodGangSet(ctx, r.client, pcsg.ObjectMeta)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeGetPodGangSet,
			component.OperationSync,
			fmt.Sprintf("failed to get owner PodGangSet for PodCliqueScalingGroup %s", client.ObjectKeyFromObject(pcsg)),
		)
	}

	expectedPCLQsPerPCSGReplica, expectedPCSGReplicas := getExpectedPodCliqueFQNsByPCSGReplica(pcsg)
	existingPCLQs, err := r.getExistingPCLQs(ctx, pcsg)
	if err != nil {
		return err
	}

	existingPCSGReplicas := getExistingPCSGReplicas(existingPCLQs)
	// If there are excess PodCliques than expected, delete the ones that are no longer expected but existing.
	// This can happen when PCSG replicas have been scaled-in.
	if err = r.triggerDeletionOfExcessPCSGReplicas(ctx, logger, pgs, pcsg, existingPCSGReplicas, expectedPCSGReplicas); err != nil {
		return err
	}

	existingPCLQs, err = refreshExistingPCLQs(existingPCLQs, expectedPCSGReplicas)
	terminationDelay := pgs.Spec.Template.TerminationDelay.Duration
	pcsgIndicesToTerminate, pcsgIndicesToRequeue := getMinAvailableBreachedPCSGIndices(logger, existingPCLQs, terminationDelay)
	minAvailableBreachedPCSGReplicas := len(pcsgIndicesToTerminate) + len(pcsgIndicesToRequeue)
	if int(pcsg.Spec.Replicas)-minAvailableBreachedPCSGReplicas < int(*pcsg.Spec.MinAvailable) {
		logger.Info("Skipping further reconciliation as MinAvailable for the PCSG has been breached. This can potentially trigger PGS replica deletion.")
		return nil
	}

	// TODO check if there is a pending update that needs to be done to all constituent PCLQs
	if isUpdateNeeded(pgs, pcsg, existingPCLQs) {
		logger.Info("Update for PodCliqueScalingGroup PodCliques necessary.")
	}

	if len(pcsgIndicesToTerminate) > 0 {
		reason := fmt.Sprintf("Delete PodCliques %v for PodCliqueScalingGroup %v which have breached MinAvailable longer than TerminationDelay: %s", pcsgIndicesToTerminate, client.ObjectKeyFromObject(pcsg), terminationDelay)
		pclqGangTerminationTasks := r.createDeleteTasks(logger, pgs, pcsg.Name, pcsgIndicesToTerminate, reason)
		if err = r.triggerDeletionOfPodCliques(ctx, logger, client.ObjectKeyFromObject(pcsg), pclqGangTerminationTasks); err != nil {
			return err
		}
	}

	// Create or update the expected PodCliques as per the PodCliqueScalingGroup configurations defined in the PodGangSet.
	existingPCLQFQNs := lo.Map(existingPCLQs, func(pclq grovecorev1alpha1.PodClique, _ int) string { return pclq.Name })
	if err = r.createExpectedPCLQs(ctx, logger, pgs, pcsg, existingPCLQFQNs, expectedPCLQsPerPCSGReplica); err != nil {
		return err
	}

	if len(pcsgIndicesToRequeue) > 0 {
		return groveerr.New(groveerr.ErrCodeRequeueAfter,
			component.OperationSync,
			"Requeuing to re-process PCLQs that have breached MinAvailable but not crossed TerminationDelay",
		)
	}

	return nil
}

func isUpdateNeeded(pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, existingPCLQS []grovecorev1alpha1.PodClique) bool {
	pclqFQNToHash := make(map[string]string)
	pcsgPCLQNames := pcsg.Spec.CliqueNames
	for _, pcsgCliqueName := range pcsgPCLQNames {
		pclqTemplateSpec, ok := lo.Find(pgs.Spec.Template.Cliques, func(pclqTemplateSpec *grovecorev1alpha1.PodCliqueTemplateSpec) bool {
			return pclqTemplateSpec.Name == pcsgCliqueName
		})
		if !ok {
			continue
		}
		podTemplateHash := componentutils.GetPCLQPodTemplateHashLabel(pclqTemplateSpec, pgs.Spec.Template.PriorityClassName)
		for pcsgReplicaIndex := range int(pcsg.Spec.Replicas) {
			cliqueFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{
				Name:    pcsg.Name,
				Replica: int(pcsgReplicaIndex),
			}, pcsgCliqueName)
			pclqFQNToHash[cliqueFQN] = podTemplateHash
			fmt.Printf("PodTemplateHash for podClique %s: %s\n", cliqueFQN, podTemplateHash)
		}
	}

	for _, pclq := range existingPCLQS {
		if pclq.GetLabels()[apicommon.LabelPodTemplateHash] != pclqFQNToHash[pclq.Name] {
			return true
		}
	}
	return false
}

func (r _resource) getExistingPCLQs(ctx context.Context, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) ([]grovecorev1alpha1.PodClique, error) {
	existingPCLQs, err := componentutils.GetPCLQsByOwner(ctx, r.client, grovecorev1alpha1.KindPodCliqueScalingGroup, client.ObjectKeyFromObject(pcsg), getPodCliqueSelectorLabels(pcsg.ObjectMeta))
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodCliquesForPCSG,
			component.OperationSync,
			fmt.Sprintf("Unable to fetch existing PodCliques for PodCliqueScalingGroup: %v", client.ObjectKeyFromObject(pcsg)),
		)
	}
	return existingPCLQs, nil
}

func getExistingPCSGReplicas(existingPCLQs []grovecorev1alpha1.PodClique) int {
	existingIndices := make([]string, 0, len(existingPCLQs))
	for _, pclq := range existingPCLQs {
		pcsgReplicaIndex, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
		if !ok {
			continue
		}
		existingIndices = append(existingIndices, pcsgReplicaIndex)
	}
	return len(lo.Uniq(existingIndices))
}

func (r _resource) triggerDeletionOfExcessPCSGReplicas(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, existingPCSGReplicas int, expectedPCSGReplicas int) error {
	// Check if the number of existing PodCliques is greater than expected, if so, we need to delete the extra ones.
	diff := existingPCSGReplicas - expectedPCSGReplicas
	if diff > 0 {
		logger.Info("Found more PodCliques than expected, triggering deletion of excess PodCliques", "expected", expectedPCSGReplicas, "existing", existingPCSGReplicas, "diff", diff)
		reason := "Delete excess PodCliqueScalingGroup replicas"
		replicaIndicesToDelete := computePCSGReplicasToDelete(existingPCSGReplicas, expectedPCSGReplicas)
		deletionTasks := r.createDeleteTasks(logger, pgs, pcsg.Name, replicaIndicesToDelete, reason)
		return r.triggerDeletionOfPodCliques(ctx, logger, client.ObjectKeyFromObject(pcsg), deletionTasks)
	}
	return nil
}

func computePCSGReplicasToDelete(existingReplicas, expectedReplicas int) []string {
	indices := make([]string, 0, expectedReplicas-existingReplicas)
	for i := expectedReplicas; i < existingReplicas; i++ {
		indices = append(indices, strconv.Itoa(i))
	}
	return indices
}

func refreshExistingPCLQs(existingPCLQs []grovecorev1alpha1.PodClique, expectedPCSGReplicas int) ([]grovecorev1alpha1.PodClique, error) {
	revisedExistingPCLQs := make([]grovecorev1alpha1.PodClique, 0, len(existingPCLQs))
	for _, pclq := range existingPCLQs {
		pcsgReplicaIndexStr, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
		if !ok {
			continue
		}
		pcsgReplicaIndex, err := strconv.Atoi(pcsgReplicaIndexStr)
		if err != nil {
			return nil, groveerr.WrapError(err,
				errCodeParsePodCliqueScalingGroupIndex,
				component.OperationSync,
				fmt.Sprintf("invalid pcsg replica index label value found on PodClique: %v", client.ObjectKeyFromObject(&pclq)),
			)
		}
		if pcsgReplicaIndex < expectedPCSGReplicas {
			revisedExistingPCLQs = append(revisedExistingPCLQs, pclq)
		}
	}
	return revisedExistingPCLQs, nil
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

func (r _resource) createExpectedPCLQs(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, existingPCLQNames []string, expectedPCLQFQNsPerPCSGReplica map[int][]string) error {
	var tasks []utils.Task
	for pcsgReplicaIndex, expectedPCLQNames := range expectedPCLQFQNsPerPCSGReplica {
		for _, pclqFQN := range expectedPCLQNames {
			if slices.Contains(existingPCLQNames, pclqFQN) {
				continue
			}
			pclqObjectKey := client.ObjectKey{
				Name:      pclqFQN,
				Namespace: pcsg.Namespace,
			}
			createTask := utils.Task{
				Name: fmt.Sprintf("CreatePodClique-%s", pclqObjectKey),
				Fn: func(ctx context.Context) error {
					return r.doCreate(ctx, logger, pgs, pcsg, int(pcsgReplicaIndex), pclqObjectKey)
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
				logger.Info("Deleting PodCliqueScalingGroup %s replicaIndex: %s", pcsgName, pcsgReplicaIndex)
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
			apicommon.LabelComponentKey:                      component.NamePCSGPodClique,
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

func (r _resource) doCreate(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex int, pclqObjectKey client.ObjectKey) error {
	logger.Info("Running CreateOrUpdate PodClique", "pclqObjectKey", pclqObjectKey)
	pclq := emptyPodClique(pclqObjectKey)
	pcsgObjKey := client.ObjectKeyFromObject(pclq)
	if err := r.buildResource(logger, pgs, pcsg, pcsgReplicaIndex, pclq); err != nil {
		return err
	}
	if err := r.client.Create(ctx, pclq); err != nil {
		if apierrors.IsAlreadyExists(err) {
			logger.Info("PodClique creation failed as it already exists", "pclq", pclqObjectKey)
			return nil
		}
		r.eventRecorder.Eventf(pcsg, corev1.EventTypeWarning, groveevents.ReasonPodCliqueCreationFailed, "PodClique %v creation failed: %v", pclqObjectKey, err)
		return groveerr.WrapError(err,
			errCodeCreatePodClique,
			component.OperationSync,
			fmt.Sprintf("Error creating PodClique: %v for PodCliqueScalingGroup: %v", pclqObjectKey, pcsgObjKey),
		)
	}
	r.eventRecorder.Eventf(pcsg, corev1.EventTypeNormal, groveevents.ReasonPodCliqueCreationSuccessful, "PodClique %v created successfully", pclqObjectKey)
	logger.Info("Successfully created PodClique", "pclqObjectKey", pclqObjectKey)
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
	pclq.Spec = pclqTemplateSpec.Spec
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
func getExpectedPodCliqueFQNsByPCSGReplica(pcsg *grovecorev1alpha1.PodCliqueScalingGroup) (map[int][]string, int) {
	var (
		expectedPCLQFQNs   = make(map[int][]string)
		totalExpectedPCLQs int
	)
	for pcsgReplicaIndex := range int(pcsg.Spec.Replicas) {
		pclqFQNs := lo.Map(pcsg.Spec.CliqueNames, func(cliqueName string, _ int) string {
			return apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{
				Name:    pcsg.Name,
				Replica: int(pcsgReplicaIndex),
			}, cliqueName)
		})
		expectedPCLQFQNs[pcsgReplicaIndex] = pclqFQNs
		totalExpectedPCLQs += len(pclqFQNs)
	}
	return expectedPCLQFQNs, totalExpectedPCLQs
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
			apicommon.LabelComponentKey:          component.NamePCSGPodClique,
			apicommon.LabelPodCliqueScalingGroup: pcsgObjectMeta.Name,
		},
	)
}

func getLabels(pgs *grovecorev1alpha1.PodGangSet, pgsReplicaIndex int, pcsg *grovecorev1alpha1.PodCliqueScalingGroup, pcsgReplicaIndex int, pclqObjectKey client.ObjectKey, pclqTemplateSpec *grovecorev1alpha1.PodCliqueTemplateSpec, podGangName string) map[string]string {
	pclqComponentLabels := map[string]string{
		apicommon.LabelAppNameKey:                        pclqObjectKey.Name,
		apicommon.LabelComponentKey:                      component.NamePCSGPodClique,
		apicommon.LabelPodCliqueScalingGroup:             pcsg.Name,
		apicommon.LabelPodGang:                           podGangName,
		apicommon.LabelPodGangSetReplicaIndex:            strconv.Itoa(pgsReplicaIndex),
		apicommon.LabelPodCliqueScalingGroupReplicaIndex: strconv.Itoa(pcsgReplicaIndex),
		apicommon.LabelPodTemplateHash:                   componentutils.GetPCLQPodTemplateHashLabel(pclqTemplateSpec, pgs.Spec.Template.PriorityClassName),
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
