package podgangsetreplica

import (
	"context"
	"fmt"
	apicommon "github.com/NVIDIA/grove/operator/api/common"
	"github.com/NVIDIA/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/NVIDIA/grove/operator/internal/component"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	groveerr "github.com/NVIDIA/grove/operator/internal/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
)

/*
	Fetch all PCLQs per PGS replica Index
	Fetch all PCSGs per PGS replica Index

	Check if there is a rolling update triggered -> check pgs.status.rollingUpdateProgress
	if there is a rolling update -> get the chosen PGS replica if there is one set.
		if there is no replica selected yet, select one replica and update the status by setting pgs.status.rollingUpdateProgress.currentlyUpdating
		if there is a chosen replica, then check if the update is completed. If not continue and requeue at the end.
		if the update is complete then {
				update the rollingUpdateProgress
				select next replica to update if there is one, update rollingUpdateProgress.currentlyUpdating
				continue and requeue at the end.
		else {
				mark the updateEndedAt time
				continue but do not requeue at the end.
		}
	} else
		continue with no requeue at the end
	}

	How to check if the update of PGS replica has concluded successfully, and we can move to the next replica:
	The criteria tells if the update is complete and we are ready to move to the next PGS replica.
		* for each standalone PCLQ check:
			* pclq.status.rollingUpdateProgress.UpdateEndedAt is set
			* pclq.status.rollingUpdateProgress.podGangSetGenerationHash == pgs.status.generationHash
			* len(pclq.status.rollingUpdateProgress.updatedPods) == pclq.spec.replicas
 			* pclq.status.availableReplicas >= pclq.spec.minAvailable
		* for each PCSG check:
			* pcsg.status.rollingUpdateProgress.UpdateEndedAt is set
			* pcsg.status.rollingUpdateProgress.podGangSetGenerationHash == pgs.status.generationHash
			* pcsg.status.rollingUpdateProgress.updatedReplicas == pcsg.spec.replicas
			* pcsg.status.availableReplicas >= pcsg.spec.minAvailable

	How to progress to next replica for a resource:
	1. Update all unscheduled replicas
	2. Update all unavailable replicas
	3. For each available replica:
		* update the replica
		* wait util availableReplicas >= minAvailable

	Criteria to pick up next index for update:
	1. First take pending replicas
	2. Take unhealthy | unavailable replicas
	3. Healthy ones in the reverser order of their ordinal value

*/

type updateWork struct {
	pgsReplicas []pgsReplica
}

type pgsReplica struct {
	pclqs []grovecorev1alpha1.PodClique
	pcsgs []grovecorev1alpha1.PodCliqueScalingGroup
}

func (r _resource) initializeUpdateWork(ctx context.Context, pgs *grovecorev1alpha1.PodGangSet) (*updateWork, error) {
	pgsObjectKey := client.ObjectKeyFromObject(pgs)
	pclqsByPGSIndex, err := componentutils.GetPCLQsByOwnerReplicaIndex(ctx, r.client, constants.KindPodGangSet, client.ObjectKeyFromObject(pgs), apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgs.Name))
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPCLQs,
			component.OperationSync,
			fmt.Sprintf("could not list PCLQs for PGS: %v", pgsObjectKey),
		)
	}
	pcsgsByPGSIndex, err := componentutils.GetPCSGsByPGSReplicaIndex(ctx, r.client, client.ObjectKeyFromObject(pgs))
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPCSGs,
			component.OperationSync,
			fmt.Sprintf("could not list PCSGs for PGS: %v", pgsObjectKey),
		)
	}
	replicas := make([]pgsReplica, 0, pgs.Spec.Replicas)
	for pgsReplicaIndex := range int(pgs.Spec.Replicas) {
		pgsReplicaIndexStr := strconv.Itoa(pgsReplicaIndex)
		replicas = append(replicas, pgsReplica{
			pclqs: pclqsByPGSIndex[pgsReplicaIndexStr],
			pcsgs: pcsgsByPGSIndex[pgsReplicaIndexStr],
		})
	}
	return &updateWork{pgsReplicas: replicas}, nil
}
