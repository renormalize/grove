package podgangset

import (
	"context"
	"fmt"

	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	ctrlcommon "github.com/NVIDIA/grove/operator/internal/controller/common"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// processGenerationHashChange computes the generation hash given a PodGangSet resource and if the generation has
// changed from the previously persisted pgs.status.generationHash then it resets the pgs.status.rollingUpdateProgress
func (r *Reconciler) processGenerationHashChange(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet) ctrlcommon.ReconcileStepResult {
	pgsObjectKey := client.ObjectKeyFromObject(pgs)
	pgsObjectName := cache.NamespacedNameAsObjectName(pgsObjectKey).String()

	// if the generationHash is not reflected correctly yet, requeue. Allow the informer cache to catch-up.
	if !r.isGenerationHashExpectationSatisfied(pgsObjectName, pgs.Status.GenerationHash) {
		return ctrlcommon.ReconcileAfter(ctrlcommon.ComponentSyncRetryInterval, fmt.Sprintf("GenerationHash is not up-to-date for PodGangSet: %v", pgsObjectKey))
	} else {
		r.pgsGenerationHashExpectations.Delete(pgsObjectName)
	}

	newGenerationHash := computeGenerationHash(pgs)
	if pgs.Status.GenerationHash == nil {
		// update the generation hash and continue reconciliation. No rolling update is required.
		if err := r.setGenerationHash(ctx, pgs, pgsObjectName, newGenerationHash); err != nil {
			return ctrlcommon.ReconcileWithErrors("error updating generation hash", err)
		}
		return ctrlcommon.ContinueReconcile()
	}

	if newGenerationHash != *pgs.Status.GenerationHash {
		// trigger rolling update by setting or overriding pgs.Status.RollingUpdateProgress.
		if err := r.initRollingUpdateProgress(ctx, pgs, pgsObjectName, newGenerationHash); err != nil {
			return ctrlcommon.ReconcileWithErrors(fmt.Sprintf("could not triggering rolling update for PGS: %v", pgsObjectKey), err)
		}
	}

	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) isGenerationHashExpectationSatisfied(pgsObjectName string, pgsGenerationHash *string) bool {
	expectedGenerationHash, ok := r.pgsGenerationHashExpectations.Load(pgsObjectName)
	return !ok || (pgsGenerationHash != nil && expectedGenerationHash.(string) == *pgsGenerationHash)
}

func computeGenerationHash(pgs *grovecorev1alpha1.PodGangSet) string {
	podTemplateSpecs := lo.Map(pgs.Spec.Template.Cliques, func(pclqTemplateSpec *grovecorev1alpha1.PodCliqueTemplateSpec, _ int) *corev1.PodTemplateSpec {
		podTemplateSpec := &corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      pclqTemplateSpec.Labels,
				Annotations: pclqTemplateSpec.Annotations,
			},
			Spec: pclqTemplateSpec.Spec.PodSpec,
		}
		podTemplateSpec.Spec.PriorityClassName = pgs.Spec.Template.PriorityClassName
		return podTemplateSpec
	})
	return k8sutils.ComputeHash(podTemplateSpecs...)
}

func (r *Reconciler) setGenerationHash(ctx context.Context, pgs *grovecorev1alpha1.PodGangSet, pgsObjectName, generationHash string) error {
	pgs.Status.GenerationHash = &generationHash
	if err := r.client.Status().Update(ctx, pgs); err != nil {
		return fmt.Errorf("could not update GenerationHash for PodGangSet: %v: %w", client.ObjectKeyFromObject(pgs), err)
	}
	r.pgsGenerationHashExpectations.Store(pgsObjectName, generationHash)
	return nil
}

func (r *Reconciler) initRollingUpdateProgress(ctx context.Context, pgs *grovecorev1alpha1.PodGangSet, pgsObjectName, newGenerationHash string) error {
	pgs.Status.RollingUpdateProgress = &grovecorev1alpha1.PodGangSetRollingUpdateProgress{
		UpdateStartedAt: metav1.Now(),
	}
	pgs.Status.GenerationHash = &newGenerationHash
	if err := r.client.Status().Update(ctx, pgs); err != nil {
		return fmt.Errorf("could not set RollingUpdateProgress for PodGangSet: %v: %w", client.ObjectKeyFromObject(pgs), err)
	}
	r.pgsGenerationHashExpectations.Store(pgsObjectName, newGenerationHash)
	return nil
}

//func (r *Reconciler) handleRollingUpdateProgress(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet) error {
//	// if there is already a PGS replica index picked for update then check if all its children have been updated
//	// and minAvailable criteria defined on each constituent PCLQ and PCSG is met. Only when that is met should we
//	// select the next replica index, else we requeue.
//	if pgs.Status.RollingUpdateProgress.CurrentlyUpdating != nil {
//		isCurrentPGSReplicaUpdateSuccessful, err := r.isCurrentPGSReplicaUpdateSuccessful(ctx, logger, pgs)
//		if err != nil {
//			return err
//		}
//		if !isCurrentPGSReplicaUpdateSuccessful {
//			return groveerr.New(groveerr.ErrCodeRequeueAfter,
//				"RollingUpdate",
//				fmt.Sprintf("Requeuing as PGS replica index %d has not been updated yet", pgs.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex),
//			)
//		}
//	}
//	// pick up the next PGS replica index to update.
//
//	return nil
//}

//func (r *Reconciler) isCurrentPGSReplicaUpdateSuccessful(ctx context.Context, logger logr.Logger, pgs *grovecorev1alpha1.PodGangSet) (bool, error) {
//	pgsObjectKey := client.ObjectKeyFromObject(pgs)
//	pgsReplicaInUpdate := int(pgs.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex)
//	allPCSGsUpdated, err := r.areAllPCSGsUpdated(ctx, logger, pgsObjectKey, pgsReplicaInUpdate)
//	if err != nil {
//		return false, err
//	}
//	allStandalonePCLQSUpdated, err := r.areAllStandalonePCLQsUpdated(ctx, logger, pgsObjectKey, pgsReplicaInUpdate)
//	if err != nil {
//		return false, err
//	}
//	return allPCSGsUpdated && allStandalonePCLQSUpdated, nil
//}

//func (r *Reconciler) areAllPCSGsUpdated(ctx context.Context, logger logr.Logger, pgsObjectKey client.ObjectKey, pgsReplicaInUpdate int) (bool, error) {
//	pcsgs, err := componentutils.GetPCSGsForPGSReplicaIndex(ctx, r.client, pgsObjectKey, pgsReplicaInUpdate)
//	if err != nil {
//		return false, err
//	}
//	allUpdated := lo.Reduce(pcsgs, func(allUpdated bool, pcsg grovecorev1alpha1.PodCliqueScalingGroup, _ int) bool {
//		pcsgUpdated := pcsg.Status.RollingUpdateProgress != nil && pcsg.Status.RollingUpdateProgress.UpdatedReplicas > *pcsg.Spec.MinAvailable
//		return allUpdated && pcsgUpdated
//	}, true)
//
//	logger.Info("Are all PCSGs updated for PGS replica?", "pgsReplicaInUpdate", pgsReplicaInUpdate, "allUpdated", allUpdated)
//	return allUpdated, nil
//}
//
//// areAllStandalonePCLQsUpdated checks if all the PCLQs are updated. These PCLQs are the ones that:
//// 1. Belong to the PGS replica under update.
//// 2. Are not part of any PCSG.
//func (r *Reconciler) areAllStandalonePCLQsUpdated(ctx context.Context, logger logr.Logger, pgsObjectKey client.ObjectKey, pgsReplicaInUpdate int) (bool, error) {
//	pclqs, err := componentutils.GetPCLQsByOwner(ctx, r.client, constants.KindPodGangSet, pgsObjectKey,
//		lo.Assign(
//			apicommon.GetDefaultLabelsForPodGangSetManagedResources(pgsObjectKey.Name),
//			map[string]string{
//				apicommon.LabelComponentKey:           component.NamePGSPodClique,
//				apicommon.LabelPodGangSetReplicaIndex: strconv.Itoa(pgsReplicaInUpdate),
//			},
//		),
//	)
//	if err != nil {
//		return false, err
//	}
//	allUpdated := lo.Reduce(pclqs, func(allUpdated bool, pclq grovecorev1alpha1.PodClique, _ int) bool {
//		pclqUpdated := pclq.Status.RollingUpdateProgress != nil && len(pclq.Status.RollingUpdateProgress.UpdatedPods) > int(*pclq.Spec.MinAvailable)
//		return allUpdated && pclqUpdated
//	}, true)
//
//	logger.Info("Are all standalone PCLQs updated for PGS replica?", "pgsReplicaInUpdate", pgsReplicaInUpdate, "allUpdated", allUpdated)
//	return allUpdated, nil
//}
