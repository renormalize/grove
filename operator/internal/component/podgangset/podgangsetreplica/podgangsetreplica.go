package podgangsetreplica

import (
	"context"
	"fmt"
	apicommon "github.com/NVIDIA/grove/operator/api/common"
	"github.com/NVIDIA/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/NVIDIA/grove/operator/internal/component"
	groveevents "github.com/NVIDIA/grove/operator/internal/component/events"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	groveerr "github.com/NVIDIA/grove/operator/internal/errors"
	"github.com/NVIDIA/grove/operator/internal/utils"
	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
	"strconv"
	"time"
)

const (
	errCodeComputingPGSReplicaDeletionWork grovecorev1alpha1.ErrorCode = "ERR_COMPUTE_PGS_REPLICA_DELETION_WORK"
	errCodeDeletePGSReplica                grovecorev1alpha1.ErrorCode = "ERR_DELETE_PGS_REPLICA"
)

type _resource struct {
	client        client.Client
	eventRecorder record.EventRecorder
}
type deletionWork struct {
	deletionTasks []utils.Task
	// minAvailableBreachedConstituents
	minAvailableBreachedConstituents map[int][]string
}

func (d deletionWork) shouldRequeue() bool {
	return len(d.minAvailableBreachedConstituents) > 0
}

func (d deletionWork) hasPendingPGSReplicaDeletion() bool {
	return len(d.deletionTasks) > 0
}

func New(client client.Client, eventRecorder record.EventRecorder) component.Operator[grovecorev1alpha1.PodGangSet] {
	return &_resource{
		client:        client,
		eventRecorder: eventRecorder,
	}
}

// GetExistingResourceNames returns the names of all the existing resources that the Role Operator manages.
func (r _resource) GetExistingResourceNames(_ context.Context, _ logr.Logger, _ metav1.ObjectMeta) ([]string, error) {
	return []string{}, nil
}

func (r _resource) Sync(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet) error {
	pgsObjectKey := client.ObjectKeyFromObject(pgs)
	work, err := r.getPGSReplicaDeletionWork(ctx, logger, pgs)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeComputingPGSReplicaDeletionWork,
			component.OperationSync,
			fmt.Sprintf("Could not compute pending replica deletion work for PGS: %v", pgsObjectKey))
	}
	if work.hasPendingPGSReplicaDeletion() {
		if runResult := utils.RunConcurrently(ctx, logger, work.deletionTasks); runResult.HasErrors() {
			return groveerr.WrapError(runResult.GetAggregatedError(),
				errCodeDeletePGSReplica,
				component.OperationSync,
				fmt.Sprintf("Error deleting PodCliques for PodGangSet: %v", pgsObjectKey),
			)
		}
	}
	if work.shouldRequeue() {
		return groveerr.New(groveerr.ErrCodeContinueReconcileAndRequeue,
			component.OperationSync,
			"Requeuing to re-process PGS replicas that have breached MinAvailable but not crossed TerminationDelay",
		)
	}
	return nil
}

func (r _resource) Delete(_ context.Context, _ logr.Logger, _ metav1.ObjectMeta) error {
	return nil
}

func (r _resource) getPGSReplicaDeletionWork(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet) (*deletionWork, error) {
	var (
		now              = time.Now()
		pgsObjectKey     = client.ObjectKeyFromObject(pgs)
		terminationDelay = pgs.Spec.Template.TerminationDelay.Duration
		deletionTasks    = make([]utils.Task, 0, pgs.Spec.Replicas)
		work             = &deletionWork{
			minAvailableBreachedConstituents: make(map[int][]string),
		}
	)

	for pgsReplicaIndex := range int(pgs.Spec.Replicas) {
		breachedPCSGNames, minPCSGWaitFor, err := r.getMinAvailableBreachedPCSGs(ctx, pgsObjectKey, pgsReplicaIndex, terminationDelay, now)
		if err != nil {
			return nil, err
		}
		breachedPCLQNames, minPCLQWaitFor, skipPGSReplicaIndex, err := r.getMinAvailableBreachedPCLQsNotInPCSG(ctx, pgs, pgsReplicaIndex, now)
		if err != nil {
			return nil, err
		}
		if skipPGSReplicaIndex {
			continue
		}
		if (len(breachedPCSGNames) > 0 && minPCSGWaitFor <= 0) ||
			(len(breachedPCLQNames) > 0 && minPCLQWaitFor <= 0) {
			// terminate all PodCliques for this PGS replica index
			reason := fmt.Sprintf("Delete all PodCliques for PodGangSet %v with replicaIndex :%d due to MinAvailable breached longer than TerminationDelay: %s", pgsObjectKey, pgsReplicaIndex, terminationDelay)
			pclqGangTerminationTask := r.createPGSReplicaDeleteTask(logger, pgs, pgsReplicaIndex, reason)
			deletionTasks = append(deletionTasks, pclqGangTerminationTask)
		} else if len(breachedPCSGNames) > 0 || len(breachedPCLQNames) > 0 {
			work.minAvailableBreachedConstituents[pgsReplicaIndex] = append(breachedPCLQNames, breachedPCLQNames...)
		}
	}
	work.deletionTasks = deletionTasks
	return work, nil
}

func (r _resource) getMinAvailableBreachedPCSGs(ctx context.Context, pgsObjKey client.ObjectKey, pgsReplicaIndex int, terminationDelay time.Duration, since time.Time) ([]string, time.Duration, error) {
	pcsgList := &grovecorev1alpha1.PodCliqueScalingGroupList{}
	if err := r.client.List(ctx,
		pcsgList,
		client.InNamespace(pgsObjKey.Namespace),
		client.MatchingLabels(lo.Assign(
			apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgsObjKey.Name),
			map[string]string{
				apicommon.LabelPodGangSetReplicaIndex: strconv.Itoa(pgsReplicaIndex),
			},
		)),
	); err != nil {
		return nil, 0, err
	}
	breachedPCSGNames, minWaitFor := getMinAvailableBreachedPCSGInfo(pcsgList.Items, terminationDelay, since)
	return breachedPCSGNames, minWaitFor, nil
}

// getMinAvailableBreachedPCSGInfo filters PodCliqueScalingGroups that have grovecorev1alpha1.ConditionTypeMinAvailableBreached set to true.
// It returns the names of all such PodCliqueScalingGroups and minimum of all the waitDurations.
func getMinAvailableBreachedPCSGInfo(pcsgs []grovecorev1alpha1.PodCliqueScalingGroup, terminationDelay time.Duration, since time.Time) ([]string, time.Duration) {
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

func (r _resource) getMinAvailableBreachedPCLQsNotInPCSG(ctx context.Context, pgs *grovecorev1alpha1.PodGangSet, pgsReplicaIndex int, since time.Time) (breachedPCLQNames []string, minWaitFor time.Duration, skipPGSReplica bool, err error) {
	pclqFQNsNotInPCSG := make([]string, 0, len(pgs.Spec.Template.Cliques))
	for _, pclqTemplateSpec := range pgs.Spec.Template.Cliques {
		if !isPCLQInPCSG(pclqTemplateSpec.Name, pgs.Spec.Template.PodCliqueScalingGroupConfigs) {
			pclqFQNsNotInPCSG = append(pclqFQNsNotInPCSG, apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pgs.Name, Replica: pgsReplicaIndex}, pclqTemplateSpec.Name))
		}
	}
	var (
		pclqs            []grovecorev1alpha1.PodClique
		notFoundPCLQFQNs []string
	)
	pclqs, notFoundPCLQFQNs, err = r.getExistingPCLQsByNames(ctx, pgs.Namespace, pclqFQNsNotInPCSG)
	if err != nil {
		return
	}
	if len(notFoundPCLQFQNs) > 0 {
		skipPGSReplica = true
		return
	}
	breachedPCLQNames, minWaitFor = componentutils.GetMinAvailableBreachedPCLQInfo(pclqs, pgs.Spec.Template.TerminationDelay.Duration, since)
	return
}

func isPCLQInPCSG(pclqName string, pcsgConfigs []grovecorev1alpha1.PodCliqueScalingGroupConfig) bool {
	return lo.Reduce(pcsgConfigs, func(agg bool, pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig, _ int) bool {
		return agg || slices.Contains(pcsgConfig.CliqueNames, pclqName)
	}, false)
}

// getExistingPCLQsByNames fetches PodClique objects. It returns the PCLQ objects that it found and a slice of PCLQ FQNs for which no PCLQ object exists. If there is an error it just returns the error.
func (r _resource) getExistingPCLQsByNames(ctx context.Context, namespace string, pclqFQNs []string) (pclqs []grovecorev1alpha1.PodClique, notFoundPCLQFQNs []string, err error) {
	for _, pclqFQN := range pclqFQNs {
		pclq := grovecorev1alpha1.PodClique{}
		if err = r.client.Get(ctx, client.ObjectKey{Name: pclqFQN, Namespace: namespace}, &pclq); err != nil {
			if apierrors.IsNotFound(err) {
				notFoundPCLQFQNs = append(notFoundPCLQFQNs, pclqFQN)
				continue
			}
			return nil, nil, err
		}
		pclqs = append(pclqs, pclq)
	}
	return pclqs, notFoundPCLQFQNs, nil
}

// createPGSReplicaDeleteTask creates a Task to delete all the PodCliques that are part of a PGS replica.
func (r _resource) createPGSReplicaDeleteTask(logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet, pgsReplicaIndex int, reason string) utils.Task {
	return utils.Task{
		Name: fmt.Sprintf("DeletePGSReplicaPodCliques-%d", pgsReplicaIndex),
		Fn: func(ctx context.Context) error {
			if err := r.client.DeleteAllOf(ctx,
				&grovecorev1alpha1.PodClique{},
				client.InNamespace(pgs.Namespace),
				client.MatchingLabels(
					lo.Assign(
						apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgs.Name),
						map[string]string{
							apicommon.LabelPodGangSetReplicaIndex: strconv.Itoa(pgsReplicaIndex),
						},
					))); err != nil {
				logger.Error(err, "failed to delete PodCliques for PGS Replica index", "pgsReplicaIndex", pgsReplicaIndex, "reason", reason)
				r.eventRecorder.Eventf(pgs, corev1.EventTypeWarning, groveevents.ReasonPodGangSetReplicaDeletionFailed, "Error deleting PodGangSet replica %d: %v", pgsReplicaIndex, err)
				return err
			}
			logger.Info("Deleted PGS replica PodCliques", "pgsReplicaIndex", pgsReplicaIndex, "reason", reason)
			r.eventRecorder.Eventf(pgs, corev1.EventTypeNormal, groveevents.ReasonPodGangSetReplicaDeletionSuccessful, "PodGangSet replica %d deleted", pgsReplicaIndex)
			return nil
		},
	}
}
