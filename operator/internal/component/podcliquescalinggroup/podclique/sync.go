package podclique

import (
	"context"
	"fmt"
	apicommon "github.com/NVIDIA/grove/operator/api/common"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/NVIDIA/grove/operator/internal/component"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	groveerr "github.com/NVIDIA/grove/operator/internal/errors"
	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
)

type syncContext struct {
	pgs                            *grovecorev1alpha1.PodGangSet
	existingPCLQs                  []grovecorev1alpha1.PodClique
	pcsgIndicesToTerminate         []string
	pcsgIndicesToRequeue           []string
	expectedPCLQFQNsPerPCSGReplica map[int][]string
	expectedPCSGReplicas           int
	expectedPCLQPodTemplateHashMap map[string]string
}

// refreshExistingPCLQs removes all the excess PCLQs that belong to any PCSG replica > expectedPCSGReplicas.
// After every successful delete operation of PCSG replica(s), this method will be called to ensure that further processing
// operates on a consistent state of existing PCLQs.
// NOTE: We will be adding expectations usage in this component as well. Then all deletions will be captured as expectations and after every
// deletion of PCSG we will re-queued.
func (sc *syncContext) refreshExistingPCLQs() error {
	revisedExistingPCLQs := make([]grovecorev1alpha1.PodClique, 0, len(sc.existingPCLQs))
	for _, pclq := range sc.existingPCLQs {
		pcsgReplicaIndexStr, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
		if !ok {
			continue
		}
		pcsgReplicaIndex, err := strconv.Atoi(pcsgReplicaIndexStr)
		if err != nil {
			return groveerr.WrapError(err,
				errCodeParsePodCliqueScalingGroupReplicaIndex,
				component.OperationSync,
				fmt.Sprintf("invalid pcsg replica index label value found on PodClique: %v", client.ObjectKeyFromObject(&pclq)),
			)
		}
		if pcsgReplicaIndex < sc.expectedPCSGReplicas {
			revisedExistingPCLQs = append(revisedExistingPCLQs, pclq)
		}
	}
	sc.existingPCLQs = revisedExistingPCLQs
	return nil
}

func (r _resource) prepareSyncContext(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) (*syncContext, error) {
	var (
		syncCtx = &syncContext{}
		err     error
	)

	// get the PodGangSet
	syncCtx.pgs, err = componentutils.GetOwnerPodGangSet(ctx, r.client, pcsg.ObjectMeta)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeGetPodGangSet,
			component.OperationSync,
			fmt.Sprintf("failed to get owner PodGangSet for PodCliqueScalingGroup %s", client.ObjectKeyFromObject(pcsg)),
		)
	}

	// compute the expected state and get existing state.
	syncCtx.expectedPCLQFQNsPerPCSGReplica, syncCtx.expectedPCSGReplicas = getExpectedPodCliqueFQNsByPCSGReplica(pcsg)
	syncCtx.existingPCLQs, err = r.getExistingPCLQs(ctx, pcsg)
	if err != nil {
		return nil, err
	}

	// compute the PCSG indices that have their MinAvailableBreached condition set to true. Segregated these into two
	// pcsgIndicesToTerminate will have the indices for which the TerminationDelay has expired.
	// pcsgIndicesToRequeue will have the indices for which the TerminationDelay has not yet expired.
	syncCtx.pcsgIndicesToTerminate, syncCtx.pcsgIndicesToRequeue = getMinAvailableBreachedPCSGIndices(logger, syncCtx.existingPCLQs, syncCtx.pgs.Spec.Template.TerminationDelay.Duration)

	// pre-compute expected PodTemplateHash for each PCLQ
	syncCtx.expectedPCLQPodTemplateHashMap = getExpectedPCLQPodTemplateHashMap(syncCtx.pgs, pcsg)

	return syncCtx, nil
}

func getExpectedPCLQPodTemplateHashMap(pgs *grovecorev1alpha1.PodGangSet, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) map[string]string {
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
	return pclqFQNToHash
}
